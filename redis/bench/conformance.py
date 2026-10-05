#!/usr/bin/env python3
"""Black-box conformance tests for mini-redis (see ../SPEC.md).

Usage: conformance.py [--port 6380] [--real-redis] [-k substring]

--real-redis: the target is a real redis-server; tests that depend on our own
memory accounting are skipped, and eviction tests configure maxmemory via CONFIG SET.

Eviction tests need the server started with MAXMEMORY; they start their own
instance when --bin is given (path to the server binary), otherwise they are skipped.
"""
import argparse
import os
import socket
import subprocess
import sys
import threading
import time
import traceback

# ---------------------------------------------------------------- RESP client


class Err(Exception):
    pass


class Conn:
    def __init__(self, port, host="127.0.0.1", timeout=5):
        self.s = socket.create_connection((host, port), timeout=timeout)
        self.s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        self.buf = b""

    def close(self):
        self.s.close()

    def send_raw(self, data: bytes):
        self.s.sendall(data)

    @staticmethod
    def encode(*args):
        out = [b"*%d\r\n" % len(args)]
        for a in args:
            if isinstance(a, int):
                a = str(a)
            if isinstance(a, str):
                a = a.encode()
            out.append(b"$%d\r\n%s\r\n" % (len(a), a))
        return b"".join(out)

    def _fill(self):
        chunk = self.s.recv(1 << 16)
        if not chunk:
            raise ConnectionError("connection closed")
        self.buf += chunk

    def _line(self):
        while b"\r\n" not in self.buf:
            self._fill()
        line, self.buf = self.buf.split(b"\r\n", 1)
        return line

    def _exact(self, n):
        while len(self.buf) < n + 2:
            self._fill()
        data, self.buf = self.buf[:n], self.buf[n + 2:]
        return data

    def read(self):
        line = self._line()
        t, rest = line[:1], line[1:]
        if t == b"+":
            return rest.decode()
        if t == b"-":
            return Err(rest.decode())
        if t == b":":
            return int(rest)
        if t == b"$":
            n = int(rest)
            return None if n < 0 else self._exact(n)
        if t == b"*":
            n = int(rest)
            return None if n < 0 else [self.read() for _ in range(n)]
        raise ValueError(f"bad reply type: {line!r}")

    def cmd(self, *args):
        self.send_raw(self.encode(*args))
        return self.read()

    def closed(self, wait=2.0):
        """True if the server closed the connection."""
        self.s.settimeout(wait)
        try:
            while True:
                if not self.s.recv(1 << 16):
                    return True
        except (ConnectionResetError, BrokenPipeError):
            return True
        except socket.timeout:
            return False


# ---------------------------------------------------------------- framework

TESTS = []


def test(fn=None, *, own_accounting=False, needs_bin=False):
    def wrap(f):
        f.own_accounting = own_accounting
        f.needs_bin = needs_bin
        TESTS.append(f)
        return f
    return wrap(fn) if fn else wrap


def eq(got, want, what=""):
    if got != want:
        raise AssertionError(f"{what}: expected {want!r}, got {got!r}")


def is_err(got, prefix, what=""):
    if not isinstance(got, Err) or not str(got).startswith(prefix):
        raise AssertionError(f"{what}: expected error starting with {prefix!r}, got {got!r}")


def info_field(c, name):
    info = c.cmd("INFO")
    if isinstance(info, Err):
        raise AssertionError(f"INFO failed: {info}")
    for line in info.decode().splitlines():
        if line.startswith(name + ":"):
            return int(line.split(":", 1)[1])
    raise AssertionError(f"INFO has no {name}")


CTX = {}  # port, real, bin


def C():
    c = Conn(CTX["port"])
    return c


def fresh():
    c = C()
    eq(c.cmd("FLUSHALL"), "OK", "FLUSHALL")
    return c


# ---------------------------------------------------------------- protocol


@test
def ping_echo():
    c = C()
    eq(c.cmd("PING"), "PONG")
    eq(c.cmd("ping", "hi"), b"hi")
    eq(c.cmd("EcHo", "a\r\nb\x00c"), b"a\r\nb\x00c", "binary-safe echo")
    is_err(c.cmd("ECHO"), "ERR wrong number of arguments for 'echo' command")


