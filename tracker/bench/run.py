#!/usr/bin/env python3
"""Benchmark tracker implementations in Docker.

Usage: run.py go-1 rust-1 [--issues 1000000] [--comments 100000] [--duration 60s] [--vus 64] [--rate 500]

Per implementation (image tracker:<name>, built from impl/<name>/Dockerfile):
  - fresh database bench_<name> in the tracker-postgres container (cpuset 2-9)
  - app container: cpuset 0-1 (2 CPUs), 1 GiB memory limit; startup time until /readyz; idle memory
  - seed through the API (bench/seed.py): ~1M issues, transitions, comments; timings
  - a webhook subscribed to all events, delivered to a fast sink container (hashicorp/http-echo)
  - k6 (cpuset 10-13, same Docker network): saturation with N VUs, then a fixed request rate
  - app CPU (cgroup cpu.stat) and memory (cgroup memory.current, sampled every 0.5 s) for each phase
Results: results/<name>.json + k6 summaries.
"""
import argparse
import json
import subprocess
import threading
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BENCH = ROOT / "bench"
RES = ROOT / "results"
NET = "tracker_default"
APP, SINK, PROBE = "trk-app", "trk-sink", "trk-probe"
JWT_SECRET = "bench-secret-0123456789abcdef"
HOST_PORT = 18080


def sh(*args, check=True):
    return subprocess.run(args, capture_output=True, text=True, check=check).stdout.strip()


def psql(sql, db="postgres"):
    return sh("docker", "exec", "tracker-postgres", "psql", "-U", "tracker", "-d", db, "-Atc", sql)


def cgroup(name):
    cid = sh("docker", "inspect", "-f", "{{.Id}}", name)
    out = sh("docker", "exec", PROBE, "sh", "-c",
             f"cat /cg/docker/{cid}/cpu.stat /cg/docker/{cid}/memory.stat; echo current $(cat /cg/docker/{cid}/memory.current)")
    kv = dict(line.split()[:2] for line in out.splitlines() if len(line.split()) >= 2)
    return {"cpu_s": int(kv["usage_usec"]) / 1e6, "mem_mb": int(kv["current"]) / 2**20, "anon_mb": int(kv["anon"]) / 2**20}


class MemSampler(threading.Thread):
    def __init__(self, name):
        super().__init__(daemon=True)
        self.name_, self.peak, self.peak_anon, self.stop_ = name, 0.0, 0.0, threading.Event()

    def run(self):
        while not self.stop_.is_set():
            try:
                c = cgroup(self.name_)
                self.peak, self.peak_anon = max(self.peak, c["mem_mb"]), max(self.peak_anon, c["anon_mb"])
            except subprocess.CalledProcessError:
                pass
            time.sleep(0.5)

    def stop(self):
        self.stop_.set()
        self.join()


def http(method, path, body=None, token=None):
    r = urllib.request.Request(f"http://127.0.0.1:{HOST_PORT}/api/v1{path}", method=method,
                               data=json.dumps(body).encode() if body is not None else None)
    r.add_header("Content-Type", "application/json")
    if token:
        r.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(r, timeout=60) as resp:
        return json.loads(resp.read() or b"null")


def phase(name, impl, k6_args, out):
    c0, t0 = cgroup(APP), time.time()
    sampler = MemSampler(APP)
    sampler.start()
    k6 = subprocess.run(["docker", "run", "--rm", "--network", NET, "--cpuset-cpus", "10-13", "-v", f"{BENCH}:/bench",
                         "-v", f"{RES}:/res", "grafana/k6", "run", "-e", f"BASE=http://{APP}:8080",
                         "-e", f"SEED=/res/seed-{impl}.json", "-e", f"OUT=/res/k6-{impl}-{name}.json", *k6_args,
                         "/bench/load.js"], capture_output=True, text=True)
    (RES / f"k6-{impl}-{name}.log").write_text(k6.stdout + k6.stderr)
    sampler.stop()
    c1, wall = cgroup(APP), time.time() - t0
    k6 = json.load(open(RES / f"k6-{impl}-{name}.json"))
    r = {"rps": round(k6["http_reqs"]["rate"]), "failed_rate": k6["failed"]["rate"], "checks": k6["checks"]["rate"],
         "app_cpu_cores": round((c1["cpu_s"] - c0["cpu_s"]) / wall, 2),
         "app_mem_peak_mb": round(sampler.peak), "app_anon_peak_mb": round(sampler.peak_anon),
         "latency_ms": {op: {k: round(v[k], 2) for k in ("p(50)", "p(90)", "p(99)", "max")} for op, v in k6["ops"].items()},
         "latency_all_ms": {k: round(k6["all"][k], 2) for k in ("p(50)", "p(90)", "p(99)", "p(99.9)")}}
    out[name] = r
    print(f"  {name}: {r['rps']} req/s, app CPU {r['app_cpu_cores']} cores, mem peak {r['app_mem_peak_mb']} MB "
          f"(anon {r['app_anon_peak_mb']}), p50/p99 {r['latency_all_ms']['p(50)']}/{r['latency_all_ms']['p(99)']} ms, "
          f"checks {r['checks'] * 100:.2f}%", flush=True)


