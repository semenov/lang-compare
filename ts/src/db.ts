import pg from "pg";
import { config } from "./config.js";

// BIGINT -> number (ids and cents fit comfortably in 2^53)
pg.types.setTypeParser(pg.types.builtins.INT8, (v) => Number(v));

export const db = new pg.Pool({ connectionString: config.databaseUrl, max: config.dbPoolSize });

export interface User {
  id: number;
  email: string;
  name: string;
  created_at: Date;
}

export interface OrderItem {
  sku: string;
  name: string;
  qty: number;
  unit_price_cents: number;
}

export interface Order {
  id: number;
  user_id: number;
  status: string;
  total_cents: number;
  currency: string;
  created_at: Date;
  items?: OrderItem[];
}

const ORDER_COLS = "id, user_id, status, total_cents, currency, created_at";

export async function insertUser(email: string, name: string): Promise<User | null> {
  const r = await db.query<User>(
    "INSERT INTO users (email, name) VALUES ($1, $2) ON CONFLICT (email) DO NOTHING RETURNING id, email, name, created_at",
    [email, name],
  );
  return r.rows[0] ?? null;
}

export async function getUser(id: number): Promise<User | null> {
  const r = await db.query<User>({
    name: "get_user",
    text: "SELECT id, email, name, created_at FROM users WHERE id = $1",
    values: [id],
  });
  return r.rows[0] ?? null;
}

export async function userExists(id: number): Promise<boolean> {
  const r = await db.query({ name: "user_exists", text: "SELECT 1 FROM users WHERE id = $1", values: [id] });
  return r.rowCount === 1;
}

export async function createOrder(userId: number, currency: string, total: number, items: OrderItem[]): Promise<Order> {
  const client = await db.connect();
  try {
    await client.query("BEGIN");
    const o = await client.query<Order>({
      name: "insert_order",
      text: `INSERT INTO orders (user_id, total_cents, currency) VALUES ($1, $2, $3) RETURNING ${ORDER_COLS}`,
      values: [userId, total, currency],
    });
    const order = o.rows[0];
    await client.query({
      name: "insert_items",
      text: `INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents)
             SELECT $1, * FROM unnest($2::text[], $3::text[], $4::int[], $5::bigint[])`,
      values: [
        order.id,
        items.map((i) => i.sku),
        items.map((i) => i.name),
        items.map((i) => i.qty),
        items.map((i) => i.unit_price_cents),
      ],
    });
    await client.query("COMMIT");
    order.items = items;
    return order;
  } catch (e) {
    await client.query("ROLLBACK").catch(() => {});
    throw e;
  } finally {
    client.release();
  }
}

export async function getOrder(id: number): Promise<Order | null> {
  const o = await db.query<Order>({
    name: "get_order",
    text: `SELECT ${ORDER_COLS} FROM orders WHERE id = $1`,
    values: [id],
  });
  if (o.rowCount === 0) return null;
  const items = await db.query<OrderItem>({
    name: "get_order_items",
    text: "SELECT sku, name, qty, unit_price_cents FROM order_items WHERE order_id = $1 ORDER BY id",
    values: [id],
  });
  return { ...o.rows[0], items: items.rows };
}

export async function listOrders(userId: number, limit: number, offset: number): Promise<Order[]> {
  const r = await db.query<Order>({
    name: "list_orders",
    text: `SELECT ${ORDER_COLS} FROM orders WHERE user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3`,
    values: [userId, limit, offset],
  });
  return r.rows;
}
