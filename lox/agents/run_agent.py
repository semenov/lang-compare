#!/usr/bin/env python3
"""Run one headless Claude Code agent that implements the Lox interpreter in one language.

Usage: run_agent.py <go|rust> <run-number> [--model claude-opus-5-5]

The workspace is outside the repo (/private/tmp/lox-runs/<lang>-<n>) and contains only SPEC.md,
run_tests.py and tests/ (the benchmark programs are not given to the agent). Every stream-json event
is written with a timestamp to runs/<lang>-<n>/events.jsonl; status.py reads these.
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
LOX = HERE.parent
WORKROOT = Path("/private/tmp/lox-runs")

PROMPT = """\
Implement the Lox interpreter described in SPEC.md (in this directory) in {lang_name}.

- Write all code in this directory; do not read or write anything outside it
  (except the toolchain, package registries and caches).
- Toolchain: {toolchain}. Network access for dependencies is available.
- Build in release mode: {build}. The interpreter binary must be `{binary}`.
- Done means: `python3 run_tests.py {binary}` reports 0 failed.
- The interpreter will be benchmarked for speed and memory usage (peak RSS) on CPU-heavy Lox programs.
- When done, reply with a short summary of the design.
"""

LANGS = {
    "go": dict(lang_name="Go", toolchain="Go 1.27.1",
               build="`go build -o lox .`", binary="./lox"),
    "rust": dict(lang_name="Rust", toolchain="Rust 1.98.1 (cargo)",
                 build="`cargo build --release`", binary="./target/release/lox"),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("lang", choices=LANGS)
    ap.add_argument("n", type=int)
    ap.add_argument("--model", default="claude-opus-5-5")
    a = ap.parse_args()

    name = f"{a.lang}-{a.n}"
    ws = WORKROOT / name
    if ws.exists():
        sys.exit(f"{ws} already exists; remove it to rerun")
    ws.mkdir(parents=True)
    shutil.copy(LOX / "SPEC.md", ws / "SPEC.md")
    shutil.copy(LOX / "run_tests.py", ws / "run_tests.py")
    shutil.copytree(LOX / "tests", ws / "tests")
    subprocess.run(["git", "init", "-q"], cwd=ws, check=True)

    out = LOX / "runs" / name
    out.mkdir(parents=True, exist_ok=True)
    prompt = PROMPT.format(**LANGS[a.lang])
    meta = dict(name=name, lang=a.lang, n=a.n, model=a.model, workspace=str(ws), prompt=prompt,
                started=time.time(), pid=os.getpid())
    (out / "meta.json").write_text(json.dumps(meta, indent=2))

    cmd = ["claude", "-p", prompt, "--model", a.model,
           "--output-format", "stream-json", "--verbose",
           "--permission-mode", "acceptEdits",
           "--allowedTools", "Bash,Read,Edit,Write,Glob,Grep,TodoWrite"]
    with open(out / "events.jsonl", "w") as log:
        p = subprocess.Popen(cmd, cwd=ws, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                             stdin=subprocess.DEVNULL, text=True)
        for line in p.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                ev = {"type": "raw", "text": line}
            log.write(json.dumps({"ts": time.time(), "ev": ev}) + "\n")
            log.flush()
        rc = p.wait()

    meta.update(finished=time.time(), exit_code=rc)
    (out / "meta.json").write_text(json.dumps(meta, indent=2))


if __name__ == "__main__":
    main()
