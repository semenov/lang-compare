#!/usr/bin/env python3
"""Load benchmark. For each language/CPU config: fresh Postgres, start app, measure
startup time and idle memory, then run oha scenarios while sampling the app's cgroup
(CPU usage, memory). Results -> results/load.json.

Usage: load.py [langs...]   (default: ts go rust)
"""
import datetime, json, os, subprocess, sys, threading, time, urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
os.chdir(ROOT)
NET = "lang-compare_default"
CONNS = 64
WARMUP, DURATION = "5s", 15
ORDER_BODY = '{"user_id":42,"items":[{"sku":"SKU-17","qty":2},{"sku":"SKU-4242","qty":1}]}'

SCENARIOS = [  # name, method, url-regex (path), body
    ("health",       "GET",  r"/health", None),
    ("get_user",     "GET",  r"/users/[1-9][0-9]{0,3}", None),
    ("proxy",        "GET",  r"/products/SKU-[1-9][0-9]{0,3}", None),
    ("get_order",    "GET",  r"/orders/[1-9][0-9]{0,4}", None),
    ("list_orders",  "GET",  r"/users/[1-9][0-9]{0,3}/orders\?limit=20", None),
    ("create_order", "POST", r"/orders", ORDER_BODY),
]
FIXED_RATE = [("get_user", 2000), ("create_order", 1000)]  # (scenario, rps) at equal load

MULTIPROC = {"ts", "ts-bun", "ts-sqlite", "py"}  # scale by processes (node/bun cluster, uvicorn --workers)
OUT = os.environ.get("OUT", "results/load.json")
ALL_APPS = ["ts", "ts-bun", "go", "go-fasthttp", "rust", "py", "ts-sqlite", "go-sqlite", "rust-sqlite"]

CONFIGS = [  # name, APP_CPUS, WORKERS(ts only), DB_POOL_SIZE
    ("1cpu", 1, 1, 20),
    ("4cpu", 4, 4, 20),
]


def sh(cmd, check=True, **kw):
    return subprocess.run(cmd, shell=True, check=check, capture_output=True, text=True, **kw).stdout


class Cgroup:
    """Reads a container's cgroup v2 files via a privileged helper container."""

    def __init__(self, container_id):
        self.path = f"/cg/docker/{container_id}"

    def _cat(self, f):
        return sh(f"docker exec cgmon cat {self.path}/{f}")

    def cpu_usec(self):
        for line in self._cat("cpu.stat").splitlines():
            k, v = line.split()
            if k == "usage_usec":
                return int(v)

    def mem_bytes(self):  # same definition as `docker stats`: current - inactive_file
        cur = int(self._cat("memory.current"))
        stat = dict(l.split() for l in self._cat("memory.stat").splitlines())
        return cur - int(stat["inactive_file"])

    def sample_peak_mem(self, seconds):
        script = (f'p={self.path}; end=$(( $(cut -d. -f1 /proc/uptime) + {seconds} )); max=0; '
                  f'while [ $(cut -d. -f1 /proc/uptime) -lt $end ]; do '
                  f'c=$(cat $p/memory.current); i=$(grep "^inactive_file " $p/memory.stat | cut -d" " -f2); '
                  f'v=$((c-i)); [ $v -gt $max ] && max=$v; sleep 0.2; done; echo $max')
        return int(sh(f"docker exec cgmon sh -c '{script}'"))


def oha(host, name, method, path, body, duration, rate=None):
    args = ["docker", "run", "--rm", "--network", NET, "ghcr.io/hatoo/oha", "--no-tui",
            "--output-format", "json", "-c", str(CONNS), "-z", f"{duration}s" if isinstance(duration, int) else duration,
            "-m", method, "--rand-regex-url"]
    if rate:
        args += ["-q", str(rate)]
    if body:
        args += ["-H", "Content-Type: application/json", "-d", body]
    args.append(f"http://{host}:8080{path}")
    out = subprocess.run(args, capture_output=True, text=True).stdout
    return json.loads(out)


