"""Throwaway mock of an OpenAI-compatible /v1/chat/completions endpoint.

Its "policy" is deterministic and depends on the system prompt, so a prompt
change produces visibly different decisions. Every request is logged as JSONL
so the evaluator can join primary and shadow calls on the W3C trace id.
"""
import json
import os
import random
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

NAME = os.environ.get("NAME", "llm")
DELAY_S = float(os.environ.get("DELAY_S", "0"))
ERROR_RATE = float(os.environ.get("ERROR_RATE", "0"))
LOG = f"/logs/{NAME}.jsonl"


def decide(system: str, user: str, tools: list) -> dict:
    """Pretend to be a model: choose a tool call or a text answer."""
    names = {t.get("function", {}).get("name") for t in tools if t.get("type") == "function"}
    s, u = system.lower(), user.lower()
    if "refund" in u and "issue_refund" in names and "may issue refunds" in s:
        return {"tool": "issue_refund", "args": {"order_id": "A-1001", "reason": "damaged"}}
    if ("order" in u or "refund" in u) and "lookup_order" in names:
        return {"tool": "lookup_order", "args": {"order_id": "A-1001"}}
    if "concise" in s:
        return {"text": "Happy to help - what is your order number?"}
    return {"text": "Thanks for reaching out! Could you share your order number so I can look into this for you?"}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # quiet
        pass

    def do_POST(self):
        t0 = time.time()
        body = json.loads(self.rfile.read(int(self.headers.get("content-length", 0))) or b"{}")
        msgs = body.get("messages", [])
        system = next((m["content"] for m in msgs if m.get("role") == "system"), "")
        user = next((m["content"] for m in reversed(msgs) if m.get("role") == "user"), "")
        tools = body.get("tools", [])
        traceparent = self.headers.get("traceparent", "")
        trace_id = traceparent.split("-")[1] if traceparent.count("-") >= 3 else ""

        if DELAY_S:
            time.sleep(DELAY_S)
        fail = random.random() < ERROR_RATE
        d = decide(system, user, tools)
        if "tool" in d:
            msg = {"role": "assistant", "content": None, "tool_calls": [{
                "id": "call_1", "type": "function",
                "function": {"name": d["tool"], "arguments": json.dumps(d["args"])}}]}
        else:
            msg = {"role": "assistant", "content": d["text"]}
        resp = {"id": f"{NAME}-{int(t0*1000)}", "object": "chat.completion", "model": body.get("model"),
                "choices": [{"index": 0, "message": msg, "finish_reason": "tool_calls" if "tool" in d else "stop"}],
                "usage": {"prompt_tokens": len(json.dumps(msgs)) // 4, "completion_tokens": 20}}

        with open(LOG, "a") as f:
            f.write(json.dumps({
                "backend": NAME, "trace_id": trace_id, "ts": t0,
                "host": self.headers.get("host"),
                "shadow_header": self.headers.get("x-shadowbox-experiment"),
                "authorization_present": "authorization" in {k.lower() for k in self.headers.keys()},
                "model": body.get("model"), "system": system, "user": user,
                "tool_types": [t.get("type") for t in tools],
                "decision": d, "status": 500 if fail else 200,
            }) + "\n")

        out = json.dumps({"error": "injected failure"} if fail else resp).encode()
        self.send_response(500 if fail else 200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
