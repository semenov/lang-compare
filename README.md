# lang-compare: one backend written in TypeScript, Go, Rust and Python

The same JSON backend (`orders-api`, spec in [SPEC.md](SPEC.md)), implemented several times:
Postgres, a transparent proxy to an upstream service, and business logic that fans out
to the upstream concurrently and writes to the database in a transaction.

| | TypeScript | Go | Rust | Python |
|---|---|---|---|---|
| Stack (current) | Node 26.10 **or Bun 1.4**, TypeScript 7.0, Fastify 5.12, pg 8.23, undici 8 (on Bun: native `fetch`) | Go 1.27.1, pgx 5.11; net/http (stdlib) **or fasthttp 1.74** + fasthttp/router | Rust 1.98.1, axum 0.8, sqlx 0.9, reqwest 0.13, tokio 1.53, mimalloc | Python 3.14, FastAPI 0.141 + pydantic 2.13, uvicorn 0.53 (uvloop, httptools), asyncpg 0.31, aiohttp 3.14 |
| Code | [ts/](ts) | [go/](go), [go-fasthttp/](go-fasthttp) | [rust/](rust) | [py/](py) |
| Lines of code (non-blank) | 333 | 463 / 464 | 416 | **258** |
| Runtime dependencies | 64 npm packages | 16 modules | ~190 crates in Cargo.lock | ~30 packages |

All implementations pass the same black-box test suite, [bench/conformance.py](bench/conformance.py) (35 checks).

There are two sets of measurements:

