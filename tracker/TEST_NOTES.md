# run_tests.py: ambiguities and interpretations

Places where SPEC.md leaves room for interpretation, and what `run_tests.py` does about each one. Each entry
says whether the suite **accepts all readings**, **does not test** the point, or **tests one reading** (used only
where the other reading seemed unreasonable). Each entry ends with a suggested clarification for the spec.

## §2 API conventions

1. **Validation codes for pattern-constrained fields.** The org slug, project key and label have both a regex and an
   implied length (3..40, 2..10, 1..50). If a value is too short or too long for the regex, the code could be
   `invalid`, `too_short` or `too_long`. *Accepts all three.* The same goes for an email longer than 254 chars
   (`too_long` or `invalid`).
   Suggestion: "a value that fails a pattern is `invalid`, even if it fails because of its length".
2. **Empty and blank required strings** (`""`, or `"   "` for trimmed fields): `required` or `too_short`.
   *Accepts both.* For a missing field the suite requires `required`. Fields that have the wrong JSON type or are
   `null` where not nullable accept any code.
   Suggestion: name the code for empty and blank strings and for wrong types.
3. **"chars" means Unicode code points.** *Tests this reading* with 255/256 × `é` titles and a 100 × `é` name.
   Counting bytes is not a reasonable reading of "chars". Grapheme counting gives the same result for the
   precomposed `é` used here.
4. **Bodies that are valid JSON but not an object** (`[]`, `null`): *not tested* (400 or 422 would both be plausible).
5. **Unknown or malformed UUIDs in paths** (comment id, user id): *only well-formed unknown UUIDs are tested* (→ 404).
6. **Pagination cursors:** an invalid or garbage `cursor` value is *not tested* (400, 422 or ignoring it would all be
   plausible). When the last page is partial, the suite expects `next_cursor: null`. Page counts are only asserted
   for lists whose size is not an exact multiple of `limit`, so an implementation may return one empty trailing page
   when the last page is exactly full.
   Suggestion: specify the behaviour for invalid cursors.
