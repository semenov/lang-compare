# Tracker — an issue tracker backend (Jira-like)

Build the backend of a multi-tenant issue tracker: users, organizations with roles, projects with access control,
issues with a fixed workflow, comments, change history, filtering and full-text search, and webhooks.

How you build it is up to you (frameworks, libraries, schema, architecture). What is fixed is the external
behavior described here, the database it runs against, and how it is started.

## 1. Runtime

- **Database:** PostgreSQL 17, started with `docker compose up -d` from the provided `docker-compose.yml`:
  `postgres://tracker:tracker@127.0.0.1:55432/tracker`. It is the only external dependency.
- **The service** is one executable, started without arguments, configured by environment variables:

| variable | meaning | default |
|---|---|---|
| `PORT` | HTTP port, listen on `0.0.0.0` | `8080` |
| `DATABASE_URL` | PostgreSQL URL | required |
| `JWT_SECRET` | HMAC key for access tokens | required |
| `WEBHOOK_BACKOFF_SCALE` | multiplier for webhook retry delays | `1.0` |

- On startup it creates/migrates its schema itself (idempotent: restarting against an existing database keeps the data).
- `SIGTERM`: stop accepting connections, finish in-flight requests (up to 10 s), exit 0. Pending webhook deliveries are
  not lost: they are sent after the next start.
- A `Dockerfile` that builds a minimal production image of the service.

## 2. API conventions

- Base path `/api/v1`. JSON in and out, UTF-8. IDs are UUID strings. Timestamps are RFC 3339 UTC with millisecond
  precision (`2026-10-05T09:00:00.123Z`); dates are `YYYY-MM-DD`.
- Unknown request fields are ignored. Malformed JSON → `400 bad_request`. Wrong types or invalid values →
  `422 validation_failed`.
- **Errors** are `application/problem+json`:
  `{"status": 422, "code": "validation_failed", "detail": "...", "errors": [{"field": "title", "code": "too_long"}]}`.
  `code` is a machine code named in this document. `errors` is present for `validation_failed`, one entry per invalid
  field (`field` may be a dotted path such as `issues.3.title`; codes: `required`, `too_short`, `too_long`, `invalid`,
  `out_of_range`). General codes: `bad_request` 400, `unauthenticated` 401, `token_expired` 401, `forbidden` 403,
  `not_found` 404, `conflict` 409, `version_mismatch` 412, `validation_failed` 422, `precondition_required` 428.
- **Visibility:** anything the caller may not see is `404 not_found` (never `403`). `403 forbidden` = the caller can see
  it but may not do this.
- **Pagination** for lists: `?limit=` 1..100 (default 50, out of range → `422`), `?cursor=` (opaque, from the previous
  page). Response `{"items": [...], "next_cursor": string|null}`. Walking all pages of an unchanged list returns every
  item exactly once.
- Everything except `/auth/*`, `/healthz` and `/readyz` requires `Authorization: Bearer <access token>`.

## 3. Users and authentication

User: `{"id", "email", "name", "created_at"}`.

- `POST /auth/register` `{"email", "password", "name"}` → `201` user. Email: contains `@`, ≤ 254 chars, stored and
  compared lower-cased, unique → `409 email_taken`. Password 10..128 chars. Name 1..100 chars (trimmed).
- Passwords are hashed with **Argon2id, m = 19456 KiB, t = 2, p = 1**, random 16-byte salt.
- `POST /auth/login` `{"email", "password"}` → `200` `{"access_token", "refresh_token", "token_type": "Bearer",
  "expires_in": 900}`. Wrong email or password → `401 invalid_credentials`.
- **Access token:** JWT, HS256 with `JWT_SECRET`, claims `sub` (user id), `iat`, `exp` (= iat + 900), `typ: "access"`.
  Expired → `401 token_expired`; otherwise invalid or missing → `401 unauthenticated`.
