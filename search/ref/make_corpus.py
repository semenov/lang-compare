#!/usr/bin/env python3
"""Convert a CirrusSearch content dump (.json.gz) to the corpus format of SPEC.md.

Usage: make_corpus.py <dump.json.gz> <out.jsonl> [--limit N]
Keeps namespace-0 pages with non-empty text; id = page_id.
"""
import gzip
import json
import sys


def main():
    src, dst = sys.argv[1], sys.argv[2]
    limit = int(sys.argv[sys.argv.index("--limit") + 1]) if "--limit" in sys.argv else None
    n = 0
    with gzip.open(src, "rt", encoding="utf-8") as f, open(dst, "w", encoding="utf-8") as out:
        for line in f:
            d = json.loads(line)
            if "index" in d or str(d.get("namespace")) != "0" or not d.get("text"):
                continue
            out.write(json.dumps({"id": int(d["page_id"]), "title": d["title"], "text": d["text"]},
                                 ensure_ascii=False) + "\n")
            n += 1
            if limit and n >= limit:
                break
    print(f"{n} docs -> {dst}", file=sys.stderr)


if __name__ == "__main__":
    main()
