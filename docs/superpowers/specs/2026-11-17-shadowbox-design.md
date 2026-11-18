# shadowbox: design (v0.1)

*Status: v0.1 implemented. Date: 2026-11-17. Module path `github.com/mountain-nerd/shadowbox`.*

## Intent

Answer one question for teams that ship agents: **is this new prompt or model version safe to roll out?** shadowbox is the agent-specific analysis layer on top of open standards. It is not a mesh or gateway.

- **Envoy / Gateway API** do the mirroring. shadowbox plugs in as an Envoy `ext_proc` service.
- **OpenTelemetry**: W3C `traceparent` is captured, and metrics are exported through the OTel SDK (Prometheus exporter).
- **MCP** tool annotations seed a tool *effect registry*, which can only tighten classifications.
- **Argo Rollouts** consumes a verdict through its standard **web metric provider** and gates canaries on it.

## Goals / non-goals / anti-goals

- **Goals (v0.1):**
  - capture primary and shadow LLM calls without affecting users
  - pair them per call
  - compute agent-specific metrics
  - return pass / fail / inconclusive for Argo Rollouts
  - rewrite shadow requests (model, system prompt, provider-side tools)
- **Non-goals (v0.1):**
  - streaming (SSE) responses
  - Anthropic Messages format (the codec interface allows it later)
  - LLM-as-judge (the `Metric` interface allows it later)
  - whole-task shadowing (Pattern B) and MCP record/replay
  - a native Argo metric plugin
- **Anti-goals:**
  - anything that can slow down, alter or fail a user's request
  - a candidate ever executing a side effect
  - storing prompt text or tool arguments by default

## Verified facts this design rests on (probes, Envoy v1.40.2)

All from `spikes/extproc-probe`:
1. **`ext_proc` `observability_mode: true`** with `STREAMED` bodies delivers the primary's request and response to shadowbox without blocking. Client latency stayed at about 104 ms while the processor slept 700 ms per message. Bodies arrive in chunks, and the end-of-stream chunk may be empty or missing if the stream closes first.
2. **Fail-open live path:** with `failure_mode_allow: true` and the processor down, live calls still returned 200. **Fail-closed shadow path:** `failure_mode_allow: false` sent zero calls to the candidate.
3. **Pairing key:** one trace has many LLM calls, so `traceparent` is not unique. **`x-request-id` is unique per call and identical on the mirrored copy** when the shadow gateway sets `preserve_external_request_id: true`. The prod gateway must generate it rather than keep a client's (`use_remote_address: true`, loopback-only `internal_address_config`, `preserve_external_request_id: false`). Otherwise a client that reuses an id could pair a candidate answer with the wrong primary. The pairer remembers paired ids for the TTL, and any duplicate holds a `pass` at `inconclusive`.
4. **Role from operator config:** gRPC `initial_metadata` (`x-shadowbox-role: observe|shadow`) reaches the processor. Clients cannot set it.
5. **Argo web metric contract:** JSON body, `jsonPath` sets `result`. With `successCondition: result == "pass"` and `failureCondition: result == "fail"`, a value that matches neither is **Inconclusive**, which pauses the rollout.

## Architecture (ports & adapters)

```
Envoy prod gw ──ext_proc(observe, async)──┐
Envoy shadow gw ─ext_proc(shadow, sync)───┤──► extproc adapter ──► ObservationSink
HTTP POST /v1/observations ───────────────┘       (role strategy)        │
                                                                         ▼
                              pairing.Pairer (bounded, TTL, keyed by request id)
                                                                         │ Pair
                                                                         ▼
                        experiment.Registry → Analyzer (Metric strategies) → analysis.Gate (rules)
                                                                         │ Verdict
                         GET /v1/experiments/{id}/verdict ◄──────────────┘ ──► Argo Rollouts web metric
                         GET /metrics (OTel → Prometheus)
```

| Package | Responsibility | Pattern |
|---|---|---|
| `internal/observation` | `Observation`, `Role`, `Decision`, `ToolCall`, `Usage`, `Pair` | Value objects |
| `internal/effect` | `Class` (ReadOnly / Idempotent / Irreversible / Unknown), `Classifier` interface, static registry, MCP-hint merge | Strategy; fail-safe default |
| `internal/llm` + `internal/llm/openai` | `ResponseParser` and `RequestMutator` interfaces; OpenAI chat implementations | Adapter (codec); Chain of Responsibility (mutator pipeline) |
| `internal/pairing` | Join primary and shadow by request id; bounded capacity; TTL with injected clock; counts unpaired | Observer (`PairSink`) |
| `internal/analysis` | `Metric` interface: agreement, new write-tool proposals, shadow error rate, token ratio. `Gate` of `Rule`s gives a `Verdict` | Strategy; Specification |
| `internal/experiment` | Experiment config, per-experiment `Analyzer`, `Registry` | Registry / Facade |
| `internal/extproc` | Envoy `ext_proc` server. Role from gRPC metadata selects `observe` or `shadow` handling | Strategy; Adapter |
| `internal/httpapi` | Verdict, ingest, health, metrics | Adapter |
| `internal/mcpimport` | MCP `tools/list` annotations → effect hints (official Go SDK) | Adapter |
| `internal/config`, `cmd/shadowbox` | YAML config and wiring | Composition root |

