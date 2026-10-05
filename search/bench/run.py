#!/usr/bin/env python3
"""Benchmark search engines on a corpus.

Usage: run.py <corpus.jsonl> name=binary [name=binary ...] [--time 20] [--conc 1,32] [--out results/x.json]

For each engine: index (wall, CPU, peak RSS, index size), serve startup (time to /health, RSS),
then oha load with a fixed query mix at each concurrency (rps, p50/p99, server CPU and RSS).
Finally the top-10 results of all engines are compared on a sample of queries.
The query mix is generated once per corpus (bench/queries-<corpus>.txt) from a sample of its documents.
"""
import json
import random
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "ref"))
import search_ref as ref  # noqa: E402

PORT = 18200


def make_queries(corpus, path, n=5000, seed=3):
    rnd = random.Random(seed)
    docs = []
    with open(corpus, encoding="utf-8") as f:
        for i, line in enumerate(f):
            if i % 50 == 0:
                docs.append(json.loads(line))
            if len(docs) >= 4000:
                break
    df = {}
    toks_by_doc = []
    for d in docs:
        toks = ref.tokenize(d["title"] + "\n" + d["text"])
        toks_by_doc.append(toks)
        for t in set(toks):
            df[t] = df.get(t, 0) + 1
    ranked = sorted(df, key=lambda t: -df[t])
    common, mid = ranked[:100], ranked[100:5000]
    out = []
    for _ in range(n):
        r = rnd.random()
        if r < 0.3:
            q = rnd.choice(mid)
        elif r < 0.4:
            q = rnd.choice(common)
        elif r < 0.7:
            q = " ".join(rnd.sample(mid, rnd.randint(2, 3)))
        elif r < 0.9:
            toks = rnd.choice([t for t in toks_by_doc if len(t) > 10])
            L = rnd.randint(2, 4)
            i = rnd.randrange(len(toks) - L)
            q = '"' + " ".join(toks[i:i + L]) + '"'
        else:
            q = f"{rnd.choice(mid)} {rnd.choice(common)} -{rnd.choice(mid)}"
        out.append(q)
    path.write_text("\n".join(out) + "\n", encoding="utf-8")


def time_l(cmd):
    r = subprocess.run(["/usr/bin/time", "-l", *cmd], capture_output=True, text=True)
    e = r.stderr
    return dict(rc=r.returncode, wall=float(re.search(r"([\d.]+) real", e)[1]),
                cpu=float(re.search(r"([\d.]+) user", e)[1]) + float(re.search(r"([\d.]+) sys", e)[1]),
                rss_mb=int(re.search(r"(\d+)\s+maximum resident set size", e)[1]) / 2**20, stderr=e)


def ps(pid, field):
    return subprocess.run(["ps", "-o", f"{field}=", "-p", str(pid)], capture_output=True, text=True).stdout.strip()


def cpu_s(pid):
    secs = 0.0
    for part in ps(pid, "time").replace("-", ":").split(":"):
        secs = secs * 60 + float(part)
    return secs


def rss_mb(pid):
    return int(ps(pid, "rss") or 0) / 1024


def get(path):
    with urllib.request.urlopen(f"http://127.0.0.1:{PORT}{path}", timeout=60) as r:
        return json.loads(r.read())