@test
def unknown_command():
    c = C()
    is_err(c.cmd("NOSUCHCMD", "x"), "ERR unknown command")
    eq(c.cmd("PING"), "PONG", "connection still usable")


@test
def pipelining():
    c = fresh()
    n = 10_000
    payload = b"".join(c.encode("SET", f"p{i}", f"v{i}") for i in range(n))
    payload += b"".join(c.encode("GET", f"p{i}") for i in range(n))
    t = threading.Thread(target=c.send_raw, args=(payload,))
    t.start()
    for i in range(n):
        eq(c.read(), "OK", f"SET p{i}")
    for i in range(n):
        eq(c.read(), f"v{i}".encode(), f"GET p{i}")
    t.join()


@test
def split_packets():
    c = fresh()
    raw = c.encode("SET", "split", "hello") + c.encode("GET", "split")
    for b in raw:
        c.send_raw(bytes([b]))
    eq(c.read(), "OK")
    eq(c.read(), b"hello")


@test
def large_value():
    c = fresh()
    v = os.urandom(5 * 1024 * 1024)
    eq(c.cmd("SET", "big", v), "OK")
    eq(c.cmd("GET", "big"), v)
    eq(c.cmd("STRLEN", "big"), len(v))


@test
def protocol_error_closes():
    c = C()
    c.send_raw(b"*1\r\n$abc\r\n")
    r = c.read()
    is_err(r, "ERR Protocol error")
    eq(c.closed(), True, "connection closed after protocol error")


@test
def quit_closes():
    c = C()
    eq(c.cmd("QUIT"), "OK")
    eq(c.closed(), True, "closed after QUIT")


@test
def config_and_command_stubs():
    c = C()
    r = c.cmd("CONFIG", "GET", "save")
    if not isinstance(r, list):
        raise AssertionError(f"CONFIG GET should return an array, got {r!r}")
    r = c.cmd("COMMAND", "DOCS")
    if not isinstance(r, list):
        raise AssertionError(f"COMMAND DOCS should return an array, got {r!r}")


@test
def many_connections():
    conns = [C() for _ in range(500)]
    for i, c in enumerate(conns):
        c.send_raw(c.encode("ECHO", str(i)))
    for i, c in enumerate(conns):
        eq(c.read(), str(i).encode())
    n = info_field(conns[0], "connected_clients")
    if n < 500:
        raise AssertionError(f"connected_clients={n}, expected >= 500")
    for c in conns:
        c.close()


# ---------------------------------------------------------------- strings


@test
def set_get_del():
    c = fresh()
    eq(c.cmd("GET", "a"), None)
    eq(c.cmd("SET", "a", "1"), "OK")
    eq(c.cmd("GET", "a"), b"1")
    eq(c.cmd("SET", "a", "2"), "OK")
    eq(c.cmd("GET", "a"), b"2")
    eq(c.cmd("SET", "b", "x"), "OK")
    eq(c.cmd("DEL", "a", "b", "zz", "a"), 2)
    eq(c.cmd("GET", "a"), None)
    eq(c.cmd("EXISTS", "a"), 0)
    is_err(c.cmd("GET"), "ERR wrong number of arguments for 'get' command")
    is_err(c.cmd("SET", "a"), "ERR wrong number of arguments for 'set' command")


@test
def set_options():
    c = fresh()
    eq(c.cmd("SET", "k", "v", "NX"), "OK")
    eq(c.cmd("SET", "k", "v2", "NX"), None)
    eq(c.cmd("GET", "k"), b"v")
    eq(c.cmd("SET", "nope", "v", "XX"), None)
    eq(c.cmd("EXISTS", "nope"), 0)
    eq(c.cmd("SET", "k", "v3", "XX"), "OK")
    eq(c.cmd("SET", "k", "v4", "GET"), b"v3")
    eq(c.cmd("SET", "new", "x", "GET"), None)
    eq(c.cmd("GET", "new"), b"x")
    is_err(c.cmd("SET", "k", "v", "FOO"), "ERR syntax error")
    is_err(c.cmd("SET", "k", "v", "NX", "XX"), "ERR syntax error")
    is_err(c.cmd("SET", "k", "v", "EX"), "ERR syntax error")
    is_err(c.cmd("SET", "k", "v", "EX", "0"), "ERR invalid expire time in 'set' command")
    is_err(c.cmd("SET", "k", "v", "PX", "-5"), "ERR invalid expire time in 'set' command")
    is_err(c.cmd("SET", "k", "v", "EX", "abc"), "ERR value is not an integer or out of range")
    c.cmd("RPUSH", "lst", "a")
    is_err(c.cmd("SET", "lst", "v", "GET"), "WRONGTYPE")
    eq(c.cmd("TYPE", "lst"), "list", "failed SET GET must not overwrite")
    eq(c.cmd("SET", "lst", "v"), "OK", "plain SET overwrites any type")
    eq(c.cmd("TYPE", "lst"), "string")


