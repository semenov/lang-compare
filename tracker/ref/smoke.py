"""Quick manual smoke checks for the reference (not a test suite)."""
import json, sys, threading, time, uuid, hmac, hashlib, urllib.request, concurrent.futures as cf
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:18080"
API = BASE + "/api/v1"


def call(method, path, body=None, tok=None, headers=None, raw=None):
    h = {"Content-Type": "application/json"}
    if tok:
        h["Authorization"] = "Bearer " + tok
    h.update(headers or {})
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(API + path, data=data, method=method, headers=h)
    try:
        with urllib.request.urlopen(req) as r:
            b = r.read()
            return r.status, (json.loads(b) if b else None), dict(r.headers)
    except urllib.error.HTTPError as e:
        b = e.read()
        return e.code, (json.loads(b) if b else None), dict(e.headers)


def check(cond, msg):
    print(("ok   " if cond else "FAIL ") + msg)
    if not cond:
        global FAILS
        FAILS += 1


FAILS = 0
sfx = uuid.uuid4().hex[:8]


def user(name):
    email = f"{name}-{sfx}@Example.com"
    s, u, _ = call("POST", "/auth/register", {"email": email, "password": "password123", "name": "  " + name + " "})
    assert s == 201, (s, u)
    s, t, _ = call("POST", "/auth/login", {"email": email.lower(), "password": "password123"})
    assert s == 200, t
    return u, t


# ---- auth
alice, at = user("alice")
check(alice["email"] == f"alice-{sfx}@example.com" and alice["name"] == "alice", "register lowercases/trims")
s, b, _ = call("POST", "/auth/register", {"email": f"ALICE-{sfx}@example.com", "password": "password123", "name": "x"})
check(s == 409 and b["code"] == "email_taken", "email_taken")
s, b, h = call("POST", "/auth/register", {"email": "nope", "password": "short", "name": ""})
check(s == 422 and {e["field"] for e in b["errors"]} == {"email", "password", "name"}
      and h["Content-Type"] == "application/problem+json", f"register validation {b}")
s, b, _ = call("POST", "/auth/register", raw=b"{bad")
check(s == 400 and b["code"] == "bad_request", "malformed json 400")
s, b, _ = call("POST", "/auth/login", {"email": alice["email"], "password": "wrongwrong1"})
check(s == 401 and b["code"] == "invalid_credentials", "invalid_credentials")
s, b, _ = call("GET", "/me")
check(s == 401 and b["code"] == "unauthenticated", "no token")
s, b, _ = call("GET", "/me", tok=at["access_token"])
check(s == 200 and b["id"] == alice["id"], "GET /me")
# refresh rotation and reuse
r1 = at["refresh_token"]
s, p2, _ = call("POST", "/auth/refresh", {"refresh_token": r1})
check(s == 200 and p2["refresh_token"] != r1, "refresh rotates")
s, b, _ = call("POST", "/auth/refresh", {"refresh_token": r1})
check(s == 401 and b["code"] == "token_reused", "token_reused")
s, b, _ = call("POST", "/auth/refresh", {"refresh_token": p2["refresh_token"]})
check(s == 401 and b["code"] == "invalid_token", "chain revoked after reuse")
s, b, _ = call("POST", "/auth/refresh", {"refresh_token": "garbage"})
check(s == 401 and b["code"] == "invalid_token", "unknown refresh")
_, at = user("alice2")[0], at
s, t2, _ = call("POST", "/auth/login", {"email": alice["email"], "password": "password123"})
s, _, _ = call("POST", "/auth/logout", {"refresh_token": t2["refresh_token"]})
s2, b, _ = call("POST", "/auth/refresh", {"refresh_token": t2["refresh_token"]})
check(s == 204 and s2 == 401 and b["code"] == "invalid_token", "logout revokes")
A = at["access_token"]

# expired token
import base64
def mk(payload, secret=b"s3cret"):
    e = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
    hp = e({"alg": "HS256", "typ": "JWT"}) + "." + e(payload)
    return hp + "." + base64.urlsafe_b64encode(hmac.new(secret, hp.encode(), hashlib.sha256).digest()).rstrip(b"=").decode()