def bench(impl, a):
    db = f"bench_{impl.replace('-', '_')}"
    res = {"impl": impl, "image_mb": round(int(sh("docker", "image", "inspect", "-f", "{{.Size}}", f"tracker:{impl}")) / 1e6, 2)}
    print(f"== {impl} (image {res['image_mb']} MB)", flush=True)
    sh("docker", "rm", "-f", APP, check=False)
    psql(f"DROP DATABASE IF EXISTS {db} WITH (FORCE)")
    psql(f"CREATE DATABASE {db}")
    t0 = time.time()
    sh("docker", "run", "-d", "--name", APP, "--network", NET, "--cpuset-cpus", "0-1", "--memory", "1g",
       "-p", f"{HOST_PORT}:8080", "-e", f"DATABASE_URL=postgres://tracker:tracker@tracker-postgres:5432/{db}",
       "-e", f"JWT_SECRET={JWT_SECRET}", f"tracker:{impl}")
    while True:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{HOST_PORT}/readyz", timeout=1) as r:
                if r.status == 200:
                    break
        except OSError:
            pass
        if time.time() - t0 > 60:
            raise SystemExit("app did not become ready")
        time.sleep(0.01)
    res["startup_s"] = round(time.time() - t0, 3)
    time.sleep(2)
    res["idle_mem_mb"] = round(cgroup(APP)["mem_mb"], 1)
    print(f"  startup {res['startup_s']} s, idle memory {res['idle_mem_mb']} MB", flush=True)

    c0 = cgroup(APP)
    sampler = MemSampler(APP)
    sampler.start()
    t0 = time.time()
    subprocess.run(["python3", str(BENCH / "seed.py"), f"http://127.0.0.1:{HOST_PORT}", "--issues", str(a.issues),
                    "--comments", str(a.comments), "--transitions", str(a.transitions), "--out", str(RES / f"seed-{impl}.json")],
                   check=True)
    sampler.stop()
    seed = json.load(open(RES / f"seed-{impl}.json"))
    res["seed"] = dict(seed["timings"], wall_s=round(time.time() - t0, 1), app_cpu_s=round(cgroup(APP)["cpu_s"] - c0["cpu_s"], 1),
                       app_mem_peak_mb=round(sampler.peak))
    res["db_size_mb"] = round(int(psql(f"SELECT pg_database_size('{db}')")) / 2**20)
    print(f"  seed: {res['seed']}, db {res['db_size_mb']} MB", flush=True)

    owner = http("POST", "/auth/login", {"email": "owner@acme.test", "password": seed["password"]})["access_token"]
    http("POST", "/orgs/acme/webhooks", {"url": f"http://{SINK}:5678/hook",
                                          "events": ["issue.created", "issue.updated", "issue.deleted", "comment.created"]}, owner)
    psql("VACUUM ANALYZE", db)
    time.sleep(3)
    res["after_seed_mem_mb"] = round(cgroup(APP)["mem_mb"], 1)

    phase("warmup", impl, ["-e", "MODE=saturate", "-e", f"VUS={a.vus}", "-e", "DURATION=20s"], {})
    phase("saturate", impl, ["-e", "MODE=saturate", "-e", f"VUS={a.vus}", "-e", f"DURATION={a.duration}"], res)
    phase("rate", impl, ["-e", "MODE=rate", "-e", f"RATE={a.rate}", "-e", f"DURATION={a.duration}"], res)
    time.sleep(5)
    res["after_load_mem_mb"] = round(cgroup(APP)["mem_mb"], 1)
    sh("docker", "stop", "-t", "15", APP)
    sh("docker", "rm", APP)
    json.dump(res, open(RES / f"{impl}.json", "w"), indent=1)
    return res


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("impls", nargs="+")
    ap.add_argument("--issues", type=int, default=1_000_000)
    ap.add_argument("--comments", type=int, default=100_000)
    ap.add_argument("--transitions", type=int, default=100_000)
    ap.add_argument("--duration", default="60s")
    ap.add_argument("--vus", type=int, default=64)
    ap.add_argument("--rate", type=int, default=500)
    ap.add_argument("--results", default="results")
    a = ap.parse_args()
    global RES
    RES = ROOT / a.results
    RES.mkdir(parents=True, exist_ok=True)
    sh("docker", "update", "--cpuset-cpus", "2-9", "tracker-postgres")
    for n in (SINK, PROBE):
        sh("docker", "rm", "-f", n, check=False)
    sh("docker", "run", "-d", "--name", SINK, "--network", NET, "--cpuset-cpus", "10-13", "hashicorp/http-echo",
       "-listen=:5678", "-text=ok")
    sh("docker", "run", "-d", "--name", PROBE, "-v", "/sys/fs/cgroup:/cg:ro", "alpine", "sleep", "infinity")
    try:
        for impl in a.impls:
            bench(impl, a)
    finally:
        for n in (APP, SINK, PROBE):
            sh("docker", "rm", "-f", n, check=False)


if __name__ == "__main__":
    main()