@test
def set_ttl_options():
    c = fresh()
    eq(c.cmd("SET", "k", "v", "EX", "100"), "OK")
    t = c.cmd("TTL", "k")
    if not 99 <= t <= 100:
        raise AssertionError(f"TTL {t}")
    eq(c.cmd("SET", "k", "v2", "KEEPTTL"), "OK")
    t = c.cmd("TTL", "k")
    if not 99 <= t <= 100:
        raise AssertionError(f"TTL after KEEPTTL {t}")
    eq(c.cmd("SET", "k", "v3"), "OK")
    eq(c.cmd("TTL", "k"), -1, "SET clears TTL")
    eq(c.cmd("SET", "k", "v", "PX", "100000"), "OK")
    pt = c.cmd("PTTL", "k")
    if not 99_000 <= pt <= 100_000:
        raise AssertionError(f"PTTL {pt}")


@test
def mget_mset():
    c = fresh()
    eq(c.cmd("MSET", "a", "1", "b", "2"), "OK")
    c.cmd("RPUSH", "l", "x")
    eq(c.cmd("MGET", "a", "nope", "b", "l"), [b"1", None, b"2", None])
    is_err(c.cmd("MSET", "a", "1", "b"), "ERR wrong number of arguments for 'mset' command")


@test
def incr_family():
    c = fresh()
    eq(c.cmd("INCR", "n"), 1)
    eq(c.cmd("INCRBY", "n", "10"), 11)
    eq(c.cmd("DECR", "n"), 10)
    eq(c.cmd("DECRBY", "n", "-5"), 15)
    eq(c.cmd("GET", "n"), b"15")
    c.cmd("SET", "n", "-7")
    eq(c.cmd("INCR", "n"), -6)
    for bad in ["abc", "1.5", " 1", "+1", "01", "", "99999999999999999999"]:
        c.cmd("SET", "bad", bad)
        is_err(c.cmd("INCR", "bad"), "ERR value is not an integer or out of range", f"INCR on {bad!r}")
    is_err(c.cmd("INCRBY", "n", "x"), "ERR value is not an integer or out of range")
    c.cmd("SET", "max", str(2**63 - 1))
    is_err(c.cmd("INCR", "max"), "ERR increment or decrement would overflow")
    c.cmd("SET", "min", str(-(2**63)))
    is_err(c.cmd("DECR", "min"), "ERR increment or decrement would overflow")
    eq(c.cmd("GET", "max"), str(2**63 - 1).encode())
    c.cmd("SET", "t", "5", "EX", "100")
    c.cmd("INCR", "t")
    if c.cmd("TTL", "t") < 0:
        raise AssertionError("INCR must keep TTL")
    c.cmd("RPUSH", "l", "x")
    is_err(c.cmd("INCR", "l"), "WRONGTYPE")


@test
def append_strlen():
    c = fresh()
    eq(c.cmd("APPEND", "s", "ab"), 2)
    eq(c.cmd("APPEND", "s", "cd"), 4)
    eq(c.cmd("GET", "s"), b"abcd")
    eq(c.cmd("STRLEN", "s"), 4)
    eq(c.cmd("STRLEN", "nope"), 0)
    c.cmd("RPUSH", "l", "x")
    is_err(c.cmd("APPEND", "l", "x"), "WRONGTYPE")
    is_err(c.cmd("STRLEN", "l"), "WRONGTYPE")


# ---------------------------------------------------------------- keys


