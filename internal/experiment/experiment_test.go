package experiment_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/effect"
	"github.com/mountain-nerd/shadowbox/internal/experiment"
	"github.com/mountain-nerd/shadowbox/internal/observation"
	"github.com/mountain-nerd/shadowbox/internal/pairing"
)

var registry = effect.NewRegistry(map[string]effect.Class{
	"lookup_order": effect.ReadOnly, "issue_refund": effect.Irreversible,
})

func newAnalyzer() *experiment.Analyzer {
	gate := analysis.Gate{MinPairs: 3, Rules: []analysis.Rule{
		{Metric: "new_write_tool_proposals", Op: analysis.AtMost, Threshold: 0, FailFast: true},
		{Metric: "decision_agreement", Op: analysis.AtLeast, Threshold: 0.8},
	}}
	return experiment.NewAnalyzer(gate,
		analysis.NewDecisionAgreement(), analysis.NewWriteToolProposals(registry))
}

func toolPair(exp, primaryTool, shadowTool string) observation.Pair {
	call := func(n string) observation.Decision {
		return observation.Decision{Kind: observation.ToolCalls, ToolCalls: []observation.ToolCall{{Name: n}}}
	}
	return observation.Pair{
		Experiment: exp,
		Primary:    observation.Observation{Role: observation.Primary, Decision: call(primaryTool)},
		Shadow:     observation.Observation{Role: observation.Shadow, Experiment: exp, Decision: call(shadowTool)},
	}
}

func TestAnalyzer_PassesWhenShadowAgrees(t *testing.T) {
	a := newAnalyzer()
	for i := 0; i < 3; i++ {
		a.Observe(toolPair("e", "lookup_order", "lookup_order"))
	}

	v := a.Verdict()

	if v.Status != analysis.Pass || v.Pairs != 3 {
		t.Fatalf("Verdict = %s with %d pairs (%v), want pass with 3", v.Status, v.Pairs, v.Reasons)
	}
	if got := v.Metrics["decision_agreement"]; got.Value != 1 || got.Samples != 3 {
		t.Fatalf("decision_agreement = %+v, want 1 over 3", got)
	}
}

func TestAnalyzer_FailsOnNewWriteTool(t *testing.T) {
	a := newAnalyzer()
	a.Observe(toolPair("e", "lookup_order", "issue_refund"))

	if v := a.Verdict(); v.Status != analysis.Fail {
		t.Fatalf("Status = %s, want fail", v.Status)
	}
}

func TestRegistry_RoutesPairsByExperiment(t *testing.T) {
	r := experiment.NewRegistry(map[string]*experiment.Analyzer{"a": newAnalyzer(), "b": newAnalyzer()})

	r.OnPair(toolPair("a", "lookup_order", "issue_refund"))

	va, _ := r.Verdict("a")
	vb, _ := r.Verdict("b")
	if va.Status != analysis.Fail || va.Pairs != 1 {
		t.Fatalf("experiment a = %s/%d, want fail/1", va.Status, va.Pairs)
	}
	if vb.Pairs != 0 || vb.Status != analysis.Inconclusive {
		t.Fatalf("experiment b = %s/%d, want inconclusive/0 (untouched)", vb.Status, vb.Pairs)
	}
}

func TestRegistry_UnknownExperimentIsCountedNotRouted(t *testing.T) {
	r := experiment.NewRegistry(map[string]*experiment.Analyzer{"a": newAnalyzer()})

	r.OnPair(toolPair("nope", "lookup_order", "issue_refund"))

	if _, ok := r.Verdict("nope"); ok {
		t.Fatalf("Verdict(nope) found, want not found")
	}
	if got := r.Stats().UnknownExperiment; got != 1 {
		t.Fatalf("UnknownExperiment = %d, want 1", got)
	}
	if va, _ := r.Verdict("a"); va.Pairs != 0 {
		t.Fatalf("experiment a got %d pairs, want 0", va.Pairs)
	}
}

