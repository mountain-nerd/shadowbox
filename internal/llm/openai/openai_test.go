package openai_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mountain-nerd/shadowbox/internal/llm"
	"github.com/mountain-nerd/shadowbox/internal/llm/openai"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

// A complete chat.completion response with a tool call, as returned by the API.
const toolCallResponse = `{
  "id": "chatcmpl-1", "object": "chat.completion", "created": 1794929716, "model": "gpt-x",
  "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
    "role": "assistant", "content": null,
    "tool_calls": [{"id": "call_1", "type": "function",
      "function": {"name": "issue_refund", "arguments": "{\"reason\":\"damaged\",\"order_id\":\"A-1001\"}"}}]}}],
  "usage": {"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150}
}`

const textResponse = `{
  "id": "chatcmpl-2", "object": "chat.completion", "created": 1794929716, "model": "gpt-x",
  "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "Hello"}}]
}`

func TestParseResponse_ToolCall(t *testing.T) {
	d, u := openai.Codec{}.ParseResponse(200, []byte(toolCallResponse))

	if d.Kind != observation.ToolCalls {
		t.Fatalf("Kind = %v, want ToolCalls", d.Kind)
	}
	if len(d.ToolCalls) != 1 || d.ToolCalls[0].Name != "issue_refund" {
		t.Fatalf("ToolCalls = %+v, want one issue_refund call", d.ToolCalls)
	}
	if d.ToolCalls[0].ArgsHash == "" || strings.Contains(d.ToolCalls[0].ArgsHash, "A-1001") {
		t.Fatalf("ArgsHash = %q, want a non-empty hash that does not contain the raw arguments", d.ToolCalls[0].ArgsHash)
	}
	want := observation.Usage{InputTokens: 120, OutputTokens: 30, Known: true}
	if u != want {
		t.Fatalf("Usage = %+v, want %+v", u, want)
	}
}

func TestParseResponse_ArgsHashIgnoresKeyOrderAndWhitespace(t *testing.T) {
	reordered := strings.Replace(toolCallResponse,
		`{\"reason\":\"damaged\",\"order_id\":\"A-1001\"}`,
		`{ \"order_id\": \"A-1001\", \"reason\": \"damaged\" }`, 1)
	changed := strings.Replace(toolCallResponse, `A-1001`, `A-9999`, 1)

	a, _ := openai.Codec{}.ParseResponse(200, []byte(toolCallResponse))
	b, _ := openai.Codec{}.ParseResponse(200, []byte(reordered))
	c, _ := openai.Codec{}.ParseResponse(200, []byte(changed))
	if len(a.ToolCalls) != 1 || len(b.ToolCalls) != 1 || len(c.ToolCalls) != 1 {
		t.Fatalf("expected one tool call in each response, got %d/%d/%d", len(a.ToolCalls), len(b.ToolCalls), len(c.ToolCalls))
	}

	if a.ToolCalls[0].ArgsHash != b.ToolCalls[0].ArgsHash {
		t.Errorf("hash differs for semantically equal arguments")
	}
	if a.ToolCalls[0].ArgsHash == c.ToolCalls[0].ArgsHash {
		t.Errorf("hash equal for different arguments")
	}
}

func TestParseResponse_Text(t *testing.T) {
	d, u := openai.Codec{}.ParseResponse(200, []byte(textResponse))

	if d.Kind != observation.Text || len(d.ToolCalls) != 0 {
		t.Fatalf("Decision = %+v, want Text with no tool calls", d)
	}
	if u.Known {
		t.Fatalf("Usage.Known = true for a response without usage")
	}
}

func TestParseResponse_ErrorStatusWinsOverWellFormedBody(t *testing.T) {
	d, _ := openai.Codec{}.ParseResponse(503, []byte(toolCallResponse))
	if d.Kind != observation.Error {
		t.Fatalf("Kind = %v, want Error: an error status must not be read as a decision", d.Kind)
	}
}

func TestParseResponse_LegacyFunctionCallIsAToolCall(t *testing.T) {
	legacy := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"function_call",
	  "message":{"role":"assistant","content":null,"function_call":{"name":"issue_refund","arguments":"{\"order_id\":\"A-1\"}"}}}]}`

	d, _ := openai.Codec{}.ParseResponse(200, []byte(legacy))

	if d.Kind != observation.ToolCalls || len(d.ToolCalls) != 1 || d.ToolCalls[0].Name != "issue_refund" {
		t.Fatalf("Decision = %+v, want the legacy function_call treated as a tool call", d)
	}
}

func TestParseResponse_ReadsEveryChoice(t *testing.T) {
	multi := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[
	  {"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Sure"}},
	  {"index":1,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
	    "tool_calls":[{"id":"call_1","type":"function","function":{"name":"issue_refund","arguments":"{}"}}]}}]}`

	d, _ := openai.Codec{}.ParseResponse(200, []byte(multi))

	if d.Kind != observation.ToolCalls || len(d.ToolCalls) != 1 || d.ToolCalls[0].Name != "issue_refund" {
		t.Fatalf("Decision = %+v, want the tool call from the second choice", d)
	}
}