@test
def type_exists():
    c = fresh()
    c.cmd("SET", "s", "v")
    c.cmd("RPUSH", "l", "v")
    c.cmd("HSET", "h", "f", "v")
    eq(c.cmd("TYPE", "s"), "string")
    eq(c.cmd("TYPE", "l"), "list")
    eq(c.cmd("TYPE", "h"), "hash")
    eq(c.cmd("TYPE", "x"), "none")
    eq(c.cmd("EXISTS", "s", "s", "l", "x"), 3)
    eq(c.cmd("DBSIZE"), 3)


@test
def expire_ttl_persist():
    c = fresh()
    eq(c.cmd("TTL", "nope"), -2)
    eq(c.cmd("PTTL", "nope"), -2)
    eq(c.cmd("EXPIRE", "nope", "10"), 0)
    c.cmd("SET", "k", "v")
    eq(c.cmd("TTL", "k"), -1)
    eq(c.cmd("PERSIST", "k"), 0)
    eq(c.cmd("EXPIRE", "k", "10"), 1)
    eq(c.cmd("TTL", "k"), 10)
    eq(c.cmd("PEXPIRE", "k", "5700"), 1)  # 5501..5700 ms left → rounds to 6, plain truncation gives 5
    eq(c.cmd("TTL", "k"), 6, "TTL rounds (ms+500)/1000")
    eq(c.cmd("PERSIST", "k"), 1)
    eq(c.cmd("TTL", "k"), -1)
    is_err(c.cmd("EXPIRE", "k", "x"), "ERR value is not an integer or out of range")
    eq(c.cmd("EXPIRE", "k", "0"), 1)
    eq(c.cmd("EXISTS", "k"), 0, "non-positive EXPIRE deletes")
    c.cmd("SET", "k", "v")
    eq(c.cmd("PEXPIRE", "k", "-1"), 1)
    eq(c.cmd("GET", "k"), None)


@test
def expiry_is_invisible():
    c = fresh()
    c.cmd("SET", "s", "v", "PX", "100")
    c.cmd("RPUSH", "l", "a")
    c.cmd("PEXPIRE", "l", "100")
    c.cmd("HSET", "h", "f", "v")
    c.cmd("PEXPIRE", "h", "100")
    c.cmd("SET", "keep", "v")
    time.sleep(0.25)
    eq(c.cmd("GET", "s"), None)
    eq(c.cmd("EXISTS", "s", "l", "h"), 0)
    eq(c.cmd("TYPE", "l"), "none")
    eq(c.cmd("TTL", "h"), -2)
    eq(c.cmd("LLEN", "l"), 0)
    eq(c.cmd("HGET", "h", "f"), None)
    eq(sorted(c.cmd("KEYS", "*")), [b"keep"])
    eq(c.cmd("INCR", "s"), 1, "INCR on expired key starts from 0")
    eq(c.cmd("TTL", "s"), -1)


@test
def active_expiry():
    c = fresh()
    n = 20_000
    payload = b"".join(c.encode("SET", f"e{i}", "x" * 10, "PX", "200") for i in range(n))
    c.send_raw(payload)
    for _ in range(n):
        c.read()
    c.cmd("SET", "keep", "v")
    time.sleep(0.2 + 2.3)  # expiry + 2 s reclaim window + slack
    eq(c.cmd("DBSIZE"), 1, "expired keys reclaimed within 2 s without access")


@test(own_accounting=True)
def active_expiry_memory():
    c = fresh()
    eq(info_field(c, "used_memory"), 0)
    for i in range(1000):
        c.send_raw(c.encode("SET", f"m{i}", "x" * 100, "PX", "100"))
    for _ in range(1000):
        c.read()
    time.sleep(0.1 + 2.3)
    eq(info_field(c, "used_memory"), 0, "used_memory after reclaim")