- **Refresh tokens:** opaque, valid 30 days, single use. `POST /auth/refresh` `{"refresh_token"}` → `200` new pair, the
  used token becomes invalid. Presenting an already-used refresh token → `401 token_reused` **and** revokes every
  refresh token of that login's rotation chain. Unknown, expired or revoked → `401 invalid_token`.
- `POST /auth/logout` `{"refresh_token"}` → `204`, revokes that rotation chain.
- `GET /me` → user. `PATCH /me` `{"name"}` → user.

## 4. Organizations, projects, access

### 4.1 Organizations

Org: `{"id", "slug", "name", "created_at", "my_role"}`. Org roles: `owner` > `admin` > `member`.

- `POST /orgs` `{"name" (1..100), "slug"}` → `201`, the caller becomes `owner`. Slug `^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`,
  unique → `409 slug_taken`.
- `GET /orgs` → orgs the caller belongs to (paginated, by slug). `GET /orgs/{slug}`.
- `GET /orgs/{slug}/members` → `{"user_id", "email", "name", "role"}` (paginated, by email). Any member.
- `POST /orgs/{slug}/members` `{"email", "role"}` (admin+) → `201` member: adds an existing user by email
  (unknown email → `422`, field `email`, code `invalid`; already a member → `409 already_member`).
- `PATCH /orgs/{slug}/members/{user_id}` `{"role"}` (admin+). Only owners may grant or take away `owner`/`admin`
  (else `403`). Removing or demoting the last owner → `409 last_owner`.
- `DELETE /orgs/{slug}/members/{user_id}` (admin+, or a user removing themselves) → `204`. Also removes their project
  memberships and unassigns their issues in the org.

### 4.2 Projects

Project: `{"id", "key", "name", "description": string|null, "visibility": "org"|"private", "created_at", "my_role"}`.
Project roles: `admin` > `developer` > `viewer`.

A user's **effective project role** is the strongest of: their explicit project membership; `admin` if they are org
`owner`/`admin`; `developer` if they are an org `member` and the project's visibility is `org`. No effective role →
the project and everything in it is invisible (`404`).

| action | min project role |
|---|---|
| view the project, its issues, comments, history | viewer |
| comment | viewer |
| create, edit, transition issues | developer |
| delete issues, edit or delete the project, manage project members | admin |

- `POST /orgs/{slug}/projects` `{"key", "name" (1..100), "description"?, "visibility"?: "org"}` (any org member) → `201`;
  the creator becomes an explicit project `admin`. Key `^[A-Z][A-Z0-9]{1,9}$`, unique per org → `409 key_taken`.
- `GET /orgs/{slug}/projects` (visible ones, paginated by key), `GET /orgs/{slug}/projects/{key}`,
  `PATCH /orgs/{slug}/projects/{key}` `{"name"?, "description"?, "visibility"?}`, `DELETE` → `204` (deletes its issues).
- `GET /orgs/{slug}/projects/{key}/members` → `{"user_id", "email", "name", "role"}` explicit members (paginated, by email).
  `PUT /orgs/{slug}/projects/{key}/members/{user_id}` `{"role"}` (the user must be an org member, else `422`) → `200`.
  `DELETE .../members/{user_id}` → `204`.

## 5. Issues

### 5.1 Representation and rules

```json
{"id", "key": "PROJ-12", "number": 12, "project_key": "PROJ",
 "type": "task"|"bug"|"story", "title", "description": string|null,
 "status": "todo"|"in_progress"|"done",
 "priority": "highest"|"high"|"medium"|"low"|"lowest",
 "assignee_id": uuid|null, "reporter_id": uuid, "labels": [string], "due_date": "YYYY-MM-DD"|null,
 "version": int, "comment_count": int, "created_at", "updated_at", "resolved_at": string|null}
```

