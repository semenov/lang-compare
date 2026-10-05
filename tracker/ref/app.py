#!/usr/bin/env python3
"""Tracker -- reference implementation of SPEC.md (correctness over speed).

Stdlib HTTP server (thread per connection) + psycopg3 pool + argon2-cffi.
"""
import base64
import datetime as dt
import hashlib
import hmac
import http.client
import json
import os
import re
import secrets
import signal
import socket
import sys
import threading
import time
import traceback
import unicodedata
import urllib.parse
import uuid
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import psycopg
from psycopg.rows import dict_row
from psycopg.types.json import Jsonb
from psycopg_pool import ConnectionPool
from argon2 import PasswordHasher, Type
from argon2.exceptions import Argon2Error, InvalidHashError

# ---------------------------------------------------------------------------
# config / globals

PORT = 8080
JWT_SECRET = b""
BACKOFF_SCALE = 1.0
POOL: ConnectionPool = None  # type: ignore
WORKER = None
STOPPING = threading.Event()

PH = PasswordHasher(time_cost=2, memory_cost=19456, parallelism=1, hash_len=32, salt_len=16, type=Type.ID)
DUMMY_HASH = None

ACCESS_TTL = 900
REFRESH_TTL = dt.timedelta(days=30)
IDEMPOTENCY_TTL = dt.timedelta(hours=24)

ORG_ROLES = ("owner", "admin", "member")
ORG_RANK = {"member": 1, "admin": 2, "owner": 3}
PROJ_ROLES = ("admin", "developer", "viewer")
PROJ_RANK = {"viewer": 1, "developer": 2, "admin": 3}
PROJ_BY_RANK = {1: "viewer", 2: "developer", 3: "admin"}
TYPES = ("task", "bug", "story")
STATUSES = ("todo", "in_progress", "done")
PRIORITIES = ("highest", "high", "medium", "low", "lowest")
PRIORITY_RANK = {"lowest": 1, "low": 2, "medium": 3, "high": 4, "highest": 5}
WORKFLOW = {("todo", "in_progress"), ("in_progress", "todo"), ("in_progress", "done"), ("done", "in_progress")}
EVENTS = ("issue.created", "issue.updated", "issue.deleted", "comment.created")
VISIBILITIES = ("org", "private")
RETRY_DELAYS = (1, 2, 4, 8, 16)
MAX_ATTEMPTS = 6

SLUG_RE = re.compile(r"[a-z0-9][a-z0-9-]{1,38}[a-z0-9]")
PKEY_RE = re.compile(r"[A-Z][A-Z0-9]{1,9}")
LABEL_RE = re.compile(r"[a-z0-9][a-z0-9_.-]{0,49}")
ISSUE_KEY_RE = re.compile(r"([A-Za-z][A-Za-z0-9]{1,9})-([0-9]{1,9})")
DATE_RE = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}")
TS_RE = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt ][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-][0-9]{2}:[0-9]{2})")

MISSING = object()


def log(*a):
    print(*a, file=sys.stderr, flush=True)


# ---------------------------------------------------------------------------
# helpers


