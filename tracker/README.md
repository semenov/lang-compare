# Tracker: a Jira-like SaaS backend, Go vs Rust, written by agents

A realistic multi-tenant backend ([SPEC.md](SPEC.md), ~25 endpoints, PostgreSQL only):
- authentication: Argon2id with fixed parameters, JWT, refresh-token rotation with reuse detection;
- organizations and projects with roles and 404-vs-403 visibility rules;
- issues: gap-free numbering under concurrency, idempotency keys, bulk create, `If-Match`/ETag, a fixed workflow, history;
- comments, filtering and full-text search with exact `total` and cursor pagination;
- webhooks: a transactional outbox, HMAC signatures, retries with backoff, per-issue ordering, survival across restarts;
- graceful shutdown and a Dockerfile.

The prompt to the agents (Opus 5.5) is the task, the language, the acceptance tests, and one sentence:
*"We want a solution that is as fast as possible and uses as little memory as possible."*

How the tests were prepared:
- [run_tests.py](run_tests.py) has **805 checks**. It starts and restarts the service itself, has a webhook receiver that verifies
  signatures, and runs concurrency and durability tests.
- The tests and the reference implementation [ref/](ref) (Python) were written **independently** by two subagents from the same spec.
  They agreed 805/805 with no fixes needed. The suite catches injected bugs (a race in numbering, idempotency turned off,
  `If-Match` not required, the wrong signature → 6–13 failures each). Ambiguities: [TEST_NOTES.md](TEST_NOTES.md), [ref/NOTES.md](ref/NOTES.md).
- The spec started much bigger (~75 endpoints: S3, SMTP, Redis, a query language, sprints…) and was cut down.

## Round 1 (2026-10-05): go-1 and rust-1 ran in parallel

| | Go | Rust |
|---|---|---|
| First code written | 4:13 | 9:18 (≈9 minutes of design before writing anything) |
| First build | 11:27 | 15:49 |
| **805/805** | **11:59** (first test run) | 17:55 (first run 792/805) |
| Done | **13:46** | 19:54 |
| Compile errors | 0 | 2 builds failed |
| Output tokens (thinking) | 95k (28k) | 127k (62k) |
| Cost | **$4.22** | $5.59 |
| Lines of code (non-blank) | 4229 | **3379** (0 × `unsafe`) |
| Dependencies | `net/http` (stdlib) + pgx v5 + x/crypto | tokio, hyper 1 (no framework), tokio-postgres + deadpool, argon2, serde, mimalloc… |
| Clean release build | **6 s** | 30 s |
| Docker image (`scratch`) | 11.1 MB | **2.6 MB** |

Both chose a "framework-free" stack (Go: stdlib `net/http`; Rust: bare hyper with a hand-written router) and similar
designs: one SQL query per request for access control, a counter on the project row for numbering, idempotency through
`INSERT … ON CONFLICT`, and an outbox in the same transaction. Go loads bulk creates with `COPY`, Rust with `unnest`.

## Benchmark ([bench/](bench)): Docker, 2 CPUs + 1 GiB per service

Docker Desktop (14 vCPU, 3.8 GB RAM). The service gets cpuset 0–1 and `--memory 1g`; PostgreSQL 17 gets cpuset 2–9; k6 and the
webhook sink get cpuset 10–13. Everything runs on one Docker network. The data is seeded through each implementation's API with a
deterministic script ([seed.py](bench/seed.py)); one webhook is subscribed to all events.
Traffic mix ([load.js](bench/load.js)):
- 30% listing with filters and sorting, 10% full-text search;
- 20% opening an issue (issue + comments + history);
- 10% create, 10% edit with `If-Match`, 5% transition, 12% comment;
- 3% login (Argon2id).

Saturation is 64 concurrent clients; the fixed rate is 500 iterations/s (~760 req/s).

### 50k issues: the service is the bottleneck — [results/50k](results/50k)

| | Go | Rust |
|---|---|---|
| **Throughput at saturation** | 2 759 req/s | **4 529 req/s (+64%)** |
| Service CPU at saturation | 1.74 of 2 | 1.83 of 2 |
| **CPU per request** | 0.63 ms | **0.40 ms** |
| p50 / p99, all requests | 26 / 82 ms | **13** / 120 ms |
| list p50/p99 | 35 / 87 ms | **16 / 59 ms** |
| search p50/p99 | 34 / 82 ms | **16 / 34 ms** |
| view p50/p99 | 24 / 76 ms | **11 / 20 ms** |
| create/update/comment p99 | 59–88 ms | **27–43 ms** |
| **login p50/p99** | **59 / 117 ms** | 122 / 278 ms ¹ |
| Fixed ~760 req/s: CPU / p50 / p99 | 0.49 cores / 0.61 / 27 ms | **0.34 cores** / 0.59 / **24 ms** |
| Memory: idle → peak under load (anon) | 17 → 119 MB | **9 → 84 MB** |
| Startup until `/readyz` | 0.29 s | 0.27 s |
| Seeding 50k issues (bulk) | 35.7k/s | 38.5k/s |

