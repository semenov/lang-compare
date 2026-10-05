#!/usr/bin/env python3
"""Acceptance tests for the search engine (see SPEC.md).

Usage: run_tests.py <path-to-binary> [--port 18080] [-v]

Indexes testdata/corpus.jsonl into a temporary directory with `<binary> index`, starts `<binary> serve`,
then compares every query in testdata/expected.jsonl, checks /doc, /health, bad requests and concurrent use.
"""
import json
import os
import random
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

HERE = Path(__file__).resolve().parent
DATA = HERE / "testdata"
SCORE_REL = 1e-6   # allowed relative score error
TIE_REL = 1e-12    # scores this close count as a tie (order within a tie group is free)


def get(port, path, timeout=30):
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=timeout) as r:
            return r.status, r.headers.get("Content-Type", ""), r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Content-Type", ""), e.read()
    except (OSError, urllib.error.URLError) as e:
        return 0, "", f"connection error: {e}".encode()


def close(a, b, rel):
    return abs(a - b) <= rel * max(abs(a), abs(b), 1e-300)


def groups(hits):
    out = []
    for h in hits:
        if out and close(out[-1][0]["score"], h["score"], TIE_REL):
            out[-1].append(h)
        else:
            out.append([h])
    return out


def check_search(port, exp):
    q = urllib.parse.urlencode({"q": exp["q"], "k": exp["k"]})
    status, ctype, body = get(port, f"/search?{q}")
    if status != 200:
        return [f"status {status}: {body[:200]!r}"]
    if "application/json" not in ctype:
        return [f"Content-Type {ctype!r}"]
    try:
        got = json.loads(body)
    except ValueError:
        return [f"invalid JSON: {body[:200]!r}"]
    fails = []
    if got.get("total") != exp["total"]:
        fails.append(f"total: expected {exp['total']}, got {got.get('total')}")
    hits = got.get("hits")
    if not isinstance(hits, list) or len(hits) != len(exp["hits"]):
        return fails + [f"hits: expected {len(exp['hits'])}, got {len(hits) if isinstance(hits, list) else hits!r}"]
    i = 0
    eg = groups(exp["hits"])
    for gi, g in enumerate(eg):
        mine = hits[i:i + len(g)]
        i += len(g)
        for h, e in zip(mine, g):
            if not isinstance(h.get("score"), (int, float)) or not close(h["score"], e["score"], SCORE_REL):
                fails.append(f"rank {i - len(g) + mine.index(h) + 1}: score {h.get('score')} vs expected ~{e['score']}")
                return fails
        if gi < len(eg) - 1:
            # complete tie group: same documents, any order; but true ties must be ordered by id
            if sorted(h.get("id") for h in mine) != sorted(h["id"] for h in g):
                fails.append(f"ranks {i - len(g) + 1}-{i}: expected ids {[h['id'] for h in g]}, got {[h.get('id') for h in mine]}")
                return fails
        exp_titles = {h["id"]: h["title"] for h in g}
        for h in mine:
            if h.get("id") in exp_titles and h.get("title") != exp_titles[h["id"]]:
                fails.append(f"id {h['id']}: title {h.get('title')!r} vs {exp_titles[h['id']]!r}")
        for a, b in zip(mine, mine[1:]):
            if a.get("score") == b.get("score") and a.get("id", 0) > b.get("id", 0):
                fails.append(f"equal scores must be ordered by id: {a.get('id')} before {b.get('id')}")
    return fails


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    binary = str(Path(sys.argv[1]).resolve())
    port = int(sys.argv[sys.argv.index("--port") + 1]) if "--port" in sys.argv else 18080
    verbose = "-v" in sys.argv
    results = []  # (name, fails)

    def record(name, fails):
        results.append((name, fails))
        if fails:
            print(f"FAIL {name}")
            for f in fails[:8]:
                print(f"     {f}")
        elif verbose:
            print(f"ok   {name}")

    tmp = Path(tempfile.mkdtemp(prefix="search-test-"))
    idx = tmp / "index"
    srv = None
    try:
        t0 = time.time()
        r = subprocess.run([binary, "index", str(DATA / "corpus.jsonl"), str(idx)], capture_output=True, timeout=900)
        print(f"index: exit {r.returncode} in {time.time() - t0:.1f}s")
        if r.returncode != 0:
            print(r.stderr.decode(errors="replace")[-2000:])
            record("index", [f"exit code {r.returncode}"])
            raise SystemExit
        srv = subprocess.Popen([binary, "serve", str(idx)], env=dict(os.environ, PORT=str(port)),
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        t0 = time.time()
        while True:
            try:
                st, _, body = get(port, "/health", timeout=2)
                if st == 200 and json.loads(body) == {"status": "ok"}:
                    break
            except (OSError, ValueError):
                pass
            if srv.poll() is not None:
                record("serve", [f"server exited with {srv.returncode}: {srv.stderr.read().decode(errors='replace')[-1500:]}"])
                raise SystemExit
            if time.time() - t0 > 120:
                record("serve", ["/health not ready after 120 s"])
                raise SystemExit
            time.sleep(0.1)
        print(f"serve: ready in {time.time() - t0:.1f}s")
        record("health", [])

        expected = [json.loads(line) for line in open(DATA / "expected.jsonl", encoding="utf-8")]
        for e in expected:
            record(f"search q={e['q']!r} k={e['k']}", check_search(port, e))

        # /doc
        docs = [json.loads(line) for line in open(DATA / "corpus.jsonl", encoding="utf-8")]
        rnd = random.Random(7)
        sample = rnd.sample(docs, 40) + [d for d in docs if d["id"] >= 2_000_000_000]
        for d in sample:
            st, ctype, body = get(port, f"/doc/{d['id']}")
            fails = []
            if st != 200:
                fails.append(f"status {st}")
            else:
                got = json.loads(body)
                if got != {"id": d["id"], "title": d["title"], "text": d["text"]}:
                    fails.append(f"document differs: {str(got)[:200]}")
            record(f"doc {d['id']}", fails)
        for path in ["/doc/999999999", "/doc/1999999999"]:
            st, _, body = get(port, path)
            record(f"doc missing {path}", [] if st == 404 and "error" in json.loads(body) else [f"status {st} {body[:100]!r}"])

        # bad requests
        for path in ["/search", "/search?k=10", "/search?q=april&k=0", "/search?q=april&k=1001", "/search?q=april&k=abc"]:
            st, _, body = get(port, path)
            ok = st == 400
            try:
                ok = ok and "error" in json.loads(body)
            except ValueError:
                ok = False
            record(f"bad request {path}", [] if ok else [f"expected 400 with error JSON, got {st} {body[:100]!r}"])
        st, _, body = get(port, "/search?q=april")
        record("default k=10", [] if st == 200 and len(json.loads(body)["hits"]) == 10 else [f"status {st}"])

        # concurrency: the same queries from 32 threads at once must give the same answers
        conc = rnd.sample([e for e in expected if e["k"] == 10], 150) * 4
        rnd.shuffle(conc)
        t0 = time.time()
        with ThreadPoolExecutor(32) as ex:
            fails = [f for fs in ex.map(lambda e: check_search(port, e), conc) for f in fs]
        record(f"concurrent ({len(conc)} queries, 32 threads, {time.time() - t0:.1f}s)", fails[:5])
    except SystemExit:
        pass
    finally:
        if srv:
            srv.kill()
            srv.wait()
        shutil.rmtree(tmp, ignore_errors=True)

    failed = sum(1 for _, f in results if f)
    print(f"\n{len(results) - failed} passed, {failed} failed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
