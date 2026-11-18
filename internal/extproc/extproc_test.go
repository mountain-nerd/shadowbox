package extproc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/mountain-nerd/shadowbox/internal/extproc"
	"github.com/mountain-nerd/shadowbox/internal/llm"
	"github.com/mountain-nerd/shadowbox/internal/llm/openai"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

type recordingSink struct {
	mu  sync.Mutex
	got []observation.Observation
}

func (s *recordingSink) Add(o observation.Observation) {
	s.mu.Lock()
	s.got = append(s.got, o)
	s.mu.Unlock()
}
func (s *recordingSink) all() []observation.Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observation.Observation(nil), s.got...)
}

// waitFor polls until the sink holds n observations (observe mode is asynchronous).
func (s *recordingSink) waitFor(t *testing.T, n int) []observation.Observation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.all(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sink has %d observations, want %d", len(s.all()), n)
	return nil
}

const v2Prompt = "You are a concise support agent. You may issue refunds."

const streamToken = "stream-secret"

func newServer(t *testing.T, sink extproc.Sink, maxBody int) (*extproc.Server, extprocv3.ExternalProcessorClient) {
	t.Helper()
	return newServerWithToken(t, sink, maxBody, streamToken)
}

func newServerWithToken(t *testing.T, sink extproc.Sink, maxBody int, token string) (*extproc.Server, extprocv3.ExternalProcessorClient) {
	t.Helper()
	srv := extproc.NewServer(extproc.Options{
		Token:  token,
		Parser: openai.Codec{},
		Experiments: map[string]extproc.Experiment{
			"support-v2": {Model: "candidate-v2", Rewrite: llm.Pipeline{
				openai.ModelOverride{Model: "candidate-v2"}, openai.SystemPrompt{Text: v2Prompt},
				openai.StripProviderTools{}, openai.DropStatefulFields{},
			}},
		},
		Sink:         sink,
		MaxBodyBytes: maxBody,
	})
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return srv, extprocv3.NewExternalProcessorClient(conn)
}

// open starts a stream that carries the shared token plus the given metadata.
func open(t *testing.T, c extprocv3.ExternalProcessorClient, md ...string) extprocv3.ExternalProcessor_ProcessClient {
	t.Helper()
	return openRaw(t, c, append([]string{"x-shadowbox-token", streamToken}, md...)...)
}

// openRaw starts a stream with exactly the given metadata.
func openRaw(t *testing.T, c extprocv3.ExternalProcessorClient, md ...string) extprocv3.ExternalProcessor_ProcessClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	ctx = metadata.AppendToOutgoingContext(ctx, md...)
	s, err := c.Process(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func headers(kv ...string) *extprocv3.HttpHeaders {
	h := &corev3.HeaderMap{}
	for i := 0; i < len(kv); i += 2 {
		h.Headers = append(h.Headers, &corev3.HeaderValue{Key: kv[i], RawValue: []byte(kv[i+1])})
	}
	return &extprocv3.HttpHeaders{Headers: h}
}

func send(t *testing.T, s extprocv3.ExternalProcessor_ProcessClient, req *extprocv3.ProcessingRequest) {
	t.Helper()
	if err := s.Send(req); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func reqHeaders(kv ...string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{RequestHeaders: headers(kv...)}}
}
func reqBody(b string, eos bool) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{RequestBody: &extprocv3.HttpBody{Body: []byte(b), EndOfStream: eos}}}
}
func respHeaders(kv ...string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{ResponseHeaders: headers(kv...)}}
}
func respBody(b string, eos bool) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseBody{ResponseBody: &extprocv3.HttpBody{Body: []byte(b), EndOfStream: eos}}}
}

const traceparent = "00-aaaabbbbccccddddeeeeffff00001111-1111222233334444-01"

const chatRequest = `{"model":"current-v1","store":true,"messages":[{"role":"system","content":"v1"},{"role":"user","content":"refund"}],
 "tools":[{"type":"function","function":{"name":"issue_refund"}},{"type":"web_search"}]}`

