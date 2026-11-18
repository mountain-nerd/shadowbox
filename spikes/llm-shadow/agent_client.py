"""Simulated agent v1: sends LLM calls through the prod gateway, as an agent would.

Each call carries a fresh W3C traceparent so primary and shadow can be joined.
The request includes a provider-executed tool (web_search) to show that the
shadow gateway strips it.
"""
import json
import secrets
import statistics
import sys
import time
import urllib.error
import urllib.request

GATEWAY = "http://localhost:10000/v1/chat/completions"
SYSTEM_V1 = "You are a friendly customer-support agent. Always look up the order before acting."
TOOLS = [
    {"type": "function", "function": {"name": "lookup_order", "parameters": {"type": "object"}}},
    {"type": "function", "function": {"name": "issue_refund", "parameters": {"type": "object"}}},
    {"type": "web_search"},  # provider-side tool: executes inside the LLM call
]
SCENARIOS = [
    "Where is my order A-1001?",
    "My blender arrived broken, I want a refund.",
    "Hi, I have a question.",
    "Can you check the status of order A-1001 please?",
    "Refund please, the item was damaged in shipping.",
    "What are your opening hours?",
]


def call(user: str) -> tuple[dict, float, str]:
    trace_id = secrets.token_hex(16)
    body = json.dumps({"model": "current-model-v1", "store": True, "tools": TOOLS,
                       "messages": [{"role": "system", "content": SYSTEM_V1},
                                    {"role": "user", "content": user}]}).encode()
    req = urllib.request.Request(GATEWAY, body, {
        "content-type": "application/json",
        "authorization": "Bearer prod-key-for-primary",
        "traceparent": f"00-{trace_id}-{secrets.token_hex(8)}-01",
    })
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=30) as r:
        resp = json.load(r)
    return resp, time.time() - t0, trace_id


def main(rounds: int) -> None:
    latencies = []
    for i in range(rounds):
        for user in SCENARIOS:
            resp, dt, tid = call(user)
            latencies.append(dt)
            msg = resp["choices"][0]["message"]
            out = (f"tool:{msg['tool_calls'][0]['function']['name']}" if msg.get("tool_calls")
                   else f"text:{msg['content'][:40]}")
            print(f"{tid[:8]}  {dt*1000:6.0f} ms  served_by={resp['id'].split('-')[0]:8} {out}")
    print(f"\nclient latency p50={statistics.median(latencies)*1000:.0f} ms "
          f"max={max(latencies)*1000:.0f} ms over {len(latencies)} calls "
          f"(candidate is configured with 1.5 s delay + 20% errors)")


if __name__ == "__main__":
    main(int(sys.argv[1]) if len(sys.argv) > 1 else 3)
