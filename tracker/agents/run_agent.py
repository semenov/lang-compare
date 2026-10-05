#!/usr/bin/env python3
"""Run one headless Claude Code agent that implements the tracker backend in one language.

Usage: run_agent.py <go|rust> <run-number> [--model claude-opus-5-5]

The workspace is outside the repo (/private/tmp/tracker-runs/<lang>-<n>) and contains only SPEC.md,
docker-compose.yml and run_tests.py (the reference implementation is not given to the agent).
PostgreSQL must already be running (container tracker-postgres); each run uses its own database. Every stream-json event
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
ROOT = HERE.parent
WORKROOT = Path("/private/tmp/tracker-runs")

PROMPT = """\
Implement the service described in SPEC.md (in this directory) in {lang_name}.

- Write all code in this directory; do not read or write anything outside it
  (except the toolchain, package registries and caches).
- Toolchain: {toolchain}. Network access for dependencies is available.
- Build in release mode: {build}. The binary must be `{binary}`.
- PostgreSQL is already running in Docker (container `tracker-postgres`, port 55432); do not start, stop or
  reconfigure it. Use only the database `{db}` (other runs use other databases at the same time); the test
  runner creates it.
- Done means: `python3 run_tests.py {binary} --db {db}` reports 0 failed.
- We want a solution that is as fast as possible and uses as little memory as possible; it will be
  benchmarked as described in the Performance section of SPEC.md.
- When done, reply with a short summary of the design.
"""

PERF_LINE = ("- We want a solution that is as fast as possible and uses as little memory as possible; it will be\n"
             "  benchmarked as described in the Performance section of SPEC.md.\n")

LANGS = {
    "go": dict(lang_name="Go", toolchain="Go 1.27.1",
               build="`go build -o tracker .`", binary="./tracker"),
    "rust": dict(lang_name="Rust", toolchain="Rust 1.98.1 (cargo)",
                 build="`cargo build --release`", binary="./target/release/tracker"),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("lang", choices=LANGS)
    ap.add_argument("n", type=int)
    ap.add_argument("--model", default="claude-opus-5-5")
    ap.add_argument("--plain", action="store_true",
                    help="no performance requirements: SPEC.md without section 10, prompt without the speed/memory line")
    a = ap.parse_args()

    name = f"{a.lang}-plain-{a.n}" if a.plain else f"{a.lang}-{a.n}"
    ws = WORKROOT / name
    if ws.exists():
        sys.exit(f"{ws} already exists; remove it to rerun")
    ws.mkdir(parents=True)
    spec = (ROOT / "SPEC.md").read_text()
    if a.plain:
        spec = spec[:spec.index("## 10. Performance")].rstrip() + "\n"
    (ws / "SPEC.md").write_text(spec)
    shutil.copy(ROOT / "run_tests.py", ws / "run_tests.py")
    shutil.copy(ROOT / "docker-compose.yml", ws / "docker-compose.yml")
    subprocess.run(["git", "init", "-q"], cwd=ws, check=True)

    out = ROOT / "runs" / name
    out.mkdir(parents=True, exist_ok=True)
    prompt = PROMPT.format(db="tracker_" + name.replace("-", "_"), **LANGS[a.lang])
    if a.plain:
        prompt = prompt.replace(PERF_LINE, "")
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