def bench_engine(name, binary, corpus, queries, secs, concs, tmp):
    res = {}
    idx = tmp / name
    print(f"== {name}", flush=True)
    r = time_l([binary, "index", str(corpus), str(idx)])
    if r["rc"]:
        print(r["stderr"][-1500:])
        raise SystemExit(f"{name}: index failed")
    size = sum(f.stat().st_size for f in idx.rglob("*") if f.is_file()) / 2**20
    res["index"] = dict(wall=r["wall"], cpu=r["cpu"], peak_rss_mb=round(r["rss_mb"]), size_mb=round(size))
    print(f"  index: {r['wall']:.1f}s wall, {r['cpu']:.1f}s cpu ({r['cpu'] / r['wall']:.1f} cores), "
          f"peak RSS {r['rss_mb']:.0f} MB, index {size:.0f} MB", flush=True)

    srv = subprocess.Popen([binary, "serve", str(idx)], env={"PORT": str(PORT), "PATH": "/usr/bin:/bin"},
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    t0 = time.time()
    while True:
        try:
            if get("/health") == {"status": "ok"}:
                break
        except OSError:
            pass
        if srv.poll() is not None or time.time() - t0 > 600:
            raise SystemExit(f"{name}: server did not start")
        time.sleep(0.02)
    res["startup"] = dict(seconds=round(time.time() - t0, 3), rss_mb=round(rss_mb(srv.pid)))
    print(f"  startup: {res['startup']['seconds']}s, RSS {res['startup']['rss_mb']} MB", flush=True)
    try:
        urls = tmp / "urls.txt"
        urls.write_text("".join(f"http://127.0.0.1:{PORT}/search?{urllib.parse.urlencode({'q': q, 'k': 10})}\n"
                                for q in queries), encoding="utf-8")
        # warm-up pass over the whole query set
        subprocess.run(["oha", "-n", str(len(queries)), "-c", "16", "--no-tui", "--urls-from-file", str(urls),
                        "--output-format", "quiet"], check=True)
        for c in concs:
            c0 = cpu_s(srv.pid)
            out = subprocess.run(["oha", "-z", f"{secs}s", "-c", str(c), "--no-tui", "--urls-from-file", str(urls),
                                  "--output-format", "json"], capture_output=True, text=True, check=True).stdout
            j = json.loads(out)
            used = cpu_s(srv.pid) - c0
            s, p = j["summary"], j["latencyPercentiles"]
            codes = j.get("statusCodeDistribution", {})
            r = dict(rps=round(s["requestsPerSec"]), p50_ms=round(p["p50"] * 1000, 3), p99_ms=round(p["p99"] * 1000, 3),
                     p999_ms=round(p["p99.9"] * 1000, 3), success=s["successRate"],
                     non200=sum(v for k, v in codes.items() if k != "200"),
                     cpu_cores=round(used / secs, 2), rss_mb=round(rss_mb(srv.pid)))
            res[f"c{c}"] = r
            print(f"  c={c:<3} {r['rps']:>7} rps  p50 {r['p50_ms']:.2f}  p99 {r['p99_ms']:.2f}  p99.9 {r['p999_ms']:.2f} ms  "
                  f"cpu {r['cpu_cores']}  RSS {r['rss_mb']} MB  non-200 {r['non200']}", flush=True)
        res["sample"] = {q: get(f"/search?{urllib.parse.urlencode({'q': q, 'k': 10})}") for q in queries[:300]}
    finally:
        srv.kill()
        srv.wait()
        shutil.rmtree(idx, ignore_errors=True)
    return res


def main():
    args, opts = [], {"--time": "20", "--conc": "1,32", "--out": None}
    it = iter(sys.argv[1:])
    for a in it:
        if a in opts:
            opts[a] = next(it)
        else:
            args.append(a)
    corpus = Path(args[0]).resolve()
    engines = [a.split("=", 1) for a in args[1:]]
    qpath = ROOT / "bench" / f"queries-{corpus.stem}.txt"
    if not qpath.exists():
        make_queries(corpus, qpath)
    queries = qpath.read_text(encoding="utf-8").splitlines()
    out = Path(opts["--out"] or ROOT / "results" / f"bench-{corpus.stem}-{time.strftime('%Y%m%d-%H%M')}.json")

    tmp = Path(tempfile.mkdtemp(prefix="search-bench-", dir=str(ROOT / "data")))
    results = {"corpus": str(corpus), "started": time.strftime("%Y-%m-%d %H:%M"), "engines": {}}
    try:
        for name, binary in engines:
            results["engines"][name] = bench_engine(name, str(Path(binary).resolve()), corpus, queries,
                                                    int(opts["--time"]), [int(c) for c in opts["--conc"].split(",")], tmp)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    names = list(results["engines"])
    if len(names) > 1:
        a = results["engines"][names[0]]["sample"]
        for other in names[1:]:
            b = results["engines"][other]["sample"]
            same_total = sum(a[q]["total"] == b[q]["total"] for q in a)
            same_ids = sum([h["id"] for h in a[q]["hits"]] == [h["id"] for h in b[q]["hits"]] for q in a)
            print(f"consistency {names[0]} vs {other}: total equal {same_total}/{len(a)}, top-10 ids equal {same_ids}/{len(a)}")
    for e in results["engines"].values():
        e.pop("sample", None)
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps(results, indent=2))
    print(f"wrote {out}")


if __name__ == "__main__":
    main()