s, b, _ = call("GET", "/me", tok=mk({"sub": alice["id"], "iat": 1, "exp": 2, "typ": "access"}))
check(s == 401 and b["code"] == "token_expired", "token_expired")
s, b, _ = call("GET", "/me", tok=mk({"sub": alice["id"], "iat": 1, "exp": 2, "typ": "access"}, b"other"))
check(s == 401 and b["code"] == "unauthenticated", "bad signature")

# ---- orgs
bob, bt = user("bob"); B = bt["access_token"]
carol, ct = user("carol"); C = ct["access_token"]
slug = f"org-{sfx}"
s, org, _ = call("POST", "/orgs", {"name": "Org", "slug": slug}, A)
check(s == 201 and org["my_role"] == "owner", "create org")
s, b, _ = call("POST", "/orgs", {"name": "Org", "slug": slug}, B)
check(s == 409 and b["code"] == "slug_taken", "slug_taken")
s, b, _ = call("POST", "/orgs", {"name": "Org", "slug": "-bad"}, B)
check(s == 422 and b["errors"][0]["field"] == "slug", "slug invalid")
s, b, _ = call("GET", f"/orgs/{slug}", None, B)
check(s == 404, "non-member org 404")
s, b, _ = call("POST", f"/orgs/{slug}/members", {"email": bob["email"], "role": "member"}, A)
check(s == 201 and b["role"] == "member", "add member")
s, b, _ = call("POST", f"/orgs/{slug}/members", {"email": bob["email"], "role": "member"}, A)
check(s == 409 and b["code"] == "already_member", "already_member")
s, b, _ = call("POST", f"/orgs/{slug}/members", {"email": "x@nowhere.zz", "role": "member"}, A)
check(s == 422 and b["errors"] == [{"field": "email", "code": "invalid"}], "unknown email 422")
s, b, _ = call("POST", f"/orgs/{slug}/members", {"email": carol["email"], "role": "member"}, B)
check(s == 403, "member can't add members")
s, b, _ = call("PATCH", f"/orgs/{slug}/members/{alice['id']}", {"role": "admin"}, A)
check(s == 409 and b["code"] == "last_owner", "last_owner demote")
s, b, _ = call("DELETE", f"/orgs/{slug}/members/{alice['id']}", None, A)
check(s == 409 and b["code"] == "last_owner", "last_owner remove")
s, b, _ = call("GET", f"/orgs/{slug}/members?limit=1", None, B)
check(s == 200 and len(b["items"]) == 1 and b["next_cursor"], "members page 1")
s, b2, _ = call("GET", f"/orgs/{slug}/members?limit=1&cursor={b['next_cursor']}", None, B)
check(s == 200 and len(b2["items"]) == 1 and b2["next_cursor"] is None, "members page 2")
s, b, _ = call("GET", f"/orgs/{slug}/members?limit=0", None, B)
check(s == 422 and b["errors"][0] == {"field": "limit", "code": "out_of_range"}, "limit out_of_range")

# ---- projects
s, p, _ = call("POST", f"/orgs/{slug}/projects", {"key": "PROJ", "name": "Proj"}, A)
check(s == 201 and p["visibility"] == "org" and p["my_role"] == "admin" and p["description"] is None, "create project")
s, b, _ = call("POST", f"/orgs/{slug}/projects", {"key": "PROJ", "name": "Proj"}, B)
check(s == 409 and b["code"] == "key_taken", "key_taken")
s, sec, _ = call("POST", f"/orgs/{slug}/projects", {"key": "SEC", "name": "Secret", "visibility": "private"}, A)
s, b, _ = call("GET", f"/orgs/{slug}/projects/SEC", None, B)
check(s == 404, "private project hidden")
s, b, _ = call("GET", f"/orgs/{slug}/projects/proj", None, B)
check(s == 200 and b["my_role"] == "developer", "org member developer on org project")
s, b, _ = call("GET", f"/orgs/{slug}/projects", None, B)
check([x["key"] for x in b["items"]] == ["PROJ"], "project list visible only")
s, b, _ = call("PATCH", f"/orgs/{slug}/projects/PROJ", {"name": "x"}, B)
check(s == 403, "developer can't patch project")
s, b, _ = call("PUT", f"/orgs/{slug}/projects/SEC/members/{carol['id']}", {"role": "viewer"}, A)
check(s == 422 and b["errors"][0]["field"] == "user_id", "project member must be org member")
s, b, _ = call("PUT", f"/orgs/{slug}/projects/SEC/members/{bob['id']}", {"role": "viewer"}, A)
check(s == 200 and b["role"] == "viewer", "put project member")
s, b, _ = call("GET", f"/orgs/{slug}/projects/SEC", None, B)
check(s == 200 and b["my_role"] == "viewer", "viewer sees private")

