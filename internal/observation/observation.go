// Package observation holds the value types shared across shadowbox.
//
// Observations deliberately carry no prompt text and no tool arguments:
// tool arguments are reduced to a hash so decisions can be compared without
// retaining user data.
package observation

import (
	"sort"
	"strings"
	"time"
)

// Role says which side of a mirror an observation came from.
type Role int

const (
	Primary Role = iota + 1
	Shadow
)

func (r Role) String() string {
	switch r {
	case Primary:
		return "primary"
	case Shadow:
		return "shadow"
	}
	return "unknown"
}

// DecisionKind is what the model chose to do.
type DecisionKind int

const (
	// Text means the model answered in text.
	Text DecisionKind = iota + 1
	// ToolCalls means the model proposed one or more tool calls.
	ToolCalls
	// Error means the call failed or the response could not be understood.
	Error
)

func (k DecisionKind) String() string {
	switch k {
	case Text:
		return "text"
	case ToolCalls:
		return "tool_calls"
	case Error:
		return "error"
	}
	return "unknown"
}

// ToolCall is a proposed tool call. ArgsHash is a hash of the canonical
// JSON arguments, never the arguments themselves.
type ToolCall struct {
	Name     string
	ArgsHash string
}

// Decision is the model's choice for one call.
type Decision struct {
	Kind      DecisionKind
	ToolCalls []ToolCall
}

// ToolNames returns the distinct tool names, sorted.
func (d Decision) ToolNames() []string {
	seen := make(map[string]struct{}, len(d.ToolCalls))
	names := make([]string, 0, len(d.ToolCalls))
	for _, c := range d.ToolCalls {
		if _, ok := seen[c.Name]; ok {
			continue
		}
		seen[c.Name] = struct{}{}
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

// Usage is token usage as reported by the provider. Known is false when the
// response carried no usage.
type Usage struct {
	InputTokens, OutputTokens int
	Known                     bool
}

// Total returns input plus output tokens.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Observation is one captured LLM call.
type Observation struct {
	// RequestID pairs a primary call with its mirrored copy (Envoy x-request-id).
	RequestID string
	// TraceID is the W3C trace id; many calls can share one.
	TraceID    string
	Experiment string
	Role       Role
	Model      string
	Status     int
	Decision   Decision
	Usage      Usage
	At         time.Time
}

// Pair is a primary call joined with its shadow copy.
type Pair struct {
	Experiment string
	Primary    Observation
	Shadow     Observation
}

// TraceIDFromTraceparent extracts the trace id from a W3C traceparent header,
// returning "" when the header is malformed or the trace id is all zeros.
func TraceIDFromTraceparent(s string) string {
	parts := strings.Split(s, "-")
	if len(parts) != 4 || len(parts[1]) != 32 || !isHex(parts[1]) || strings.Trim(parts[1], "0") == "" {
		return ""
	}
	return parts[1]
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
