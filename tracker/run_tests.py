#!/usr/bin/env python3
"""Acceptance tests for the Tracker service (see SPEC.md).

Usage: run_tests.py <path-to-binary> [--db NAME] [-k substring] [-v]

Black-box over HTTP. Needs the `tracker-postgres` container (docker compose up -d). For every test group the
runner drops and recreates the `public` schema of the database, starts the service with PORT / DATABASE_URL /
JWT_SECRET / WEBHOOK_BACKOFF_SCALE, runs the group's tests, then stops it with SIGTERM and expects exit code 0.
Webhooks are received by a local HTTP server in this process. Python 3 standard library only.
"""
import argparse
import base64
import concurrent.futures as cf
import hashlib
import hmac
import http.client
import http.server
import itertools
import json
import os
import random
import re
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import traceback
import unicodedata
import urllib.parse
import uuid
from datetime import datetime, timedelta, timezone

JWT_SECRET = "acceptance-tests-jwt-secret-7f3a9c2e"
PG_CONTAINER = "tracker-postgres"
PG_URL = "postgres://tracker:tracker@127.0.0.1:55432/{db}"
BACKOFF_SCALE = 0.05
PW = "correct horse battery"

TS_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$")
UUID_RE = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
TYPES = ["task", "bug", "story"]
PRIORITIES = ["highest", "high", "medium", "low", "lowest"]
PRI_RANK = {"lowest": 0, "low": 1, "medium": 2, "high": 3, "highest": 4}
EVENTS = ["issue.created", "issue.updated", "issue.deleted", "comment.created"]
ISSUE_FIELDS = {"id", "key", "number", "project_key", "type", "title", "description", "status", "priority",
                "assignee_id", "reporter_id", "labels", "due_date", "version", "comment_count", "created_at",
                "updated_at", "resolved_at"}

REQ = {"required"}
EMPTY = {"required", "too_short"}            # empty / blank strings
LONG = {"too_long"}
INV = {"invalid"}
PATTERN = {"invalid", "too_short", "too_long"}  # pattern-constrained fields that also have a length
ANY = None                                     # any code


# ----------------------------------------------------------------------------------------------------------------
# small utilities