## Fail-safe rules (each encoded as a test)

1. No data, or fewer than `minPairs` pairs, gives **inconclusive**, never pass.
2. An **unknown tool counts as a write**.
3. MCP annotation hints can only **tighten** a classification, never loosen the registry. MCP says annotations are untrusted.
4. Shadow errors and unpaired calls are **counted**, never silently dropped. A primary call with no shadow is "not sampled", not an error.
5. The pairing store is **bounded** and has a **TTL**. Tests run with `-race`.
6. The role (observe / shadow) and the experiment come from **operator config** (gRPC metadata), never from a client-settable header on the live path.
7. **No prompt text or tool arguments are retained.** Arguments are compared through a SHA-256 hash of canonical JSON. Trace and request ids never appear in metric labels.
8. The shadow handler **fails closed**: if rewriting fails, it returns an immediate error to the shadow gateway and the copy is not forwarded. The observe handler never responds (async mode).

## Definition of done (v0.1) and how each part is verified

| Standard | Done means | Verified by |
|---|---|---|
| Envoy | Observe and shadow `ext_proc` modes work on v1.40.2 | Docker e2e: real Envoy, mock models, verdict flips pass ↔ fail with the candidate prompt |
| Gateway API | `HTTPRoute` `RequestMirror` example for the prod side | Manifest only. **Not run on a Gateway API controller**; the e2e uses the same Envoy data plane directly |
| Argo Rollouts | Verdict endpoint follows the web-metric contract; example `AnalysisTemplate` + `Rollout` | e2e asserts the JSON at `{$.status}`. **Not run under a real Argo controller** (no cluster here) |
| OpenTelemetry | `traceparent` captured on observations; metrics exported through the OTel SDK at `/metrics` without high-cardinality labels | Unit test scrapes `/metrics` |
| MCP | Effect hints imported from a live MCP server's `tools/list` | Test against an in-process MCP server from the official Go SDK |

## Results (v0.1, 2026-11-18)

- **93 test functions in 12 packages, written test-first** against compiling stubs. `make check` runs gofmt, vet and the full suite under `-race`.
  - **Not every test was watched failing on the first pass.** In `llm/openai`, an early panic masked the RED run, and three other tests passed against stubs by coincidence (two in `effect`, the pairer callback test).
  - **Those were verified afterwards by mutation.** `make mutation` breaks the code each test protects and requires a failure. It caught 13 of 13, after it exposed one unprotected branch (an error status with a well-formed body) and a test was added for it.
- **Fixes from review, each test-first:**
  - ingest is disabled without a token
  - the shadow copy drops `authorization`, `x-api-key` and `api-key`
  - legacy `function_call` and all choices are parsed
  - the e2e covers fail-open and fail-closed with shadowbox stopped
- **One real bug was caught by the suite.** `Serve` could take about 5 s to stop, because net/http waits for idle-new connections. It now uses a bounded grace period and then force-closes; reproduced 4 times in 5 under `-race` before the fix and 0 in 10 after.
- **e2e on Envoy v1.40.2:** the tone-only candidate gave `pass` (24/24 paired, agreement 1.0); the refunds-allowed candidate gave `fail` (`new_write_tool_proposals = 8, issue_refund`; agreement 0.667). Client p50 was about 204 ms in both, against a 1.5 s candidate.
- **Not verified:** under a real Argo Rollouts or Gateway API controller.

## Approaches considered

- **Chosen:** an `ext_proc` service plus an HTTP verdict, on top of Envoy / Gateway API mirroring. It plugs into any Envoy-based gateway and into Argo Rollouts without forking either.
- **Rejected:** our own gateway/proxy (crowded, not differentiated). OTLP-span ingest as the only source (depends on opt-in content capture in every agent; can be added later as another `ObservationSink` producer). A native Argo metric plugin first (the web provider is standard and enough for v0.1).

## Left to the builder (calls made)

- Language: Go, for native Envoy go-control-plane, MCP Go SDK and Argo ecosystems.
- Verdict windows are cumulative per experiment id. A new candidate gets a new experiment id.
- Default rules: `minPairs: 20`, `maxNewWriteToolProposals: 0`, `minDecisionAgreement: 0.8`, `maxShadowErrorRate: 0.05`, `maxTokenRatio: 1.5`. All configurable.
