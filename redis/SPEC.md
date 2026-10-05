# mini-redis — reference server spec

An in-memory key-value server that speaks the Redis protocol (RESP2). It is a strict
subset of Redis: every command listed here must behave exactly as in Redis 8
(same replies, same error strings), unless this document says otherwise.
The black-box test suite `bench/conformance.py` also passes against real `redis-server`.

Unlike Redis, the server **must use multiple CPU cores** for request processing.

## Environment

| var         | default | meaning |
|-------------|---------|---------|
| `PORT`      | `6380`  | TCP port, listen on `0.0.0.0` |
| `THREADS`   | number of CPUs | max OS threads executing request handling (`GOMAXPROCS` / tokio workers / thread pool size) |
| `MAXMEMORY` | `0`     | memory limit in bytes of *accounted* memory (see below); `0` = unlimited |

No persistence, no replication, no auth, no databases other than 0, no RESP3.

## Protocol

- Requests are RESP arrays of bulk strings (`*2\r\n$3\r\nGET\r\n$1\r\nk\r\n`). Inline commands
  are not required.
- Command names are case-insensitive. Keys and values are binary-safe byte strings.
- **Pipelining** must work: a client may send many commands without waiting; replies come in order.
  Replies may be buffered and flushed when the input buffer has no complete command left.
- Many concurrent clients (≥ 10 000 idle connections must be fine).
- A protocol error (malformed RESP) → reply `-ERR Protocol error: <anything>` and close the connection.
- Unknown command → `-ERR unknown command '<name>', with args beginning with: <args...>`
  (tests only check the `-ERR unknown command` prefix).
- Wrong number of arguments → `-ERR wrong number of arguments for '<name-lowercase>' command`.
- Operation on a key holding the wrong type →
  `-WRONGTYPE Operation against a key holding the wrong kind of value`.
- Integer argument that does not parse as a signed 64-bit integer → `-ERR value is not an integer or out of range`.
- `-ERR syntax error` for bad options (e.g. `SET k v FOO`).

## Data types

String, list, hash. One keyspace. Each key may have an expiry time (millisecond precision).

## Commands

Connection / server:

| command | reply |
|---|---|
| `PING [msg]` | `+PONG` or bulk `msg` |
| `ECHO msg` | bulk |
| `DBSIZE` | integer, see *Expiration* |
| `FLUSHALL` | `+OK` — removes all keys |
| `INFO [section]` | bulk string; must contain the line `used_memory:<n>` (accounted bytes, see below) and `connected_clients:<n>`. Other lines optional. |
| `CONFIG GET pattern` | `*0` (empty array) is fine — exists so `redis-benchmark` doesn't complain |
| `COMMAND ...` | `*0` (empty array) — exists so `redis-cli` doesn't complain |
| `QUIT` | `+OK`, then close |

Keys:

| command | notes |
|---|---|
| `DEL key [key ...]` | integer, number removed |
| `EXISTS key [key ...]` | integer, counts repeats |
| `TYPE key` | `+string` / `+list` / `+hash` / `+none` |
| `EXPIRE key seconds`, `PEXPIRE key ms` | `:1` if set, `:0` if key missing. Non-positive timeout deletes the key (returns `:1`). No NX/XX/GT/LT options. |
| `TTL key`, `PTTL key` | `-2` missing, `-1` no expiry, else remaining (TTL rounds like Redis: `(ms+500)/1000`) |
| `PERSIST key` | `:1` if an expiry was removed, else `:0` |
| `KEYS pattern` | glob: `*`, `?`, `[abc]`, `[^a]`, `[a-z]`, `\x` escapes. Order unspecified. |

Strings:

| command | notes |
|---|---|
| `GET key` | bulk or null bulk `$-1` |
| `SET key value [NX\|XX] [GET] [EX s\|PX ms\|KEEPTTL]` | `+OK`, or null if NX/XX condition failed; with `GET` returns old value (null if none; WRONGTYPE if old is not a string). SET without KEEPTTL clears the TTL. `EX`/`PX` ≤ 0 → `-ERR invalid expire time in 'set' command` |
| `MGET key [key ...]` | array; non-strings and missing → null |
| `MSET k v [k v ...]` | `+OK`, atomic |
| `INCR`, `DECR`, `INCRBY key n`, `DECRBY key n` | integer; missing key = 0; value must be a decimal int64 without leading `+`/spaces/leading zeros, else `-ERR value is not an integer or out of range`; overflow → `-ERR increment or decrement would overflow`. Keeps TTL. |
| `APPEND key value` | integer, new length. Keeps TTL. |
| `STRLEN key` | integer |

