// Package llm defines provider-neutral ports for reading LLM responses and
// rewriting LLM requests. Provider formats implement them as adapters.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mountain-nerd/shadowbox/internal/observation"
)

// ResponseParser extracts the model's decision and token usage from a
// response. It never fails: anything it cannot understand is an Error decision.
type ResponseParser interface {
	ParseResponse(status int, body []byte) (observation.Decision, observation.Usage)
}

// RequestMutator rewrites a decoded JSON request body in place.
type RequestMutator interface {
	Mutate(req map[string]any) error
}

// Pipeline applies mutators in order (chain of responsibility). Fields the
// mutators do not touch are preserved.
type Pipeline []RequestMutator

// ErrNotObject is returned when the body is not a JSON object.
var ErrNotObject = errors.New("llm: request body is not a JSON object")

// Apply decodes body, runs every mutator and re-encodes the result.
func (p Pipeline) Apply(body []byte) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		return nil, ErrNotObject
	}
	for _, m := range p {
		if err := m.Mutate(req); err != nil {
			return nil, fmt.Errorf("llm: %T: %w", m, err)
		}
	}
	return json.Marshal(req)
}
