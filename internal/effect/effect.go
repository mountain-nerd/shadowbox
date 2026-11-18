// Package effect classifies tools by the side effects they can cause.
//
// The classification is fail-safe: a tool nobody has classified is Unknown,
// and Unknown is treated as a write. MCP tool annotations are untrusted hints
// (per the MCP spec) and may only tighten an operator's classification unless
// the server is explicitly trusted.
package effect

import "fmt"

// Class is a tool's effect class. The numeric order is the strictness order
// used when merging classifications: higher is stricter.
type Class int

const (
	// ReadOnly tools cannot change state.
	ReadOnly Class = iota + 1
	// Idempotent tools change state, but repeating a call is safe.
	Idempotent
	// Irreversible tools change state in ways that are not safe to repeat or undo.
	Irreversible
	// Unknown tools have no classification and are treated as the strictest class.
	Unknown
)

// IsWrite reports whether a tool of this class may change state.
func (c Class) IsWrite() bool { return c != ReadOnly }

func (c Class) String() string {
	switch c {
	case ReadOnly:
		return "read-only"
	case Idempotent:
		return "idempotent"
	case Irreversible:
		return "irreversible"
	default:
		return "unknown"
	}
}

// ParseClass parses the textual form used in configuration.
func ParseClass(s string) (Class, error) {
	switch s {
	case "read-only":
		return ReadOnly, nil
	case "idempotent":
		return Idempotent, nil
	case "irreversible":
		return Irreversible, nil
	}
	return Unknown, fmt.Errorf("effect: unknown class %q (want read-only, idempotent or irreversible)", s)
}

// Hints mirrors MCP tool annotations. A nil field means "not set".
type Hints struct {
	ReadOnly    *bool
	Destructive *bool
	Idempotent  *bool
}

// FromHints derives a class from MCP annotations using the MCP defaults:
// readOnlyHint=false, destructiveHint=true, idempotentHint=false.
func FromHints(h Hints) Class {
	if isTrue(h.ReadOnly) {
		return ReadOnly
	}
	destructive := h.Destructive == nil || *h.Destructive
	if !destructive && isTrue(h.Idempotent) {
		return Idempotent
	}
	return Irreversible
}

func isTrue(b *bool) bool { return b != nil && *b }

// Classifier classifies tools by name.
type Classifier interface {
	Classify(tool string) Class
}

// Registry is an operator-maintained tool classification. It is immutable;
// MergeHints returns a new Registry.
type Registry struct {
	classes map[string]Class
}

// NewRegistry copies m into a new Registry.
func NewRegistry(m map[string]Class) *Registry {
	c := make(map[string]Class, len(m))
	for k, v := range m {
		c[k] = v
	}
	return &Registry{classes: c}
}

// Classify returns the tool's class, or Unknown when it is not registered.
func (r *Registry) Classify(tool string) Class {
	if c, ok := r.classes[tool]; ok {
		return c
	}
	return Unknown
}

// MergeHints returns a new Registry with MCP hints applied. Hints only ever
// make a registered tool stricter. Unregistered tools are classified from
// hints only when the hint source is trusted.
func (r *Registry) MergeHints(hints map[string]Hints, trusted bool) *Registry {
	out := NewRegistry(r.classes)
	for tool, h := range hints {
		hinted := FromHints(h)
		current, registered := out.classes[tool]
		switch {
		case registered && hinted > current:
			out.classes[tool] = hinted
		case !registered && trusted:
			out.classes[tool] = hinted
		}
	}
	return out
}
