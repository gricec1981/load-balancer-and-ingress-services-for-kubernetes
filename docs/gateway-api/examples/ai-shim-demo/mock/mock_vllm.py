#!/usr/bin/env python3
"""Mock vLLM: streams OpenAI-style SSE deltas so the shim can be demoed
without GPUs. Behavior hooks driven by the prompt text:
  contains "leak"    -> emits SECRET-API-KEY-123 mid-stream (inline DLP kill)
  contains "scam"    -> emits "wire the funds" mid-stream (async scanner kill)
  contains "card"    -> emits a valid-Luhn PAN SPLIT across two SSE frames
                        (DLP redaction / cross-boundary catch demo)
  contains "long"    -> 200 tokens (pair with X-Token-Budget for budget kill)
  contains "burn"    -> 500 tokens at a fast cadence (cumulative-budget burn demos)
  otherwise          -> ~40 token pleasant answer
"""
import json, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

WORDS = ("Kubernetes native AI traffic governance runs on the service engine "
         "data plane with policy enforced at a single point ").split()

def tokens_for(prompt):
    p = prompt.lower()
    n = 500 if "burn" in p else (200 if "long" in p else 40)
    toks = [WORDS[i % len(WORDS)] + " " for i in range(n)]
    if "leak" in p:
        toks[12] = "SECRET-API-KEY-123 "
    if "scam" in p:
        toks[15:18] = ["wire ", "the ", "funds "]
    if "card" in p:
        # 4242424242424242 is Luhn-valid; split so the 16 digits straddle the
        # frame-14/15 boundary (no space between) -> per-frame scanning misses
        # it, the shim's accumulating window catches it.
        toks[14] = "4242424242"     # first 10 digits, no trailing space
        toks[15] = "424242 "        # last 6 digits + space -> contiguous PAN
    return toks

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        try:
            prompt = json.loads(raw)["messages"][-1]["content"]
        except Exception:
            prompt = ""
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        toks = tokens_for(prompt)
        cadence = 0.002 if "burn" in prompt.lower() else 0.08  # burn = fast, for budget demos
        for i, t in enumerate(toks):
            frame = {"choices": [{"delta": {"content": t}, "index": 0}]}
            self._sse(json.dumps(frame))
            time.sleep(cadence)                    # streaming cadence (fast for burn)
        self._sse(json.dumps({"choices": [{"delta": {}, "finish_reason": "stop"}],
                              "usage": {"completion_tokens": len(toks)}}))
        self._sse("[DONE]")
        self._chunk(b"")                           # terminal chunk
    def _sse(self, payload):
        self._chunk(f"data: {payload}\n\n".encode())
    def _chunk(self, b):
        self.wfile.write(f"{len(b):x}\r\n".encode() + b + b"\r\n")
        self.wfile.flush()
    def log_message(self, *a): pass

if __name__ == "__main__":
    print("mock vLLM streaming on :8000")
    ThreadingHTTPServer(("0.0.0.0", 8000), H).serve_forever()