@test
def keys_glob():
    c = fresh()
    for k in ["hello", "hallo", "hxllo", "hllo", "heeello", "h*llo", "a[b", "x?y", "abc"]:
        c.cmd("SET", k, "1")

    def keys(p):
        return sorted(x.decode() for x in c.cmd("KEYS", p))

    eq(keys("h?llo"), ["h*llo", "hallo", "hello", "hxllo"])
    eq(keys("h*llo"), ["h*llo", "hallo", "heeello", "hello", "hllo", "hxllo"])
    eq(keys("h[ae]llo"), ["hallo", "hello"])
    eq(keys("h[^e]llo"), ["h*llo", "hallo", "hxllo"])
    eq(keys("h[a-b]llo"), ["hallo"])
    eq(keys("h\\*llo"), ["h*llo"])
    eq(keys("a\\[b"), ["a[b"])
    eq(keys("x\\?y"), ["x?y"])
    eq(keys("*"), sorted(["hello", "hallo", "hxllo", "hllo", "heeello", "h*llo", "a[b", "x?y", "abc"]))
    eq(keys("nomatch*"), [])


# ---------------------------------------------------------------- lists


@test
def list_basic():
    c = fresh()
    eq(c.cmd("RPUSH", "l", "a", "b", "c"), 3)
    eq(c.cmd("LPUSH", "l", "x", "y"), 5)
    eq(c.cmd("LRANGE", "l", "0", "-1"), [b"y", b"x", b"a", b"b", b"c"])
    eq(c.cmd("LLEN", "l"), 5)
    eq(c.cmd("LINDEX", "l", "0"), b"y")
    eq(c.cmd("LINDEX", "l", "-1"), b"c")
    eq(c.cmd("LINDEX", "l", "99"), None)
    eq(c.cmd("LPOP", "l"), b"y")
    eq(c.cmd("RPOP", "l"), b"c")
    eq(c.cmd("LPOP", "l", "2"), [b"x", b"a"])
    eq(c.cmd("RPOP", "l", "5"), [b"b"])
    eq(c.cmd("EXISTS", "l"), 0, "empty list deleted")
    eq(c.cmd("LPOP", "l"), None)
    eq(c.cmd("LPOP", "l", "2"), None)
    eq(c.cmd("LLEN", "l"), 0)
    is_err(c.cmd("LPOP", "l", "x"), "ERR value is out of range, must be positive")
    is_err(c.cmd("LPOP", "l", "-1"), "ERR value is out of range, must be positive")
    c.cmd("SET", "s", "v")
    is_err(c.cmd("LPUSH", "s", "a"), "WRONGTYPE")
    is_err(c.cmd("LRANGE", "s", "0", "1"), "WRONGTYPE")
    is_err(c.cmd("RPUSH", "l"), "ERR wrong number of arguments for 'rpush' command")


@test
def lrange_indexes():
    c = fresh()
    c.cmd("RPUSH", "l", *[str(i) for i in range(10)])

    def r(a, b):
        return [int(x) for x in c.cmd("LRANGE", "l", str(a), str(b))]

    eq(r(0, 2), [0, 1, 2])
    eq(r(-3, -1), [7, 8, 9])
    eq(r(-100, 1), [0, 1])
    eq(r(8, 100), [8, 9])
    eq(r(5, 2), [])
    eq(r(20, 30), [])
    eq(r(0, -11), [])
    eq(c.cmd("LRANGE", "nope", "0", "-1"), [])


# ---------------------------------------------------------------- hashes


@test
def hash_basic():
    c = fresh()
    eq(c.cmd("HSET", "h", "a", "1", "b", "2"), 2)
    eq(c.cmd("HSET", "h", "a", "10", "c", "3"), 1)
    eq(c.cmd("HGET", "h", "a"), b"10")
    eq(c.cmd("HGET", "h", "zz"), None)
    eq(c.cmd("HGET", "nope", "a"), None)
    eq(c.cmd("HLEN", "h"), 3)
    eq(c.cmd("HEXISTS", "h", "b"), 1)
    eq(c.cmd("HEXISTS", "h", "zz"), 0)
    flat = c.cmd("HGETALL", "h")
    eq(dict(zip(flat[::2], flat[1::2])), {b"a": b"10", b"b": b"2", b"c": b"3"})
    eq(c.cmd("HGETALL", "nope"), [])
    eq(c.cmd("HDEL", "h", "a", "b", "zz"), 2)
    eq(c.cmd("HDEL", "h", "c"), 1)
    eq(c.cmd("EXISTS", "h"), 0, "empty hash deleted")
    is_err(c.cmd("HSET", "h", "a"), "ERR wrong number of arguments for 'hset' command")
    c.cmd("SET", "s", "v")
    is_err(c.cmd("HGET", "s", "a"), "WRONGTYPE")


