package mcpimport_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mountain-nerd/shadowbox/internal/effect"
	"github.com/mountain-nerd/shadowbox/internal/mcpimport"
)

type noArgs struct{}

func f(b bool) *bool { return &b }

// toolServer is a real MCP server exposing three tools with different annotations.
func toolServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "shop-tools", Version: "1.0.0"}, nil)
	handler := func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lookup_order", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, handler)
	mcp.AddTool(s, &mcp.Tool{Name: "issue_refund"}, handler) // no annotations: MCP default is destructive
	mcp.AddTool(s, &mcp.Tool{Name: "tag_ticket", Annotations: &mcp.ToolAnnotations{DestructiveHint: f(false), IdempotentHint: true}}, handler)
	return s
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestFetchHints_InMemory(t *testing.T) {
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := toolServer().Connect(ctx(t), serverT, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "shadowbox-test", Version: "0"}, nil).Connect(ctx(t), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	hints, err := mcpimport.FetchHints(ctx(t), cs)
	if err != nil {
		t.Fatalf("FetchHints: %v", err)
	}

	want := map[string]effect.Class{"lookup_order": effect.ReadOnly, "issue_refund": effect.Irreversible, "tag_ticket": effect.Idempotent}
	if len(hints) != len(want) {
		t.Fatalf("got %d tools, want %d: %+v", len(hints), len(want), hints)
	}
	for tool, cls := range want {
		if got := effect.FromHints(hints[tool]); got != cls {
			t.Errorf("%s classified %v, want %v", tool, got, cls)
		}
	}
}

func TestFetchURL_StreamableHTTP(t *testing.T) {
	srv := toolServer()
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)

	hints, err := mcpimport.FetchURL(ctx(t), ts.URL)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}

	// Untrusted hints only tighten: the operator said sync is read-only, the
	// server's (missing) annotations say destructive, so it becomes irreversible;
	// lookup_order is not registered and stays Unknown despite its read-only hint.
	reg := effect.NewRegistry(map[string]effect.Class{"issue_refund": effect.ReadOnly}).MergeHints(hints, false)
	if got := reg.Classify("issue_refund"); got != effect.Irreversible {
		t.Errorf("issue_refund = %v, want Irreversible after tightening", got)
	}
	if got := reg.Classify("lookup_order"); got != effect.Unknown {
		t.Errorf("lookup_order = %v, want Unknown (untrusted hints never classify unregistered tools)", got)
	}
}

func TestFetchURL_UnreachableServerIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close()

	if _, err := mcpimport.FetchURL(ctx(t), ts.URL); err == nil {
		t.Fatal("err = nil, want error for an unreachable server")
	}
}
