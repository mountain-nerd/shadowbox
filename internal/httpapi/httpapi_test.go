package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/httpapi"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

type fakeVerdicts map[string]analysis.Verdict

func (f fakeVerdicts) Verdict(id string) (analysis.Verdict, bool) { v, ok := f[id]; return v, ok }
func (f fakeVerdicts) IDs() []string {
	ids := []string{}
	for k := range f {
		ids = append(ids, k)
	}
	return ids
}

type recordingIngestor struct {
	mu  sync.Mutex
	got []observation.Observation
}

func (r *recordingIngestor) Add(o observation.Observation) {
	r.mu.Lock()
	r.got = append(r.got, o)
	r.mu.Unlock()
}

var verdicts = fakeVerdicts{
	"support-v2": {
		Status: analysis.Fail, Pairs: 18,
		Reasons: []string{"new_write_tool_proposals = 6, want <= 0 (issue_refund)"},
		Metrics: map[string]analysis.Result{"decision_agreement": {Value: 0.67, Samples: 18}},
	},
}

func serve(t *testing.T, h http.Handler, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestVerdict_MatchesArgoWebMetricContract(t *testing.T) {
	h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: &recordingIngestor{}})

	rec := serve(t, h, http.MethodGet, "/v1/experiments/support-v2/verdict", "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Status  string                     `json:"status"`
		Pairs   int                        `json:"pairs"`
		Reasons []string                   `json:"reasons"`
		Metrics map[string]analysis.Result `json:"metrics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	// Argo: jsonPath "{$.status}", successCondition result == "pass", failureCondition result == "fail".
	if body.Status != "fail" || body.Pairs != 18 || len(body.Reasons) != 1 {
		t.Fatalf("body = %+v, want status fail, 18 pairs, one reason", body)
	}
	if body.Metrics["decision_agreement"].Samples != 18 {
		t.Fatalf("metrics = %+v, want decision_agreement over 18 samples", body.Metrics)
	}
}

func TestVerdict_UnknownExperimentIs404(t *testing.T) {
	h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: &recordingIngestor{}})

	rec := serve(t, h, http.MethodGet, "/v1/experiments/nope/verdict", "", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestListExperiments(t *testing.T) {
	h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: &recordingIngestor{}})

	rec := serve(t, h, http.MethodGet, "/v1/experiments", "", nil)

	var body struct{ Experiments []string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || len(body.Experiments) != 1 || body.Experiments[0] != "support-v2" {
		t.Fatalf("status %d body %s, want [support-v2]", rec.Code, rec.Body)
	}
}

const shadowObservation = `{
  "requestId": "r-1", "traceparent": "00-aaaabbbbccccddddeeeeffff00001111-1111222233334444-01",
  "experiment": "support-v2", "role": "shadow", "model": "candidate-v2", "status": 200,
  "decision": {"kind": "tool_calls", "toolCalls": [{"name": "issue_refund", "argsHash": "abc"}]},
  "usage": {"inputTokens": 100, "outputTokens": 20}
}`

func TestIngest_AcceptsObservation(t *testing.T) {
	ing := &recordingIngestor{}
	h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: ing, IngestToken: "s3cret"})

	rec := serve(t, h, http.MethodPost, "/v1/observations", shadowObservation, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer s3cret"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body)
	}
	if len(ing.got) != 1 {
		t.Fatalf("ingested %d, want 1", len(ing.got))
	}
	o := ing.got[0]
	if o.RequestID != "r-1" || o.Role != observation.Shadow || o.TraceID != "aaaabbbbccccddddeeeeffff00001111" ||
		o.Experiment != "support-v2" || o.Status != 200 || o.Model != "candidate-v2" {
		t.Fatalf("observation = %+v, want fields mapped from JSON", o)
	}
	if o.Decision.Kind != observation.ToolCalls || len(o.Decision.ToolCalls) != 1 || o.Decision.ToolCalls[0].Name != "issue_refund" {
		t.Fatalf("decision = %+v, want one issue_refund call", o.Decision)
	}
	if o.Usage != (observation.Usage{InputTokens: 100, OutputTokens: 20, Known: true}) {
		t.Fatalf("usage = %+v, want known 100/20", o.Usage)
	}
}

func TestIngest_RejectsInvalidObservations(t *testing.T) {
	cases := map[string]string{
		"not json":          `{`,
		"missing requestId": strings.Replace(shadowObservation, `"requestId": "r-1",`, ``, 1),
		"bad role":          strings.Replace(shadowObservation, `"shadow"`, `"canary"`, 1),
		"bad kind":          strings.Replace(shadowObservation, `"tool_calls"`, `"maybe"`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ing := &recordingIngestor{}
			h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: ing, IngestToken: "s3cret"})

			rec := serve(t, h, http.MethodPost, "/v1/observations", body, map[string]string{"Authorization": "Bearer s3cret"})

			if rec.Code != http.StatusBadRequest || len(ing.got) != 0 {
				t.Fatalf("status = %d, ingested %d; want 400 and nothing ingested", rec.Code, len(ing.got))
			}
		})
	}
}

func TestIngest_RequiresTokenWhenConfigured(t *testing.T) {
	ing := &recordingIngestor{}
	h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: ing, IngestToken: "s3cret"})

	none := serve(t, h, http.MethodPost, "/v1/observations", shadowObservation, nil)
	wrong := serve(t, h, http.MethodPost, "/v1/observations", shadowObservation, map[string]string{"Authorization": "Bearer nope"})
	right := serve(t, h, http.MethodPost, "/v1/observations", shadowObservation, map[string]string{"Authorization": "Bearer s3cret"})

	if none.Code != 401 || wrong.Code != 401 || right.Code != 202 || len(ing.got) != 1 {
		t.Fatalf("codes = %d/%d/%d ingested %d; want 401/401/202 and 1", none.Code, wrong.Code, right.Code, len(ing.got))
	}
}

// Without a token, ingest is off: otherwise any caller that can reach the
// port could post agreeing pairs and push a verdict toward pass.
func TestIngest_DisabledWithoutToken(t *testing.T) {
	ing := &recordingIngestor{}
	h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: ing})

	rec := serve(t, h, http.MethodPost, "/v1/observations", shadowObservation, map[string]string{"Authorization": "Bearer anything"})

	if rec.Code != http.StatusForbidden || len(ing.got) != 0 {
		t.Fatalf("status = %d, ingested %d; want 403 and nothing ingested", rec.Code, len(ing.got))
	}
}

func TestMetricsEndpointDelegates(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("shadowbox_pairs_total 3\n")) })
	h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: &recordingIngestor{}, Metrics: metrics})

	rec := serve(t, h, http.MethodGet, "/metrics", "", nil)

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "shadowbox_pairs_total 3") {
		t.Fatalf("status %d body %q, want delegated metrics output", rec.Code, rec.Body)
	}
}

func TestHealthz(t *testing.T) {
	h := httpapi.New(httpapi.Options{Verdicts: verdicts, Ingestor: &recordingIngestor{}})
	if rec := serve(t, h, http.MethodGet, "/healthz", "", nil); rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