@test
def hincrby():
    c = fresh()
    eq(c.cmd("HINCRBY", "h", "n", "5"), 5)
    eq(c.cmd("HINCRBY", "h", "n", "-7"), -2)
    c.cmd("HSET", "h", "s", "abc")
    is_err(c.cmd("HINCRBY", "h", "s", "1"), "ERR hash value is not an integer")
    is_err(c.cmd("HINCRBY", "h", "n", "x"), "ERR value is not an integer or out of range")
    c.cmd("HSET", "h", "m", str(2**63 - 1))
    is_err(c.cmd("HINCRBY", "h", "m", "1"), "ERR increment or decrement would overflow")


# ---------------------------------------------------------------- pub/sub


@test
def pubsub_basic():
    c = fresh()
    sub = C()
    eq(sub.cmd("SUBSCRIBE", "a", "b"), [b"subscribe", b"a", 1])
    eq(sub.read(), [b"subscribe", b"b", 2])
    eq(c.cmd("PUBLISH", "a", "hello"), 1)
    eq(c.cmd("PUBLISH", "zz", "x"), 0)
    eq(sub.read(), [b"message", b"a", b"hello"])
    is_err(sub.cmd("GET", "k"), "ERR Can't execute 'get'")
    eq(sub.cmd("PING"), [b"pong", b""])
    eq(sub.cmd("UNSUBSCRIBE", "a"), [b"unsubscribe", b"a", 1])
    eq(c.cmd("PUBLISH", "a", "x"), 0)
    eq(sub.cmd("UNSUBSCRIBE"), [b"unsubscribe", b"b", 0])
    eq(sub.cmd("GET", "k"), None, "normal mode after unsubscribing all")
    eq(sub.cmd("UNSUBSCRIBE"), [b"unsubscribe", None, 0])
    eq(sub.cmd("PING"), "PONG")


@test
def pubsub_fanout_order():
    pub = C()
    subs = [C() for _ in range(20)]
    for s in subs:
        eq(s.cmd("SUBSCRIBE", "ch"), [b"subscribe", b"ch", 1])
    n = 2000
    payload = b"".join(pub.encode("PUBLISH", "ch", f"m{i}") for i in range(n))
    pub.send_raw(payload)
    for i in range(n):
        eq(pub.read(), 20, "receivers")
    for s in subs:
        for i in range(n):
            eq(s.read(), [b"message", b"ch", f"m{i}".encode()], "message order")


@test
def pubsub_disconnect_cleanup():
    pub = C()
    sub = C()
    sub.cmd("SUBSCRIBE", "gone")
    sub.close()
    deadline = time.time() + 2
    while pub.cmd("PUBLISH", "gone", "x") != 0:
        if time.time() > deadline:
            raise AssertionError("closed subscriber still counted after 2 s")
        time.sleep(0.05)


@test
def slow_subscriber_does_not_block():
    pub = C()
    slow = C()
    slow.cmd("SUBSCRIBE", "flood")  # never reads again
    other = C()
    big = "x" * 64 * 1024
    t0 = time.time()
    for i in range(300):  # ~19 MB to a subscriber that does not read
        pub.cmd("PUBLISH", "flood", big)
        if i % 50 == 0:
            eq(other.cmd("PING"), "PONG")
    if time.time() - t0 > 10:
        raise AssertionError("publishing to a slow subscriber is too slow")
    slow.close()


# ---------------------------------------------------------------- concurrency


def parallel(n, fn):
    errors = []

    def run(i):
        try:
            fn(i)
        except Exception as e:  # noqa: BLE001
            errors.append(e)

    ts = [threading.Thread(target=run, args=(i,)) for i in range(n)]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    if errors:
        raise errors[0]


@test
def concurrent_incr():
    fresh()

    def work(_):
        c = C()
        payload = b"".join(c.encode("INCR", "ctr") for _ in range(1000))
        c.send_raw(payload)
        for _ in range(1000):
            c.read()

    parallel(50, work)
    eq(C().cmd("GET", "ctr"), b"50000")


