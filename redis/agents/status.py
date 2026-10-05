#!/usr/bin/env python3
"""Live dashboard for agent runs (reads runs/*/events.jsonl).

Usage: status.py            one snapshot
       status.py -w [sec]   refresh every N seconds (default 5)
       status.py --log go-1 [N]   last N actions of one run
"""
import json
import os
import re
import sys
import time
from pathlib import Path

RUNS = Path(__file__).resolve().parent.parent / "runs"
TESTS_RE = re.compile(r"(\d+) passed, (\d+) failed, (\d+) skipped")


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def short(s, n):
    s = " ".join(str(s).split())
    return s if len(s) <= n else s[: n - 1] + "…"


def describe_tool(block):
    name, inp = block.get("name"), block.get("input", {})
    if name == "Bash":
        return f"$ {inp.get('command', '')}"
    if name in ("Edit", "Write", "Read"):
        return f"{name} {Path(inp.get('file_path', '')).name}"
    return f"{name} {json.dumps(inp)[:80]}"


def tool_result_text(block):
    c = block.get("content")
    if isinstance(c, list):
        return " ".join(x.get("text", "") for x in c if isinstance(x, dict))
    return str(c or "")


def analyze(d):
    meta = json.loads((d / "meta.json").read_text())
    st = dict(name=meta["name"], started=meta["started"], finished=meta.get("finished"),
              turns=0, tools=0, edits=0, builds=0, build_fails=0, tests=None, best=None,
              last="", last_ts=meta["started"], result=None, cost=None, tokens_out=0,
              actions=[], pid=meta.get("pid"))
    pending = {}  # tool_use_id -> kind
    f = d / "events.jsonl"
    if f.exists():
        for line in f.read_text().splitlines():
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            ts, ev = rec["ts"], rec["ev"]
            t = ev.get("type")
            if t == "assistant":
                st["turns"] += 1
                msg = ev.get("message", {})
                st["tokens_out"] += (msg.get("usage") or {}).get("output_tokens", 0)
                for b in msg.get("content", []):
                    if b.get("type") == "tool_use":
                        st["tools"] += 1
                        desc = describe_tool(b)
                        st["last"], st["last_ts"] = desc, ts
                        st["actions"].append((ts, desc))
                        if b["name"] in ("Edit", "Write"):
                            st["edits"] += 1
                        cmd = b.get("input", {}).get("command", "") if b["name"] == "Bash" else ""
                        if re.search(r"\b(go build|cargo build|go vet|cargo check)\b", cmd):
                            st["builds"] += 1
                            pending[b["id"]] = "build"
                        if "conformance.py" in cmd:
                            pending[b["id"]] = "test"
                    elif b.get("type") == "text" and b.get("text", "").strip():
                        st["actions"].append((ts, "💬 " + b["text"]))
            elif t == "user":
                for b in ev.get("message", {}).get("content", []):
                    if not isinstance(b, dict) or b.get("type") != "tool_result":
                        continue
                    kind = pending.pop(b.get("tool_use_id"), None)
                    text = tool_result_text(b)
                    if kind == "build" and (b.get("is_error") or re.search(r"\berror(\[E\d+\])?:", text)):
                        st["build_fails"] += 1
                    m = TESTS_RE.findall(text)
                    if m:
                        p, fl, sk = map(int, m[-1])
                        st["tests"] = (p, fl, sk)
                        if st["best"] is None or p > st["best"][0]:
                            st["best"] = (p, fl, sk)
            elif t == "system" and ev.get("subtype") == "thinking_tokens":
                if ts - st["last_ts"] > 5:
                    st["last"] = f"🧠 thinking ({ev.get('estimated_tokens', 0) / 1000:.1f}k tok)"
                st["thinking_ts"] = ts
            elif t == "result":
                st["result"] = ev.get("subtype")
                st["cost"] = ev.get("total_cost_usd")
                st["duration"] = ev.get("duration_ms", 0) / 1000
                # streamed per-message usage is partial; the final result has the real total
                st["tokens_out"] = (ev.get("usage") or {}).get("output_tokens", st["tokens_out"])
    if st["finished"]:
        st["state"] = "done" if st["result"] == "success" else f"exit:{st['result']}"
    elif st["pid"] and not alive(st["pid"]):
        st["state"] = "DIED"
    else:
        st["state"] = "running"
    return st


def fmt_dur(s):
    s = int(s)
    return f"{s // 60}:{s % 60:02d}"


def snapshot():
    now = time.time()
    rows = [analyze(d) for d in sorted(RUNS.iterdir()) if (d / "meta.json").exists()] if RUNS.exists() else []
    hdr = f"{'run':<8} {'state':<9} {'elapsed':>7} {'turns':>5} {'tools':>5} {'edits':>5} {'builds':>9} {'tests':>10} {'out tok':>8} {'cost':>7}  last action"
    lines = [time.strftime("%H:%M:%S"), hdr, "-" * len(hdr)]
    for r in rows:
        end = r["finished"] or now
        tests = "-" if not r["tests"] else f"{r['tests'][0]}/{sum(r['tests'])}"
        builds = f"{r['builds']} ({r['build_fails']}✗)"
        cost = f"${r['cost']:.2f}" if r["cost"] is not None else ""
        idle = "" if r["finished"] else f" [{int(now - max(r['last_ts'], r.get('thinking_ts', 0)))}s ago]"
        lines.append(f"{r['name']:<8} {r['state']:<9} {fmt_dur(end - r['started']):>7} {r['turns']:>5} {r['tools']:>5} "
                     f"{r['edits']:>5} {builds:>9} {tests:>10} {r['tokens_out']:>8} {cost:>7}  {short(r['last'], 60)}{idle}")
    if not rows:
        lines.append("(no runs yet)")
    return "\n".join(lines)


def show_log(name, n):
    r = analyze(RUNS / name)
    for ts, desc in r["actions"][-n:]:
        print(f"{fmt_dur(ts - r['started']):>6}  {short(desc, 150)}")


def main():
    args = sys.argv[1:]
    if args[:1] == ["--log"]:
        show_log(args[1], int(args[2]) if len(args) > 2 else 30)
    elif args[:1] == ["-w"]:
        every = float(args[1]) if len(args) > 1 else 5
        while True:
            out = snapshot()
            print("\033[2J\033[H" + out, flush=True)
            time.sleep(every)
    else:
        print(snapshot())


if __name__ == "__main__":
    main()