- **A. Native macOS, latest versions** (the main one): everything runs directly on the MacBook Pro M3 Max
  (10P+4E cores, 36 GB): Postgres 17, the catalog mock, the app, and the load generator [oha](https://github.com/hatoo/oha).
- **C. Docker, all variants, current versions**: Docker Desktop (a Linux VM) with hard CPU limits via cgroups
  (1 or 4 CPU per app). Closest to how services run in production (k8s/containers).
- **B. Docker, previous versions** (Node 24, TS 5.9, sqlx 0.8 with default settings): the first run; it is kept for the findings
  about GOMAXPROCS and musl malloc.

## 1. Time to write

Wall-clock time from the first file to "Docker image built and conformance passing".
These are my (Claude's) times, not a human's, so read them as relative, not absolute.

| | TS | Go | Rust | Python |
|---|---|---|---|---|
| Time | 2 min 10 s | 1 min 20 s | 3 min 24 s | **1 min 14 s** ³ |
| Fix iterations | 3 (2 were environment issues: ports 8080/5432 on the host were taken, plus a test bug) | 0, passed on the first run | 1 (host linker unavailable, moved to Docker); the code itself compiled and passed on the first try | 0 on the first run; then switched from the deprecated `ORJSONResponse` to response models |
| Performance tuning afterwards | none | `GOMAXPROCS=1` under a 1-CPU cgroup limit | mimalloc instead of musl malloc; `test_before_acquire(false)` in sqlx | none |
| Upgrading to the latest versions | no code changes (including TS 7) | no code changes | sqlx 0.9 rejects SQL built with `format!`, so the queries were rewritten with `concat!` | written right away on the latest versions |
| Switching the runtime/framework | Bun: the code ran unchanged, but the `undici` client doesn't work on Bun, so the upstream calls go through `fetch` | fasthttp: **the whole HTTP layer was rewritten** (it's incompatible with net/http: a different handler type, context, router and client) | — | — |

³ Python was written last, when the spec, the tests and three reference implementations already existed, so its time is not directly comparable with the others.

Qualitatively: Go and TS have almost no boilerplate. In Rust most of the time goes into types
(extractor rejections, `sqlx::FromRow`, error enums) and waiting on the compiler.

## 2. Compilation

TS/Go: median of 3 runs. Rust: 1 run. Every run starts from a cold cache.

| | TS 7 (`tsc`, native) | Go | Rust debug | Rust release, thin LTO | Rust release, no LTO |
|---|---|---|---|---|---|
| **macOS natively** — clean build | 0.68 s (TS 5.9: 0.79 s) | 4.7 s | 11.6 s | 24.4 s | 25.7 s |
| **macOS natively** — rebuild after editing one file | 0.31 s | 0.38 s | 0.48 s | 7.6 s | 3.6 s |
| **Docker/Linux** (previous versions) — clean build | 0.95 s | 3.4 s | 16.5 s | **104.7 s** | 62.4 s |
| **Docker/Linux** (previous versions) — rebuild after editing one file | 0.9 s | **0.23 s** | 1.2 s | **84.3 s** | 26.6 s |
| Type check only (macOS) | 0.30 s | 6.3 s (`go vet`) | 14.6 s clean / 0.19 s incremental (`cargo check`) | | |
| Install dependencies (macOS) | 0.5 s (`npm ci`) | 0.7 s | 0.35 s (`cargo fetch`) | | |

- Rust release builds with LTO in Docker/musl are 4–10× slower than on macOS (the Mac linker is much faster).
  If you build your production image in CI, the Docker numbers are the ones you will actually see.
- TypeScript 7 (the Go-native compiler) is only about 15% faster on a project this small; most of the 0.3–0.7 s is process startup.
- `docker build --no-cache` (base images already pulled): TS 3 s, Go 5 s, Rust 116 s.
- Python has no compile step: `pip install` into a fresh venv takes a few seconds, startup is ~0.3 s.

## 3. Disk (current versions)

| | TS | Go | Rust | Python |
|---|---|---|---|---|
| Artifact | 28 KB JS + ~18 MB node_modules (+ the node runtime; the Bun binary is 62 MB) | 10.8 MB static binary | 4.1 MB static binary (musl + mimalloc) | sources + a venv with dependencies |
| Docker image (uncompressed) | 190 MB (node:26-alpine); **97 MB on Bun** (oven/bun:1-alpine) | 10.8 MB (`scratch`) | **4.1 MB** (`scratch`) | **224 MB** (python:3.14-slim) |
| Docker image (gzip, ≈ registry size) | 67 MB (Node) | 3.8 MB | 1.8 MB | 66 MB |

## 4. Run A — native macOS, latest versions

Without cgroups, parallelism is limited by each runtime's own setting: `WORKERS` (node/bun `cluster`, `uvicorn --workers`), `GOMAXPROCS`, `TOKIO_WORKER_THREADS`.
CPU = CPU time of the process tree (`ps`), memory = summed RSS. Each scenario: 5 s warmup + 15 s of measurement at 64 connections;
Postgres is recreated for each variant. Success rate was 100% everywhere.

### 1 thread / 1 process (rps; in parentheses, CPU cores actually used where it differs noticeably from 1)

| Scenario | Python (FastAPI) | TS (Node) | TS (Bun) | Go (net/http) | Go (fasthttp) | Rust |
|---|---|---|---|---|---|---|
| `GET /health` | 26.1k | 82.1k | 92.7k | 97.3k | 134.9k | **142.7k** |
| `GET /users/{id}` (1 SELECT) | 8.7k | 35.7k | 40.5k | 41.8k | **53.5k** | 37.8k |
| `GET /products/{sku}` (proxy) | 7.9k | 31.0k | 53.2k (1.7 cores) | 36.2k | **60.5k** | 50.4k |
| `GET /orders/{id}` (2 SELECTs) | 5.5k | 22.5k | 25.8k | 27.8k | **33.2k** | 21.9k |
| `GET /users/{id}/orders` (2 SELECTs in parallel) | 4.9k | 16.3k | 19.5k | 22.3k | **26.3k** | 18.2k |
| `POST /orders` (SELECT + 2 upstream calls + transaction) | 2.5k | 7.9k | **11.3k** (1.4 cores) | 9.3k | 10.8k | 9.7k |
| p99 for `POST /orders` | **72.7 ms** | 9.3 ms | 8.0 ms | 9.4 ms | 8.1 ms | **7.4 ms** |

Before the fix (sqlx's default `test_before_acquire = true`, an extra ping to Postgres on every connection checkout), Rust was nearly 2× slower on the DB endpoints
(`get_order` 11.9k, `POST /orders` 5.6k). sqlx 0.8 and 0.9 with this setting give the same result, so **the library version didn't matter; this setting did**.

### 4 threads / 4 processes

| Scenario | Python | TS (Node) | TS (Bun) ⁴ | Go (net/http) | Go (fasthttp) ⁵ | Rust |
|---|---|---|---|---|---|---|
| `GET /health` | 35.8k | 107.3k | 94.7k | 118.8k | 104.7k | **137.9k** |
| `GET /users/{id}` | 10.9k | 46.8k | 39.8k | **52.5k** | 38.6k | 40.1k |
| `GET /products/{sku}` | 12.4k | 47.6k | 53.1k | 50.5k | 43.6k | **56.6k** |
| `GET /orders/{id}` | 6.7k | 29.6k | 24.7k | **32.3k** | 22.7k | 21.1k |
| `GET /users/{id}/orders` | 5.9k | 24.7k | 18.9k | **28.5k** | 19.7k | 19.7k |
| `POST /orders` | 3.3k | 10.3k | 10.6k | **11.0k** | 7.8k | 9.9k |

With 4 threads nobody reaches 4 cores: on a single laptop the load generator, Postgres and the catalog take the rest of the CPU,
so the 4-thread native numbers are limited by the machine, not the language. For honest scaling, see run B.

⁴ On macOS, Bun's `cluster` doesn't spread connections across processes (it relies on `SO_REUSEPORT`, which balances only on Linux): 4 processes
use ~1–1.5 cores and are no faster than a single one. On Linux it should behave differently; not measured here.
⁵ fasthttp with `GOMAXPROCS=4` used only ~1.6–2 cores and ended up slower than net/http; the cause wasn't investigated, so it's better to rely on the 1-thread numbers.

### Memory, startup, CPU at the same load

| | Python | TS (Node) | TS (Bun) | Go (net/http) | Go (fasthttp) | Rust |
|---|---|---|---|---|---|---|
| Startup to the first `200` | 290 ms | 157 ms | 133 ms | 20–440 ms ⁶ | 47 ms | **13 ms** |
| RSS idle (1 thread) | 69 MB | 95 MB | 64 MB | 13 MB | 13 MB | **9 MB** |
| RSS peak under load (1 thread) | 74 MB | 238 MB | 159 MB | 30 MB | 27 MB | **19 MB** |
| RSS peak under load (4 threads/processes) ⁷ | 336 MB | **964 MB** | 357 MB | 36 MB | 32 MB | **23 MB** |
| CPU for `GET /users/{id}` @ 2000 rps | **0.42 cores** | 0.26 | 0.30 | 0.22 | **0.20** | 0.23 |
| CPU for `POST /orders` @ 1000 rps | **0.41 cores** | 0.26 | 0.37 | 0.33 | 0.29 | 0.28 |

⁶ Most of the time it's 20–50 ms; the single 440 ms reading is most likely a macOS hiccup.
⁷ The sum of RSS across processes; shared pages are counted several times, so actual usage is somewhat lower.

### Is fasthttp or Bun worth it?

- **fasthttp** makes Go **+16–67% faster at 1 thread on the same single core** (proxy +67%, `/health` +39%, DB reads +18–28%,
  `POST /orders` +16%). The price is rewriting the entire HTTP layer onto an incompatible API, and losing most of the net/http ecosystem
  (middleware, `httptest`, HTTP/2). Most services won't notice the difference because the database is the bottleneck anyway.
- **Bun** runs the same code **+13–72% faster** than Node, but part of that comes from extra threads: under load Bun used up to 1.4–1.7 cores
  where Node used 1. **Per core it's roughly the same as Node** (`POST /orders`: 8.0k vs 7.6k rps/core), yet it uses ~1.5× less memory.
  On macOS, cluster mode didn't help it at all (see ⁴). Compatibility: pg and Fastify worked unchanged, `undici` didn't.

## 5. Run C — Docker with CPU limits, all variants, current versions

Linux VM with 14 vCPU; Postgres 4 CPU, catalog 3 CPU, the app is hard-limited to 1 or 4 CPU (`--cpus`); Node/Bun/Python scale with 4 processes,
Go with `GOMAXPROCS=1` under the 1-CPU limit (see run B). Memory comes from cgroups (as in `docker stats`). Success rate was 100% everywhere.

### 1 CPU

| Scenario | Python | TS (Node) | TS (Bun) | Go (net/http) | Go (fasthttp) | Rust |
|---|---|---|---|---|---|---|
| `GET /health` | 29.4k | 71.8k | 90.9k | 93.7k | **164.2k** | 144.4k |
| `GET /users/{id}` | 9.4k | 27.5k | 31.6k | 36.2k | **43.5k** | 31.2k |
| `GET /products/{sku}` (proxy) | 9.1k | 27.3k | 38.2k | 35.4k | **61.5k** | 48.7k |
| `GET /orders/{id}` | 5.9k | 17.3k | 18.9k | 22.8k | **25.9k** | 17.7k |
| `GET /users/{id}/orders` | 5.1k | 13.3k | 15.4k | 19.0k | **21.9k** | 15.3k |
| `POST /orders` | 2.4k | 5.8k | 6.6k | 7.6k | **9.5k** | 7.9k |
| p99 `POST /orders` | 78 ms | 33 ms | 31 ms | 11 ms | **9 ms** | 10 ms |
| Memory idle / peak | 51 / 55 MB | 40 / 66 MB | 35 / 83 MB | 5 / 27 MB | **3 / 18 MB** | 7 / 22 MB |
| CPU for `POST /orders` @ 1000 rps | 0.47 | 0.33 | 0.40 | 0.26 | **0.24** | 0.26 |

### 4 CPU

| Scenario | Python | TS (Node) | TS (Bun) | Go (net/http) | Go (fasthttp) | Rust |
|---|---|---|---|---|---|---|
| `GET /health` | 95.9k | 203.1k | 200.2k | 195.3k | **319.0k** | 268.7k |
| `GET /users/{id}` | 26.3k | 72.0k | 71.2k | 81.2k | **100.1k** | 57.8k |
| `GET /products/{sku}` (proxy) | 34.8k | 82.6k | 91.1k | 83.1k | **160.5k** | 100.7k |
| `GET /orders/{id}` | 16.7k | 46.4k | 49.3k | 57.1k | **58.3k** | 33.7k |
| `GET /users/{id}/orders` | 15.1k | 37.2k | 37.1k | 48.4k | **52.4k** | 30.6k |
| `POST /orders` | 7.8k | **17.8k** | 17.6k | **21.0k** | **20.8k** | 18.3k |
| p99 `POST /orders` | 17 ms | 9 ms | 19 ms | **6 ms** | 16 ms | **6 ms** |
| Memory idle / peak | 236 / 240 MB | 171 / 258 MB | 147 / 308 MB | 5 / 31 MB | 5 / 28 MB | 9 / 34 MB |

What Docker settled that the Mac run couldn't:
- **fasthttp scales normally on Linux** and is the fastest variant overall (up to 1.9× faster than net/http; at 4 CPU on DB endpoints it is level); on `POST /orders` at 4 CPU it
  ties net/http (~21k), because Postgres and the upstream become the limit, but it gets there on 2.7 cores instead of 3.4.
  The poor 4-thread result on the Mac was a macOS artefact.
- **Bun's cluster mode works on Linux** (all 3.9 cores are used). On equal CPU Bun is +10–40% faster than Node at 1 CPU, but at 4 CPU they are level;
  at a fixed load Bun needs a bit more CPU than Node (0.40 vs 0.33 cores).
- **Python** scales with processes (×2.8–3.8 from 1 → 4 CPU) but stays 2–3× behind Node and 2.2–3.4× behind Go.

## 6. Run B — Docker with CPU limits (previous versions)

Linux VM with 14 vCPU; Postgres 4 CPU, catalog 3 CPU, the app 1 or 4 CPU via `--cpus`. Memory comes from cgroups (as in `docker stats`).
Rust here is still on sqlx 0.8 with the extra ping, but already with mimalloc.

| Scenario | TS 1 CPU | Go 1 CPU (default) | Go 1 CPU `GOMAXPROCS=1` | Rust 1 CPU | TS 4 CPU (cluster) | Go 4 CPU | Rust 4 CPU |
|---|---|---|---|---|---|---|---|
| `GET /health` | 65.8k | 64.5k | 95.8k | **148.8k** | 182k | 196k | **270k** |
| `GET /users/{id}` | 26.5k | 25.7k | **36.2k** | 25.4k | 67.1k | **80.4k** | 44.3k |
| `GET /products/{sku}` | 29.0k | 25.9k | 35.9k | **49.6k** | 81.4k | 81.7k | **102.6k** |
| `GET /orders/{id}` | 16.7k | 16.4k | **22.8k** | 14.1k | 44.2k | **56.8k** | 24.7k |
| `GET /users/{id}/orders` | 13.0k | 14.4k | **18.9k** | 12.4k | 36.5k | **47.1k** | 23.5k |
| `POST /orders` | 6.2k | 6.0k | **7.6k** | 7.0k | 17.6k | **20.8k** | 15.7k |
| p99 `POST /orders` | 17 ms | **65 ms** ⚠️ | 11 ms | 11 ms | 5.6 ms | 5.9 ms | 5.1 ms |
| Memory idle / peak | 104 / 145 MB | 3 / 25 MB | 5 / 26 MB | 7 / 18 MB | 171 / 321 MB | 5 / 33 MB | 7 / 30 MB |

The first Rust build without mimalloc (musl's `malloc`) was **slower at 4 CPUs than at 1** (`/health` 142k → 95k):
`results/load-rust-muslmalloc.json`.

## Conclusions

1. **Go is the best overall balance**: fastest to write, fast to rebuild (0.2–0.4 s), leads on almost every DB scenario,
   ~30 MB of memory, 11 MB image. The catch: under a 1-CPU cgroup limit, Go 1.25+ still sets `GOMAXPROCS=2`,
   which caused p99 spikes of 50–65 ms and cost 30–40% of throughput; `GOMAXPROCS=1` fixes it.
2. **Rust is fastest where the code is its own** (`/health`, the proxy: 1.4–2.3× faster than Go on net/http and Node; roughly on par with Go on fasthttp), and has the least memory,
   the fastest startup and the smallest image. On database endpoints it is **close to Go at 1 thread, but falls behind at 4 threads**:
   sqlx's overhead grows with concurrency. Two defaults cost Rust up to 2× before they were found: musl `malloc` and sqlx's
   `test_before_acquire`. In Rust, what you get depends more on your library choices and their defaults than on the language.
3. **TS/Node is surprisingly close on throughput**: 75–90% of Go at 1 process, and ahead of the Rust version that still had
   the extra ping. It pays in memory (8–30× more than Go), an image 17× larger than Go's and 45× larger than Rust's, and the need for `cluster`/multiple pods to use more than one core.
   With TS 7 the build takes a fraction of a second.
4. **Rust's compile time is its main tax, especially in Docker/CI**: a release build with LTO in a musl container takes 1.5 min even after
   a one-line edit (7.6 s natively on the Mac).
5. **Python (FastAPI) is 3–4× slower than Node/Go/Rust** on the same logic, with a p99 of ~70 ms under saturation. But at a moderate load
   (1000–2000 rps) it only needs ~1.5× more CPU than the others. Its strength is the least code (258 lines) and the fastest development.

6. **At realistic loads (well below saturation) the difference in CPU is within 30%**; most of the time is spent in the network and
   in Postgres/upstream round-trips, not in the language.
7. **Alternative runtimes/frameworks:** fasthttp makes Go the fastest variant overall (up to 1.9× faster than net/http, level on DB endpoints at 4 CPU; the proxy and `/health` gain the most);
   the price is rewriting the HTTP layer and losing net/http compatibility. Bun beats Node by 10–40% at 1 CPU, is level at 4 CPU,
   and has half the image size; porting was almost free (only undici → fetch).
8. **The version upgrade barely changed the picture**; the biggest effects came from runtime and library settings
   (allocator, GOMAXPROCS, the pool ping), none of which show up in functional tests, only under load.

## Caveats

- Everything ran on one laptop: the load generator, Postgres and the catalog share the CPU with the app. The relative ratios
  matter more than the absolute numbers.
- macOS can't pin threads to cores, and it moves them between P and E cores, so the native run is noisier than Docker (differences
  up to ~10–15% between runs are possible). Docker gives hard CPU limits but adds VM and virtual-network overhead.
- Postgres was tuned for benchmarking (`fsync=off`, `synchronous_commit=off`), so the database is not the bottleneck.
- Frameworks were picked as "typical" ones (Fastify, stdlib, axum + sqlx, FastAPI) and not tuned to the limit.

## Reproducing

Native (macOS, needs `brew install postgresql@17 oha go`, rustup, and Node 26 via nvm):

```sh
bench/native/infra.sh up                       # Postgres in /tmp/lc-pg on :15432 + catalog on :9000
(cd ts && npm ci && npx tsc -p .); (cd go && go build -o /tmp/lc-go .); (cd rust && cargo build --release)
(cd go-fasthttp && go build -o /tmp/lc-go-fasthttp .); (cd py && python3 -m venv .venv && .venv/bin/pip install -r requirements.txt)
bench/native/compile.sh                        # -> results/native/compile.txt
python3 bench/native/load.py ts ts-bun go go-fasthttp rust py   # -> results/native/load.json (~40 min)
bench/native/infra.sh down
```

Docker:

```sh
docker compose up -d --build postgres catalog && docker compose build app-ts app-ts-bun app-go app-go-fasthttp app-rust app-py
python3 bench/conformance.py http://localhost:18080   # with one app running: docker compose --profile go up -d app-go
bench/compile.sh                                       # -> results/compile.txt
python3 bench/load.py ts go rust                       # -> results/load.json (~20 min)
ONLY_CFG=1cpu GOMAXPROCS=1 LABEL=-gomaxprocs1 python3 bench/load.py go
OUT=results/docker/load.json python3 bench/load.py ts ts-bun go go-fasthttp rust py   # run C (~50 min)
```
