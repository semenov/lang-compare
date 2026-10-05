#!/usr/bin/env python3
"""Benchmark Lox interpreters on bench/programs (the agents never see these).

Usage: run.py name=path [name=path ...] [--runs 3] [--out results/x.json]
       e.g. run.py clox=/private/tmp/ci/clox go-1=bin/go-1 rust-1=bin/rust-1

Per program and interpreter: median wall time and median peak RSS of N runs (/usr/bin/time -l).
zoo_batch runs for a fixed 10 s; its metric is the batch count (higher is better).
Output (without timing lines) is checked against the first interpreter given.
string_equality.lox is excluded: it exceeds the 256-constants-per-chunk limit that the tests require.
"""
import json
import re
import statistics
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PROGRAMS = ROOT / "bench" / "programs"
SKIP = {"string_equality.lox"}
# lines that are elapsed times (printed via clock()) vary run to run and are not compared
TIMING_LINES = {"binary_trees.lox": [-1], "equality.lox": [-5, -3, -1], "fib.lox": [-1],
                "instantiation.lox": [-1], "invocation.lox": [-1], "method_call.lox": [-1],
                "properties.lox": [-1], "trees.lox": [-1], "zoo.lox": [-1], "zoo_batch.lox": [-3, -2, -1]}  # sum depends on the batch count


def run_once(interp, prog):
    r = subprocess.run(["/usr/bin/time", "-l", interp, str(prog)], capture_output=True, text=True, timeout=600)
    wall = float(re.search(r"([\d.]+) real", r.stderr)[1])
    rss = int(re.search(r"(\d+)\s+maximum resident set size", r.stderr)[1]) / 2**20
    out = r.stdout.splitlines()
    drop = {i % len(out) for i in TIMING_LINES.get(prog.name, []) if -len(out) <= i < len(out)}
    out = [line for j, line in enumerate(out) if j not in drop]
    return dict(rc=r.returncode, wall=wall, rss=rss, out=out, raw=r.stdout.splitlines())


def main():
    args, runs, out_path = [], 3, ROOT / "results" / f"bench-{time.strftime('%Y%m%d-%H%M')}.json"
    it = iter(sys.argv[1:])
    for a in it:
        if a == "--runs":
            runs = int(next(it))
        elif a == "--out":
            out_path = Path(next(it))
        else:
            args.append(a.split("=", 1))
    progs = sorted(p for p in PROGRAMS.glob("*.lox") if p.name not in SKIP)
    results = {"started": time.strftime("%Y-%m-%d %H:%M"), "runs": runs, "programs": {}}
    ref_out = {}
    hdr = f"{'program':<18}" + "".join(f"{n:>22}" for n, _ in args)
    print(hdr + "\n" + "-" * len(hdr), flush=True)
    for prog in progs:
        row = results["programs"][prog.name] = {}
        line = f"{prog.stem:<18}"
        for name, interp in args:
            rs = [run_once(interp, prog) for _ in range(runs)]
            ok = all(r["rc"] == 0 for r in rs)
            if prog.name not in ref_out:
                ref_out[prog.name] = rs[0]["out"]
            same = all(r["out"] == ref_out[prog.name] for r in rs)
            wall = statistics.median(r["wall"] for r in rs)
            rss = statistics.median(r["rss"] for r in rs)
            entry = dict(wall=wall, rss_mb=round(rss, 1), ok=ok and same)
            if prog.name == "zoo_batch.lox":
                batches = [int(r["raw"][-2]) if ok else 0 for r in rs]
                entry["batches"] = statistics.median(batches)
                cell = f"{entry['batches']:>7.0f} batch {rss:6.1f}MB"
            else:
                cell = f"{wall:>7.2f}s {rss:7.1f}MB"
            if not entry["ok"]:
                cell = ("RC! " if not ok else "OUT! ") + cell.strip()
            row[name] = entry
            line += f"{cell:>22}"
        print(line, flush=True)
    out_path.parent.mkdir(exist_ok=True)
    out_path.write_text(json.dumps(results, indent=2))
    print(f"wrote {out_path}")


if __name__ == "__main__":
    main()
