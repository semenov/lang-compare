# Lox interpreter

Implement an interpreter for **Lox**, the language from Robert Nystrom's book
*Crafting Interpreters* (https://craftinginterpreters.com): dynamic typing, numbers (double),
strings, booleans, `nil`, `var`, blocks and lexical scope, `if`/`while`/`for`, `and`/`or`,
functions, closures, classes, methods, initializers, `this`, single inheritance and `super`,
`print`, and the native function `clock()`.

## Interface

- `<interpreter> path/to/script.lox` runs the script. A REPL is not required.
- Program output (`print`) goes to stdout, one value per line.
- Compile (syntax/resolution) errors are reported on stderr in the form `[line N] Error at 'x': message`,
  and the process exits with code **65**. All errors found in the script are reported, not just the first.
- Runtime errors print the message on stderr followed by a stack trace with `[line N]` entries,
  and the process exits with code **70**.
- The exact error messages, number formatting, and implementation limits must match what the test
  suite expects.

## Acceptance

`python3 run_tests.py <interpreter>` must report **0 failed**. The tests in `tests/` are the book's
official test suite (MIT license); each test states its expected stdout, compile errors, or
runtime error in comments.

## Performance

The interpreter will be benchmarked for **speed** and **memory usage (peak RSS)** on
CPU-heavy Lox programs.