const refundResponse = `{"id":"c1","object":"chat.completion","model":"candidate-v2","choices":[{"index":0,"finish_reason":"tool_calls",
 "message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"issue_refund","arguments":"{\"order_id\":\"A-1\"}"}}]}}],
 "usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}`

func headerValue(m *extprocv3.HeaderMutation, key string) string {
	for _, h := range m.GetSetHeaders() {
		if h.GetHeader().GetKey() == key {
			if v := h.GetHeader().GetRawValue(); len(v) > 0 {
				return string(v)
			}
			return h.GetHeader().GetValue()
		}
	}
	return ""
}

func TestShadow_RewritesRequestBody(t *testing.T) {
	_, c := newServer(t, &recordingSink{}, 0)
	s := open(t, c, "x-shadowbox-role", "shadow", "x-shadowbox-experiment", "support-v2")

	send(t, s, reqHeaders(":path", "/v1/chat/completions", "x-request-id", "r-1", "traceparent", traceparent))
	if r, err := s.Recv(); err != nil || r.GetRequestHeaders() == nil {
		t.Fatalf("headers response = %v, %v; want continue", r, err)
	}
	send(t, s, reqBody(chatRequest, true))
	r, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	common := r.GetRequestBody().GetResponse()
	body := common.GetBodyMutation().GetBody()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("mutated body is not JSON: %v (%q)", err, body)
	}
	if got["model"] != "candidate-v2" {
		t.Errorf("model = %v, want candidate-v2", got["model"])
	}
	if sys := got["messages"].([]any)[0].(map[string]any)["content"]; sys != v2Prompt {
		t.Errorf("system prompt = %v, want v2", sys)
	}
	if tools := got["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v, want web_search stripped", tools)
	}
	if _, ok := got["store"]; ok {
		t.Errorf("store still present")
	}
	if cl := headerValue(common.GetHeaderMutation(), "content-length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("content-length = %q, want %d", cl, len(body))
	}
}

// Only an allowlist of headers reaches the candidate: anything else (session
// cookies, any provider's key header, custom secrets, baggage) is removed.
func TestShadow_ForwardsOnlyAllowlistedHeaders(t *testing.T) {
	_, c := newServer(t, &recordingSink{}, 0)
	s := open(t, c, "x-shadowbox-role", "shadow", "x-shadowbox-experiment", "support-v2")

	send(t, s, reqHeaders(
		":method", "POST", ":path", "/v1/chat/completions", ":authority", "llm", "host", "llm",
		"content-type", "application/json", "accept", "application/json", "traceparent", traceparent,
		"x-request-id", "r-1", "x-shadowbox-experiment", "support-v2",
		"authorization", "Bearer prod-key", "x-api-key", "k", "api-key", "k", "cookie", "session=1",
		"proxy-authorization", "Basic x", "x-goog-api-key", "g", "x-internal-secret", "s", "baggage", "user.id=42"))
	r, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}

	removed := map[string]bool{}
	for _, h := range r.GetRequestHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders() {
		removed[h] = true
	}
	for _, h := range []string{"authorization", "x-api-key", "api-key", "cookie", "proxy-authorization", "x-goog-api-key", "x-internal-secret", "baggage"} {
		if !removed[h] {
			t.Errorf("%s not removed from the shadow copy", h)
		}
	}
	for _, h := range []string{":method", ":path", ":authority", "host", "content-type", "accept", "traceparent", "x-request-id", "x-shadowbox-experiment"} {
		if removed[h] {
			t.Errorf("%s removed, want it forwarded", h)
		}
	}
}

func TestStream_RequiresSharedToken(t *testing.T) {
	cases := map[string][]string{
		"missing token": {"x-shadowbox-role", "observe"},
		"wrong token":   {"x-shadowbox-token", "guess", "x-shadowbox-role", "observe"},
	}
	for name, md := range cases {
		t.Run(name, func(t *testing.T) {
			sink := &recordingSink{}
			_, c := newServer(t, sink, 0)
			s := openRaw(t, c, md...)

			send(t, s, reqHeaders("x-request-id", "r-1"))
			_, err := s.Recv()

			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("err = %v, want Unauthenticated", err)
			}
			if len(sink.all()) != 0 {
				t.Fatalf("an unauthenticated stream produced observations")
			}
		})
	}
}

