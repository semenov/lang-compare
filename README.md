# lang-compare: one backend written in TypeScript, Go and Rust

The same JSON backend (`orders-api`, spec in [SPEC.md](SPEC.md)), implemented three times:
Postgres, a transparent proxy to an upstream service, and business logic that fans out
to the upstream concurrently and writes to the database in a transaction.

| | TypeScript | Go | Rust |
|---|---|---|---|
| Stack (current) | Node 26.10, TypeScript 7.0, Fastify 5.12, pg 8.23, undici 8 | Go 1.27.1, net/http (stdlib), pgx 5.11 | Rust 1.98.1, axum 0.8, sqlx 0.9, reqwest 0.13, tokio 1.53, mimalloc |
| Code | [ts/](ts) | [go/](go) | [rust/](rust) |
| Lines of code (non-blank) | 333 | 463 | 416 |
| Runtime dependencies | 64 npm packages | 16 modules | ~190 crates in Cargo.lock |

All three pass the same black-box test suite, [bench/conformance.py](bench/conformance.py) (35 checks).

There are two sets of measurements:

- **A. Native macOS, latest versions** (the main one): everything runs directly on the MacBook Pro M3 Max
  (10P+4E cores, 36 GB): Postgres 17, the catalog mock, the app, and the load generator [oha](https://github.com/hatoo/oha).
- **B. Docker, previous versions** (Node 24, TS 5.9, sqlx 0.8 with default settings): Docker Desktop, a Linux VM,
  with hard CPU limits via cgroups. It is kept because it shows the effect of container CPU limits.

## 1. Time to write

Wall-clock time from the first file to "Docker image built and conformance passing".
These are my (Claude's) times, not a human's, so read them as relative, not absolute.

| | TS | Go | Rust |
|---|---|---|---|
| Time | 2 min 10 s | **1 min 20 s** | 3 min 24 s |
| Fix iterations | 3 (2 were environment issues: ports 8080/5432 on the host were taken, plus a test bug) | 0, passed on the first run | 1 (host linker unavailable, moved to Docker); the code itself compiled and passed on the first try |
| Performance tuning afterwards | none | `GOMAXPROCS=1` under a 1-CPU cgroup limit | mimalloc instead of musl malloc; `test_before_acquire(false)` in sqlx |
| Upgrading to the latest versions | no code changes (including TS 7) | no code changes | sqlx 0.9 rejects SQL built with `format!`, so the queries were rewritten with `concat!` |

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

## 3. Disk (current versions)

| | TS | Go | Rust |
|---|---|---|---|
| Artifact | 28 KB JS + ~18 MB node_modules (+ the node runtime) | 10.8 MB static binary | 4.1 MB static binary (musl + mimalloc) |
| Docker image (uncompressed) | **190 MB** (node:26-alpine) | 10.8 MB (`scratch`) | **4.1 MB** (`scratch`) |
| Docker image (gzip, ≈ registry size) | 67 MB | 3.8 MB | 1.8 MB |

## 4. Run A — native macOS, latest versions

Without cgroups, parallelism is limited by each runtime's own setting: `WORKERS` (node:cluster), `GOMAXPROCS`, `TOKIO_WORKER_THREADS`.
CPU = CPU time of the process tree (`ps`), memory = summed RSS. Each scenario: 5 s warmup + 15 s of measurement at 64 connections;
Postgres is recreated for each language. Success rate was 100% everywhere.

### 1 thread / 1 process

| Scenario | TS | Go | Rust | Rust with the extra ping ¹ |
|---|---|---|---|---|
| `GET /health` | 83.6k | 97.0k | **142.7k** | 92.4k |
| `GET /users/{id}` (1 SELECT) | 35.8k | **42.0k** | 37.8k | 21.4k |
| `GET /products/{sku}` (proxy) | 31.3k | 36.3k | **50.4k** | 31.3k |
| `GET /orders/{id}` (2 SELECTs) | 22.6k | **27.8k** | 21.9k | 11.9k |
| `GET /users/{id}/orders` (2 SELECTs in parallel) | 16.4k | **22.3k** | 18.2k | 10.5k |
| `POST /orders` (SELECT + 2 upstream calls + transaction) | 8.0k | 9.3k | **9.7k** | 5.6k |
| p99 for `POST /orders` | 8.9 ms | 9.4 ms | **7.4 ms** | 23.3 ms |

¹ The old code with `test_before_acquire = true`, sqlx's default, which pings Postgres every time a connection is taken from the pool.
sqlx 0.8 and 0.9 with this setting give the same result (e.g. `get_order` 11.9k vs 11.8k), so **the library version didn't matter; this setting did**.
The `health` and `proxy` rows in this column don't touch the database; their gap with the main Rust column is most likely run-to-run noise on macOS (see Caveats).

### 4 threads / 4 processes

| Scenario | TS | Go | Rust |
|---|---|---|---|
| `GET /health` | 110.7k | 119.8k | **137.9k** |
| `GET /users/{id}` | 48.7k | **55.9k** | 40.1k |
| `GET /products/{sku}` (proxy) | 47.5k | **60.7k** | 56.6k |
| `GET /orders/{id}` | 29.3k | **36.2k** | 21.1k |
| `GET /users/{id}/orders` | 24.6k | **34.0k** | 19.7k |
| `POST /orders` | 10.1k | **11.2k** | 9.9k |
| CPU actually used | ~2.6–2.8 cores | ~2.5–3.5 | ~2.3–2.6 |

With 4 threads nobody reaches 4 cores: on a single laptop the load generator, Postgres and the catalog take the rest of the CPU,
so the 4-thread native numbers are limited by the machine, not the language. For honest scaling, see run B.

### Memory, startup, CPU at the same load

| | TS | Go | Rust |
|---|---|---|---|
| Startup to the first `200` | 155 ms | 21 ms | **13 ms** |
| RSS idle (1 thread) | 95 MB | 12.6 MB | **8.7 MB** |
| RSS peak under load (1 thread) | 239 MB | 30 MB | **19 MB** |
| RSS peak under load (4 threads/processes) | **983 MB** ² | 34 MB | **23 MB** |
| CPU for `GET /users/{id}` @ 2000 rps | 0.31 cores | 0.24 | **0.23** |
| CPU for `POST /orders` @ 1000 rps | 0.32 cores | 0.33 | **0.28** |

² The sum of RSS across 4 node processes; shared pages are counted several times, so actual usage is somewhat lower.

## 5. Run B — Docker with CPU limits (previous versions)

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
2. **Rust is fastest where the code is its own** (`/health`, the proxy: 1.4–2.3× faster than Go/TS), and has the least memory,
   the fastest startup and the smallest image. On database endpoints it is **close to Go at 1 thread, but falls behind at 4 threads**:
   sqlx's overhead grows with concurrency. Two defaults cost Rust up to 2× before they were found: musl `malloc` and sqlx's
   `test_before_acquire`. In Rust, what you get depends more on your library choices and their defaults than on the language.
3. **TS/Node is surprisingly close on throughput**: 75–90% of Go at 1 process, and ahead of the Rust version that still had
   the extra ping. It pays in memory (8–30× more than Go), an image 17× larger than Go's and 45× larger than Rust's, and the need for `cluster`/multiple pods to use more than one core.
   With TS 7 the build takes a fraction of a second.
4. **Rust's compile time is its main tax, especially in Docker/CI**: a release build with LTO in a musl container takes 1.5 min even after
   a one-line edit (7.6 s natively on the Mac).
5. **At realistic loads (well below saturation) the difference in CPU is within 30%**; most of the time is spent in the network and
   in Postgres/upstream round-trips, not in the language.
6. **The version upgrade barely changed the picture**; the biggest effects came from runtime and library settings
   (allocator, GOMAXPROCS, the pool ping), none of which show up in functional tests, only under load.

## Caveats

- Everything ran on one laptop: the load generator, Postgres and the catalog share the CPU with the app. The relative ratios
  matter more than the absolute numbers.
- macOS can't pin threads to cores, and it moves them between P and E cores, so the native run is noisier than Docker (differences
  up to ~10–15% between runs are possible). Docker gives hard CPU limits but adds VM and virtual-network overhead.
- Postgres was tuned for benchmarking (`fsync=off`, `synchronous_commit=off`), so the database is not the bottleneck.
- Frameworks were picked as "typical" ones (Fastify, stdlib, axum + sqlx) and not tuned to the limit.

## Reproducing

Native (macOS, needs `brew install postgresql@17 oha go`, rustup, and Node 26 via nvm):

```sh
bench/native/infra.sh up                       # Postgres in /tmp/lc-pg on :15432 + catalog on :9000
(cd ts && npm ci && npx tsc -p .); (cd go && go build -o /tmp/lc-go .); (cd rust && cargo build --release)
bench/native/compile.sh                        # -> results/native/compile.txt
python3 bench/native/load.py ts go rust        # -> results/native/load.json (~25 min)
bench/native/infra.sh down
```

Docker:

```sh
docker compose up -d --build postgres catalog && docker compose build app-ts app-go app-rust
python3 bench/conformance.py http://localhost:18080   # with one app running: docker compose --profile go up -d app-go
bench/compile.sh                                       # -> results/compile.txt
python3 bench/load.py ts go rust                       # -> results/load.json (~20 min)
ONLY_CFG=1cpu GOMAXPROCS=1 LABEL=-gomaxprocs1 python3 bench/load.py go
```
