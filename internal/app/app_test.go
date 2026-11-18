package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/mountain-nerd/shadowbox/internal/app"
	"github.com/mountain-nerd/shadowbox/internal/config"
)

func loadConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shadowbox.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

var client = &http.Client{Timeout: 3 * time.Second}

type running struct {
	httpURL  string
	grpcAddr string
}

func start(t *testing.T, cfg *config.Config) running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := app.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	hl, _ := net.Listen("tcp", "127.0.0.1:0")
	gl, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, hl, gl) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})
	return running{httpURL: "http://" + hl.Addr().String(), grpcAddr: gl.Addr().String()}
}

const baseConfig = `
ingestToken: test-token
extprocToken: stream-secret
tools: {lookup_order: read-only, issue_refund: irreversible}
experiments:
- id: support-v2
  candidate: {model: candidate-v2, systemPrompt: "v2"}
  gate:
    minPairs: 1
    rules: [{metric: new_write_tool_proposals, op: at_most, threshold: 0, failFast: true}]
`

func post(t *testing.T, url, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/observations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("ingest status = %d", resp.StatusCode)
	}
}

func obs(id, role, tool string) string {
	return fmt.Sprintf(`{"requestId":%q,"role":%q,"experiment":"support-v2","status":200,
	  "decision":{"kind":"tool_calls","toolCalls":[{"name":%q}]}}`, id, role, tool)
}

type verdict struct {
	Status  string   `json:"status"`
	Pairs   int      `json:"pairs"`
	Reasons []string `json:"reasons"`
}

func getVerdict(t *testing.T, url string) verdict {
	t.Helper()
	resp, err := client.Get(url + "/v1/experiments/support-v2/verdict")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v verdict
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestApp_IngestedPairsDriveVerdictAndMetrics(t *testing.T) {
	r := start(t, loadConfig(t, baseConfig))

	post(t, r.httpURL, obs("r1", "primary", "lookup_order"))
	post(t, r.httpURL, obs("r1", "shadow", "issue_refund"))

	v := getVerdict(t, r.httpURL)
	if v.Status != "fail" || v.Pairs != 1 || !strings.Contains(strings.Join(v.Reasons, ";"), "issue_refund") {
		t.Fatalf("verdict = %+v, want fail with 1 pair naming issue_refund", v)
	}
	resp, err := client.Get(r.httpURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !regexp.MustCompile(`(?m)^shadowbox_pairs_total(\{[^}]*\})? 1$`).Match(b) {
		t.Fatalf("/metrics missing shadowbox_pairs_total 1:\n%s", b)
	}
}

func TestApp_ServesExtProcShadowRewrite(t *testing.T) {
	r := start(t, loadConfig(t, baseConfig))
	conn, err := grpc.NewClient(r.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-shadowbox-token", "stream-secret", "x-shadowbox-role", "shadow", "x-shadowbox-experiment", "support-v2")
	s, err := extprocv3.NewExternalProcessorClient(conn).Process(ctx)
	if err != nil {
		t.Fatal(err)
	}

	_ = s.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{RequestHeaders: &extprocv3.HttpHeaders{
		Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{{Key: "x-request-id", RawValue: []byte("r9")}}}}}})
	_, _ = s.Recv()
	_ = s.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{RequestBody: &extprocv3.HttpBody{
		Body: []byte(`{"model":"v1","messages":[{"role":"system","content":"v1"}]}`), EndOfStream: true}}})
	resp, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}

	body := string(resp.GetRequestBody().GetResponse().GetBodyMutation().GetBody())
	if !strings.Contains(body, `"model":"candidate-v2"`) || !strings.Contains(body, `"content":"v2"`) {
		t.Fatalf("rewritten body = %s, want candidate model and v2 prompt from config", body)
	}
}

func mcpServerURL(t *testing.T) string {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "kb", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "search_kb", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestApp_TrustedMCPHintsClassifyTools(t *testing.T) {
	cases := map[bool]string{true: "pass", false: "fail"}
	for trusted, want := range cases {
		t.Run(fmt.Sprintf("trusted=%v", trusted), func(t *testing.T) {
			cfg := loadConfig(t, baseConfig+fmt.Sprintf("mcp: {servers: [{url: %q, trusted: %v}]}\n", mcpServerURL(t), trusted))
			r := start(t, cfg)

			post(t, r.httpURL, obs("r1", "primary", "lookup_order"))
			post(t, r.httpURL, obs("r1", "shadow", "search_kb"))

			if v := getVerdict(t, r.httpURL); v.Status != want {
				t.Fatalf("verdict = %+v, want %s (search_kb is read-only only if the MCP server is trusted)", v, want)
			}
		})
	}
}

func TestApp_UnreachableMCPServerFallsBackToStricterRegistry(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	r := start(t, loadConfig(t, baseConfig+fmt.Sprintf("mcp: {servers: [{url: %q, trusted: true}]}\n", url)))

	post(t, r.httpURL, obs("r1", "primary", "lookup_order"))
	post(t, r.httpURL, obs("r1", "shadow", "search_kb"))

	if v := getVerdict(t, r.httpURL); v.Status != "fail" {
		t.Fatalf("verdict = %+v, want fail: without hints, search_kb is unknown and counts as a write", v)
	}
}
