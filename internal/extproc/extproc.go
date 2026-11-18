// Package extproc adapts shadowbox to Envoy's external processing (ext_proc)
// API. One gRPC service serves two roles, chosen per stream from gRPC
// metadata that the operator sets on the Envoy side (grpc_service
// initial_metadata), never from client-controlled HTTP headers:
//
//   - observe: on the live gateway, with ext_proc observability_mode. Envoy
//     does not wait for us; we never reply. We only capture the primary's
//     response. Combined with failure_mode_allow: true, the live path cannot
//     be slowed down or broken by shadowbox.
//   - shadow: on the shadow gateway, synchronous and fail-closed. We rewrite
//     the mirrored request into the candidate variant and capture the
//     candidate's response. If anything is wrong we answer with an immediate
//     503 so an un-rewritten copy never reaches the candidate.
package extproc

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/mountain-nerd/shadowbox/internal/llm"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

// gRPC metadata keys set by the operator in Envoy's grpc_service.initial_metadata.
const (
	MetadataRole       = "x-shadowbox-role"
	MetadataExperiment = "x-shadowbox-experiment"
	// MetadataToken carries the shared secret that authenticates a stream.
	MetadataToken = "x-shadowbox-token"
	// ShadowHeader is the experiment marker the prod gateway adds to mirrored
	// copies. It is only read on the shadow role, as a fallback.
	ShadowHeader = "x-shadowbox-experiment"
)

// forwardedHeaders is the allowlist of request headers a shadow copy keeps.
// Every other header (credentials, cookies, any provider's key header,
// custom secrets, baggage) is removed before the copy reaches the candidate.
// Pseudo-headers (":path" etc.) and Envoy's own x-envoy-* headers are left
// to Envoy.
var forwardedHeaders = map[string]bool{
	"host": true, "content-type": true, "content-length": true, "accept": true,
	"accept-encoding": true, "user-agent": true, "traceparent": true, "tracestate": true,
	"x-request-id": true, ShadowHeader: true,
}

// removedHeaders lists the request headers that are not allowlisted.
func removedHeaders(h *extprocv3.HttpHeaders) []string {
	var out []string
	for _, kv := range h.GetHeaders().GetHeaders() {
		k := strings.ToLower(kv.GetKey())
		if forwardedHeaders[k] || strings.HasPrefix(k, ":") || strings.HasPrefix(k, "x-envoy-") {
			continue
		}
		out = append(out, k)
	}
	return out
}

// Sink receives captured observations (implemented by pairing.Pairer).
type Sink interface{ Add(observation.Observation) }

// Experiment is how one experiment's candidate is produced.
type Experiment struct {
	// Model is recorded on shadow observations.
	Model string
	// Rewrite turns the primary request into the candidate request.
	Rewrite llm.Pipeline
}

// Options configures the Server.
type Options struct {
	// Token is the shared secret every stream must present in the
	// x-shadowbox-token gRPC metadata. An empty Token rejects every stream:
	// anyone who can open a stream can feed the verdict.
	Token       string
	Parser      llm.ResponseParser
	Experiments map[string]Experiment
	Sink        Sink
	// MaxBodyBytes bounds captured response bodies (default 4 MiB). A larger
	// body is recorded as an Error decision.
	MaxBodyBytes int
}

// Stats counts shadow streams that were refused.
type Stats struct {
	// Rejected streams had no known experiment.
	Rejected int64 `json:"rejected"`
	// RewriteFailures had a body that could not be rewritten.
	RewriteFailures int64 `json:"rewriteFailures"`
}

// Server implements extprocv3.ExternalProcessorServer.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer
	opts            Options
	rejected        atomic.Int64
	rewriteFailures atomic.Int64
}

// NewServer creates a Server.
func NewServer(o Options) *Server {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 4 << 20
	}
	return &Server{opts: o}
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{Rejected: s.rejected.Load(), RewriteFailures: s.rewriteFailures.Load()}
}

// handler is the per-stream strategy for a role.
type handler interface {
	// handle returns the reply to send (nil for none) and whether to stop.
	handle(*extprocv3.ProcessingRequest) (*extprocv3.ProcessingResponse, bool)
	// finish runs once when the stream ends.
	finish()
}

// Process implements the ext_proc bidirectional stream.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	if s.opts.Token == "" || subtle.ConstantTimeCompare([]byte(first(md, MetadataToken)), []byte(s.opts.Token)) != 1 {
		return status.Errorf(codes.Unauthenticated, "shadowbox: missing or invalid %s", MetadataToken)
	}
	var h handler
	switch first(md, MetadataRole) {
	case "observe":
		h = &observeHandler{call: s.newCall(observation.Primary, "", "")}
	case "shadow":
		h = &shadowHandler{server: s, metaExperiment: first(md, MetadataExperiment), call: s.newCall(observation.Shadow, "", "")}
	default:
		return status.Errorf(codes.InvalidArgument, "shadowbox: gRPC metadata %q must be \"observe\" or \"shadow\"", MetadataRole)
	}
	defer h.finish()
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		resp, stop := h.handle(req)
		if resp != nil {
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
		if stop {
			return nil
		}
	}
}