func TestRegistry_UnpairedShadowIsReportedOnItsExperiment(t *testing.T) {
	r := experiment.NewRegistry(map[string]*experiment.Analyzer{"a": newAnalyzer()})

	r.OnUnpaired(observation.Observation{Role: observation.Shadow, Experiment: "a", RequestID: "r1"})
	r.OnUnpaired(observation.Observation{Role: observation.Primary, RequestID: "r2"})

	va, _ := r.Verdict("a")
	if got := va.Metrics["unpaired_shadows"]; got.Value != 1 {
		t.Fatalf("unpaired_shadows = %+v, want 1", got)
	}
	if got := r.Stats().UnsampledPrimaries; got != 1 {
		t.Fatalf("UnsampledPrimaries = %d, want 1", got)
	}
}

func agreeing(a *experiment.Analyzer, exp string) {
	for i := 0; i < 3; i++ {
		a.Observe(toolPair(exp, "lookup_order", "lookup_order"))
	}
}

// A duplicate request id means pairs may be mismatched, so the experiment
// must not pass on them.
func TestRegistry_DuplicateRequestIDHoldsAPassAtInconclusive(t *testing.T) {
	a := newAnalyzer()
	agreeing(a, "a")
	r := experiment.NewRegistry(map[string]*experiment.Analyzer{"a": a})

	r.OnDuplicate(observation.Observation{Role: observation.Shadow, Experiment: "a", RequestID: "r1"})

	v, _ := r.Verdict("a")
	if v.Status != analysis.Inconclusive || !strings.Contains(strings.Join(v.Reasons, ";"), "duplicate") {
		t.Fatalf("Verdict = %s %v, want inconclusive naming the duplicates", v.Status, v.Reasons)
	}
	if got := v.Metrics[experiment.MetricDuplicateRequestIDs]; got.Value != 1 {
		t.Fatalf("%s = %+v, want 1", experiment.MetricDuplicateRequestIDs, got)
	}
}

// A primary carries no experiment id, so a duplicate one taints every experiment.
func TestRegistry_DuplicatePrimaryTaintsEveryExperiment(t *testing.T) {
	a, b := newAnalyzer(), newAnalyzer()
	agreeing(a, "a")
	agreeing(b, "b")
	r := experiment.NewRegistry(map[string]*experiment.Analyzer{"a": a, "b": b})

	r.OnDuplicate(observation.Observation{Role: observation.Primary, RequestID: "r1"})

	for _, id := range []string{"a", "b"} {
		if v, _ := r.Verdict(id); v.Status != analysis.Inconclusive {
			t.Errorf("%s: Verdict = %s, want inconclusive", id, v.Status)
		}
	}
}

func TestRegistry_DuplicateDoesNotHideAFailure(t *testing.T) {
	a := newAnalyzer()
	a.Observe(toolPair("a", "lookup_order", "issue_refund"))
	r := experiment.NewRegistry(map[string]*experiment.Analyzer{"a": a})

	r.OnDuplicate(observation.Observation{Role: observation.Shadow, Experiment: "a", RequestID: "r1"})

	if v, _ := r.Verdict("a"); v.Status != analysis.Fail {
		t.Fatalf("Verdict = %s, want fail to stand", v.Status)
	}
}

func TestRegistry_IsAPairingSinkAndSafeForConcurrentUse(t *testing.T) {
	r := experiment.NewRegistry(map[string]*experiment.Analyzer{"a": newAnalyzer()})
	var sink pairing.Sink = r

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); sink.OnPair(toolPair("a", "lookup_order", "lookup_order")) }()
		go func() { defer wg.Done(); _, _ = r.Verdict("a") }()
	}
	wg.Wait()

	if va, _ := r.Verdict("a"); va.Pairs != 100 {
		t.Fatalf("Pairs = %d, want 100", va.Pairs)
	}
}

func TestRegistry_IDsAreSorted(t *testing.T) {
	r := experiment.NewRegistry(map[string]*experiment.Analyzer{"b": newAnalyzer(), "a": newAnalyzer()})
	ids := r.IDs()
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("IDs = %v, want [a b]", ids)
	}
}
