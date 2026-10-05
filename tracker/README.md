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

## TypeScript on Node.js without performance requirements: ts-plain-1

The same conditions as go-plain-1: no Performance section, no line about speed in the prompt. Node.js 26.10 + TypeScript 7.
The agent picked plain `node:http` with no framework (its own router), the `pg` driver, and `@node-rs/argon2` (Argon2 as a native addon written in Rust),
plus a `./tracker` launcher of the form `exec node dist/main.js`. The log was checked by hand: in 23 actions it never accessed anything outside its workspace.

| | **ts-plain-1** | go-plain-1 | go-1 | rust-1 |
|---|---|---|---|---|
| First code written | 2:18 | 5:41 | 4:13 | 9:18 |
| **805/805** | **7:39** (first run) | 11:54 | 11:59 | 17:55 |
| Done | **12:07** | 12:45 | 13:46 | 19:54 |
| Output tokens (thinking) | **59k** (16k) | 83k (34k) | 95k (28k) | 127k (62k) |
| Cost | **$2.87** | $3.54 | $4.22 | $5.59 |
| Lines of code | **2254** | 3021 | 4229 | 3379 |
| Dependencies | pg, @node-rs/argon2 | pgx, x/crypto | pgx, x/crypto | tokio, hyper, tokio-postgres, … |
| Clean build | 1.2 s (`npm ci` + `tsc`) | ~6 s | 6 s | 30 s |
| Docker image | 273 MB (node:26-slim) | 13.3 MB | 11.1 MB | **2.6 MB** |
| **50k: throughput** | 1 516 req/s | 2 321–2 458 | 2 759 | **4 529** |
| 50k: CPU used (of 2) | **1.36** ¹ | 1.84 | 1.77 | 1.83 |
| 50k: req/s per CPU core | 1 115 | ~1 300 | 1 560 | **2 475** |
| 50k: p50 / p99 | 52 / 110 ms | 28 / 89–168 ms | 26 / 80 ms | 13 / 120 ms |
| 50k, ~760 req/s: CPU / p50 | 0.64 / 1.7 ms | 0.62 / 0.8 ms | 0.52 / 0.6 ms | **0.34** / 0.6 ms |
| 50k: memory under load / idle | 167 / 27 MB | 160 / 28 MB | 126 / 14 MB | **84 / 9 MB** |
| **1M: throughput** | **44 req/s** ² | 368 | 361 | 322 |
| 1M: view p50, list p50 | 1.8 s, 3.9 s | 65 ms, 211 ms | 165 ms, 290 ms | 109 ms, 234 ms |
| 1M: database size | 2 070 MB | 2 497 MB | 3 244 MB | 3 298 MB |

