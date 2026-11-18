"""Mutation check (make mutation): apply one production-code mutation at a time, run the test
that should catch it, require FAIL, and always restore the original file."""
import pathlib
import subprocess
import sys

REPO = pathlib.Path(sys.argv[1])
M = [
    # (file, old, new, package, test regex)
    ("internal/llm/openai/openai.go", "return observation.Decision{Kind: observation.Text}, usage",
     "return observation.Decision{Kind: observation.ToolCalls}, usage", "./internal/llm/openai", "TestParseResponse_Text$"),
    ("internal/llm/openai/openai.go", "if status >= 400 {", "if status >= 600 {", "./internal/llm/openai", "TestParseResponse_Error"),
    ("internal/llm/openai/openai.go", 'req["model"] = m.Model', "_ = m.Model", "./internal/llm/openai", "TestModelOverride"),
    ("internal/llm/openai/openai.go", 'mm["content"] = s.Text', "_ = mm", "./internal/llm/openai", "TestSystemPrompt_ReplacesExistingSystemMessage"),
    ("internal/llm/openai/openai.go", "if !replaced {", "if false && !replaced {", "./internal/llm/openai", "TestSystemPrompt_InsertsWhenMissing"),
    ("internal/llm/openai/openai.go", "if !ok {\n\t\treturn errMessages", "if !ok {\n\t\treturn nil", "./internal/llm/openai", "TestSystemPrompt_RejectsMalformedMessages"),
    ("internal/llm/openai/openai.go", 'req["tools"] = kept', "_ = kept", "./internal/llm/openai", "TestStripProviderTools"),
    ("internal/llm/openai/openai.go", 'delete(req, "store")', "_ = req", "./internal/llm/openai", "TestDropStatefulFields"),
    ("internal/llm/llm.go", "if err := json.Unmarshal(body, &req); err != nil || req == nil {",
     "_ = json.Unmarshal(body, &req)\n\tif false {", "./internal/llm/openai", "TestPipeline_RejectsNonObjectBodies"),
    ("internal/llm/llm.go", "for _, m := range p {", "for i := len(p) - 1; i >= 0; i-- {\n\t\tm := p[i]", "./internal/llm/openai", "TestPipeline_AppliesInOrder"),
    ("internal/effect/effect.go", "case !registered && trusted:", "case false && trusted:", "./internal/effect", "TestMergeHints_TrustedHintsClassifyUnregisteredTools"),
    ("internal/effect/effect.go", "out := NewRegistry(r.classes)", "out := r", "./internal/effect", "TestMergeHints_DoesNotMutateOriginal"),
    ("internal/extproc/extproc.go", 'if s.opts.Token == "" || subtle.ConstantTimeCompare', 'if false && subtle.ConstantTimeCompare',
     "./internal/extproc", "TestStream_"),
    ("internal/extproc/extproc.go", 'if forwardedHeaders[k] || strings.HasPrefix(k, ":")', 'if true || strings.HasPrefix(k, ":")',
     "./internal/extproc", "TestShadow_ForwardsOnlyAllowlistedHeaders"),
    ("internal/pairing/pairing.go", "\tp.addLocked(o, &ev)\n\tp.mu.Unlock()\n\tp.dispatch(ev)",
     "\tp.addLocked(o, &ev)\n\tp.dispatch(ev)\n\tp.mu.Unlock()", "./internal/pairing", "TestSinkMayCallBackIntoPairer"),
]

def gotest(pkg, run):
    cmd = ["docker", "run", "--rm", "-v", f"{REPO}:/src", "-v", "shadowbox-gomod:/go/pkg/mod",
           "-v", "shadowbox-gocache:/root/.cache/go-build", "-w", "/src", "golang:1.27-alpine",
           "go", "test", "-count=1", "-timeout", "60s", "-run", run, pkg]
    return subprocess.run(cmd, capture_output=True, text=True)

caught = 0
for f, old, new, pkg, run in M:
    path = REPO / f
    orig = path.read_text()
    if orig.count(old) < 1:
        print(f"SKIP (pattern not found) {f}: {old[:40]!r}")
        continue
    try:
        path.write_text(orig.replace(old, new, 1))
        r = gotest(pkg, run)
        failed = r.returncode != 0 and "FAIL" in r.stdout + r.stderr
        caught += failed
        print(f"{'CAUGHT ' if failed else 'MISSED '} {run:55} <- {f.split('/')[-1]}: {old.splitlines()[0][:45]!r}")
    finally:
        path.write_text(orig)
print(f"\n{caught}/{len(M)} mutations caught")
sys.exit(0 if caught == len(M) else 1)
