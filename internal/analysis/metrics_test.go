package analysis_test

import (
	"math"
	"testing"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/effect"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

func tools(names ...string) observation.Decision {
	d := observation.Decision{Kind: observation.ToolCalls}
	for _, n := range names {
		d.ToolCalls = append(d.ToolCalls, observation.ToolCall{Name: n})
	}
	return d
}

var (
	text   = observation.Decision{Kind: observation.Text}
	failed = observation.Decision{Kind: observation.Error}
)

func pair(p, s observation.Decision) observation.Pair {
	return observation.Pair{
		Primary: observation.Observation{Role: observation.Primary, Decision: p},
		Shadow:  observation.Observation{Role: observation.Shadow, Decision: s},
	}
}

func withUsage(pr observation.Pair, primaryTokens, shadowTokens int) observation.Pair {
	pr.Primary.Usage = observation.Usage{InputTokens: primaryTokens, Known: true}
	pr.Shadow.Usage = observation.Usage{InputTokens: shadowTokens, Known: true}
	return pr
}

func feed(m analysis.Metric, pairs ...observation.Pair) analysis.Result {
	for _, p := range pairs {
		m.Observe(p)
	}
	return m.Result()
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDecisionAgreement(t *testing.T) {
	r := feed(analysis.NewDecisionAgreement(),
		pair(tools("lookup_order"), tools("lookup_order")), // agree
		pair(tools("lookup_order"), tools("issue_refund")), // disagree
		pair(text, text),                       // agree
		pair(failed, tools("lookup_order")),    // excluded: primary failed
		pair(tools("lookup_order"), failed),    // excluded: shadow failed
		pair(tools("a", "b"), tools("b", "a")), // agree: order-insensitive
		pair(text, tools("lookup_order")),      // disagree: different kind
	)

	if r.Samples != 5 || !near(r.Value, 3.0/5.0) {
		t.Fatalf("Result = %+v, want 3/5 over 5 samples", r)
	}
}

func TestNewWriteToolProposals(t *testing.T) {
	reg := effect.NewRegistry(map[string]effect.Class{
		"lookup_order": effect.ReadOnly,
		"issue_refund": effect.Irreversible,
	})
	r := feed(analysis.NewWriteToolProposals(reg),
		pair(tools("lookup_order"), tools("issue_refund")),                 // new write: counts
		pair(tools("lookup_order"), tools("lookup_order")),                 // read-only: no
		pair(tools("issue_refund"), tools("issue_refund")),                 // write, but not new: no
		pair(text, tools("delete_account")),                                // unknown tool => write: counts
		pair(tools("lookup_order"), tools("lookup_order", "issue_refund")), // new write alongside read: counts
		pair(tools("lookup_order"), text),                                  // no proposal: no
	)

	if r.Value != 3 || r.Samples != 6 {
		t.Fatalf("Result = %+v, want Value=3 over 6 samples", r)
	}
	if r.Detail != "delete_account, issue_refund" {
		t.Fatalf("Detail = %q, want %q", r.Detail, "delete_account, issue_refund")
	}
}

func TestShadowErrorRate(t *testing.T) {
	r := feed(analysis.NewShadowErrorRate(),
		pair(text, text), pair(text, failed), pair(tools("x"), tools("x")), pair(text, text))

	if r.Samples != 4 || !near(r.Value, 0.25) {
		t.Fatalf("Result = %+v, want 0.25 over 4 samples", r)
	}
}

func TestTokenRatio(t *testing.T) {
	r := feed(analysis.NewTokenRatio(),
		withUsage(pair(text, text), 100, 150),
		withUsage(pair(text, text), 100, 50),
		pair(text, text),                        // usage unknown: excluded
		withUsage(pair(text, failed), 100, 900), // shadow failed: excluded
	)

	if r.Samples != 2 || !near(r.Value, 200.0/200.0) {
		t.Fatalf("Result = %+v, want ratio 1.0 over 2 samples", r)
	}
}

func TestMetricsWithNoDataReportZeroSamples(t *testing.T) {
	reg := effect.NewRegistry(nil)
	for _, m := range []analysis.Metric{
		analysis.NewDecisionAgreement(), analysis.NewWriteToolProposals(reg),
		analysis.NewShadowErrorRate(), analysis.NewTokenRatio(),
	} {
		if r := m.Result(); r.Samples != 0 || r.Value != 0 {
			t.Errorf("%s with no data = %+v, want zero", m.Name(), r)
		}
	}
}
