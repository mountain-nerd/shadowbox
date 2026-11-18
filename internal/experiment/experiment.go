// Package experiment routes pairs to per-experiment analyzers (a facade over
// metrics and the gate) and implements pairing.Sink.
package experiment

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

// MetricUnpairedShadows reports shadow calls whose primary was never seen
// (for example because the observer was down). It is informational.
const MetricUnpairedShadows = "unpaired_shadows"

// Analyzer accumulates one experiment's pairs. It is safe for concurrent use.
type Analyzer struct {
	gate analysis.Gate

	mu       sync.Mutex
	metrics  []analysis.Metric
	pairs    int
	unpaired int
}

// NewAnalyzer creates an Analyzer with the given gate and metrics.
func NewAnalyzer(g analysis.Gate, metrics ...analysis.Metric) *Analyzer {
	return &Analyzer{gate: g, metrics: metrics}
}

// Observe feeds a pair to every metric.
func (a *Analyzer) Observe(p observation.Pair) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pairs++
	for _, m := range a.metrics {
		m.Observe(p)
	}
}

func (a *Analyzer) unpairedShadow() {
	a.mu.Lock()
	a.unpaired++
	a.mu.Unlock()
}

// Verdict evaluates the gate on the current metric results.
func (a *Analyzer) Verdict() analysis.Verdict {
	a.mu.Lock()
	results := make(map[string]analysis.Result, len(a.metrics)+1)
	for _, m := range a.metrics {
		results[m.Name()] = m.Result()
	}
	results[MetricUnpairedShadows] = analysis.Result{Value: float64(a.unpaired), Samples: a.pairs + a.unpaired}
	pairs := a.pairs
	a.mu.Unlock()
	return a.gate.Evaluate(pairs, results)
}

// Stats counts observations that did not reach an analyzer.
type Stats struct {
	UnknownExperiment  int64 `json:"unknownExperiment"`
	UnsampledPrimaries int64 `json:"unsampledPrimaries"`
}

// Registry holds the configured experiments. The set of experiments is fixed
// at construction; each Analyzer synchronizes itself.
type Registry struct {
	analyzers map[string]*Analyzer
	unknown   atomic.Int64
	unsampled atomic.Int64
}

// NewRegistry creates a Registry over the given analyzers.
func NewRegistry(analyzers map[string]*Analyzer) *Registry {
	m := make(map[string]*Analyzer, len(analyzers))
	for k, v := range analyzers {
		m[k] = v
	}
	return &Registry{analyzers: m}
}

// OnPair implements pairing.Sink.
func (r *Registry) OnPair(p observation.Pair) {
	a, ok := r.analyzers[p.Experiment]
	if !ok {
		r.unknown.Add(1)
		return
	}
	a.Observe(p)
}

// OnUnpaired implements pairing.Sink. A primary without a shadow was simply
// not sampled; a shadow without a primary is reported on its experiment.
func (r *Registry) OnUnpaired(o observation.Observation) {
	if o.Role != observation.Shadow {
		r.unsampled.Add(1)
		return
	}
	if a, ok := r.analyzers[o.Experiment]; ok {
		a.unpairedShadow()
		return
	}
	r.unknown.Add(1)
}

// Verdict returns the verdict for an experiment.
func (r *Registry) Verdict(id string) (analysis.Verdict, bool) {
	a, ok := r.analyzers[id]
	if !ok {
		return analysis.Verdict{}, false
	}
	return a.Verdict(), true
}

// Stats returns registry-level counters.
func (r *Registry) Stats() Stats {
	return Stats{UnknownExperiment: r.unknown.Load(), UnsampledPrimaries: r.unsampled.Load()}
}

// IDs returns the experiment ids, sorted.
func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.analyzers))
	for id := range r.analyzers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