7. **Non-integer `limit`** (`abc`, `1.5`, `-1`) → 422 with field `limit`: *tested* ("wrong types or invalid values →
   422"). An empty `limit=` is *not tested*.

## §3 Auth

8. **JWT edge cases.** A token with `typ` ≠ `"access"` is treated as invalid (→ `401 unauthenticated`): *tested*.
   *Not tested*:
   - the precedence between expiry and a bad signature (expired + wrong signature);
   - a correctly signed token whose `sub` is not an existing user;
   - tokens with missing claims.

   Suggestion: "the signature is checked first; `token_expired` is only returned for correctly signed tokens".
9. **Refresh/logout edge cases.** *Not tested*:
   - logout with an unknown or already-used token (204 or 401?);
   - presenting a token that is both used and revoked (`token_reused` or `invalid_token`);
   - a missing `refresh_token` field (401 or 422);
   - whether access tokens stay valid after their refresh chain is revoked (they are stateless JWTs, so presumably they do).

   The 30-day expiry and the Argon2id parameters are not observable through the API and are *not tested*.
10. **Login email** is compared lower-cased: *tested* (log in with the upper-cased email).

## §4 Orgs and projects

11. **POST /orgs/{slug}/members by an admin with role `admin`/`owner`.** "Only owners may grant … owner/admin" is
    stated under PATCH. It is not clear whether it also limits adding a new member with an elevated role.
    *Not tested.* An owner adding a member as `owner`/`admin` is used as a fixture.
    Suggestion: say explicitly that the rule also applies to POST.
12. **DELETE of an admin or owner by an admin.** "Take away owner/admin" could cover removal. *Not tested.* An owner
    removing another owner (while one more owner remains) is tested → 204.
13. **DELETE of a user who is not a member** → *accepts 404 or 204*.
14. **Precedence of 403 vs 409 `last_owner`** when an admin tries to demote the only owner: the suite avoids the
    question. It adds a second owner before testing "admin demotes owner → 403".
15. **Org-member removal and issue side effects.** Whether unassigning the removed user's issues bumps `version`,
    adds history entries or emits `issue.updated` webhooks is *not tested*. The suite only checks that `assignee_id`
    becomes null.
    Suggestion: specify it (most natural: it is a normal update by the remover, or explicitly "silent").
16. **`my_role` on a project is the effective role** (not only the explicit membership): *tested*. Examples:
    - explicit `viewer` on an org-visible project → `developer`;
    - org admin with no explicit membership → `admin`.
17. **Project key / org slug case in paths.** Only issue keys are specified as case-insensitive, so lower-case project
    keys and slugs in paths are *not tested*. A lower-case key in `POST /projects` is expected to fail validation
    (→ 422): the spec gives a regex and, unlike labels, no normalization.
18. **Response body of PATCH org member / PUT project member.** Only the status code is checked. The resulting
    state is verified with the list endpoints.
19. **Who can list project members.** The suite assumes "view the project" (viewer+) includes the project member list.
20. **Field name for "PUT project member: user is not an org member → 422"**: only `validation_failed` is checked,
    not the field name (`user_id`?).

## §5 Issues

21. **Bulk numbering order.** The suite assumes `keys[i]` is the issue made from `issues[i]` and that numbers in one
    bulk request increase in request order. With concurrent requests it only checks that the numbers increase.
22. **Bulk side effects** (idempotency keys, one `issue.created` webhook per item) are *not tested*. An invalid bulk
    request must produce no events (tested; nothing is created).
23. **Idempotency details.**
    - Concurrent same-key requests: exactly one issue is created. The other requests may answer 201 with the same body,
      or any 4xx (e.g. 409 while the first one is in flight): *accepts both*.
    - An `Idempotency-Key` outside 1..255 chars → *accepts 400 or 422*.
    - *Not tested*: replay of a stored error response; the same key on a different project URL; the 24 h expiry;
      whether "same body" means byte-equal or JSON-equal (the suite replays byte-identical bodies and uses a
      different title for the "different body" case).

    Suggestion: specify the losers' response, and whether the key is scoped per endpoint.
24. **No-op PATCH.** It must return 200 without bumping `version` or `updated_at` (tested, including labels that are
    equal after normalization). Whether it adds an (empty) history entry or emits `issue.updated` is *not tested*.
    Suggestion: "a no-op patch adds no history entry and emits no event".
25. **Transition to the current status** (e.g. `todo → todo`): *not tested* (409 per the table, or a no-op 200?).
26. **If-Match vs other errors.** The order of permission (403), If-Match (428/412) and validation (422) checks
    on the same request is *not tested*. Permission tests always send a correct `If-Match`. Stale-`If-Match` tests
    use otherwise valid bodies.
27. **If-Match syntax.** Only the quoted form `"N"` is sent; `W/"N"`, `*` and unquoted values are *not tested*.
28. **`updated_at` must advance with every version bump.** The suite sleeps ≥ 5 ms before such checks so that
    millisecond precision cannot produce equal values.
29. **Label sort order** ("returned sorted"): the suite compares with Python's code-point order. Tests that check
    the order use only `[a-z0-9]` labels, where code-point order and locale collation agree. Labels with `.`, `-`
    or `_` are only checked as a set.
    Suggestion: "sorted by byte/code-point order".
30. **Duplicates and the 20-label limit**: whether the limit applies before or after removing duplicates is *not
    tested*. The tests use 20 and 21 distinct labels.

## §5.3 / §5.4 Comments and history

31. **`comment_count` after deleting a comment** is expected to go down (count of existing comments): *tested*.
32. **Comments do not change `version` or `updated_at`**: *tested* ("`updated_at` changes together with `version`").
    Whether a comment adds a history entry is *not tested*: the history tests add no comments.
33. **Order of `changes` inside one history entry** is unspecified, so the suite compares them as a map from field to
    (from, to). Failed operations (412, 409, 422) must not add entries: *tested*.

## §6 Listing and search

34. **Multi-word `q`:** "every word appears … in the title or description" can mean:
    - each word may be in either field (word A in the title, word B in the description), or
    - all words must be in the same field.

    The data includes cases that tell the two apart (`alpha beta`, `über ärger`). *Accepts either reading*, as long
    as `items` and `total` agree with the same reading.
    Suggestion: "each word must appear in the title or in the description".
35. **No stemming, no accent folding, no prefix matching:** *tested*. `run` ≠ `Running`, `naive` ≠ `Naïve`,
    `日本` ≠ `日本語`, and `art` does not match `start`. This follows from "whole word, case-insensitive", but an
    implementation built on PostgreSQL full-text search (stemming) or `unaccent` would fail these tests.
36. **Case folding** uses simple lower-casing (`ПРИВЕТ` = `привет`, `CAFÉ` = `café`, `ÜBER` = `über`). Characters
    with special folding (`ß`, Turkish `İ`) are avoided. The data uses only letters (L*) and decimal digits (Nd),
    plus ASCII punctuation as separators (`-`, `_`, `/`, `,`, `.`, parentheses). Underscore is a separator.
37. **A `q` with no words** (e.g. `q=!!!`): *not tested*.
38. **`project=`, `label=` with values that do not exist or are not visible** (unknown key, private project, upper-case
    label): *not tested* (empty result or 422 would both be plausible).
39. **Timestamp precision vs. filters and sorting.** The service returns millisecond timestamps but may store
    microseconds.
    - Filter boundaries are placed at least 2 ms away from every issue's timestamp, so truncation and rounding
      cannot change the result.
    - For `created`/`updated` sorts, the sequence of millisecond values must match. The order of items with the
      same millisecond value is not checked, because they may differ at the microsecond level. The `key`
      tie-break is fully checked through the `priority` sorts.
    - RFC 3339 filter values with an offset (`+02:00`) are tested, because RFC 3339 allows offsets.

    Suggestion: say whether filters and sorting work on the returned (millisecond) values.
40. **Field name for an invalid `limit`/`sort`** on the issue list: the suite expects `limit` and `sort`, the same as
    the filter names.

## §7 Webhooks

41. **`comment.created` is "about the same issue"** for the per-issue ordering rule. The payload test expects
    `issue.created`, `issue.updated` ×2, `comment.created` and `issue.deleted` for one issue to arrive in that order.
42. **`data.issue` in `comment.created`**: only its `key` is checked. Whether it shows `comment_count` before or after
    the comment is not specified. For the other events, `data.issue` must equal the API response of the operation
    (or, for `issue.deleted`, the last GET before deletion).
43. **Timeout handling** (a 2xx arriving after 5 s is a failure): *tested* with a receiver that answers the first
    attempt after 6.5 s. This costs ~5 s of runtime.
44. **Backoff timing** is only checked as a lower bound (each gap ≥ 80 % of `delay × WEBHOOK_BACKOFF_SCALE`). Upper
    bounds are not checked, so a polling dispatcher is fine; the poll timeouts are generous (≤ 25 s).
45. **Durability test timing.** The first phase of the `durability` group runs with `WEBHOOK_BACKOFF_SCALE=1.0`. This
    keeps the service from using up its 6 attempts (only 1.55 s at scale 0.05) while it shuts down with the receiver
    unreachable. After the restart the scale is 0.05 again. Whether attempts made before the restart count toward
    the 6 is not checked. The pending delivery must be delivered successfully exactly once.
46. **Webhook validation** only checks the URL scheme (http/https), that `events` is non-empty, and that event names
    are known. A `secret` field in PATCH is not tested. A generated secret only has to be a non-empty string.
47. **GET of a webhook by a plain org member → 403** (the member can see the org, so it is not a 404 case); the same
    for listing and deliveries.
48. **Delivery `last_status_code` after a connection error or timeout** is not checked; only HTTP status values are.

## §1 / §8 Operations

49. **`/readyz` → 503 when the database is unreachable** is *not tested*. It would require stopping or pausing the
    shared `tracker-postgres` container.
50. **SIGTERM finishing in-flight requests (up to 10 s)** is *not tested* (it is hard to make deterministic). Exit code 0
    on SIGTERM is checked at the end of every group and explicitly in the durability test.
51. **The Dockerfile** is not covered by the runner.

## Runtime

The full suite takes about 30 s against an in-memory mock. Slowest parts:
- webhooks group (~14 s, including the 5 s timeout test);
- durability restart (~2 s);
- search dataset setup (~3 s: ~290 issues via bulk plus ~155 mutations).

Every HTTP call has a 30 s timeout. Webhook polls have timeouts of ≤ 25 s. Service start waits ≤ 30 s for `/readyz`,
and stop waits ≤ 15 s after SIGTERM before a SIGKILL. A broken implementation therefore cannot hang the run
indefinitely.
