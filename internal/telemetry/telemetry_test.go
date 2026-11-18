package telemetry_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/experiment"
	"github.com/mountain-nerd/shadowbox/internal/extproc"
	"github.com/mountain-nerd/shadowbox/internal/pairing"
	"github.com/mountain-nerd/shadowbox/internal/telemetry"
)

type fakeRegistry struct{}

func (fakeRegistry) IDs() []string { return []string{"support-v2"} }
func (fakeRegistry) Verdict(string) (analysis.Verdict, bool) {
	return analysis.Verdict{Status: analysis.Fail, Pairs: 18, Metrics: map[string]analysis.Result{
		"decision_agreement": {Value: 0.67, Samples: 18},
	}}, true
}
func (fakeRegistry) Stats() experiment.Stats {
	return experiment.Stats{UnknownExperiment: 2, UnsampledPrimaries: 40}
}

func scrape(t *testing.T) string {
	t.Helper()
	h, shutdown, err := telemetry.NewHandler(telemetry.Sources{
		Pairing:  func() pairing.Stats { return pairing.Stats{Pending: 3, Paired: 18, Expired: 4, Evicted: 1, Dropped: 2} },
		Registry: fakeRegistry{},
		Shadow:   func() extproc.Stats { return extproc.Stats{Rejected: 5, RewriteFailures: 6} },
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(t.Context()) })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	return string(b)
}

// sample finds a sample line by metric name and label fragments and returns its value.
func sample(t *testing.T, out, name string, labels ...string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + name + `(\{[^}]*\})? (\S+)$`)
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		ok := true
		for _, l := range labels {
			ok = ok && strings.Contains(m[1], l)
		}
		if ok {
			return m[2]
		}
	}
	t.Fatalf("no sample %s %v in:\n%s", name, labels, out)
	return ""
}

func TestMetricsExposePairingVerdictAndShadowCounters(t *testing.T) {
	out := scrape(t)

	checks := []struct {
		name   string
		labels []string
		want   string
	}{
		{"shadowbox_pairs_total", nil, "18"},
		{"shadowbox_pending", nil, "3"},
		{"shadowbox_unpaired_total", []string{`reason="expired"`}, "4"},
		{"shadowbox_unpaired_total", []string{`reason="evicted"`}, "1"},
		{"shadowbox_unpaired_total", []string{`reason="dropped"`}, "2"},
		{"shadowbox_unpaired_total", []string{`reason="unsampled_primary"`}, "40"},
		{"shadowbox_unpaired_total", []string{`reason="unknown_experiment"`}, "2"},
		{"shadowbox_shadow_rejected_total", []string{`reason="unknown_experiment"`}, "5"},
		{"shadowbox_shadow_rejected_total", []string{`reason="rewrite_failure"`}, "6"},
		{"shadowbox_verdict", []string{`experiment="support-v2"`, `status="fail"`}, "1"},
		{"shadowbox_verdict", []string{`experiment="support-v2"`, `status="pass"`}, "0"},
		{"shadowbox_experiment_pairs", []string{`experiment="support-v2"`}, "18"},
		{"shadowbox_experiment_metric", []string{`experiment="support-v2"`, `metric="decision_agreement"`}, "0.67"},
	}
	for _, c := range checks {
		if got := sample(t, out, c.name, c.labels...); got != c.want {
			t.Errorf("%s%v = %s, want %s", c.name, c.labels, got, c.want)
		}
	}
}

func TestMetricsUseOnlyBoundedLabels(t *testing.T) {
	out := scrape(t)
	allowed := map[string]bool{"experiment": true, "metric": true, "status": true, "reason": true,
		"otel_scope_name": true, "otel_scope_version": true, "otel_scope_schema_url": true}
	re := regexp.MustCompile(`(?m)^shadowbox_\w+\{([^}]*)\}`)
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		for _, kv := range strings.Split(m[1], ",") {
			key := strings.SplitN(kv, "=", 2)[0]
			if !allowed[key] && !strings.HasPrefix(key, "otel_scope_") {
				t.Errorf("unexpected label %q (labels must stay low-cardinality)", key)
			}
		}
	}
}
