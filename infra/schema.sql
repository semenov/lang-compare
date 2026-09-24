CREATE TABLE users (
  id         BIGSERIAL PRIMARY KEY,
  email      TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE orders (
  id          BIGSERIAL PRIMARY KEY,
  user_id     BIGINT NOT NULL REFERENCES users(id),
  status      TEXT NOT NULL DEFAULT 'created',
  total_cents BIGINT NOT NULL,
  currency    TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX orders_user_id_id_idx ON orders (user_id, id DESC);

CREATE TABLE order_items (
  id               BIGSERIAL PRIMARY KEY,
  order_id         BIGINT NOT NULL REFERENCES orders(id),
  sku              TEXT NOT NULL,
  name             TEXT NOT NULL,
  qty              INT NOT NULL,
  unit_price_cents BIGINT NOT NULL
);
CREATE INDEX order_items_order_id_idx ON order_items (order_id);

-- seed: must match catalog pricing: price_cents = 100 + (n * 37) % 9900
INSERT INTO users (email, name)
SELECT 'user' || g || '@example.com', 'User ' || g FROM generate_series(1, 10000) g;

INSERT INTO orders (user_id, total_cents, currency)
SELECT 1 + (g % 10000), 0, 'USD' FROM generate_series(1, 100000) g;

INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents)
SELECT o.id, 'SKU-' || s.n, 'Product ' || s.n, 1 + (o.id % 5), 100 + (s.n * 37) % 9900
FROM orders o
CROSS JOIN LATERAL (
  SELECT 1 + ((o.id * 7919 + k * 104729) % 10000) AS n FROM generate_series(1, 1 + (o.id % 3)::int) k
) s;

UPDATE orders o SET total_cents = t.sum
FROM (SELECT order_id, SUM(qty * unit_price_cents) AS sum FROM order_items GROUP BY order_id) t
WHERE t.order_id = o.id;

ANALYZE;