func TestParseResponse_ErrorsBecomeErrorDecisions(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"http 500":      {500, `{"error":{"message":"overloaded"}}`},
		"invalid json":  {200, `{"choices": [`},
		"no choices":    {200, `{"id":"x","choices":[]}`},
		"empty body":    {200, ``},
		"http 429 text": {429, `rate limited`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d, _ := openai.Codec{}.ParseResponse(c.status, []byte(c.body))
			if d.Kind != observation.Error {
				t.Fatalf("Kind = %v, want Error", d.Kind)
			}
		})
	}
}

const request = `{"model":"current-model-v1","store":true,"previous_response_id":"r1","temperature":0.2,
 "messages":[{"role":"system","content":"v1 prompt"},{"role":"user","content":"refund please"}],
 "tools":[{"type":"function","function":{"name":"lookup_order"}},{"type":"web_search"}]}`

func applyJSON(t *testing.T, p llm.Pipeline, body string) map[string]any {
	t.Helper()
	out, err := p.Apply([]byte(body))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return m
}

func TestModelOverride(t *testing.T) {
	m := applyJSON(t, llm.Pipeline{openai.ModelOverride{Model: "candidate-v2"}}, request)
	if m["model"] != "candidate-v2" {
		t.Fatalf("model = %v, want candidate-v2", m["model"])
	}
}

func TestSystemPrompt_ReplacesExistingSystemMessage(t *testing.T) {
	m := applyJSON(t, llm.Pipeline{openai.SystemPrompt{Text: "v2 prompt"}}, request)

	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "v2 prompt" {
		t.Fatalf("first message = %v, want system v2 prompt", first)
	}
	if msgs[1].(map[string]any)["content"] != "refund please" {
		t.Fatalf("user message changed: %v", msgs[1])
	}
}

func TestSystemPrompt_InsertsWhenMissing(t *testing.T) {
	m := applyJSON(t, llm.Pipeline{openai.SystemPrompt{Text: "v2 prompt"}},
		`{"messages":[{"role":"user","content":"hi"}]}`)

	msgs := m["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "v2 prompt" || msgs[1].(map[string]any)["content"] != "hi" {
		t.Fatalf("messages = %v, want [system v2 prompt, user hi]", msgs)
	}
}

func TestStripProviderTools_KeepsOnlyFunctionTools(t *testing.T) {
	m := applyJSON(t, llm.Pipeline{openai.StripProviderTools{}}, request)

	tools := m["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Fatalf("tools = %v, want only the function tool", tools)
	}
}

func TestDropStatefulFields(t *testing.T) {
	m := applyJSON(t, llm.Pipeline{openai.DropStatefulFields{}}, request)

	for _, k := range []string{"store", "previous_response_id"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s still present", k)
		}
	}
	if m["temperature"] != 0.2 {
		t.Errorf("unrelated field temperature changed: %v", m["temperature"])
	}
}

func TestAllowFields_RejectsFieldsOutsideTheAllowlist(t *testing.T) {
	// web_search_options makes the provider run a web search inside the call:
	// a side effect the shadow copy must never have. Any field shadowbox does
	// not know is refused the same way (fail closed), so new provider options
	// cannot slip through.
	for _, field := range []string{"web_search_options", "store", "previous_response_id", "some_future_option"} {
		body := `{"model":"m","messages":[],"` + field + `":{}}`
		_, err := (llm.Pipeline{openai.AllowFields{}}).Apply([]byte(body))
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want an error naming the field", field, err)
		}
	}
}

func TestAllowFields_KeepsKnownFieldsUnchanged(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.2,"tools":[],"tool_choice":"auto",
	 "parallel_tool_calls":true,"max_completion_tokens":100,"response_format":{"type":"json_object"},"seed":1,"user":"u","metadata":{"k":"v"}}`
	got := applyJSON(t, llm.Pipeline{openai.AllowFields{}}, body)
	var want map[string]any
	_ = json.Unmarshal([]byte(body), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want the request unchanged", got)
	}
}

func TestAllowFields_ExtraWidensTheAllowlist(t *testing.T) {
	applyJSON(t, llm.Pipeline{openai.AllowFields{Extra: []string{"store"}}}, `{"model":"m","messages":[],"store":true}`)
}

func TestPipeline_AppliesInOrderAndPreservesUnknownFields(t *testing.T) {
	m := applyJSON(t, llm.Pipeline{openai.ModelOverride{Model: "a"}, openai.ModelOverride{Model: "b"}}, request)
	if m["model"] != "b" {
		t.Fatalf("model = %v, want b (last mutator wins)", m["model"])
	}
	if !reflect.DeepEqual(m["temperature"], 0.2) {
		t.Fatalf("temperature = %v, want 0.2 preserved", m["temperature"])
	}
}

func TestPipeline_RejectsNonObjectBodies(t *testing.T) {
	for _, body := range []string{``, `[1,2]`, `not json`} {
		if _, err := (llm.Pipeline{openai.ModelOverride{Model: "x"}}).Apply([]byte(body)); err == nil {
			t.Errorf("Apply(%q) err = nil, want error", body)
		}
	}
}

func TestSystemPrompt_RejectsMalformedMessages(t *testing.T) {
	if _, err := (llm.Pipeline{openai.SystemPrompt{Text: "x"}}).Apply([]byte(`{"messages":"oops"}`)); err == nil {
		t.Fatalf("err = nil, want error for non-array messages")
	}
}