func TestStream_ServerWithoutTokenRejectsEverything(t *testing.T) {
	_, c := newServerWithToken(t, &recordingSink{}, 0, "")
	s := openRaw(t, c, "x-shadowbox-token", "", "x-shadowbox-role", "observe")

	send(t, s, reqHeaders("x-request-id", "r-1"))
	if _, err := s.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated (an unconfigured token must not mean open)", err)
	}
}

func TestShadow_CapturesCandidateDecision(t *testing.T) {
	sink := &recordingSink{}
	_, c := newServer(t, sink, 0)
	s := open(t, c, "x-shadowbox-role", "shadow", "x-shadowbox-experiment", "support-v2")

	send(t, s, reqHeaders("x-request-id", "r-1", "traceparent", traceparent))
	_, _ = s.Recv()
	send(t, s, reqBody(chatRequest, true))
	_, _ = s.Recv()
	send(t, s, respHeaders(":status", "200"))
	if r, err := s.Recv(); err != nil || r.GetResponseHeaders() == nil {
		t.Fatalf("response headers reply = %v, %v; want continue", r, err)
	}
	send(t, s, respBody(refundResponse, true))
	if r, err := s.Recv(); err != nil || r.GetResponseBody() == nil {
		t.Fatalf("response body reply = %v, %v; want continue", r, err)
	}

	got := sink.waitFor(t, 1)[0]
	if got.Role != observation.Shadow || got.Experiment != "support-v2" || got.RequestID != "r-1" ||
		got.TraceID != "aaaabbbbccccddddeeeeffff00001111" || got.Status != 200 || got.Model != "candidate-v2" {
		t.Fatalf("observation = %+v, want shadow/support-v2/r-1/trace/200/candidate-v2", got)
	}
	if got.Decision.Kind != observation.ToolCalls || got.Decision.ToolCalls[0].Name != "issue_refund" || got.Usage.Total() != 60 {
		t.Fatalf("decision/usage = %+v/%+v, want issue_refund and 60 tokens", got.Decision, got.Usage)
	}
}

func TestShadow_ExperimentFallsBackToShadowHeader(t *testing.T) {
	sink := &recordingSink{}
	_, c := newServer(t, sink, 0)
	s := open(t, c, "x-shadowbox-role", "shadow")

	send(t, s, reqHeaders("x-request-id", "r-2", "x-shadowbox-experiment", "support-v2"))
	if r, err := s.Recv(); err != nil || r.GetImmediateResponse() != nil {
		t.Fatalf("headers reply = %v, %v; want continue for a known experiment", r, err)
	}
}

func TestShadow_UnknownExperimentFailsClosed(t *testing.T) {
	sink := &recordingSink{}
	srv, c := newServer(t, sink, 0)
	s := open(t, c, "x-shadowbox-role", "shadow", "x-shadowbox-experiment", "nope")

	send(t, s, reqHeaders("x-request-id", "r-3"))
	r, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ir := r.GetImmediateResponse(); ir == nil || ir.GetStatus().GetCode() != 503 {
		t.Fatalf("reply = %v, want immediate 503", r)
	}
	if srv.Stats().Rejected != 1 || len(sink.all()) != 0 {
		t.Fatalf("Rejected = %d, sink = %d; want 1 and 0", srv.Stats().Rejected, len(sink.all()))
	}
}

func TestShadow_UnrewritableBodyFailsClosed(t *testing.T) {
	srv, c := newServer(t, &recordingSink{}, 0)
	s := open(t, c, "x-shadowbox-role", "shadow", "x-shadowbox-experiment", "support-v2")

	send(t, s, reqHeaders("x-request-id", "r-4"))
	_, _ = s.Recv()
	send(t, s, reqBody(`not json`, true))
	r, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if r.GetImmediateResponse().GetStatus().GetCode() != 503 {
		t.Fatalf("reply = %v, want immediate 503 (never forward an un-rewritten copy)", r)
	}
	if srv.Stats().RewriteFailures != 1 {
		t.Fatalf("RewriteFailures = %d, want 1", srv.Stats().RewriteFailures)
	}
}

