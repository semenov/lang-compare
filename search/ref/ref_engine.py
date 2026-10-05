#!/usr/bin/env python3
"""The reference implementation behind the SPEC.md CLI, used to validate run_tests.py.
`index` just copies the corpus; `serve` builds the in-memory reference index at startup."""
import json
import os
import shutil
import sys
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import search_ref as ref  # noqa: E402


def main():
    cmd = sys.argv[1]
    if cmd == "index":
        Path(sys.argv[3]).mkdir(parents=True, exist_ok=True)
        shutil.copy(sys.argv[2], Path(sys.argv[3]) / "corpus.jsonl")
        return
    corpus = Path(sys.argv[2]) / "corpus.jsonl"
    idx = ref.Index(corpus)
    docs = {d["id"]: d for d in map(json.loads, open(corpus, encoding="utf-8"))}

    class H(BaseHTTPRequestHandler):
        def send(self, code, obj):
            body = json.dumps(obj, ensure_ascii=False).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            u = urllib.parse.urlparse(self.path)
            if u.path == "/health":
                return self.send(200, {"status": "ok"})
            if u.path.startswith("/doc/"):
                try:
                    d = docs.get(int(u.path[5:]))
                except ValueError:
                    d = None
                return self.send(200, d) if d else self.send(404, {"error": "not found"})
            if u.path == "/search":
                qs = urllib.parse.parse_qs(u.query, keep_blank_values=True)
                if "q" not in qs:
                    return self.send(400, {"error": "missing q"})
                try:
                    k = int(qs.get("k", ["10"])[0])
                    assert 1 <= k <= 1000
                except (ValueError, AssertionError):
                    return self.send(400, {"error": "bad k"})
                total, hits = idx.search(qs["q"][0], k)
                return self.send(200, {"total": total, "hits": hits})
            self.send(404, {"error": "not found"})

        def log_message(self, *a):
            pass

    ThreadingHTTPServer.request_queue_size = 1024
    ThreadingHTTPServer(("0.0.0.0", int(os.environ.get("PORT", "8080"))), H).serve_forever()


if __name__ == "__main__":
    main()
