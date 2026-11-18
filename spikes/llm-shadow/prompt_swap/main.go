// prompt-swap: a throwaway Envoy ext_proc server for the *shadow* gateway.
//
// It runs on the shadow gateway's normal request path (not on a mirror leg),
// buffers the OpenAI-style request body and rewrites it into the candidate
// variant:
//   - model            -> $CANDIDATE_MODEL
//   - system message   -> contents of $CANDIDATE_PROMPT_FILE
//   - non-function tools (provider-executed, e.g. web_search) are stripped so
//     the shadow call can have no side effects
//   - store / previous_response_id removed (no provider-side state)
package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"strconv"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
)

type server struct {
	extproc.UnimplementedExternalProcessorServer
	model  string
	prompt string
}

func (s *server) rewrite(raw []byte) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if s.model != "" {
		body["model"] = s.model
	}
	if msgs, ok := body["messages"].([]any); ok && s.prompt != "" {
		replaced := false
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] == "system" {
				mm["content"] = s.prompt
				replaced = true
			}
		}
		if !replaced {
			body["messages"] = append([]any{map[string]any{"role": "system", "content": s.prompt}}, msgs...)
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		kept := []any{}
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok && tm["type"] == "function" {
				kept = append(kept, t)
			}
		}
		body["tools"] = kept
	}
	delete(body, "store")
	delete(body, "previous_response_id")
	return json.Marshal(body)
}

func (s *server) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		resp := &extproc.ProcessingResponse{}
		switch v := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			resp.Response = &extproc.ProcessingResponse_RequestHeaders{RequestHeaders: &extproc.HeadersResponse{}}
		case *extproc.ProcessingRequest_RequestBody:
			out, err := s.rewrite(v.RequestBody.Body)
			cr := &extproc.CommonResponse{}
			if err != nil {
				log.Printf("rewrite failed, passing through: %v", err)
			} else {
				cr.BodyMutation = &extproc.BodyMutation{Mutation: &extproc.BodyMutation_Body{Body: out}}
				cr.HeaderMutation = &extproc.HeaderMutation{SetHeaders: []*corev3.HeaderValueOption{
					{Header: &corev3.HeaderValue{Key: "content-length", RawValue: []byte(strconv.Itoa(len(out)))}},
					{Header: &corev3.HeaderValue{Key: "x-agentmesh-candidate", RawValue: []byte(s.model)}},
				}}
			}
			resp.Response = &extproc.ProcessingResponse_RequestBody{RequestBody: &extproc.BodyResponse{Response: cr}}
		default:
			// Response phases are skipped via processing_mode; ack anything else.
			continue
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func main() {
	prompt, err := os.ReadFile(os.Getenv("CANDIDATE_PROMPT_FILE"))
	if err != nil {
		log.Fatalf("read prompt: %v", err)
	}
	lis, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatal(err)
	}
	g := grpc.NewServer()
	extproc.RegisterExternalProcessorServer(g, &server{model: os.Getenv("CANDIDATE_MODEL"), prompt: string(prompt)})
	log.Printf("prompt-swap ext_proc listening on :9000 (model=%s)", os.Getenv("CANDIDATE_MODEL"))
	log.Fatal(g.Serve(lis))
}