func TestStreamWithoutRoleIsRejected(t *testing.T) {
	_, c := newServer(t, &recordingSink{}, 0)
	s := open(t, c)

	send(t, s, reqHeaders("x-request-id", "r-5", "x-shadowbox-role", "shadow"))
	_, err := s.Recv()

	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument (role must come from gRPC metadata, not HTTP headers)", err)
	}
}

func TestObserve_CapturesPrimaryWithoutReplying(t *testing.T) {
	sink := &recordingSink{}
	_, c := newServer(t, sink, 0)
	s := open(t, c, "x-shadowbox-role", "observe")

	send(t, s, reqHeaders("x-request-id", "r-6", "traceparent", traceparent))
	send(t, s, respHeaders(":status", "200"))
	half := len(refundResponse) / 2
	send(t, s, respBody(refundResponse[:half], false))
	send(t, s, respBody(refundResponse[half:], false))
	send(t, s, respBody("", true))
	_ = s.CloseSend()

	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv = %v, want EOF: observe mode must never reply", err)
	}
	got := sink.waitFor(t, 1)
	if len(got) != 1 {
		t.Fatalf("observations = %d, want exactly 1", len(got))
	}
	o := got[0]
	if o.Role != observation.Primary || o.RequestID != "r-6" || o.Status != 200 || o.Decision.Kind != observation.ToolCalls {
		t.Fatalf("observation = %+v, want primary r-6 200 tool_calls", o)
	}
}

func TestObserve_FinalizesWhenStreamEndsWithoutEndOfStream(t *testing.T) {
	sink := &recordingSink{}
	_, c := newServer(t, sink, 0)
	s := open(t, c, "x-shadowbox-role", "observe")

	send(t, s, reqHeaders("x-request-id", "r-7"))
	send(t, s, respHeaders(":status", "200"))
	send(t, s, respBody(refundResponse, false))
	_ = s.CloseSend()
	_, _ = s.Recv()

	o := sink.waitFor(t, 1)[0]
	if o.RequestID != "r-7" || o.Decision.Kind != observation.ToolCalls {
		t.Fatalf("observation = %+v, want r-7 tool_calls captured on stream end", o)
	}
}

func TestObserve_IgnoresShadowHeaderOnLivePath(t *testing.T) {
	sink := &recordingSink{}
	_, c := newServer(t, sink, 0)
	s := open(t, c, "x-shadowbox-role", "observe")

	send(t, s, reqHeaders("x-request-id", "r-8", "x-shadowbox-experiment", "support-v2"))
	send(t, s, reqBody(chatRequest, true))
	send(t, s, respHeaders(":status", "200"))
	send(t, s, respBody(refundResponse, true))
	_ = s.CloseSend()

	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv = %v, want EOF: a client header must not switch the live path to shadow", err)
	}
	if o := sink.waitFor(t, 1)[0]; o.Role != observation.Primary || o.Experiment != "" {
		t.Fatalf("observation = %+v, want primary with no experiment", o)
	}
}

func TestObserve_OversizedBodyBecomesErrorDecision(t *testing.T) {
	sink := &recordingSink{}
	_, c := newServer(t, sink, 64)
	s := open(t, c, "x-shadowbox-role", "observe")

	send(t, s, reqHeaders("x-request-id", "r-9"))
	send(t, s, respHeaders(":status", "200"))
	send(t, s, respBody(refundResponse, true))
	_ = s.CloseSend()
	_, _ = s.Recv()

	if o := sink.waitFor(t, 1)[0]; o.Decision.Kind != observation.Error {
		t.Fatalf("decision = %v, want error for a truncated body", o.Decision.Kind)
	}
}
