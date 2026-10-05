#!/usr/bin/env python3
"""Run one headless Claude Code agent that implements mini-redis in one language.

Usage: run_agent.py <go|rust> <run-number> [--model claude-opus-5-5]

Creates an isolated workspace outside the repo (so agents can't see each other's
code or the other implementations), copies SPEC.md + conformance.py there, and
runs `claude -p` in it. Every stream-json event is written to
runs/<lang>-<n>/events.jsonl with a wall-clock timestamp; status.py reads these.
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
REDIS = HERE.parent
WORKROOT = Path("/private/tmp/mini-redis-runs")

PROMPT = """\
Implement the server described in SPEC.md (in this directory) in {lang_name}.

- Write all code in this directory; do not read or write anything outside it
  (except the toolchain, package registries and caches).
- Toolchain: {toolchain}. Network access for dependencies is available.
- Build in release mode: {build}. The binary must be `{binary}`.
- Done means: the release binary is running with `PORT={port}` and
  `python3 bench/conformance.py --port {port} --evict-port {evict_port} --bin {binary}`
  reports 0 failed and 0 skipped. Use exactly these ports (other runs use other ports).
- Performance and memory efficiency matter: this server will be benchmarked
  (throughput, p99 latency, RSS per million keys, multi-core scaling).
- Stop any server processes you started before finishing.
- When done, reply with a short summary of the design.
"""

LANGS = {
    "go": dict(lang_name="Go", toolchain="Go 1.27.1",
               build="`go build -o mini-redis .`", binary="./mini-redis"),
    "rust": dict(lang_name="Rust", toolchain="Rust 1.98.1 (cargo)",
                 build="`cargo build --release`", binary="./target/release/mini-redis"),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("lang", choices=LANGS)
    ap.add_argument("n", type=int)
    ap.add_argument("--model", default="claude-opus-5-5")
    a = ap.parse_args()

    name = f"{a.lang}-{a.n}"
    # unique ports per run: go-1 → 6410/6510, rust-1 → 6415/6515, go-2 → 6420/6520, ...
    port = 6400 + a.n * 10 + (0 if a.lang == "go" else 5)
    evict_port = port + 100

    ws = WORKROOT / name
    if ws.exists():
        sys.exit(f"{ws} already exists; remove it to rerun")
    ws.mkdir(parents=True)
    (ws / "bench").mkdir()
    shutil.copy(REDIS / "SPEC.md", ws / "SPEC.md")
    shutil.copy(REDIS / "bench" / "conformance.py", ws / "bench" / "conformance.py")
    subprocess.run(["git", "init", "-q"], cwd=ws, check=True)

    out = REDIS / "runs" / name
    out.mkdir(parents=True, exist_ok=True)
    prompt = PROMPT.format(port=port, evict_port=evict_port, **LANGS[a.lang])
    meta = dict(name=name, lang=a.lang, n=a.n, model=a.model, workspace=str(ws),
                port=port, evict_port=evict_port, prompt=prompt,
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
