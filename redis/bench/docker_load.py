#!/usr/bin/env python3
"""Benchmark in Docker (Linux VM): the server and memtier share one network namespace, so traffic
goes over Linux loopback inside the VM instead of the macOS TCP stack (which caps at ~150k
round trips/s for the whole machine, see README).

Server: cpuset 0 (THREADS=1) or 0-3 (THREADS=4). memtier: cpuset 4-13, 8 threads x 25 connections.

Usage: docker_load.py [--servers go-1,rust-1,redis] [--threads 1,4] [--time 15] [--out ...]
Images: mini-redis:<name> built from impl/<name>/Dockerfile; redis:8; redislabs/memtier_benchmark.
"""
import argparse
import json
import subprocess
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SRV = "mr-bench-server"
CLIENT_CPUS = "4-13"
PROBE = "mr-bench-cgprobe"  # long-lived alpine with the VM's cgroup fs mounted, to read the server's counters


def sh(*args, check=True):
    return subprocess.run(args, capture_output=True, text=True, check=check).stdout.strip()


def start(name, threads):
    sh("docker", "rm", "-f", SRV, check=False)
    cpus = "0" if threads == 1 else f"0-{threads - 1}"
    base = ["docker", "run", "-d", "--name", SRV, "--cpuset-cpus", cpus, "--ulimit", "nofile=65536:65536"]
    if name == "redis":
        sh(*base, "redis:8", "redis-server", "--save", "", "--appendonly", "no", "--protected-mode", "no",
           "--io-threads", str(threads))
    else:
        sh(*base, "-e", f"THREADS={threads}", "-e", "PORT=6379", f"mini-redis:{name}")
    for _ in range(100):
        r = subprocess.run(["docker", "run", "--rm", "--network", f"container:{SRV}", "redis:8",
                            "redis-cli", "-h", "127.0.0.1", "PING"], capture_output=True, text=True)
        if r.stdout.strip() == "PONG":
            return
        time.sleep(0.1)
    raise RuntimeError(f"{name} did not start")


def cgroup():
    """Server container counters: cpu usage/user/system (s) and anon memory (MB, ~RSS)."""
    cid = sh("docker", "inspect", "-f", "{{.Id}}", SRV)
    out = sh("docker", "exec", PROBE, "sh", "-c", f"cat /cg/docker/{cid}/cpu.stat /cg/docker/{cid}/memory.stat")
    kv = dict(line.split()[:2] for line in out.splitlines() if len(line.split()) >= 2)
    return {"cpu": int(kv["usage_usec"]) / 1e6, "user": int(kv["user_usec"]) / 1e6,
            "sys": int(kv["system_usec"]) / 1e6, "anon_mb": int(kv["anon"]) / 2**20}


def memtier(outdir, *args):
    subprocess.run(["docker", "run", "--rm", "--network", f"container:{SRV}", "--cpuset-cpus", CLIENT_CPUS,
                    "-v", f"{outdir}:/out", "redislabs/memtier_benchmark",
                    "-s", "127.0.0.1", "-p", "6379", "--hide-histogram", "--print-percentiles", "50,99,99.9",
                    "--json-out-file", "/out/r.json", *args],
                   capture_output=True, check=True)
    t = json.load(open(Path(outdir) / "r.json"))["ALL STATS"]["Totals"]
    pl = t["Percentile Latencies"]
    return {"ops": round(t["Ops/sec"]), "p50": pl["p50.00"], "p99": pl["p99.00"], "p999": pl["p99.90"],
            "max": t["Max Latency"]}


def scenario(outdir, label, *args):
    c0, t0 = cgroup(), time.time()
    r = memtier(outdir, *args)
    c1, wall = cgroup(), time.time() - t0  # wall includes ~1 s of container start; cpu-s/op is exact
    r["cpu"] = round((c1["cpu"] - c0["cpu"]) / wall, 2)  # avg cores used
    r["sys_share"] = round((c1["sys"] - c0["sys"]) / max(c1["cpu"] - c0["cpu"], 1e-9), 2)
    r["ops_per_cpu_s"] = round(r["ops"] * ARGS.time / (c1["cpu"] - c0["cpu"]))
    r["mem_mb"] = round(c1["anon_mb"])
    print(f"    {label:<8} {r['ops']:>9} ops/s  p50 {r['p50']:.3f}  p99 {r['p99']:.3f}  p99.9 {r['p999']:.3f}  "
          f"max {r['max']:.1f} ms  cpu {r['cpu']} (sys {r['sys_share']:.0%})  ops/cpu-s {r['ops_per_cpu_s']}  "
          f"mem {r['mem_mb']} MB", flush=True)
    return r


def bench(name, threads, secs):
    start(name, threads)
    res = {}
    try:
        with tempfile.TemporaryDirectory() as d:
            memtier(d, "--ratio", "1:0", "--key-pattern", "P:P", "--key-minimum", "1", "--key-maximum", "1000000",
                    "-n", "allkeys", "-t", "4", "-c", "1", "--pipeline", "64", "-d", "100")
            common = ["-t", "8", "-c", "25", "--ratio", "1:10", "--key-pattern", "R:R", "--key-minimum", "1",
                      "--key-maximum", "1000000", "-d", "100", "--test-time", str(secs)]
            res["nopipe"] = scenario(d, "no-pipe", *common)
            res["pipe16"] = scenario(d, "pipe16", *common, "--pipeline", "16")
            res["rate100k"] = scenario(d, "100k/s", *common, "--rate-limiting", "500")
    finally:
        sh("docker", "rm", "-f", SRV, check=False)
    return res


def main():
    global ARGS
    ap = argparse.ArgumentParser()
    ap.add_argument("--servers", default="go-1,rust-1,redis")
    ap.add_argument("--threads", default="1,4")
    ap.add_argument("--time", type=int, default=15)
    ap.add_argument("--out", default=str(ROOT / "results" / f"docker-{time.strftime('%Y%m%d-%H%M')}.json"))
    a = ARGS = ap.parse_args()
    sh("docker", "rm", "-f", PROBE, check=False)
    sh("docker", "run", "-d", "--name", PROBE, "-v", "/sys/fs/cgroup:/cg:ro", "alpine", "sleep", "infinity")
    results = {"started": time.strftime("%Y-%m-%d %H:%M"), "time_s": a.time,
               "docker": sh("docker", "info", "--format", "{{.ServerVersion}} {{.NCPU}}cpu {{.MemTotal}}"),
               "servers": {}}
    for name in a.servers.split(","):
        print(f"== {name}", flush=True)
        for t in map(int, a.threads.split(",")):
            print(f"  THREADS={t}", flush=True)
            results["servers"].setdefault(name, {})[f"t{t}"] = bench(name, t, a.time)
            Path(a.out).write_text(json.dumps(results, indent=2))
    sh("docker", "rm", "-f", PROBE, check=False)
    print(f"wrote {a.out}")


if __name__ == "__main__":
    main()
