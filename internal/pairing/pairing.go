// Package pairing joins primary calls with their mirrored shadow copies.
//
// Observations are keyed by request id (Envoy x-request-id, identical on the
// mirrored copy). The pending set is bounded and every entry has a TTL, so a
// burst of unsampled primaries cannot grow memory without limit. Nothing is
// dropped silently: expired and evicted observations are reported to the
// Sink, and malformed or duplicate ones are counted.
package pairing

import (
	"container/list"
	"sync"
	"time"

	"github.com/mountain-nerd/shadowbox/internal/observation"
)

// Clock is injected so tests can control time.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Sink observes pairing outcomes. It is called without the pairer's lock
// held, so it may call back into the Pairer.
type Sink interface {
	OnPair(observation.Pair)
	OnUnpaired(observation.Observation)
}

// Options configures a Pairer.
type Options struct {
	// TTL is how long an observation waits for its counterpart.
	TTL time.Duration
	// MaxPending bounds the number of waiting observations.
	MaxPending int
	// Clock defaults to the system clock.
	Clock Clock
}

// Stats counts pairing outcomes since the Pairer was created.
type Stats struct {
	Pending, Paired, Expired, Evicted, Dropped int64
}

type entry struct {
	obs   observation.Observation
	added time.Time
	elem  *list.Element
}

type key struct {
	id   string
	role observation.Role
}

// Pairer joins observations. It is safe for concurrent use.
type Pairer struct {
	sink Sink
	opts Options

	mu      sync.Mutex
	pending map[key]*entry
	order   *list.List // of key, oldest first
	stats   Stats
}

// New creates a Pairer that reports to sink.
func New(sink Sink, opts Options) *Pairer {
	if opts.Clock == nil {
		opts.Clock = systemClock{}
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = 10_000
	}
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Second
	}
	return &Pairer{sink: sink, opts: opts, pending: map[key]*entry{}, order: list.New()}
}

// events collects sink notifications so they can be sent after unlocking.
type events struct {
	pairs    []observation.Pair
	unpaired []observation.Observation
}

// Add offers an observation for pairing.
func (p *Pairer) Add(o observation.Observation) {
	var ev events
	p.mu.Lock()
	p.expireLocked(&ev)
	p.addLocked(o, &ev)
	p.mu.Unlock()
	p.dispatch(ev)
}

// Sweep reports observations whose TTL has passed.
func (p *Pairer) Sweep() {
	var ev events
	p.mu.Lock()
	p.expireLocked(&ev)
	p.mu.Unlock()
	p.dispatch(ev)
}

// Stats returns a snapshot of the counters.
func (p *Pairer) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Pending = int64(len(p.pending))
	return s
}

func (p *Pairer) addLocked(o observation.Observation, ev *events) {
	if o.RequestID == "" || (o.Role != observation.Primary && o.Role != observation.Shadow) {
		p.stats.Dropped++
		return
	}
	self := key{o.RequestID, o.Role}
	if _, dup := p.pending[self]; dup {
		p.stats.Dropped++
		return
	}
	other := key{o.RequestID, counterpart(o.Role)}
	if match, ok := p.pending[other]; ok {
		p.removeLocked(other, match)
		p.stats.Paired++
		ev.pairs = append(ev.pairs, makePair(o, match.obs))
		return
	}
	if len(p.pending) >= p.opts.MaxPending {
		oldest := p.order.Front().Value.(key)
		e := p.pending[oldest]
		p.removeLocked(oldest, e)
		p.stats.Evicted++
		ev.unpaired = append(ev.unpaired, e.obs)
	}
	e := &entry{obs: o, added: p.opts.Clock.Now()}
	e.elem = p.order.PushBack(self)
	p.pending[self] = e
}

func (p *Pairer) expireLocked(ev *events) {
	cutoff := p.opts.Clock.Now().Add(-p.opts.TTL)
	for front := p.order.Front(); front != nil; front = p.order.Front() {
		k := front.Value.(key)
		e := p.pending[k]
		if e.added.After(cutoff) {
			return
		}
		p.removeLocked(k, e)
		p.stats.Expired++
		ev.unpaired = append(ev.unpaired, e.obs)
	}
}

func (p *Pairer) removeLocked(k key, e *entry) {
	p.order.Remove(e.elem)
	delete(p.pending, k)
}

func (p *Pairer) dispatch(ev events) {
	for _, pr := range ev.pairs {
		p.sink.OnPair(pr)
	}
	for _, o := range ev.unpaired {
		p.sink.OnUnpaired(o)
	}
}

func counterpart(r observation.Role) observation.Role {
	if r == observation.Primary {
		return observation.Shadow
	}
	return observation.Primary
}

func makePair(a, b observation.Observation) observation.Pair {
	pr := observation.Pair{Primary: a, Shadow: b}
	if a.Role == observation.Shadow {
		pr = observation.Pair{Primary: b, Shadow: a}
	}
	pr.Experiment = pr.Shadow.Experiment
	return pr
}