@test
def concurrent_lists_hashes():
    fresh()

    def work(i):
        c = C()
        for j in range(200):
            c.cmd("RPUSH", "biglist", f"{i}:{j}", f"{i}:{j}b")
            c.cmd("HINCRBY", "hh", "n", "1")
            c.cmd("HSET", "hh", f"f{i}-{j}", "v")

    parallel(30, work)
    c = C()
    eq(c.cmd("LLEN", "biglist"), 30 * 200 * 2)
    items = c.cmd("LRANGE", "biglist", "0", "-1")
    pos = {x: k for k, x in enumerate(items)}
    for i in range(30):
        for j in range(200):
            eq(pos[f"{i}:{j}b".encode()], pos[f"{i}:{j}".encode()] + 1, "multi-value RPUSH is atomic")
    eq(c.cmd("HGET", "hh", "n"), b"6000")
    eq(c.cmd("HLEN", "hh"), 6001)


@test
def mset_atomic():
    fresh()
    stop = threading.Event()
    bad = []

    def writer(i):
        c = C()
        for j in range(500):
            v = f"{i}-{j}"
            c.cmd("MSET", "x", v, "y", v, "z", v)
        stop.set()

    def reader(_):
        c = C()
        while not stop.is_set():
            r = c.cmd("MGET", "x", "y", "z")
            if len(set(r)) != 1:
                bad.append(r)
                return

    ws = [threading.Thread(target=writer, args=(i,)) for i in range(4)]
    rs = [threading.Thread(target=reader, args=(i,)) for i in range(4)]
    for t in ws + rs:
        t.start()
    for t in ws + rs:
        t.join()
    if bad:
        raise AssertionError(f"MGET observed partial MSET: {bad[0]}")


# ---------------------------------------------------------------- memory accounting


@test(own_accounting=True)
def accounting_exact():
    c = fresh()
    eq(info_field(c, "used_memory"), 0)
    c.cmd("SET", "key", "value")  # 64 + 3 + 5
    eq(info_field(c, "used_memory"), 72)
    c.cmd("APPEND", "key", "xx")
    eq(info_field(c, "used_memory"), 74)
    c.cmd("RPUSH", "lst", "a", "bcd")  # 64 + 3 + (16+1) + (16+3)
    eq(info_field(c, "used_memory"), 74 + 103)
    c.cmd("HSET", "h", "f", "vv")  # 64 + 1 + 32 + 1 + 2
    eq(info_field(c, "used_memory"), 74 + 103 + 100)
    c.cmd("HSET", "h", "f", "v")
    eq(info_field(c, "used_memory"), 74 + 103 + 99)
    c.cmd("LPOP", "lst")
    eq(info_field(c, "used_memory"), 74 + 86 + 99)
    c.cmd("DEL", "key", "lst", "h")
    eq(info_field(c, "used_memory"), 0)
    c.cmd("INCR", "n")  # "1"
    eq(info_field(c, "used_memory"), 66)
    c.cmd("INCRBY", "n", "99")  # "100"
    eq(info_field(c, "used_memory"), 68)
    c.cmd("FLUSHALL")
    eq(info_field(c, "used_memory"), 0)


# ---------------------------------------------------------------- eviction (own server instance)


