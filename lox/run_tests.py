#!/usr/bin/env python3
"""Lox conformance runner: a Python port of craftinginterpreters' tool/bin/test.dart
(the "clox" suite: all tests in tests/, with the C-interpreter variants of error expectations).

Usage: run_tests.py <interpreter> [path-filter]
       e.g. run_tests.py ./lox            run_tests.py ./lox closure/

The interpreter is invoked as `<interpreter> <file.lox>`. Expectations are comments in the tests:
  // expect: <stdout line>
  // Error ...                         compile error on this line -> stderr "[line N] Error ...", exit 65
  // [line N] Error ... / [c line N]   compile error on line N
  // expect runtime error: <message>   stderr line 1 = message, then "[line N]" in the trace, exit 70
"""
import re
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

TESTS = Path(__file__).resolve().parent / "tests"

EXPECTED_OUTPUT = re.compile(r"// expect: ?(.*)")
EXPECTED_ERROR = re.compile(r"// (Error.*)")
ERROR_LINE = re.compile(r"// \[((java|c) )?line (\d+)\] (Error.*)")
EXPECTED_RUNTIME_ERROR = re.compile(r"// expect runtime error: (.+)")
SYNTAX_ERROR = re.compile(r"\[.*line (\d+)\] (Error.+)")
STACK_TRACE = re.compile(r"\[line (\d+)\]")
NON_TEST = re.compile(r"// nontest")


def parse(path):
    t = dict(output=[], errors=set(), runtime=None, runtime_line=0, exit=0)
    for num, line in enumerate(path.read_text().splitlines(), 1):
        if NON_TEST.search(line):
            return None
        if m := EXPECTED_OUTPUT.search(line):
            t["output"].append((num, m[1]))
            continue
        if m := EXPECTED_ERROR.search(line):
            t["errors"].add(f"[{num}] {m[1]}")
            t["exit"] = 65
            continue
        if m := ERROR_LINE.search(line):
            if m[2] in (None, "c"):
                t["errors"].add(f"[{m[3]}] {m[4]}")
                t["exit"] = 65
            continue
        if m := EXPECTED_RUNTIME_ERROR.search(line):
            t["runtime"], t["runtime_line"], t["exit"] = m[1], num, 70
    return t


def run(interp, path):
    t = parse(path)
    if t is None:
        return None
    try:
        r = subprocess.run([interp, str(path)], capture_output=True, timeout=30)
    except subprocess.TimeoutExpired:
        return ["timed out after 30 s"]
    out = r.stdout.decode(errors="replace").splitlines()
    err = r.stderr.decode(errors="replace").splitlines()
    fails = []

    if t["runtime"] is not None:
        if len(err) < 2:
            fails.append(f"Expected runtime error '{t['runtime']}' and got none.")
        else:
            if err[0] != t["runtime"]:
                fails += [f"Expected runtime error '{t['runtime']}' and got:", err[0]]
            m = next((m for m in map(STACK_TRACE.search, err[1:]) if m), None)
            if not m:
                fails += ["Expected stack trace and got:", *err[1:]]
            elif int(m[1]) != t["runtime_line"]:
                fails.append(f"Expected runtime error on line {t['runtime_line']} but was on line {m[1]}.")
    else:
        found, unexpected = set(), 0
        for line in err:
            m = SYNTAX_ERROR.search(line)
            if m and f"[{m[1]}] {m[2]}" in t["errors"]:
                found.add(f"[{m[1]}] {m[2]}")
            elif m or line != "":
                if unexpected < 10:
                    fails += ["Unexpected error:" if m else "Unexpected output on stderr:", line]
                unexpected += 1
        fails += [f"Missing expected error: {e}" for e in sorted(t["errors"] - found)]

    if r.returncode != t["exit"]:
        fails += [f"Expected return code {t['exit']} and got {r.returncode}. Stderr:", *err[:10]]

    if out and out[-1] == "":
        out.pop()
    for i, line in enumerate(out):
        if i >= len(t["output"]):
            fails.append(f"Got output '{line}' when none was expected.")
        elif t["output"][i][1] != line:
            fails.append(f"Expected output '{t['output'][i][1]}' on line {t['output'][i][0]} and got '{line}'.")
    for num, exp in t["output"][len(out):]:
        fails.append(f"Missing expected output '{exp}' on line {num}.")
    return fails


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    interp = str(Path(sys.argv[1]).resolve())
    flt = sys.argv[2] if len(sys.argv) > 2 else ""
    paths = sorted(p for p in TESTS.rglob("*.lox") if str(p.relative_to(TESTS)).startswith(flt))
    with ThreadPoolExecutor(8) as ex:
        results = list(ex.map(lambda p: (p, run(interp, p)), paths))
    passed = failed = 0
    for p, fails in results:
        if fails is None:
            continue
        if fails:
            failed += 1
            print(f"FAIL {p.relative_to(TESTS)}")
            for f in fails[:12]:
                print(f"     {f}")
        else:
            passed += 1
    print(f"\n{passed} passed, {failed} failed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
