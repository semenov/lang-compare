#!/usr/bin/env python3
"""Reference implementation of SPEC.md semantics, used to generate expected results.
Deliberately simple (in-memory, Python); NOT given to the agents.

Usage: search_ref.py <corpus.jsonl> <queries.txt> <out expected.jsonl> [--k 10,100]
Each line of queries.txt is one raw query string. Each output line is
{"q": ..., "k": ..., "total": ..., "hits": [{"id", "title", "score"}]}.
"""
import json
import math
import sys
import unicodedata
from collections import defaultdict

ALNUM = {"Lu", "Ll", "Lt", "Lm", "Lo", "Nd", "Nl", "No"}
K1, B = 1.2, 0.75


def simple_lower(c):
    lo = c.lower()
    if len(lo) == 1:
        return lo
    # str.lower() uses full mappings; the only unconditional multi-char lowercase is U+0130
    assert c == "İ", c
    return "i"


_cache = {}


def is_alnum(c):
    r = _cache.get(c)
    if r is None:
        r = _cache[c] = unicodedata.category(c) in ALNUM
    return r


def tokenize(s):
    out, cur = [], []
    for c in s:
        if is_alnum(c):
            cur.append(simple_lower(c))
        elif cur:
            out.append("".join(cur))
            cur = []
    if cur:
        out.append("".join(cur))
    return out


def is_space(c):
    # Unicode White_Space property
    return c in "\t\n\x0b\x0c\r \x85\xa0           " \
                "     　"


def parse_query(q):
    """-> list of (negative: bool, tokens: list[str]); clauses without tokens dropped."""
    clauses, i, n = [], 0, len(q)
    while True:
        while i < n and is_space(q[i]):
            i += 1
        if i >= n:
            break
        neg = False
        if q[i] == "-":
            neg, i = True, i + 1
        if i < n and q[i] == '"':
            j = q.find('"', i + 1)
            body, i = (q[i + 1:], n) if j < 0 else (q[i + 1:j], j + 1)
        else:
            j = i
            while j < n and not is_space(q[j]):
                j += 1
            body, i = q[i:j], j
        toks = tokenize(body)
        if toks:
            clauses.append((neg, toks))
    return clauses


class Index:
    def __init__(self, corpus):
        self.docs = {}  # id -> (title, tokens)
        self.postings = defaultdict(dict)  # token -> {id: [positions]}
        for line in open(corpus, encoding="utf-8"):
            d = json.loads(line)
            toks = tokenize(d["title"] + "\n" + d["text"])
            self.docs[d["id"]] = (d["title"], len(toks))
            for pos, t in enumerate(toks):
                self.postings[t].setdefault(d["id"], []).append(pos)
        self.N = len(self.docs)
        self.avgdl = sum(dl for _, dl in self.docs.values()) / self.N

    def match_clause(self, toks):
        first = self.postings.get(toks[0], {})
        if len(toks) == 1:
            return set(first)
        res = set()
        for doc, positions in first.items():
            others = []
            for t in toks[1:]:
                p = self.postings.get(t, {}).get(doc)
                if p is None:
                    break
                others.append(set(p))
            else:
                if any(all(p + k + 1 in others[k] for k in range(len(others))) for p in positions):
                    res.add(doc)
        return res

    def search(self, q, k):
        clauses = parse_query(q)
        pos = [t for neg, t in clauses if not neg]
        if not pos:
            return 0, []
        matched = None
        for toks in pos:
            m = self.match_clause(toks)
            matched = m if matched is None else matched & m
        for neg, toks in clauses:
            if neg:
                matched -= self.match_clause(toks)
        terms = sorted({t for toks in pos for t in toks})
        idf = {}
        for t in terms:
            df = len(self.postings.get(t, {}))
            idf[t] = math.log(1 + (self.N - df + 0.5) / (df + 0.5))
        scored = []
        for doc in matched:
            dl = self.docs[doc][1]
            norm = K1 * (1 - B + B * dl / self.avgdl)
            s = 0.0
            for t in terms:
                tf = len(self.postings[t].get(doc, ()))
                if tf:
                    s += idf[t] * tf * (K1 + 1) / (tf + norm)
            scored.append((-s, doc))
        scored.sort()
        hits = [{"id": d, "title": self.docs[d][0], "score": -s} for s, d in scored[:k]]
        return len(matched), hits


def main():
    corpus, queries, out = sys.argv[1:4]
    ks = [10]
    if "--k" in sys.argv:
        ks = [int(x) for x in sys.argv[sys.argv.index("--k") + 1].split(",")]
    idx = Index(corpus)
    with open(out, "w", encoding="utf-8") as f:
        for q in open(queries, encoding="utf-8").read().splitlines():
            for k in ks:
                total, hits = idx.search(q, k)
                f.write(json.dumps({"q": q, "k": k, "total": total, "hits": hits}, ensure_ascii=False) + "\n")


if __name__ == "__main__":
    main()
