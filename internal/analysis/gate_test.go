package analysis_test

import (
	"strings"
	"testing"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
)

func results(kv ...any) map[string]analysis.Result {
	m := map[string]analysis.Result{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1].(analysis.Result)
	}
	return m
}

var gate = analysis.Gate{
	MinPairs: 20,
	Rules: []analysis.Rule{
		{Metric: "new_write_tool_proposals", Op: analysis.AtMost, Threshold: 0, FailFast: true},
		{Metric: "decision_agreement", Op: analysis.AtLeast, Threshold: 0.8},
		{Metric: "shadow_error_rate", Op: analysis.AtMost, Threshold: 0.05},
	},
}

func healthy(pairs int) map[string]analysis.Result {
	return results(
		"new_write_tool_proposals", analysis.Result{Value: 0, Samples: pairs},
		"decision_agreement", analysis.Result{Value: 0.9, Samples: pairs},
		"shadow_error_rate", analysis.Result{Value: 0.0, Samples: pairs},
	)
}

func TestGate_ZeroPairsIsInconclusiveNeverPass(t *testing.T) {
	v := gate.Evaluate(0, healthy(0))
	if v.Status != analysis.Inconclusive {
		t.Fatalf("Status = %s, want inconclusive", v.Status)
	}
}

func TestGate_BelowMinPairsIsInconclusive(t *testing.T) {
	v := gate.Evaluate(19, healthy(19))
	if v.Status != analysis.Inconclusive {
		t.Fatalf("Status = %s, want inconclusive", v.Status)
	}
	if !strings.Contains(strings.Join(v.Reasons, ";"), "19 of 20") {
		t.Fatalf("Reasons = %v, want mention of 19 of 20 pairs", v.Reasons)
	}
}

func TestGate_HealthyWithEnoughPairsPasses(t *testing.T) {
	v := gate.Evaluate(20, healthy(20))
	if v.Status != analysis.Pass {
		t.Fatalf("Status = %s (%v), want pass", v.Status, v.Reasons)
	}
}

func TestGate_ViolationWithEnoughPairsFails(t *testing.T) {
	r := healthy(40)
	r["decision_agreement"] = analysis.Result{Value: 0.67, Samples: 40}

	v := gate.Evaluate(40, r)

	if v.Status != analysis.Fail {
		t.Fatalf("Status = %s, want fail", v.Status)
	}
	if !strings.Contains(strings.Join(v.Reasons, ";"), "decision_agreement") {
		t.Fatalf("Reasons = %v, want decision_agreement named", v.Reasons)
	}
}

func TestGate_FailFastRuleFailsBelowMinPairs(t *testing.T) {
	r := healthy(3)
	r["new_write_tool_proposals"] = analysis.Result{Value: 1, Samples: 3, Detail: "issue_refund"}

	v := gate.Evaluate(3, r)

	if v.Status != analysis.Fail {
		t.Fatalf("Status = %s, want fail", v.Status)
	}
	if !strings.Contains(strings.Join(v.Reasons, ";"), "issue_refund") {
		t.Fatalf("Reasons = %v, want the tool named", v.Reasons)
	}
}

func TestGate_NonFailFastViolationBelowMinPairsIsInconclusive(t *testing.T) {
	r := healthy(5)
	r["shadow_error_rate"] = analysis.Result{Value: 0.2, Samples: 5}

	if v := gate.Evaluate(5, r); v.Status != analysis.Inconclusive {
		t.Fatalf("Status = %s, want inconclusive (too few pairs to judge a rate)", v.Status)
	}
}

func TestGate_MissingMetricIsInconclusive(t *testing.T) {
	r := healthy(30)
	delete(r, "shadow_error_rate")

	v := gate.Evaluate(30, r)

	if v.Status != analysis.Inconclusive {
		t.Fatalf("Status = %s, want inconclusive when a rule's metric is missing", v.Status)
	}
}

func TestGate_MetricWithoutSamplesIsInconclusive(t *testing.T) {
	r := healthy(30)
	r["decision_agreement"] = analysis.Result{Samples: 0}

	if v := gate.Evaluate(30, r); v.Status != analysis.Inconclusive {
		t.Fatalf("Status = %s, want inconclusive when a rule has no samples", v.Status)
	}
}

func TestGate_ThresholdsAreInclusive(t *testing.T) {
	r := healthy(20)
	r["decision_agreement"] = analysis.Result{Value: 0.8, Samples: 20}
	r["shadow_error_rate"] = analysis.Result{Value: 0.05, Samples: 20}

	if v := gate.Evaluate(20, r); v.Status != analysis.Pass {
		t.Fatalf("Status = %s (%v), want pass at exact thresholds", v.Status, v.Reasons)
	}
}

func TestGate_FailBeatsInconclusive(t *testing.T) {
	r := healthy(30)
	r["new_write_tool_proposals"] = analysis.Result{Value: 2, Samples: 30, Detail: "issue_refund"}
	delete(r, "shadow_error_rate")

	if v := gate.Evaluate(30, r); v.Status != analysis.Fail {
		t.Fatalf("Status = %s, want fail when any rule is violated", v.Status)
	}
}
