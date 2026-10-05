# Full-text search (BM25 over Wikipedia): Go vs Rust, written by agents

An on-disk inverted index plus an HTTP search server with AND, phrase and negation queries and exact BM25 ranking
([SPEC.md](SPEC.md)). Implemented in Go and in Rust by headless Claude Code agents (Opus 5.5). The prompt contains only the task,
the language, the acceptance tests and what will be measured. It says nothing about how to build it.

- [SPEC.md](SPEC.md): what the agents got. Tokenization (Unicode L*/N* + simple lowercase), the query grammar and the
  BM25 formula are defined exactly, so every implementation must return the same results (to within 1e-6).
- [testdata/](testdata) + [run_tests.py](run_tests.py): 5,000 simplewiki articles + 14 hand-written edge-case documents
  (scripts, emoji, combining characters, `İ`, ties), 494 queries → 629 checks, including `/doc`, 400/404 errors and concurrent use.
- [ref/](ref): a hidden reference implementation in Python (generates `expected.jsonl`, and passes 629/629 itself). The runner
  catches mutations: `k1=1.3` → 494 failures, full lowercase mapping instead of simple → 355.
- Data (not in git, `data/`): CirrusSearch dumps from 2025-12-22. **simplewiki**: 277,910 articles, 425 MB.
  **enwiki-part**: the first 3.7 GB of the 43 GB enwiki dump (download cut short) → 199,820 large articles, 3.3 GB.
- [bench/run.py](bench/run.py): indexing (`/usr/bin/time -l`), index size, startup until `/health`, then `oha` for 20 s with a fixed mix of
  5,000 queries (30% medium-frequency words, 10% common words, 30% AND of 2–3 words, 20% phrases, 10% with negation), k=10.
  At the end the top-10 results of both engines are compared.

## Round 1 (2026-10-05): go-1 and rust-1 ran in parallel

| | Go | Rust |
|---|---|---|
| First code written | 4:04 | 3:00 |
| First build | 6:18 | 4:59 |
| **629/629** (first test run) | 6:20 | **5:15** |
| Done | **14:02** | 16:32 |
| Compile errors | 0 | 0 |
| Builds | 14 | 18 |
| Output tokens (thinking) | 67k (30k) | 59k (26k) |
| Cost | $2.79 | $2.69 |
| Lines of code (non-blank) | 1971 | **1242** (2 × `unsafe`) |
| Dependencies | stdlib `net/http` + klauspost/compress (zstd) | hyper, tokio, rayon, memmap2, zstd, serde_json, unicode-general-category |
| Clean release build | 6.9 s | 15.0 s |
| Binary | 10.4 MB | 2.0 MB |

The two agents independently designed **almost the same architecture**, a mini-Lucene:
parallel indexing in chunks into temporary runs, then a parallel merge across hash groups of terms (Go: 16, Rust: 64);
postings in blocks of 128 documents with skip tables, and positions in a separate file; document text compressed with zstd in 64 KB blocks;
the server `mmap`s everything. AND matching is driven by the rarest term, which jumps through the skip tables.
The one important difference: **the Go agent added intra-query parallelism**. A heavy query is split by
document range across up to 8 goroutines ([query.go](impl/go-1/query.go)). The Rust server handles each query on one thread.

Both agents tested on corpora they synthesized themselves (copies of the test corpus), since they never saw the real data.

## Results (native macOS, M3 Max; oha on the same machine)

Both engines return **identical results**: top-10 and `total` match on 296/296 sampled queries on both corpora.

| | simplewiki, Go | simplewiki, Rust | enwiki-part, Go | enwiki-part, Rust |
|---|---|---|---|---|
| Indexing, wall | 1.3 s | 1.2 s | 16.9 s | **9.6 s** |
| Indexing, CPU (cores used) | 12.4 s (9.3) | 13.4 s (10.9) | 145.9 s (8.6) | **107.9 s** (11.3) |
| Indexing, peak RSS | 772 MB | **464 MB** | 1177 MB | **881 MB** |
| Index on disk | **357 MB** | 404 MB | **2470 MB** | 2661 MB |
| Startup until ready | 0.02 s / 11 MB | 0.17 s / 40 MB | 0.02 s / 11 MB | 0.16 s / 134 MB |
| **1 connection**: rps | **3 322** | 2 009 | **1 327** | 823 |
| 1 connection: p50 / p99 ms | 0.15 / **2.5** | 0.14 / 8.7 | 0.35 / **8.6** | 0.33 / 17.6 |
| 1 connection: server CPU (cores) | 2.6 | 0.9 | 2.8 | 0.9 |
| **32 connections**: rps | 5 214 | **12 512** | 2 496 | **5 907** |
| 32 connections: p50 / p99 / p99.9 ms | 1.2 / 78 / 155 | 1.4 / **23 / 60** | 2.2 / 142 / 235 | 3.8 / **39 / 137** |
| 32 connections: server CPU (cores) | 7.2 | 10.1 | 9.2 | 12.0 |
| Requests per CPU-second, 32 connections | 721 | **1 245** | 272 | **492** |
| RSS under load | 197 MB | 226 MB | 755 MB | 862 MB |

Takeaways:
- **Under load Rust is 2.4× faster** (12.5k vs 5.2k rps on simplewiki, 5.9k vs 2.5k on enwiki-part), with **a 3–4× better p99**,
  and it gets ~1.7–1.8× more work out of each CPU core.
- **With a single client Go is faster** (1.6× in rps, a 2–3× lower p99) because it parallelizes each query across cores. That's a
  design choice by the agent, not a property of the language (though in Go it's 15 lines with goroutines). Under load the same
  parallelism turns into overhead.
- **Indexing on the large corpus: Rust is 1.8× faster with 25% less memory.** On simplewiki they're equal (~1.2 s, disk-bound and parallel).
- **Agent time and cost are almost the same** (14 vs 16.5 minutes, $2.79 vs $2.69). Rust got to a working version faster (5:15 vs 6:20) and
  with a third less code, thanks to its crates (hyper, rayon, an off-the-shelf Unicode category table).