¹ Node runs JavaScript on a single thread: of the 2 CPUs, one is busy plus a bit of background threads (Argon2, I/O). The agent didn't use
`cluster` or worker threads (it wasn't asked to). Per CPU core the gap to go-plain is only ~15%.

² Not because of Node (the service uses 0.1 cores and waits on the database), but because of the schema: the agent made no index for listing
issues within a project with sorting (only `org_id`, plus GIN on labels and words; go-plain-1 also has `(project_id, created_at)` and
`assignee_id`). At 1M issues every listing reads too many rows, PostgreSQL is saturated, and even a view by key waits 1.8 s.
~1% of requests timed out. At 50k this is invisible: everything fits in memory.

- **The TypeScript agent was the fastest and cheapest of all**: it went from start to 805/805 in 7.7 minutes, at half the cost of Rust, with the least code.
- **At moderate load the services are equivalent.** At saturation TS loses because of single-threaded Node (−35% against go-plain on 2 CPUs).
  At 1M it collapses because of one missing index, which is exactly what you'd catch by asking for performance or doing a review.

The seeding script was moved to keep-alive connections for this run (otherwise macOS ran out of ephemeral ports in TIME_WAIT
against Node, which doesn't close connections itself). go-plain-1 was re-run with the new script at 50k: 2 321 req/s, vs. 2 458 the first time,
so the noise between runs is ~±5% ([results/50k-ts](results/50k-ts)).

## Code quality review (2026-10-05)

Not spec compliance (the tests cover that) but common sense: is this code readable and maintainable, does it follow the
language's usual norms, and would a senior engineer accept it into a production codebase? One reviewer agent (Opus 5.5) per
implementation; the most serious claims were then checked by hand in the code.

| | Readability | Maintainability | Idiomaticity | **Overall** |
|---|---|---|---|---|
| go-plain-1 | 6 | 5 | 6 | **6** |
| ts-plain-1 | 6 | 5 | 6 | **5.5** |
| go-1 | 5 | 4 | 5 | **5** |
| rust-1 | 5 | 4 | 6 | **5** |

The verdict is the same for all four: a competent prototype, "request changes" as a production PR.

**Common strengths:** correct transactions and row locks (`FOR UPDATE`, a counter on the project row, a lock on the org for
last-owner checks); a transactional webhook outbox; well-thought-out idempotency and refresh-token rotation; parameterized SQL
with batch inserts and keyset pagination; a clean problem+json error model; graceful shutdown; few dependencies.

**Common problems:**
- No layering: every handler parses the body, checks access, runs SQL and builds JSON in one function. Business rules can't
  be tested without HTTP and a live PostgreSQL.
- **Zero tests**, not even for pure functions (issue-key parsing, effective role, cursors).
- Roles, statuses and visibility are magic numbers or strings, in both code and SQL (`role >= 2`, `status == 1`).
- The "fetch limit+1, truncate, build cursor" pagination is copy-pasted ~8 times in each implementation.
- The schema is one `CREATE … IF NOT EXISTS` blob, with no versioned migrations.
- Errors carry no context, some are silently swallowed, and there are no structured or request logs and no request IDs.
- Global mutable state; config read from `os.Getenv`/`process.env` scattered through `main`.
- Hand-written infrastructure: all four write their own JWT; the Go versions also do UUIDs, and Rust also does URL and date parsing.
- Cryptic short names (`R`, `F`, `q1`, `verrs`, `est/ust/sst`, `c`, `cl`, `o`).

**Per implementation:**
- **go-plain-1 (6)** is the most readable: handlers return `error` through one adapter, and the `applyInput → diffIssues →
  saveIssueUpdate` pipeline is reused well. Minuses:
  - Responses are built as `map[string]any`.
  - Errors are never wrapped (`%w`).
  - The request body limit is 128 MB.
  - A latent hazard: `p.can("develper")` with a typo returns true for everyone, because an unknown role gets rank 0
    (`projects.go:33`).
- **ts-plain-1 (5.5)** is the most compact and easy to scan. Minuses:
  - `any` for every database row.
  - No `noUncheckedIndexedAccess`.
  - The webhook worker's state machine is hard to follow.
  - `readBody` has no size limit at all (`main.ts:82`).
  - Every authenticated request runs `WHERE id::text = $1` (`auth.ts:52`), which can't use the primary-key index.
  - Small slips: a dead ternary `n === 0 ? "too_short" : "too_short"`, `charLen` defined twice, and a comment claiming "a single
    query" where the code loops one query per item.
- **go-1 (5)** has the strongest "benchmark entry" smell. Minuses:
  - All JSON output is assembled by hand with byte appends (`appendIssue`, `appendHook`, …), plus a custom UUIDv7 generator
    and JWT. This saves microseconds on a service that waits on the database, and adding a field means changing 5–6 places
    that must stay in sync.
  - `hListIssues` is a 300-line function.
  - Access helpers write the HTTP response themselves and return `(…, bool)`.
- **rust-1 (5)** has the best router: slice patterns (`(GET, ["orgs", slug, "issues", ik])`) that are clear and exhaustive.
  Minuses:
  - Database rows are read by column position (`o + 0 .. o + 16`), so reordering one column breaks things at runtime.
  - Webhook payloads are stored with the leading `{` cut off and the ID spliced back in at send time.
  - Labels are joined into a string and split again in SQL.
  - The search list function is about 260 lines.
  - Validation uses "validate into an `Option`, then `unwrap()`".
  - **A real bug:** `https` webhook URLs pass validation, but the client is a bare `HttpConnector` with no TLS dependency, so
    every https webhook will fail.

**The quality ranking is the reverse of the performance ranking.** The "plain" implementations, written with no speed
requirement, read better. The line "as fast as possible" pushed the agents toward hand-written JSON (go-1), packed payload
formats and positional row access (rust-1), all of which cost maintainability. Still, all four lack the same basics (layers,
types, tests, migrations), which says more about the setup (agents racing to 805/805) than about the languages.

## Takeaways

- **Agent:** Go finished faster (13:46 vs 19:54) and cheaper ($4.22 vs $5.59), with zero compile errors and 805/805 on the first test run.
  Rust spent 9 minutes designing before writing any code, had 2 failed builds, and its first test run gave 792/805 (fixed in a minute).
- **Service:** when the service is the bottleneck, Rust delivers **+64% throughput** and **1.6× less CPU per request**, with
  2–3× lower p99 on most operations and **30% less memory**. The exception is login (Argon2id): Go is 2× faster there.
- When the database is the bottleneck, the language doesn't matter. Throughput is the same (~340 req/s), and Rust just uses less CPU and memory.
- **Code quality:** all four are competent prototypes scoring 5–6/10 for maintainability. They have no layering and no tests, and
  roles and statuses are magic numbers. Asking for speed made the code worse to maintain: go-1 and rust-1 rank below the "plain" versions.
