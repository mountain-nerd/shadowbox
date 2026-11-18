// Package mcpimport reads MCP tool annotations (tools/list) into effect hints
// using the official MCP Go SDK. Per the MCP spec, annotations are untrusted
// unless the server is trusted; effect.Registry.MergeHints enforces that.
package mcpimport

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mountain-nerd/shadowbox/internal/effect"
)

// FetchHints lists every tool on an open session (following pagination) and
// returns its annotations as hints.
func FetchHints(ctx context.Context, cs *mcp.ClientSession) (map[string]effect.Hints, error) {
	hints := map[string]effect.Hints{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcpimport: tools/list: %w", err)
		}
		hints[tool.Name] = toHints(tool.Annotations)
	}
	return hints, nil
}

// FetchURL connects to a streamable HTTP MCP server, reads its tools and disconnects.
func FetchURL(ctx context.Context, url string) (map[string]effect.Hints, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "shadowbox", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpimport: connect %s: %w", url, err)
	}
	defer cs.Close()
	return FetchHints(ctx, cs)
}

func toHints(a *mcp.ToolAnnotations) effect.Hints {
	if a == nil {
		return effect.Hints{}
	}
	readOnly, idempotent := a.ReadOnlyHint, a.IdempotentHint
	return effect.Hints{ReadOnly: &readOnly, Destructive: a.DestructiveHint, Idempotent: &idempotent}
}
