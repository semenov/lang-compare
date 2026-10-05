# mini-redis: Go vs Rust, written by agents

The same in-memory Redis-compatible server ([SPEC.md](SPEC.md)), implemented in Go and in Rust
by headless Claude Code agents. It compares runtime speed and memory, compile time, and how long
the agent takes to build it.

- [SPEC.md](SPEC.md): a subset of Redis 8 (strings, lists, hashes, TTL with active expiry, pub/sub,
  allkeys-lru under `MAXMEMORY`, pipelining) that **must use multiple cores**.
- [bench/conformance.py](bench/conformance.py): 35 black-box tests. They pass against real `redis-server` 8.10
  (32/32; the 3 tests of our logical memory accounting are skipped there).
- [agents/run_agent.py](agents/run_agent.py) `go|rust N`: runs one agent (`claude -p`, Opus 5.5) in an isolated
  workspace in `/tmp`, so it can't see the other implementations. The prompt is the same for both languages apart from the language.
  Every stream-json event is logged with a timestamp to `runs/<lang>-N/events.jsonl`.
- [agents/status.py](agents/status.py): live dashboard (`-w`), or `--log go-1 40` for a run's last actions.
- [impl/](impl): the code the agents produced, copied unchanged (no build artifacts).

## Round 1 (2026-10-05): go-1 and rust-1 ran in parallel

| | Go | Rust |
|---|---|---|
| First code written | 6:49 | 6:26 |
| First build | 9:43 | 9:36 |
| **All 35 tests pass** (first test run) | **9:55** | **9:58** |
| Done (including self-tuning) | 14:35 | 12:47 |
| Compile errors | 0 | 0 |
| Builds | 6 | 5 |
| Output tokens (thinking) | 75.8k (41.6k) | 78.0k (40.9k) |
| Cost | $2.88 | $2.99 |
| Lines of code (non-blank) | 2283 | 2155 |
| Dependencies | stdlib only | tokio, parking_lot, ahash, mimalloc, bytes, itoa, libc (39 crates in lock) |
| Clean release build (empty cache) | 2.1 s | 10.0 s (fat LTO, codegen-units=1) |
| Binary | 3.6 MB | 0.9 MB |

The first ~6.5 minutes of both runs were design (reading the spec and tests, then ~20k–30k thinking tokens) before any code was written.
Both wrote the whole server in one pass, without a single compile error, and passed the full suite on the first test run.
After that each spent a few minutes optimizing on its own.

Designs (from the agents' own summaries):

- **Go**: a goroutine per connection, 256 shards with a mutex each, and an open-addressing table of `uint64`
  slots (pointer + hash tag). **String records live in memory allocated directly from the OS, outside the Go heap**, in a hand-written
  slab allocator with ~60 size classes, so the GC has almost nothing to scan. Lists are ring buffers; hashes are a flat slice up to 16 fields, then a map.
  Expiry sweeper: 1/8 of each table every 100 ms. LRU: sample 16 keys in a random shard.
  This is far from idiomatic Go: the agent sidestepped the GC by hand.
- **Rust**: tokio, 1024 shards behind `parking_lot` locks, 16-byte table entries (pointer + hash + LRU stamp),
  with key, expiry and value in a single allocation with a 3-byte header. mimalloc. Per-shard min-heap of deadlines for expiry.
  LRU: sample 16 keys per shard.

Independent check after copying: clean rebuild + 5 conformance runs each → 5/5 green for both.
(The single flaky failure, in `expire_ttl_persist`, was a bug in the test: `PEXPIRE 5500` → `TTL` gives 5 if 1 ms passes.
Fixed with `PEXPIRE 5700`.)

Benchmarks (throughput, p99, RSS per 1M keys, scaling across THREADS): not run yet.
