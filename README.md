# lang-compare: one backend written in TypeScript, Go and Rust

The same JSON backend (`orders-api`, spec in [SPEC.md](SPEC.md)), implemented three times:
Postgres, a transparent proxy to an upstream service, and business logic that fans out
to the upstream concurrently and writes to the database in a transaction.

| | TypeScript | Go | Rust |
|---|---|---|---|
| Stack | Node 24, Fastify 5, pg, undici | Go 1.27, net/http (stdlib), pgx v5 | Rust 1.98, axum 0.8, sqlx 0.8, reqwest, tokio |
| Code | [ts/](ts) | [go/](go) | [rust/](rust) |
| Lines of code (non-blank) | 333 | 463 | 405 |
| Runtime dependencies | 64 npm packages | 16 modules | ~200 crates in Cargo.lock |

All three pass the same black-box test suite, [bench/conformance.py](bench/conformance.py) (35 checks).

Machine: MacBook Pro M3 Max, Docker Desktop (Linux VM, 14 vCPU / 4 GB). Everything ran in Docker:
Postgres (4 CPU), the catalog mock in Go (3 CPU), the app (1 or 4 CPU via `--cpus`), and the load generator
[oha](https://github.com/hatoo/oha) on the same Docker network, with 64 connections.

## 1. Time to write

Wall-clock time from the first file to "Docker image built and conformance passing".
These are my (Claude's) times, not a human's, so read them as relative, not absolute.

| | TS | Go | Rust |
|---|---|---|---|
| Time | 2 min 10 s | **1 min 20 s** | 3 min 24 s |
| Fix iterations | 3 (2 were environment issues: ports 8080/5432 on the host were taken, plus a test bug) | 0, passed on the first run | 1 (host linker unavailable, moved to Docker); the code itself compiled and passed on the first try |
| Performance tuning afterwards | none | `GOMAXPROCS=1` (env var) | mimalloc instead of the musl allocator (3 lines); **without it Rust scaled worse than TS**, see below |

Qualitatively: Go and TS have almost no boilerplate. In Rust most of the time goes into types
(extractor rejections, `sqlx::FromRow`, error enums) and waiting on the compiler.

## 2. Compilation (in Docker, Linux arm64, 14 CPU; TS/Go median of 3 runs)

| | TS (`tsc`) | Go | Rust debug | Rust release (thin LTO) | Rust release (no LTO) |
|---|---|---|---|---|---|
| Install dependencies | 0.8 s (`npm ci`) | 0.8 s | 1.6 s (`cargo fetch`) | — | — |
| Clean build | 0.95 s | 3.4 s | 16.5 s | **104.7 s** | 62.4 s |
| Rebuild after editing one file | 0.9 s | **0.23 s** | 1.2 s | **84.3 s** | 26.6 s |
| Type check only | 0.9 s (`tsc --noEmit`) | 3.4 s (`go vet`) | 13.3 s clean / 0.15 s incremental (`cargo check`) | | |

Full Docker image build (`docker build --no-cache`, base images already pulled): TS 3 s, Go 5 s, Rust 116 s.

## 3. Disk

| | TS | Go | Rust |
|---|---|---|---|
| Artifact | 28 KB JS + 17.6 MB node_modules (+ the node runtime) | 10.8 MB static binary | 4.1 MB static binary (musl) |
| Docker image (uncompressed) | **176 MB** (node:24-alpine) | 10.8 MB (`scratch`) | **4.1 MB** (`scratch`) |
| Docker image (gzip, ≈ registry size) | 62 MB | 3.8 MB | 1.8 MB |

## 4. Memory (container cgroup, `docker stats` method)

| | TS | Go | Rust |
|---|---|---|---|
| Idle, 1 CPU | 104 MB | 3–5 MB | 6.7 MB (0.7 MB with musl malloc) |
| Peak under load, 1 CPU | 145 MB | 25 MB | 18 MB |
| Idle, 4 CPU (TS = 4 cluster workers) | 171 MB | 5 MB | 6.8 MB |
| Peak under load, 4 CPU | **321 MB** | 33 MB | 30 MB |

## 5. Throughput (rps) and latency

Each scenario: 5 s warmup, then 15 s of measurement at 64 connections. The app saturates its CPU in every test (0.95–0.97 cores out of 1, ~3.5–3.8 out of 4).
Success rate was 100% everywhere.

### 1 CPU

| Scenario | TS | Go (default) | Go `GOMAXPROCS=1` | Rust |
|---|---|---|---|---|
| `GET /health` | 65.8k | 64.5k | 95.8k | **148.8k** |
| `GET /users/{id}` (1 SELECT) | 26.5k | 25.7k | **36.2k** | 25.4k |
| `GET /products/{sku}` (proxy) | 29.0k | 25.9k | 35.9k | **49.6k** |
| `GET /orders/{id}` (2 SELECTs) | 16.7k | 16.4k | **22.8k** | 14.1k |
| `GET /users/{id}/orders` (2 SELECTs in parallel) | 13.0k | 14.4k | **18.9k** | 12.4k |
| `POST /orders` (SELECT + 2 upstream calls + transaction) | 6.2k | 6.0k | **7.6k** | 7.0k |
| p99 for `POST /orders` | 17 ms | **65 ms** ⚠️ | 11 ms | 11 ms |

### 4 CPU (TS as `node:cluster` with 4 workers)

| Scenario | TS | Go | Rust |
|---|---|---|---|
| `GET /health` | 182k | 196k | **270k** |
| `GET /users/{id}` | 67.1k | **80.4k** | 44.3k |
| `GET /products/{sku}` (proxy) | 81.4k | 81.7k | **102.6k** |
| `GET /orders/{id}` | 44.2k | **56.8k** | 24.7k |
| `GET /users/{id}/orders` | 36.5k | **47.1k** | 23.5k |
| `POST /orders` | 17.6k | **20.8k** | 15.7k |
| p99 for `POST /orders` | 5.6 ms | 5.9 ms | 5.1 ms |

## 6. CPU at the same load (1 CPU limit, fixed rate)

| Scenario | TS | Go (default) | Go `GOMAXPROCS=1` | Rust |
|---|---|---|---|---|
| `GET /users/{id}` @ 2000 rps | 0.33 cores | 0.33 | **0.25** | 0.29 |
| `POST /orders` @ 1000 rps | 0.31 cores | 0.33 | 0.25 | **0.25** |

At realistic loads (well below saturation) all three differ by at most about 30% in CPU. Most of the time goes to
the network stack and to Postgres/upstream round-trips, not to the language.

## 7. Other

- Startup to the first `200` on `/health`: about 0.3 s for all three (in this setup, most of that is Docker itself).

## Conclusions

1. **Rust wins where the code is its own**: `/health` and the proxy (hyper + reqwest) are 1.5–2.3× faster than Go/TS,
   and it has the smallest image (4 MB) and memory footprint.
   **But on database paths Rust with sqlx is the slowest of the three**, especially at 4 CPUs (2× behind Go).
   That comes from sqlx (its pool and protocol layer), not from the language; tokio-postgres + deadpool would most likely
   close the gap. The practical lesson is that in Rust your choice of libraries matters more than the language does.
2. **Go is the best overall balance**: fastest to write, fastest to rebuild (0.2 s), best on every DB scenario,
   ~30 MB of memory, 11 MB image. The one catch: on a 1-CPU quota, Go 1.25+ still sets `GOMAXPROCS` to 2, which caused
   p99 spikes of 50–65 ms from CFS throttling and cost 30–40% of throughput. `GOMAXPROCS=1` fixes it.
3. **TS/Node is surprisingly close on throughput**: at 1 CPU it matches default Go, and in cluster mode it is 15–25% behind Go.
   It pays in memory (10–20× more: 100–320 MB), a 40× larger image, and the need for `cluster`/multiple pods to use more than one core.
   The build is the fastest of all.
4. **Rust's compile time is its main tax**: a release build with LTO takes 1.5 min even after a one-line edit, and Docker builds
   take ~2 min. Debug with incremental compilation is fine (1.2 s).
5. **Both "production" gotchas were runtime issues, not code issues**: Rust + musl without mimalloc scaled *worse* at 4 CPUs than at 1
   (`/health` 142k → 95k, DB endpoints 30–40% lower); Go needs `GOMAXPROCS=1` on a 1-CPU quota. Neither shows up
   in functional tests, only under load.

## Caveats

- Everything ran on one laptop in a Docker Desktop VM. The load generator shares CPU with the apps, so the absolute numbers
  (especially `/health` above 150k rps) depend on the load generator too. The relative ratios are what count.
- Postgres was tuned for benchmarking (`fsync=off`, tmpfs), so the database is not the bottleneck.
- Frameworks were picked as "typical" ones (Fastify, stdlib, axum + sqlx) and not tuned to the limit.
- The original Rust results (musl malloc) are in `results/load-rust-muslmalloc.json`.

## Reproducing

```sh
docker compose up -d --build postgres catalog
docker compose build app-ts app-go app-rust
python3 bench/conformance.py http://localhost:18080   # with one app running: docker compose --profile go up -d app-go
bench/compile.sh                                       # -> results/compile.txt
python3 bench/load.py ts go rust                       # -> results/load.json (~20 min)
ONLY_CFG=1cpu GOMAXPROCS=1 LABEL=-gomaxprocs1 python3 bench/load.py go
```
