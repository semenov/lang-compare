#!/usr/bin/env python3
"""Benchmark mini-redis implementations (and real redis-server as a reference) with memtier_benchmark.

Usage: load.py [--servers go-1,rust-1,redis] [--threads 1,4,8] [--time 15] [--out results/run.json]

Binaries are built from impl/<name> into bin/ first. Everything runs on this machine;
memtier uses 8 threads x 25 connections, 127.0.0.1 (impls may listen on IPv4 only). Server CPU = delta of the process's CPU time.
"""
import argparse
import json
import os
import socket
import subprocess
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BIN = ROOT / "bin"
PORT = 6600


def build(name):
    src = ROOT / "impl" / name
    BIN.mkdir(exist_ok=True)
    out = BIN / name
    if name.startswith("go"):
        subprocess.run(["go", "build", "-o", str(out), "."], cwd=src, check=True)
    else:
        tgt = BIN / f"target-{name}"
        subprocess.run(["cargo", "build", "--release", "-q"], cwd=src, check=True,
                       env=dict(os.environ, CARGO_TARGET_DIR=str(tgt)))
        out.unlink(missing_ok=True)
        out.symlink_to(tgt / "release" / "mini-redis")
    return out


def start(name, threads):
    if name == "redis":
        cmd = ["redis-server", "--port", str(PORT), "--save", "", "--appendonly", "no",
               "--io-threads", str(threads), "--maxclients", "20000"]
        env = os.environ
    else:
        cmd = [str(BIN / name)]
        env = dict(os.environ, PORT=str(PORT), THREADS=str(threads))
    p = subprocess.Popen(cmd, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    deadline = time.time() + 10
    while True:
        try:
            socket.create_connection(("127.0.0.1", PORT), timeout=1).close()
            return p
        except OSError:
            if time.time() > deadline:
                p.kill()
                raise RuntimeError(f"{name} did not start")
            time.sleep(0.05)


def stop(p):
    p.kill()
    p.wait()
    time.sleep(0.5)


def ps(pid, field):
    out = subprocess.run(["ps", "-o", f"{field}=", "-p", str(pid)], capture_output=True, text=True).stdout.strip()
    return out


def rss_mb(pid):
    return int(ps(pid, "rss")) / 1024


def cpu_s(pid):
    t = ps(pid, "time")  # [[dd-]hh:]mm:ss.ss
    secs = 0.0
    for part in t.replace("-", ":").split(":"):
        secs = secs * 60 + float(part)
    return secs


def redis_cmd(*args):
    return subprocess.run(["redis-cli", "-h", "127.0.0.1", "-p", str(PORT), *args], capture_output=True, text=True).stdout.strip()


def memtier(*args):
    with tempfile.NamedTemporaryFile(suffix=".json") as f:
        subprocess.run(["memtier_benchmark", "-s", "127.0.0.1", "-p", str(PORT), "--hide-histogram",
                        "--print-percentiles", "50,99,99.9", "--json-out-file", f.name, *args],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
        t = json.load(open(f.name))["ALL STATS"]["Totals"]
    pl = t["Percentile Latencies"]
    return {"ops": round(t["Ops/sec"]), "p50": pl["p50.00"], "p99": pl["p99.00"],
            "p999": pl["p99.90"], "max": t["Max Latency"]}


def load_keys(n, size, clients=4):
    memtier("--ratio", "1:0", "--key-pattern", "P:P", "--key-minimum", "1", "--key-maximum", str(n),
            "-n", "allkeys", "-t", str(clients), "-c", "1", "--pipeline", "64", "-d", str(size))


def scenario(pid, name, *args):
    c0 = cpu_s(pid)
    r = memtier(*args)
    used = cpu_s(pid) - c0
    r["cpu"] = round(used / float(ARGS.time), 2)  # avg cores used
    r["ops_per_cpu_s"] = round(r["ops"] * ARGS.time / used) if used else None  # client-independent efficiency
    r["rss_mb"] = round(rss_mb(pid))
    print(f"    {name:<10} {r['ops']:>9} ops/s  p50 {r['p50']:.3f}  p99 {r['p99']:.3f}  p99.9 {r['p999']:.3f}  "
          f"max {r['max']:.1f} ms  cpu {r['cpu']}  ops/cpu-s {r['ops_per_cpu_s']}  rss {r['rss_mb']} MB", flush=True)
    return r


def bench(name, threads):
    p = start(name, threads)
    res = {}
    try:
        res["rss_idle_mb"] = round(rss_mb(p.pid), 1)
        load_keys(1_000_000, 100)
        res["rss_1m_mb"] = round(rss_mb(p.pid))
        common = ["-t", "8", "-c", "25", "--ratio", "1:10", "--key-pattern", "R:R",
                  "--key-minimum", "1", "--key-maximum", "1000000", "-d", "100", "--test-time", str(ARGS.time)]
        res["nopipe"] = scenario(p.pid, "no-pipe", *common)
        res["pipe16"] = scenario(p.pid, "pipe16", *common, "--pipeline", "16")
        res["rate100k"] = scenario(p.pid, "100k/s", *common, "--rate-limiting", "500")  # 200 conns x 500 = 100k ops/s
    finally:
        stop(p)
    return res


def memory(name):
    """RSS for 5M keys with 32-byte values (THREADS=4 / io-threads 1)."""
    p = start(name, 1 if name == "redis" else 4)
    try:
        base = rss_mb(p.pid)
        load_keys(5_000_000, 32)
        time.sleep(1)
        rss = rss_mb(p.pid)
        n = int(redis_cmd("DBSIZE"))
        r = {"keys": n, "rss_idle_mb": round(base, 1), "rss_mb": round(rss),
             "bytes_per_key": round((rss - base) * 1024 * 1024 / n, 1)}
        redis_cmd("FLUSHALL")
        time.sleep(2)
        r["rss_after_flush_mb"] = round(rss_mb(p.pid))
    finally:
        stop(p)
    print(f"  memory: {r}", flush=True)
    return r


def main():
    global ARGS
    ap = argparse.ArgumentParser()
    ap.add_argument("--servers", default="go-1,rust-1,redis")
    ap.add_argument("--threads", default="1,2,4")  # + 8 memtier threads, 14 cores total
    ap.add_argument("--time", type=int, default=15)
    ap.add_argument("--out", default=str(ROOT / "results" / f"bench-{time.strftime('%Y%m%d-%H%M')}.json"))
    ARGS = ap.parse_args()

    results = {"started": time.strftime("%Y-%m-%d %H:%M"), "time_s": ARGS.time, "servers": {}}
    for name in ARGS.servers.split(","):
        if name != "redis":
            build(name)
        print(f"== {name}", flush=True)
        r = results["servers"][name] = {"memory": memory(name)}
        for t in map(int, ARGS.threads.split(",")):
            print(f"  THREADS={t}", flush=True)
            r[f"t{t}"] = bench(name, t)
        Path(ARGS.out).parent.mkdir(exist_ok=True)
        Path(ARGS.out).write_text(json.dumps(results, indent=2))
    print(f"wrote {ARGS.out}")


if __name__ == "__main__":
    main()
