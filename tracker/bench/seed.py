#!/usr/bin/env python3
"""Seed a tracker instance through its public API with a deterministic dataset.

Usage: seed.py <base-url> [--issues 1000000] [--projects 20] [--users 50] [--comments 100000]
               [--transitions 100000] [--out seed.json]

Everything goes through the API, so the same data ends up in every implementation regardless of its schema.
Writes users/projects/issue counts and timings to --out (used by the k6 scenario).
"""
import argparse
import json
import random
import threading
import time
import http.client
import urllib.parse
from concurrent.futures import ThreadPoolExecutor

PASSWORD = "benchmark-password-1"
ORG = "acme"


_local = threading.local()


def req(base, method, path, body=None, token=None, headers=None, timeout=300):
    """One persistent keep-alive connection per thread (a new TCP connection per request exhausts
    macOS's ephemeral ports through TIME_WAIT when the server keeps connections open)."""
    u = urllib.parse.urlsplit(base)
    hdrs = {"Content-Type": "application/json"}
    if token:
        hdrs["Authorization"] = "Bearer " + token
    hdrs.update(headers or {})
    data = json.dumps(body).encode() if body is not None else None
    while True:
        conn = getattr(_local, "conn", None)
        reused = conn is not None
        if conn is None:
            conn = _local.conn = http.client.HTTPConnection(u.hostname, u.port, timeout=timeout)
        try:
            conn.request(method, "/api/v1" + path, body=data, headers=hdrs)
            resp = conn.getresponse()
            raw = resp.read()
        except (http.client.RemoteDisconnected, ConnectionResetError, BrokenPipeError):
            conn.close()
            _local.conn = None
            if reused:  # the server closed an idle keep-alive connection; the request was not processed
                continue
            raise
        if resp.getheader("Connection", "").lower() == "close":
            conn.close()
            _local.conn = None
        try:
            return resp.status, (json.loads(raw) if raw else None)
        except ValueError:
            return resp.status, raw


def must(res, *ok):
    if res[0] not in ok:
        raise RuntimeError(f"unexpected {res[0]}: {str(res[1])[:300]}")
    return res[1]


def make_vocab(rnd, n=5000):
    syl = ["ka", "lo", "mi", "ne", "ru", "ta", "vi", "so", "pe", "da", "gu", "zo", "fa", "be", "xi", "qu", "wy", "ho",
           "ja", "ce", "ri", "mo", "nu", "sa", "te", "li", "po", "de", "ga", "fi"]
    words = set()
    while len(words) < n:
        words.add("".join(rnd.choice(syl) for _ in range(rnd.randint(2, 4))))
    return sorted(words)