def now():
    t = dt.datetime.now(dt.UTC)
    return t.replace(microsecond=(t.microsecond // 1000) * 1000)


def fmt_ts(t):
    if t is None:
        return None
    t = t.astimezone(dt.UTC)
    return t.strftime("%Y-%m-%dT%H:%M:%S.") + f"{t.microsecond // 1000:03d}Z"


def sid(u):
    return None if u is None else str(u)


def parse_uuid(s):
    if not isinstance(s, str):
        return None
    try:
        u = uuid.UUID(s)
    except ValueError:
        return None
    # require canonical-ish form (36 chars with hyphens)
    if len(s) != 36:
        return None
    return u


def search_words(*texts):
    out = []
    for text in texts:
        if not text:
            continue
        cur = []
        for ch in text:
            c = unicodedata.category(ch)
            if c[0] == "L" or c == "Nd":
                cur.append(ch)
            elif cur:
                out.append("".join(cur).casefold())
                cur = []
        if cur:
            out.append("".join(cur).casefold())
    return out


def b64u(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def b64u_dec(s):
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


class ApiError(Exception):
    def __init__(self, status, code, detail=None, errors=None):
        super().__init__(code)
        self.status = status
        self.code = code
        self.detail = detail or code.replace("_", " ")
        self.errors = errors


def not_found(what="resource"):
    return ApiError(404, "not_found", f"{what} not found")


def forbidden(detail="not allowed"):
    return ApiError(403, "forbidden", detail)


def vfail(field, code):
    return ApiError(422, "validation_failed", "request validation failed", [{"field": field, "code": code}])


class V:
    """Validation error collector."""

    def __init__(self, prefix=""):
        self.prefix = prefix
        self.errors = []

    def add(self, field, code):
        self.errors.append({"field": self.prefix + field, "code": code})

    def check(self):
        if self.errors:
            raise ApiError(422, "validation_failed", "request validation failed", self.errors)


def v_str(v, body, field, *, required=False, nullable=False, min_len=None, max_len=None, trim=False):
    """Returns MISSING (absent or invalid), None (null, if nullable) or the string."""
    if field not in body:
        if required:
            v.add(field, "required")
        return MISSING
    val = body[field]
    if val is None:
        if nullable:
            return None
        v.add(field, "required" if required else "invalid")
        return MISSING
    if not isinstance(val, str):
        v.add(field, "invalid")
        return MISSING
    if trim:
        val = val.strip()
    if min_len is not None and len(val) < min_len:
        v.add(field, "too_short")
        return MISSING
    if max_len is not None and len(val) > max_len:
        v.add(field, "too_long")
        return MISSING
    return val


def v_enum(v, body, field, choices, *, required=False):
    if field not in body:
        if required:
            v.add(field, "required")
        return MISSING
    val = body[field]
    if val is None:
        v.add(field, "required" if required else "invalid")
        return MISSING
    if not isinstance(val, str) or val not in choices:
        v.add(field, "invalid")
        return MISSING
    return val


def v_bool(v, body, field):
    if field not in body:
        return MISSING
    val = body[field]
    if not isinstance(val, bool):
        v.add(field, "invalid")
        return MISSING
    return val


def v_labels(v, body, field="labels"):
    if field not in body:
        return MISSING
    val = body[field]
    if not isinstance(val, list):
        v.add(field, "invalid")
        return MISSING
    out = set()
    for x in val:
        if not isinstance(x, str):
            v.add(field, "invalid")
            return MISSING
        x = x.lower()
        if not LABEL_RE.fullmatch(x):
            v.add(field, "invalid")
            return MISSING
        out.add(x)
    if len(out) > 20:
        v.add(field, "too_long")
        return MISSING
    return sorted(out)


def v_date(v, body, field):
    if field not in body:
        return MISSING
    val = body[field]
    if val is None:
        return None
    if not isinstance(val, str) or not DATE_RE.fullmatch(val):
        v.add(field, "invalid")
        return MISSING
    try:
        return dt.date.fromisoformat(val)
    except ValueError:
        v.add(field, "invalid")
        return MISSING


def v_uuid(v, body, field, *, nullable=True):
    if field not in body:
        return MISSING
    val = body[field]
    if val is None:
        if nullable:
            return None
        v.add(field, "invalid")
        return MISSING
    u = parse_uuid(val)
    if u is None:
        v.add(field, "invalid")
        return MISSING
    return u


def parse_ts(s):
    s = s.replace(" ", "+") if "T" in s or "t" in s else s
    if not TS_RE.fullmatch(s):
        return None
    try:
        s2 = s.upper().replace("Z", "+00:00")
        t = dt.datetime.fromisoformat(s2)
    except ValueError:
        return None
    if t.tzinfo is None:
        return None
    return t


# ---------------------------------------------------------------------------
# JWT


def make_access_token(user_id):
    iat = int(time.time())
    header = b64u(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
    payload = b64u(json.dumps({"sub": str(user_id), "iat": iat, "exp": iat + ACCESS_TTL, "typ": "access"},
                              separators=(",", ":")).encode())
    signing = f"{header}.{payload}".encode()
    sig = b64u(hmac.new(JWT_SECRET, signing, hashlib.sha256).digest())
    return f"{header}.{payload}.{sig}"


def verify_access_token(tok):
    """Returns user UUID; raises ApiError 401."""
    unauth = ApiError(401, "unauthenticated", "missing or invalid access token")
    parts = tok.split(".")
    if len(parts) != 3:
        raise unauth
    try:
        header = json.loads(b64u_dec(parts[0]))
        sig = b64u_dec(parts[2])
    except Exception:
        raise unauth
    if not isinstance(header, dict) or header.get("alg") != "HS256":
        raise unauth
    expect = hmac.new(JWT_SECRET, f"{parts[0]}.{parts[1]}".encode(), hashlib.sha256).digest()
    if not hmac.compare_digest(expect, sig):
        raise unauth
    try:
        payload = json.loads(b64u_dec(parts[1]))
    except Exception:
        raise unauth
    if not isinstance(payload, dict) or payload.get("typ") != "access":
        raise unauth
    exp = payload.get("exp")
    if not isinstance(exp, (int, float)) or isinstance(exp, bool):
        raise unauth
    uid = parse_uuid(payload.get("sub"))
    if uid is None:
        raise unauth
    if time.time() >= exp:
        raise ApiError(401, "token_expired", "access token expired")
    return uid


# ---------------------------------------------------------------------------
# schema

SCHEMA = """
CREATE TABLE IF NOT EXISTS users (
  id uuid PRIMARY KEY,
  email text COLLATE "C" NOT NULL UNIQUE,
  name text NOT NULL,
  password_hash text NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS refresh_tokens (
  id uuid PRIMARY KEY,
  token_hash text NOT NULL UNIQUE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  chain_id uuid NOT NULL,
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  used_at timestamptz,
  revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS refresh_tokens_chain_idx ON refresh_tokens(chain_id);
CREATE TABLE IF NOT EXISTS orgs (
  id uuid PRIMARY KEY,
  slug text COLLATE "C" NOT NULL UNIQUE,
  name text NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS org_members (
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role text NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS org_members_user_idx ON org_members(user_id);
CREATE TABLE IF NOT EXISTS projects (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  key text COLLATE "C" NOT NULL,
  name text NOT NULL,
  description text,
  visibility text NOT NULL,
  created_at timestamptz NOT NULL,
  issue_seq integer NOT NULL DEFAULT 0,
  UNIQUE (org_id, key)
);
CREATE TABLE IF NOT EXISTS project_members (
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role text NOT NULL,
  PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS project_members_user_idx ON project_members(user_id);
CREATE TABLE IF NOT EXISTS issues (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  project_key text COLLATE "C" NOT NULL,
  number integer NOT NULL,
  type text NOT NULL,
  title text NOT NULL,
  description text,
  status text NOT NULL,
  priority text NOT NULL,
  priority_rank smallint NOT NULL,
  assignee_id uuid,
  reporter_id uuid NOT NULL,
  labels text[] NOT NULL,
  due_date date,
  version integer NOT NULL,
  comment_count integer NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  resolved_at timestamptz,
  words text[] NOT NULL,
  UNIQUE (project_id, number)
);
CREATE INDEX IF NOT EXISTS issues_org_created_idx ON issues(org_id, created_at);
CREATE INDEX IF NOT EXISTS issues_assignee_idx ON issues(assignee_id);
CREATE INDEX IF NOT EXISTS issues_words_idx ON issues USING gin(words);
CREATE INDEX IF NOT EXISTS issues_labels_idx ON issues USING gin(labels);
CREATE TABLE IF NOT EXISTS comments (
  id uuid PRIMARY KEY,
  seq bigserial,
  org_id uuid NOT NULL,
  issue_id uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  author_id uuid NOT NULL,
  body text NOT NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  edited boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS comments_issue_idx ON comments(issue_id, seq);
CREATE TABLE IF NOT EXISTS issue_history (
  id uuid PRIMARY KEY,
  seq bigserial,
  issue_id uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  created_at timestamptz NOT NULL,
  changes jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS issue_history_issue_idx ON issue_history(issue_id, seq);
CREATE TABLE IF NOT EXISTS idempotency_keys (
  user_id uuid NOT NULL,
  key text NOT NULL,
  fingerprint text NOT NULL,
  status integer,
  body text,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (user_id, key)
);
CREATE TABLE IF NOT EXISTS webhooks (
  id uuid PRIMARY KEY,
  seq bigserial,
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  url text NOT NULL,
  events text[] NOT NULL,
  secret text NOT NULL,
  active boolean NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id uuid PRIMARY KEY,
  seq bigserial,
  webhook_id uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
  event text NOT NULL,
  issue_id uuid NOT NULL,
  body text NOT NULL,
  status text NOT NULL,
  attempts integer NOT NULL DEFAULT 0,
  last_status_code integer,
  created_at timestamptz NOT NULL,
  next_attempt_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS webhook_deliveries_pending_idx ON webhook_deliveries(webhook_id, issue_id, seq)
  WHERE status = 'pending';
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS lease_until timestamptz;
CREATE INDEX IF NOT EXISTS webhook_deliveries_hook_idx ON webhook_deliveries(webhook_id, seq);
"""


def migrate():
    with POOL.connection() as conn:
        conn.execute("SELECT pg_advisory_xact_lock(727274)")
        for stmt in SCHEMA.split(";"):
            if stmt.strip():
                conn.execute(stmt)


# ---------------------------------------------------------------------------
# request plumbing


class Raw:
    """Pre-serialized JSON body."""

    def __init__(self, data: bytes):
        self.data = data


class Req:
    def __init__(self, method, path, query, headers, raw):
        self.method = method
        self.path = path
        self.query = query
        self.headers = headers
        self.raw = raw
        self.uid = None
        self._json = MISSING
        self.wake_worker = False

    def json(self):
        if self._json is MISSING:
            if not self.raw or not self.raw.strip():
                raise ApiError(400, "bad_request", "request body required")
            try:
                obj = json.loads(self.raw.decode("utf-8"))
            except (ValueError, UnicodeDecodeError):
                raise ApiError(400, "bad_request", "malformed JSON")
            if not isinstance(obj, dict):
                raise ApiError(400, "bad_request", "request body must be a JSON object")
            self._json = obj
        return self._json

    def q(self, name):
        vals = self.query.get(name)
        if not vals:
            return None
        return vals[0]

    def header(self, name):
        return self.headers.get(name)


def page_params(req):
    limit = 50
    s = req.q("limit")
    if s is not None:
        try:
            limit = int(s.strip())
        except ValueError:
            raise vfail("limit", "invalid")
        if limit < 1 or limit > 100:
            raise vfail("limit", "out_of_range")
    return limit


def dec_cursor(req):
    c = req.q("cursor")
    if c is None or c == "":
        return None
    try:
        obj = json.loads(b64u_dec(c))
        if not isinstance(obj, dict):
            raise ValueError
        return obj
    except Exception:
        raise vfail("cursor", "invalid")


def enc_cursor(obj):
    return b64u(json.dumps(obj, separators=(",", ":")).encode())


def offset_page(req):
    limit = page_params(req)
    cur = dec_cursor(req)
    off = 0
    if cur is not None:
        off = cur.get("o")
        if not isinstance(off, int) or isinstance(off, bool) or off < 0:
            raise vfail("cursor", "invalid")
    return limit, off


def offset_result(items, limit, off):
    nxt = None
    if len(items) > limit:
        items = items[:limit]
        nxt = enc_cursor({"o": off + limit})
    return {"items": items, "next_cursor": nxt}


# ---------------------------------------------------------------------------
# representations


def user_json(r):
    return {"id": str(r["id"]), "email": r["email"], "name": r["name"], "created_at": fmt_ts(r["created_at"])}


def org_json(r):
    return {"id": str(r["id"]), "slug": r["slug"], "name": r["name"], "created_at": fmt_ts(r["created_at"]),
            "my_role": r["my_role"]}


def member_json(r):
    return {"user_id": str(r["user_id"]), "email": r["email"], "name": r["name"], "role": r["role"]}


def project_json(r):
    return {"id": str(r["id"]), "key": r["key"], "name": r["name"], "description": r["description"],
            "visibility": r["visibility"], "created_at": fmt_ts(r["created_at"]), "my_role": r["my_role"]}


def issue_key(r):
    return f"{r['project_key']}-{r['number']}"


def issue_json(r):
    return {
        "id": str(r["id"]),
        "key": issue_key(r),
        "number": r["number"],
        "project_key": r["project_key"],
        "type": r["type"],
        "title": r["title"],
        "description": r["description"],
        "status": r["status"],
        "priority": r["priority"],
        "assignee_id": sid(r["assignee_id"]),
        "reporter_id": str(r["reporter_id"]),
        "labels": list(r["labels"]),
        "due_date": r["due_date"].isoformat() if r["due_date"] else None,
        "version": r["version"],
        "comment_count": r["comment_count"],
        "created_at": fmt_ts(r["created_at"]),
        "updated_at": fmt_ts(r["updated_at"]),
        "resolved_at": fmt_ts(r["resolved_at"]),
    }


def comment_json(r, ikey):
    return {"id": str(r["id"]), "issue_key": ikey, "author_id": str(r["author_id"]), "body": r["body"],
            "created_at": fmt_ts(r["created_at"]), "updated_at": fmt_ts(r["updated_at"]), "edited": r["edited"]}


def webhook_json(r):
    return {"id": str(r["id"]), "url": r["url"], "events": list(r["events"]), "active": r["active"],
            "created_at": fmt_ts(r["created_at"])}


def delivery_json(r):
    return {"id": str(r["id"]), "event": r["event"], "status": r["status"], "attempts": r["attempts"],
            "last_status_code": r["last_status_code"], "created_at": fmt_ts(r["created_at"])}


# ---------------------------------------------------------------------------
# access helpers


def eff_role(org_role, proj_role, visibility):
    best = PROJ_RANK.get(proj_role, 0) if proj_role else 0
    if org_role in ("owner", "admin"):
        best = 3
    elif org_role == "member" and visibility == "org":
        best = max(best, 2)
    return PROJ_BY_RANK.get(best)


def get_org(conn, req, slug, lock=False):
    r = conn.execute(
        "SELECT o.*, m.role AS my_role FROM orgs o JOIN org_members m ON m.org_id = o.id AND m.user_id = %s "
        "WHERE o.slug = %s" + (" FOR UPDATE OF o" if lock else ""),
        (req.uid, slug.lower())).fetchone()
    if not r:
        raise not_found("organization")
    return r


def require_org_role(org, min_role):
    if ORG_RANK[org["my_role"]] < ORG_RANK[min_role]:
        raise forbidden(f"requires org role {min_role}")


def get_project(conn, req, org, key):
    r = conn.execute(
        "SELECT p.*, pm.role AS proj_role FROM projects p "
        "LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = %s "
        "WHERE p.org_id = %s AND p.key = %s", (req.uid, org["id"], key.upper())).fetchone()
    if not r:
        raise not_found("project")
    role = eff_role(org["my_role"], r["proj_role"], r["visibility"])
    if not role:
        raise not_found("project")
    r["my_role"] = role
    return r


def require_proj_role(proj, min_role):
    if PROJ_RANK[proj["my_role"]] < PROJ_RANK[min_role]:
        raise forbidden(f"requires project role {min_role}")


def get_issue(conn, req, slug, ikey, min_role, lock=False):
    org = get_org(conn, req, slug)
    m = ISSUE_KEY_RE.fullmatch(ikey)
    if not m:
        raise not_found("issue")
    proj = get_project(conn, req, org, m.group(1))
    issue = conn.execute("SELECT * FROM issues WHERE project_id = %s AND number = %s" + (" FOR UPDATE" if lock else ""),
                         (proj["id"], int(m.group(2)))).fetchone()
    if not issue:
        raise not_found("issue")
    require_proj_role(proj, min_role)
    return org, proj, issue


def user_role_in_project(conn, proj, user_id):
    r = conn.execute(
        "SELECT om.role AS org_role, pm.role AS proj_role FROM org_members om "
        "LEFT JOIN project_members pm ON pm.project_id = %s AND pm.user_id = om.user_id "
        "WHERE om.org_id = %s AND om.user_id = %s", (proj["id"], proj["org_id"], user_id)).fetchone()
    if not r:
        return None
    return eff_role(r["org_role"], r["proj_role"], proj["visibility"])


def can_be_assignee(conn, proj, user_id, cache=None):
    if cache is not None and user_id in cache:
        return cache[user_id]
    role = user_role_in_project(conn, proj, user_id)
    ok = role is not None and PROJ_RANK[role] >= PROJ_RANK["developer"]
    if cache is not None:
        cache[user_id] = ok
    return ok


# ---------------------------------------------------------------------------
# events / history


def emit(conn, req, org, event, issue_id, data, actor_id):
    hooks = conn.execute("SELECT id FROM webhooks WHERE org_id = %s AND active AND %s = ANY(events)",
                         (org["id"], event)).fetchall()
    if not hooks:
        return
    ts = now()
    for h in hooks:
        did = uuid.uuid4()
        body = json.dumps({"id": str(did), "event": event, "created_at": fmt_ts(ts), "org": org["slug"],
                           "actor_id": str(actor_id), "data": data}, ensure_ascii=False, separators=(",", ":"))
        conn.execute(
            "INSERT INTO webhook_deliveries (id, webhook_id, event, issue_id, body, status, attempts, created_at, "
            "next_attempt_at) VALUES (%s, %s, %s, %s, %s, 'pending', 0, %s, %s)",
            (did, h["id"], event, issue_id, body, ts, ts))
    req.wake_worker = True


def add_history(conn, issue_id, actor_id, changes, ts):
    conn.execute("INSERT INTO issue_history (id, issue_id, actor_id, created_at, changes) VALUES (%s,%s,%s,%s,%s)",
                 (uuid.uuid4(), issue_id, actor_id, ts, Jsonb(changes)))


# ---------------------------------------------------------------------------
# auth handlers


def h_register(req, conn):
    body = req.json()
    v = V()
    email = v_str(v, body, "email", required=True, max_len=254)
    if isinstance(email, str):
        email = email.strip().lower()
        if "@" not in email:
            v.add("email", "invalid")
    password = v_str(v, body, "password", required=True, min_len=10, max_len=128)
    name = v_str(v, body, "name", required=True, min_len=1, max_len=100, trim=True)
    v.check()
    pw_hash = PH.hash(password)
    uid = uuid.uuid4()
    ts = now()
    r = conn.execute("INSERT INTO users (id, email, name, password_hash, created_at) VALUES (%s,%s,%s,%s,%s) "
                     "ON CONFLICT (email) DO NOTHING RETURNING *", (uid, email, name, pw_hash, ts)).fetchone()
    if not r:
        raise ApiError(409, "email_taken", "email already registered")
    return 201, user_json(r)


def issue_tokens(conn, user_id, chain_id):
    tok = secrets.token_urlsafe(32)
    ts = now()
    conn.execute("INSERT INTO refresh_tokens (id, token_hash, user_id, chain_id, created_at, expires_at) "
                 "VALUES (%s,%s,%s,%s,%s,%s)",
                 (uuid.uuid4(), hashlib.sha256(tok.encode()).hexdigest(), user_id, chain_id, ts, ts + REFRESH_TTL))
    return {"access_token": make_access_token(user_id), "refresh_token": tok, "token_type": "Bearer",
            "expires_in": ACCESS_TTL}


def h_login(req, conn):
    body = req.json()
    v = V()
    email = v_str(v, body, "email", required=True)
    password = v_str(v, body, "password", required=True)
    v.check()
    r = conn.execute("SELECT * FROM users WHERE email = %s", (email.strip().lower(),)).fetchone()
    ok = False
    try:
        ok = PH.verify(r["password_hash"] if r else DUMMY_HASH, password)
    except (Argon2Error, InvalidHashError):
        ok = False
    if not r or not ok:
        raise ApiError(401, "invalid_credentials", "wrong email or password")
    return 200, issue_tokens(conn, r["id"], uuid.uuid4())


def get_refresh_body(req):
    body = req.json()
    v = V()
    tok = v_str(v, body, "refresh_token", required=True)
    v.check()
    return tok


def h_refresh(req, conn):
    tok = get_refresh_body(req)
    th = hashlib.sha256(tok.encode()).hexdigest()
    r = conn.execute("SELECT * FROM refresh_tokens WHERE token_hash = %s FOR UPDATE", (th,)).fetchone()
    if not r:
        raise ApiError(401, "invalid_token", "unknown refresh token")
    if r["used_at"] is not None:
        conn.execute("UPDATE refresh_tokens SET revoked_at = %s WHERE chain_id = %s AND revoked_at IS NULL",
                     (now(), r["chain_id"]))
        # commit the revocation even though we answer with an error
        conn.commit()
        raise ApiError(401, "token_reused", "refresh token already used; session revoked")
    if r["revoked_at"] is not None or r["expires_at"] <= dt.datetime.now(dt.UTC):
        raise ApiError(401, "invalid_token", "refresh token expired or revoked")
    conn.execute("UPDATE refresh_tokens SET used_at = %s WHERE id = %s", (now(), r["id"]))
    return 200, issue_tokens(conn, r["user_id"], r["chain_id"])


def h_logout(req, conn):
    tok = get_refresh_body(req)
    th = hashlib.sha256(tok.encode()).hexdigest()
    r = conn.execute("SELECT * FROM refresh_tokens WHERE token_hash = %s", (th,)).fetchone()
    if not r:
        raise ApiError(401, "invalid_token", "unknown refresh token")
    conn.execute("UPDATE refresh_tokens SET revoked_at = %s WHERE chain_id = %s AND revoked_at IS NULL",
                 (now(), r["chain_id"]))
    return 204, None


def h_me(req, conn):
    r = conn.execute("SELECT * FROM users WHERE id = %s", (req.uid,)).fetchone()
    return 200, user_json(r)


def h_me_patch(req, conn):
    body = req.json()
    v = V()
    name = v_str(v, body, "name", min_len=1, max_len=100, trim=True)
    v.check()
    if name is not MISSING:
        conn.execute("UPDATE users SET name = %s WHERE id = %s", (name, req.uid))
    return h_me(req, conn)


# ---------------------------------------------------------------------------
# orgs


def h_org_create(req, conn):
    body = req.json()
    v = V()
    name = v_str(v, body, "name", required=True, min_len=1, max_len=100, trim=True)
    slug = v_str(v, body, "slug", required=True)
    if isinstance(slug, str) and not SLUG_RE.fullmatch(slug):
        v.add("slug", "invalid")
    v.check()
    oid = uuid.uuid4()
    ts = now()
    r = conn.execute("INSERT INTO orgs (id, slug, name, created_at) VALUES (%s,%s,%s,%s) "
                     "ON CONFLICT (slug) DO NOTHING RETURNING *", (oid, slug, name, ts)).fetchone()
    if not r:
        raise ApiError(409, "slug_taken", "slug already taken")
    conn.execute("INSERT INTO org_members (org_id, user_id, role, created_at) VALUES (%s,%s,'owner',%s)",
                 (oid, req.uid, ts))
    r["my_role"] = "owner"
    return 201, org_json(r)


def h_orgs_list(req, conn):
    limit, off = offset_page(req)
    rows = conn.execute(
        "SELECT o.*, m.role AS my_role FROM orgs o JOIN org_members m ON m.org_id = o.id AND m.user_id = %s "
        "ORDER BY o.slug LIMIT %s OFFSET %s", (req.uid, limit + 1, off)).fetchall()
    return 200, offset_result([org_json(r) for r in rows], limit, off)


def h_org_get(req, conn, slug):
    return 200, org_json(get_org(conn, req, slug))


def h_members_list(req, conn, slug):
    org = get_org(conn, req, slug)
    limit, off = offset_page(req)
    rows = conn.execute(
        "SELECT m.user_id, u.email, u.name, m.role FROM org_members m JOIN users u ON u.id = m.user_id "
        "WHERE m.org_id = %s ORDER BY u.email, u.id LIMIT %s OFFSET %s", (org["id"], limit + 1, off)).fetchall()
    return 200, offset_result([member_json(r) for r in rows], limit, off)


def h_member_add(req, conn, slug):
    org = get_org(conn, req, slug, lock=True)
    require_org_role(org, "admin")
    body = req.json()
    v = V()
    email = v_str(v, body, "email", required=True)
    role = v_enum(v, body, "role", ORG_ROLES, required=True)
    v.check()
    if role in ("owner", "admin") and org["my_role"] != "owner":
        raise forbidden("only owners may grant owner or admin")
    u = conn.execute("SELECT * FROM users WHERE email = %s", (email.strip().lower(),)).fetchone()
    if not u:
        raise vfail("email", "invalid")
    r = conn.execute("INSERT INTO org_members (org_id, user_id, role, created_at) VALUES (%s,%s,%s,%s) "
                     "ON CONFLICT DO NOTHING RETURNING role", (org["id"], u["id"], role, now())).fetchone()
    if not r:
        raise ApiError(409, "already_member", "user is already a member")
    return 201, {"user_id": str(u["id"]), "email": u["email"], "name": u["name"], "role": role}


def get_member(conn, org, user_id_s):
    uid = parse_uuid(user_id_s)
    if uid is None:
        raise not_found("member")
    r = conn.execute("SELECT m.user_id, u.email, u.name, m.role FROM org_members m JOIN users u ON u.id = m.user_id "
                     "WHERE m.org_id = %s AND m.user_id = %s", (org["id"], uid)).fetchone()
    if not r:
        raise not_found("member")
    return r


def owner_count(conn, org):
    return conn.execute("SELECT count(*) AS n FROM org_members WHERE org_id = %s AND role = 'owner'",
                        (org["id"],)).fetchone()["n"]


def h_member_patch(req, conn, slug, user_id):
    org = get_org(conn, req, slug, lock=True)
    require_org_role(org, "admin")
    target = get_member(conn, org, user_id)
    body = req.json()
    v = V()
    role = v_enum(v, body, "role", ORG_ROLES, required=True)
    v.check()
    if org["my_role"] != "owner" and (role in ("owner", "admin") or target["role"] in ("owner", "admin")):
        if role != target["role"]:
            raise forbidden("only owners may grant or take away owner/admin")
    if target["role"] == "owner" and role != "owner" and owner_count(conn, org) <= 1:
        raise ApiError(409, "last_owner", "cannot demote the last owner")
    conn.execute("UPDATE org_members SET role = %s WHERE org_id = %s AND user_id = %s",
                 (role, org["id"], target["user_id"]))
    target["role"] = role
    return 200, member_json(target)


def h_member_delete(req, conn, slug, user_id):
    org = get_org(conn, req, slug, lock=True)
    target = get_member(conn, org, user_id)
    is_self = target["user_id"] == req.uid
    if not is_self:
        require_org_role(org, "admin")
        if org["my_role"] != "owner" and target["role"] in ("owner", "admin"):
            raise forbidden("only owners may remove owners/admins")
    if target["role"] == "owner" and owner_count(conn, org) <= 1:
        raise ApiError(409, "last_owner", "cannot remove the last owner")
    tuid = target["user_id"]
    conn.execute("DELETE FROM org_members WHERE org_id = %s AND user_id = %s", (org["id"], tuid))
    conn.execute("DELETE FROM project_members pm USING projects p WHERE pm.project_id = p.id AND p.org_id = %s "
                 "AND pm.user_id = %s", (org["id"], tuid))
    # unassign their issues in the org: a regular change (version bump, history, webhook)
    rows = conn.execute("SELECT id FROM issues WHERE org_id = %s AND assignee_id = %s ORDER BY project_key, number "
                        "FOR UPDATE", (org["id"], tuid)).fetchall()
    ts = now()
    for row in rows:
        r = conn.execute("UPDATE issues SET assignee_id = NULL, version = version + 1, updated_at = %s "
                         "WHERE id = %s RETURNING *", (ts, row["id"])).fetchone()
        changes = [{"field": "assignee_id", "from": str(tuid), "to": None}]
        add_history(conn, r["id"], req.uid, changes, ts)
        emit(conn, req, org, "issue.updated", r["id"], {"issue": issue_json(r), "changes": changes}, req.uid)
    return 204, None


# ---------------------------------------------------------------------------
# projects


def h_project_create(req, conn, slug):
    org = get_org(conn, req, slug)
    body = req.json()
    v = V()
    key = v_str(v, body, "key", required=True)
    if isinstance(key, str) and not PKEY_RE.fullmatch(key):
        v.add("key", "invalid")
    name = v_str(v, body, "name", required=True, min_len=1, max_len=100, trim=True)
    desc = v_str(v, body, "description", nullable=True)
    vis = v_enum(v, body, "visibility", VISIBILITIES)
    v.check()
    pid = uuid.uuid4()
    ts = now()
    r = conn.execute(
        "INSERT INTO projects (id, org_id, key, name, description, visibility, created_at) VALUES (%s,%s,%s,%s,%s,%s,%s) "
        "ON CONFLICT (org_id, key) DO NOTHING RETURNING *",
        (pid, org["id"], key, name, None if desc is MISSING else desc, "org" if vis is MISSING else vis, ts)).fetchone()
    if not r:
        raise ApiError(409, "key_taken", "project key already taken")
    conn.execute("INSERT INTO project_members (project_id, user_id, role) VALUES (%s,%s,'admin')", (pid, req.uid))
    r["my_role"] = "admin"
    return 201, project_json(r)


def h_projects_list(req, conn, slug):
    org = get_org(conn, req, slug)
    limit, off = offset_page(req)
    rows = conn.execute(
        "SELECT p.*, pm.role AS proj_role FROM projects p "
        "LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = %s "
        "WHERE p.org_id = %s ORDER BY p.key", (req.uid, org["id"])).fetchall()
    vis = []
    for r in rows:
        role = eff_role(org["my_role"], r["proj_role"], r["visibility"])
        if role:
            r["my_role"] = role
            vis.append(project_json(r))
    return 200, offset_result(vis[off:off + limit + 1], limit, off)


def h_project_get(req, conn, slug, key):
    org = get_org(conn, req, slug)
    return 200, project_json(get_project(conn, req, org, key))


def h_project_patch(req, conn, slug, key):
    org = get_org(conn, req, slug)
    proj = get_project(conn, req, org, key)
    require_proj_role(proj, "admin")
    body = req.json()
    v = V()
    name = v_str(v, body, "name", min_len=1, max_len=100, trim=True)
    desc = v_str(v, body, "description", nullable=True)
    vis = v_enum(v, body, "visibility", VISIBILITIES)
    v.check()
    sets, params = [], []
    for col, val in (("name", name), ("description", desc), ("visibility", vis)):
        if val is not MISSING:
            sets.append(f"{col} = %s")
            params.append(val)
    if sets:
        r = conn.execute(f"UPDATE projects SET {', '.join(sets)} WHERE id = %s RETURNING *",
                         (*params, proj["id"])).fetchone()
        r["my_role"] = proj["my_role"]
        proj = r
    return 200, project_json(proj)


def h_project_delete(req, conn, slug, key):
    org = get_org(conn, req, slug)
    proj = get_project(conn, req, org, key)
    require_proj_role(proj, "admin")
    conn.execute("DELETE FROM projects WHERE id = %s", (proj["id"],))
    return 204, None


def h_pmembers_list(req, conn, slug, key):
    org = get_org(conn, req, slug)
    proj = get_project(conn, req, org, key)
    limit, off = offset_page(req)
    rows = conn.execute(
        "SELECT pm.user_id, u.email, u.name, pm.role FROM project_members pm JOIN users u ON u.id = pm.user_id "
        "WHERE pm.project_id = %s ORDER BY u.email, u.id LIMIT %s OFFSET %s", (proj["id"], limit + 1, off)).fetchall()
    return 200, offset_result([member_json(r) for r in rows], limit, off)


def h_pmember_put(req, conn, slug, key, user_id):
    org = get_org(conn, req, slug)
    proj = get_project(conn, req, org, key)
    require_proj_role(proj, "admin")
    body = req.json()
    v = V()
    role = v_enum(v, body, "role", PROJ_ROLES, required=True)
    uid = parse_uuid(user_id)
    u = None
    if uid is not None:
        u = conn.execute("SELECT u.* FROM org_members m JOIN users u ON u.id = m.user_id "
                         "WHERE m.org_id = %s AND m.user_id = %s", (org["id"], uid)).fetchone()
    if not u:
        v.add("user_id", "invalid")
    v.check()
    conn.execute("INSERT INTO project_members (project_id, user_id, role) VALUES (%s,%s,%s) "
                 "ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role", (proj["id"], uid, role))
    return 200, {"user_id": str(uid), "email": u["email"], "name": u["name"], "role": role}


def h_pmember_delete(req, conn, slug, key, user_id):
    org = get_org(conn, req, slug)
    proj = get_project(conn, req, org, key)
    require_proj_role(proj, "admin")
    uid = parse_uuid(user_id)
    if uid is None:
        raise not_found("project member")
    r = conn.execute("DELETE FROM project_members WHERE project_id = %s AND user_id = %s RETURNING user_id",
                     (proj["id"], uid)).fetchone()
    if not r:
        raise not_found("project member")
    return 204, None


# ---------------------------------------------------------------------------
# issues


def validate_issue_body(v, body, create):
    out = {}
    out["type"] = v_enum(v, body, "type", TYPES, required=create)
    out["title"] = v_str(v, body, "title", required=create, min_len=1, max_len=255, trim=True)
    out["description"] = v_str(v, body, "description", nullable=True, max_len=65536)
    out["priority"] = v_enum(v, body, "priority", PRIORITIES)
    out["assignee_id"] = v_uuid(v, body, "assignee_id")
    out["labels"] = v_labels(v, body)
    out["due_date"] = v_date(v, body, "due_date")
    return out


def check_assignee(conn, v, proj, vals, cache):
    a = vals.get("assignee_id")
    if a is not MISSING and a is not None:
        if not can_be_assignee(conn, proj, a, cache):
            v.add("assignee_id", "invalid")
            vals["assignee_id"] = MISSING


def insert_issue(conn, req, org, proj, vals, number, ts):
    iid = uuid.uuid4()

    def g(k, d):
        x = vals.get(k, MISSING)
        return d if x is MISSING else x

    priority = g("priority", "medium")
    title = vals["title"]
    desc = g("description", None)
    r = conn.execute(
        "INSERT INTO issues (id, org_id, project_id, project_key, number, type, title, description, status, priority, "
        "priority_rank, assignee_id, reporter_id, labels, due_date, version, comment_count, created_at, updated_at, "
        "resolved_at, words) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,'todo',%s,%s,%s,%s,%s,%s,1,0,%s,%s,NULL,%s) RETURNING *",
        (iid, org["id"], proj["id"], proj["key"], number, vals["type"], title, desc, priority, PRIORITY_RANK[priority],
         g("assignee_id", None), req.uid, g("labels", []), g("due_date", None), ts, ts,
         sorted(set(search_words(title, desc))))).fetchone()
    add_history(conn, iid, req.uid, [{"field": "created", "from": None, "to": None}], ts)
    emit(conn, req, org, "issue.created", iid, {"issue": issue_json(r)}, req.uid)
    return r


def idempotency_begin(conn, req):
    """Returns (stored_response | None, active_key | None)."""
    key = req.header("Idempotency-Key")
    if key is None:
        return None, None
    if len(key) < 1 or len(key) > 255:
        raise vfail("Idempotency-Key", "too_long" if len(key) > 255 else "too_short")
    body = req.json()
    fp = hashlib.sha256((req.method + " " + req.path.rstrip("/").lower() + "\n" +
                         json.dumps(body, sort_keys=True, separators=(",", ":"))).encode()).hexdigest()
    ts = now()
    for _ in range(3):
        ins = conn.execute("INSERT INTO idempotency_keys (user_id, key, fingerprint, created_at) VALUES (%s,%s,%s,%s) "
                           "ON CONFLICT DO NOTHING RETURNING key", (req.uid, key, fp, ts)).fetchone()
        if ins:
            return None, key
        r = conn.execute("SELECT * FROM idempotency_keys WHERE user_id = %s AND key = %s",
                         (req.uid, key)).fetchone()
        if r is None:
            continue
        if r["created_at"] < ts - IDEMPOTENCY_TTL:
            conn.execute("DELETE FROM idempotency_keys WHERE user_id = %s AND key = %s", (req.uid, key))
            continue
        if r["fingerprint"] != fp:
            raise ApiError(422, "idempotency_key_reused", "idempotency key was used with a different request")
        if r["status"] is None:
            continue
        return (r["status"], Raw(r["body"].encode())), None
    raise ApiError(409, "conflict", "idempotency key in use")


def idempotency_finish(conn, req, key, status, payload):
    data = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
    conn.execute("UPDATE idempotency_keys SET status = %s, body = %s WHERE user_id = %s AND key = %s",
                 (status, data, req.uid, key))
    return status, Raw(data.encode())


def h_issue_create(req, conn, slug, key):
    org = get_org(conn, req, slug)
    proj = get_project(conn, req, org, key)
    require_proj_role(proj, "developer")
    body = req.json()
    stored, ikey = idempotency_begin(conn, req)
    if stored:
        return stored
    v = V()
    vals = validate_issue_body(v, body, True)
    check_assignee(conn, v, proj, vals, {})
    v.check()
    n = conn.execute("UPDATE projects SET issue_seq = issue_seq + 1 WHERE id = %s RETURNING issue_seq",
                     (proj["id"],)).fetchone()["issue_seq"]
    r = insert_issue(conn, req, org, proj, vals, n, now())
    out = issue_json(r)
    if ikey:
        st, raw = idempotency_finish(conn, req, ikey, 201, out)
        return st, raw, {"ETag": f'"{r["version"]}"'}
    return 201, out, {"ETag": f'"{r["version"]}"'}


def h_issue_bulk(req, conn, slug, key):
    org = get_org(conn, req, slug)
    proj = get_project(conn, req, org, key)
    require_proj_role(proj, "developer")
    body = req.json()
    stored, ikey = idempotency_begin(conn, req)
    if stored:
        return stored
    v = V()
    items = body.get("issues", MISSING)
    if items is MISSING or items is None:
        v.add("issues", "required")
    elif not isinstance(items, list):
        v.add("issues", "invalid")
    elif len(items) < 1:
        v.add("issues", "too_short")
    elif len(items) > 1000:
        v.add("issues", "too_long")
    v.check()
    all_vals = []
    cache = {}
    for i, it in enumerate(items):
        iv = V(f"issues.{i}.")
        if not isinstance(it, dict):
            v.add(f"issues.{i}", "invalid")
            all_vals.append(None)
            continue
        vals = validate_issue_body(iv, it, True)
        check_assignee(conn, iv, proj, vals, cache)
        v.errors.extend(iv.errors)
        all_vals.append(vals)
    v.check()
    n = len(all_vals)
    last = conn.execute("UPDATE projects SET issue_seq = issue_seq + %s WHERE id = %s RETURNING issue_seq",
                        (n, proj["id"])).fetchone()["issue_seq"]
    first = last - n + 1
    ts = now()
    keys = []
    for i, vals in enumerate(all_vals):
        r = insert_issue(conn, req, org, proj, vals, first + i, ts)
        keys.append(issue_key(r))
    out = {"keys": keys}
    if ikey:
        return idempotency_finish(conn, req, ikey, 201, out)
    return 201, out


def h_issue_get(req, conn, slug, ikey):
    _, _, issue = get_issue(conn, req, slug, ikey, "viewer")
    return 200, issue_json(issue), {"ETag": f'"{issue["version"]}"'}


def parse_if_match(h):
    """Returns a set of acceptable versions, or '*'."""
    out = set()
    for part in h.split(","):
        p = part.strip()
        if p == "*":
            return "*"
        if p.startswith("W/"):
            p = p[2:]
        if len(p) >= 2 and p[0] == '"' and p[-1] == '"':
            p = p[1:-1]
        try:
            out.add(int(p))
        except ValueError:
            pass
    return out


def check_if_match(req, issue, required):
    h = req.header("If-Match")
    if h is None:
        if required:
            raise ApiError(428, "precondition_required", "If-Match header is required")
        return
    ok = parse_if_match(h)
    if ok != "*" and issue["version"] not in ok:
        raise ApiError(412, "version_mismatch", f"issue is at version {issue['version']}")


FIELD_ORDER = ("title", "description", "type", "status", "priority", "assignee_id", "labels", "due_date")


def jval(field, val):
    if val is None:
        return None
    if field == "assignee_id":
        return str(val)
    if field == "due_date":
        return val.isoformat()
    if field == "labels":
        return list(val)
    return val


def h_issue_patch(req, conn, slug, ikey):
    org, proj, issue = get_issue(conn, req, slug, ikey, "developer", lock=True)
    body = req.json()
    check_if_match(req, issue, True)
    v = V()
    if "status" in body:
        v.add("status", "invalid")
    vals = validate_issue_body(v, body, False)
    check_assignee(conn, v, proj, vals, {})
    v.check()
    changes = []
    sets, params = [], []
    for f in FIELD_ORDER:
        nv = vals.get(f, MISSING)
        if nv is MISSING:
            continue
        ov = issue[f]
        if f == "labels":
            ov = list(ov)
        if ov == nv:
            continue
        changes.append({"field": f, "from": jval(f, ov), "to": jval(f, nv)})
        sets.append(f"{f} = %s")
        params.append(nv)
        if f == "priority":
            sets.append("priority_rank = %s")
            params.append(PRIORITY_RANK[nv])
    if not changes:
        return 200, issue_json(issue), {"ETag": f'"{issue["version"]}"'}
    ts = now()
    if vals["title"] is not MISSING or vals["description"] is not MISSING:
        t = vals["title"] if vals["title"] is not MISSING else issue["title"]
        d = vals["description"] if vals["description"] is not MISSING else issue["description"]
        sets.append("words = %s")
        params.append(sorted(set(search_words(t, d))))
    r = conn.execute(f"UPDATE issues SET {', '.join(sets)}, version = version + 1, updated_at = %s WHERE id = %s "
                     "RETURNING *", (*params, ts, issue["id"])).fetchone()
    add_history(conn, r["id"], req.uid, changes, ts)
    emit(conn, req, org, "issue.updated", r["id"], {"issue": issue_json(r), "changes": changes}, req.uid)
    return 200, issue_json(r), {"ETag": f'"{r["version"]}"'}


def h_issue_transition(req, conn, slug, ikey):
    org, proj, issue = get_issue(conn, req, slug, ikey, "developer", lock=True)
    body = req.json()
    check_if_match(req, issue, False)
    v = V()
    st = v_enum(v, body, "status", STATUSES, required=True)
    v.check()
    if (issue["status"], st) not in WORKFLOW:
        raise ApiError(409, "transition_not_allowed", f"cannot transition from {issue['status']} to {st}")
    ts = now()
    resolved = ts if st == "done" else None
    r = conn.execute("UPDATE issues SET status = %s, resolved_at = %s, version = version + 1, updated_at = %s "
                     "WHERE id = %s RETURNING *", (st, resolved, ts, issue["id"])).fetchone()
    changes = [{"field": "status", "from": issue["status"], "to": st}]
    add_history(conn, r["id"], req.uid, changes, ts)
    emit(conn, req, org, "issue.updated", r["id"], {"issue": issue_json(r), "changes": changes}, req.uid)
    return 200, issue_json(r), {"ETag": f'"{r["version"]}"'}


def h_issue_delete(req, conn, slug, ikey):
    org, proj, issue = get_issue(conn, req, slug, ikey, "admin", lock=True)
    emit(conn, req, org, "issue.deleted", issue["id"], {"issue": issue_json(issue)}, req.uid)
    conn.execute("DELETE FROM issues WHERE id = %s", (issue["id"],))
    return 204, None


# ---------------------------------------------------------------------------
# comments / history


def h_comment_create(req, conn, slug, ikey):
    org, proj, issue = get_issue(conn, req, slug, ikey, "viewer", lock=True)
    body = req.json()
    v = V()
    text = v_str(v, body, "body", required=True, min_len=1, max_len=20000)
    v.check()
    ts = now()
    c = conn.execute("INSERT INTO comments (id, org_id, issue_id, author_id, body, created_at, updated_at, edited) "
                     "VALUES (%s,%s,%s,%s,%s,%s,%s,false) RETURNING *",
                     (uuid.uuid4(), org["id"], issue["id"], req.uid, text, ts, ts)).fetchone()
    r = conn.execute("UPDATE issues SET comment_count = comment_count + 1 WHERE id = %s RETURNING *",
                     (issue["id"],)).fetchone()
    cj = comment_json(c, issue_key(r))
    emit(conn, req, org, "comment.created", r["id"], {"issue": issue_json(r), "comment": cj}, req.uid)
    return 201, cj


def h_comments_list(req, conn, slug, ikey):
    _, _, issue = get_issue(conn, req, slug, ikey, "viewer")
    limit, off = offset_page(req)
    rows = conn.execute("SELECT * FROM comments WHERE issue_id = %s ORDER BY seq LIMIT %s OFFSET %s",
                        (issue["id"], limit + 1, off)).fetchall()
    k = issue_key(issue)
    return 200, offset_result([comment_json(r, k) for r in rows], limit, off)


def load_comment(conn, req, slug, cid, lock=False):
    org = get_org(conn, req, slug)
    u = parse_uuid(cid)
    if u is None:
        raise not_found("comment")
    c = conn.execute("SELECT * FROM comments WHERE id = %s AND org_id = %s" + (" FOR UPDATE" if lock else ""),
                     (u, org["id"])).fetchone()
    if not c:
        raise not_found("comment")
    issue = conn.execute("SELECT * FROM issues WHERE id = %s", (c["issue_id"],)).fetchone()
    p = conn.execute("SELECT key FROM projects WHERE id = %s", (issue["project_id"],)).fetchone()
    proj = get_project(conn, req, org, p["key"])  # 404 if not visible
    return org, proj, issue, c


def h_comment_patch(req, conn, slug, cid):
    org, proj, issue, c = load_comment(conn, req, slug, cid, lock=True)
    if c["author_id"] != req.uid:
        raise forbidden("only the author may edit a comment")
    body = req.json()
    v = V()
    text = v_str(v, body, "body", required=True, min_len=1, max_len=20000)
    v.check()
    r = conn.execute("UPDATE comments SET body = %s, edited = true, updated_at = %s WHERE id = %s RETURNING *",
                     (text, now(), c["id"])).fetchone()
    return 200, comment_json(r, issue_key(issue))


def h_comment_delete(req, conn, slug, cid):
    org, proj, issue, c = load_comment(conn, req, slug, cid, lock=True)
    if c["author_id"] != req.uid and proj["my_role"] != "admin":
        raise forbidden("only the author or a project admin may delete a comment")
    conn.execute("DELETE FROM comments WHERE id = %s", (c["id"],))
    conn.execute("UPDATE issues SET comment_count = comment_count - 1 WHERE id = %s", (issue["id"],))
    return 204, None


def h_history(req, conn, slug, ikey):
    _, _, issue = get_issue(conn, req, slug, ikey, "viewer")
    limit, off = offset_page(req)
    rows = conn.execute("SELECT * FROM issue_history WHERE issue_id = %s ORDER BY seq DESC LIMIT %s OFFSET %s",
                        (issue["id"], limit + 1, off)).fetchall()
    items = [{"id": str(r["id"]), "actor_id": str(r["actor_id"]), "created_at": fmt_ts(r["created_at"]),
              "changes": r["changes"]} for r in rows]
    return 200, offset_result(items, limit, off)


# ---------------------------------------------------------------------------
# listing / search

SORTS = {"created": "created_at", "updated": "updated_at", "priority": "priority_rank", "key": None}


def split_vals(req, name):
    s = req.q(name)
    if s is None:
        return None
    vals = [x.strip() for x in s.split(",")]
    if any(not x for x in vals):
        raise vfail(name, "invalid")
    return vals


def h_issues_list(req, conn, slug):
    org = get_org(conn, req, slug)
    limit = page_params(req)
    rows = conn.execute(
        "SELECT p.id, p.key, p.visibility, pm.role AS proj_role FROM projects p "
        "LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = %s WHERE p.org_id = %s",
        (req.uid, org["id"])).fetchall()
    visible = [r["id"] for r in rows if eff_role(org["my_role"], r["proj_role"], r["visibility"])]
    where = ["org_id = %s", "project_id = ANY(%s::uuid[])"]
    params = [org["id"], visible]

    vals = split_vals(req, "project")
    if vals is not None:
        keys = [x.upper() for x in vals]
        if any(not PKEY_RE.fullmatch(k) for k in keys):
            raise vfail("project", "invalid")
        where.append("project_key = ANY(%s::text[])")
        params.append(keys)
    for name, choices in (("status", STATUSES), ("priority", PRIORITIES), ("type", TYPES)):
        vals = split_vals(req, name)
        if vals is not None:
            if any(x not in choices for x in vals):
                raise vfail(name, "invalid")
            where.append(f"{name} = ANY(%s::text[])")
            params.append(vals)
    vals = split_vals(req, "assignee")
    if vals is not None:
        ids, none = [], False
        for x in vals:
            if x == "me":
                ids.append(req.uid)
            elif x == "none":
                none = True
            else:
                u = parse_uuid(x)
                if u is None:
                    raise vfail("assignee", "invalid")
                ids.append(u)
        conds = []
        if ids:
            conds.append("assignee_id = ANY(%s::uuid[])")
            params.append(ids)
        if none:
            conds.append("assignee_id IS NULL")
        where.append("(" + " OR ".join(conds) + ")")
    vals = split_vals(req, "reporter")
    if vals is not None:
        ids = []
        for x in vals:
            if x == "me":
                ids.append(req.uid)
            else:
                u = parse_uuid(x)
                if u is None:
                    raise vfail("reporter", "invalid")
                ids.append(u)
        where.append("reporter_id = ANY(%s::uuid[])")
        params.append(ids)
    vals = split_vals(req, "label")
    if vals is not None:
        labels = [x.lower() for x in vals]
        if any(not LABEL_RE.fullmatch(x) for x in labels):
            raise vfail("label", "invalid")
        where.append("labels && %s::text[]")
        params.append(labels)
    for name, col, op in (("created_after", "created_at", ">"), ("created_before", "created_at", "<"),
                          ("updated_after", "updated_at", ">")):
        s = req.q(name)
        if s is not None:
            t = parse_ts(s.strip())
            if t is None:
                raise vfail(name, "invalid")
            where.append(f"{col} {op} %s")
            params.append(t)
    q = req.q("q")
    if q is not None:
        words = sorted(set(search_words(q)))
        if words:
            where.append("words @> %s::text[]")
            params.append(words)

    sort = req.q("sort")
    if sort is None or sort == "":
        sort = "-created"
    desc = sort.startswith("-")
    field = sort[1:] if desc else sort
    if field not in SORTS:
        raise vfail("sort", "invalid")
    d = "DESC" if desc else "ASC"

    total = conn.execute(f"SELECT count(*) AS n FROM issues WHERE {' AND '.join(where)}", params).fetchone()["n"]

    cur = dec_cursor(req)
    pwhere, pparams = list(where), list(params)
    col = SORTS[field]
    if cur is not None:
        try:
            if cur.get("s") != sort:
                raise ValueError
            ck, cn = cur["k"], cur["n"]
            if not isinstance(ck, str) or not isinstance(cn, int):
                raise ValueError
            if field == "key":
                pwhere.append(f"(project_key, number) {'<' if desc else '>'} (%s, %s)")
                pparams += [ck, cn]
            else:
                cv = cur["v"]
                if field == "priority":
                    if not isinstance(cv, int):
                        raise ValueError
                else:
                    cv = dt.datetime.fromisoformat(cv)
                op = "<" if desc else ">"
                pwhere.append(f"({col} {op} %s OR ({col} = %s AND (project_key, number) > (%s, %s)))")
                pparams += [cv, cv, ck, cn]
        except (ValueError, KeyError, TypeError):
            raise vfail("cursor", "invalid")
    if field == "key":
        order = f"project_key {d}, number {d}"
    else:
        order = f"{col} {d}, project_key ASC, number ASC"
    rows = conn.execute(f"SELECT * FROM issues WHERE {' AND '.join(pwhere)} ORDER BY {order} LIMIT %s",
                        (*pparams, limit + 1)).fetchall()
    nxt = None
    if len(rows) > limit:
        rows = rows[:limit]
        last = rows[-1]
        c = {"s": sort, "k": last["project_key"], "n": last["number"]}
        if field in ("created", "updated"):
            c["v"] = last[col].isoformat()
        elif field == "priority":
            c["v"] = last[col]
        nxt = enc_cursor(c)
    return 200, {"total": total, "items": [issue_json(r) for r in rows], "next_cursor": nxt}


# ---------------------------------------------------------------------------
# webhooks


def v_url(v, body, required):
    url = v_str(v, body, "url", required=required)
    if isinstance(url, str):
        try:
            p = urllib.parse.urlsplit(url)
            ok = p.scheme in ("http", "https") and bool(p.hostname)
            if ok:
                p.port  # noqa: B018 -- raises on bad port
        except ValueError:
            ok = False
        if not ok:
            v.add("url", "invalid")
            return MISSING
    return url


def v_events(v, body, required):
    if "events" not in body:
        if required:
            v.add("events", "required")
        return MISSING
    ev = body["events"]
    if ev is None and required:
        v.add("events", "required")
        return MISSING
    if not isinstance(ev, list) or any(not isinstance(e, str) or e not in EVENTS for e in ev):
        v.add("events", "invalid")
        return MISSING
    if not ev:
        v.add("events", "too_short")
        return MISSING
    out = []
    for e in ev:
        if e not in out:
            out.append(e)
    return out


def h_webhook_create(req, conn, slug):
    org = get_org(conn, req, slug)
    require_org_role(org, "admin")
    body = req.json()
    v = V()
    url = v_url(v, body, True)
    events = v_events(v, body, True)
    secret = v_str(v, body, "secret", min_len=1, nullable=True)
    v.check()
    if secret is MISSING or secret is None:
        secret = secrets.token_hex(32)
    r = conn.execute("INSERT INTO webhooks (id, org_id, url, events, secret, active, created_at) "
                     "VALUES (%s,%s,%s,%s,%s,true,%s) RETURNING *",
                     (uuid.uuid4(), org["id"], url, events, secret, now())).fetchone()
    out = webhook_json(r)
    out["secret"] = secret
    return 201, out


def h_webhooks_list(req, conn, slug):
    org = get_org(conn, req, slug)
    require_org_role(org, "admin")
    limit, off = offset_page(req)
    rows = conn.execute("SELECT * FROM webhooks WHERE org_id = %s ORDER BY seq LIMIT %s OFFSET %s",
                        (org["id"], limit + 1, off)).fetchall()
    return 200, offset_result([webhook_json(r) for r in rows], limit, off)


def get_webhook(conn, req, slug, wid):
    org = get_org(conn, req, slug)
    require_org_role(org, "admin")
    u = parse_uuid(wid)
    if u is None:
        raise not_found("webhook")
    r = conn.execute("SELECT * FROM webhooks WHERE id = %s AND org_id = %s", (u, org["id"])).fetchone()
    if not r:
        raise not_found("webhook")
    return org, r


def h_webhook_get(req, conn, slug, wid):
    return 200, webhook_json(get_webhook(conn, req, slug, wid)[1])


def h_webhook_patch(req, conn, slug, wid):
    org, w = get_webhook(conn, req, slug, wid)
    body = req.json()
    v = V()
    url = v_url(v, body, False)
    events = v_events(v, body, False)
    active = v_bool(v, body, "active")
    v.check()
    sets, params = [], []
    for col, val in (("url", url), ("events", events), ("active", active)):
        if val is not MISSING:
            sets.append(f"{col} = %s")
            params.append(val)
    if sets:
        w = conn.execute(f"UPDATE webhooks SET {', '.join(sets)} WHERE id = %s RETURNING *",
                         (*params, w["id"])).fetchone()
        req.wake_worker = True
    return 200, webhook_json(w)


def h_webhook_delete(req, conn, slug, wid):
    org, w = get_webhook(conn, req, slug, wid)
    conn.execute("DELETE FROM webhooks WHERE id = %s", (w["id"],))
    return 204, None


def h_deliveries(req, conn, slug, wid):
    org, w = get_webhook(conn, req, slug, wid)
    limit, off = offset_page(req)
    rows = conn.execute("SELECT * FROM webhook_deliveries WHERE webhook_id = %s ORDER BY seq DESC LIMIT %s OFFSET %s",
                        (w["id"], limit + 1, off)).fetchall()
    return 200, offset_result([delivery_json(r) for r in rows], limit, off)


# ---------------------------------------------------------------------------
# webhook delivery worker


class Worker:
    def __init__(self):
        self.stop = threading.Event()
        self.wake = threading.Event()
        self.lock = threading.Lock()
        self.inflight = set()
        self.pool = ThreadPoolExecutor(max_workers=16, thread_name_prefix="wh")
        self.thread = threading.Thread(target=self.run, name="wh-sched", daemon=True)

    def start(self):
        self.thread.start()

    def run(self):
        while not self.stop.is_set():
            try:
                wait = self.tick()
            except Exception:
                log("webhook scheduler error:\n" + traceback.format_exc())
                wait = 1.0
            self.wake.wait(wait)
            self.wake.clear()

    def tick(self):
        with POOL.connection() as conn:
            rows = conn.execute(
                "SELECT d.id, EXTRACT(EPOCH FROM (GREATEST(d.next_attempt_at, d.lease_until) - clock_timestamp()))::float8 AS wait "
                "FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id "
                "WHERE d.status = 'pending' AND w.active AND NOT EXISTS ("
                "  SELECT 1 FROM webhook_deliveries e WHERE e.webhook_id = d.webhook_id AND e.issue_id = d.issue_id "
                "  AND e.status = 'pending' AND e.seq < d.seq) "
                "ORDER BY d.seq LIMIT 1000").fetchall()
        wait = 1.0
        for r in rows:
            if self.stop.is_set():
                break
            with self.lock:
                if r["id"] in self.inflight:
                    continue
                if r["wait"] > 0:
                    wait = min(wait, r["wait"])
                    continue
                self.inflight.add(r["id"])
            self.pool.submit(self.attempt, r["id"])
        return max(wait, 0.002)

    def attempt(self, did):
        try:
            # atomically claim: still pending, due, head of its (webhook, issue) chain, webhook active, not leased
            with POOL.connection() as conn:
                r = conn.execute(
                    "UPDATE webhook_deliveries d SET lease_until = clock_timestamp() + interval '15 seconds' "
                    "FROM webhooks w WHERE d.id = %s AND w.id = d.webhook_id AND w.active AND d.status = 'pending' "
                    "AND d.next_attempt_at <= clock_timestamp() "
                    "AND (d.lease_until IS NULL OR d.lease_until < clock_timestamp()) "
                    "AND NOT EXISTS (SELECT 1 FROM webhook_deliveries e WHERE e.webhook_id = d.webhook_id "
                    "  AND e.issue_id = d.issue_id AND e.status = 'pending' AND e.seq < d.seq) "
                    "RETURNING d.*, w.url, w.secret", (did,)).fetchone()
            if not r:
                return
            body = r["body"].encode()
            code = send_webhook(r["url"], r["secret"], r["event"], str(r["id"]), body)
            ok = code is not None and 200 <= code < 300
            attempts = r["attempts"] + 1
            with POOL.connection() as conn:
                if ok:
                    conn.execute("UPDATE webhook_deliveries SET status = 'succeeded', attempts = %s, "
                                 "last_status_code = %s, lease_until = NULL WHERE id = %s AND status = 'pending'", (attempts, code, did))
                elif attempts >= MAX_ATTEMPTS:
                    conn.execute("UPDATE webhook_deliveries SET status = 'failed', attempts = %s, "
                                 "last_status_code = %s, lease_until = NULL WHERE id = %s AND status = 'pending'", (attempts, code, did))
                else:
                    delay = RETRY_DELAYS[attempts - 1] * BACKOFF_SCALE
                    conn.execute("UPDATE webhook_deliveries SET attempts = %s, last_status_code = %s, "
                                 "next_attempt_at = clock_timestamp() + make_interval(secs => %s), lease_until = NULL "
                                 "WHERE id = %s AND status = 'pending'", (attempts, code, delay, did))
        except Exception:
            log("webhook attempt error:\n" + traceback.format_exc())
        finally:
            with self.lock:
                self.inflight.discard(did)
            self.wake.set()

    def shutdown(self, timeout):
        self.stop.set()
        self.wake.set()
        # drop queued attempts, let running ones finish (bounded by timeout)
        t = threading.Thread(target=self.pool.shutdown, kwargs={"wait": True, "cancel_futures": True}, daemon=True)
        t.start()
        t.join(timeout)


def send_webhook(url, secret, event, delivery_id, body):
    """Returns HTTP status code, or None on transport error / timeout (> 5 s)."""
    p = urllib.parse.urlsplit(url)
    path = p.path or "/"
    if p.query:
        path += "?" + p.query
    ts = str(int(time.time()))
    sig = hmac.new(secret.encode(), ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    headers = {"Content-Type": "application/json", "X-Tracker-Event": event, "X-Tracker-Delivery": delivery_id,
               "X-Tracker-Timestamp": ts, "X-Tracker-Signature": f"sha256={sig}", "Content-Length": str(len(body)),
               "User-Agent": "tracker-ref/1"}
    start = time.monotonic()
    cls = http.client.HTTPSConnection if p.scheme == "https" else http.client.HTTPConnection
    conn = None
    try:
        conn = cls(p.hostname, p.port, timeout=5)
        conn.request("POST", path, body=body, headers=headers)
        resp = conn.getresponse()
        code = resp.status
        resp.read(1 << 16)
    except Exception:
        return None
    finally:
        if conn:
            try:
                conn.close()
            except Exception:
                pass
    if time.monotonic() - start > 5.0:
        return None
    return code


# ---------------------------------------------------------------------------
# routing

ROUTES = []


def route(method, pattern, auth=True):
    rx = re.compile("^" + re.sub(r"\{(\w+)\}", r"(?P<\1>[^/]+)", pattern) + "$")

    def deco(fn):
        ROUTES.append((method, rx, fn, auth))
        return fn
    return deco


def add_routes():
    R = [
        ("POST", "/auth/register", h_register, False),
        ("POST", "/auth/login", h_login, False),
        ("POST", "/auth/refresh", h_refresh, False),
        ("POST", "/auth/logout", h_logout, False),
        ("GET", "/me", h_me, True),
        ("PATCH", "/me", h_me_patch, True),
        ("POST", "/orgs", h_org_create, True),
        ("GET", "/orgs", h_orgs_list, True),
        ("GET", "/orgs/{slug}", h_org_get, True),
        ("GET", "/orgs/{slug}/members", h_members_list, True),
        ("POST", "/orgs/{slug}/members", h_member_add, True),
        ("PATCH", "/orgs/{slug}/members/{user_id}", h_member_patch, True),
        ("DELETE", "/orgs/{slug}/members/{user_id}", h_member_delete, True),
        ("POST", "/orgs/{slug}/projects", h_project_create, True),
        ("GET", "/orgs/{slug}/projects", h_projects_list, True),
        ("GET", "/orgs/{slug}/projects/{key}", h_project_get, True),
        ("PATCH", "/orgs/{slug}/projects/{key}", h_project_patch, True),
        ("DELETE", "/orgs/{slug}/projects/{key}", h_project_delete, True),
        ("GET", "/orgs/{slug}/projects/{key}/members", h_pmembers_list, True),
        ("PUT", "/orgs/{slug}/projects/{key}/members/{user_id}", h_pmember_put, True),
        ("DELETE", "/orgs/{slug}/projects/{key}/members/{user_id}", h_pmember_delete, True),
        ("POST", "/orgs/{slug}/projects/{key}/issues", h_issue_create, True),
        ("POST", "/orgs/{slug}/projects/{key}/issues/bulk", h_issue_bulk, True),
        ("GET", "/orgs/{slug}/issues", h_issues_list, True),
        ("GET", "/orgs/{slug}/issues/{ikey}", h_issue_get, True),
        ("PATCH", "/orgs/{slug}/issues/{ikey}", h_issue_patch, True),
        ("DELETE", "/orgs/{slug}/issues/{ikey}", h_issue_delete, True),
        ("POST", "/orgs/{slug}/issues/{ikey}/transition", h_issue_transition, True),
        ("GET", "/orgs/{slug}/issues/{ikey}/comments", h_comments_list, True),
        ("POST", "/orgs/{slug}/issues/{ikey}/comments", h_comment_create, True),
        ("GET", "/orgs/{slug}/issues/{ikey}/history", h_history, True),
        ("PATCH", "/orgs/{slug}/comments/{cid}", h_comment_patch, True),
        ("DELETE", "/orgs/{slug}/comments/{cid}", h_comment_delete, True),
        ("GET", "/orgs/{slug}/webhooks", h_webhooks_list, True),
        ("POST", "/orgs/{slug}/webhooks", h_webhook_create, True),
        ("GET", "/orgs/{slug}/webhooks/{wid}", h_webhook_get, True),
        ("PATCH", "/orgs/{slug}/webhooks/{wid}", h_webhook_patch, True),
        ("DELETE", "/orgs/{slug}/webhooks/{wid}", h_webhook_delete, True),
        ("GET", "/orgs/{slug}/webhooks/{wid}/deliveries", h_deliveries, True),
    ]
    for m, p, fn, auth in R:
        route(m, p, auth)(fn)


add_routes()


def handle_api(method, path, query, headers, raw):
    """Returns (status, payload | Raw | None, extra headers)."""
    if path in ("/healthz", "/api/v1/healthz"):
        if method != "GET":
            raise not_found()
        return 200, {"status": "ok"}, {}
    if path in ("/readyz", "/api/v1/readyz"):
        if method != "GET":
            raise not_found()
        try:
            with POOL.connection(timeout=2) as conn:
                conn.execute("SELECT 1")
            return 200, {"status": "ok"}, {}
        except Exception:
            return 503, {"status": "unavailable"}, {}
    if not path.startswith("/api/v1/"):
        raise not_found()
    sub = path[len("/api/v1"):]
    if len(sub) > 1 and sub.endswith("/"):
        sub = sub[:-1]
    match = None
    path_matched = False
    for m, rx, fn, auth in ROUTES:
        mo = rx.match(sub)
        if mo:
            path_matched = True
            if m == method:
                match = (fn, auth, {k: urllib.parse.unquote(v) for k, v in mo.groupdict().items()})
                break
    if not match:
        raise not_found() if not path_matched else ApiError(404, "not_found", "method not supported")
    fn, auth, params = match
    req = Req(method, path, query, headers, raw)
    if auth:
        h = headers.get("Authorization") or ""
        parts = h.split(None, 1)
        if len(parts) != 2 or parts[0].lower() != "bearer":
            raise ApiError(401, "unauthenticated", "missing bearer token")
        req.uid = verify_access_token(parts[1].strip())

    for attempt in range(2):
        try:
            with POOL.connection() as conn:
                if auth:
                    if not conn.execute("SELECT 1 FROM users WHERE id = %s", (req.uid,)).fetchone():
                        raise ApiError(401, "unauthenticated", "unknown user")
                res = fn(req, conn, **params)
            break
        except (psycopg.errors.UndefinedTable, psycopg.errors.InvalidSchemaName):
            if attempt:
                raise
            migrate()  # database was reset underneath us
        except psycopg.errors.SerializationFailure:
            if attempt:
                raise
    if req.wake_worker and WORKER:
        WORKER.wake.set()
    if len(res) == 2:
        return res[0], res[1], {}
    return res


# ---------------------------------------------------------------------------
# HTTP server

class ServerState:
    def __init__(self):
        self.lock = threading.Lock()
        self.cond = threading.Condition(self.lock)
        self.idle = set()
        self.active = 0
        self.conns = 0


STATE = ServerState()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "tracker-ref"
    sys_version = ""
    timeout = 120

    def log_message(self, fmt, *args):
        pass

    def setup(self):
        super().setup()
        with STATE.lock:
            STATE.conns += 1

    def finish(self):
        try:
            super().finish()
        finally:
            with STATE.lock:
                STATE.conns -= 1
                STATE.idle.discard(self)
                STATE.cond.notify_all()

    def handle(self):
        self.close_connection = True
        self.handle_one_request()
        while not self.close_connection and not STOPPING.is_set():
            self.handle_one_request()

    def handle_one_request(self):
        try:
            with STATE.lock:
                if STOPPING.is_set():
                    self.close_connection = True
                    return
                STATE.idle.add(self)
            try:
                self.raw_requestline = self.rfile.readline(65537)
            finally:
                with STATE.lock:
                    STATE.idle.discard(self)
            if not self.raw_requestline:
                self.close_connection = True
                return
            with STATE.lock:
                STATE.active += 1
            try:
                if len(self.raw_requestline) > 65536:
                    self.send_error(414)
                    return
                if not self.parse_request():
                    return
                self.serve()
                self.wfile.flush()
            finally:
                with STATE.lock:
                    STATE.active -= 1
                    STATE.cond.notify_all()
            if STOPPING.is_set():
                self.close_connection = True
        except (TimeoutError, ConnectionError, OSError):
            self.close_connection = True

    def read_body(self):
        te = (self.headers.get("Transfer-Encoding") or "").lower()
        if "chunked" in te:
            chunks = []
            while True:
                line = self.rfile.readline(65537)
                size = int(line.split(b";")[0].strip() or b"0", 16)
                if size == 0:
                    while self.rfile.readline(65537) not in (b"\r\n", b"\n", b""):
                        pass
                    break
                chunks.append(self.rfile.read(size))
                self.rfile.readline(65537)
            return b"".join(chunks)
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n > 0 else b""

    def serve(self):
        method = self.command
        try:
            raw = self.read_body()
        except Exception:
            self.close_connection = True
            self.respond(400, problem(ApiError(400, "bad_request", "unreadable body")), {}, problem_ct=True)
            return
        sp = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(sp.query, keep_blank_values=True)
        try:
            status, payload, headers = handle_api(method, sp.path, query, self.headers, raw)
            self.respond(status, payload, headers)
        except ApiError as e:
            self.respond(e.status, problem(e), {}, problem_ct=True)
        except Exception:
            log("internal error:\n" + traceback.format_exc())
            self.respond(500, {"status": 500, "code": "internal", "detail": "internal server error"}, {},
                         problem_ct=True)

    def respond(self, status, payload, headers, problem_ct=False):
        if payload is None:
            data = b""
        elif isinstance(payload, Raw):
            data = payload.data
        else:
            data = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        if status != 204:
            self.send_header("Content-Type", "application/problem+json" if problem_ct else "application/json")
            self.send_header("Content-Length", str(len(data)))
        for k, val in headers.items():
            self.send_header(k, val)
        if STOPPING.is_set():
            self.send_header("Connection", "close")
            self.close_connection = True
        self.end_headers()
        if status != 204 and data:
            self.wfile.write(data)


def problem(e: ApiError):
    out = {"status": e.status, "code": e.code, "detail": e.detail}
    if e.code == "validation_failed":
        out["errors"] = e.errors or []
    return out


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 1024


# ---------------------------------------------------------------------------
# main


def main():
    global PORT, JWT_SECRET, BACKOFF_SCALE, POOL, WORKER, DUMMY_HASH
    db = os.environ.get("DATABASE_URL")
    secret = os.environ.get("JWT_SECRET")
    if not db or not secret:
        log("DATABASE_URL and JWT_SECRET are required")
        sys.exit(2)
    PORT = int(os.environ.get("PORT", "8080"))
    JWT_SECRET = secret.encode()
    BACKOFF_SCALE = float(os.environ.get("WEBHOOK_BACKOFF_SCALE", "1.0"))
    DUMMY_HASH = PH.hash(secrets.token_hex(8))

    POOL = ConnectionPool(db, min_size=2, max_size=40, kwargs={"row_factory": dict_row}, open=False, timeout=30)
    POOL.open(wait=True, timeout=30)
    migrate()

    WORKER = Worker()
    WORKER.start()

    server = Server(("0.0.0.0", PORT), Handler)

    def on_term(signum, frame):
        if STOPPING.is_set():
            return
        STOPPING.set()
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, on_term)
    signal.signal(signal.SIGINT, on_term)
    log(f"tracker-ref listening on :{PORT}")
    server.serve_forever(poll_interval=0.1)

    # stop accepting new connections
    try:
        server.socket.close()
    except Exception:
        pass
    deadline = time.monotonic() + 10
    # close idle keep-alive connections; let in-flight requests finish
    with STATE.lock:
        idle = list(STATE.idle)
    for h in idle:
        try:
            h.connection.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
    with STATE.lock:
        while STATE.active > 0 and time.monotonic() < deadline:
            STATE.cond.wait(0.05)
            for h in list(STATE.idle):
                try:
                    h.connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
    WORKER.shutdown(max(0.5, deadline - time.monotonic()))
    try:
        POOL.close(timeout=2)
    except Exception:
        pass
    log("tracker-ref stopped")
    sys.stdout.flush()
    sys.stderr.flush()
    os._exit(0)


if __name__ == "__main__":
    main()
