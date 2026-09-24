#!/usr/bin/env python3
"""Native (macOS, no Docker) load benchmark.

macOS has no cgroups, so instead of CPU quotas each app is limited by its own
thread knob: WORKERS (node cluster), GOMAXPROCS (Go), TOKIO_WORKER_THREADS (Rust).
CPU = cumulative CPU time of the process tree (ps), memory = summed RSS.
Postgres + catalog come from bench/native/infra.sh. Results -> results/native/load.json

Usage: load.py [variant...]   (default: all variants in VARIANTS)
"""
import json, os, subprocess, sys, threading, time, urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
os.chdir(ROOT)
PORT = 18080
CONNS = 64
WARMUP, DURATION = 5, 15
NODE = os.path.expanduser("~/.nvm/versions/node/v26.10.0/bin/node")
BUN = os.path.expanduser("~/.bun/bin/bun")
ORDER_BODY = '{"user_id":42,"items":[{"sku":"SKU-17","qty":2},{"sku":"SKU-4242","qty":1}]}'

SCENARIOS = [
    ("health",       "GET",  r"/health", None),
    ("get_user",     "GET",  r"/users/[1-9][0-9]{0,3}", None),
    ("proxy",        "GET",  r"/products/SKU-[1-9][0-9]{0,3}", None),
    ("get_order",    "GET",  r"/orders/[1-9][0-9]{0,4}", None),
    ("list_orders",  "GET",  r"/users/[1-9][0-9]{0,3}/orders\?limit=20", None),
    ("create_order", "POST", r"/orders", ORDER_BODY),
]
FIXED_RATE = [("get_user", 2000), ("create_order", 1000)]

