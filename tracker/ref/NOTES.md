# Reference implementation — interpretation notes

Where SPEC.md was ambiguous or silent, this is what the reference does.

## §2 API conventions
1. **Body that is valid JSON but not an object** (e.g. `[1]`, `"x"`), or an empty body where one is required → `400 bad_request`, not 422.
2. **`null` for a required field** → `required`. `null` for an optional non-nullable field (`priority`, `type` on PATCH, `visibility`, `labels`, `title` on PATCH, webhook `active`) → `invalid`.
3. **Validation error codes:** wrong JSON type, bad enum, regex mismatch, bad UUID or bad date → `invalid`. String shorter or longer than allowed → `too_short` / `too_long` (an empty or whitespace-only trimmed name or title → `too_short`). Arrays use the same codes: an empty `issues` or `events` → `too_short`; more than 1000 bulk items or more than 20 distinct labels → `too_long`. Only `limit` uses `out_of_range`. The `labels` field gets one `labels`/`invalid` entry, not one entry per bad element.
4. **Order of checks:** 401 → 404 (visibility) → 403 (role) → 400 (malformed body) → 428/412 (preconditions) → 422 (validation) → 409 (conflicts). So a PATCH with no `If-Match` gets 428 even if the body is invalid, and a stale `If-Match` gets 412 before the body is validated.
5. **Unknown route, or known path with an unsupported method** → `404 not_found`. The spec names no 405 code.
6. **Pagination cursors:** issue search uses a keyset cursor (sort value + project key + number). The other lists use an opaque offset cursor, which is enough for "unchanged list returns every item exactly once". A malformed cursor, or an issue cursor from a different `sort`, → `422`, field `cursor`, code `invalid`. `limit` that is not an integer → `invalid`; an integer outside 1..100 → `out_of_range`.
7. **`/healthz` and `/readyz`** are served both at the root and under `/api/v1`. Neither needs auth. When not ready, `/readyz` returns `503 {"status":"unavailable"}`.
8. **Path case:** issue keys are case-insensitive, as the spec says. Project keys in `/projects/{key}` and org slugs in paths are also matched case-insensitively (upper-cased or lower-cased respectively).
9. **Timestamps** are stored truncated to milliseconds. That way a returned `created_at` used as `created_after` excludes the item itself.

## §3 Auth
10. **Email:** trimmed and lower-cased. The only format check is "contains `@`". Login, and adding an org member by email, use the same normalization.
11. **Password length** counts Unicode code points (10..128). The password is not trimmed.
12. **Login with missing or wrong-typed fields** → `422`, not `401`.
13. **Refresh state precedence:** unknown → `invalid_token`. Already used → `token_reused`, which revokes the whole chain; this applies even if the chain is already revoked, so presenting a used token always yields `token_reused`. Revoked or expired (but unused) → `invalid_token`. The chain revocation is committed even though the response is an error.
14. **Logout** needs no access token (it is under `/auth/*`). Unknown token → `401 invalid_token`. A known token in any state → `204`, and its chain is revoked.
15. **Access tokens:** the JWT header must say `alg: HS256`. The `typ` claim must be `"access"` and `sub` must be an existing user, otherwise `401 unauthenticated`. Expiry is checked only after the signature is verified, so `token_expired` is only ever returned for genuine tokens.
16. **`PATCH /me` without `name`** → `200`, nothing changes.

## §4 Orgs and projects
17. **Org and project names** are trimmed, like the user's name. Project `description` has no length limit and is not trimmed.
18. **Granting owner/admin on add:** the "only owners may grant or take away owner/admin" rule also applies to `POST /members`. An admin adding someone with role `owner` or `admin` → `403`.
19. **Removal by an admin:** an admin (not owner) removing another admin or an owner → `403`, because that takes away owner/admin. Anyone may remove themselves. The last owner removing themselves → `409 last_owner`.
20. **PATCH member to the same role** by an admin → allowed (no-op), even if the target is an admin.
21. **Unassigning on member removal** is a normal issue change: `version` +1, `updated_at` changes, a history entry is recorded with the remover as actor, and an `issue.updated` webhook is sent. Removing someone's explicit project membership (or their org removal reducing their role) does not unassign issues anywhere else.
22. **Project member endpoints:** listing needs viewer. `PUT` and `DELETE` need project admin. A `user_id` that is not a UUID or not an org member → `422` (`user_id`, `invalid`). `DELETE` of a non-member → `404`. Removing the last project admin is allowed.
23. **Deleting a project** does not emit `issue.deleted` events for its issues. Deliveries already queued for those issues are still delivered.
24. **`GET /orgs/{slug}/webhooks`** is ordered by creation, oldest first. The spec gives no order.

