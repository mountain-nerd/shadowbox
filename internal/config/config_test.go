package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/config"
	"github.com/mountain-nerd/shadowbox/internal/effect"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

const full = `
extprocToken: stream-secret
listen: {http: ":8081", grpc: ":9001"}
pairing: {ttl: 45s, maxPending: 500}
tools:
  lookup_order: read-only
  issue_refund: irreversible
experiments:
- id: support-v2
  candidate:
    model: candidate-model-v2
    systemPromptFile: prompt.txt
  gate:
    minPairs: 10
    rules:
    - {metric: new_write_tool_proposals, op: at_most, threshold: 0, failFast: true}
    - {metric: decision_agreement, op: at_least, threshold: 0.9}
`

func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "shadowbox.yaml")
}

func TestLoad_FullConfig(t *testing.T) {
	path := write(t, map[string]string{"shadowbox.yaml": full, "prompt.txt": "  v2 prompt\n"})

	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen.HTTP != ":8081" || c.Listen.GRPC != ":9001" || c.Pairing.TTL != 45*time.Second || c.Pairing.MaxPending != 500 {
		t.Fatalf("listen/pairing = %+v %+v", c.Listen, c.Pairing)
	}
	if got := c.Experiments[0].Candidate.SystemPrompt; got != "v2 prompt" {
		t.Fatalf("system prompt = %q, want file contents trimmed, read relative to the config", got)
	}
}

func TestLoad_Defaults(t *testing.T) {
	path := write(t, map[string]string{"shadowbox.yaml": `
extprocToken: stream-secret
experiments:
- id: e1
  candidate: {model: m2}
`})

	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen.HTTP != ":8080" || c.Listen.GRPC != ":9000" || c.Pairing.TTL != 30*time.Second || c.Pairing.MaxPending != 10000 {
		t.Fatalf("defaults = %+v %+v", c.Listen, c.Pairing)
	}
	g := c.Experiments[0].Gate
	if g.MinPairs != 20 || len(g.Rules) != 4 {
		t.Fatalf("default gate = %+v, want minPairs 20 and 4 rules", g)
	}
	if !c.Experiments[0].Candidate.StripProviderTools() || !c.Experiments[0].Candidate.DropStatefulFields() {
		t.Fatalf("provider tools and stateful fields must be stripped by default")
	}
}

func TestLoad_RejectsInvalidConfig(t *testing.T) {
	cases := map[string]string{
		"no experiments": `tools: {a: read-only}`,
		"empty id":       "experiments:\n- id: \"\"\n  candidate: {model: m}",
		"duplicate id":   "experiments:\n- id: a\n  candidate: {model: m}\n- id: a\n  candidate: {model: n}",
		"no change":      "experiments:\n- id: a\n  candidate: {}",
		"bad tool class": "tools: {a: safe}\nexperiments:\n- id: a\n  candidate: {model: m}",
		"bad op":         "experiments:\n- id: a\n  candidate: {model: m}\n  gate: {rules: [{metric: decision_agreement, op: about, threshold: 1}]}",
		"bad metric":     "experiments:\n- id: a\n  candidate: {model: m}\n  gate: {rules: [{metric: vibes, op: at_least, threshold: 1}]}",
		"bad ttl":        "pairing: {ttl: soon}\nexperiments:\n- id: a\n  candidate: {model: m}",
		"missing prompt": "experiments:\n- id: a\n  candidate: {systemPromptFile: nope.txt}",
		"unknown field":  "experimants: []",
		"two prompts":    "experiments:\n- id: a\n  candidate: {systemPrompt: x, systemPromptFile: y.txt}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(write(t, map[string]string{"shadowbox.yaml": "extprocToken: t\n" + body})); err == nil {
				t.Fatalf("Load succeeded, want error")
			}
		})
	}
}

func TestLoad_RequiresExtProcToken(t *testing.T) {
	t.Setenv("SHADOWBOX_EXTPROC_TOKEN", "")
	path := write(t, map[string]string{"shadowbox.yaml": "experiments:\n- id: a\n  candidate: {model: m}"})

	if _, err := config.Load(path); err == nil {
		t.Fatal("Load succeeded without extprocToken, want error (an open ext_proc port lets anyone feed the verdict)")
	}
}