Lists (empty lists are deleted):

| command | notes |
|---|---|
| `LPUSH`, `RPUSH key v [v ...]` | integer, new length |
| `LPOP`, `RPOP key [count]` | without count: bulk or null; with count: array or null array `*-1` if key missing; count that is not a non-negative integer → `-ERR value is out of range, must be positive` |
| `LLEN key` | integer |
| `LRANGE key start stop` | Redis index semantics (negative from end, out of range clamped) |
| `LINDEX key index` | bulk or null |

Hashes (empty hashes are deleted):

| command | notes |
|---|---|
| `HSET key f v [f v ...]` | integer, number of *new* fields |
| `HGET key f` | bulk or null |
| `HDEL key f [f ...]` | integer |
| `HGETALL key` | flat array `f1 v1 f2 v2 …`, order unspecified |
| `HLEN key`, `HEXISTS key f` | integer |
| `HINCRBY key f n` | integer; same parsing/overflow rules as INCRBY, error for non-int field: `-ERR hash value is not an integer` |

Pub/Sub:

| command | notes |
|---|---|
| `SUBSCRIBE ch [ch ...]` | for each channel: `*3 $9 subscribe $<ch> :<count>` |
| `UNSUBSCRIBE [ch ...]` | for each: `*3 $11 unsubscribe $<ch> :<count>`; no args = all; if subscribed to nothing: `*3 $11 unsubscribe $-1 :0` |
| `PUBLISH ch msg` | integer, number of receivers. Subscribers get `*3 $7 message $<ch> $<msg>` |

While a connection has ≥ 1 subscription only `SUBSCRIBE`, `UNSUBSCRIBE`, `PING`, `QUIT` are allowed;
others → `-ERR Can't execute '<cmd-lowercase>': only (P|S)SUBSCRIBE / (P|S)UNSUBSCRIBE / PING / QUIT / RESET are allowed in this context`.
`PING` in subscribed mode replies `*2 $4 pong $0 ` (or `$<msg>`).
Messages published by one client must be delivered to each subscriber in publish order.
A slow subscriber must not block publishers or other clients (buffer or drop; dropping
a subscriber whose output buffer exceeds 32 MB by closing its connection is allowed).

## Expiration

- An expired key is never visible to any command (`GET`, `EXISTS`, `TTL`, `KEYS`, `TYPE`, …).
- Expired keys must also be **reclaimed** (memory freed) without being accessed: within **2 seconds**
  after expiry, a key must no longer be counted by `DBSIZE` or `used_memory`.
  `DBSIZE` may count keys that expired less than 2 s ago.

## Memory accounting and eviction

*Accounted memory* is a logical number, identical across implementations:

```
key_cost    = 64 + len(key) + value_cost
string      = len(value)
list        = Σ (16 + len(element))
hash        = Σ (32 + len(field) + len(value))
used_memory = Σ key_cost over all live (not yet reclaimed) keys
```

`INFO` reports `used_memory:<used_memory>`. (Real Redis reports its true allocator usage here;
the tests that check exact numbers are skipped against real Redis.)

When `MAXMEMORY > 0`, policy is **allkeys-lru**:
- Any command that reads or writes a key counts as an access to it.
- After a write command, if `used_memory > MAXMEMORY`, keys are evicted until it is `≤ MAXMEMORY`,
  **before the reply is sent** (with no concurrent writers, `INFO` right after the reply must show
  `used_memory ≤ MAXMEMORY`).
- A command never evicts the key it is writing.
- LRU may be approximate (sampling like Redis, per-shard LRU, …) but must be good:
  fill 10 000 keys, then access the first 1 000 of them, then write 2 000 new keys of the same
  size with a limit that only fits 10 000 → at least **95 %** of the 1 000 recently accessed keys
  must survive.
- If a write cannot fit even after evicting all other keys →
  `-OOM command not allowed when used memory > 'maxmemory'.` and nothing is changed.

## Concurrency

- Every command is atomic (`MSET`, `INCR`, `LPUSH` with several values, `HSET`, `DEL k1 k2`, …):
  no client may observe a partially applied command.
- 50 clients each doing 1 000 `INCR` on the same key → exactly 50 000.
- The server must scale on multiple cores (`THREADS`): throughput with `THREADS=8` should be
  clearly higher than with `THREADS=1` on a multi-key workload.

## Build & run

- Go: `go build -o mini-redis .` (any module layout), Go 1.27, standard library preferred; third-party modules allowed.
- Rust: `cargo build --release`, binary `target/release/mini-redis`, edition 2024, any crates.
- Graceful shutdown on SIGTERM/SIGINT is not required.
