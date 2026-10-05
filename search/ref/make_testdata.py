#!/usr/bin/env python3
"""Build testdata/ for the agents: corpus.jsonl (N wiki articles + edge cases), queries, expected.jsonl.

Usage: make_testdata.py <full-corpus.jsonl> <testdata-dir> [--n 5000] [--seed 1]
"""
import json
import random
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import search_ref as ref  # noqa: E402

EDGE_BASE = 2_000_000_000
EDGE_DOCS = [
    ("Ünïcödé Tëst", "Grüße aus Köln. ΟΔΟΣ οδός Σίσυφος. Москва — столица России. 東京都 渋谷区. "
                     "İstanbul ISTANBUL istanbul. Ⅻ ½ ² 3rd x²+y²."),
    ("Emoji 🙂 page", "I ❤ pizza🍕and café-au-lait; e-mail me at foo@bar.example, ok? C++ C# .NET"),
    ("Repeated", "buffalo buffalo Buffalo buffalo buffalo buffalo Buffalo buffalo"),
    ("New\nline title", "text after a title with a newline. \"Quoted\" words and \\backslashes\\ and\ttabs"),
    ("Phrase boundary", "york is here"),
    ("New York", "New York city is big. York is old. new, york! newyork"),
    ("Numbers", "1984 2001 2,001 3.14159 007 0 00 1e10 1_000 ٣ ໓"),
    ("Mixed scripts", "abcабв αβγ mixedСкрипт snake_case CamelCase dash-separated-words"),
    ("Empty-ish", "!!! ... --- ???"),
    ("Combining", "Café vs Café; naïve vs naïve"),
    ("Long word", "pneumonoultramicroscopicsilicovolcanoconiosis supercalifragilisticexpialidocious"),
    ("Same score A", "zebra quagga"),
    ("Same score B", "zebra quagga"),
    ("Same score C", "zebra quagga"),
]

HAND_QUERIES = [
    "the", "THE", "april", "April month", "\"fourth month\"", "month -april", "-april", "-", "\"\"", "   ",
    "grüße", "GRÜSSE", "köln", "οδος", "οδοσ", "ΟΔΟΣ", "σίσυφος", "москва", "столица россии", "東京都",
    "istanbul", "İstanbul", "ⅻ", "½", "²", "x", "y²",
    "pizza", "pizza🍕", "🍕", "❤", "café", "cafe", "au-lait", "\"café au lait\"", "e-mail", "email", "c", "net",
    "buffalo", "\"buffalo buffalo buffalo\"", "\"buffalo buffalo buffalo buffalo buffalo buffalo buffalo buffalo\"",
    "\"buffalo buffalo buffalo buffalo buffalo buffalo buffalo buffalo buffalo\"",
    "title", "\"line title\"", "\"title text\"", "\"new line\"", "quoted", "backslashes",
    "\"new york\"", "new york", "-\"new york\" york", "york -new", "\"york is\"", "\"phrase boundary york\"",
    "newyork", "\"new york city\"", "\"york new\"",
    "1984", "2001", "001", "3", "14159", "007", "0", "00", "1e10", "1_000", "1", "٣",
    "snake_case", "snake", "camelcase", "mixedскрипт", "абв", "αβγ", "dash-separated-words", "\"dash separated\"",
    "pneumonoultramicroscopicsilicovolcanoconiosis", "nosuchwordanywhere", "the nosuchwordanywhere",
    "zebra", "quagga zebra", "\"zebra quagga\"", "\"quagga zebra\"",
    "\"unterminated phrase", "-\"unterminated", "a\"b c\"d", "\"a b\"c", "--april", "april-", "april--month",
    "the of and", "the -the", "\"of the\" -\"in the\"",
]


def doc_tokens(d):
    return ref.tokenize(d["title"] + "\n" + d["text"])


def main():
    src, outdir = sys.argv[1], Path(sys.argv[2])
    n = int(sys.argv[sys.argv.index("--n") + 1]) if "--n" in sys.argv else 5000
    rnd = random.Random(int(sys.argv[sys.argv.index("--seed") + 1]) if "--seed" in sys.argv else 1)
    outdir.mkdir(parents=True, exist_ok=True)

    docs = []
    for line in open(src, encoding="utf-8"):
        docs.append(json.loads(line))
        if len(docs) >= n:
            break
    for i, (title, text) in enumerate(EDGE_DOCS):
        docs.append({"id": EDGE_BASE + i, "title": title, "text": text})
    rnd.shuffle(docs)  # ids are not sorted in the corpus
    with open(outdir / "corpus.jsonl", "w", encoding="utf-8") as f:
        for d in docs:
            f.write(json.dumps(d, ensure_ascii=rnd.random() < 0.5) + "\n")  # mix of escaped/raw unicode

    # generated queries from the wiki articles
    wiki = [d for d in docs if d["id"] < EDGE_BASE]
    df = {}
    for d in wiki:
        for t in set(doc_tokens(d)):
            df[t] = df.get(t, 0) + 1
    by_df = sorted(df, key=lambda t: (df[t], t))
    rare = [t for t in by_df if df[t] <= 3]
    mid = [t for t in by_df if 20 <= df[t] <= 300]
    common = by_df[-200:]
    q = list(HAND_QUERIES)
    q += rnd.sample(rare, 40) + rnd.sample(mid, 60) + rnd.sample(common, 20)
    for _ in range(60):
        q.append(" ".join(rnd.sample(mid, 2)))
        q.append(f"{rnd.choice(common)} {rnd.choice(mid)}")
    for _ in range(30):
        q.append(f"{rnd.choice(mid)} -{rnd.choice(common)}")
        q.append(f"{rnd.choice(common)} {rnd.choice(common)} -\"{rnd.choice(common)} {rnd.choice(common)}\"")
    for _ in range(80):  # phrases sampled from real text, 2..5 tokens, sometimes upper-cased or mixed with a word
        toks = doc_tokens(rnd.choice(wiki))
        if len(toks) < 6:
            continue
        L = rnd.randint(2, 5)
        i = rnd.randrange(len(toks) - L)
        phrase = " ".join(toks[i:i + L])
        q.append(f"\"{phrase.upper() if rnd.random() < 0.2 else phrase}\"" + (f" {rnd.choice(mid)}" if rnd.random() < 0.3 else ""))
    for _ in range(20):  # titles as queries
        q.append(rnd.choice(wiki)["title"])
    q = list(dict.fromkeys(q))
    (outdir / "queries.txt").write_text("\n".join(q) + "\n", encoding="utf-8")

    idx = ref.Index(outdir / "corpus.jsonl")
    with open(outdir / "expected.jsonl", "w", encoding="utf-8") as f:
        for i, query in enumerate(q):
            for k in ([10] if i % 7 else [10, 1000]):
                total, hits = idx.search(query, k)
                f.write(json.dumps({"q": query, "k": k, "total": total, "hits": hits}, ensure_ascii=False) + "\n")
    stats = dict(docs=len(docs), queries=len(q), avgdl=round(idx.avgdl, 1), terms=len(idx.postings))
    print(stats, file=sys.stderr)


if __name__ == "__main__":
    main()