func TestLoad_RejectsPlaceholderTokens(t *testing.T) {
	cases := map[string]struct{ file, extprocEnv, ingestEnv string }{
		"extproc in file":  {file: "extprocToken: REPLACE-WITH-A-RANDOM-SECRET\n"},
		"extproc from env": {extprocEnv: "REPLACE-WITH-A-RANDOM-SECRET"},
		"ingest in file":   {file: "extprocToken: t\ningestToken: replace-with-a-random-secret\n"},
		"ingest from env":  {file: "extprocToken: t\n", ingestEnv: "REPLACE-WITH-A-RANDOM-SECRET"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SHADOWBOX_EXTPROC_TOKEN", tc.extprocEnv)
			t.Setenv("SHADOWBOX_INGEST_TOKEN", tc.ingestEnv)
			path := write(t, map[string]string{"shadowbox.yaml": tc.file + "experiments:\n- id: a\n  candidate: {model: m}"})
			if _, err := config.Load(path); err == nil {
				t.Fatal("Load succeeded with the example manifest's placeholder token, want error (it is public, so anyone could present it)")
			}
		})
	}
}

func TestLoad_ExtProcTokenFromEnvironment(t *testing.T) {
	t.Setenv("SHADOWBOX_EXTPROC_TOKEN", "from-env")
	path := write(t, map[string]string{"shadowbox.yaml": "experiments:\n- id: a\n  candidate: {model: m}"})

	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ExtProcToken != "from-env" {
		t.Fatalf("ExtProcToken = %q, want from-env", c.ExtProcToken)
	}
}

func TestBuild_KeepingStatefulFieldsAllowsThem(t *testing.T) {
	path := write(t, map[string]string{"shadowbox.yaml": `
extprocToken: t
experiments:
- id: a
  candidate: {model: m, dropStatefulFields: false}
`})
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_, rewrites := c.Build(c.EffectRegistry())
	if _, err := rewrites["a"].Rewrite.Apply([]byte(`{"model":"v1","messages":[],"store":true}`)); err != nil {
		t.Fatalf("store refused with dropStatefulFields: false: %v", err)
	}
}

func TestBuild_WiresRegistryRewriteAndGate(t *testing.T) {
	path := write(t, map[string]string{"shadowbox.yaml": full, "prompt.txt": "v2 prompt"})
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	reg := c.EffectRegistry()
	if reg.Classify("issue_refund") != effect.Irreversible || reg.Classify("lookup_order") != effect.ReadOnly {
		t.Fatalf("registry not built from tools")
	}

	analyzers, rewrites := c.Build(reg)

	rw, ok := rewrites["support-v2"]
	if !ok || rw.Model != "candidate-model-v2" {
		t.Fatalf("rewrite = %+v, want model candidate-model-v2", rw)
	}
	out, err := rw.Rewrite.Apply([]byte(`{"model":"v1","store":true,"messages":[{"role":"system","content":"v1"}],"tools":[{"type":"web_search"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	if req["model"] != "candidate-model-v2" || req["store"] != nil || len(req["tools"].([]any)) != 0 ||
		req["messages"].([]any)[0].(map[string]any)["content"] != "v2 prompt" {
		t.Fatalf("rewritten request = %s", out)
	}

	if _, err := rw.Rewrite.Apply([]byte(`{"model":"v1","messages":[],"web_search_options":{}}`)); err == nil {
		t.Fatal("web_search_options passed the rewrite, want the field allowlist to refuse it")
	}

	a := analyzers["support-v2"]
	if a == nil {
		t.Fatal("no analyzer for support-v2")
	}
	call := func(n string) observation.Decision {
		return observation.Decision{Kind: observation.ToolCalls, ToolCalls: []observation.ToolCall{{Name: n}}}
	}
	a.Observe(observation.Pair{Experiment: "support-v2",
		Primary: observation.Observation{Decision: call("lookup_order")},
		Shadow:  observation.Observation{Decision: call("issue_refund")}})
	v := a.Verdict()
	if v.Status != analysis.Fail || !strings.Contains(strings.Join(v.Reasons, ";"), "issue_refund") {
		t.Fatalf("verdict = %s %v, want fail naming issue_refund (fail-fast rule from config)", v.Status, v.Reasons)
	}
}
