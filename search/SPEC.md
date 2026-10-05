# Full-text search engine

Build a full-text search engine over a corpus of Wikipedia articles: an **indexer** that builds an
on-disk index, and an HTTP **server** that answers queries from that index with BM25 ranking.

## Program

One binary, two subcommands:

- `search index <corpus.jsonl> <index-dir>`: reads the corpus and writes the index into `index-dir`
  (created if missing). Any on-disk format. It may use all CPU cores. Exit code 0 on success.
- `search serve <index-dir>`: serves HTTP on `0.0.0.0:$PORT` (default `8080`) using only the files in
  `index-dir` (the corpus file is not available to the server).

### Corpus

JSON Lines, UTF-8, one article per line:

```
{"id": 12, "title": "Anarchism", "text": "Anarchism is a political philosophy ..."}
```

`id` is a unique non-negative integer (< 2^31); lines are not necessarily sorted by id. `title` and `text` are
arbitrary Unicode strings (may contain newlines, quotes, emoji, any script).

## Text processing

The searchable content of a document is `title + "\n" + text`.

**Tokens** are maximal runs of characters whose Unicode general category is a letter or a number
(`Lu Ll Lt Lm Lo Nd Nl No`). Everything else separates tokens. Each token is lowercased **character by
character** with the Unicode *simple* lowercase mapping (`UnicodeData.txt`, field 13; characters without a
mapping stay unchanged). For example, `"Hello, Wörld! e-mail 2024 İstanbul"` → `hello`, `wörld`, `e`, `mail`, `2024`, `istanbul`
(U+0130 maps to U+0069). No stemming, no stop words, no other normalization.
Any Unicode version ≥ 15.0 is fine; the tests don't use characters whose properties changed between versions.

`dl` (document length) = the number of tokens in the document. Token positions are 0, 1, 2, … in order.

## Queries

`GET /search?q=<query>&k=<n>` (URL-encoded; `k` defaults to 10, `1 ≤ k ≤ 1000`).

The query is parsed left to right into clauses:

1. Skip whitespace (Unicode `White_Space`). Stop at the end of the query.
2. If the next character is `-`, the clause is **negative**; consume the `-`.
3. If the next character is `"`, the clause is a **phrase**: everything up to the next `"` (consumed) or to
   the end of the query. Otherwise the clause is a **word**: everything up to the next whitespace or the end of the query
   (`"` inside a word is an ordinary character). Go to step 1.

So `-"new york" city` has a negative phrase `new york` and a positive word `city`; `"a b"c` has a phrase `a b` and a word `c`;
a lone `-` is a negative clause with no tokens.

- Each clause is tokenized with the rules above. A clause that yields no tokens is ignored.
- A clause with one token matches documents that contain that token.
- A clause with several tokens (a quoted phrase, or a word like `e-mail` that splits into `e`, `mail`)
  matches documents where those tokens occur at **consecutive positions** in that order.
- A document matches the query if it matches **every** positive clause and **no** negative clause.
- A query with no positive clauses matches nothing.

### Ranking (BM25)

Let `T` be the set of distinct tokens from positive clauses. For a matching document `d`:

```
score(d) = Σ_{t ∈ T} idf(t) · tf(t,d)·(k1 + 1) / (tf(t,d) + k1·(1 − b + b·dl(d)/avgdl))

idf(t) = ln(1 + (N − df(t) + 0.5) / (df(t) + 0.5))
k1 = 1.2, b = 0.75
```

`N` = the number of documents in the corpus, `df(t)` = the number of documents containing `t`, `tf(t,d)` = occurrences of `t` in `d`,
`avgdl` = the mean `dl` over all documents. Use 64-bit floating point.

Results are sorted by score descending, then by `id` ascending.

### Response

`200`, `Content-Type: application/json`:

```json
{"total": 1234, "hits": [{"id": 12, "title": "Anarchism", "score": 7.123456}, ...]}
```

`total` is the exact number of matching documents; `hits` are the top `k` of them. `score` must be within a relative error of 1e-6
of the exact value. Invalid `k` or missing `q` → `400 {"error": "..."}`.

### Other endpoints

- `GET /doc/<id>` → `200 {"id": .., "title": "..", "text": ".."}` with the original title and text, or `404 {"error": "not found"}`.
- `GET /health` → `200 {"status": "ok"}` once the server is ready to answer queries.

The server must handle many concurrent connections and use multiple cores.

## Acceptance

`python3 run_tests.py <path-to-binary>` reports 0 failed. It indexes `testdata/corpus.jsonl` (5,000 Simple English
Wikipedia articles plus hand-written edge cases), starts the server, and compares the responses with `testdata/expected.jsonl`.

## Performance

The engine will be benchmarked on the full Simple English Wikipedia (~250k articles, about 1 GB of text) and larger corpora:
indexing time and peak memory, index size on disk, server startup time, server memory, query throughput and latency
under concurrent load.
