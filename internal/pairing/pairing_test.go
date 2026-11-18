package pairing_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mountain-nerd/shadowbox/internal/observation"
	"github.com/mountain-nerd/shadowbox/internal/pairing"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type recordingSink struct {
	mu         sync.Mutex
	pairs      []observation.Pair
	unpaired   []observation.Observation
	duplicates []observation.Observation
}

func (s *recordingSink) OnPair(p observation.Pair) {
	s.mu.Lock()
	s.pairs = append(s.pairs, p)
	s.mu.Unlock()
}
func (s *recordingSink) OnUnpaired(o observation.Observation) {
	s.mu.Lock()
	s.unpaired = append(s.unpaired, o)
	s.mu.Unlock()
}
func (s *recordingSink) OnDuplicate(o observation.Observation) {
	s.mu.Lock()
	s.duplicates = append(s.duplicates, o)
	s.mu.Unlock()
}

func primary(id string) observation.Observation {
	return observation.Observation{RequestID: id, Role: observation.Primary, Model: "v1"}
}
func shadow(id, exp string) observation.Observation {
	return observation.Observation{RequestID: id, Role: observation.Shadow, Model: "v2", Experiment: exp}
}

func newPairer(sink pairing.Sink, clock *fakeClock, max int) *pairing.Pairer {
	return pairing.New(sink, pairing.Options{TTL: 30 * time.Second, MaxPending: max, Clock: clock})
}

func TestPairsPrimaryThenShadow(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 100)

	p.Add(primary("r1"))
	p.Add(shadow("r1", "exp-a"))

	if len(sink.pairs) != 1 {
		t.Fatalf("pairs = %d, want 1", len(sink.pairs))
	}
	got := sink.pairs[0]
	if got.Experiment != "exp-a" || got.Primary.Model != "v1" || got.Shadow.Model != "v2" {
		t.Fatalf("pair = %+v, want experiment exp-a with v1 primary and v2 shadow", got)
	}
}

func TestPairsShadowThenPrimary(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 100)

	p.Add(shadow("r1", "exp-a"))
	p.Add(primary("r1"))

	if len(sink.pairs) != 1 || sink.pairs[0].Primary.Role != observation.Primary || sink.pairs[0].Shadow.Role != observation.Shadow {
		t.Fatalf("pairs = %+v, want one correctly oriented pair", sink.pairs)
	}
}

func TestDifferentRequestIDsDoNotPair(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 100)

	p.Add(primary("r1"))
	p.Add(shadow("r2", "exp-a"))

	if len(sink.pairs) != 0 {
		t.Fatalf("pairs = %d, want 0", len(sink.pairs))
	}
	if got := p.Stats().Pending; got != 2 {
		t.Fatalf("Pending = %d, want 2", got)
	}
}

func TestExpiredObservationsAreReportedUnpairedAndNotPairedLater(t *testing.T) {
	sink := &recordingSink{}
	clock := &fakeClock{now: time.Unix(0, 0)}
	p := newPairer(sink, clock, 100)

	p.Add(primary("r1"))
	clock.Advance(31 * time.Second)
	p.Sweep()
	p.Add(shadow("r1", "exp-a"))

	if len(sink.unpaired) != 1 || sink.unpaired[0].Role != observation.Primary {
		t.Fatalf("unpaired = %+v, want the expired primary", sink.unpaired)
	}
	if len(sink.pairs) != 0 {
		t.Fatalf("pairs = %d, want 0 after expiry", len(sink.pairs))
	}
	if got := p.Stats().Expired; got != 1 {
		t.Fatalf("Expired = %d, want 1", got)
	}
}

func TestNotExpiredBeforeTTL(t *testing.T) {
	sink := &recordingSink{}
	clock := &fakeClock{now: time.Unix(0, 0)}
	p := newPairer(sink, clock, 100)

	p.Add(primary("r1"))
	clock.Advance(29 * time.Second)
	p.Sweep()
	p.Add(shadow("r1", "exp-a"))

	if len(sink.pairs) != 1 {
		t.Fatalf("pairs = %d, want 1 when the shadow arrives within the TTL", len(sink.pairs))
	}
}

func TestAddSweepsExpiredEntries(t *testing.T) {
	sink := &recordingSink{}
	clock := &fakeClock{now: time.Unix(0, 0)}
	p := newPairer(sink, clock, 100)

	p.Add(primary("r1"))
	clock.Advance(31 * time.Second)
	p.Add(primary("r2"))

	if len(sink.unpaired) != 1 || sink.unpaired[0].RequestID != "r1" {
		t.Fatalf("unpaired = %+v, want r1 expired during Add", sink.unpaired)
	}
}

func TestCapacityEvictsOldestPending(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 2)

	p.Add(primary("r1"))
	p.Add(primary("r2"))
	p.Add(primary("r3"))

	if len(sink.unpaired) != 1 || sink.unpaired[0].RequestID != "r1" {
		t.Fatalf("unpaired = %+v, want r1 evicted", sink.unpaired)
	}
	st := p.Stats()
	if st.Evicted != 1 || st.Pending != 2 {
		t.Fatalf("Stats = %+v, want Evicted=1 Pending=2", st)
	}
}

