import crypto from "node:crypto";
import http from "node:http";
import https from "node:https";
import type pg from "pg";
import { pool } from "./db.js";
import {
  type Ctx,
  type Result,
  Validator,
  iso,
  notFound,
  page,
  pageParams,
  UUID_RE,
} from "./http.js";
import { loadOrg, requireOrgAdmin } from "./access.js";

export const EVENTS = ["issue.created", "issue.updated", "issue.deleted", "comment.created"] as const;

// ---------------------------------------------------------------------------------------------------------------
// event recording (inside the caller's transaction)

export interface OrgRef {
  id: string;
  slug: string;
}

/** Records one delivery per subscribed active webhook. Returns the number of deliveries created. */
export async function emit(
  cl: pg.PoolClient,
  org: OrgRef,
  event: string,
  actorId: string,
  groupKey: string,
  data: unknown,
): Promise<number> {
  const hooks = await cl.query("SELECT id FROM webhooks WHERE org_id = $1 AND active AND $2 = ANY(events)", [
    org.id,
    event,
  ]);
  if (!hooks.rowCount) return 0;
  const now = new Date();
  for (const h of hooks.rows) {
    const id = crypto.randomUUID();
    const payload = JSON.stringify({ id, event, created_at: now.toISOString(), org: org.slug, actor_id: actorId, data });
    await cl.query(
      `INSERT INTO deliveries (id, webhook_id, group_key, event, payload, status, next_attempt_at, created_at)
       VALUES ($1,$2,$3,$4,$5,'pending',$6,$6)`,
      [id, h.id, groupKey, event, payload, now],
    );
  }
  pendingKick = true;
  return hooks.rowCount;
}

/** Batch variant for many events of the same type (bulk create). */
export async function emitMany(
  cl: pg.PoolClient,
  org: OrgRef,
  event: string,
  actorId: string,
  items: { groupKey: string; data: unknown }[],
): Promise<void> {
  const hooks = await cl.query("SELECT id FROM webhooks WHERE org_id = $1 AND active AND $2 = ANY(events)", [
    org.id,
    event,
  ]);
  if (!hooks.rowCount || !items.length) return;
  const now = new Date();
  const ids: string[] = [], whs: string[] = [], groups: string[] = [], payloads: string[] = [];
  for (const it of items) {
    for (const h of hooks.rows) {
      const id = crypto.randomUUID();
      ids.push(id);
      whs.push(h.id);
      groups.push(it.groupKey);
      payloads.push(
        JSON.stringify({ id, event, created_at: now.toISOString(), org: org.slug, actor_id: actorId, data: it.data }),
      );
    }
  }
  await cl.query(
    `INSERT INTO deliveries (id, webhook_id, group_key, event, payload, status, next_attempt_at, created_at)
     SELECT a.id, a.wh, a.g, $5, a.p, 'pending', $6, $6
       FROM unnest($1::uuid[], $2::uuid[], $3::text[], $4::text[]) WITH ORDINALITY AS a(id, wh, g, p, ord)
      ORDER BY a.ord`,
    [ids, whs, groups, payloads, event, now],
  );
  pendingKick = true;
}

let pendingKick = false;

// ---------------------------------------------------------------------------------------------------------------
// CRUD

const hookJson = (h: any) => ({
  id: h.id,
  url: h.url,
  events: h.events,
  active: h.active,
  created_at: iso(h.created_at),
});

function validUrl(s: string): boolean {
  try {
    const u = new URL(s);
    return (u.protocol === "http:" || u.protocol === "https:") && !!u.hostname;
  } catch {
    return false;
  }
}

function checkUrl(v: Validator, b: Record<string, unknown>, required: boolean): string | undefined {
  if (!("url" in b) || b.url === null) {
    if (required || "url" in b) v.add("url", "required");
    return undefined;
  }
  if (typeof b.url !== "string" || !validUrl(b.url) || b.url.length > 2048) {
    v.add("url", "invalid");
    return undefined;
  }
  return b.url;
}

function checkEvents(v: Validator, b: Record<string, unknown>, required: boolean): string[] | undefined {
  if (!("events" in b) || b.events === null) {
    if (required || "events" in b) v.add("events", "required");
    return undefined;
  }
  const e = b.events;
  if (!Array.isArray(e)) {
    v.add("events", "invalid");
    return undefined;
  }
  if (e.length === 0) {
    v.add("events", "too_short");
    return undefined;
  }
  let ok = true;
  e.forEach((x, i) => {
    if (typeof x !== "string" || !(EVENTS as readonly string[]).includes(x)) {
      v.add(`events.${i}`, "invalid");
      ok = false;
    }
  });
  if (!ok) {
    v.add("events", "invalid");
    return undefined;
  }
  return [...new Set(e as string[])];
}

