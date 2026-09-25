#!/usr/bin/env python3
"""Local-only fake OpenAI-compatible provider for integration smoke tests.

Never use this as a real upstream: it fabricates usage numbers. It exists so the
Compose stack and `scripts/e2e_smoke.sh` can be exercised without第三方凭据.

    python3 scripts/mock_openai_provider.py                   # 127.0.0.1:19000
    MOCK_HOST=0.0.0.0 MOCK_PORT=9000 MOCK_MODELS=fake-chat python3 ...
"""
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os

MODELS = [m for m in os.getenv("MOCK_MODELS", "fake-chat").split(",") if m]
PROMPT_TOKENS = int(os.getenv("MOCK_PROMPT_TOKENS", "3"))
COMPLETION_TOKENS = int(os.getenv("MOCK_COMPLETION_TOKENS", "2"))


class Handler(BaseHTTPRequestHandler):
    def log_message(self, _fmt, *_args):
        # Do not log request bodies or credentials.
        pass

    def do_GET(self):
        if self.path.endswith("/models"):
            self.send_json(200, {"object": "list", "data": [
                {"id": model, "object": "model", "owned_by": "local-test"} for model in MODELS
            ]})
        else:
            self.send_json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        if not self.path.endswith("/chat/completions"):
            self.send_json(404, {"error": {"message": "not found"}})
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        usage = {
            "prompt_tokens": PROMPT_TOKENS,
            "completion_tokens": COMPLETION_TOKENS,
            "total_tokens": PROMPT_TOKENS + COMPLETION_TOKENS,
        }
        model = body.get("model", MODELS[0] if MODELS else "fake-chat")
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            self.wfile.write(
                b'data: {"id":"mock","object":"chat.completion.chunk","model":"%s","choices":[{"delta":{"content":"ok"},"index":0}]}\n\n'
                % model.encode()
            )
            self.wfile.write(
                b'data: {"id":"mock","object":"chat.completion.chunk","model":"%s","choices":[],"usage":%s}\n\n'
                % (model.encode(), json.dumps(usage).encode())
            )
            self.wfile.write(b"data: [DONE]\n\n")
            return
        self.send_json(200, {
            "id": "mock-completion",
            "object": "chat.completion",
            "created": 1,
            "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
            "usage": usage,
        })

    def send_json(self, status, obj):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    host = os.getenv("MOCK_HOST", "127.0.0.1")
    port = int(os.getenv("MOCK_PORT", "19000"))
    HTTPServer((host, port), Handler).serve_forever()
