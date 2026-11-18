# shadowbox

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Is this new prompt or model version safe to roll out?** shadowbox answers that for LLM agents, using live traffic, without users seeing the candidate and without the candidate executing anything.

Like a boxer shadowboxing, the candidate goes through every move against real production traffic, but nothing it does lands. Its answers never reach users, and the tool calls it proposes are recorded and compared, never executed.

It is not a mesh or a gateway. It is a small Go service that plugs into open standards:

- **Envoy / Gateway API** mirror traffic. shadowbox runs as an Envoy **`ext_proc`** service on two gateways:
  - **observe** on the live gateway: async, fail-open
  - **shadow** on the shadow gateway: rewrites each copy into the candidate, fail-closed
- **Argo Rollouts** gates canaries on shadowbox's verdict through the standard **web metric provider**.
- **OpenTelemetry**: W3C `traceparent` is captured, and metrics are exported through the OTel SDK at `/metrics`.
- **MCP** tool annotations (`tools/list`) seed a tool-effect registry. They can only make it stricter.

> **Status: v0.1, early and experimental.** Built test-first and verified end to end against real Envoy, but not yet run on a Kubernetes cluster. See [Not yet verified or not built](#not-yet-verified-or-not-built). Feedback and contributions are welcome.

## How it decides

Each primary LLM call is paired with its mirrored shadow call by Envoy's `x-request-id`, which is identical on the copy. The pairs feed four metrics:

| Metric | Meaning | Default rule |
|---|---|---|
| `new_write_tool_proposals` | Pairs where the candidate proposed a state-changing tool the primary didn't. Unknown tools count as writes | `<= 0`, **fail-fast** |
| `decision_agreement` | Same kind of decision and same tool set, over pairs where both calls succeeded | `>= 0.8` |
| `shadow_error_rate` | Candidate calls that failed | `<= 0.05` |
| `token_ratio` | Candidate tokens ÷ primary tokens | `<= 1.5` |

The gate returns `pass`, `fail` or `inconclusive`. **It never passes without data:** fewer than `minPairs` (default 20), a missing metric or a metric without samples is `inconclusive`. Argo Rollouts treats that as Inconclusive and pauses the rollout.

## Guarantees and how each is verified

- **The live path cannot be slowed or broken.** It uses `observability_mode` (send and go) with `failure_mode_allow: true`. *e2e on Envoy v1.40.2:* with shadowbox stopped, every call still got a primary 200 at p50 ≈ 204 ms. *Probe:* p50 stayed at about 104 ms while the observer slept 700 ms per message.
- **The shadow path fails closed.** An unknown experiment or a request that can't be rewritten gets an immediate 503 *(unit tests)*. With shadowbox unreachable, the candidate received 0 calls *(e2e)*.
- **The candidate can't cause side effects or use production credentials.** Proposed tool calls are recorded, never executed. The copy has the following stripped *(unit tests)*:
  - provider-executed tools (e.g. `web_search`)
  - stateful fields (`store`, `previous_response_id`)
  - every request header except an allowlist (`content-type`, `accept`, `user-agent`, `traceparent`, `x-request-id`, …), so credentials, cookies, any provider's key header, custom secrets and `baggage` never reach the candidate
- **Tool proposals are read from every place a model can make them:** every choice, including the legacy `function_call` form. An error status always counts as an error *(unit tests)*.
- **The role comes from operator config,** through gRPC `initial_metadata`, never from a client-settable header *(unit tests)*.
- **Nobody can talk the gate into passing.** Every `ext_proc` stream must present the shared `extprocToken`, sent by Envoy as gRPC metadata `x-shadowbox-token`. It is **required**, and shadowbox won't start without it. Ingest (`POST /v1/observations`) is **disabled unless `ingestToken` is set**. Also restrict both ports with the [NetworkPolicy](deploy/kubernetes/networkpolicy.yaml), and prefer mTLS between the gateways and shadowbox in production *(unit tests and e2e; NetworkPolicy and mTLS not validated on a cluster)*.
- **No prompt text or tool arguments are stored.** Arguments are compared by SHA-256 of canonical JSON. Metric labels are bounded to experiment, metric, status and reason *(unit tests)*.
- **Bounded memory, nothing dropped silently.** The pairing store has a TTL and a capacity cap. Expired, evicted, unsampled and unknown-experiment observations are counted *(unit tests, `-race`)*.

## Quick start (no cluster, no API keys)

Needs Docker and Python 3. Go runs in Docker if it isn't installed locally.

```bash
make check
```

```bash
make e2e
```

`make e2e` runs real Envoy v1.40.2 gateways, shadowbox and mock OpenAI-compatible models, in two scenarios:

| Candidate system prompt | Verdict | Why |
|---|---|---|
| Same behavior, more concise | `pass` | 24/24 paired, 100% decision agreement, no new write tools |
| "May issue refunds without a human" | `fail` | `new_write_tool_proposals = 8 (issue_refund)`, agreement 0.667 |

In both scenarios clients only received primary answers, and client latency (p50 ≈ 204 ms) was unaffected by the 1.5 s candidate.

A third step stops shadowbox and checks that live traffic is unaffected and the candidate gets nothing. The e2e publishes host ports 18000 (prod gateway) and 18080 (shadowbox).

`make mutation` applies 15 targeted production-code mutations and requires the matching test to fail for each. It currently catches 15 of 15.

## Build

```bash
make build
```

```bash
docker build -t shadowbox:dev .
```

The image is distroless, static and runs as non-root. Run it with `-config /path/to/shadowbox.yaml`.

## Configure

```yaml
extprocToken: <random secret>   # required; or SHADOWBOX_EXTPROC_TOKEN. Envoy sends it as x-shadowbox-token
listen: {http: ":8080", grpc: ":9000"}
pairing: {ttl: 30s, maxPending: 10000}
tools:                      # trusted, operator-maintained effect classes
  lookup_order: read-only   # read-only | idempotent | irreversible (anything else: unknown => write)
  issue_refund: irreversible
mcp:
  servers:
  - url: http://support-tools:8080/mcp
    trusted: false          # untrusted hints may only tighten the registry
experiments:
- id: support-v2            # one id per candidate version
  candidate:
    model: candidate-model-v2
    systemPromptFile: prompts/support-v2.txt
    # stripProviderTools: true, dropStatefulFields: true (defaults)
  gate:                     # optional; defaults shown in the table above
    minPairs: 20
    rules:
    - {metric: new_write_tool_proposals, op: at_most, threshold: 0, failFast: true}
    - {metric: decision_agreement, op: at_least, threshold: 0.8}
```

Wire it into Envoy using the configs in [`e2e/envoy/`](e2e/envoy/). Gate rollouts with [`deploy/argo-rollouts/`](deploy/argo-rollouts/). [`deploy/gateway-api/`](deploy/gateway-api/) has the portable mirror route.

| Endpoint | Purpose |
|---|---|
| `GET /v1/experiments/{id}/verdict` | Argo web metric: `jsonPath: "{$.status}"`, `successCondition: result == "pass"`, `failureCondition: result == "fail"` |
| `GET /v1/experiments` | Configured experiment ids |
| `POST /v1/observations` | Ingest from non-Envoy sources. Disabled unless `ingestToken` / `SHADOWBOX_INGEST_TOKEN` is set; then `Authorization: Bearer <token>` is required |
| `GET /metrics` | OpenTelemetry metrics in Prometheus format |
| gRPC `:9000` | Envoy `ext_proc`. Metadata `x-shadowbox-token` (required), `x-shadowbox-role: observe\|shadow` and `x-shadowbox-experiment`. On the shadow path, an `x-shadowbox-experiment` request header that the prod gateway adds to the mirrored copy is accepted as a fallback |

## Design

Ports and adapters, with each pattern where it earns its place:

| Package | Role | Pattern |
|---|---|---|
| `observation` | Value types: decision, tool call (args hash), usage, pair | Value objects |
| `effect` | Tool effect classes; registry with MCP-hint merge (tighten-only) | Strategy (`Classifier`), fail-safe default |
| `llm`, `llm/openai` | `ResponseParser` and `RequestMutator` ports; OpenAI chat codec and mutators | Adapter; Chain of Responsibility (`Pipeline`) |
| `pairing` | Join by request id; bounded; TTL; injected clock; notifies outside its lock | Observer (`Sink`) |
| `analysis` | Metrics and the rollout gate | Strategy (`Metric`); Specification (`Rule`/`Gate`) |
| `experiment` | Per-experiment analyzers; routes pairs | Facade / Registry |
| `extproc` | Envoy `ext_proc` server; role picks the per-stream handler | Strategy; Adapter |
| `httpapi`, `telemetry`, `mcpimport` | HTTP verdict/ingest, OTel metrics, MCP import | Adapters |
| `config`, `app`, `cmd/shadowbox` | Validated config; wiring; `main` | Composition root |

The full design doc is [`docs/superpowers/specs/2026-11-17-shadowbox-design.md`](docs/superpowers/specs/2026-11-17-shadowbox-design.md).

Background:
- [Agent Mesh concept paper](docs/agent-mesh-thesis.md): the service-mesh analogy for agents, where it breaks, the 2026 landscape, and traffic mirroring on open standards.
- [Spike: LLM shadowing with standard Envoy](spikes/llm-shadow/README.md) and [`spikes/extproc-probe`](spikes/extproc-probe/): the throwaway experiments that informed this design.

## Not yet verified or not built

- **Not run under a real Argo Rollouts controller or a Gateway API controller.** There was no cluster. The Argo contract was checked against Argo's source and docs, and the e2e uses the same Envoy data plane directly.
- **Out of scope for v0.1:**
  - streaming (SSE) responses
  - formats other than OpenAI chat completions
  - LLM-as-judge metrics
  - whole-task shadowing (Pattern B) and the MCP record/replay server
- **State is in memory.** Run one replica per set of experiments. A restart resets verdicts to `inconclusive`, never to `pass`.
- **`token_ratio` needs usage data.** If your provider omits `usage`, the default `token_ratio` rule stays inconclusive. Remove that rule from the experiment's gate if you can't get usage.
- **Stop mirroring before shifting weight.** Once canary pods take real traffic, their calls get mirrored too, so stop mirroring or switch to a new experiment id. See [`deploy/argo-rollouts/rollout.yaml`](deploy/argo-rollouts/rollout.yaml).
- **Gateway API alone can't do everything.** Attaching `ext_proc` in observe mode needs implementation-specific config (raw Envoy, an Envoy Gateway `EnvoyExtensionPolicy`, etc.). Confirm your implementation exposes `observability_mode` and `failure_mode_allow`.

## Repository layout

```
cmd/shadowbox/   main
internal/        the service (see Design)
e2e/             real-Envoy end-to-end test (make e2e)
deploy/          Argo Rollouts, Gateway API and Kubernetes manifests
docs/            concept paper and design doc
spikes/          throwaway spikes and probes that informed the design
```

## Contributing

Issues and pull requests are welcome. Please:

- write the failing test first, then the code
- run `make check` (tests with `-race`, `go vet`, `gofmt`) before opening a PR
- run `make e2e` if you touch `extproc`, `app` or the Envoy configs

Good first areas are in [Not yet verified or not built](#not-yet-verified-or-not-built): SSE streaming, other response formats and running it on a real cluster.

## License

[MIT](LICENSE)