func first(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

// call captures one HTTP exchange.
type call struct {
	server    *Server
	obs       observation.Observation
	body      bytes.Buffer
	overflow  bool
	responded bool
	emitted   bool
}

func (s *Server) newCall(role observation.Role, experiment, model string) *call {
	return &call{server: s, obs: observation.Observation{Role: role, Experiment: experiment, Model: model}}
}

func (c *call) requestHeaders(h *extprocv3.HttpHeaders) {
	c.obs.RequestID = header(h, "x-request-id")
	c.obs.TraceID = observation.TraceIDFromTraceparent(header(h, "traceparent"))
}

func (c *call) responseHeaders(h *extprocv3.HttpHeaders) {
	c.responded = true
	c.obs.Status, _ = strconv.Atoi(header(h, ":status"))
}

func (c *call) responseBody(b *extprocv3.HttpBody) {
	if c.body.Len()+len(b.GetBody()) > c.server.opts.MaxBodyBytes {
		c.overflow = true
		return
	}
	c.body.Write(b.GetBody())
}

// emit sends the observation once, if a response was seen.
func (c *call) emit() {
	if c.emitted || !c.responded {
		return
	}
	c.emitted = true
	if c.overflow {
		c.obs.Decision = observation.Decision{Kind: observation.Error}
	} else {
		c.obs.Decision, c.obs.Usage = c.server.opts.Parser.ParseResponse(c.obs.Status, c.body.Bytes())
	}
	c.obs.At = time.Now()
	c.server.opts.Sink.Add(c.obs)
}

func header(h *extprocv3.HttpHeaders, key string) string {
	for _, kv := range h.GetHeaders().GetHeaders() {
		if kv.GetKey() == key {
			if v := kv.GetRawValue(); len(v) > 0 {
				return string(v)
			}
			return kv.GetValue()
		}
	}
	return ""
}

// observeHandler never replies (Envoy runs it in observability_mode).
type observeHandler struct{ call *call }

func (o *observeHandler) handle(req *extprocv3.ProcessingRequest) (*extprocv3.ProcessingResponse, bool) {
	switch v := req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		o.call.requestHeaders(v.RequestHeaders)
	case *extprocv3.ProcessingRequest_ResponseHeaders:
		o.call.responseHeaders(v.ResponseHeaders)
	case *extprocv3.ProcessingRequest_ResponseBody:
		o.call.responseBody(v.ResponseBody)
		if v.ResponseBody.GetEndOfStream() {
			o.call.emit()
		}
	}
	return nil, false
}

func (o *observeHandler) finish() { o.call.emit() }

// shadowHandler rewrites the mirrored request and fails closed.
type shadowHandler struct {
	server         *Server
	metaExperiment string
	experiment     Experiment
	call           *call
}

func (s *shadowHandler) handle(req *extprocv3.ProcessingRequest) (*extprocv3.ProcessingResponse, bool) {
	switch v := req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		s.call.requestHeaders(v.RequestHeaders)
		id := s.metaExperiment
		if id == "" {
			id = header(v.RequestHeaders, ShadowHeader)
		}
		exp, ok := s.server.opts.Experiments[id]
		if !ok {
			s.server.rejected.Add(1)
			return reject("shadowbox: unknown experiment"), true
		}
		s.experiment = exp
		s.call.obs.Experiment, s.call.obs.Model = id, exp.Model
		// Only allowlisted headers reach the candidate, so production
		// credentials never do; the shadow gateway injects the candidate's own.
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestHeaders{RequestHeaders: &extprocv3.HeadersResponse{
			Response: &extprocv3.CommonResponse{HeaderMutation: &extprocv3.HeaderMutation{RemoveHeaders: removedHeaders(v.RequestHeaders)}},
		}}}, false

	case *extprocv3.ProcessingRequest_RequestBody:
		out, err := s.experiment.Rewrite.Apply(v.RequestBody.GetBody())
		if err != nil {
			s.server.rewriteFailures.Add(1)
			return reject("shadowbox: request could not be rewritten"), true
		}
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestBody{RequestBody: &extprocv3.BodyResponse{
			Response: &extprocv3.CommonResponse{
				BodyMutation: &extprocv3.BodyMutation{Mutation: &extprocv3.BodyMutation_Body{Body: out}},
				HeaderMutation: &extprocv3.HeaderMutation{SetHeaders: []*corev3.HeaderValueOption{{
					Header: &corev3.HeaderValue{Key: "content-length", RawValue: []byte(strconv.Itoa(len(out)))},
				}}},
			},
		}}}, false

	case *extprocv3.ProcessingRequest_ResponseHeaders:
		s.call.responseHeaders(v.ResponseHeaders)
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseHeaders{ResponseHeaders: &extprocv3.HeadersResponse{}}}, false

	case *extprocv3.ProcessingRequest_ResponseBody:
		s.call.responseBody(v.ResponseBody)
		if v.ResponseBody.GetEndOfStream() {
			s.call.emit()
		}
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseBody{ResponseBody: &extprocv3.BodyResponse{}}}, false
	}
	return nil, false
}

func (s *shadowHandler) finish() { s.call.emit() }

func reject(details string) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ImmediateResponse{ImmediateResponse: &extprocv3.ImmediateResponse{
		Status:  &typev3.HttpStatus{Code: typev3.StatusCode_ServiceUnavailable},
		Body:    []byte(`{"error":"` + details + `"}`),
		Details: details,
	}}}
}