# ---- issues
s, i1, h = call("POST", f"/orgs/{slug}/projects/PROJ/issues",
                {"type": "bug", "title": "  Crash on start-up  ", "labels": ["UI", "ui", "backend"],
                 "description": "The naïve café crashes", "due_date": "2026-12-01"}, B)
check(s == 201 and i1["key"] == "PROJ-1" and i1["labels"] == ["backend", "ui"] and i1["title"] == "Crash on start-up"
      and i1["priority"] == "medium" and i1["status"] == "todo" and i1["version"] == 1, f"create issue {i1}")
s, b, _ = call("POST", f"/orgs/{slug}/projects/SEC/issues", {"type": "bug", "title": "x"}, B)
check(s == 403, "viewer can't create")
s, b, _ = call("POST", f"/orgs/{slug}/projects/PROJ/issues", {"type": "bad", "title": "", "assignee_id": carol["id"]}, B)
check(s == 422 and {e["field"] for e in b["errors"]} == {"type", "title", "assignee_id"}, f"issue validation {b}")
s, b, h = call("GET", f"/orgs/{slug}/issues/proj-1", None, A)
check(s == 200 and h.get("ETag") == '"1"', "get issue + etag")
s, b, _ = call("PATCH", f"/orgs/{slug}/issues/PROJ-1", {"title": "t"}, B)
check(s == 428 and b["code"] == "precondition_required", "428")
s, b, _ = call("PATCH", f"/orgs/{slug}/issues/PROJ-1", {"title": "t"}, B, {"If-Match": '"7"'})
check(s == 412 and b["code"] == "version_mismatch", "412")
s, b, _ = call("PATCH", f"/orgs/{slug}/issues/PROJ-1", {"status": "done"}, B, {"If-Match": '"1"'})
check(s == 422 and b["errors"][0]["field"] == "status", "status in patch 422")
s, b, _ = call("PATCH", f"/orgs/{slug}/issues/PROJ-1", {"title": "Crash on start-up"}, B, {"If-Match": '"1"'})
check(s == 200 and b["version"] == 1, "no-op patch")
s, b, h = call("PATCH", f"/orgs/{slug}/issues/PROJ-1", {"title": "Crash", "priority": "high", "assignee_id": bob["id"],
                                                         "due_date": None}, B, {"If-Match": '"1"'})
check(s == 200 and b["version"] == 2 and b["assignee_id"] == bob["id"] and b["due_date"] is None, "patch")
s, b, _ = call("POST", f"/orgs/{slug}/issues/PROJ-1/transition", {"status": "done"}, B)
check(s == 409 and b["code"] == "transition_not_allowed", "bad transition")
s, b, _ = call("POST", f"/orgs/{slug}/issues/PROJ-1/transition", {"status": "in_progress"}, B, {"If-Match": '"1"'})
check(s == 412, "transition honors If-Match")
s, b, _ = call("POST", f"/orgs/{slug}/issues/PROJ-1/transition", {"status": "in_progress"}, B)
s, b, _ = call("POST", f"/orgs/{slug}/issues/PROJ-1/transition", {"status": "done"}, B)
check(s == 200 and b["resolved_at"] and b["version"] == 4, "done sets resolved_at")
s, b, _ = call("POST", f"/orgs/{slug}/issues/PROJ-1/transition", {"status": "in_progress"}, B)
check(s == 200 and b["resolved_at"] is None, "leaving done clears")
s, b, _ = call("GET", f"/orgs/{slug}/issues/PROJ-1/history", None, B)
check(s == 200 and len(b["items"]) == 5 and b["items"][-1]["changes"] == [{"field": "created", "from": None, "to": None}]
      and b["items"][-2]["changes"][0]["field"] == "title", f"history {json.dumps(b['items'][-2])}")