export async function createHook(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  requireOrgAdmin(o);
  const b = c.body();
  const v = new Validator();
  const url = checkUrl(v, b, true);
  const events = checkEvents(v, b, true);
  let secret: string | undefined;
  if ("secret" in b && b.secret !== null) {
    if (typeof b.secret !== "string" || b.secret.length === 0 || b.secret.length > 1000) v.add("secret", "invalid");
    else secret = b.secret;
  }
  v.throwIfAny();
  secret ??= crypto.randomBytes(24).toString("base64url");
  const r = await pool.query(
    `INSERT INTO webhooks (id, org_id, url, events, secret, active, created_at)
     VALUES ($1,$2,$3,$4,$5,true,$6) RETURNING *`,
    [crypto.randomUUID(), o.id, url, events, secret, new Date()],
  );
  return { status: 201, body: { ...hookJson(r.rows[0]), secret } };
}

export async function listHooks(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  requireOrgAdmin(o);
  const { limit, offset } = pageParams(c.query);
  const r = await pool.query(
    "SELECT * FROM webhooks WHERE org_id = $1 ORDER BY created_at, id LIMIT $2 OFFSET $3",
    [o.id, limit + 1, offset],
  );
  const p = page(r.rows, limit, offset);
  return { status: 200, body: { items: p.items.map(hookJson), next_cursor: p.next_cursor } };
}

async function loadHook(c: Ctx) {
  const o = await loadOrg(c.params[0], c.userId);
  requireOrgAdmin(o);
  const id = c.params[1];
  if (!UUID_RE.test(id)) throw notFound("webhook not found");
  const r = await pool.query("SELECT * FROM webhooks WHERE id = $1 AND org_id = $2", [id, o.id]);
  if (!r.rowCount) throw notFound("webhook not found");
  return r.rows[0];
}

export async function getHook(c: Ctx): Promise<Result> {
  return { status: 200, body: hookJson(await loadHook(c)) };
}

export async function patchHook(c: Ctx): Promise<Result> {
  const h = await loadHook(c);
  const b = c.body();
  const v = new Validator();
  const url = checkUrl(v, b, false);
  const events = checkEvents(v, b, false);
  let active: boolean | undefined;
  if ("active" in b) {
    if (typeof b.active !== "boolean") v.add("active", "invalid");
    else active = b.active;
  }
  v.throwIfAny();
  const r = await pool.query(
    `UPDATE webhooks SET url = COALESCE($2, url), events = COALESCE($3, events), active = COALESCE($4, active)
      WHERE id = $1 RETURNING *`,
    [h.id, url ?? null, events ?? null, active ?? null],
  );
  return { status: 200, body: hookJson(r.rows[0]) };
}

export async function deleteHook(c: Ctx): Promise<Result> {
  const h = await loadHook(c);
  await pool.query("DELETE FROM webhooks WHERE id = $1", [h.id]);
  return { status: 204 };
}

export async function listDeliveries(c: Ctx): Promise<Result> {
  const h = await loadHook(c);
  const { limit, offset } = pageParams(c.query);
  const r = await pool.query(
    `SELECT id, event, status, attempts, last_status_code, created_at FROM deliveries
      WHERE webhook_id = $1 ORDER BY seq DESC LIMIT $2 OFFSET $3`,
    [h.id, limit + 1, offset],
  );
  const p = page(r.rows, limit, offset);
  return {
    status: 200,
    body: {
      items: p.items.map((d) => ({
        id: d.id,
        event: d.event,
        status: d.status,
        attempts: d.attempts,
        last_status_code: d.last_status_code,
        created_at: iso(d.created_at),
      })),
      next_cursor: p.next_cursor,
    },
  };
}

// ---------------------------------------------------------------------------------------------------------------
// delivery worker

const MAX_ATTEMPTS = 6;
const TIMEOUT_MS = 5000;

let backoffScale = 1;
let running = false;
let timer: NodeJS.Timeout | null = null;
let ticking = false;
let again = false;
const inFlight = new Map<string, Promise<void>>();

export function startWorker(scale: number): void {
  backoffScale = scale;
  running = true;
  schedule(0);
}

export function kick(): void {
  if (!pendingKick || !running) return;
  pendingKick = false;
  if (ticking) again = true;
  else schedule(0);
}

function schedule(ms: number): void {
  if (!running) return;
  if (ticking) {
    if (ms === 0) again = true;
    return;
  }
  if (timer) clearTimeout(timer);
  timer = setTimeout(tick, ms);
}

