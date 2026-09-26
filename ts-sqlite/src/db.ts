import { existsSync, readFileSync } from "node:fs";
import Database from "better-sqlite3";
import { config } from "./config.js";

function open(): Database.Database {
  const db = new Database(config.sqlitePath);
  db.pragma("journal_mode = WAL");
  db.pragma("synchronous = NORMAL");
  db.pragma("busy_timeout = 5000");
  return db;
}

/** Creates and seeds the database file if it doesn't exist yet. Run once, before forking workers. */
export function initDb(): void {
  const fresh = !existsSync(config.sqlitePath);
  const db = open();
  if (fresh) db.exec(readFileSync(config.sqliteInit, "utf8"));
  db.close();
}

// better-sqlite3 is synchronous: one connection per process, queries run on the event loop thread.
let db: Database.Database;
let stmts: ReturnType<typeof prepare>;

const ORDER_COLS = "id, user_id, status, total_cents, currency, created_at";

function prepare(db: Database.Database) {
  return {
    insertUser: db.prepare("INSERT INTO users (email, name) VALUES (?, ?) RETURNING id, email, name, created_at"),
    getUser: db.prepare("SELECT id, email, name, created_at FROM users WHERE id = ?"),
    userExists: db.prepare("SELECT 1 FROM users WHERE id = ?").pluck(),
    insertOrder: db.prepare(
      `INSERT INTO orders (user_id, total_cents, currency) VALUES (?, ?, ?) RETURNING ${ORDER_COLS}`,
    ),
    insertItem: db.prepare(
      "INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents) VALUES (?, ?, ?, ?, ?)",
    ),
    getOrder: db.prepare(`SELECT ${ORDER_COLS} FROM orders WHERE id = ?`),
    getItems: db.prepare("SELECT sku, name, qty, unit_price_cents FROM order_items WHERE order_id = ? ORDER BY id"),
    listOrders: db.prepare(`SELECT ${ORDER_COLS} FROM orders WHERE user_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`),
  };
}

export function connect(): void {
  db = open();
  stmts = prepare(db);
}

export interface User {
  id: number;
  email: string;
  name: string;
  created_at: string;
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
  created_at: string;
  items?: OrderItem[];
}

export function insertUser(email: string, name: string): User | null {
  try {
    return stmts.insertUser.get(email, name) as User;
  } catch (e) {
    if ((e as { code?: string }).code === "SQLITE_CONSTRAINT_UNIQUE") return null;
    throw e;
  }
}

export function getUser(id: number): User | null {
  return (stmts.getUser.get(id) as User | undefined) ?? null;
}

export function userExists(id: number): boolean {
  return stmts.userExists.get(id) !== undefined;
}

export function createOrder(userId: number, currency: string, total: number, items: OrderItem[]): Order {
  // IMMEDIATE takes the write lock up front, so concurrent writers (other cluster workers) wait on busy_timeout
  // instead of failing on a lock upgrade.
  const tx = db.transaction(() => {
    const order = stmts.insertOrder.get(userId, total, currency) as Order;
    for (const i of items) stmts.insertItem.run(order.id, i.sku, i.name, i.qty, i.unit_price_cents);
    return { ...order, items };
  });
  return tx.immediate();
}

export function getOrder(id: number): Order | null {
  const order = stmts.getOrder.get(id) as Order | undefined;
  if (!order) return null;
  return { ...order, items: stmts.getItems.all(id) as OrderItem[] };
}

export function listOrders(userId: number, limit: number, offset: number): Order[] {
  return stmts.listOrders.all(userId, limit, offset) as Order[];
}