- `title`: trimmed, 1..255 chars. `description`: ≤ 65 536 chars. `priority` default `medium`.
- `labels`: lower-cased, each `^[a-z0-9][a-z0-9_.-]{0,49}$`, duplicates removed, at most 20, returned sorted.
- `assignee_id`: must be a user with effective role ≥ `developer` in the project → else `422` (`assignee_id`, `invalid`).
- `version` starts at 1 and grows by exactly 1 with every change to the issue's own fields (comments don't change it,
  they change `comment_count`). `updated_at` changes together with `version`.
- **Workflow (fixed):** new issues are `todo`. Allowed transitions: `todo → in_progress`, `in_progress → todo`,
  `in_progress → done`, `done → in_progress`. Entering `done` sets `resolved_at`; leaving it clears it.

### 5.2 Endpoints

- `POST /orgs/{slug}/projects/{key}/issues` `{"type", "title", "description"?, "priority"?, "assignee_id"?, "labels"?,
  "due_date"?}` → `201` issue.
  - Issue numbers are per project, start at 1 and grow by 1, with no gaps or duplicates, also under concurrent
    creation. Numbers of deleted issues are not reused.
  - **Idempotency:** with an `Idempotency-Key` header (1..255 chars), repeating the request with the same key by the same
    user within 24 h returns the original status and body and creates nothing; the same key with a different body →
    `422 idempotency_key_reused`. Concurrent requests with the same key create exactly one issue.
- `POST /orgs/{slug}/projects/{key}/issues/bulk` `{"issues": [<create body>, ...]}` (1..1000) → `201` `{"keys": [...]}`
  in request order. All-or-nothing: any invalid item → `422` with fields like `issues.17.title`, nothing created.
- `GET /orgs/{slug}/issues/{issue_key}` → issue with header `ETag: "<version>"`. Keys are case-insensitive in paths.
- `PATCH /orgs/{slug}/issues/{issue_key}` with any of `title`, `description`, `type`, `priority`, `assignee_id`, `labels`,
  `due_date` (absent = unchanged, `null` = clear where nullable; `status` here → `422`). **Requires**
  `If-Match: "<version>"`: missing → `428 precondition_required`, stale → `412 version_mismatch`. A patch that changes
  nothing returns `200` without bumping `version`.
- `POST /orgs/{slug}/issues/{issue_key}/transition` `{"status"}` → `200` issue; a transition not in the workflow →
  `409 transition_not_allowed`. Honors `If-Match` if present.
- `DELETE /orgs/{slug}/issues/{issue_key}` (project admin) → `204`. The issue is gone for good.

### 5.3 Comments

`{"id", "issue_key", "author_id", "body", "created_at", "updated_at", "edited": bool}`.

- `POST /orgs/{slug}/issues/{issue_key}/comments` `{"body"}` (1..20 000 chars, viewer+) → `201`.
- `GET /orgs/{slug}/issues/{issue_key}/comments` → oldest first, paginated.
- `PATCH /orgs/{slug}/comments/{id}` `{"body"}` (author only) → `200`, `edited: true`.
  `DELETE /orgs/{slug}/comments/{id}` (author or project admin) → `204`.

### 5.4 History

`GET /orgs/{slug}/issues/{issue_key}/history` → newest first, paginated:
`{"id", "actor_id", "created_at", "changes": [{"field", "from", "to"}]}`. The oldest entry is the creation:
`[{"field": "created", "from": null, "to": null}]`. Every update or transition adds one entry with one change per changed
field (`title`, `description`, `type`, `status`, `priority`, `assignee_id`, `labels` as arrays, `due_date`).

## 6. Listing and search

`GET /orgs/{slug}/issues?<filters>&sort=&limit=&cursor=` → `{"total": <exact number of matches>, "items": [...],
"next_cursor"}`. Only issues in projects the caller can view.

