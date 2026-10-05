# Lox interpreter: Go vs Rust, written by agents

A Lox interpreter (the language from *Crafting Interpreters*), implemented in Go and in Rust by headless Claude Code agents
(Opus 5.5). The prompt contains only the task, the language, the acceptance tests, and the fact that speed and peak RSS
will be measured. It says nothing about how to build the interpreter.

- [SPEC.md](SPEC.md): what the agents got.
- [tests/](tests): the book's official test suite (246 tests, MIT). [run_tests.py](run_tests.py) is a Python port of the book's
  `test.dart`, validated with **246/246 on the reference clox**.
- [bench/programs/](bench/programs): the book's benchmarks. **The agents didn't see them**. `string_equality.lox` is excluded
  (it exceeds the 256-constant limit even in clox). [bench/run.py](bench/run.py) reports the median of 3 runs, wall time
  and peak RSS (`/usr/bin/time -l`), and checks the output against clox.
- [agents/](agents): runner and dashboard (same as for mini-redis). [runs/](runs): full agent logs. [impl/](impl): the code, unchanged.

## Round 1 (2026-10-05): go-1 and rust-1 ran in parallel

| | Go | Rust |
|---|---|---|
| First code written | 3:00 | 2:48 |
| First build | 6:01 | 6:20 |
| **All 246 tests pass** | **6:02** (first build) | **6:26** (after 1 build error) |
| Done (including self-optimization) | **13:17** | 31:22 |
| Compile errors | 0 | 1 (7× the `dangerous_implicit_autorefs` lint, silenced with `#![allow]`) |
| Builds | 12 | 29 |
| Output tokens (thinking) | 88k (37k) | 164k (68k) |
| Cost | **$3.49** | $7.48 |
| Lines of code (non-blank) | 2154 | 3055 (66 × `unsafe`) |
| Dependencies | none | mimalloc |
| Clean release build | 1.8 s | 4.9 s (fat LTO) |
| Binary | 2.6 MB | 0.6 MB |

Both agents independently wrote a **clox-style bytecode VM**: a single-pass Pratt compiler, globals resolved to slots,
hidden classes with inline caches, and fused instructions. The differences:

- **Go**: 16-byte values (float + pointer), **Go's own GC**, and raw pointers in the dispatch loop. After ~7 minutes of pprof
  profiling and GOGC tuning it decided it was done.
- **Rust**: 8-byte NaN-boxed values, **its own mark-sweep GC and its own size-class pool allocator** (`memory.rs`), everything on
  `unsafe` raw pointers, and dead-code elimination for expression statements with no side effects. It spent another ~25 minutes on
  optimization: it read the disassembly of the VM loop, tried to get LLVM to produce threaded dispatch, and compared allocators.
  Along the way it broke one test and fixed it a minute later.

## Speed and memory (native macOS, M3 Max) — [results/bench-round1.log](results/bench-round1.log)

| program | clox (C, from the book) | Go | Rust |
|---|---|---|---|
| binary_trees | 1.07 s / 23.6 MB | 0.78 s / 24.9 MB | **0.49 s / 11.5 MB** |
| fib | 0.80 s / 1.8 MB | 0.62 s / 4.6 MB | **0.43 s** / 2.6 MB |
| instantiation | 0.43 s / 3.0 MB | 0.42 s / 11.2 MB | **0.17 s** / 4.6 MB |
| invocation | 0.16 s | 0.19 s | **0.12 s** |
| method_call | 0.12 s | 0.09 s | **0.06 s** |
| properties | 0.24 s | 0.20 s | **0.12 s** |
| trees | 1.47 s / 84.1 MB | 1.30 s / 78.6 MB | **0.87 s / 43.7 MB** |
| zoo | 0.17 s | 0.15 s | **0.11 s** |
| zoo_batch (batches in 10 s, more is better) | 8 795 | 9 734 | **13 216** |
| equality ¹ | 2.52 s | 3.41 s | 0.14 s ¹ |
| **Speed vs clox (geometric mean, 9 programs)** | 1.00× | 1.15× | **1.82×** |

¹ Not comparable: the Rust compiler drops expression statements with no side effects (`1 == 1;`), so both loops of
this benchmark become empty. It's a legitimate optimization, but it means it's measuring an empty loop. Excluded from the mean.

- **Rust is 1.58× faster than Go** (geometric mean) and uses **about half the memory** on the allocation-heavy programs
  (binary_trees 11.5 vs 24.9 MB, trees 43.7 vs 78.6 MB): 8-byte NaN-boxed values plus its own GC and allocator,
  versus 16-byte values and Go's GC.
- Both beat the book's reference C interpreter (Go only slightly).
- **The price is agent time and money: Rust took 2.4× longer and cost 2.1× more** (31 vs 13 minutes, $7.48 vs $3.49). Time to a working
  interpreter was about the same (6:02 vs 6:26). The difference is entirely how deeply each agent optimized afterwards.