def b64u(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def b64u_dec(s):
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def jwt_encode(claims, secret=JWT_SECRET, header=None):
    h = b64u(json.dumps(header or {"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
    p = b64u(json.dumps(claims, separators=(",", ":")).encode())
    sig = "" if secret is None else b64u(hmac.new(secret.encode(), f"{h}.{p}".encode(), hashlib.sha256).digest())
    return f"{h}.{p}.{sig}"


def jwt_decode(tok, secret=JWT_SECRET):
    """-> (header, claims, signature_valid)"""
    h, p, s = tok.split(".")
    good = hmac.compare_digest(b64u(hmac.new(secret.encode(), f"{h}.{p}".encode(), hashlib.sha256).digest()), s)
    return json.loads(b64u_dec(h)), json.loads(b64u_dec(p)), good


def parse_ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


def ts_ms(s):
    d = parse_ts(s)
    return int(round(d.timestamp() * 1000))


def ms_to_iso(ms):
    d = datetime.fromtimestamp(ms // 1000, timezone.utc)
    return d.strftime("%Y-%m-%dT%H:%M:%S.") + f"{ms % 1000:03d}Z"


def is_ts(v):
    return isinstance(v, str) and bool(TS_RE.match(v))


def is_uuid(v):
    return isinstance(v, str) and bool(UUID_RE.match(v))


def short(v, n=400):
    s = v if isinstance(v, str) else json.dumps(v, ensure_ascii=False, default=str)
    return s if len(s) <= n else s[:n] + f"... ({len(s)} chars)"


def qs(params):
    if not params:
        return ""
    return "?" + urllib.parse.urlencode(params, quote_via=urllib.parse.quote, safe=",")


def words(text):
    """Maximal runs of Unicode letters and digits, lower-cased (SPEC §6)."""
    out, cur = [], []
    for ch in text or "":
        cat = unicodedata.category(ch)
        if cat[0] == "L" or cat == "Nd":
            cur.append(ch)
        elif cur:
            out.append("".join(cur).lower())
            cur = []
    if cur:
        out.append("".join(cur).lower())
    return out


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def long_email(n):
    dom = ""
    total = n - 61 - 4
    while len(dom) < total:
        dom += "." if len(dom) % 41 == 40 else "d"
    if dom.endswith("."):
        dom = dom[:-1] + "d"
    e = "x" * 60 + "@" + dom + ".com"
    assert len(e) == n, len(e)
    return e


class Bad(Exception):
    """A precondition of a test failed (unexpected response while setting up)."""


def must(r, status, what):
    if r.status != status:
        raise Bad(f"{what}: expected status {status}, got {r.status}: {r.text[:300]}")
    return r.json


# ----------------------------------------------------------------------------------------------------------------
# database

def psql(db, sql):
    p = subprocess.run(["docker", "exec", "-i", PG_CONTAINER, "psql", "-U", "tracker", "-d", db,
                        "-v", "ON_ERROR_STOP=1", "-qtAc", sql], capture_output=True, text=True, timeout=60)
    if p.returncode != 0:
        raise RuntimeError(f"psql failed ({db}): {p.stderr.strip() or p.stdout.strip()}")
    return p.stdout.strip()


def ensure_db(db):
    if not re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", db):
        raise RuntimeError(f"invalid database name {db!r}")
    if psql("postgres", f"SELECT 1 FROM pg_database WHERE datname = '{db}'") != "1":
        psql("postgres", f'CREATE DATABASE "{db}"')


def reset_db(db):
    psql(db, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "
             "WHERE datname = current_database() AND pid <> pg_backend_pid()")
    psql(db, "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;")


# ----------------------------------------------------------------------------------------------------------------
# HTTP client

class Resp:
    def __init__(self, status, headers, body):
        self.status = status
        self.headers = {k.lower(): v for k, v in headers}
        self.body = body
        self.text = body.decode("utf-8", "replace")
        try:
            self.json = json.loads(self.text) if body else None
        except ValueError:
            self.json = None

    def get(self, k, default=None):
        return self.json.get(k, default) if isinstance(self.json, dict) else default

    def __repr__(self):
        return f"<{self.status} {self.text[:200]}>"


class Api:
    def __init__(self, svc):
        self.svc = svc

    def req(self, method, path, body=None, token=None, headers=None, raw=None, timeout=30):
        if not (path.startswith("/healthz") or path.startswith("/readyz")):
            path = "/api/v1" + path
        h = {}
        data = None
        if raw is not None:
            data = raw
            h["Content-Type"] = "application/json"
        elif body is not None:
            data = json.dumps(body, ensure_ascii=False).encode()
            h["Content-Type"] = "application/json"
        if token is not None:
            tok = token.token if isinstance(token, User) else token
            h["Authorization"] = f"Bearer {tok}"
        if headers:
            h.update(headers)
        conn = http.client.HTTPConnection("127.0.0.1", self.svc.port, timeout=timeout)
        for i in range(5):  # connection setup only (nothing sent yet), so retrying is safe
            try:
                conn.connect()
                break
            except (ConnectionResetError, ConnectionRefusedError):
                if i == 4:
                    raise
                conn.close()
                time.sleep(0.05 * (i + 1))
        try:
            conn.request(method, path, body=data, headers=h)
            r = conn.getresponse()
            return Resp(r.status, r.getheaders(), r.read())
        finally:
            conn.close()

    def get(self, path, token=None, **kw):
        return self.req("GET", path, token=token, **kw)

    def post(self, path, body=None, token=None, **kw):
        return self.req("POST", path, body=body, token=token, **kw)

    def patch(self, path, body=None, token=None, **kw):
        return self.req("PATCH", path, body=body, token=token, **kw)

    def put(self, path, body=None, token=None, **kw):
        return self.req("PUT", path, body=body, token=token, **kw)

    def delete(self, path, token=None, **kw):
        return self.req("DELETE", path, token=token, **kw)


class User:
    def __init__(self, id, email, name, token, refresh):
        self.id, self.email, self.name, self.token, self.refresh = id, email, name, token, refresh

    def __repr__(self):
        return f"<User {self.email}>"


# ----------------------------------------------------------------------------------------------------------------
# the service under test

class ServiceError(Exception):
    pass


class Service:
    def __init__(self, binary, db, logpath):
        self.binary, self.db, self.logpath = binary, db, logpath
        self.proc = None
        self.port = None

    def start(self, extra_env=None, timeout=30):
        self.port = free_port()
        env = dict(os.environ)
        env.update(PORT=str(self.port), DATABASE_URL=PG_URL.format(db=self.db), JWT_SECRET=JWT_SECRET,
                   WEBHOOK_BACKOFF_SCALE=str(BACKOFF_SCALE))
        env.update(extra_env or {})
        with open(self.logpath, "ab") as f:
            f.write(f"\n===== start {time.strftime('%H:%M:%S')} port {self.port} {extra_env or ''} =====\n".encode())
        logf = open(self.logpath, "ab")
        self.proc = subprocess.Popen([self.binary], env=env, stdout=logf, stderr=subprocess.STDOUT,
                                     stdin=subprocess.DEVNULL, cwd=os.path.dirname(self.binary) or None)
        logf.close()
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.proc.poll() is not None:
                raise ServiceError(f"service exited with code {self.proc.returncode} during startup")
            try:
                conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=1)
                conn.request("GET", "/readyz")
                if conn.getresponse().status == 200:
                    conn.close()
                    return
                conn.close()
            except OSError:
                pass
            time.sleep(0.05)
        raise ServiceError(f"service not ready within {timeout}s")

    def alive(self):
        return self.proc is not None and self.proc.poll() is None

    def stop(self, timeout=15):
        """SIGTERM; returns the exit code, or None if it had to be killed."""
        if self.proc is None:
            return None
        p, self.proc = self.proc, None
        if p.poll() is not None:
            return p.returncode
        p.send_signal(signal.SIGTERM)
        try:
            return p.wait(timeout)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()
            return None

    def tail(self, n=40):
        try:
            with open(self.logpath, "rb") as f:
                lines = f.read().decode("utf-8", "replace").splitlines()
            return "\n".join("    | " + l for l in lines[-n:])
        except OSError:
            return "    | (no log)"


# ----------------------------------------------------------------------------------------------------------------
# webhook receiver

class Receiver:
    """Records every POST. `policies[path](rec, attempt)` returns a status or (status, delay_seconds)."""

    def __init__(self):
        self.records = []
        self.cond = threading.Condition()
        self.policies = {}
        self.port = None
        self.server = None
        self.up()

    def url(self, path):
        return f"http://127.0.0.1:{self.port}{path}"

    def up(self):
        rcv = self

        class H(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_POST(self):
                if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
                    raw = b""
                    while True:
                        size = int(self.rfile.readline().split(b";")[0].strip() or b"0", 16)
                        if size == 0:
                            while self.rfile.readline() not in (b"\r\n", b"\n", b""):
                                pass
                            break
                        raw += self.rfile.read(size)
                        self.rfile.readline()
                else:
                    n = int(self.headers.get("Content-Length") or 0)
                    raw = self.rfile.read(n) if n else b""
                try:
                    js = json.loads(raw)
                except ValueError:
                    js = None
                rec = {"t": time.monotonic(), "wall": time.time(), "path": self.path,
                       "headers": {k.lower(): v for k, v in self.headers.items()}, "raw": raw, "json": js}
                did = rec["headers"].get("x-tracker-delivery")
                with rcv.cond:
                    attempt = 1 + sum(1 for r in rcv.records if r["path"] == self.path
                                      and r["headers"].get("x-tracker-delivery") == did)
                    rec["attempt"] = attempt
                    pol = rcv.policies.get(self.path)
                    res = 200
                    if pol:
                        try:
                            res = pol(rec, attempt)
                        except Exception:  # noqa: BLE001
                            res = 200
                    status, delay = res if isinstance(res, tuple) else (res, 0)
                    rec["status"] = status
                    rcv.records.append(rec)
                    rcv.cond.notify_all()
                if delay:
                    time.sleep(delay)
                try:
                    self.send_response(status)
                    self.send_header("Content-Type", "text/plain")
                    self.send_header("Content-Length", "2")
                    self.end_headers()
                    self.wfile.write(b"ok")
                except OSError:
                    pass

            def log_message(self, *a):
                pass

        class Srv(http.server.ThreadingHTTPServer):
            daemon_threads = True

            def handle_error(self, request, client_address):
                pass

        srv = Srv(("127.0.0.1", self.port or 0), H)
        srv.daemon_threads = True
        self.port = srv.server_address[1]
        self.server = srv
        threading.Thread(target=srv.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True).start()

    def down(self):
        if self.server:
            self.server.shutdown()
            self.server.server_close()
            self.server = None

    def recs(self, path):
        with self.cond:
            return [r for r in self.records if r["path"] == path]

    def wait(self, pred, timeout=15.0):
        """Wait until pred(records) is truthy; returns its last value."""
        deadline = time.time() + timeout
        with self.cond:
            while True:
                res = pred(list(self.records))
                rem = deadline - time.time()
                if res or rem <= 0:
                    return res
                self.cond.wait(min(rem, 0.1))

    def wait_path(self, path, pred, timeout=15.0):
        return self.wait(lambda recs: pred([r for r in recs if r["path"] == path]), timeout)


def sig_ok(rec, secret):
    ts = rec["headers"].get("x-tracker-timestamp", "")
    want = "sha256=" + hmac.new(secret.encode(), ts.encode() + b"." + rec["raw"], hashlib.sha256).hexdigest()
    return hmac.compare_digest(rec["headers"].get("x-tracker-signature", "").lower(), want)


def successes(recs):
    return [r for r in recs if 200 <= r["status"] < 300]


def wait_until(fn, timeout=10.0, interval=0.05):
    deadline = time.time() + timeout
    while True:
        res = fn()
        if res or time.time() >= deadline:
            return res
        time.sleep(interval)


# ----------------------------------------------------------------------------------------------------------------
# checks

class Checker:
    def __init__(self, verbose):
        self.verbose = verbose
        self.passed = 0
        self.failed = 0
        self.prefix = ""
        self.lock = threading.Lock()

    def record(self, name, ok, detail=""):
        full = f"{self.prefix}: {name}" if self.prefix else name
        with self.lock:
            if ok:
                self.passed += 1
                if self.verbose:
                    print(f"ok {full}", flush=True)
            else:
                self.failed += 1
                print(f"FAIL {full}", flush=True)
                if detail:
                    for line in str(detail).splitlines():
                        print(f"    {line}", flush=True)
        return ok

    def ok(self, name, cond, detail=""):
        return self.record(name, bool(cond), detail)

    def eq(self, name, actual, expected):
        return self.record(name, actual == expected, f"expected: {short(expected)}\nactual:   {short(actual)}")

    def status(self, name, r, expected):
        exp = expected if isinstance(expected, (set, tuple, list)) else (expected,)
        return self.record(name, r.status in exp, f"expected status {expected}, got {r.status}: {r.text[:500]}")

    def err(self, name, r, status, code=None, field=None, fcodes=ANY, prefix=False):
        """Expect a problem+json error; optionally an `errors` entry for `field` with a code in `fcodes`."""
        probs = []
        if r.status != status:
            probs.append(f"expected status {status}, got {r.status}")
        else:
            ct = r.headers.get("content-type", "")
            if not ct.startswith("application/problem+json"):
                probs.append(f"Content-Type {ct!r}, expected application/problem+json")
            j = r.json
            if not isinstance(j, dict):
                probs.append("body is not a JSON object")
            else:
                if j.get("status") != status:
                    probs.append(f"body.status {j.get('status')!r}")
                if code and j.get("code") != code:
                    probs.append(f"code {j.get('code')!r}, expected {code!r}")
                if field is not None:
                    errs = j.get("errors")
                    if not isinstance(errs, list):
                        probs.append("no `errors` list")
                    else:
                        hits = [e for e in errs if isinstance(e, dict) and isinstance(e.get("field"), str) and
                                (e["field"] == field or (prefix and e["field"].startswith(field + ".")))]
                        if not hits:
                            probs.append(f"no errors entry for field {field!r}")
                        elif fcodes is not ANY and not any(e.get("code") in fcodes for e in hits):
                            probs.append(f"field {field!r} code {[e.get('code') for e in hits]}, expected one of "
                                         f"{sorted(fcodes)}")
        return self.record(name, not probs, "; ".join(probs) + f"\nbody: {r.text[:400]}")

    def vfail(self, name, r, field, fcodes=ANY, prefix=False):
        return self.err(name, r, 422, "validation_failed", field, fcodes, prefix)


# ----------------------------------------------------------------------------------------------------------------
# test context & registry

class Ctx:
    _n = itertools.count(1)

    def __init__(self, api, svc, t, rcv, group):
        self.api, self.svc, self.t, self.rcv, self.group = api, svc, t, rcv, group
        self.fx = None

    # -- users
    def register(self, email, name="Test User", password=PW):
        return self.api.post("/auth/register", {"email": email, "password": password, "name": name})

    def login(self, email, password=PW):
        return self.api.post("/auth/login", {"email": email, "password": password})

    def user(self, tag="u", name=None):
        n = next(Ctx._n)
        email = f"{tag}{n:04d}x{uuid.uuid4().hex[:6]}@example.com"
        j = must(self.register(email, name or f"User {tag}{n}"), 201, f"register {email}")
        lj = must(self.login(email), 200, f"login {email}")
        return User(j["id"], j["email"], j["name"], lj["access_token"], lj["refresh_token"])

    # -- orgs & projects
    def org(self, owner, slug=None, name="Test Org"):
        slug = slug or f"org{next(Ctx._n)}-{uuid.uuid4().hex[:6]}"
        must(self.api.post("/orgs", {"name": name, "slug": slug}, owner), 201, f"create org {slug}")
        return slug

    def add_member(self, by, slug, user, role="member"):
        return must(self.api.post(f"/orgs/{slug}/members", {"email": user.email, "role": role}, by), 201,
                    f"add {user} to {slug} as {role}")

    def project(self, user, slug, key, visibility="org", name=None):
        return must(self.api.post(f"/orgs/{slug}/projects",
                                  {"key": key, "name": name or f"Project {key}", "visibility": visibility}, user),
                    201, f"create project {key}")

    def pmember(self, by, slug, key, user, role):
        return must(self.api.put(f"/orgs/{slug}/projects/{key}/members/{user.id}", {"role": role}, by), 200,
                    f"set {user} {role} on {key}")

    def issue(self, user, slug, key, **fields):
        body = {"type": "task", "title": "An issue"}
        body.update(fields)
        return must(self.api.post(f"/orgs/{slug}/projects/{key}/issues", body, user), 201, f"create issue in {key}")

    def get_issue(self, user, slug, ikey):
        return must(self.api.get(f"/orgs/{slug}/issues/{ikey}", user), 200, f"get {ikey}")

    def patch_issue(self, user, slug, ikey, fields, version=None):
        if version is None:
            version = self.get_issue(user, slug, ikey)["version"]
        return self.api.patch(f"/orgs/{slug}/issues/{ikey}", fields, user, headers={"If-Match": f'"{version}"'})

    def transition(self, user, slug, ikey, status, headers=None):
        return self.api.post(f"/orgs/{slug}/issues/{ikey}/transition", {"status": status}, user, headers=headers)

    def walk(self, user, path, params=None, limit=None, max_pages=300):
        """Follow next_cursor; -> (items, pages, problems)."""
        items, pages, probs, cursor = [], [], [], None
        for _ in range(max_pages):
            p = dict(params or {})
            if limit is not None:
                p["limit"] = limit
            if cursor is not None:
                p["cursor"] = cursor
            r = self.api.get(path + qs(p), user)
            if r.status != 200:
                probs.append(f"page {len(pages) + 1}: status {r.status}: {r.text[:300]}")
                break
            j = r.json
            if not isinstance(j, dict) or not isinstance(j.get("items"), list) or "next_cursor" not in j:
                probs.append(f"page {len(pages) + 1}: bad page shape: {r.text[:300]}")
                break
            pages.append(j)
            if limit is not None and len(j["items"]) > limit:
                probs.append(f"page {len(pages)} has {len(j['items'])} items > limit {limit}")
            items.extend(j["items"])
            cursor = j["next_cursor"]
            if cursor is None:
                break
            if not isinstance(cursor, str):
                probs.append(f"next_cursor is {cursor!r}")
                break
        else:
            probs.append("too many pages")
        return items, pages, probs

    def restart(self, extra_env=None):
        code = self.svc.stop()
        self.svc.start(extra_env)
        return code


class Group:
    def __init__(self, name, env=None):
        self.name, self.env = name, env
        self.setup_fn = None
        self.tests = []

    def setup(self, fn):
        self.setup_fn = fn
        return fn

    def test(self, fn):
        self.tests.append(fn)
        return fn


GROUPS = []


def group(name, env=None):
    g = Group(name, env)
    GROUPS.append(g)
    return g


def problem_keys_ok(j, keys):
    return isinstance(j, dict) and all(k in j for k in keys)


# ================================================================================================================
# 1. health, conventions

G = group("health")


@G.test
def health_endpoints(c):
    t = c.t
    r = c.api.get("/healthz")
    t.status("GET /healthz → 200", r, 200)
    t.eq("GET /healthz body", r.json, {"status": "ok"})
    r = c.api.get("/readyz")
    t.status("GET /readyz → 200 with database up", r, 200)
    t.eq("GET /readyz body", r.json, {"status": "ok"})


@G.test
def auth_required(c):
    for method, path in [("GET", "/me"), ("PATCH", "/me"), ("GET", "/orgs"), ("POST", "/orgs"), ("GET", "/orgs/acme"),
                         ("GET", "/orgs/acme/members"), ("GET", "/orgs/acme/projects"),
                         ("POST", "/orgs/acme/projects/ABC/issues"), ("GET", "/orgs/acme/issues"),
                         ("GET", "/orgs/acme/issues/ABC-1"), ("GET", "/orgs/acme/webhooks")]:
        r = c.api.req(method, path, body={} if method in ("POST", "PATCH") else None)
        c.t.err(f"{method} {path} without token → 401 unauthenticated", r, 401, "unauthenticated")


@G.test
def malformed_json(c):
    r = c.api.post("/auth/register", raw=b'{"email": "a@b.c", ')
    c.t.err("malformed JSON on /auth/register → 400 bad_request", r, 400, "bad_request")
    u = c.user("mj")
    r = c.api.post("/orgs", raw=b"{nope", token=u)
    c.t.err("malformed JSON on POST /orgs → 400 bad_request", r, 400, "bad_request")


# ================================================================================================================
# 2. auth

G = group("auth")


@G.test
def register_ok(c):
    t = c.t
    r = c.api.post("/auth/register", {"email": "Alice.Smith@Example.COM", "password": PW, "name": "  Alice  ",
                                      "unknown_field": 1})
    t.status("register → 201 (unknown fields ignored)", r, 201)
    j = r.json or {}
    t.ok("register: id is a UUID", is_uuid(j.get("id")), r.text)
    t.eq("register: email stored lower-cased", j.get("email"), "alice.smith@example.com")
    t.eq("register: name trimmed", j.get("name"), "Alice")
    t.ok("register: created_at is RFC 3339 UTC with ms", is_ts(j.get("created_at")), j.get("created_at"))
    t.ok("register: response has no password material", not any("pass" in k.lower() for k in j), list(j))
    r = c.api.post("/auth/register", {"email": "ALICE.smith@example.com", "password": PW, "name": "Other"})
    t.err("register same email, different case → 409 email_taken", r, 409, "email_taken")
    for label, body in [("password of 10 chars", {"password": "p" * 10}), ("password of 128 chars",
                                                                          {"password": "p" * 128}),
                        ("name of 100 chars", {"name": "n" * 100}), ("name of 100 non-ASCII chars", {"name": "é" * 100}),
                        ("email of 254 chars", {"email": long_email(254)})]:
        b = {"email": f"ok{next(Ctx._n)}@example.com", "password": PW, "name": "Bob"}
        b.update(body)
        t.status(f"register accepts {label}", c.api.post("/auth/register", b), 201)


@G.test
def register_validation(c):
    def body(**kw):
        b = {"email": f"v{next(Ctx._n)}@example.com", "password": PW, "name": "Val"}
        for k, v in kw.items():
            if v is KeyError:
                b.pop(k)
            else:
                b[k] = v
        return b

    cases = [
        ("missing email", body(email=KeyError), "email", REQ),
        ("email without @", body(email="nobody.example.com"), "email", INV),
        ("email of 255 chars", body(email=long_email(255)), "email", {"too_long", "invalid"}),
        ("email not a string", body(email=42), "email", ANY),
        ("missing password", body(password=KeyError), "password", REQ),
        ("password of 9 chars", body(password="p" * 9), "password", {"too_short"}),
        ("password of 129 chars", body(password="p" * 129), "password", LONG),
        ("missing name", body(name=KeyError), "name", REQ),
        ("empty name", body(name=""), "name", EMPTY),
        ("blank name", body(name="   "), "name", EMPTY),
        ("name of 101 chars", body(name="n" * 101), "name", LONG),
    ]
    for label, b, field, codes in cases:
        c.t.vfail(f"register with {label} → 422 ({field})", c.api.post("/auth/register", b), field, codes)
    r = c.api.post("/auth/register", {"email": "nope", "password": "short", "name": ""})
    c.t.ok("register with three invalid fields lists all three in errors",
           r.status == 422 and {e.get("field") for e in (r.get("errors") or [])} >= {"email", "password", "name"},
           r.text)


@G.test
def login(c):
    t = c.t
    email = f"login{next(Ctx._n)}@example.com"
    uid = must(c.register(email.upper(), "Lo Gin"), 201, "register")["id"]
    r = c.login(email)
    t.status("login → 200", r, 200)
    j = r.json or {}
    t.eq("login: token_type", j.get("token_type"), "Bearer")
    t.eq("login: expires_in", j.get("expires_in"), 900)
    t.ok("login: access_token and refresh_token are non-empty strings",
         all(isinstance(j.get(k), str) and j.get(k) for k in ("access_token", "refresh_token")), r.text)
    t.status("login with upper-cased email → 200", c.login(email.upper()), 200)
    t.err("login with wrong password → 401 invalid_credentials", c.login(email, "wrong password!"), 401,
          "invalid_credentials")
    t.err("login with unknown email → 401 invalid_credentials", c.login("ghost@example.com"), 401,
          "invalid_credentials")
    tok = j.get("access_token", "x.y.z")
    try:
        hdr, claims, good = jwt_decode(tok)
    except Exception as e:  # noqa: BLE001
        t.ok("access token is a decodable JWT", False, f"{e}: {tok[:100]}")
        return
    t.ok("access token signature is HS256 with JWT_SECRET", good and hdr.get("alg") == "HS256", hdr)
    t.eq("access token sub = user id", claims.get("sub"), uid)
    t.eq("access token typ = access", claims.get("typ"), "access")
    t.ok("access token exp = iat + 900", isinstance(claims.get("iat"), int) and claims.get("exp") == claims["iat"] + 900,
         claims)
    t.ok("access token iat ≈ now", isinstance(claims.get("iat"), int) and abs(claims["iat"] - time.time()) < 60, claims)
    r = c.api.get("/me", tok)
    t.status("GET /me with the access token → 200", r, 200)
    t.eq("GET /me returns the user", (r.get("id"), r.get("email"), r.get("name")), (uid, email.lower(), "Lo Gin"))


@G.test
def forged_tokens(c):
    t = c.t
    u = c.user("jwt")
    now = int(time.time())
    good = {"sub": u.id, "iat": now, "exp": now + 900, "typ": "access"}
    r = c.api.get("/me", jwt_encode(good))
    t.status("token forged with JWT_SECRET is accepted", r, 200)
    t.eq("forged token resolves to its sub", r.get("id"), u.id)
    exp = dict(good, iat=now - 1000, exp=now - 100)
    t.err("expired token → 401 token_expired", c.api.get("/me", jwt_encode(exp)), 401, "token_expired")
    t.err("token signed with another secret → 401 unauthenticated",
          c.api.get("/me", jwt_encode(good, secret="not-the-secret")), 401, "unauthenticated")
    t.err("token with typ=refresh → 401 unauthenticated",
          c.api.get("/me", jwt_encode(dict(good, typ="refresh"))), 401, "unauthenticated")
    t.err("unsigned alg=none token → 401 unauthenticated",
          c.api.get("/me", jwt_encode(good, secret=None, header={"alg": "none", "typ": "JWT"})), 401, "unauthenticated")
    h, p, s = jwt_encode(good).split(".")
    other = b64u(json.dumps(dict(good, sub=str(uuid.uuid4()))).encode())
    t.err("token with tampered payload → 401 unauthenticated", c.api.get("/me", f"{h}.{other}.{s}"), 401,
          "unauthenticated")
    t.err("garbage bearer token → 401 unauthenticated", c.api.get("/me", "not-a-jwt"), 401, "unauthenticated")
    t.err("non-Bearer Authorization → 401 unauthenticated",
          c.api.get("/me", headers={"Authorization": f"Basic {base64.b64encode(b'a:b').decode()}"}), 401,
          "unauthenticated")


@G.test
def refresh_rotation(c):
    t = c.t
    u = c.user("rf")
    r = c.api.post("/auth/refresh", {"refresh_token": u.refresh})
    t.status("refresh → 200", r, 200)
    j = r.json or {}
    t.ok("refresh returns a new pair", isinstance(j.get("access_token"), str) and isinstance(j.get("refresh_token"), str)
         and j.get("refresh_token") != u.refresh, r.text)
    t.eq("refresh: token_type and expires_in", (j.get("token_type"), j.get("expires_in")), ("Bearer", 900))
    t.status("new access token works", c.api.get("/me", j.get("access_token", "")), 200)
    r2 = j.get("refresh_token", "")
    r3 = must(c.api.post("/auth/refresh", {"refresh_token": r2}), 200, "second refresh")["refresh_token"]
    # independent login = independent chain
    other = must(c.login(u.email), 200, "second login")["refresh_token"]
    t.err("reusing a used refresh token → 401 token_reused", c.api.post("/auth/refresh", {"refresh_token": u.refresh}),
          401, "token_reused")
    t.err("after reuse, the latest token of the chain is revoked → 401 invalid_token",
          c.api.post("/auth/refresh", {"refresh_token": r3}), 401, "invalid_token")
    t.status("a refresh token from a different login is unaffected by reuse detection",
             c.api.post("/auth/refresh", {"refresh_token": other}), 200)
    t.err("unknown refresh token → 401 invalid_token",
          c.api.post("/auth/refresh", {"refresh_token": "definitely-not-a-token"}), 401, "invalid_token")


@G.test
def logout(c):
    t = c.t
    u = c.user("lo")
    r2 = must(c.api.post("/auth/refresh", {"refresh_token": u.refresh}), 200, "refresh")["refresh_token"]
    other = must(c.login(u.email), 200, "second login")["refresh_token"]
    r = c.api.post("/auth/logout", {"refresh_token": r2})
    t.status("logout → 204", r, 204)
    t.err("refresh with a logged-out token → 401 invalid_token", c.api.post("/auth/refresh", {"refresh_token": r2}),
          401, "invalid_token")
    t.status("logout does not affect another login's chain", c.api.post("/auth/refresh", {"refresh_token": other}),
             200)


@G.test
def me(c):
    t = c.t
    u = c.user("me", name="Me Myself")
    r = c.api.get("/me", u)
    t.status("GET /me → 200", r, 200)
    t.eq("GET /me fields", (r.get("id"), r.get("email"), r.get("name")), (u.id, u.email, "Me Myself"))
    t.ok("GET /me created_at format", is_ts(r.get("created_at")), r.text)
    r = c.api.patch("/me", {"name": "  Renamed  "}, u)
    t.status("PATCH /me → 200", r, 200)
    t.eq("PATCH /me trims and returns the new name", r.get("name"), "Renamed")
    t.eq("GET /me after PATCH shows the new name", c.api.get("/me", u).get("name"), "Renamed")
    t.vfail("PATCH /me with empty name → 422", c.api.patch("/me", {"name": ""}, u), "name", EMPTY)
    t.vfail("PATCH /me with 101-char name → 422", c.api.patch("/me", {"name": "x" * 101}, u), "name", LONG)


# ================================================================================================================
# 3. organizations

G = group("orgs")


@G.setup
def orgs_setup(c):
    return {"O": c.user("oo"), "X": c.user("ox")}


@G.test
def create_org(c):
    t = c.t
    O = c.fx["O"]
    r = c.api.post("/orgs", {"name": "Acme Corp", "slug": "acme"}, O)
    t.status("POST /orgs → 201", r, 201)
    j = r.json or {}
    t.eq("org fields", (j.get("slug"), j.get("name"), j.get("my_role")), ("acme", "Acme Corp", "owner"))
    t.ok("org id is UUID, created_at is a timestamp", is_uuid(j.get("id")) and is_ts(j.get("created_at")), r.text)
    t.err("duplicate slug → 409 slug_taken", c.api.post("/orgs", {"name": "Other", "slug": "acme"}, c.fx["X"]), 409,
          "slug_taken")
    r = c.api.get("/orgs/acme", O)
    t.status("GET /orgs/{slug} as owner → 200", r, 200)
    t.eq("GET /orgs/{slug} my_role", r.get("my_role"), "owner")
    t.err("GET /orgs/{slug} as non-member → 404", c.api.get("/orgs/acme", c.fx["X"]), 404, "not_found")
    t.err("GET unknown org → 404", c.api.get("/orgs/no-such-org", O), 404, "not_found")
    for slug in ["abc", "a-b", "0rg", "x-1", "a--b", "a" * 40]:
        t.status(f"slug {slug!r} is valid", c.api.post("/orgs", {"name": "N", "slug": slug}, O), 201)
    t.status("org name of 100 chars is valid", c.api.post("/orgs", {"name": "n" * 100, "slug": "name-100"}, O), 201)


@G.test
def org_validation(c):
    O = c.fx["O"]
    cases = [("ab", PATTERN), ("a" * 41, PATTERN), ("Acme", PATTERN), ("-acme", PATTERN), ("acme-", PATTERN),
             ("ac_me", PATTERN), ("ac me", PATTERN), ("acmé", PATTERN)]
    for slug, codes in cases:
        c.t.vfail(f"slug {slug!r} → 422", c.api.post("/orgs", {"name": "N", "slug": slug}, O), "slug", codes)
    c.t.vfail("missing slug → 422 required", c.api.post("/orgs", {"name": "N"}, O), "slug", REQ)
    c.t.vfail("missing name → 422 required", c.api.post("/orgs", {"slug": "noname"}, O), "name", REQ)
    c.t.vfail("empty name → 422", c.api.post("/orgs", {"name": "", "slug": "emptyname"}, O), "name", EMPTY)
    c.t.vfail("name of 101 chars → 422", c.api.post("/orgs", {"name": "n" * 101, "slug": "longname"}, O), "name", LONG)


@G.test
def list_orgs(c):
    t = c.t
    u = c.user("lst")
    other = c.user("lsto")
    for s in ["zeta-l", "alpha-l", "mid-l"]:
        c.org(u, s)
    c.org(other, "beta-l")  # u is not a member
    c.org(other, "gamma-l")
    c.add_member(other, "gamma-l", u)
    items, _, probs = c.walk(u, "/orgs", limit=1)
    t.ok("walking GET /orgs with limit=1 works", not probs, probs)
    t.eq("GET /orgs lists exactly the caller's orgs sorted by slug", [o.get("slug") for o in items],
         ["alpha-l", "gamma-l", "mid-l", "zeta-l"])
    t.eq("GET /orgs my_role per org", [o.get("my_role") for o in items], ["owner", "member", "owner", "owner"])


@G.test
def members(c):
    t = c.t
    O = c.user("mo")
    A, M, N = c.user("ma"), c.user("mm"), c.user("mn")
    slug = c.org(O)
    r = c.api.post(f"/orgs/{slug}/members", {"email": A.email.upper(), "role": "admin"}, O)
    t.status("owner adds a member by (case-insensitive) email → 201", r, 201)
    t.eq("added member representation", (r.get("user_id"), r.get("email"), r.get("name"), r.get("role")),
         (A.id, A.email, A.name, "admin"))
    t.status("admin adds a member with role member → 201",
             c.api.post(f"/orgs/{slug}/members", {"email": M.email, "role": "member"}, A), 201)
    t.err("adding an existing member → 409 already_member",
          c.api.post(f"/orgs/{slug}/members", {"email": M.email, "role": "member"}, O), 409, "already_member")
    t.vfail("adding an unknown email → 422 email invalid",
            c.api.post(f"/orgs/{slug}/members", {"email": "nobody@nowhere.example", "role": "member"}, O), "email", INV)
    t.vfail("adding with an invalid role → 422",
            c.api.post(f"/orgs/{slug}/members", {"email": N.email, "role": "boss"}, O), "role", ANY)
    t.err("plain member adding a member → 403",
          c.api.post(f"/orgs/{slug}/members", {"email": N.email, "role": "member"}, M), 403, "forbidden")
    t.err("non-member adding a member → 404",
          c.api.post(f"/orgs/{slug}/members", {"email": N.email, "role": "member"}, N), 404, "not_found")
    items, _, probs = c.walk(M, f"/orgs/{slug}/members", limit=2)
    t.ok("a plain member can list members", not probs, probs)
    t.eq("members are sorted by email", [m.get("email") for m in items], sorted([O.email, A.email, M.email]))
    t.eq("member list roles", {m.get("user_id"): m.get("role") for m in items},
         {O.id: "owner", A.id: "admin", M.id: "member"})
    t.ok("member entries have user_id, email, name, role",
         all(problem_keys_ok(m, ["user_id", "email", "name", "role"]) for m in items), items)
    t.err("non-member listing members → 404", c.api.get(f"/orgs/{slug}/members", N), 404, "not_found")


@G.test
def roles(c):
    t = c.t
    O, A, A2, M, M2, O2 = c.user("ro"), c.user("ra"), c.user("rb"), c.user("rm"), c.user("rn"), c.user("rq")
    slug = c.org(O)
    c.add_member(O, slug, O2, "owner")
    c.add_member(O, slug, A, "admin")
    c.add_member(O, slug, A2, "admin")
    c.add_member(O, slug, M)
    c.add_member(O, slug, M2)
    base = f"/orgs/{slug}/members"
    t.err("admin promoting a member to admin → 403", c.api.patch(f"{base}/{M.id}", {"role": "admin"}, A), 403,
          "forbidden")
    t.err("admin promoting a member to owner → 403", c.api.patch(f"{base}/{M.id}", {"role": "owner"}, A), 403,
          "forbidden")
    t.err("admin demoting another admin → 403", c.api.patch(f"{base}/{A2.id}", {"role": "member"}, A), 403,
          "forbidden")
    t.err("admin demoting an owner → 403", c.api.patch(f"{base}/{O.id}", {"role": "member"}, A), 403, "forbidden")
    t.status("an owner removes another owner → 204", c.api.delete(f"{base}/{O2.id}", O), 204)
    t.err("member changing roles → 403", c.api.patch(f"{base}/{M2.id}", {"role": "member"}, M), 403, "forbidden")
    t.vfail("invalid role → 422", c.api.patch(f"{base}/{M.id}", {"role": "superuser"}, O), "role", ANY)
    t.err("PATCH unknown member → 404", c.api.patch(f"{base}/{uuid.uuid4()}", {"role": "member"}, O), 404,
          "not_found")
    r = c.api.patch(f"{base}/{M.id}", {"role": "admin"}, O)
    t.status("owner promotes member to admin → 200", r, 200)
    t.status("owner demotes admin to member → 200", c.api.patch(f"{base}/{A2.id}", {"role": "member"}, O), 200)
    t.eq("GET /orgs/{slug} reflects new role", c.api.get(f"/orgs/{slug}", M).get("my_role"), "admin")
    t.err("sole owner demoting themselves → 409 last_owner", c.api.patch(f"{base}/{O.id}", {"role": "admin"}, O), 409,
          "last_owner")
    t.err("sole owner leaving → 409 last_owner", c.api.delete(f"{base}/{O.id}", O), 409, "last_owner")
    t.status("owner makes another owner → 200", c.api.patch(f"{base}/{A.id}", {"role": "owner"}, O), 200)
    t.status("with two owners, an owner can step down → 200", c.api.patch(f"{base}/{O.id}", {"role": "member"}, O),
             200)
    t.eq("former owner is now member", c.api.get(f"/orgs/{slug}", O).get("my_role"), "member")
    t.err("the remaining owner cannot demote themselves → 409 last_owner",
          c.api.patch(f"{base}/{A.id}", {"role": "admin"}, A), 409, "last_owner")


@G.test
def remove_member(c):
    t = c.t
    O, A, M, M2, M3 = c.user("do"), c.user("da"), c.user("dm"), c.user("dn"), c.user("dp")
    slug = c.org(O)
    c.add_member(O, slug, A, "admin")
    for u in (M, M2, M3):
        c.add_member(O, slug, u)
    base = f"/orgs/{slug}/members"
    t.err("member removing another member → 403", c.api.delete(f"{base}/{M2.id}", M), 403, "forbidden")
    t.status("admin removes a member → 204", c.api.delete(f"{base}/{M2.id}", A), 204)
    t.err("removed member can no longer see the org → 404", c.api.get(f"/orgs/{slug}", M2), 404, "not_found")
    t.status("member removes themselves → 204", c.api.delete(f"{base}/{M3.id}", M3), 204)
    t.err("self-removed member can no longer see the org", c.api.get(f"/orgs/{slug}", M3), 404, "not_found")
    t.status("removing a user who is not a member → 404 (or idempotent 204)", c.api.delete(f"{base}/{M3.id}", O),
             (404, 204))
    items, _, _ = c.walk(O, base, limit=100)
    t.eq("member list after removals", sorted(m.get("user_id") for m in items), sorted([O.id, A.id, M.id]))


@G.test
def remove_member_cleanup(c):
    t = c.t
    O, R = c.user("co"), c.user("cr")
    slug = c.org(O)
    c.add_member(O, slug, R)
    c.project(O, slug, "PUBL")
    c.project(O, slug, "PRIV", visibility="private")
    c.pmember(O, slug, "PUBL", R, "viewer")
    c.pmember(O, slug, "PRIV", R, "developer")
    i1 = c.issue(O, slug, "PUBL", assignee_id=R.id)
    i2 = c.issue(O, slug, "PRIV", assignee_id=R.id)
    i3 = c.issue(O, slug, "PUBL", assignee_id=O.id)
    t.status("owner removes a member with project roles and assigned issues → 204",
             c.api.delete(f"/orgs/{slug}/members/{R.id}", O), 204)
    for k in ("PUBL", "PRIV"):
        items, _, _ = c.walk(O, f"/orgs/{slug}/projects/{k}/members", limit=100)
        t.ok(f"project {k} memberships of the removed user are gone", R.id not in [m.get("user_id") for m in items],
             items)
    t.eq("issues assigned to the removed user are unassigned",
         [c.get_issue(O, slug, i["key"]).get("assignee_id") for i in (i1, i2)], [None, None])
    t.eq("other assignments are untouched", c.get_issue(O, slug, i3["key"]).get("assignee_id"), O.id)
    c.add_member(O, slug, R)
    t.err("re-added member does not regain the private-project membership",
          c.api.get(f"/orgs/{slug}/projects/PRIV", R), 404, "not_found")


# ================================================================================================================
# 4. projects and access

G = group("projects")


@G.setup
def projects_setup(c):
    f = {k: c.user("p" + k.lower()) for k in ("O", "A", "M", "V", "D", "X")}
    slug = c.org(f["O"], "acme")
    c.add_member(f["O"], slug, f["A"], "admin")
    for k in ("M", "V", "D"):
        c.add_member(f["O"], slug, f[k])
    c.project(f["O"], slug, "PUB")
    c.project(f["O"], slug, "PRIV", visibility="private")
    c.pmember(f["O"], slug, "PRIV", f["V"], "viewer")
    c.pmember(f["O"], slug, "PRIV", f["D"], "developer")
    f["slug"] = slug
    return f


@G.test
def create_project(c):
    t = c.t
    f = c.fx
    r = c.api.post("/orgs/acme/projects", {"key": "MPRJ", "name": "Member project"}, f["M"])
    t.status("plain org member creates a project → 201", r, 201)
    j = r.json or {}
    t.eq("project fields", (j.get("key"), j.get("name"), j.get("description"), j.get("visibility"), j.get("my_role")),
         ("MPRJ", "Member project", None, "org", "admin"))
    t.ok("project id/created_at", is_uuid(j.get("id")) and is_ts(j.get("created_at")), r.text)
    items, _, _ = c.walk(f["M"], "/orgs/acme/projects/MPRJ/members", limit=100)
    t.eq("creator is an explicit project admin", [(m.get("user_id"), m.get("role")) for m in items],
         [(f["M"].id, "admin")])
    r = c.api.post("/orgs/acme/projects", {"key": "DESC", "name": "With desc", "description": "Hello",
                                           "visibility": "private"}, f["O"])
    t.status("create with description and private visibility → 201", r, 201)
    t.eq("description/visibility echoed", (r.get("description"), r.get("visibility")), ("Hello", "private"))
    t.err("duplicate key in the same org → 409 key_taken",
          c.api.post("/orgs/acme/projects", {"key": "MPRJ", "name": "Again"}, f["O"]), 409, "key_taken")
    other = c.org(f["X"])
    t.status("same key in another org → 201", c.api.post(f"/orgs/{other}/projects", {"key": "MPRJ", "name": "X"},
                                                          f["X"]), 201)
    t.err("non-member creating a project → 404", c.api.post("/orgs/acme/projects", {"key": "XX", "name": "X"},
                                                             f["X"]), 404, "not_found")
    for key in ["AB", "A1", "ABCDEFGHIJ", "Z9Z9"]:
        t.status(f"key {key!r} is valid", c.api.post("/orgs/acme/projects", {"key": key, "name": "K"}, f["O"]), 201)


@G.test
def project_validation(c):
    O = c.fx["O"]
    for key in ["A", "ABCDEFGHIJK", "abc", "1AB", "AB-1", "A B", "ÄB"]:
        c.t.vfail(f"key {key!r} → 422", c.api.post("/orgs/acme/projects", {"key": key, "name": "N"}, O), "key", PATTERN)
    c.t.vfail("missing key → 422 required", c.api.post("/orgs/acme/projects", {"name": "N"}, O), "key", REQ)
    c.t.vfail("empty name → 422", c.api.post("/orgs/acme/projects", {"key": "EMPTYN", "name": ""}, O), "name", EMPTY)
    c.t.vfail("name of 101 chars → 422",
              c.api.post("/orgs/acme/projects", {"key": "LONGN", "name": "n" * 101}, O), "name", LONG)
    c.t.vfail("visibility 'public' → 422",
              c.api.post("/orgs/acme/projects", {"key": "VISX", "name": "N", "visibility": "public"}, O), "visibility",
              ANY)


@G.test
def visibility(c):
    t = c.t
    f = c.fx
    for who, role in [("O", "admin"), ("A", "admin"), ("V", "viewer"), ("D", "developer")]:
        r = c.api.get("/orgs/acme/projects/PRIV", f[who])
        t.eq(f"private project: {who} sees it with my_role {role}", (r.status, r.get("my_role")), (200, role))
    t.err("private project: org member without membership → 404", c.api.get("/orgs/acme/projects/PRIV", f["M"]), 404,
          "not_found")
    r = c.api.get("/orgs/acme/projects/PUB", f["M"])
    t.eq("org-visible project: plain org member is developer", (r.status, r.get("my_role")), (200, "developer"))
    t.eq("org-visible project: org admin is admin", c.api.get("/orgs/acme/projects/PUB", f["A"]).get("my_role"),
         "admin")
    t.err("org-visible project: non-member → 404", c.api.get("/orgs/acme/projects/PUB", f["X"]), 404, "not_found")
    t.err("unknown project → 404", c.api.get("/orgs/acme/projects/NOPE", f["O"]), 404, "not_found")
    for who, expect in [("M", ["PUB"]), ("V", ["PRIV", "PUB"]), ("O", ["PRIV", "PUB"])]:
        items, _, probs = c.walk(f[who], "/orgs/acme/projects", limit=1)
        keys = [p.get("key") for p in items if p.get("key") in ("PUB", "PRIV")]
        t.ok(f"project list for {who}: walk ok", not probs, probs)
        t.eq(f"project list for {who} contains exactly the visible ones of PUB/PRIV, sorted", keys, expect)
    items, _, _ = c.walk(f["O"], "/orgs/acme/projects", limit=3)
    keys = [p.get("key") for p in items]
    t.eq("project list is sorted by key, each once", keys, sorted(set(keys)))
    t.err("non-member listing projects → 404", c.api.get("/orgs/acme/projects", f["X"]), 404, "not_found")


@G.test
def effective_role(c):
    t = c.t
    f = c.fx
    O, M = f["O"], f["M"]
    c.project(O, "acme", "EFF")
    c.pmember(O, "acme", "EFF", M, "viewer")
    t.eq("explicit viewer + org-visible project → effective developer",
         c.api.get("/orgs/acme/projects/EFF", M).get("my_role"), "developer")
    t.status("…and can create issues", c.api.post("/orgs/acme/projects/EFF/issues", {"type": "task", "title": "x"}, M),
             201)
    r = c.api.patch("/orgs/acme/projects/EFF", {"visibility": "private"}, O)
    t.status("project admin makes the project private → 200", r, 200)
    t.eq("visibility is private", r.get("visibility"), "private")
    t.eq("explicit viewer of a private project → viewer",
         c.api.get("/orgs/acme/projects/EFF", M).get("my_role"), "viewer")
    t.err("…and can no longer create issues → 403",
          c.api.post("/orgs/acme/projects/EFF/issues", {"type": "task", "title": "x"}, M), 403, "forbidden")
    t.status("…but can comment", c.api.post("/orgs/acme/issues/EFF-1/comments", {"body": "hi"}, M), 201)
    t.status("removing the explicit membership → 204", c.api.delete(f"/orgs/acme/projects/EFF/members/{M.id}", O), 204)
    t.err("now the project is invisible → 404", c.api.get("/orgs/acme/projects/EFF", M), 404, "not_found")
    t.err("…and so are its issues", c.api.get("/orgs/acme/issues/EFF-1", M), 404, "not_found")


@G.test
def permissions_matrix(c):
    t = c.t
    f = c.fx
    O = f["O"]
    iss = c.issue(O, "acme", "PRIV", title="Private issue")
    k = iss["key"]
    cm = must(c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": "by owner"}, O), 201, "comment")
    hdr = {"If-Match": '"1"'}
    # viewer
    V = f["V"]
    t.status("viewer: GET issue → 200", c.api.get(f"/orgs/acme/issues/{k}", V), 200)
    t.status("viewer: list comments → 200", c.api.get(f"/orgs/acme/issues/{k}/comments", V), 200)
    t.status("viewer: history → 200", c.api.get(f"/orgs/acme/issues/{k}/history", V), 200)
    t.status("viewer: list project members → 200", c.api.get("/orgs/acme/projects/PRIV/members", V), 200)
    t.err("viewer: create issue → 403", c.api.post("/orgs/acme/projects/PRIV/issues", {"type": "task", "title": "x"}, V),
          403, "forbidden")
    t.err("viewer: bulk create → 403",
          c.api.post("/orgs/acme/projects/PRIV/issues/bulk", {"issues": [{"type": "task", "title": "x"}]}, V), 403,
          "forbidden")
    t.err("viewer: patch issue → 403", c.api.patch(f"/orgs/acme/issues/{k}", {"title": "y"}, V, headers=hdr), 403,
          "forbidden")
    t.err("viewer: transition → 403", c.transition(V, "acme", k, "in_progress"), 403, "forbidden")
    t.err("viewer: delete issue → 403", c.api.delete(f"/orgs/acme/issues/{k}", V), 403, "forbidden")
    t.err("viewer: patch project → 403", c.api.patch("/orgs/acme/projects/PRIV", {"name": "n"}, V), 403, "forbidden")
    t.err("viewer: manage members → 403",
          c.api.put(f"/orgs/acme/projects/PRIV/members/{f['M'].id}", {"role": "viewer"}, V), 403, "forbidden")
    t.status("viewer: comment → 201", c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": "viewer says"}, V), 201)
    # developer
    D = f["D"]
    t.status("developer: create issue → 201",
             c.api.post("/orgs/acme/projects/PRIV/issues", {"type": "task", "title": "dev"}, D), 201)
    t.status("developer: patch issue → 200", c.api.patch(f"/orgs/acme/issues/{k}", {"title": "dev edit"}, D,
                                                          headers=hdr), 200)
    t.status("developer: transition → 200", c.transition(D, "acme", k, "in_progress"), 200)
    t.err("developer: delete issue → 403", c.api.delete(f"/orgs/acme/issues/{k}", D), 403, "forbidden")
    t.err("developer: patch project → 403", c.api.patch("/orgs/acme/projects/PRIV", {"name": "n"}, D), 403,
          "forbidden")
    t.err("developer: delete project → 403", c.api.delete("/orgs/acme/projects/PRIV", D), 403, "forbidden")
    t.err("developer: manage members → 403",
          c.api.put(f"/orgs/acme/projects/PRIV/members/{f['M'].id}", {"role": "viewer"}, D), 403, "forbidden")
    # no access: org member without membership, and outsider
    for who in ("M", "X"):
        u = f[who]
        lbl = "org member without access" if who == "M" else "non-member"
        checks = [
            ("GET issue", c.api.get(f"/orgs/acme/issues/{k}", u)),
            ("PATCH issue", c.api.patch(f"/orgs/acme/issues/{k}", {"title": "z"}, u, headers={"If-Match": '"3"'})),
            ("transition", c.transition(u, "acme", k, "todo")),
            ("DELETE issue", c.api.delete(f"/orgs/acme/issues/{k}", u)),
            ("comment", c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": "x"}, u)),
            ("list comments", c.api.get(f"/orgs/acme/issues/{k}/comments", u)),
            ("history", c.api.get(f"/orgs/acme/issues/{k}/history", u)),
            ("edit comment", c.api.patch(f"/orgs/acme/comments/{cm['id']}", {"body": "x"}, u)),
            ("delete comment", c.api.delete(f"/orgs/acme/comments/{cm['id']}", u)),
            ("create issue", c.api.post("/orgs/acme/projects/PRIV/issues", {"type": "task", "title": "x"}, u)),
            ("list project members", c.api.get("/orgs/acme/projects/PRIV/members", u)),
            ("PATCH project", c.api.patch("/orgs/acme/projects/PRIV", {"name": "x"}, u)),
        ]
        for what, r in checks:
            t.err(f"{lbl}: {what} on private project → 404", r, 404, "not_found")
    # org admin (no explicit membership) is project admin
    t.status("org admin: patch project → 200", c.api.patch("/orgs/acme/projects/PRIV", {"name": "Private!"}, f["A"]),
             200)
    t.status("org admin: delete issue → 204", c.api.delete(f"/orgs/acme/issues/{k}", f["A"]), 204)


@G.test
def patch_delete_project(c):
    t = c.t
    f = c.fx
    O, D = f["O"], f["D"]
    c.project(O, "acme", "PD")
    c.pmember(O, "acme", "PD", D, "developer")
    r = c.api.patch("/orgs/acme/projects/PD", {"name": "Renamed", "description": "Now described"}, O)
    t.status("PATCH project → 200", r, 200)
    t.eq("PATCH project result", (r.get("key"), r.get("name"), r.get("description"), r.get("visibility")),
         ("PD", "Renamed", "Now described", "org"))
    r = c.api.patch("/orgs/acme/projects/PD", {"description": None}, O)
    t.eq("PATCH project description null clears it; name unchanged", (r.status, r.get("description"), r.get("name")),
         (200, None, "Renamed"))
    t.vfail("PATCH project empty name → 422", c.api.patch("/orgs/acme/projects/PD", {"name": ""}, O), "name", EMPTY)
    t.vfail("PATCH project bad visibility → 422", c.api.patch("/orgs/acme/projects/PD", {"visibility": "all"}, O),
            "visibility", ANY)
    i1 = c.issue(O, "acme", "PD")
    t.err("developer DELETE project → 403", c.api.delete("/orgs/acme/projects/PD", D), 403, "forbidden")
    t.status("project admin DELETE project → 204", c.api.delete("/orgs/acme/projects/PD", O), 204)
    t.err("deleted project → 404", c.api.get("/orgs/acme/projects/PD", O), 404, "not_found")
    t.err("issues of a deleted project → 404", c.api.get(f"/orgs/acme/issues/{i1['key']}", O), 404, "not_found")
    items, _, _ = c.walk(O, "/orgs/acme/issues", limit=100)
    t.ok("issues of a deleted project are not listed", i1["id"] not in [i.get("id") for i in items], "")


@G.test
def project_members(c):
    t = c.t
    f = c.fx
    O, M, D, V, X = f["O"], f["M"], f["D"], f["V"], f["X"]
    c.project(M, "acme", "PM", visibility="private")   # M is explicit admin (but only org member)
    base = "/orgs/acme/projects/PM/members"
    r = c.api.put(f"{base}/{D.id}", {"role": "developer"}, M)
    t.status("project admin (plain org member) adds a member → 200", r, 200)
    t.status("PUT again changes the role → 200", c.api.put(f"{base}/{D.id}", {"role": "viewer"}, M), 200)
    t.status("org owner manages project members → 200", c.api.put(f"{base}/{V.id}", {"role": "developer"}, O), 200)
    items, _, probs = c.walk(M, base, limit=1)
    t.ok("walk project members", not probs, probs)
    t.eq("project members: explicit only, sorted by email, with roles",
         [(m.get("user_id"), m.get("role")) for m in items],
         sorted([(M.id, "admin"), (D.id, "viewer"), (V.id, "developer")],
                key=lambda p: {M.id: M.email, D.id: D.email, V.id: V.email}[p[0]]))
    t.ok("project member entries have user_id, email, name, role",
         all(problem_keys_ok(m, ["user_id", "email", "name", "role"]) for m in items), items)
    r = c.api.put(f"{base}/{X.id}", {"role": "viewer"}, M)
    t.err("PUT a user who is not an org member → 422", r, 422, "validation_failed")
    r = c.api.put(f"{base}/{uuid.uuid4()}", {"role": "viewer"}, M)
    t.err("PUT an unknown user id → 422", r, 422, "validation_failed")
    t.vfail("PUT with invalid role → 422", c.api.put(f"{base}/{D.id}", {"role": "owner"}, M), "role", ANY)
    t.err("developer managing members → 403", c.api.put(f"{base}/{D.id}", {"role": "admin"}, V), 403, "forbidden")
    t.status("DELETE project member → 204", c.api.delete(f"{base}/{D.id}", M), 204)
    t.err("removed member of a private project loses access", c.api.get("/orgs/acme/projects/PM", D), 404,
          "not_found")


@G.test
def assignee_rules(c):
    t = c.t
    f = c.fx
    O = f["O"]
    url = "/orgs/acme/projects/PRIV/issues"
    for who, ok in [("V", False), ("M", False), ("X", False), ("A", True), ("D", True), ("O", True)]:
        r = c.api.post(url, {"type": "task", "title": "assign", "assignee_id": f[who].id}, O)
        role = {"V": "explicit viewer", "M": "org member without access", "X": "non-member", "A": "org admin",
                "D": "explicit developer", "O": "org owner"}[who]
        if ok:
            t.eq(f"private project: assigning {role} → 201", (r.status, r.get("assignee_id")), (201, f[who].id))
        else:
            t.vfail(f"private project: assigning {role} → 422", r, "assignee_id", INV)
    r = c.api.post("/orgs/acme/projects/PUB/issues", {"type": "task", "title": "a", "assignee_id": f["M"].id}, O)
    t.eq("org project: assigning a plain org member (effective developer) → 201", (r.status, r.get("assignee_id")),
         (201, f["M"].id))


# ================================================================================================================
# 5. issues

G = group("issues")


@G.setup
def issues_setup(c):
    f = {k: c.user("i" + k.lower()) for k in ("O", "D", "V", "X")}
    c.org(f["O"], "acme")
    c.add_member(f["O"], "acme", f["D"])
    c.add_member(f["O"], "acme", f["V"])
    c.project(f["O"], "acme", "ISS")
    c.project(f["O"], "acme", "PRV", visibility="private")
    c.pmember(f["O"], "acme", "PRV", f["V"], "viewer")
    return f


def count_in_project(c, user, key):
    r = c.api.get("/orgs/acme/issues" + qs({"project": key, "limit": 1}), user)
    return r.get("total")


@G.test
def create_issue(c):
    t = c.t
    O, D = c.fx["O"], c.fx["D"]
    c.project(O, "acme", "CRE")
    r = c.api.post("/orgs/acme/projects/CRE/issues", {"type": "task", "title": "  First  ", "foo": "ignored"}, O)
    t.status("create minimal issue → 201", r, 201)
    j = r.json or {}
    t.ok("issue has all specified fields", set(j) >= ISSUE_FIELDS, sorted(ISSUE_FIELDS - set(j)))
    t.eq("new issue defaults",
         {k: j.get(k) for k in ("key", "number", "project_key", "type", "title", "description", "status", "priority",
                                "assignee_id", "reporter_id", "labels", "due_date", "version", "comment_count",
                                "resolved_at")},
         {"key": "CRE-1", "number": 1, "project_key": "CRE", "type": "task", "title": "First", "description": None,
          "status": "todo", "priority": "medium", "assignee_id": None, "reporter_id": O.id, "labels": [],
          "due_date": None, "version": 1, "comment_count": 0, "resolved_at": None})
    t.ok("issue id UUID, created_at/updated_at timestamps",
         is_uuid(j.get("id")) and is_ts(j.get("created_at")) and is_ts(j.get("updated_at")), r.text)
    body = {"type": "bug", "title": "Full", "description": "Details\nmore", "priority": "highest",
            "assignee_id": D.id, "labels": ["Backend", "backend", "UI", "api"], "due_date": "2028-02-29"}
    r = c.api.post("/orgs/acme/projects/CRE/issues", body, D)
    t.status("create full issue → 201", r, 201)
    t.eq("full issue fields",
         {k: r.get(k) for k in ("key", "number", "type", "title", "description", "priority", "assignee_id",
                                "reporter_id", "labels", "due_date")},
         {"key": "CRE-2", "number": 2, "type": "bug", "title": "Full", "description": "Details\nmore",
          "priority": "highest", "assignee_id": D.id, "reporter_id": D.id, "labels": ["api", "backend", "ui"],
          "due_date": "2028-02-29"})
    r = c.api.post("/orgs/acme/projects/CRE/issues", {"type": "story", "title": "Nulls", "description": None,
                                                      "assignee_id": None, "due_date": None, "labels": []}, O)
    t.eq("explicit nulls are accepted", (r.status, r.get("description"), r.get("assignee_id"), r.get("due_date")),
         (201, None, None, None))
    r = c.api.post("/orgs/acme/projects/CRE/issues", {"type": "task", "title": "x", "labels": ["a.b-c_d", "9lives"]}, O)
    t.eq("labels with . - _ and leading digit are valid", (r.status, sorted(r.get("labels") or [])),
         (201, ["9lives", "a.b-c_d"]))
    for label, extra in [("title of 255 chars", {"title": "t" * 255}), ("title of 255 non-ASCII chars",
                                                                         {"title": "é" * 255}),
                         ("description of 65536 chars", {"description": "d" * 65536}),
                         ("20 labels", {"labels": [f"l{i:02d}" for i in range(20)]}),
                         ("label of 50 chars", {"labels": ["a" * 50]}),
                         ("upper-case label of 50 chars", {"labels": ["A" * 50]})]:
        b = {"type": "task", "title": "ok"}
        b.update(extra)
        t.status(f"create accepts {label}", c.api.post("/orgs/acme/projects/CRE/issues", b, O), 201)
    r = c.api.post("/orgs/acme/projects/CRE/issues", {"type": "task", "title": "x", "labels": ["Zed", "alpha", "mid"]},
                   O)
    t.eq("labels are returned sorted and lower-cased", r.get("labels"), ["alpha", "mid", "zed"])
    t.err("create in unknown project → 404", c.api.post("/orgs/acme/projects/NOPE/issues", {"type": "task",
                                                                                          "title": "x"}, O), 404,
          "not_found")


@G.test
def create_validation(c):
    O, X = c.fx["O"], c.fx["X"]
    c.project(O, "acme", "VAL")
    url = "/orgs/acme/projects/VAL/issues"

    def b(**kw):
        d = {"type": "task", "title": "ok"}
        for k, v in kw.items():
            if v is KeyError:
                d.pop(k)
            else:
                d[k] = v
        return d

    cases = [
        ("missing type", b(type=KeyError), "type", REQ, False),
        ("type 'epic'", b(type="epic"), "type", INV, False),
        ("type not a string", b(type=3), "type", ANY, False),
        ("missing title", b(title=KeyError), "title", REQ, False),
        ("blank title", b(title="   "), "title", EMPTY, False),
        ("title of 256 chars", b(title="t" * 256), "title", LONG, False),
        ("title of 256 non-ASCII chars", b(title="é" * 256), "title", LONG, False),
        ("title null", b(title=None), "title", ANY, False),
        ("title not a string", b(title=12), "title", ANY, False),
        ("description of 65537 chars", b(description="d" * 65537), "description", LONG, False),
        ("priority 'urgent'", b(priority="urgent"), "priority", INV, False),
        ("label with bad first char", b(labels=["ok", "-bad"]), "labels", INV, True),
        ("label with a space", b(labels=["two words"]), "labels", INV, True),
        ("empty label", b(labels=[""]), "labels", ANY, True),
        ("label of 51 chars", b(labels=["a" * 51]), "labels", {"too_long", "invalid"}, True),
        ("21 labels", b(labels=[f"l{i:02d}" for i in range(21)]), "labels", {"too_long", "invalid", "out_of_range"},
         True),
        ("labels not a list", b(labels="ui"), "labels", ANY, True),
        ("due_date 2026-02-30", b(due_date="2026-02-30"), "due_date", INV, False),
        ("due_date 2026/03/01", b(due_date="2026/03/01"), "due_date", INV, False),
        ("due_date as a timestamp", b(due_date="2026-03-01T00:00:00Z"), "due_date", INV, False),
        ("assignee_id not a UUID", b(assignee_id="nope"), "assignee_id", INV, False),
        ("assignee_id of an unknown user", b(assignee_id=str(uuid.uuid4())), "assignee_id", INV, False),
        ("assignee_id of a non-member", b(assignee_id=X.id), "assignee_id", INV, False),
    ]
    for label, body, field, codes, pref in cases:
        c.t.vfail(f"create with {label} → 422 ({field})", c.api.post(url, body, O), field, codes, pref)
    r = c.api.post(url, {"type": "epic", "title": ""}, O)
    c.t.ok("two invalid fields → two errors entries",
           r.status == 422 and {e.get("field") for e in (r.get("errors") or [])} >= {"type", "title"}, r.text)
    c.t.err("malformed JSON → 400 bad_request", c.api.post(url, raw=b'{"type": "task", "title": }', token=O), 400,
            "bad_request")
    r = c.api.post(url, {"type": "task", "title": "after failures"}, O)
    c.t.eq("failed creates consume no numbers", r.get("key"), "VAL-1")


@G.test
def numbering_concurrent(c):
    t = c.t
    O = c.fx["O"]
    c.project(O, "acme", "NUM")

    def single(i):
        return c.api.post("/orgs/acme/projects/NUM/issues", {"type": "task", "title": f"c{i}"}, O)

    def bulk(i):
        return c.api.post("/orgs/acme/projects/NUM/issues/bulk",
                          {"issues": [{"type": "task", "title": f"b{i}-{j}"} for j in range(20)]}, O)

    with cf.ThreadPoolExecutor(max_workers=33) as ex:
        futs = [ex.submit(single, i) for i in range(30)] + [ex.submit(bulk, i) for i in range(3)]
        res = [f.result() for f in futs]
    t.ok("30 concurrent creates + 3 concurrent bulk(20) all succeed", all(r.status == 201 for r in res),
         [r.status for r in res if r.status != 201][:5])
    nums = [r.get("number") for r in res[:30]]
    for r in res[30:]:
        nums += [int(k.split("-")[1]) for k in (r.get("keys") or [])]
    t.eq("numbers are exactly 1..90 with no gaps or duplicates", sorted(n for n in nums if isinstance(n, int)),
         list(range(1, 91)))
    for r in res[30:]:
        ks = [int(k.split("-")[1]) for k in (r.get("keys") or [])]
        t.ok("bulk keys are in request order (increasing)", ks == sorted(ks), ks)
    t.status("delete NUM-90", c.api.delete("/orgs/acme/issues/NUM-90", O), 204)
    t.eq("number of a deleted last issue is not reused", c.issue(O, "acme", "NUM").get("number"), 91)
    t.status("delete NUM-45", c.api.delete("/orgs/acme/issues/NUM-45", O), 204)
    t.eq("next number after deleting a middle issue", c.issue(O, "acme", "NUM").get("number"), 92)


@G.test
def idempotency(c):
    t = c.t
    O, D = c.fx["O"], c.fx["D"]
    c.project(O, "acme", "IDEM")
    url = "/orgs/acme/projects/IDEM/issues"
    raw = json.dumps({"type": "bug", "title": "Idempotent", "labels": ["x"]}).encode()
    h = {"Idempotency-Key": "key-123"}
    r1 = c.api.post(url, raw=raw, token=O, headers=h)
    t.status("first request with Idempotency-Key → 201", r1, 201)
    r2 = c.api.post(url, raw=raw, token=O, headers=h)
    t.eq("repeat with same key and body returns the original status and body", (r2.status, r2.json),
         (r1.status, r1.json))
    t.eq("…and created nothing", count_in_project(c, O, "IDEM"), 1)
    r = c.api.post(url, {"type": "bug", "title": "Different"}, O, headers=h)
    t.err("same key with a different body → 422 idempotency_key_reused", r, 422, "idempotency_key_reused")
    r3 = c.api.post(url, raw=raw, token=D, headers=h)
    t.ok("same key by another user is independent → 201 new issue",
         r3.status == 201 and r3.get("id") != r1.get("id"), r3.text)
    r4 = c.api.post(url, raw=raw, token=O, headers={"Idempotency-Key": "key-124"})
    t.ok("different key → new issue", r4.status == 201 and r4.get("id") != r1.get("id"), r4.text)
    r5 = c.api.post(url, raw=raw, token=O, headers={"Idempotency-Key": "k" * 256})
    t.ok("Idempotency-Key longer than 255 chars → 400 or 422", r5.status in (400, 422), f"{r5.status} {r5.text[:200]}")
    t.status("Idempotency-Key of 255 chars is accepted",
             c.api.post(url, raw=raw, token=O, headers={"Idempotency-Key": "k" * 255}), 201)
    before = count_in_project(c, O, "IDEM")
    craw = json.dumps({"type": "task", "title": "Concurrent idempotent"}).encode()
    with cf.ThreadPoolExecutor(max_workers=12) as ex:
        res = list(ex.map(lambda _: c.api.post(url, raw=craw, token=O, headers={"Idempotency-Key": "conc-1"}),
                          range(12)))
    ok = [r for r in res if r.status == 201]
    t.ok("concurrent same-key requests: at least one 201, others 201 or 4xx",
         ok and all(r.status == 201 or 400 <= r.status < 500 for r in res), [r.status for r in res])
    t.eq("concurrent same-key 201 responses all describe the same issue", len({r.get("id") for r in ok}), 1)
    t.eq("concurrent same-key requests create exactly one issue", count_in_project(c, O, "IDEM"), before + 1)
    nxt = c.issue(O, "acme", "IDEM")
    t.eq("no numbers were consumed by replays", nxt.get("number"), before + 2)


@G.test
def bulk(c):
    t = c.t
    O, X = c.fx["O"], c.fx["X"]
    c.project(O, "acme", "BLK")
    url = "/orgs/acme/projects/BLK/issues/bulk"
    r = c.api.post(url, {"issues": [{"type": "task", "title": "one"}, {"type": "bug", "title": "two",
                                                                         "labels": ["B", "a"]},
                                    {"type": "story", "title": "three", "priority": "low"}]}, O)
    t.status("bulk create → 201", r, 201)
    t.eq("bulk returns keys in request order", r.get("keys"), ["BLK-1", "BLK-2", "BLK-3"])
    got = [c.get_issue(O, "acme", k) for k in ["BLK-1", "BLK-2", "BLK-3"]]
    t.eq("bulk-created issues have the requested fields",
         [(i.get("title"), i.get("type"), i.get("labels"), i.get("priority")) for i in got],
         [("one", "task", [], "medium"), ("two", "bug", ["a", "b"], "medium"), ("three", "story", [], "low")])
    items = [{"type": "task", "title": f"ok{i}"} for i in range(6)]
    items[1] = {"type": "task"}
    items[3] = {"type": "nope", "title": "x"}
    items[5] = {"type": "task", "title": "x", "assignee_id": X.id}
    r = c.api.post(url, {"issues": items}, O)
    t.err("bulk with invalid items → 422", r, 422, "validation_failed")
    fields = {e.get("field") for e in (r.get("errors") or []) if isinstance(e, dict)}
    t.ok("bulk errors use indexed paths issues.1.title, issues.3.type, issues.5.assignee_id",
         {"issues.1.title", "issues.3.type", "issues.5.assignee_id"} <= fields, sorted(map(str, fields)))
    t.ok("bulk errors do not report valid items", not any(f and f.startswith(("issues.0.", "issues.2.", "issues.4."))
                                                          for f in fields), fields)
    t.eq("all-or-nothing: nothing was created", count_in_project(c, O, "BLK"), 3)
    t.vfail("bulk with empty list → 422", c.api.post(url, {"issues": []}, O), "issues", ANY)
    t.vfail("bulk with 1001 items → 422", c.api.post(url, {"issues": [{"type": "task", "title": "x"}] * 1001}, O),
            "issues", ANY)
    t.vfail("bulk without issues → 422", c.api.post(url, {}, O), "issues", ANY)
    t.eq("still nothing created", count_in_project(c, O, "BLK"), 3)
    r = c.api.post(url, {"issues": [{"type": "task", "title": f"m{i}"} for i in range(1000)]}, O)
    t.status("bulk with 1000 items → 201", r, 201)
    t.eq("1000 keys in order", r.get("keys"), [f"BLK-{i}" for i in range(4, 1004)])
    t.eq("next single create continues numbering", c.issue(O, "acme", "BLK").get("key"), "BLK-1004")


@G.test
def get_issue(c):
    t = c.t
    O = c.fx["O"]
    c.project(O, "acme", "GET")
    iss = c.issue(O, "acme", "GET", title="Get me")
    r = c.api.get("/orgs/acme/issues/GET-1", O)
    t.status("GET issue → 200", r, 200)
    t.eq("GET issue body equals create response", r.json, iss)
    t.eq('GET issue ETag is "<version>"', r.headers.get("etag"), '"1"')
    r = c.api.get("/orgs/acme/issues/get-1", O)
    t.eq("issue keys are case-insensitive in paths", (r.status, r.get("key")), (200, "GET-1"))
    t.err("unknown number → 404", c.api.get("/orgs/acme/issues/GET-999", O), 404, "not_found")
    t.err("unknown project key → 404", c.api.get("/orgs/acme/issues/NOPE-1", O), 404, "not_found")
    t.err("malformed key → 404", c.api.get("/orgs/acme/issues/nokey", O), 404, "not_found")
    other = c.org(O)
    t.err("issue looked up under another org → 404", c.api.get(f"/orgs/{other}/issues/GET-1", O), 404, "not_found")
    t.err("non-member → 404", c.api.get("/orgs/acme/issues/GET-1", c.fx["X"]), 404, "not_found")


@G.test
def patch_issue(c):
    t = c.t
    O, D, V, X = c.fx["O"], c.fx["D"], c.fx["V"], c.fx["X"]
    c.project(O, "acme", "PAT")
    iss = c.issue(O, "acme", "PAT", type="bug", title="Patch me", labels=["a", "b"], description="d",
                  due_date="2026-12-01", assignee_id=D.id)
    url = "/orgs/acme/issues/PAT-1"
    t.err("PATCH without If-Match → 428 precondition_required", c.api.patch(url, {"title": "x"}, O), 428,
          "precondition_required")
    t.err("PATCH with stale If-Match → 412 version_mismatch",
          c.api.patch(url, {"title": "x"}, O, headers={"If-Match": '"7"'}), 412, "version_mismatch")
    t.eq("rejected PATCHes changed nothing", (c.get_issue(O, "acme", "PAT-1").get("title"),
                                              c.get_issue(O, "acme", "PAT-1").get("version")), ("Patch me", 1))
    time.sleep(0.01)
    r = c.api.patch(url, {"title": "  Patched  ", "priority": "high"}, D, headers={"If-Match": '"1"'})
    t.status("PATCH with current If-Match → 200", r, 200)
    j = r.json or {}
    t.eq("PATCH applies fields (title trimmed) and bumps version by 1",
         (j.get("title"), j.get("priority"), j.get("version")), ("Patched", "high", 2))
    t.eq("absent fields are unchanged", {k: j.get(k) for k in ("type", "labels", "description", "due_date",
                                                               "assignee_id", "status", "reporter_id")},
         {"type": "bug", "labels": ["a", "b"], "description": "d", "due_date": "2026-12-01", "assignee_id": D.id,
          "status": "todo", "reporter_id": O.id})
    t.ok("updated_at advanced", is_ts(j.get("updated_at")) and ts_ms(j["updated_at"]) > ts_ms(iss["updated_at"]),
         (iss.get("updated_at"), j.get("updated_at")))
    t.eq("created_at unchanged", j.get("created_at"), iss.get("created_at"))
    t.eq("ETag follows the version", c.api.get(url, O).headers.get("etag"), '"2"')
    t.err("old version now stale → 412", c.api.patch(url, {"title": "x"}, O, headers={"If-Match": '"1"'}), 412,
          "version_mismatch")
    r = c.api.patch(url, {"title": "Patched", "labels": ["B", "A", "a"], "priority": "high"}, O,
                    headers={"If-Match": '"2"'})
    t.eq("no-op PATCH (incl. equivalent labels) → 200 without bumping version", (r.status, r.get("version")), (200, 2))
    t.eq("no-op PATCH keeps updated_at", r.get("updated_at"), j.get("updated_at"))
    r = c.api.patch(url, {}, O, headers={"If-Match": '"2"'})
    t.eq("empty PATCH → 200, version unchanged", (r.status, r.get("version")), (200, 2))
    r = c.api.patch(url, {"description": None, "due_date": None, "assignee_id": None, "labels": []}, O,
                    headers={"If-Match": '"2"'})
    t.eq("null clears nullable fields (one version bump)",
         (r.status, r.get("description"), r.get("due_date"), r.get("assignee_id"), r.get("labels"), r.get("version")),
         (200, None, None, None, [], 3))
    bad = [("status", {"status": "done"}, "status", ANY, False), ("title null", {"title": None}, "title", ANY, False),
           ("empty title", {"title": " "}, "title", EMPTY, False), ("type 'epic'", {"type": "epic"}, "type", INV, False),
           ("type null", {"type": None}, "type", ANY, False), ("priority null", {"priority": None}, "priority", ANY,
                                                                False),
           ("bad label", {"labels": ["Bad Label"]}, "labels", INV, True),
           ("bad due_date", {"due_date": "2026-13-01"}, "due_date", INV, False),
           ("non-member assignee", {"assignee_id": X.id}, "assignee_id", INV, False),
           ("title of 256 chars", {"title": "x" * 256}, "title", LONG, False)]
    for label, body, field, codes, pref in bad:
        t.vfail(f"PATCH with {label} → 422", c.api.patch(url, body, O, headers={"If-Match": '"3"'}), field, codes, pref)
    t.eq("failed PATCHes did not bump the version", c.get_issue(O, "acme", "PAT-1").get("version"), 3)
    r = c.api.patch("/orgs/acme/issues/pat-1", {"type": "story", "due_date": "2027-01-31", "assignee_id": O.id}, O,
                    headers={"If-Match": '"3"'})
    t.eq("PATCH via lower-case key", (r.status, r.get("type"), r.get("due_date"), r.get("assignee_id"),
                                      r.get("version")), (200, "story", "2027-01-31", O.id, 4))
    t.err("PATCH unknown issue → 404", c.api.patch("/orgs/acme/issues/PAT-99", {"title": "x"}, O,
                                                    headers={"If-Match": '"1"'}), 404, "not_found")
    c.issue(O, "acme", "PRV", title="private")
    t.err("viewer PATCH → 403", c.api.patch("/orgs/acme/issues/PRV-1", {"title": "x"}, V, headers={"If-Match": '"1"'}),
          403, "forbidden")


@G.test
def transitions(c):
    t = c.t
    O = c.fx["O"]
    c.project(O, "acme", "TRN")
    c.issue(O, "acme", "TRN", title="flow")
    k = "TRN-1"
    t.err("todo → done → 409 transition_not_allowed", c.transition(O, "acme", k, "done"), 409,
          "transition_not_allowed")
    r = c.transition(O, "acme", k, "in_progress")
    t.eq("todo → in_progress → 200", (r.status, r.get("status"), r.get("version"), r.get("resolved_at")),
         (200, "in_progress", 2, None))
    r = c.transition(O, "acme", k, "done")
    t.eq("in_progress → done → 200, version 3", (r.status, r.get("status"), r.get("version")), (200, "done", 3))
    t.ok("entering done sets resolved_at", is_ts(r.get("resolved_at")), r.text)
    first_resolved = r.get("resolved_at")
    t.err("done → todo → 409", c.transition(O, "acme", k, "todo"), 409, "transition_not_allowed")
    r = c.transition(O, "acme", k, "in_progress")
    t.eq("done → in_progress clears resolved_at", (r.status, r.get("status"), r.get("resolved_at")),
         (200, "in_progress", None))
    r = c.transition(O, "acme", k, "todo")
    t.eq("in_progress → todo → 200", (r.status, r.get("status"), r.get("version")), (200, "todo", 5))
    t.vfail("unknown status → 422", c.transition(O, "acme", k, "blocked"), "status", ANY)
    t.vfail("missing status → 422", c.api.post(f"/orgs/acme/issues/{k}/transition", {}, O), "status", REQ)
    t.err("transition with stale If-Match → 412", c.transition(O, "acme", k, "in_progress", {"If-Match": '"1"'}), 412,
          "version_mismatch")
    r = c.transition(O, "acme", k, "in_progress", {"If-Match": '"5"'})
    t.eq("transition with current If-Match → 200", (r.status, r.get("version")), (200, 6))
    time.sleep(0.01)
    r = c.transition(O, "acme", k.lower(), "done")
    t.eq("transition via lower-case key", (r.status, r.get("status")), (200, "done"))
    t.ok("re-entering done sets a new resolved_at", is_ts(r.get("resolved_at")) and first_resolved and
         ts_ms(r.json["resolved_at"]) > ts_ms(first_resolved), (first_resolved, r.get("resolved_at")))
    t.eq("GET shows the final state", (lambda j: (j.get("status"), j.get("version"), j.get("resolved_at")))(
        c.get_issue(O, "acme", k)), ("done", 7, r.get("resolved_at")))
    t.err("transition unknown issue → 404", c.transition(O, "acme", "TRN-42", "in_progress"), 404, "not_found")


@G.test
def delete_issue(c):
    t = c.t
    O, D = c.fx["O"], c.fx["D"]
    c.project(O, "acme", "DEL")
    c.issue(O, "acme", "DEL")
    c.issue(O, "acme", "DEL")
    t.err("developer DELETE issue → 403", c.api.delete("/orgs/acme/issues/DEL-2", D), 403, "forbidden")
    t.status("project admin DELETE issue → 204", c.api.delete("/orgs/acme/issues/del-2", O), 204)
    t.err("deleted issue → 404 on GET", c.api.get("/orgs/acme/issues/DEL-2", O), 404, "not_found")
    t.err("deleted issue → 404 on DELETE", c.api.delete("/orgs/acme/issues/DEL-2", O), 404, "not_found")
    t.err("deleted issue → 404 on comments", c.api.post("/orgs/acme/issues/DEL-2/comments", {"body": "x"}, O), 404,
          "not_found")
    t.err("deleted issue → 404 on history", c.api.get("/orgs/acme/issues/DEL-2/history", O), 404, "not_found")
    t.err("deleted issue → 404 on transition", c.transition(O, "acme", "DEL-2", "in_progress"), 404, "not_found")
    t.eq("number of deleted issue not reused", c.issue(O, "acme", "DEL").get("key"), "DEL-3")
    t.eq("listing no longer contains it", count_in_project(c, O, "DEL"), 2)


# ================================================================================================================
# 6. comments

G = group("comments")


@G.setup
def comments_setup(c):
    f = {k: c.user("c" + k.lower()) for k in ("O", "D", "V", "M")}
    c.org(f["O"], "acme")
    for k in ("D", "V", "M"):
        c.add_member(f["O"], "acme", f[k])
    c.project(f["O"], "acme", "CMT", visibility="private")
    c.pmember(f["O"], "acme", "CMT", f["V"], "viewer")
    c.pmember(f["O"], "acme", "CMT", f["D"], "developer")
    return f


@G.test
def create_and_list(c):
    t = c.t
    O, V, D, M = c.fx["O"], c.fx["V"], c.fx["D"], c.fx["M"]
    iss = c.issue(O, "acme", "CMT", title="discuss")
    k = iss["key"]
    time.sleep(0.01)
    r = c.api.post(f"/orgs/acme/issues/{k.lower()}/comments", {"body": "First!"}, V)
    t.status("viewer comments → 201", r, 201)
    j = r.json or {}
    t.eq("comment fields", (j.get("issue_key"), j.get("author_id"), j.get("body"), j.get("edited")),
         (k, V.id, "First!", False))
    t.ok("comment id/created_at/updated_at", is_uuid(j.get("id")) and is_ts(j.get("created_at"))
         and is_ts(j.get("updated_at")), r.text)
    ids = [j.get("id")]
    for i, (u, body) in enumerate([(D, "second"), (O, "third"), (V, "fourth"), (D, "x" * 20000)]):
        r = c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": body}, u)
        ids.append(r.get("id"))
        if i == 3:
            t.status("comment of 20000 chars → 201", r, 201)
    t.vfail("empty comment → 422", c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": ""}, V), "body", EMPTY)
    t.vfail("comment of 20001 chars → 422", c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": "x" * 20001}, V),
            "body", LONG)
    t.vfail("comment without body → 422", c.api.post(f"/orgs/acme/issues/{k}/comments", {}, V), "body", REQ)
    after = c.get_issue(O, "acme", k)
    t.eq("comment_count counts comments; version and updated_at unchanged",
         (after.get("comment_count"), after.get("version"), after.get("updated_at")), (5, 1, iss.get("updated_at")))
    items, pages, probs = c.walk(V, f"/orgs/acme/issues/{k}/comments", limit=2)
    t.ok("walk comments with limit=2", not probs and len(pages) >= 3, probs)
    t.eq("comments listed oldest first, each exactly once", [i.get("id") for i in items], ids)
    t.err("comments of an invisible issue → 404", c.api.get(f"/orgs/acme/issues/{k}/comments", M), 404, "not_found")
    t.err("comment on unknown issue → 404", c.api.post("/orgs/acme/issues/CMT-999/comments", {"body": "x"}, O), 404,
          "not_found")


@G.test
def edit_and_delete(c):
    t = c.t
    O, V, D, M = c.fx["O"], c.fx["V"], c.fx["D"], c.fx["M"]
    iss = c.issue(O, "acme", "CMT", title="edits")
    k = iss["key"]
    cv = must(c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": "by viewer"}, V), 201, "comment")
    cd = must(c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": "by dev"}, D), 201, "comment")
    co = must(c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": "by owner"}, O), 201, "comment")
    time.sleep(0.01)
    r = c.api.patch(f"/orgs/acme/comments/{cv['id']}", {"body": "edited by viewer"}, V)
    t.status("author edits own comment → 200", r, 200)
    t.eq("edited comment", (r.get("id"), r.get("body"), r.get("edited"), r.get("author_id"), r.get("created_at")),
         (cv["id"], "edited by viewer", True, V.id, cv["created_at"]))
    t.ok("edit advances updated_at", is_ts(r.get("updated_at")) and ts_ms(r.json["updated_at"]) > ts_ms(cv["updated_at"]),
         (cv["updated_at"], r.get("updated_at")))
    t.err("project admin editing someone else's comment → 403",
          c.api.patch(f"/orgs/acme/comments/{cv['id']}", {"body": "x"}, O), 403, "forbidden")
    t.err("another member editing a comment → 403", c.api.patch(f"/orgs/acme/comments/{cd['id']}", {"body": "x"}, V),
          403, "forbidden")
    t.vfail("edit with empty body → 422", c.api.patch(f"/orgs/acme/comments/{cv['id']}", {"body": ""}, V), "body",
            EMPTY)
    t.err("edit unknown comment → 404", c.api.patch(f"/orgs/acme/comments/{uuid.uuid4()}", {"body": "x"}, O), 404,
          "not_found")
    t.err("comment in an invisible project → 404 on edit",
          c.api.patch(f"/orgs/acme/comments/{cv['id']}", {"body": "x"}, M), 404, "not_found")
    t.err("comment in an invisible project → 404 on delete", c.api.delete(f"/orgs/acme/comments/{cv['id']}", M), 404,
          "not_found")
    t.err("developer deleting someone else's comment → 403", c.api.delete(f"/orgs/acme/comments/{co['id']}", D), 403,
          "forbidden")
    t.status("author deletes own comment → 204", c.api.delete(f"/orgs/acme/comments/{cd['id']}", D), 204)
    t.status("project admin deletes someone else's comment → 204",
             c.api.delete(f"/orgs/acme/comments/{cv['id']}", O), 204)
    t.err("deleted comment → 404", c.api.delete(f"/orgs/acme/comments/{cv['id']}", O), 404, "not_found")
    items, _, _ = c.walk(O, f"/orgs/acme/issues/{k}/comments", limit=50)
    t.eq("list after deletions", [i.get("id") for i in items], [co["id"]])
    t.eq("comment_count reflects deletions", c.get_issue(O, "acme", k).get("comment_count"), 1)
    r = c.api.get(f"/orgs/acme/issues/{k}/comments", O)
    t.eq("unedited comment has edited=false", [i.get("edited") for i in (r.get("items") or [])], [False])


# ================================================================================================================
# 7. history

G = group("history")


@G.setup
def history_setup(c):
    f = {k: c.user("h" + k.lower()) for k in ("O", "D", "X")}
    c.org(f["O"], "acme")
    c.add_member(f["O"], "acme", f["D"])
    c.project(f["O"], "acme", "HIS")
    return f


def changes_map(entry):
    ch = entry.get("changes") if isinstance(entry, dict) else None
    if not isinstance(ch, list):
        return None
    return {x.get("field"): (x.get("from"), x.get("to")) for x in ch if isinstance(x, dict)}


@G.test
def entries(c):
    t = c.t
    O, D = c.fx["O"], c.fx["D"]
    iss = c.issue(O, "acme", "HIS", title="Hist")
    k = iss["key"]
    r = c.api.get(f"/orgs/acme/issues/{k}/history", O)
    t.status("history → 200", r, 200)
    items = r.get("items") or []
    t.eq("new issue: one creation entry", [e.get("changes") for e in items],
         [[{"field": "created", "from": None, "to": None}]])
    t.ok("creation entry fields", len(items) == 1 and items[0].get("actor_id") == O.id and is_uuid(items[0].get("id"))
         and is_ts(items[0].get("created_at")), items)
    steps = [
        (D, "patch", {"title": "Hist 2", "priority": "high"}, {"title": ("Hist", "Hist 2"),
                                                                "priority": ("medium", "high")}),
        (O, "transition", "in_progress", {"status": ("todo", "in_progress")}),
        (D, "patch", {"labels": ["x", "Y"]}, {"labels": ([], ["x", "y"])}),
        (O, "patch", {"assignee_id": D.id}, {"assignee_id": (None, D.id)}),
        (O, "patch", {"due_date": "2027-01-15", "description": "text"},
         {"due_date": (None, "2027-01-15"), "description": (None, "text")}),
        (D, "patch", {"type": "bug", "labels": ["y"]}, {"type": ("task", "bug"), "labels": (["x", "y"], ["y"])}),
        (O, "transition", "done", {"status": ("in_progress", "done")}),
        (O, "patch", {"assignee_id": None, "due_date": None}, {"assignee_id": (D.id, None),
                                                               "due_date": ("2027-01-15", None)}),
    ]
    for u, kind, arg, _ in steps:
        r = c.patch_issue(u, "acme", k, arg) if kind == "patch" else c.transition(u, "acme", k, arg)
        must(r, 200, f"{kind} {arg}")
        time.sleep(0.003)
    # failed operations add nothing
    c.api.patch(f"/orgs/acme/issues/{k}", {"title": "nope"}, O, headers={"If-Match": '"1"'})
    c.transition(O, "acme", k, "todo")
    c.api.patch(f"/orgs/acme/issues/{k}", {"title": ""}, O, headers={"If-Match": '"9"'})
    r = c.api.get(f"/orgs/acme/issues/{k}/history" + qs({"limit": 100}), O)
    items = r.get("items") or []
    t.eq("one entry per update/transition plus creation; failed operations add none", len(items), len(steps) + 1)
    exp = [s[3] for s in reversed(steps)] + [{"created": (None, None)}]
    got = [changes_map(e) for e in items]
    for i, (g, e) in enumerate(zip(got, exp)):
        t.eq(f"entry {i} (newest first) changes: {sorted(e)}", g, e)
    t.eq("actor_id per entry", [e.get("actor_id") for e in items], [s[0].id for s in reversed(steps)] + [O.id])
    ts = [ts_ms(e["created_at"]) for e in items if is_ts(e.get("created_at"))]
    t.ok("entries newest first (created_at non-increasing)", len(ts) == len(items) and ts == sorted(ts, reverse=True),
         [e.get("created_at") for e in items])
    walked, pages, probs = c.walk(O, f"/orgs/acme/issues/{k}/history", limit=4)
    t.ok("walk history limit=4", not probs and len(pages) == 3, (probs, len(pages)))
    t.eq("paginated history equals the full list", [e.get("id") for e in walked], [e.get("id") for e in items])
    t.err("history for a non-member → 404", c.api.get(f"/orgs/acme/issues/{k}/history", c.fx["X"]), 404, "not_found")
    t.err("history for an unknown issue → 404", c.api.get("/orgs/acme/issues/HIS-77/history", O), 404, "not_found")


@G.test
def noop_and_comments_do_not_bump(c):
    t = c.t
    O = c.fx["O"]
    iss = c.issue(O, "acme", "HIS", title="Same", labels=["a"])
    k = iss["key"]
    r = c.patch_issue(O, "acme", k, {"title": "Same", "labels": ["A"]}, version=1)
    t.eq("no-op patch keeps version 1", (r.status, r.get("version")), (200, 1))
    c.api.post(f"/orgs/acme/issues/{k}/comments", {"body": "a comment"}, O)
    t.eq("comment keeps version 1", c.get_issue(O, "acme", k).get("version"), 1)
    r = c.patch_issue(O, "acme", k, {"title": "Changed"}, version=1)
    t.eq("next real change → version 2", (r.status, r.get("version")), (200, 2))


# ================================================================================================================
# 8. listing and search

G = group("search")

POOL = ["login", "crash", "slow", "query", "button", "export", "import", "cache", "token", "timeout", "memory",
        "layout", "report", "sync", "error", "page"]
SEPS = [" ", ", ", ". ", "-", "_", "/", " (", ") "]
LABELS = ["ui", "api", "db", "perf", "ux", "v2", "infra"]
CRAFTED = [  # (project, title, description)
    ("APP", "Running the café server", None),
    ("APP", "Cat category", "catalog of cats"),
    ("APP", "Smart start", "chart"),
    ("APP", "Art deco poster", None),
    ("APP", "Привет мир", "Тестовое описание"),
    ("BACK", "日本語 support", None),
    ("BACK", "x-ray scanner", "foo_bar baz"),
    ("APP", "Naïve approach", "uses CAFÉ data"),
    ("BACK", "naive approach", None),
    ("CORE", "alpha", "beta gamma"),
    ("CORE", "Version v2 rollout", "v20 later"),
    ("SEC", "café secret", None),
    ("CORE", "ÜBER Ärger", "straight"),
    ("CORE", "über", "ärger"),
]
QUERIES = ["café", "CAFÉ", "run", "running", "cat", "cats", "art", "привет", "ПРИВЕТ мир", "日本語", "日本", "ray",
           "x-ray", "foo", "foo_bar", "naïve", "naive", "alpha beta", "v2", "über ärger", "alpha zzz", "crash",
           "LOGIN", "cache token", "memory-sync", "error page", "data café", "approach"]


@G.setup
def search_setup(c):
    rnd = random.Random(20261005)
    O, M1, M2, X = c.user("so"), c.user("sma"), c.user("smb"), c.user("sx")
    slug = c.org(O, "search")
    c.add_member(O, slug, M1)
    c.add_member(O, slug, M2)
    for k in ("APP", "BACK", "CORE"):
        c.project(O, slug, k)
    c.project(O, slug, "SEC", visibility="private")
    c.pmember(O, slug, "SEC", M2, "developer")
    model = {}

    def gen(proj, creator):
        typ = rnd.choice(TYPES)
        title = rnd.choice(SEPS[:1] + SEPS).join(
            (w.capitalize() if rnd.random() < 0.3 else w) for w in rnd.sample(POOL, rnd.randint(1, 4)))
        title = title.strip(" (")  # keep it simple; trailing ") " trimmed by the service anyway
        title = title.strip()
        desc = None
        if rnd.random() < 0.7:
            desc = "".join(w + rnd.choice(SEPS) for w in (rnd.choice(POOL) for _ in range(rnd.randint(1, 6)))).strip()
        body = {"type": typ, "title": title}
        if desc is not None:
            body["description"] = desc
        pr = rnd.choice(PRIORITIES + [None])
        if pr:
            body["priority"] = pr
        labs = rnd.sample(LABELS, rnd.randint(0, 3))
        if labs or rnd.random() < 0.5:
            body["labels"] = [x.upper() if rnd.random() < 0.3 else x for x in labs]
        cands = [None, O, M2] + ([M1] if proj != "SEC" else [])
        a = rnd.choice(cands)
        if a:
            body["assignee_id"] = a.id
        m = {"project": proj, "type": typ, "title": title, "description": desc, "priority": pr or "medium",
             "labels": sorted(set(labs)), "assignee": a.id if a else None, "reporter": creator.id, "status": "todo"}
        return body, m

    batches = [(O, "APP", 40), (M1, "BACK", 35), (M2, "SEC", 30), (O, "CORE", 30), (M1, "APP", 30), (M2, "BACK", 30),
               (O, "SEC", 25), (M2, "CORE", 30), (M1, "CORE", 25)]
    for creator, proj, n in batches:
        items = [gen(proj, creator) for _ in range(n)]
        r = c.api.post(f"/orgs/{slug}/projects/{proj}/issues/bulk", {"issues": [b for b, _ in items]}, creator)
        if r.status == 201 and isinstance(r.get("keys"), list) and len(r.get("keys")) == n:
            keys = r.get("keys")
        else:  # bulk is tested elsewhere; fall back to single creates
            keys = [must(c.api.post(f"/orgs/{slug}/projects/{proj}/issues", b, creator), 201, "create")["key"]
                    for b, _ in items]
        for k, (_, m) in zip(keys, items):
            model[k] = m
        time.sleep(0.03)
    for proj, title, desc in CRAFTED:
        body = {"type": "task", "title": title}
        if desc is not None:
            body["description"] = desc
        j = c.issue(O, slug, proj, **body)
        model[j["key"]] = {"project": proj, "type": "task", "title": title, "description": desc, "priority": "medium",
                           "labels": [], "assignee": None, "reporter": O.id, "status": "todo"}
        time.sleep(0.005)
    # mutations: transitions and patches, in two phases separated by a pause
    keys = sorted(model)
    rnd.shuffle(keys)
    versions = {}

    def bump(k, r):
        must(r, 200, f"mutate {k}")
        versions[k] = r.json["version"]

    for k in keys[:70]:
        bump(k, c.transition(O, slug, k, "in_progress"))
        model[k]["status"] = "in_progress"
    time.sleep(0.06)
    for k in keys[:35]:
        bump(k, c.transition(O, slug, k, "done"))
        model[k]["status"] = "done"
    for k in keys[:10]:
        bump(k, c.transition(O, slug, k, "in_progress"))
        model[k]["status"] = "in_progress"
    for k in keys[100:140]:
        m = model[k]
        ch = {}
        if rnd.random() < 0.6:
            ch["priority"] = m["priority"] = rnd.choice(PRIORITIES)
        if rnd.random() < 0.5:
            a = rnd.choice([None, O, M2] + ([M1] if m["project"] != "SEC" else []))
            ch["assignee_id"] = m["assignee"] = a.id if a else None
        if rnd.random() < 0.4 or not ch:
            labs = rnd.sample(LABELS, rnd.randint(0, 2))
            ch["labels"] = labs
            m["labels"] = sorted(labs)
        bump(k, c.patch_issue(O, slug, k, ch, version=versions.get(k, 1)))
    items, _, probs = c.walk(O, f"/orgs/{slug}/issues", {"sort": "key"}, limit=100)
    fetched = {i.get("key"): i for i in items}
    return {"O": O, "M1": M1, "M2": M2, "X": X, "slug": slug, "model": model, "fetched": fetched,
            "fetch_problems": probs, "fetched_list": items}


def s_visible(fx, user):
    keys = [k for k, m in fx["model"].items() if m["project"] != "SEC" or user is not fx["M1"]]
    return keys


def key_tuple(k):
    p, n = k.rsplit("-", 1)
    return (p, int(n))


@G.test
def dataset_matches(c):
    t = c.t
    fx = c.fx
    t.ok("walking all issues (sort=key, limit=100) works", not fx["fetch_problems"], fx["fetch_problems"])
    t.eq("listing returns every created issue exactly once",
         sorted([i.get("key") for i in fx["fetched_list"]], key=key_tuple), sorted(fx["model"], key=key_tuple))
    bad = []
    for k, m in fx["model"].items():
        i = fx["fetched"].get(k)
        if not i:
            continue
        got = {"project": i.get("project_key"), "type": i.get("type"), "title": i.get("title"),
               "description": i.get("description"), "priority": i.get("priority"), "labels": i.get("labels"),
               "assignee": i.get("assignee_id"), "reporter": i.get("reporter_id"), "status": i.get("status")}
        if got != m:
            bad.append((k, {f: (got[f], m[f]) for f in m if got[f] != m[f]}))
    t.ok("listed issues carry the expected field values", not bad, bad[:5])
    t.ok("listed items are full issue representations",
         all(set(i) >= ISSUE_FIELDS for i in fx["fetched_list"]), "")
    r = c.api.get(f"/orgs/{fx['slug']}/issues" + qs({"limit": 5}), fx["O"])
    t.eq("first page: total is the exact number of matches", r.get("total"), len(fx["model"]))


def run_filter_case(c, name, user, params, pred, alt_pred=None):
    """Compare the set of keys (walk, limit 100) and `total` against the model."""
    fx = c.fx
    vis = s_visible(fx, user)
    exp = sorted([k for k in vis if pred(k, fx["model"][k])], key=key_tuple)
    alt = sorted([k for k in vis if alt_pred(k, fx["model"][k])], key=key_tuple) if alt_pred else exp
    items, pages, probs = c.walk(user, f"/orgs/{fx['slug']}/issues", dict(params, sort="key"), limit=100)
    got = [i.get("key") for i in items]
    totals = {p.get("total") for p in pages}
    if probs:
        c.t.ok(f"{name}: request ok", False, probs)
        return
    ok = sorted(got, key=key_tuple) in (exp, alt) and len(set(got)) == len(got)
    c.t.ok(f"{name}: matching keys", ok, f"expected ({len(exp)}): {short(exp, 300)}" +
           (f"\nor ({len(alt)}): {short(alt, 300)}" if alt != exp else "") + f"\nactual ({len(got)}): {short(got, 300)}")
    c.t.ok(f"{name}: total", totals in ({len(exp)}, {len(alt)}), f"expected {len(exp)}, got totals {totals}")


@G.test
def filters(c):
    fx = c.fx
    O, M1, M2 = fx["O"], fx["M1"], fx["M2"]
    model = fx["model"]
    ts = lambda k, f: ts_ms(fx["fetched"][k][f]) if k in fx["fetched"] else 0  # noqa: E731
    cvals = sorted({ts(k, "created_at") for k in model})
    uvals = sorted({ts(k, "updated_at") for k in model})

    def boundary(vals, frac):
        cands = [i for i in range(len(vals) - 1) if vals[i + 1] - vals[i] >= 4]
        if not cands:
            return None
        i = min(cands, key=lambda i: abs(i - frac * len(vals)))
        return vals[i] + (vals[i + 1] - vals[i]) // 2

    b1, b2, bu = boundary(cvals, 0.33), boundary(cvals, 0.7), boundary(uvals, 0.5)
    cases = [
        ("no filters (owner sees everything)", O, {}, lambda k, m: True),
        ("no filters (member does not see private project)", M1, {}, lambda k, m: True),
        ("project=APP", O, {"project": "APP"}, lambda k, m: m["project"] == "APP"),
        ("project=APP,BACK", M1, {"project": "APP,BACK"}, lambda k, m: m["project"] in ("APP", "BACK")),
        ("project=SEC (developer)", M2, {"project": "SEC"}, lambda k, m: m["project"] == "SEC"),
        ("status=todo", O, {"status": "todo"}, lambda k, m: m["status"] == "todo"),
        ("status=in_progress,done", O, {"status": "in_progress,done"}, lambda k, m: m["status"] != "todo"),
        ("status=done", M1, {"status": "done"}, lambda k, m: m["status"] == "done"),
        ("priority=high,highest", O, {"priority": "high,highest"}, lambda k, m: m["priority"] in ("high", "highest")),
        ("priority=medium", M1, {"priority": "medium"}, lambda k, m: m["priority"] == "medium"),
        ("type=bug", O, {"type": "bug"}, lambda k, m: m["type"] == "bug"),
        ("type=bug,story", O, {"type": "bug,story"}, lambda k, m: m["type"] in ("bug", "story")),
        ("assignee=me", M1, {"assignee": "me"}, lambda k, m: m["assignee"] == M1.id),
        ("assignee=none", O, {"assignee": "none"}, lambda k, m: m["assignee"] is None),
        ("assignee=<id>", M1, {"assignee": M2.id}, lambda k, m: m["assignee"] == M2.id),
        ("assignee=me,none", M2, {"assignee": "me,none"}, lambda k, m: m["assignee"] in (M2.id, None)),
        ("assignee=<id>,<id>", O, {"assignee": f"{M1.id},{O.id}"}, lambda k, m: m["assignee"] in (M1.id, O.id)),
        ("reporter=me", M1, {"reporter": "me"}, lambda k, m: m["reporter"] == M1.id),
        ("reporter=<id>,me", O, {"reporter": f"{M2.id},me"}, lambda k, m: m["reporter"] in (M2.id, O.id)),
        ("label=ui", O, {"label": "ui"}, lambda k, m: "ui" in m["labels"]),
        ("label=db,perf", M1, {"label": "db,perf"}, lambda k, m: bool({"db", "perf"} & set(m["labels"]))),
        ("label=v2", O, {"label": "v2"}, lambda k, m: "v2" in m["labels"]),
        ("project=APP&status=todo&priority=high,highest", O,
         {"project": "APP", "status": "todo", "priority": "high,highest"},
         lambda k, m: m["project"] == "APP" and m["status"] == "todo" and m["priority"] in ("high", "highest")),
        ("label=ui&assignee=none&type=bug,task", O, {"label": "ui", "assignee": "none", "type": "bug,task"},
         lambda k, m: "ui" in m["labels"] and m["assignee"] is None and m["type"] in ("bug", "task")),
        ("reporter=me&status=in_progress (member)", M1, {"reporter": "me", "status": "in_progress"},
         lambda k, m: m["reporter"] == M1.id and m["status"] == "in_progress"),
        ("q=crash&status=todo", O, {"q": "crash", "status": "todo"},
         lambda k, m: "crash" in words(m["title"]) + words(m["description"]) and m["status"] == "todo"),
    ]
    if b1 and b2:
        cases += [
            ("created_after", O, {"created_after": ms_to_iso(b1)}, lambda k, m: ts(k, "created_at") > b1),
            ("created_before", O, {"created_before": ms_to_iso(b1)}, lambda k, m: ts(k, "created_at") < b1),
            ("created_after&created_before", M1, {"created_after": ms_to_iso(b1), "created_before": ms_to_iso(b2)},
             lambda k, m: b1 < ts(k, "created_at") < b2),
            ("created_after with +02:00 offset", O,
             {"created_after": (datetime.fromtimestamp(b2 / 1000, timezone.utc) + timedelta(hours=2)).strftime(
                 "%Y-%m-%dT%H:%M:%S.") + f"{b2 % 1000:03d}+02:00"},
             lambda k, m: ts(k, "created_at") > b2),
        ]
    else:
        c.t.ok("dataset has created_at gaps to place filter boundaries", False, cvals[:20])
    if bu:
        cases += [("updated_after", O, {"updated_after": ms_to_iso(bu)}, lambda k, m: ts(k, "updated_at") > bu),
                  ("updated_after&status=done", O, {"updated_after": ms_to_iso(bu), "status": "done"},
                   lambda k, m: ts(k, "updated_at") > bu and m["status"] == "done")]
    else:
        c.t.ok("dataset has updated_at gaps to place filter boundaries", False, uvals[:20])
    for name, user, params, pred in cases:
        run_filter_case(c, name, user, params, pred)


@G.test
def full_text(c):
    fx = c.fx
    for q in QUERIES:
        qw = words(q)

        def per_word(k, m, qw=qw):
            ws = set(words(m["title"])) | set(words(m["description"]))
            return all(w in ws for w in qw)

        def one_field(k, m, qw=qw):
            return all(w in words(m["title"]) for w in qw) or all(w in words(m["description"]) for w in qw)

        for user in (fx["O"], fx["M1"]) if q in ("café", "data café") else (fx["O"],):
            who = "owner" if user is fx["O"] else "member"
            run_filter_case(c, f"q={q!r} ({who})", user, {"q": q}, per_word, one_field)
    # spot checks of the semantics independent of the generated data
    fxm = fx["model"]
    hit = lambda q: {i.get("key") for i in c.walk(fx["O"], f"/orgs/{fx['slug']}/issues", {"q": q}, limit=100)[0]}  # noqa
    title_of = {m["title"]: k for k, m in fxm.items()}
    c.t.ok("q is whole-word: 'cat' matches 'Cat category', 'art' does not match 'Smart start' / 'chart'",
           title_of["Cat category"] in hit("cat") and title_of["Smart start"] not in hit("art"), "")
    c.t.ok("q does not stem: 'run' does not match 'Running'", title_of["Running the café server"] not in hit("run"), "")
    c.t.ok("q does not fold accents: 'naive' ≠ 'naïve'",
           title_of["Naïve approach"] not in hit("naive") and title_of["naive approach"] not in hit("naïve"), "")


@G.test
def sorting(c):
    fx = c.fx
    O = fx["O"]
    fetched = fx["fetched"]

    def val(k, field):
        i = fetched[k]
        if field == "created":
            return ts_ms(i["created_at"])
        if field == "updated":
            return ts_ms(i["updated_at"])
        if field == "priority":
            return PRI_RANK[fx["model"][k]["priority"]]
        return key_tuple(k)

    def expected(keys, sort):
        f = sort.lstrip("-")
        base = sorted(keys, key=key_tuple)
        return sorted(base, key=lambda k: val(k, f), reverse=sort.startswith("-"))

    def check(name, user, params, sort, limit):
        vis = [k for k in s_visible(fx, user) if k in fetched]
        pred = params.pop("_pred", lambda k: True)
        vis = [k for k in vis if pred(k)]
        exp = expected(vis, sort)
        p = dict(params)
        if sort is not None:
            p["sort"] = sort
        items, _, probs = c.walk(user, f"/orgs/{fx['slug']}/issues", p, limit=limit)
        got = [i.get("key") for i in items]
        if probs:
            c.t.ok(f"{name}: request ok", False, probs)
            return
        f = sort.lstrip("-")
        if f in ("created", "updated"):
            # timestamps are compared at the returned (ms) precision; order inside equal values is not checked
            ok = sorted(got, key=key_tuple) == sorted(exp, key=key_tuple) and \
                [val(k, f) for k in got if k in fetched] == [val(k, f) for k in exp]
        else:
            ok = got == exp
        c.t.ok(f"{name}: order", ok, f"expected: {short(exp, 300)}\nactual:   {short(got, 300)}")
        c.t.eq(f"{name}: every item exactly once", len(got), len(set(got)))

    for sort in ["created", "-created", "updated", "-updated", "priority", "-priority", "key", "-key"]:
        check(f"sort={sort} walk limit=37", O, {}, sort, 37)
    check("sort=-priority with status=todo, limit=11", O,
          {"status": "todo", "_pred": lambda k: fx["model"][k]["status"] == "todo"}, "-priority", 11)
    check("sort=priority as member, limit=50", fx["M1"], {}, "priority", 50)
    check("sort=-key with label=api, limit=3", O, {"label": "api", "_pred": lambda k: "api" in fx["model"][k]["labels"]},
          "-key", 3)
    # default sort and limit
    r = c.api.get(f"/orgs/{fx['slug']}/issues", O)
    got = [i.get("key") for i in (r.get("items") or [])]
    exp = expected([k for k in fetched], "-created")
    c.t.eq("default page size is 50", len(got), 50)
    c.t.ok("default sort is -created", [val(k, "created") for k in got if k in fetched] ==
           [val(k, "created") for k in exp[:50]], f"{got[:10]} vs {exp[:10]}")
    c.t.ok("next_cursor present when more results exist", isinstance(r.get("next_cursor"), str), r.get("next_cursor"))


@G.test
def invalid_filters(c):
    fx = c.fx
    O = fx["O"]
    for param, value in [("status", "open"), ("status", "todo,blocked"), ("priority", "urgent"), ("type", "epic"),
                         ("assignee", "someone"), ("reporter", "nobody"), ("created_after", "yesterday"),
                         ("created_before", "2026-13-45T00:00:00Z"), ("updated_after", "garbage"),
                         ("sort", "title"), ("sort", "-bogus"), ("limit", "0"), ("limit", "101"), ("limit", "x")]:
        r = c.api.get(f"/orgs/{fx['slug']}/issues" + qs({param: value}), O)
        c.t.vfail(f"{param}={value} → 422 with field {param}", r, param, ANY)
    c.t.err("listing issues of an org the caller is not in → 404", c.api.get(f"/orgs/{fx['slug']}/issues", fx["X"]),
            404, "not_found")


# ================================================================================================================
# 9. webhooks

G = group("webhooks")


@G.setup
def webhooks_setup(c):
    return {k: c.user("w" + k.lower()) for k in ("O", "A", "M", "X")}


def wh_org(c, key="WH"):
    f = c.fx
    slug = c.org(f["O"])
    c.add_member(f["O"], slug, f["A"], "admin")
    c.add_member(f["O"], slug, f["M"])
    c.project(f["O"], slug, key)
    return slug


def mk_hook(c, slug, path, events=None, secret="s3cret-value", user=None):
    body = {"url": c.rcv.url(path), "events": events or EVENTS}
    if secret is not None:
        body["secret"] = secret
    return must(c.api.post(f"/orgs/{slug}/webhooks", body, user or c.fx["O"]), 201, f"create webhook {path}")


def settled_deliveries(c, slug, wid, user, n, timeout=5.0):
    """Poll the deliveries list until it has n entries, none pending; returns the last list seen."""
    last = []

    def probe():
        nonlocal last
        r = c.api.get(f"/orgs/{slug}/webhooks/{wid}/deliveries" + qs({"limit": 100}), user)
        last = (r.get("items") or []) if r.status == 200 else []
        return len(last) == n and all(isinstance(d, dict) and d.get("status") != "pending" for d in last)

    wait_until(probe, timeout)
    return last


def ev_issue_key(rec):
    try:
        return rec["json"]["data"]["issue"]["key"]
    except (TypeError, KeyError):
        return None


@G.test
def crud(c):
    t = c.t
    f = c.fx
    slug = wh_org(c)
    base = f"/orgs/{slug}/webhooks"
    r = c.api.post(base, {"url": "https://example.com/hook", "events": ["issue.created"], "secret": "abc"}, f["O"])
    t.status("owner creates webhook → 201", r, 201)
    j = r.json or {}
    t.eq("webhook fields", (j.get("url"), j.get("events"), j.get("active"), j.get("secret")),
         ("https://example.com/hook", ["issue.created"], True, "abc"))
    t.ok("webhook id/created_at", is_uuid(j.get("id")) and is_ts(j.get("created_at")), r.text)
    wid = j.get("id")
    r = c.api.post(base, {"url": "http://example.com/other", "events": EVENTS}, f["A"])
    t.status("org admin creates webhook without secret → 201", r, 201)
    t.ok("a secret is generated when not given", isinstance(r.get("secret"), str) and len(r.get("secret")) > 0,
         r.text)
    wid2 = r.get("id")
    t.err("plain member creating a webhook → 403",
          c.api.post(base, {"url": "http://example.com/x", "events": EVENTS}, f["M"]), 403, "forbidden")
    t.err("plain member listing webhooks → 403", c.api.get(base, f["M"]), 403, "forbidden")
    t.err("plain member reading a webhook → 403", c.api.get(f"{base}/{wid}", f["M"]), 403, "forbidden")
    t.err("plain member reading deliveries → 403", c.api.get(f"{base}/{wid}/deliveries", f["M"]), 403, "forbidden")
    t.err("non-member listing webhooks → 404", c.api.get(base, f["X"]), 404, "not_found")
    items, _, probs = c.walk(f["A"], base, limit=1)
    t.eq("list webhooks (walk limit=1) returns both exactly once", sorted(i.get("id") for i in items),
         sorted([wid, wid2]))
    t.ok("listed webhooks do not reveal the secret", all("secret" not in i for i in items), items)
    r = c.api.get(f"{base}/{wid}", f["O"])
    t.eq("GET webhook", (r.status, r.get("id"), r.get("url"), r.get("events"), r.get("active")),
         (200, wid, "https://example.com/hook", ["issue.created"], True))
    t.ok("GET webhook does not reveal the secret", "secret" not in (r.json or {}), r.text)
    r = c.api.patch(f"{base}/{wid}", {"active": False, "events": ["issue.updated", "comment.created"]}, f["O"])
    t.eq("PATCH webhook active/events", (r.status, r.get("active"), sorted(r.get("events") or [])),
         (200, False, ["comment.created", "issue.updated"]))
    r = c.api.patch(f"{base}/{wid}", {"url": "http://example.org/new"}, f["O"])
    t.eq("PATCH webhook url", (r.status, r.get("url"), r.get("active")), (200, "http://example.org/new", False))
    t.vfail("PATCH webhook with invalid url → 422", c.api.patch(f"{base}/{wid}", {"url": "ftp://x/y"}, f["O"]), "url",
            ANY)
    t.vfail("PATCH webhook with empty events → 422", c.api.patch(f"{base}/{wid}", {"events": []}, f["O"]), "events",
            ANY)
    for label, body, field in [
        ("ftp url", {"url": "ftp://example.com/x", "events": EVENTS}, "url"),
        ("non-url", {"url": "not a url", "events": EVENTS}, "url"),
        ("missing url", {"events": EVENTS}, "url"),
        ("empty events", {"url": "http://example.com/x", "events": []}, "events"),
        ("unknown event", {"url": "http://example.com/x", "events": ["issue.exploded"]}, "events"),
        ("missing events", {"url": "http://example.com/x"}, "events"),
    ]:
        t.vfail(f"create webhook with {label} → 422", c.api.post(base, body, f["O"]), field, ANY, True)
    t.status("DELETE webhook → 204", c.api.delete(f"{base}/{wid}", f["O"]), 204)
    t.err("deleted webhook → 404", c.api.get(f"{base}/{wid}", f["O"]), 404, "not_found")
    t.err("unknown webhook → 404", c.api.get(f"{base}/{uuid.uuid4()}", f["O"]), 404, "not_found")
    other = c.org(f["X"])
    t.err("webhook looked up under another org → 404", c.api.get(f"/orgs/{other}/webhooks/{wid2}", f["X"]), 404,
          "not_found")


@G.test
def payloads(c):
    t = c.t
    O = c.fx["O"]
    slug = wh_org(c)
    path = f"/payload-{slug}"
    secret = "whsec-payload-123"
    wh = mk_hook(c, slug, path, secret=secret)
    iss = c.issue(O, slug, "WH", type="bug", title="Hook me", labels=["a"])
    time.sleep(0.005)
    patched = must(c.patch_issue(O, slug, "WH-1", {"title": "Hooked", "priority": "high"}, version=1), 200, "patch")
    time.sleep(0.005)
    trans = must(c.transition(O, slug, "WH-1", "in_progress"), 200, "transition")
    time.sleep(0.005)
    com = must(c.api.post(f"/orgs/{slug}/issues/WH-1/comments", {"body": "hello hook"}, O), 201, "comment")
    time.sleep(0.005)
    before_delete = c.get_issue(O, slug, "WH-1")
    must(c.api.delete(f"/orgs/{slug}/issues/WH-1", O), 204, "delete")
    recs = c.rcv.wait_path(path, lambda rs: rs if len(rs) >= 5 else None, 15) or c.rcv.recs(path)
    t.eq("all 5 events delivered once each (no retries needed)", len(c.rcv.recs(path)), 5)
    evs = [r["json"].get("event") if isinstance(r["json"], dict) else None for r in recs]
    t.eq("events arrive in order for the same issue", evs,
         ["issue.created", "issue.updated", "issue.updated", "comment.created", "issue.deleted"])
    now = time.time()
    for r in recs:
        h = r["headers"]
        j = r["json"] if isinstance(r["json"], dict) else {}
        ev = j.get("event")
        t.ok(f"{ev}: Content-Type application/json", h.get("content-type", "").startswith("application/json"), h)
        t.eq(f"{ev}: X-Tracker-Event header", h.get("x-tracker-event"), ev)
        t.ok(f"{ev}: X-Tracker-Delivery equals body id", h.get("x-tracker-delivery") and
             h.get("x-tracker-delivery") == j.get("id"), (h.get("x-tracker-delivery"), j.get("id")))
        ts_h = h.get("x-tracker-timestamp", "")
        t.ok(f"{ev}: X-Tracker-Timestamp is current unix seconds", ts_h.isdigit() and abs(int(ts_h) - now) < 60, ts_h)
        t.ok(f"{ev}: X-Tracker-Signature is HMAC-SHA256(secret, timestamp.body)", sig_ok(r, secret),
             h.get("x-tracker-signature"))
        t.eq(f"{ev}: body org/actor", (j.get("org"), j.get("actor_id")), (slug, O.id))
        t.ok(f"{ev}: body created_at", is_ts(j.get("created_at")), j.get("created_at"))
    data = [r["json"].get("data") if isinstance(r["json"], dict) else {} for r in recs] + [{}] * 5
    t.eq("issue.created data.issue is the created issue", data[0].get("issue"), iss)
    t.eq("issue.updated (patch) data.issue is the updated issue", data[1].get("issue"), patched)
    ch = {x.get("field"): (x.get("from"), x.get("to")) for x in (data[1].get("changes") or [])}
    t.eq("issue.updated (patch) changes", ch, {"title": ("Hook me", "Hooked"), "priority": ("medium", "high")})
    t.eq("issue.updated (transition) data", (data[2].get("issue"), data[2].get("changes")),
         (trans, [{"field": "status", "from": "todo", "to": "in_progress"}]))
    t.eq("comment.created data.comment", data[3].get("comment"), com)
    t.eq("comment.created data.issue is the issue", (data[3].get("issue") or {}).get("key"), "WH-1")
    t.eq("issue.deleted data.issue is the issue as it was", data[4].get("issue"), before_delete)
    ids = [r["headers"].get("x-tracker-delivery") for r in recs]
    t.eq("delivery ids are unique", len(set(ids)), 5)
    dl = settled_deliveries(c, slug, wh["id"], O, 5)
    t.eq("deliveries listed newest first", [d.get("id") for d in dl], list(reversed(ids)))
    t.eq("deliveries: event/status/attempts/last_status_code",
         [(d.get("event"), d.get("status"), d.get("attempts"), d.get("last_status_code")) for d in dl],
         [(e, "succeeded", 1, 200) for e in reversed(evs)])
    t.ok("deliveries have created_at", all(is_ts(d.get("created_at")) for d in dl), dl)
    items, pages, probs = c.walk(O, f"/orgs/{slug}/webhooks/{wh['id']}/deliveries", limit=2)
    t.eq("deliveries pagination walk (limit=2)", ([d.get("id") for d in items], len(pages)), (list(reversed(ids)), 3))


@G.test
def subscriptions(c):
    t = c.t
    O = c.fx["O"]
    slug = wh_org(c)
    p_all, p_com, p_off = f"/all-{slug}", f"/com-{slug}", f"/off-{slug}"
    mk_hook(c, slug, p_all, secret="one")
    w_com = mk_hook(c, slug, p_com, events=["comment.created"], secret="two")
    w_off = mk_hook(c, slug, p_off)
    must(c.api.patch(f"/orgs/{slug}/webhooks/{w_off['id']}", {"active": False}, O), 200, "deactivate")
    c.issue(O, slug, "WH", title="subs")
    must(c.api.post(f"/orgs/{slug}/issues/WH-1/comments", {"body": "c"}, O), 201, "comment")
    c.rcv.wait_path(p_all, lambda rs: len(rs) >= 2, 10)
    c.rcv.wait_path(p_com, lambda rs: len(rs) >= 1, 10)
    must(c.api.patch(f"/orgs/{slug}/webhooks/{w_com['id']}", {"events": ["issue.updated"]}, O), 200, "change events")
    must(c.patch_issue(O, slug, "WH-1", {"title": "subs 2"}), 200, "patch")
    c.rcv.wait_path(p_com, lambda rs: len(rs) >= 2, 10)
    c.rcv.wait_path(p_all, lambda rs: len(rs) >= 3, 10)
    time.sleep(0.4)
    ev = lambda p: [r["json"].get("event") for r in c.rcv.recs(p) if isinstance(r["json"], dict)]  # noqa: E731
    t.eq("webhook with all events receives all", ev(p_all), ["issue.created", "comment.created", "issue.updated"])
    t.eq("webhook receives only subscribed events (and PATCHed events apply)", ev(p_com),
         ["comment.created", "issue.updated"])
    t.eq("inactive webhook receives nothing", ev(p_off), [])
    t.ok("each webhook signs with its own secret",
         all(sig_ok(r, "one") for r in c.rcv.recs(p_all)) and all(sig_ok(r, "two") for r in c.rcv.recs(p_com)), "")


@G.test
def retries(c):
    t = c.t
    O = c.fx["O"]
    slug = wh_org(c)
    path = f"/retry-{slug}"
    c.rcv.policies[path] = lambda rec, attempt: 500 if attempt <= 2 else 200
    wh = mk_hook(c, slug, path)
    c.issue(O, slug, "WH", title="retry")
    recs = c.rcv.wait_path(path, lambda rs: rs if successes(rs) else None, 15) or c.rcv.recs(path)
    t.eq("failed attempts are retried until success (3 attempts)", [r["status"] for r in recs], [500, 500, 200])
    t.eq("same X-Tracker-Delivery for every attempt", len({r["headers"].get("x-tracker-delivery") for r in recs}), 1)
    t.ok("every attempt is correctly signed", all(sig_ok(r, "s3cret-value") for r in recs), "")
    gaps = [b["t"] - a["t"] for a, b in zip(recs, recs[1:])]
    t.ok("retry delays follow 1s, 2s × WEBHOOK_BACKOFF_SCALE", len(gaps) == 2 and gaps[0] >= 0.8 * BACKOFF_SCALE and
         gaps[1] >= 0.8 * 2 * BACKOFF_SCALE, gaps)
    d = (settled_deliveries(c, slug, wh["id"], O, 1) or [{}])[0]
    t.eq("delivery record after retries", (d or {}).get("status"), "succeeded")
    t.eq("delivery attempts and last_status_code", ((d or {}).get("attempts"), (d or {}).get("last_status_code")),
         (3, 200))


@G.test
def permanent_failure(c):
    t = c.t
    O = c.fx["O"]
    slug = wh_org(c)
    path = f"/fail-{slug}"
    c.rcv.policies[path] = lambda rec, attempt: 503
    wh = mk_hook(c, slug, path)
    c.issue(O, slug, "WH", title="fail")
    c.rcv.wait_path(path, lambda rs: len(rs) >= 6, 20)
    time.sleep(1.5)
    recs = c.rcv.recs(path)
    t.eq("exactly 6 attempts, then no more", len(recs), 6)
    gaps = [b["t"] - a["t"] for a, b in zip(recs, recs[1:])]
    t.ok("backoff 1, 2, 4, 8, 16 s × scale", len(gaps) == 5 and all(g >= 0.8 * BACKOFF_SCALE * 2 ** i
                                                                    for i, g in enumerate(gaps)), gaps)
    d = (settled_deliveries(c, slug, wh["id"], O, 1) or [{}])[0]
    t.eq("delivery is failed with 6 attempts and last_status_code 503",
         ((d or {}).get("status"), (d or {}).get("attempts"), (d or {}).get("last_status_code")), ("failed", 6, 503))


@G.test
def timeout_counts_as_failure(c):
    t = c.t
    O = c.fx["O"]
    slug = wh_org(c)
    path = f"/slow-{slug}"
    c.rcv.policies[path] = lambda rec, attempt: (200, 6.5) if attempt == 1 else 200
    wh = mk_hook(c, slug, path)
    c.issue(O, slug, "WH", title="slow")
    recs = c.rcv.wait_path(path, lambda rs: rs if len(rs) >= 2 else None, 20) or c.rcv.recs(path)
    t.eq("an answer slower than 5 s is a failure and is retried (2 attempts)", len(recs), 2)
    d = (settled_deliveries(c, slug, wh["id"], O, 1, timeout=8) or [{}])[0]
    t.eq("delivery succeeded on attempt 2", ((d or {}).get("status"), (d or {}).get("attempts")), ("succeeded", 2))


@G.test
def ordering(c):
    t = c.t
    O = c.fx["O"]
    slug = wh_org(c, "ORD")
    path = f"/order-{slug}"

    def pol(rec, attempt):
        j = rec["json"] or {}
        return 500 if j.get("event") == "issue.created" and ev_issue_key(rec) == "ORD-1" else 200

    c.rcv.policies[path] = pol
    wh = mk_hook(c, slug, path)
    c.issue(O, slug, "ORD", title="X")
    must(c.patch_issue(O, slug, "ORD-1", {"title": "X1"}, version=1), 200, "patch 1")
    must(c.patch_issue(O, slug, "ORD-1", {"title": "X2"}, version=2), 200, "patch 2")
    c.issue(O, slug, "ORD", title="Y")
    c.rcv.wait_path(path, lambda rs: len([r for r in rs if ev_issue_key(r) == "ORD-1"]) >= 8, 25)
    time.sleep(0.3)
    recs = c.rcv.recs(path)
    x = [r for r in recs if ev_issue_key(r) == "ORD-1"]
    y = [r for r in recs if ev_issue_key(r) == "ORD-2"]
    xc = [r for r in x if r["json"].get("event") == "issue.created"]
    xu = [r for r in x if r["json"].get("event") == "issue.updated"]
    t.eq("X: created attempted 6 times, then both updates delivered", (len(xc), len(xu)), (6, 2))
    seq = [r["json"].get("id") for r in x]
    groups = [k for k, _ in itertools.groupby(seq)]
    t.ok("X: attempts of different deliveries never interleave", len(groups) == len(set(groups)), seq)
    t.ok("X: first update attempted only after created failed for good",
         len(xc) == 6 and xu and xu[0]["t"] > xc[-1]["t"], [(r["json"].get("event"), round(r["t"], 3)) for r in x])
    t.eq("X: updates delivered in event order", [((r["json"].get("data") or {}).get("issue") or {}).get("title")
                                                 for r in xu], ["X1", "X2"])
    t.ok("Y is not blocked by X's failing delivery", y and len(xc) == 6 and y[0]["t"] < xc[-1]["t"],
         ([round(r["t"], 3) for r in y], [round(r["t"], 3) for r in xc]))
    dl = settled_deliveries(c, slug, wh["id"], O, 4)
    st = sorted((d.get("event"), d.get("status"), d.get("attempts")) for d in dl)
    t.eq("deliveries: X created failed (6), the rest succeeded",
         st, sorted([("issue.created", "failed", 6), ("issue.updated", "succeeded", 1),
                     ("issue.updated", "succeeded", 1), ("issue.created", "succeeded", 1)]))


@G.test
def no_events_for_failed_operations(c):
    t = c.t
    f = c.fx
    O = f["O"]
    slug = wh_org(c, "NEG")
    c.project(O, slug, "NEGP", visibility="private")
    c.pmember(O, slug, "NEGP", f["M"], "viewer")
    path = f"/neg-{slug}"
    mk_hook(c, slug, path)
    c.issue(O, slug, "NEG", title="neg")
    c.issue(O, slug, "NEGP", title="neg private")
    c.rcv.wait_path(path, lambda rs: len(rs) >= 2, 10)
    u = f"/orgs/{slug}/issues/NEG-1"
    rs = [
        c.api.patch(u, {"title": "stale"}, O, headers={"If-Match": '"5"'}),
        c.api.patch(u, {"title": "no if-match"}, O),
        c.api.patch(u, {"title": ""}, O, headers={"If-Match": '"1"'}),
        c.transition(O, slug, "NEG-1", "done"),
        c.transition(O, slug, "NEG-1", "in_progress", {"If-Match": '"9"'}),
        c.api.post(f"/orgs/{slug}/projects/NEG/issues", {"type": "nope", "title": "bad"}, O),
        c.api.post(f"/orgs/{slug}/projects/NEG/issues/bulk", {"issues": [{"type": "task", "title": "ok"},
                                                                          {"type": "task"}]}, O),
        c.api.post(f"/orgs/{slug}/issues/NEG-1/comments", {"body": ""}, O),
        c.api.patch(f"/orgs/{slug}/issues/NEGP-1", {"title": "viewer"}, f["M"], headers={"If-Match": '"1"'}),
        c.api.delete(f"/orgs/{slug}/issues/NEGP-1", f["M"]),
    ]
    t.ok("all the failing operations failed", all(r.status >= 400 for r in rs), [r.status for r in rs])
    must(c.patch_issue(O, slug, "NEG-1", {"title": "marker"}, version=1), 200, "marker")
    must(c.patch_issue(O, slug, "NEGP-1", {"title": "marker"}, version=1), 200, "marker")
    c.rcv.wait_path(path, lambda rs: len(rs) >= 4, 10)
    time.sleep(0.5)
    got = sorted((r["json"].get("event"), ev_issue_key(r), ((r["json"].get("data") or {}).get("issue") or {})
                  .get("title")) for r in c.rcv.recs(path))
    t.eq("only the successful operations produced events", got,
         sorted([("issue.created", "NEG-1", "neg"), ("issue.created", "NEGP-1", "neg private"),
                 ("issue.updated", "NEG-1", "marker"), ("issue.updated", "NEGP-1", "marker")]))


# ================================================================================================================
# 10. restart & durability (first phase runs with WEBHOOK_BACKOFF_SCALE=1 so the 6 attempts can't run out)

G = group("durability", env={"WEBHOOK_BACKOFF_SCALE": "1.0"})


@G.test
def restart(c):
    t = c.t
    rcv2 = Receiver()
    try:
        O = c.user("dur")
        slug = c.org(O, "dura")
        c.project(O, slug, "DUR")
        c.issue(O, slug, "DUR", title="before")
        must(c.api.post(f"/orgs/{slug}/issues/DUR-1/comments", {"body": "kept"}, O), 201, "comment")
        wh = must(c.api.post(f"/orgs/{slug}/webhooks", {"url": rcv2.url("/dur"), "events": ["issue.created"],
                                                        "secret": "dur-secret"}, O), 201, "webhook")
        rcv2.down()
        c.issue(O, slug, "DUR", title="while down")
        code = c.svc.stop()
        t.eq("SIGTERM with a pending webhook delivery → exit code 0", code, 0)
        rcv2.up()
        c.svc.start()
        r = c.api.get("/me", O)
        t.status("access token issued before the restart is still valid", r, 200)
        t.status("login works after restart", c.login(O.email), 200)
        t.status("refresh token issued before the restart still works",
                 c.api.post("/auth/refresh", {"refresh_token": O.refresh}), 200)
        i1 = c.api.get(f"/orgs/{slug}/issues/DUR-1", O)
        t.eq("data survives the restart", (i1.status, i1.get("title"), i1.get("comment_count")), (200, "before", 1))
        t.eq("issue created right before SIGTERM exists", c.api.get(f"/orgs/{slug}/issues/DUR-2", O).get("title"),
             "while down")
        t.eq("numbering continues after restart", c.issue(O, slug, "DUR", title="after").get("number"), 3)
        recs = rcv2.wait_path("/dur", lambda rs: rs if {ev_issue_key(r) for r in successes(rs)} >=
                              {"DUR-2", "DUR-3"} else None, 25) or rcv2.recs("/dur")
        keys = [ev_issue_key(r) for r in successes(recs)]
        t.ok("pending delivery is sent after the restart", "DUR-2" in keys, keys)
        t.ok("deliveries after the restart work", "DUR-3" in keys, keys)
        t.ok("pending delivery signed correctly", all(sig_ok(r, "dur-secret") for r in recs), "")
        t.eq("the pending delivery is delivered successfully exactly once", keys.count("DUR-2"), 1)
        dl = settled_deliveries(c, slug, wh["id"], O, 2)
        t.eq("deliveries listing shows both succeeded", sorted(d.get("status") for d in dl),
             ["succeeded", "succeeded"])
        c.svc.stop()
        c.svc.start()
        t.status("restart with unchanged schema (idempotent migration) → ready", c.api.get("/readyz"), 200)
        t.eq("data still there after second restart", c.api.get(f"/orgs/{slug}/issues/DUR-3", O).get("title"),
             "after")
    finally:
        rcv2.down()


# ================================================================================================================
# 11. pagination parameters

G = group("pagination")


@G.setup
def pagination_setup(c):
    U = c.user("pg")
    others = [c.user("pgm") for _ in range(5)]
    for s in ["pg-e", "pg-a", "pg-d", "pg-b", "pg-c", "pg-g", "pg-f"]:
        c.org(U, s)
    for o in others:
        c.add_member(U, "pg-a", o)
    for k in ["PE", "PA", "PD", "PB", "PC"]:
        c.project(U, "pg-a", k)
    must(c.api.post("/orgs/pg-a/projects/PA/issues/bulk",
                    {"issues": [{"type": "task", "title": f"i{i}"} for i in range(60)]}, U), 201, "bulk")
    cids = [must(c.api.post("/orgs/pg-a/issues/PA-1/comments", {"body": f"c{i}"}, U), 201, "comment")["id"]
            for i in range(7)]
    for i in range(5):
        must(c.patch_issue(U, "pg-a", "PA-1", {"title": f"t{i}"}, version=i + 1), 200, "patch")
    wh = must(c.api.post("/orgs/pg-a/webhooks", {"url": c.rcv.url("/pag"), "events": ["issue.updated"]}, U), 201,
              "webhook")
    return {"U": U, "others": others, "cids": cids, "wh": wh["id"]}


@G.test
def limit_validation(c):
    f = c.fx
    U = f["U"]
    endpoints = ["/orgs", "/orgs/pg-a/members", "/orgs/pg-a/projects", "/orgs/pg-a/projects/PA/members",
                 "/orgs/pg-a/issues", "/orgs/pg-a/issues/PA-1/comments", "/orgs/pg-a/issues/PA-1/history",
                 "/orgs/pg-a/webhooks", f"/orgs/pg-a/webhooks/{f['wh']}/deliveries"]
    for ep in endpoints:
        c.t.vfail(f"{ep} limit=0 → 422", c.api.get(ep + "?limit=0", U), "limit", ANY)
        c.t.vfail(f"{ep} limit=101 → 422", c.api.get(ep + "?limit=101", U), "limit", ANY)
        r = c.api.get(ep + "?limit=100", U)
        c.t.ok(f"{ep} limit=100 → 200 with items/next_cursor", r.status == 200 and isinstance(r.get("items"), list)
               and "next_cursor" in (r.json or {}), f"{r.status} {r.text[:200]}")
    for v in ["-1", "abc", "1.5"]:
        c.t.vfail(f"/orgs limit={v!r} → 422", c.api.get(f"/orgs?limit={v}", U), "limit", ANY)


@G.test
def walks(c):
    t = c.t
    f = c.fx
    U = f["U"]
    items, pages, probs = c.walk(U, "/orgs", limit=3)
    t.eq("orgs: walk limit=3 → sorted by slug, each once, 3 pages",
         ([o.get("slug") for o in items], len(pages), probs), ([f"pg-{x}" for x in "abcdefg"], 3, []))
    r = c.api.get("/orgs", U)
    t.eq("orgs: everything fits in the default page → next_cursor null", (len(r.get("items") or []),
                                                                            r.get("next_cursor", "missing")), (7, None))
    items, pages, probs = c.walk(U, "/orgs/pg-a/members", limit=4)
    t.eq("members: walk limit=4 → sorted by email, each once",
         ([m.get("email") for m in items], len(pages), probs),
         (sorted([U.email] + [o.email for o in f["others"]]), 2, []))
    items, pages, probs = c.walk(U, "/orgs/pg-a/projects", limit=2)
    t.eq("projects: walk limit=2 → sorted by key, each once",
         ([p.get("key") for p in items], len(pages), probs), (["PA", "PB", "PC", "PD", "PE"], 3, []))
    items, pages, probs = c.walk(U, "/orgs/pg-a/issues/PA-1/comments", limit=3)
    t.eq("comments: walk limit=3 → oldest first, each once", ([x.get("id") for x in items], len(pages), probs),
         (f["cids"], 3, []))
    items, pages, probs = c.walk(U, "/orgs/pg-a/issues/PA-1/history", limit=4)
    t.eq("history: walk limit=4 → 6 entries, creation last", (len(items), len(pages), probs,
                                                               (items[-1].get("changes") if items else None)),
         (6, 2, [], [{"field": "created", "from": None, "to": None}]))
    r = c.api.get("/orgs/pg-a/issues" + qs({"project": "PA"}), U)
    t.eq("issues: default limit 50", (len(r.get("items") or []), r.get("total"), isinstance(r.get("next_cursor"), str)),
         (50, 60, True))
    r2 = c.api.get("/orgs/pg-a/issues" + qs({"project": "PA", "cursor": r.get("next_cursor") or ""}), U)
    keys = [i.get("key") for i in (r.get("items") or []) + (r2.get("items") or [])]
    t.eq("issues: second page completes the list, next_cursor null",
         (len(r2.get("items") or []), r2.get("next_cursor", "missing"), len(set(keys)), r2.get("total")),
         (10, None, 60, 60))
    items, pages, probs = c.walk(U, "/orgs/pg-a/issues", {"sort": "key"}, limit=1)
    t.eq("issues: walk limit=1 visits each once in key order", [i.get("key") for i in items],
         [f"PA-{n}" for n in range(1, 61)])


# ================================================================================================================
# runner

def run(args):
    t = Checker(args.verbose)
    binary = os.path.abspath(args.binary)
    if not (os.path.isfile(binary) and os.access(binary, os.X_OK)):
        print(f"FAIL harness: {binary} is not an executable file")
        print("0 passed, 1 failed")
        return 1
    try:
        ensure_db(args.db)
    except Exception as e:  # noqa: BLE001
        print(f"FAIL harness: cannot prepare database {args.db!r}: {e}")
        print("0 passed, 1 failed")
        return 1
    logfd, logpath = tempfile.mkstemp(prefix="tracker-service-", suffix=".log")
    os.close(logfd)
    svc = Service(binary, args.db, logpath)
    api = Api(svc)
    rcv = Receiver()
    started = time.time()
    for g in GROUPS:
        tests = [fn for fn in g.tests if not args.k or args.k in f"{g.name}.{fn.__name__}"]
        if not tests:
            continue
        t.prefix = g.name
        gstart = time.time()
        try:
            reset_db(args.db)
            svc.start(g.env)
        except Exception as e:  # noqa: BLE001
            t.ok("database reset and service start", False, f"{e}\n{svc.tail()}")
            svc.stop()
            continue
        c = Ctx(api, svc, t, rcv, g)
        if g.setup_fn:
            t.prefix = f"{g.name}.setup"
            try:
                c.fx = g.setup_fn(c)
            except Exception as e:  # noqa: BLE001
                t.ok("group setup", False, f"{type(e).__name__}: {e}\n" +
                     (traceback.format_exc() if args.verbose else "") + ("" if svc.alive() else svc.tail()))
                svc.stop()
                continue
        for fn in tests:
            t.prefix = f"{g.name}.{fn.__name__}"
            if not svc.alive():
                t.ok("service is running", False, f"service died before this test\n{svc.tail()}")
                try:
                    svc.stop()
                    svc.start()
                except Exception as e:  # noqa: BLE001
                    t.ok("service restart", False, f"{e}\n{svc.tail()}")
                    break
            try:
                fn(c)
            except Exception as e:  # noqa: BLE001
                t.ok("test completed without crashing", False, f"{type(e).__name__}: {e}\n" +
                     (traceback.format_exc() if args.verbose else "") + ("" if svc.alive() else svc.tail()))
        t.prefix = g.name
        was_alive = svc.alive()
        code = svc.stop()
        if not t.eq("service exits with code 0 on SIGTERM", code if was_alive else f"died earlier ({code})", 0):
            print(svc.tail())
        if args.verbose:
            print(f"-- group {g.name}: {time.time() - gstart:.1f}s", flush=True)
    rcv.down()
    t.prefix = ""
    if args.verbose:
        print(f"-- total {time.time() - started:.1f}s; service log: {logpath}")
    elif t.failed:
        print(f"service log: {logpath}")
    print(f"{t.passed} passed, {t.failed} failed")
    return 1 if t.failed else 0


def main():
    ap = argparse.ArgumentParser(description="Tracker acceptance tests")
    ap.add_argument("binary")
    ap.add_argument("--db", default="tracker")
    ap.add_argument("-k", default=None, help="only run tests whose 'group.test' name contains this substring")
    ap.add_argument("-v", action="store_true", dest="verbose")
    args = ap.parse_args()
    try:
        sys.exit(run(args))
    except KeyboardInterrupt:
        sys.exit(130)


if __name__ == "__main__":
    main()