# comments
s, c, _ = call("POST", f"/orgs/{slug}/issues/PROJ-1/comments", {"body": "hello"}, A)
check(s == 201 and c["issue_key"] == "PROJ-1" and c["edited"] is False, "comment")
s, b, _ = call("GET", f"/orgs/{slug}/issues/PROJ-1", None, A)
check(b["comment_count"] == 1 and b["version"] == 5, "comment_count, no version bump")
s, b, _ = call("PATCH", f"/orgs/{slug}/comments/{c['id']}", {"body": "x"}, B)
check(s == 403, "non-author can't edit")
s, b, _ = call("PATCH", f"/orgs/{slug}/comments/{c['id']}", {"body": "hello2"}, A)
check(s == 200 and b["edited"] is True, "edit comment")
s, b, _ = call("DELETE", f"/orgs/{slug}/comments/{c['id']}", None, B)
check(s == 403, "developer non-author can't delete")
s, b, _ = call("DELETE", f"/orgs/{slug}/comments/{c['id']}", None, A)
check(s == 204, "admin deletes")

# concurrency numbering + idempotency
def mk_issue(i):
    return call("POST", f"/orgs/{slug}/projects/PROJ/issues", {"type": "task", "title": f"conc {i}"}, A)[1]["number"]
with cf.ThreadPoolExecutor(20) as ex:
    nums = sorted(ex.map(mk_issue, range(40)))
check(nums == list(range(2, 42)), "gap-free concurrent numbering")
key = uuid.uuid4().hex
def idem(_):
    s, b, _ = call("POST", f"/orgs/{slug}/projects/PROJ/issues", {"type": "task", "title": "idem"}, A, {"Idempotency-Key": key})
    return s, b["key"]
with cf.ThreadPoolExecutor(10) as ex:
    res = list(ex.map(idem, range(10)))
check(len(set(res)) == 1 and res[0][0] == 201, f"concurrent idempotency {set(res)}")
s, b, _ = call("POST", f"/orgs/{slug}/projects/PROJ/issues", {"type": "task", "title": "other"}, A, {"Idempotency-Key": key})
check(s == 422 and b["code"] == "idempotency_key_reused", "idempotency_key_reused")
# bulk
s, b, _ = call("POST", f"/orgs/{slug}/projects/PROJ/issues/bulk", {"issues": [{"type": "task", "title": "ok"},
                                                                             {"type": "task", "title": ""}]}, A)
check(s == 422 and b["errors"] == [{"field": "issues.1.title", "code": "too_short"}], f"bulk invalid {b}")
s, b, _ = call("POST", f"/orgs/{slug}/projects/PROJ/issues/bulk", {"issues": [{"type": "story", "title": f"bulk word{i}", "priority": ["low", "highest", "high"][i % 3], "labels": ["bulk"]} for i in range(30)]}, A)
check(s == 201 and b["keys"][0] == "PROJ-43" and len(b["keys"]) == 30, f"bulk {b['keys'][:2]}")

# listing
s, b, _ = call("GET", f"/orgs/{slug}/issues?q=CAFE", None, A)
check(b["total"] == 0, "q whole word: cafe != café")
s, b, _ = call("GET", f"/orgs/{slug}/issues?q=Caf%C3%A9%20na%C3%AFve", None, A)
check(b["total"] == 1, "q case-insensitive unicode")
s, b, _ = call("GET", f"/orgs/{slug}/issues?q=start", None, A)
check(b["total"] == 0, "q title changed to Crash")
s, b, _ = call("GET", f"/orgs/{slug}/issues?label=BULK&sort=-priority&limit=7", None, A)
allitems, cur = b["items"], b["next_cursor"]
while cur:
    s, b2, _ = call("GET", f"/orgs/{slug}/issues?label=BULK&sort=-priority&limit=7&cursor={cur}", None, A)
    allitems += b2["items"]; cur = b2["next_cursor"]
pr = {"lowest": 1, "low": 2, "medium": 3, "high": 4, "highest": 5}
exp = sorted(allitems, key=lambda x: (-pr[x["priority"]], x["project_key"], x["number"]))
check(b["total"] == 30 and len(allitems) == 30 and [x["key"] for x in allitems] == [x["key"] for x in exp], "paging -priority")
s, b, _ = call("GET", f"/orgs/{slug}/issues?status=nope", None, A)
check(s == 422 and b["errors"][0]["field"] == "status", "bad filter")
s, b, _ = call("GET", f"/orgs/{slug}/issues?assignee=me,none&sort=key&limit=100", None, B)
check(s == 200 and b["total"] == 72, f"assignee filter total {b['total']}")
s, b, _ = call("GET", f"/orgs/{slug}/issues?created_after=2000-01-01T00:00:00Z&sort=-key&limit=3", None, A)
check(s == 200 and b["items"][0]["key"] == "PROJ-72", "sort -key")