func TestObservationWithoutRequestIDIsDroppedAndCounted(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 100)

	p.Add(primary(""))

	if st := p.Stats(); st.Dropped != 1 || st.Pending != 0 {
		t.Fatalf("Stats = %+v, want Dropped=1 Pending=0", st)
	}
}

func TestDuplicateSameRoleIsDroppedAndFirstStillPairs(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 100)

	first := primary("r1")
	first.Model = "first"
	dup := primary("r1")
	dup.Model = "dup"
	p.Add(first)
	p.Add(dup)
	p.Add(shadow("r1", "exp-a"))

	if st := p.Stats(); st.Duplicates != 1 || st.Dropped != 0 {
		t.Fatalf("Stats = %+v, want Duplicates=1 Dropped=0", st)
	}
	if len(sink.duplicates) != 1 || sink.duplicates[0].Model != "dup" {
		t.Fatalf("duplicates = %+v, want the second primary reported", sink.duplicates)
	}
	if len(sink.pairs) != 1 || sink.pairs[0].Primary.Model != "first" {
		t.Fatalf("pairs = %+v, want the first primary paired", sink.pairs)
	}
}

// A client that reuses an x-request-id could get a candidate answer paired
// with the wrong primary. Ids that already paired are remembered for the TTL,
// so a reuse is reported instead of forming a second, possibly mismatched pair.
func TestRequestIDReusedAfterPairingIsADuplicate(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 100)

	p.Add(primary("r1"))
	p.Add(shadow("r1", "exp-a"))
	p.Add(primary("r1"))
	p.Add(shadow("r1", "exp-a"))

	if len(sink.pairs) != 1 {
		t.Fatalf("pairs = %d, want 1 (the reuse must not pair)", len(sink.pairs))
	}
	if len(sink.duplicates) != 2 {
		t.Fatalf("duplicates = %d, want 2", len(sink.duplicates))
	}
	if st := p.Stats(); st.Duplicates != 2 || st.Pending != 0 {
		t.Fatalf("Stats = %+v, want Duplicates=2 Pending=0", st)
	}
}

func TestRequestIDMayBeReusedAfterTheTTL(t *testing.T) {
	sink := &recordingSink{}
	clock := &fakeClock{now: time.Unix(0, 0)}
	p := newPairer(sink, clock, 100)

	p.Add(primary("r1"))
	p.Add(shadow("r1", "exp-a"))
	clock.Advance(31 * time.Second)
	p.Add(primary("r1"))
	p.Add(shadow("r1", "exp-a"))

	if len(sink.pairs) != 2 || len(sink.duplicates) != 0 {
		t.Fatalf("pairs = %d, duplicates = %d, want 2 and 0", len(sink.pairs), len(sink.duplicates))
	}
}

func TestRememberedRequestIDsAreBounded(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 2)

	for _, id := range []string{"r1", "r2", "r3"} {
		p.Add(primary(id))
		p.Add(shadow(id, "exp-a"))
	}
	p.Add(primary("r3")) // still remembered
	p.Add(primary("r1")) // forgotten: only the newest MaxPending ids are kept

	if len(sink.duplicates) != 1 || sink.duplicates[0].RequestID != "r3" {
		t.Fatalf("duplicates = %+v, want only r3", sink.duplicates)
	}
	if st := p.Stats(); st.Pending != 1 {
		t.Fatalf("Pending = %d, want 1 (r1 waits as a new primary)", st.Pending)
	}
}

func TestSinkMayCallBackIntoPairer(t *testing.T) {
	var p *pairing.Pairer
	sink := &callbackSink{onPair: func() { _ = p.Stats() }}
	p = pairing.New(sink, pairing.Options{TTL: time.Minute, MaxPending: 10, Clock: &fakeClock{now: time.Unix(0, 0)}})

	done := make(chan struct{})
	go func() {
		p.Add(primary("r1"))
		p.Add(shadow("r1", "exp-a"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: sink was called while the pairer held its lock")
	}
}

type callbackSink struct{ onPair func() }

func (s *callbackSink) OnPair(observation.Pair)             { s.onPair() }
func (s *callbackSink) OnUnpaired(observation.Observation)  {}
func (s *callbackSink) OnDuplicate(observation.Observation) {}

func TestConcurrentAddsPairEverything(t *testing.T) {
	sink := &recordingSink{}
	p := newPairer(sink, &fakeClock{now: time.Unix(0, 0)}, 10_000)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("r%d", i)
		wg.Add(2)
		go func() { defer wg.Done(); p.Add(primary(id)) }()
		go func() { defer wg.Done(); p.Add(shadow(id, "exp-a")) }()
	}
	wg.Wait()

	if len(sink.pairs) != 200 {
		t.Fatalf("pairs = %d, want 200", len(sink.pairs))
	}
	if st := p.Stats(); st.Pending != 0 || st.Paired != 200 {
		t.Fatalf("Stats = %+v, want Pending=0 Paired=200", st)
	}
}