def run_scenario(host, cg, scen, rate=None):
    name, method, path, body = scen
    oha(host, name, method, path, body, WARMUP, rate)  # warmup
    peak = {}
    t = threading.Thread(target=lambda: peak.setdefault("v", cg.sample_peak_mem(DURATION - 1)))
    c0, w0 = cg.cpu_usec(), time.time()
    t.start()
    r = oha(host, name, method, path, body, DURATION, rate)
    c1, w1 = cg.cpu_usec(), time.time()
    t.join()
    s = r["summary"]
    codes = r.get("statusCodeDistribution", {})
    total = sum(codes.values())
    ok = sum(v for k, v in codes.items() if k.startswith("2"))
    res = {
        "rps": round(s["requestsPerSec"], 1),
        "p50_ms": round(r["latencyPercentiles"]["p50"] * 1000, 2),
        "p99_ms": round(r["latencyPercentiles"]["p99"] * 1000, 2),
        "success": round(ok / total, 4) if total else 0,
        "errors": r.get("errorDistribution", {}),
        "cpu_cores": round((c1 - c0) / 1e6 / (w1 - w0), 3),
        "peak_mem_mb": round(peak["v"] / 2**20, 1),
    }
    res["rps_per_core"] = round(res["rps"] / res["cpu_cores"], 0) if res["cpu_cores"] else None
    return res


def wait_http(url, timeout=60):
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            urllib.request.urlopen(url, timeout=1).read()
            return time.time() - t0
        except Exception:
            time.sleep(0.01)
    raise RuntimeError(f"{url} did not come up")


def main():
    langs = sys.argv[1:] or ["ts", "go", "rust"]
    sh("docker rm -f cgmon", check=False)
    sh("docker run -d --name cgmon --privileged -v /sys/fs/cgroup:/cg:ro alpine sleep infinity")
    results = json.load(open(OUT)) if os.path.exists(OUT) else {}
    try:
        for lang in langs:
            for cfg_name, cpus, workers, pool in CONFIGS:
                if os.environ.get("ONLY_CFG") and cfg_name != os.environ["ONLY_CFG"]:
                    continue
                svc = f"app-{lang}"
                multiproc = lang in MULTIPROC
                env = dict(os.environ, APP_CPUS=str(cpus), WORKERS=str(workers if multiproc else 1),
                           DB_POOL_SIZE=str(pool // workers if multiproc else pool))
                if lang.startswith("go") and cpus == 1:
                    env["GOMAXPROCS"] = "1"  # Go 1.25+ picks 2 under a 1-CPU quota, which throttles badly
                print(f"\n### {lang} {cfg_name}", flush=True)
                sh("docker compose " + " ".join(f"--profile {a}" for a in ALL_APPS) + " stop "
                   + " ".join(f"app-{a}" for a in ALL_APPS), check=False)
                sh("docker compose up -d --force-recreate --wait postgres catalog", env=env)
                sh(f"docker compose --profile {lang} create --force-recreate {svc}", env=env)
                t0 = time.time()
                sh(f"docker compose --profile {lang} start {svc}", env=env)
                wait_http("http://localhost:18080/health")
                ready = time.time()
                cid = sh(f"docker compose --profile {lang} ps -q {svc}").strip()
                started = sh(f"docker inspect -f '{{{{.State.StartedAt}}}}' {cid}").strip()
                started = datetime.datetime.fromisoformat(started.replace("Z", "+00:00")).timestamp()
                startup = round(ready - started, 3)  # container process start -> first 200 on /health
                cg = Cgroup(cid)
                time.sleep(2)
                entry = {"startup_s": startup, "idle_mem_mb": round(cg.mem_bytes() / 2**20, 1), "scenarios": {}}
                print(f"startup {startup}s idle mem {entry['idle_mem_mb']} MB", flush=True)
                for scen in SCENARIOS:
                    res = run_scenario(svc, cg, scen)
                    entry["scenarios"][scen[0]] = res
                    print(f"  {scen[0]:13} {json.dumps(res)}", flush=True)
                if cfg_name == "1cpu":
                    entry["fixed_rate"] = {}
                    for sname, rate in FIXED_RATE:
                        scen = next(s for s in SCENARIOS if s[0] == sname)
                        res = run_scenario(svc, cg, scen, rate)
                        entry["fixed_rate"][f"{sname}@{rate}"] = res
                        print(f"  {sname}@{rate:<6} {json.dumps(res)}", flush=True)
                entry["mem_after_load_mb"] = round(cg.mem_bytes() / 2**20, 1)
                results.setdefault(lang + os.environ.get("LABEL", ""), {})[cfg_name] = entry
                json.dump(results, open(OUT, "w"), indent=1)
                sh(f"docker compose --profile {lang} stop {svc}", check=False)
    finally:
        sh("docker rm -f cgmon", check=False)


if __name__ == "__main__":
    main()
