"""Join primary and shadow LLM calls on trace id, check safety invariants and
compare decisions. Deliberately simple (no LLM judge): tool-choice agreement.
"""
import collections
import json
import pathlib
import urllib.request

LOGS = pathlib.Path(__file__).parent / "logs"
WRITE_TOOLS = {"issue_refund"}  # effect class would come from the tool registry
V2_PROMPT = (pathlib.Path(__file__).parent / "prompts/support-v2.txt").read_text().strip()


def load(name):
    p = LOGS / f"{name}.jsonl"
    return [json.loads(l) for l in p.read_text().splitlines()] if p.exists() else []


def label(d):
    return f"tool:{d['tool']}" if "tool" in d else "text"


def check(name, ok):
    print(f"  [{'PASS' if ok else 'FAIL'}] {name}")
    return ok


def main():
    prim = {r["trace_id"]: r for r in load("primary")}
    shad = {r["trace_id"]: r for r in load("candidate")}
    joined = [(prim[t], shad[t]) for t in prim if t in shad]

    print(f"primary calls={len(prim)}  mirrored to candidate={len(shad)}  joined={len(joined)}")
    print("\nSafety / correctness invariants")
    s = list(shad.values())
    check("primary never sees the shadow marker", all(not r["shadow_header"] for r in prim.values()))
    check("every shadow call carries x-agentmesh-shadow", all(r["shadow_header"] == "exp-support-v2" for r in s))
    check("shadow Host has Envoy's -shadow suffix (before the port)", all("-shadow" in (r["host"] or "").split(":")[0] for r in s))
    check("prod Authorization header removed from copy", all(not r["authorization_present"] for r in s))
    check("model swapped to candidate", all(r["model"] == "candidate-model-v2" for r in s))
    check("system prompt swapped to v2", all(r["system"] == V2_PROMPT for r in s))
    check("provider-side tools stripped (only 'function')", all(set(r["tool_types"]) <= {"function"} for r in s))
    check("primary still received provider-side tool", all("web_search" in r["tool_types"] for r in prim.values()))

    print("\nDecision comparison (primary v1 -> candidate v2)")
    pairs = collections.Counter((p["user"], label(p["decision"]), label(c["decision"])) for p, c in joined)
    agree = sum(n for (u, a, b), n in pairs.items() if a == b)
    for (user, a, b), n in sorted(pairs.items()):
        flag = "  same" if a == b else "  DIFF"
        risk = "  <- proposes side-effecting tool (recorded, never executed)" if b.split(":")[-1] in WRITE_TOOLS else ""
        print(f"{flag}  x{n}  {user[:45]:45}  {a:20} -> {b}{risk}")
    if joined:
        print(f"\nagreement: {agree}/{len(joined)} = {agree/len(joined):.0%}")
    errs = sum(1 for r in s if r["status"] != 200)
    print(f"candidate errors (invisible to clients): {errs}/{len(s)}")

    try:
        stats = urllib.request.urlopen("http://localhost:9901/stats?filter=shadow_gateway.upstream_rq_(total|2xx|5xx)$").read().decode()
        print("\nprod-gateway Envoy stats:\n  " + "\n  ".join(stats.strip().splitlines()))
    except Exception as e:  # noqa: BLE001
        print(f"(could not read Envoy stats: {e})")


if __name__ == "__main__":
    main()
