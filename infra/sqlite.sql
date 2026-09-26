-- SQLite schema + seed, same data as infra/schema.sql. Run by the *-sqlite apps on first start.
CREATE TABLE users (
  id         INTEGER PRIMARY KEY,
  email      TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE orders (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id),
  status      TEXT NOT NULL DEFAULT 'created',
  total_cents INTEGER NOT NULL,
  currency    TEXT NOT NULL,
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX orders_user_id_id_idx ON orders (user_id, id DESC);

CREATE TABLE order_items (
  id               INTEGER PRIMARY KEY,
  order_id         INTEGER NOT NULL REFERENCES orders(id),
  sku              TEXT NOT NULL,
  name             TEXT NOT NULL,
  qty              INTEGER NOT NULL,
  unit_price_cents INTEGER NOT NULL
);
CREATE INDEX order_items_order_id_idx ON order_items (order_id);

-- seed: must match catalog pricing: price_cents = 100 + (n * 37) % 9900
WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM g WHERE n < 10000)
INSERT INTO users (email, name) SELECT 'user' || n || '@example.com', 'User ' || n FROM g;

WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM g WHERE n < 100000)
INSERT INTO orders (user_id, total_cents, currency) SELECT 1 + (n % 10000), 0, 'USD' FROM g;

WITH RECURSIVE k(k) AS (SELECT 1 UNION ALL SELECT k + 1 FROM k WHERE k < 3)
INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents)
SELECT id, 'SKU-' || n, 'Product ' || n, 1 + (id % 5), 100 + (n * 37) % 9900
FROM (SELECT o.id, 1 + ((o.id * 7919 + k.k * 104729) % 10000) AS n
      FROM orders o JOIN k ON k.k <= 1 + (o.id % 3)
      ORDER BY o.id, k.k);

UPDATE orders SET total_cents =
  (SELECT SUM(qty * unit_price_cents) FROM order_items WHERE order_id = orders.id);

ANALYZE;