// Attempts that finished after a tick's query started may still look pending in that tick's snapshot.
let epoch = 0;
const finishedAt = new Map<string, number>();

async function tick(): Promise<void> {
  timer = null;
  if (!running) return;
  ticking = true;
  const start = epoch;
  try {
    const r = await pool.query(
      `SELECT d.id, d.payload, d.event, d.attempts, w.url, w.secret FROM deliveries d
         JOIN webhooks w ON w.id = d.webhook_id
        WHERE d.status = 'pending' AND d.next_attempt_at <= now()
          AND NOT EXISTS (SELECT 1 FROM deliveries e WHERE e.webhook_id = d.webhook_id AND e.group_key = d.group_key
                             AND e.status = 'pending' AND e.seq < d.seq)
        ORDER BY d.seq LIMIT 200`,
    );
    for (const [id, e] of finishedAt) if (e <= start) finishedAt.delete(id);
    for (const d of r.rows) {
      if (inFlight.has(d.id) || finishedAt.has(d.id) || !running) continue;
      const p = attempt(d).finally(() => {
        inFlight.delete(d.id);
        finishedAt.set(d.id, ++epoch);
        if (running) schedule(0);
      });
      inFlight.set(d.id, p);
    }
  } catch {
    // database hiccup: try again on the next tick
  } finally {
    ticking = false;
  }
  if (again) {
    again = false;
    schedule(0);
  } else {
    schedule(50);
  }
}

function post(url: string, headers: Record<string, string>, body: string): Promise<number | null> {
  return new Promise((resolve) => {
    let done = false;
    const finish = (v: number | null) => {
      if (!done) {
        done = true;
        resolve(v);
      }
    };
    let u: URL;
    try {
      u = new URL(url);
    } catch {
      return finish(null);
    }
    const mod = u.protocol === "https:" ? https : http;
    const req = mod.request(
      u,
      {
        method: "POST",
        headers: { ...headers, "Content-Length": String(Buffer.byteLength(body)), Connection: "close" },
        agent: false,
        timeout: TIMEOUT_MS,
      },
      (res) => {
        res.resume();
        res.on("end", () => finish(res.statusCode ?? null));
        res.on("error", () => finish(null));
      },
    );
    const t = setTimeout(() => {
      finish(-1);
      req.destroy();
    }, TIMEOUT_MS);
    req.on("timeout", () => {
      finish(-1);
      req.destroy();
    });
    req.on("error", () => finish(null));
    req.on("close", () => clearTimeout(t));
    req.end(body);
  });
}

async function attempt(d: any): Promise<void> {
  const ts = String(Math.floor(Date.now() / 1000));
  const sig = crypto.createHmac("sha256", d.secret).update(`${ts}.${d.payload}`).digest("hex");
  const code = await post(
    d.url,
    {
      "Content-Type": "application/json",
      "X-Tracker-Event": d.event,
      "X-Tracker-Delivery": d.id,
      "X-Tracker-Timestamp": ts,
      "X-Tracker-Signature": `sha256=${sig}`,
    },
    d.payload,
  );
  const ok = code !== null && code >= 200 && code < 300;
  const statusCode = code !== null && code >= 0 ? code : null;
  const attempts = d.attempts + 1;
  try {
    if (ok) {
      await pool.query(
        "UPDATE deliveries SET status = 'succeeded', attempts = $2, last_status_code = $3 WHERE id = $1",
        [d.id, attempts, statusCode],
      );
    } else if (attempts >= MAX_ATTEMPTS) {
      await pool.query(
        "UPDATE deliveries SET status = 'failed', attempts = $2, last_status_code = $3 WHERE id = $1",
        [d.id, attempts, statusCode],
      );
    } else {
      const delay = 1000 * 2 ** (attempts - 1) * backoffScale;
      await pool.query(
        `UPDATE deliveries SET attempts = $2, last_status_code = $3,
                next_attempt_at = now() + make_interval(secs => $4) WHERE id = $1`,
        [d.id, attempts, statusCode, delay / 1000],
      );
    }
  } catch {
    // leave it pending; it will be retried
  }
}

/** Stop picking up new deliveries and wait (bounded) for in-flight attempts. */
export async function stopWorker(maxMs: number): Promise<void> {
  running = false;
  if (timer) clearTimeout(timer);
  timer = null;
  if (!inFlight.size) return;
  await Promise.race([Promise.allSettled([...inFlight.values()]), new Promise((r) => setTimeout(r, maxMs))]);
}