¹ Login is pure Argon2id (19 MiB, t=2). Rust runs at most one hash per CPU (a semaphore, to cap memory), so under load logins queue up;
Go hashes on every goroutine at once.

### 1M issues: PostgreSQL is the bottleneck — [results/1m](results/1m)

The database (~3.2 GB per implementation) doesn't fit in the Docker VM's memory. Both services hit PostgreSQL's limit and use only
0.2–0.34 of their 2 cores. Everything slows down: list p50 230–290 ms, and even opening an issue takes 110–165 ms.

| | Go | Rust |
|---|---|---|
| Throughput at saturation | 361 req/s | 322 req/s |
| Service CPU | 0.34 cores | **0.20 cores** |
| Peak memory (anon) | 163 MB | **91 MB** |
| Seeding 1M issues | 31.7k/s | 30.5k/s |
| Database size | 3244 MB | 3298 MB |

At this size the comparison is about the agents' SQL schema and index design, not the language. Neither agent got a heavy
listing query with an exact `total` to perform well on a dataset that doesn't fit in memory.

## Without performance requirements: go-plain-1

The same task in Go, with no request for speed: section 10 (Performance) removed from the spec, and the
"as fast as possible and as little memory as possible" line removed from the prompt. Everything else is identical, including the 805 tests.

| | **go-plain-1** (no requirements) | go-1 (asked for speed) | rust-1 (asked for speed) |
|---|---|---|---|
| 805/805 | 11:54 | 11:59 | 17:55 |
| Done | 12:45 | 13:46 | 19:54 |
| Cost | **$3.54** | $4.22 | $5.59 |
| Lines of code | **3021** | 4229 | 3379 |
| Stack | `net/http` + pgx + x/crypto | the same | hyper + tokio-postgres |
| **50k: throughput** | 2 458 req/s | 2 759 req/s (+12%) | **4 529 req/s** (+84%) |
| 50k: CPU per request | 0.75 ms | 0.64 ms | **0.40 ms** |
| 50k: p50 / p99 | 28 / 89 ms | 26 / 80 ms | 13 / 120 ms |
| 50k: memory under load (anon) / idle | 164 / 25 MB | 126 / 14 MB | **84 / 9 MB** |
| 1M: throughput / service CPU | 368 req/s / 0.54 cores | 361 / 0.34 | 322 / **0.20** |
| 1M: view p50/p99, list p50/p99, search p99 | **65/539**, 211/3524, 3261 ms | 165/426, 290/**1871**, **496** ms | 109/301, 234/3494, 644 ms |
| 1M: database size | **2497 MB** | 3244 MB | 3298 MB |
| Image | 13.3 MB | 11.1 MB | **2.6 MB** |

The go-1 rerun at 50k gave exactly the same 2 759 req/s ([results/50k-plain](results/50k-plain)), so the measurements reproduce.

- **One sentence in the prompt buys Go ~12% throughput and ~25% less memory, at +40% code and +$0.7.** The agents' timelines are almost
  identical: go-1 spent only ~1.5 minutes after the tests passed on "optimization" (a quick load test). The difference comes from how the code was
  designed from the start (`COPY` for bulk inserts, smallint enums, UUIDv7 for pagination, extra indexes).
- On the large database the extra indexes cut both ways: go-1 has a 3× better search p99 and half the list p99, while go-plain-1, with a database
  25% smaller and cheaper writes, is faster on simple operations (view p50 65 vs 165 ms).
- **The language gap is far bigger than the prompt's effect:** Go "asked for speed" is still 1.6× behind Rust, and Go without requirements is 1.84× behind.

## Takeaways

- **Agent:** Go finished faster (13:46 vs 19:54) and cheaper ($4.22 vs $5.59), with zero compile errors and 805/805 on the first test run.
  Rust spent 9 minutes designing before writing any code, had 2 failed builds, and its first test run gave 792/805 (fixed in a minute).
- **Service:** when the service is the bottleneck, Rust delivers **+64% throughput** and **1.6× less CPU per request**, with
  2–3× lower p99 on most operations and **30% less memory**. The exception is login (Argon2id): Go is 2× faster there.
- When the database is the bottleneck, the language doesn't matter. Throughput is the same (~340 req/s), and Rust just uses less CPU and memory.
