// Throwaway probe: logs everything Envoy sends over ext_proc, with the role from gRPC metadata.
package main

import (
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type srv struct{ extproc.UnimplementedExternalProcessorServer }

func hdr(h *extproc.HttpHeaders, names ...string) string {
	var out []string
	for _, kv := range h.GetHeaders().GetHeaders() {
		for _, n := range names {
			if kv.Key == n {
				out = append(out, kv.Key+"="+string(kv.RawValue))
			}
		}
	}
	return strings.Join(out, " ")
}

func (srv) Process(s extproc.ExternalProcessor_ProcessServer) error {
	md, _ := metadata.FromIncomingContext(s.Context())
	role := strings.Join(md.Get("x-shadowbox-role"), ",")
	for {
		req, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			log.Printf("[%s] recv err: %v", role, err)
			return err
		}
		if role == "observe" {
			time.Sleep(700 * time.Millisecond) // prove we don't block the client
		}
		resp := &extproc.ProcessingResponse{}
		switch v := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			log.Printf("[%s] REQ_HEADERS %s", role, hdr(v.RequestHeaders, ":authority", "x-request-id", "traceparent", "x-agentmesh-shadow"))
			resp.Response = &extproc.ProcessingResponse_RequestHeaders{RequestHeaders: &extproc.HeadersResponse{}}
		case *extproc.ProcessingRequest_RequestBody:
			log.Printf("[%s] REQ_BODY %d bytes eos=%v", role, len(v.RequestBody.Body), v.RequestBody.EndOfStream)
			resp.Response = &extproc.ProcessingResponse_RequestBody{RequestBody: &extproc.BodyResponse{}}
		case *extproc.ProcessingRequest_ResponseHeaders:
			log.Printf("[%s] RESP_HEADERS %s", role, hdr(v.ResponseHeaders, ":status", "content-type"))
			resp.Response = &extproc.ProcessingResponse_ResponseHeaders{ResponseHeaders: &extproc.HeadersResponse{}}
		case *extproc.ProcessingRequest_ResponseBody:
			b := string(v.ResponseBody.Body)
			if len(b) > 60 {
				b = b[:60]
			}
			log.Printf("[%s] RESP_BODY %d bytes eos=%v %q", role, len(v.ResponseBody.Body), v.ResponseBody.EndOfStream, b)
			resp.Response = &extproc.ProcessingResponse_ResponseBody{ResponseBody: &extproc.BodyResponse{}}
		default:
			continue
		}
		if role != "observe" {
			if err := s.Send(resp); err != nil {
				return err
			}
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatal(err)
	}
	log.SetOutput(os.Stdout)
	g := grpc.NewServer()
	extproc.RegisterExternalProcessorServer(g, srv{})
	log.Fatal(g.Serve(lis))
}
