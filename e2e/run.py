"""End-to-end check: real Envoy v1.40.2 + shadowbox + mock models.

For each scenario it (re)starts shadowbox with that scenario's config,
sends simulated agent traffic through the prod gateway, then reads the
verdict exactly the way an Argo Rollouts web metric would
(jsonPath {$.status}; success: result == "pass"; failure: result == "fail").

Exit code 0 only if every scenario produced its expected verdict, clients only
ever saw primary answers, and client latency stayed independent of the slow
candidate.
"""
import json
import os
import secrets
import statistics
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
GATEWAY = "http://localhost:18000/v1/chat/completions"
VERDICT = "http://localhost:18080/v1/experiments/support-v2/verdict"
SYSTEM_V1 = "You are a friendly customer-support agent. Always look up the order before acting."
TOOLS = [
    {"type": "function", "function": {"name": "lookup_order", "parameters": {"type": "object"}}},
    {"type": "function", "function": {"name": "issue_refund", "parameters": {"type": "object"}}},
    {"type": "web_search"},
]
USERS = [
    "Where is my order A-1001?",
    "My blender arrived broken, I want a refund.",
    "Hi, I have a question.",
    "Can you check the status of order A-1001 please?",
    "Refund please, the item was damaged in shipping.",
    "What are your opening hours?",
]
SCENARIOS = {"pass": "pass", "fail": "fail"}
# Every client call sends the same x-request-id. The prod gateway must replace
# it with its own, or calls would share a pairing key and could be mismatched.
CLIENT_REQUEST_ID = "client-chosen-id"


def compose(*args, env=None):
    subprocess.run(["docker", "compose", *args], cwd=HERE, check=True, env={**os.environ, **(env or {})},
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def wait_ready(url, timeout=60):
    end = time.time() + timeout
    while time.time() < end:
        try:
            urllib.request.urlopen(url, timeout=2)
            return
        except urllib.error.HTTPError:
            return  # server is up (e.g. 404/405)
        except Exception:
            time.sleep(0.5)
    raise SystemExit(f"timed out waiting for {url}")


def call(user):
    trace = secrets.token_hex(16)  # several calls of one task may share this; pairing uses x-request-id
    body = json.dumps({"model": "current-model-v1", "store": True, "tools": TOOLS,
                       "messages": [{"role": "system", "content": SYSTEM_V1}, {"role": "user", "content": user}]}).encode()
    req = urllib.request.Request(GATEWAY, body, {"content-type": "application/json", "authorization": "Bearer prod-key",
                                                 "traceparent": f"00-{trace}-{secrets.token_hex(8)}-01",
                                                 "x-request-id": CLIENT_REQUEST_ID})
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=30) as r:
        resp = json.load(r)
    return resp, time.time() - t0


def verdict():
    with urllib.request.urlopen(VERDICT, timeout=5) as r:
        return json.load(r)


def run_scenario(name, expected):
    print(f"\n=== scenario {name}: expect {expected}")
    compose("up", "-d", "--force-recreate", "shadowbox", env={"SCENARIO": name})
    wait_ready("http://localhost:18080/healthz")
    compose("restart", "shadow-gateway", "prod-gateway")
    wait_ready("http://localhost:18000/")

    latencies, served = [], set()
    for _ in range(4):
        for u in USERS:
            resp, dt = call(u)
            latencies.append(dt)
            served.add(resp["id"].split("-")[0])
    p50 = statistics.median(latencies)
    print(f"client: {len(latencies)} calls, p50 {p50*1000:.0f} ms, max {max(latencies)*1000:.0f} ms, served_by={sorted(served)}")

    deadline, v = time.time() + 20, {}
    while time.time() < deadline:
        v = verdict()
        if v["status"] == expected and v["pairs"] >= len(latencies):
            break
        time.sleep(0.5)
    print(f"verdict: {json.dumps({k: v[k] for k in ('status', 'pairs', 'reasons')})}")
    for name_, m in sorted(v.get("metrics", {}).items()):
        print(f"  {name_:26} value={m['value']:.3f} samples={m['samples']} {m.get('detail', '')}")

    ok = True
    ok &= check(v["status"] == expected, f"verdict status is {expected!r} (Argo: {'Successful' if expected == 'pass' else 'Failed'})")
    ok &= check(v["pairs"] == len(latencies), f"every call paired ({v['pairs']}/{len(latencies)})")
    dups = v.get("metrics", {}).get("duplicate_request_ids", {}).get("value")
    ok &= check(dups == 0, f"no duplicate request ids although every client sent x-request-id: {CLIENT_REQUEST_ID} (got {dups})")
    ok &= check(served == {"primary"}, "clients only ever saw primary answers")
    ok &= check(p50 < 1.0, f"client p50 {p50*1000:.0f} ms is independent of the 1.5 s candidate")
    return ok


def check(cond, msg):
    print(f"  [{'PASS' if cond else 'FAIL'}] {msg}")
    return cond


def candidate_calls():
    path = os.path.join(HERE, "logs", "candidate.jsonl")
    return sum(1 for _ in open(path)) if os.path.exists(path) else 0


def shadowbox_down():
    """Live path fails open, shadow path fails closed, with shadowbox stopped."""
    print("\n=== shadowbox stopped: live path must be unaffected, candidate must get nothing")
    compose("stop", "shadowbox")
    before = candidate_calls()
    latencies, served = [], set()
    for u in USERS * 2:
        resp, dt = call(u)  # raises on any non-2xx
        latencies.append(dt)
        served.add(resp["id"].split("-")[0])
    time.sleep(2)
    p50 = statistics.median(latencies)
    print(f"client: {len(latencies)} calls, p50 {p50*1000:.0f} ms, served_by={sorted(served)}")
    ok = True
    ok &= check(served == {"primary"}, f"all {len(latencies)} calls answered 200 by the primary")
    ok &= check(p50 < 1.0, f"client p50 {p50*1000:.0f} ms unchanged (observer is send-and-go, fail-open)")
    ok &= check(candidate_calls() == before, f"candidate received 0 calls (shadow fail-closed); before={before} after={candidate_calls()}")
    return ok


def main():
    os.makedirs(os.path.join(HERE, "logs"), exist_ok=True)
    print("building shadowbox:dev ...")
    subprocess.run(["docker", "build", "-q", "-t", "shadowbox:dev", os.path.dirname(HERE)], check=True, stdout=subprocess.DEVNULL)
    compose("up", "-d", env={"SCENARIO": "pass"})
    ok = True
    try:
        for name, expected in SCENARIOS.items():
            ok &= run_scenario(name, expected)
        metrics = urllib.request.urlopen("http://localhost:18080/metrics", timeout=5).read().decode()
        print("\nshadowbox /metrics (excerpt):")
        for line in metrics.splitlines():
            if line.startswith(("shadowbox_pairs_total", "shadowbox_verdict", "shadowbox_shadow_rejected")):
                print("  " + line)
        ok &= shadowbox_down()
    finally:
        if os.environ.get("KEEP") != "1":
            compose("down")
    print("\nE2E", "PASSED" if ok else "FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