| filter | meaning |
|---|---|
| `project=KEY1,KEY2` | in any of these projects |
| `status=todo,in_progress` | any of these statuses |
| `priority=high,highest` | any of these priorities |
| `type=bug` | any of these types |
| `assignee=<user id>,me,none` | assigned to any of these (`me` = the caller, `none` = unassigned) |
| `reporter=<user id>,me` | reported by any of these |
| `label=a,b` | has at least one of these labels |
| `created_after=`, `created_before=`, `updated_after=` | RFC 3339 timestamp; `after` is `>`, `before` is `<` |
| `q=<text>` | every word of the text appears as a whole word in the title or description (case-insensitive; words are maximal runs of Unicode letters and digits) |

Different filters are combined with AND. An invalid filter value → `422` with the filter name as `field`.

`sort` is one of `created`, `updated`, `priority`, `key`, each optionally prefixed with `-` for descending (default
`-created`). Priority sorts `lowest` < `low` < `medium` < `high` < `highest` (so `-priority` puts `highest` first).
`key` sorts by project key, then number. Ties are always broken by `key` ascending.

## 7. Webhooks

Webhook: `{"id", "url", "events": [...], "active": bool, "created_at"}`. Org admin+ only.

- `POST /orgs/{slug}/webhooks` `{"url" (http/https), "events": [...] (at least one), "secret"?}` → `201` webhook plus
  `"secret"` (generated if not given; returned only here). `GET /orgs/{slug}/webhooks`,
  `GET/PATCH/DELETE /orgs/{slug}/webhooks/{id}` (`PATCH`: `url`, `events`, `active`).
- Events: `issue.created`, `issue.updated` (also transitions), `issue.deleted`, `comment.created`.
- Delivery: `POST <url>`, headers `Content-Type: application/json`, `X-Tracker-Event`, `X-Tracker-Delivery` (delivery
  id, the same for every attempt), `X-Tracker-Timestamp` (unix seconds of this attempt),
  `X-Tracker-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + raw body)>`. Body:
  `{"id": <delivery id>, "event", "created_at", "org": <slug>, "actor_id", "data": {...}}` with `data` =
  `{"issue": <issue>, "changes": [...]}` for `issue.updated` (`changes` as in the history), `{"issue": <issue>}` for
  `issue.created` and `issue.deleted` (the issue as it was), `{"issue": <issue>, "comment": <comment>}` for `comment.created`.
- A 2xx answer within 5 s is success. Otherwise retry after 1, 2, 4, 8, 16 s (× `WEBHOOK_BACKOFF_SCALE`): at most
  6 attempts, then the delivery is `failed`.
- **Ordering:** deliveries of one webhook about the same issue are sent in event order; the next one is not attempted
  until the previous one succeeded or failed for good. Different issues don't wait for each other.
- **Durability:** an event is recorded atomically with the change that caused it (no event without the change, no change
  without the event) and is delivered even if the service is restarted in between.
- `GET /orgs/{slug}/webhooks/{id}/deliveries` → newest first, paginated: `{"id", "event", "status":
  "pending"|"succeeded"|"failed", "attempts", "last_status_code": int|null, "created_at"}`.

## 8. Operations

- `GET /healthz` → `200 {"status": "ok"}`. `GET /readyz` → `200 {"status": "ok"}` when the database is reachable, else `503`.

## 9. Acceptance

With the database running (`docker compose up -d`; the container is named `tracker-postgres`),
`python3 run_tests.py <path-to-binary> [--db <database name>]` must report 0 failed. It creates the database if needed
(default name `tracker`), starts and stops the service itself with `DATABASE_URL` pointing at that database, resets the
database between test groups (drops and recreates the `public` schema) and receives webhooks on a local port.

## 10. Performance

**We want an implementation that is as fast as possible and uses as little memory as possible.** It will be
benchmarked in Docker with CPU and memory limits, using its own image, on a dataset of about 1M issues in a realistic
traffic mix: listing and searching issues, opening issues with comments and history, creating and updating issues,
transitions, comments, logins. We will measure throughput, latency percentiles, CPU and memory under load, memory at
idle, startup time and image size.