## §5 Issues
25. **`description`** is not trimmed, and an empty string is stored as `""`. **Comment `body`** is not trimmed either: the length must be 1..20000, so a whitespace-only body is accepted.
26. **Labels:** each must be a string. It is lower-cased, then matched against the regex. The 20 limit is checked after duplicates are removed.
27. **`assignee_id`** must be in canonical 36-character UUID form.
28. **Idempotency:**
    - The scope is (user, key). The fingerprint is method + path + canonical JSON of the body, so reusing a key on a different path or project counts as a "different body" → `422 idempotency_key_reused`.
    - Only successful (2xx) responses are stored. A request that failed (e.g. 422) leaves no record, so it can be retried with the same key.
    - An invalid header length (empty or more than 255) → `422`, field `Idempotency-Key`.
    - Bulk create honors `Idempotency-Key` the same way.
    - The key is checked after auth, visibility and role.
29. **`If-Match`** accepts `"N"`, `N`, `W/"N"`, a comma-separated list, or `*`. Anything unparseable does not match → `412`.
30. **PATCH containing `status`** → `422` (`status`, `invalid`) whenever the key is present, even if the value equals the current status.
31. **Transition to the current status** (e.g. todo → todo) → `409 transition_not_allowed`.
32. **ETag** is returned on every single-issue response (GET, create, PATCH, transition).
33. **History change order** follows the field order in the spec: title, description, type, status, priority, assignee_id, labels, due_date. `resolved_at` is never listed as a change.
34. **Comment edits** do not touch the issue's `updated_at`, `version`, or history. Comment edits and deletes emit no webhooks (there are no event types for them). Editing to the same body still sets `edited: true`.

## §6 Listing and search
35. **Filter values:** comma-separated lists; an empty element (`status=` or `status=a,,b`) → `422`. `project` keys and `label` values are matched case-insensitively. A key or label with an invalid format → `422`. Unknown or invisible project keys simply match nothing.
36. **`assignee=` / `reporter=`** values must be a UUID, `me`, or (assignee only) `none`.
37. **Timestamps** must be RFC 3339 with a timezone offset. A space is accepted in place of `+`, because an unencoded `+` decodes to a space in query strings.
38. **`q` word definition:** a "word" is a maximal run of characters in Unicode categories L* (letters) or Nd (decimal digits). Marks (Mn/Mc), `_` and other numerics split words. Comparison is case-insensitive via `str.casefold()` on both sides, so e.g. `ß` matches `SS`. Words may come from the title and description together (some in each is fine). A `q` with no words in it (empty or punctuation only) does not filter at all.
39. **`sort=`** empty or absent → `-created`. Ties are broken by key ascending (project key, then number) for `created`, `updated` and `priority` in both directions. `-key` is fully descending.
40. **Key ordering** is byte (C collation) order of the project key, then numeric order of the number. Emails (member lists) and slugs (org list) also use C collation.

## §7 Webhooks
41. **Inactive webhooks** get no new deliveries. Their already-queued pending deliveries are paused (not attempted, not failed) until the webhook is reactivated. Deleting a webhook deletes its deliveries.
42. **URL and secret at send time:** each attempt uses the webhook's current URL and secret.
43. **`secret`:** if given, it must be a non-empty string. `null` or absent → a 64-hex-character secret is generated. `events` are de-duplicated with order preserved; unknown event names → `invalid`.
44. **Delivery `created_at`** (both in the payload and in the delivery list) is the event time. The body is frozen at event time, so retries send byte-identical bodies; only the timestamp and signature headers change.
45. **`last_status_code`** is `null` when the last attempt got no HTTP response (connection error, or no complete response within 5 s). A response that arrives after more than 5 s counts as a failure with `last_status_code = null`.
46. **Retry delays:** after attempt n fails, the next is scheduled `[1,2,4,8,16][n-1] × WEBHOOK_BACKOFF_SCALE` seconds after attempt n finished. After the 6th failed attempt the delivery is `failed`. A failed delivery unblocks the next one in its (webhook, issue) chain.
47. **Ordering key:** the issue id. `comment.created` events are ordered together with that issue's `issue.*` events.
48. **Delivery semantics are at-least-once:** an attempt interrupted by a crash (not SIGTERM) is retried after restart. Each attempt is claimed with a 15 s lease in the database, so even several instances sharing one database don't double-send.

## §1 Runtime
49. **SIGTERM:** the listening socket is closed and idle keep-alive connections are shut. In-flight requests get up to 10 s to finish (responses carry `Connection: close`). The webhook worker stops taking new attempts and lets running ones finish within the remaining budget. Then exit 0. Pending deliveries stay in the database and are sent after the next start.
50. **Database reset while running:** if the schema disappears under a running service (e.g. `DROP SCHEMA public`), the next request that hits "undefined table" re-runs the idempotent migration and retries once.