# variant -> (command, env for N threads)
VARIANTS = {
    "ts":          ([NODE, "ts/dist/main.js"],             lambda n: {"WORKERS": str(n), "DB_POOL_SIZE": str(20 // n)}),
    "ts-bun":      ([BUN, "ts/dist/main.js"],              lambda n: {"WORKERS": str(n), "DB_POOL_SIZE": str(20 // n)}),
    "py":          (["py/.venv/bin/python", "-m", "app"],  lambda n: {"WORKERS": str(n), "DB_POOL_SIZE": str(20 // n), "PYTHONPATH": "py"}),
    "go":          (["/tmp/lc-go"],                        lambda n: {"GOMAXPROCS": str(n)}),
    "go-fasthttp": (["/tmp/lc-go-fasthttp"],               lambda n: {"GOMAXPROCS": str(n)}),
    "rust":        (["rust/target/release/orders-api"],    lambda n: {"TOKIO_WORKER_THREADS": str(n)}),
    "rust-sqlx08": (["/tmp/lc-rust-sqlx08"],               lambda n: {"TOKIO_WORKER_THREADS": str(n)}),
}
THREADS = [1, 4]


def tree_pids(pid):
    pids, frontier = [pid], [pid]
    while frontier:
        out = subprocess.run(["pgrep", "-P", ",".join(map(str, frontier))], capture_output=True, text=True).stdout
        frontier = [int(x) for x in out.split()]
        pids += frontier
    return pids


def ps_fields(pids, field):
    out = subprocess.run(["ps", "-o", f"{field}=", "-p", ",".join(map(str, pids))], capture_output=True, text=True).stdout
    return [x.strip() for x in out.splitlines() if x.strip()]


def cpu_seconds(pid):
    total = 0.0
    for t in ps_fields(tree_pids(pid), "time"):  # [[hh:]mm:]ss.cc
        parts = [float(p) for p in t.split(":")]
        total += sum(p * 60 ** i for i, p in enumerate(reversed(parts)))
    return total


def rss_mb(pid):
    return sum(int(x) for x in ps_fields(tree_pids(pid), "rss")) / 1024


def oha(method, path, body, duration, rate=None):
    args = ["oha", "--no-tui", "--output-format", "json", "-c", str(CONNS), "-z", f"{duration}s",
            "-m", method, "--rand-regex-url"]
    if rate:
        args += ["-q", str(rate)]
    if body:
        args += ["-H", "Content-Type: application/json", "-d", body]
    args.append(f"http://127.0.0.1:{PORT}{path}")
    return json.loads(subprocess.run(args, capture_output=True, text=True).stdout)


def run_scenario(pid, scen, rate=None):
    name, method, path, body = scen
    oha(method, path, body, WARMUP, rate)
    peak, stop = [0.0], threading.Event()

    def sampler():
        while not stop.is_set():
            peak[0] = max(peak[0], rss_mb(pid))
            time.sleep(0.25)

    th = threading.Thread(target=sampler)
    c0, w0 = cpu_seconds(pid), time.time()
    th.start()
    r = oha(method, path, body, DURATION, rate)
    c1, w1 = cpu_seconds(pid), time.time()
    stop.set(); th.join()
    s = r["summary"]
    codes = r.get("statusCodeDistribution", {})
    total = sum(codes.values())
    ok = sum(v for k, v in codes.items() if k.startswith("2"))
    res = {
        "rps": round(s["requestsPerSec"], 1),
        "p50_ms": round(r["latencyPercentiles"]["p50"] * 1000, 2),
        "p99_ms": round(r["latencyPercentiles"]["p99"] * 1000, 2),
        "success": round(ok / total, 4) if total else 0,
        "cpu_cores": round((c1 - c0) / (w1 - w0), 3),
        "peak_rss_mb": round(peak[0], 1),
    }
    res["rps_per_core"] = round(res["rps"] / res["cpu_cores"]) if res["cpu_cores"] else None
    return res


def main():
    variants = sys.argv[1:] or list(VARIANTS)
    out_path = "results/native/load.json"
    os.makedirs("results/native", exist_ok=True)
    results = json.load(open(out_path)) if os.path.exists(out_path) else {}
    for v in variants:
        cmd, env_for = VARIANTS[v]
        for n in THREADS:
            print(f"\n### {v} threads={n}", flush=True)
            subprocess.run(["bench/native/infra.sh", "reset-db"], check=True)
            env = dict(os.environ, PORT=str(PORT), DATABASE_URL="postgres://app@localhost:15432/app",
                       CATALOG_URL="http://127.0.0.1:9000", **env_for(n))
            if subprocess.run(["lsof", "-ti", f"tcp:{PORT}", "-sTCP:LISTEN"], capture_output=True).stdout:
                sys.exit(f"port {PORT} is already in use; refusing to benchmark a stray process")
            t0 = time.time()
            proc = subprocess.Popen(cmd, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            while True:
                try:
                    urllib.request.urlopen(f"http://127.0.0.1:{PORT}/health", timeout=1).read()
                    break
                except Exception:
                    if proc.poll() is not None:
                        sys.exit(f"{v} exited with code {proc.returncode} before becoming healthy")
                    if time.time() - t0 > 30:
                        raise
                    time.sleep(0.005)
            entry = {"startup_s": round(time.time() - t0, 3)}
            time.sleep(2)
            entry["idle_rss_mb"] = round(rss_mb(proc.pid), 1)
            print(f"startup {entry['startup_s']}s idle rss {entry['idle_rss_mb']} MB", flush=True)
            entry["scenarios"] = {}
            for scen in SCENARIOS:
                res = run_scenario(proc.pid, scen)
                entry["scenarios"][scen[0]] = res
                print(f"  {scen[0]:13} {json.dumps(res)}", flush=True)
            if n == 1:
                entry["fixed_rate"] = {}
                for sname, rate in FIXED_RATE:
                    scen = next(s for s in SCENARIOS if s[0] == sname)
                    res = run_scenario(proc.pid, scen, rate)
                    entry["fixed_rate"][f"{sname}@{rate}"] = res
                    print(f"  {sname}@{rate:<6} {json.dumps(res)}", flush=True)
            pids = tree_pids(proc.pid)
            subprocess.run(["kill", *map(str, pids)])
            proc.wait()
            while subprocess.run(["lsof", "-ti", f"tcp:{PORT}", "-sTCP:LISTEN"], capture_output=True).stdout:
                time.sleep(0.1)  # make sure no stray worker keeps the port
            results.setdefault(v, {})[f"{n}thr"] = entry
            json.dump(results, open(out_path, "w"), indent=1)


if __name__ == "__main__":
    main()
