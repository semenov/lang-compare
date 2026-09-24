# orders-api — reference backend spec

A small but typical JSON backend: users, orders, Postgres, and an upstream
"catalog" HTTP service that is both proxied and used for business logic.
All three implementations (TS, Go, Rust) must behave identically.

## Environment

| var            | default                                          |
|----------------|--------------------------------------------------|
| `PORT`         | `8080`                                           |
| `DATABASE_URL` | `postgres://app:app@postgres:5432/app`           |
| `CATALOG_URL`  | `http://catalog:9000`                            |
| `DB_POOL_SIZE` | `20`                                             |

The schema is created by `infra/schema.sql` (not by the app).

## Upstream: catalog service (`catalog/`)

`GET /products/{sku}` → `200 {"sku","name","price_cents","currency","stock"}`
or `404 {"error":"not found"}`. Valid SKUs are `SKU-1` … `SKU-10000`. The price is derived
deterministically from the number. Optional `LATENCY_MS` env adds artificial delay.

## Endpoints

All responses are `application/json`. Errors are `{"error": "<message>"}`.

### `GET /health`
`200 {"status":"ok"}` — no I/O.

### `POST /users`
Body `{"email": string, "name": string}`.
- `email` must contain `@` and be 3..254 chars; `name` must be 1..100 chars. Otherwise `400`.
- Duplicate email → `409 {"error":"email already exists"}`.
- `201` → user object: `{"id": int, "email": str, "name": str, "created_at": RFC3339 str}`.

### `GET /users/{id}`
`200` user object, `404` if missing, `400` if id is not a positive integer.

### `GET /products/{sku}` — transparent proxy
Forwards to `CATALOG_URL/products/{sku}` and returns the upstream status code and
body unchanged (content-type `application/json`). If upstream is unreachable → `502`.

### `POST /orders`
Body `{"user_id": int, "items": [{"sku": string, "qty": int}, ...]}`.
1. Validate: `user_id > 0`, `1..=20` items, `qty` in `1..=1000`, `sku` non-empty. Else `400`.
2. User must exist → else `404 {"error":"user not found"}`.
3. Fetch every item's product from the catalog **concurrently**.
   Any 404 → `422 {"error":"unknown sku: <sku>"}`. Other upstream failure → `502`.
   `qty > stock` → `422 {"error":"insufficient stock: <sku>"}`.
4. `total_cents = Σ price_cents * qty`. Currency is taken from the products (all are USD).
5. Insert into `orders` and `order_items` in **one transaction**.
6. `201` → order object:
```json
{"id": 1, "user_id": 1, "status": "created", "total_cents": 1234, "currency": "USD",
 "created_at": "…", "items": [{"sku": "SKU-1", "name": "…", "qty": 2, "unit_price_cents": 617}]}
```

### `GET /orders/{id}`
`200` order object with items (item order = insertion order), `404` if missing.

### `GET /users/{id}/orders?limit=20&offset=0`
`limit` 1..100 (default 20), `offset` ≥ 0 (default 0); invalid → `400`.
Orders of the user sorted by `id DESC`, **without** items:
`200 {"orders": [{"id","user_id","status","total_cents","currency","created_at"}], "limit": n, "offset": n}`.
Unknown user → `404`.

## Seed data
10 000 users (`user1@example.com`…), 100 000 orders with 1–3 items each.