class Server:
    def __init__(self, port, maxmemory):
        self.port = port
        if CTX["real"]:
            self.p = subprocess.Popen(
                ["redis-server", "--port", str(port), "--save", "", "--appendonly", "no",
                 "--maxmemory", str(maxmemory), "--maxmemory-policy", "allkeys-lru"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        else:
            env = dict(os.environ, PORT=str(port), MAXMEMORY=str(maxmemory))
            self.p = subprocess.Popen([CTX["bin"]], env=env,
                                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.time() + 10
        while True:
            try:
                Conn(port).close()
                break
            except OSError:
                if time.time() > deadline:
                    self.stop()
                    raise AssertionError("eviction server did not start")
                time.sleep(0.05)

    def stop(self):
        self.p.kill()
        self.p.wait()




@test(needs_bin=True)
def eviction_lru_quality():
    # each key: 64 + len("k00000") + 100 = 170 bytes; limit fits exactly 10 000
    cost = 64 + 6 + 100
    limit = cost * 10_000
    if CTX["real"]:
        limit = 40 * 1024 * 1024  # real redis: approximate by allocator memory, sized below
    srv = Server(CTX["evict_port"], limit)
    try:
        c = Conn(CTX["evict_port"])
        val = "v" * 100
        if CTX["real"]:
            # find how many keys fit, then treat that as the "10 000"
            base = info_field(c, "used_memory")
            c.send_raw(b"".join(c.encode("SET", f"k{i:05d}", val) for i in range(1000)))
            for _ in range(1000):
                c.read()
            per = (info_field(c, "used_memory") - base) / 1000
            c.cmd("FLUSHALL")
            c.cmd("CONFIG", "SET", "maxmemory", str(int(base + per * 10_000)))
        c.send_raw(b"".join(c.encode("SET", f"k{i:05d}", val) for i in range(10_000)))
        for _ in range(10_000):
            eq(c.read(), "OK")
        time.sleep(0.01)
        c.send_raw(b"".join(c.encode("GET", f"k{i:05d}") for i in range(1000)))
        for _ in range(1000):
            c.read()
        c.send_raw(b"".join(c.encode("SET", f"n{i:05d}", val) for i in range(2000)))
        for _ in range(2000):
            eq(c.read(), "OK")
        survived = c.cmd("EXISTS", *[f"k{i:05d}" for i in range(1000)])
        if survived < 950:
            raise AssertionError(f"only {survived}/1000 recently used keys survived eviction")
        if not CTX["real"]:
            used = info_field(c, "used_memory")
            if used > limit:
                raise AssertionError(f"used_memory {used} > maxmemory {limit}")
            if used < limit - 10 * cost:
                raise AssertionError(f"evicted too much: used_memory {used}, limit {limit}")
            n = c.cmd("DBSIZE")
            if not 9_990 <= n <= 10_000:
                raise AssertionError(f"DBSIZE {n}, expected ~10000")
    finally:
        srv.stop()


@test(needs_bin=True, own_accounting=True)
def eviction_limit_and_oom():
    limit = 10_000
    srv = Server(CTX["evict_port"], limit)
    try:
        c = Conn(CTX["evict_port"])
        for i in range(200):
            eq(c.cmd("SET", f"key{i}", "x" * 50), "OK")
            used = info_field(c, "used_memory")
            if used > limit:
                raise AssertionError(f"after SET #{i}: used_memory {used} > {limit}")
        for i in range(150):  # list alone: 64 + 7 + 150 * 56 = 8471 bytes, fits
            c.cmd("RPUSH", "biglist", "y" * 40)
            if info_field(c, "used_memory") > limit:
                raise AssertionError("used_memory above limit after RPUSH")
        eq(c.cmd("LLEN", "biglist"), 150, "a command never evicts its own key")
        before = c.cmd("DBSIZE")
        is_err(c.cmd("SET", "huge", "z" * 20_000), "OOM command not allowed when used memory > 'maxmemory'.")
        eq(c.cmd("EXISTS", "huge"), 0)
        eq(c.cmd("DBSIZE"), before, "failed OOM write must not evict")
    finally:
        srv.stop()


# ---------------------------------------------------------------- main


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=6380)
    ap.add_argument("--real-redis", action="store_true")
    ap.add_argument("--bin", help="server binary, for eviction tests that start their own instance")
    ap.add_argument("--evict-port", type=int, default=6390, help="port for the instances started by eviction tests")
    ap.add_argument("-k", default="")
    a = ap.parse_args()
    CTX.update(port=a.port, real=a.real_redis, bin=a.bin, evict_port=a.evict_port)

    passed = failed = skipped = 0
    for t in TESTS:
        if a.k not in t.__name__:
            continue
        if (a.real_redis and t.own_accounting) or (t.needs_bin and not (a.bin or a.real_redis)):
            print(f"SKIP {t.__name__}")
            skipped += 1
            continue
        t0 = time.time()
        try:
            t()
            print(f"ok   {t.__name__} ({time.time() - t0:.2f}s)")
            passed += 1
        except Exception as e:  # noqa: BLE001
            print(f"FAIL {t.__name__}: {e}")
            if not isinstance(e, AssertionError):
                traceback.print_exc()
            failed += 1
    print(f"\n{passed} passed, {failed} failed, {skipped} skipped")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
