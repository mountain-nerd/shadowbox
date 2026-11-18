// Package openai implements the llm ports for the OpenAI chat completions format.
package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/mountain-nerd/shadowbox/internal/observation"
)

// Codec parses chat.completion responses.
type Codec struct{}

type function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Function function `json:"function"`
			} `json:"tool_calls"`
			// FunctionCall is the deprecated single-function form; still a tool call.
			FunctionCall *function `json:"function_call"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

var errorDecision = observation.Decision{Kind: observation.Error}

// ParseResponse implements llm.ResponseParser. Tool calls are collected from
// every choice (including the legacy function_call form), so a write proposed
// in any choice is seen.
func (Codec) ParseResponse(status int, body []byte) (observation.Decision, observation.Usage) {
	if status >= 400 {
		return errorDecision, observation.Usage{}
	}
	var r chatResponse
	if err := json.Unmarshal(body, &r); err != nil || len(r.Choices) == 0 {
		return errorDecision, observation.Usage{}
	}
	var usage observation.Usage
	if r.Usage != nil {
		usage = observation.Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens, Known: true}
	}
	var calls []observation.ToolCall
	for _, ch := range r.Choices {
		for _, c := range ch.Message.ToolCalls {
			calls = append(calls, observation.ToolCall{Name: c.Function.Name, ArgsHash: hashArgs(c.Function.Arguments)})
		}
		if f := ch.Message.FunctionCall; f != nil {
			calls = append(calls, observation.ToolCall{Name: f.Name, ArgsHash: hashArgs(f.Arguments)})
		}
	}
	if len(calls) == 0 {
		return observation.Decision{Kind: observation.Text}, usage
	}
	return observation.Decision{Kind: observation.ToolCalls, ToolCalls: calls}, usage
}

// hashArgs hashes the canonical JSON form of the arguments, so key order and
// whitespace do not matter. Arguments that are not valid JSON are hashed raw.
func hashArgs(args string) string {
	canonical := []byte(args)
	var v any
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		if b, err := json.Marshal(v); err == nil { // encoding/json sorts map keys
			canonical = b
		}
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// ModelOverride sets the model.
type ModelOverride struct{ Model string }

func (m ModelOverride) Mutate(req map[string]any) error {
	req["model"] = m.Model
	return nil
}

// SystemPrompt replaces every system message's content, or inserts a system
// message first when there is none.
type SystemPrompt struct{ Text string }

var errMessages = errors.New("messages is not an array of objects")

func (s SystemPrompt) Mutate(req map[string]any) error {
	raw, ok := req["messages"]
	if !ok {
		req["messages"] = []any{map[string]any{"role": "system", "content": s.Text}}
		return nil
	}
	msgs, ok := raw.([]any)
	if !ok {
		return errMessages
	}
	replaced := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			return errMessages
		}
		if mm["role"] == "system" {
			mm["content"] = s.Text
			replaced = true
		}
	}
	if !replaced {
		req["messages"] = append([]any{map[string]any{"role": "system", "content": s.Text}}, msgs...)
	}
	return nil
}

// StripProviderTools removes tools that execute inside the provider (anything
// that is not a client-side "function" tool), so a shadow call cannot cause
// side effects.
type StripProviderTools struct{}

func (StripProviderTools) Mutate(req map[string]any) error {
	tools, ok := req["tools"].([]any)
	if !ok {
		return nil
	}
	kept := make([]any, 0, len(tools))
	for _, t := range tools {
		if tm, ok := t.(map[string]any); ok && tm["type"] == "function" {
			kept = append(kept, t)
		}
	}
	req["tools"] = kept
	return nil
}

// AllowFields refuses any top-level request field outside a known-safe
// allowlist, so the shadow copy fails closed (503) instead of carrying an
// option that runs something inside the provider or stores state there.
// Extra adds fields the operator explicitly keeps.
type AllowFields struct{ Extra []string }

// allowedFields are chat completions request fields that neither execute
// anything inside the provider nor store state there. Deliberately absent:
// web_search_options (provider-run web search), store and
// previous_response_id (provider-side state).
var allowedFields = map[string]bool{
	"model": true, "messages": true, "tools": true, "tool_choice": true, "parallel_tool_calls": true,
	"functions": true, "function_call": true, // legacy function calling
	"temperature": true, "top_p": true, "n": true, "stop": true, "seed": true,
	"max_tokens": true, "max_completion_tokens": true, "presence_penalty": true, "frequency_penalty": true,
	"logit_bias": true, "logprobs": true, "top_logprobs": true, "response_format": true,
	"reasoning_effort": true, "verbosity": true, "modalities": true, "audio": true, "prediction": true,
	"stream": true, "stream_options": true, "service_tier": true, "metadata": true,
	"user": true, "safety_identifier": true, "prompt_cache_key": true,
}

func (a AllowFields) Mutate(req map[string]any) error {
	for k := range req {
		if !allowedFields[k] && !slices.Contains(a.Extra, k) {
			return fmt.Errorf("field %q is not on the shadow allowlist", k)
		}
	}
	return nil
}

// DropStatefulFields removes fields that make the provider store or reuse state.
type DropStatefulFields struct{}

func (DropStatefulFields) Mutate(req map[string]any) error {
	delete(req, "store")
	delete(req, "previous_response_id")
	return nil
}