# ---- webhooks
received = []
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        received.append((dict(self.headers), body, time.time()))
        js = json.loads(body)
        fail = js["data"]["issue"]["title"].startswith("fail") and sum(1 for r in received if r[0]["X-Tracker-Delivery"] == self.headers["X-Tracker-Delivery"]) < 3
        self.send_response(500 if fail else 200); self.send_header("Content-Length", "0"); self.end_headers()
    def log_message(self, *a): pass
srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
url = f"http://127.0.0.1:{srv.server_address[1]}/hook"
s, b, _ = call("POST", f"/orgs/{slug}/webhooks", {"url": url, "events": ["issue.created", "issue.updated"], "secret": "sek"}, B)
check(s == 403, "member can't create webhook")
s, w, _ = call("POST", f"/orgs/{slug}/webhooks", {"url": url, "events": ["issue.created", "issue.updated"], "secret": "sek"}, A)
check(s == 201 and w["secret"] == "sek" and w["active"], "create webhook")
s, b, _ = call("GET", f"/orgs/{slug}/webhooks/{w['id']}", None, A)
check("secret" not in b, "secret not returned on get")
s, fi, _ = call("POST", f"/orgs/{slug}/projects/PROJ/issues", {"type": "task", "title": "fail first"}, A)
for st in ("in_progress", "done"):
    call("POST", f"/orgs/{slug}/issues/{fi['key']}/transition", {"status": st}, A)
s, ok, _ = call("POST", f"/orgs/{slug}/projects/PROJ/issues", {"type": "task", "title": "fine"}, A)
time.sleep(2)
mine = [json.loads(r[1]) for r in received if json.loads(r[1])["data"]["issue"]["key"] == fi["key"]]
seq = [(m["event"], m["data"].get("changes", [{}])[0].get("to")) for m in mine]
check(seq == [("issue.created", None)] * 3 + [("issue.updated", "in_progress")] * 3 + [("issue.updated", "done")] * 3, f"ordering with retries {seq}")
okt = [r[2] for r in received if json.loads(r[1])["data"]["issue"]["key"] == ok["key"]]
check(okt and okt[0] < received[2][2], "other issue not blocked by retries")
hdr, body, _ = received[-1]
sig = "sha256=" + hmac.new(b"sek", hdr["X-Tracker-Timestamp"].encode() + b"." + body, hashlib.sha256).hexdigest()
check(hdr["X-Tracker-Signature"] == sig and hdr["Content-Type"] == "application/json", "signature")
s, d, _ = call("GET", f"/orgs/{slug}/webhooks/{w['id']}/deliveries", None, A)
check(s == 200 and all(x["status"] == "succeeded" for x in d["items"]) and d["items"][-1]["attempts"] == 3
      and d["items"][-1]["last_status_code"] == 200, f"deliveries {d['items'][-1]}")
# failing forever
call("PATCH", f"/orgs/{slug}/webhooks/{w['id']}", {"url": "http://127.0.0.1:1/x"}, A)
call("POST", f"/orgs/{slug}/projects/PROJ/issues", {"type": "task", "title": "dead"}, A)
time.sleep(3)
s, d, _ = call("GET", f"/orgs/{slug}/webhooks/{w['id']}/deliveries?limit=1", None, A)
check(d["items"][0]["status"] == "failed" and d["items"][0]["attempts"] == 6 and d["items"][0]["last_status_code"] is None, f"failed after 6 {d['items'][0]}")

# member removal unassigns
s, _, _ = call("DELETE", f"/orgs/{slug}/members/{bob['id']}", None, A)
s, b, _ = call("GET", f"/orgs/{slug}/issues/PROJ-1", None, A)
check(s == 200 and b["assignee_id"] is None, "removal unassigns")
s, b, _ = call("DELETE", f"/orgs/{slug}/issues/PROJ-1", None, A)
s2, _, _ = call("GET", f"/orgs/{slug}/issues/PROJ-1", None, A)
check(s == 204 and s2 == 404, "delete issue")
print("FAILS:", FAILS)
