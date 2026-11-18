# Spike: shadowing a new prompt/model with standard Envoy mirroring

> **Throwaway spike.** It proves out *Pattern A* from the concept paper ([§7.3](../../docs/agent-mesh-thesis.md)). It is not production code and has no tests beyond the evaluator's invariant checks.

**Question:** can we shadow a changed system prompt plus a different model using only standard proxy features, plus one small `ext_proc` service, without touching the agent?

**Answer:** yes, at the LLM-call level (step-level shadowing).

## What runs

```
agent_client.py ──► prod-gateway (Envoy :10000) ──► primary-llm (mock "current-model-v1")
                         │  request_mirror_policies (fire-and-forget, runtime %)
                         │  + copy-only header mutations: add x-agentmesh-shadow, drop Authorization
                         ▼
                    shadow-gateway (Envoy :10001)
                         │  local_ratelimit  = experiment budget
                         │  ext_proc → prompt-swap (Go): model→candidate-model-v2,
                         │             system prompt→prompts/support-v2.txt,
                         │             strip provider-side tools, drop store
                         ▼
                    candidate-llm (mock, 1.5 s slower, 20% errors)

evaluate.py joins primary + candidate logs on the W3C trace id (the mirror copies `traceparent`).
```

| Component | What it is | Standard? |
|---|---|---|
| `envoy/prod-gateway.yaml` | Envoy v1.40.2, `RouteAction.request_mirror_policies` with `runtime_fraction` and `request_headers_mutations` | Stock Envoy |
| `envoy/shadow-gateway.yaml` | Envoy v1.40.2, `ext_proc` (fail-closed) + `local_ratelimit` | Stock Envoy |
| `prompt_swap/main.go` | ~130-line Envoy external-processing server | Custom code on the standard ext_proc API |
| `mock_llm/server.py` | OpenAI-compatible mock with a prompt-dependent "policy" | Test double |
| `agent_client.py`, `evaluate.py` | Simulated agent v1; trace-id join + invariants + decision diff | Test harness |

## Run it

Needs Docker and Python 3. No API keys.

```bash
docker compose up -d --build
```

```bash
python3 agent_client.py 3 && sleep 4 && python3 evaluate.py
```

Change the mirror percentage live, with no restart:

```bash
curl -XPOST 'localhost:9901/runtime_modify?shadow.llm_percent=30'
```

Tear down:

```bash
docker compose down
```

## Results (2026-11-17, Envoy v1.40.2)

| Check | Result |
|---|---|
| Client only ever sees the primary's responses | ✅ all `served_by=primary` |
| Client latency unaffected by a slower, flaky candidate | ✅ p50 ≈ 205–209 ms vs. candidate 1.5 s delay + 20% errors |
| Candidate errors invisible to clients | ✅ e.g. 8/18 candidate 5xx, 0 client errors |
| Every call mirrored at 100%; ~30% after a live runtime change | ✅ 18/18; then 14/60 at 30% |
| Shadow marker on copy only; prod `Authorization` stripped from copy | ✅ |
| Envoy appends `-shadow` to Host | ✅ inserted *before* the port: `localhost-shadow:10000` |
| Model and system prompt swapped on the shadow path | ✅ |
| Provider-side tool (`web_search`) stripped from shadow only | ✅ primary still received it |
| Primary and shadow joinable by trace id | ✅ 18/18 joined |
| **Fail-closed:** ext_proc down → nothing reaches the candidate | ✅ 0 candidate calls, 12 immediate rejections, client p50 unchanged |
| Decision diff surfaced | ✅ 67% agreement. v2 proposes `issue_refund` (a write tool) for damaged-item requests where v1 looked up the order first. Recorded, never executed |

## What we learned

1. **Mirroring is a solved, standard primitive.** Fire-and-forget, live percentage control and copy-only header mutations all work in stock Envoy. The Gateway API `RequestMirror` filter expresses the same thing portably, except for the copy-only header mutations.
2. **"Mirror to a shadow gateway" closes the copy-mutation gap.** The shadow gateway processes the copy on its *own* primary path, so `ext_proc` runs (which does not happen on a mirror leg). The only custom code is the prompt-swap service.
3. **The diff is the product.** The useful output wasn't "the shadow worked" but "v2 would issue refunds without looking up the order". That is a behavior change a reviewer needs to see before a canary.
4. **Docs vs. release drift:** the "latest" Envoy docs list `stat_prefix` on `RequestMirrorPolicy`, but v1.40.2 rejects it. Per-mirror stats aren't available in this release; use the mirror cluster's `upstream_rq_*` stats instead.
5. **Fail-closed matters.** With `failure_mode_allow: false`, a broken rewriter drops shadow traffic instead of sending an un-rewritten (v1) request to the candidate, which would silently corrupt the comparison.

## Not covered (next steps if this goes further)

- **Real models.** Point `candidate_llm` / `primary_llm` at OpenAI-compatible endpoints. The shadow gateway would then inject the candidate's credentials.
- **Streaming responses (SSE).** Not exercised.
- **Token-based budgets.** Here the budget is request-based (`local_ratelimit`). Token budgets need an AI-aware gateway (agentgateway / Agent Router). The budget-exhaustion path was not exercised.
- **Kubernetes / Gateway API.** Express the prod side as an `HTTPRoute` with `RequestMirror` (`percent`) on Cilium, Envoy Gateway, Istio or agentgateway. Copy-only header mutation then isn't available, so the shadow gateway should add the marker itself.
- **Real evaluation.** Pairwise LLM-as-judge and outcome metrics instead of tool-choice agreement.
- **Trajectory-level shadowing (Pattern B).** Identity-isolated shadow agents and stub/replay MCP tools.