def zipf_picker(rnd, items, s=1.1):
    weights = [1 / (i + 1) ** s for i in range(len(items))]
    cum, t = [], 0.0
    for w in weights:
        t += w
        cum.append(t)
    import bisect

    def pick():
        return items[bisect.bisect_left(cum, rnd.random() * t)]
    return pick


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("base")
    ap.add_argument("--issues", type=int, default=1_000_000)
    ap.add_argument("--projects", type=int, default=20)
    ap.add_argument("--users", type=int, default=50)
    ap.add_argument("--comments", type=int, default=100_000)
    ap.add_argument("--transitions", type=int, default=100_000)
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--out", default="seed.json")
    a = ap.parse_args()
    base = a.base.rstrip("/")
    rnd = random.Random(42)
    vocab = make_vocab(rnd)
    word = zipf_picker(rnd, vocab)
    labels_pool = [f"label-{i}" for i in range(30)]
    label = zipf_picker(rnd, labels_pool, 1.3)
    timings = {}

    t0 = time.time()
    emails = ["owner@acme.test"] + [f"user{i:03d}@acme.test" for i in range(a.users)]

    def register(e):
        must(req(base, "POST", "/auth/register", {"email": e, "password": PASSWORD, "name": e.split("@")[0]}), 201)
        return must(req(base, "POST", "/auth/login", {"email": e, "password": PASSWORD}), 200)["access_token"]

    with ThreadPoolExecutor(a.workers) as ex:
        tokens = list(ex.map(register, emails))
    owner = tokens[0]
    users = {e: must(req(base, "GET", "/me", token=t), 200)["id"] for e, t in zip(emails, tokens)}
    must(req(base, "POST", "/orgs", {"name": "Acme", "slug": ORG}, owner), 201)
    for e in emails[1:]:
        must(req(base, "POST", f"/orgs/{ORG}/members", {"email": e, "role": "member"}, owner), 201)
    projects = [f"P{i:02d}" for i in range(a.projects)]
    for i, key in enumerate(projects):
        private = i >= a.projects - 2  # last two projects are private with a few explicit members
        must(req(base, "POST", f"/orgs/{ORG}/projects",
                 {"key": key, "name": f"Project {key}", "visibility": "private" if private else "org"}, owner), 201)
        if private:
            for e in emails[1:11]:
                must(req(base, "PUT", f"/orgs/{ORG}/projects/{key}/members/{users[e]}", {"role": "developer"}, owner), 200)
    timings["setup_s"] = round(time.time() - t0, 1)

    # issues: bulk create, 1000 per request, projects in parallel
    member_ids = [users[e] for e in emails[1:]]
    per_project = a.issues // a.projects

    private_ids = [users[e] for e in emails[1:11]]

    def gen_issue(r, assignees):
        t = r.random()
        desc = None if r.random() < 0.1 else " ".join(word() for _ in range(r.randint(20, 120)))
        issue = {"type": "task" if t < 0.6 else "bug" if t < 0.85 else "story",
                 "title": " ".join(word() for _ in range(r.randint(4, 10))),
                 "description": desc,
                 "priority": r.choices(["highest", "high", "medium", "low", "lowest"], [5, 15, 50, 20, 10])[0],
                 "labels": sorted({label() for _ in range(r.choice([0, 0, 1, 1, 2, 3]))}),
                 "assignee_id": r.choice(assignees) if r.random() < 0.8 else None}
        if r.random() < 0.3:
            issue["due_date"] = f"2026-{r.randint(1, 12):02d}-{r.randint(1, 28):02d}"
        return issue

    # pre-generate batches deterministically (word() uses the shared rnd, so do it single-threaded)
    t0 = time.time()
    batches = []
    for key in projects:
        for start in range(0, per_project, 1000):
            pool = private_ids if key in projects[-2:] else member_ids
            batches.append((key, [gen_issue(rnd, pool) for _ in range(min(1000, per_project - start))]))
    timings["generate_s"] = round(time.time() - t0, 1)

    t0 = time.time()
    done = [0]
    lock = threading.Lock()

    def bulk(item):
        key, issues = item
        # private projects only have explicit members (users 0..9), so their issues come from the owner
        tok = owner if key in projects[-2:] else tokens[1 + int(key[1:]) % a.users]
        must(req(base, "POST", f"/orgs/{ORG}/projects/{key}/issues/bulk", {"issues": issues}, tok), 201)
        with lock:
            done[0] += len(issues)
            if done[0] % 100_000 == 0:
                print(f"  issues: {done[0]} ({time.time() - t0:.0f}s)", flush=True)

    # one worker per project at a time keeps per-project numbering contention realistic
    by_project = {}
    for b in batches:
        by_project.setdefault(b[0], []).append(b)

    def run_project(key):
        for b in by_project[key]:
            bulk(b)

    with ThreadPoolExecutor(a.workers) as ex:
        list(ex.map(run_project, projects))
    timings["bulk_issues_s"] = round(time.time() - t0, 1)
    timings["bulk_issues_per_s"] = round(a.issues / timings["bulk_issues_s"])

    # transitions and comments: individual requests, concurrent
    r2 = random.Random(7)
    trans = []
    for _ in range(a.transitions):
        trans.append((r2.choice(projects[:-2]), r2.randint(1, per_project), r2.random() < 0.4))
    t0 = time.time()

    def transition(t):
        key, n, to_done = t
        tok = tokens[1 + n % a.users]
        st, body = req(base, "POST", f"/orgs/{ORG}/issues/{key}-{n}/transition", {"status": "in_progress"}, tok)
        if st == 200 and to_done:
            req(base, "POST", f"/orgs/{ORG}/issues/{key}-{n}/transition", {"status": "done"}, tok)

    with ThreadPoolExecutor(a.workers * 4) as ex:
        list(ex.map(transition, trans))
    timings["transitions_s"] = round(time.time() - t0, 1)

    comments = []
    for _ in range(a.comments):
        comments.append((r2.choice(projects[:-2]), r2.randint(1, per_project),
                         " ".join(r2.choice(vocab) for _ in range(r2.randint(5, 40)))))
    t0 = time.time()

    def comment(c):
        key, n, body = c
        must(req(base, "POST", f"/orgs/{ORG}/issues/{key}-{n}/comments", {"body": body}, tokens[1 + n % a.users]), 201)

    with ThreadPoolExecutor(a.workers * 4) as ex:
        list(ex.map(comment, comments))
    timings["comments_s"] = round(time.time() - t0, 1)

    out = {"org": ORG, "password": PASSWORD, "emails": emails[1:], "projects": projects[:-2],
           "private_projects": projects[-2:], "per_project": per_project, "labels": labels_pool[:15],
           "search_words": vocab[200:1200], "timings": timings}
    json.dump(out, open(a.out, "w"), indent=1)
    print(json.dumps(timings), flush=True)


if __name__ == "__main__":
    main()
