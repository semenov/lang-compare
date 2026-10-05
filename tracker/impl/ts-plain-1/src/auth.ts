import crypto from "node:crypto";
import { hash as argonHash, verify as argonVerify, Algorithm } from "@node-rs/argon2";
import { pool, tx } from "./db.js";
import { ApiError, type Ctx, type Result, Validator, conflict, iso, optStr, reqStr } from "./http.js";

const ACCESS_TTL = 900;
const REFRESH_TTL_MS = 30 * 24 * 3600 * 1000;

let jwtSecret = "";
export function setJwtSecret(s: string): void {
  jwtSecret = s;
}

const b64u = (b: Buffer | string) => Buffer.from(b).toString("base64url");

function sign(data: string): string {
  return crypto.createHmac("sha256", jwtSecret).update(data).digest("base64url");
}

export function issueAccessToken(userId: string): string {
  const iat = Math.floor(Date.now() / 1000);
  const h = b64u(JSON.stringify({ alg: "HS256", typ: "JWT" }));
  const p = b64u(JSON.stringify({ sub: userId, iat, exp: iat + ACCESS_TTL, typ: "access" }));
  return `${h}.${p}.${sign(`${h}.${p}`)}`;
}

const unauth = () => new ApiError(401, "unauthenticated", "authentication required");

/** Validates the Authorization header; returns the user id. */
export async function authenticate(header: string | undefined): Promise<string> {
  if (!header) throw unauth();
  const m = /^Bearer\s+(\S+)$/i.exec(header.trim());
  if (!m) throw unauth();
  const parts = m[1].split(".");
  if (parts.length !== 3) throw unauth();
  const [h, p, s] = parts;
  let hdr: any, claims: any;
  try {
    hdr = JSON.parse(Buffer.from(h, "base64url").toString("utf8"));
    claims = JSON.parse(Buffer.from(p, "base64url").toString("utf8"));
  } catch {
    throw unauth();
  }
  if (!hdr || hdr.alg !== "HS256") throw unauth();
  const expected = Buffer.from(sign(`${h}.${p}`));
  const got = Buffer.from(s);
  if (expected.length !== got.length || !crypto.timingSafeEqual(expected, got)) throw unauth();
  if (!claims || claims.typ !== "access" || typeof claims.sub !== "string" || typeof claims.exp !== "number") {
    throw unauth();
  }
  if (claims.exp <= Math.floor(Date.now() / 1000)) throw new ApiError(401, "token_expired", "access token expired");
  const r = await pool.query("SELECT 1 FROM users WHERE id::text = $1", [claims.sub]);
  if (!r.rowCount) throw unauth();
  return claims.sub;
}

export function userJson(u: any) {
  return { id: u.id, email: u.email, name: u.name, created_at: iso(u.created_at) };
}

function validateEmail(v: Validator, body: Record<string, unknown>): string | undefined {
  const e = reqStr(v, body, "email", { max: 100000, trim: true });
  if (e === undefined) return undefined;
  if (e.length === 0) {
    v.add("email", "required");
    return undefined;
  }
  if (charLen(e) > 254) {
    v.add("email", "too_long");
    return undefined;
  }
  const at = e.indexOf("@");
  if (at <= 0 || at === e.length - 1 || /\s/.test(e)) {
    v.add("email", "invalid");
    return undefined;
  }
  return e.toLowerCase();
}

const charLen = (s: string) => [...s].length;

export async function register(c: Ctx): Promise<Result> {
  const b = c.body();
  const v = new Validator();
  const email = validateEmail(v, b);
  const password = reqStr(v, b, "password", { min: 10, max: 128 });
  const name = reqStr(v, b, "name", { min: 1, max: 100, trim: true });
  v.throwIfAny();
  const hash = await argonHash(password!, {
    algorithm: Algorithm.Argon2id,
    memoryCost: 19456,
    timeCost: 2,
    parallelism: 1,
  });
  const id = crypto.randomUUID();
  try {
    const r = await pool.query(
      "INSERT INTO users (id, email, name, password_hash, created_at) VALUES ($1,$2,$3,$4,$5) RETURNING *",
      [id, email, name, hash, new Date()],
    );
    return { status: 201, body: userJson(r.rows[0]) };
  } catch (e: any) {
    if (e.code === "23505") throw conflict("email_taken", "email already registered");
    throw e;
  }
}

const tokenHash = (t: string) => crypto.createHash("sha256").update(t).digest("hex");

async function issuePair(client: { query: typeof pool.query }, userId: string, chainId: string) {
  const refresh = crypto.randomBytes(32).toString("base64url");
  await client.query(
    "INSERT INTO refresh_tokens (id, user_id, chain_id, token_hash, expires_at) VALUES ($1,$2,$3,$4,$5)",
    [crypto.randomUUID(), userId, chainId, tokenHash(refresh), new Date(Date.now() + REFRESH_TTL_MS)],
  );
  return {
    access_token: issueAccessToken(userId),
    refresh_token: refresh,
    token_type: "Bearer",
    expires_in: ACCESS_TTL,
  };
}

const DUMMY_HASH = argonHash("dummy-password-for-timing", {
  algorithm: Algorithm.Argon2id,
  memoryCost: 19456,
  timeCost: 2,
  parallelism: 1,
});

export async function login(c: Ctx): Promise<Result> {
  const b = c.body();
  const bad = () => new ApiError(401, "invalid_credentials", "wrong email or password");
  if (typeof b.email !== "string" || typeof b.password !== "string") {
    const v = new Validator();
    if (typeof b.email !== "string") v.add("email", b.email === undefined ? "required" : "invalid");
    if (typeof b.password !== "string") v.add("password", b.password === undefined ? "required" : "invalid");
    v.throwIfAny();
  }
  const email = (b.email as string).trim().toLowerCase();
  const r = await pool.query("SELECT id, password_hash FROM users WHERE email = $1", [email]);
  if (!r.rowCount) {
    try {
      await argonVerify(await DUMMY_HASH, b.password as string);
    } catch {}
    throw bad();
  }
  let ok = false;
  try {
    ok = await argonVerify(r.rows[0].password_hash, b.password as string);
  } catch {
    ok = false;
  }
  if (!ok) throw bad();
  return { status: 200, body: await issuePair(pool, r.rows[0].id, crypto.randomUUID()) };
}

const invalidToken = () => new ApiError(401, "invalid_token", "invalid refresh token");

export async function refresh(c: Ctx): Promise<Result> {
  const b = c.body();
  if (typeof b.refresh_token !== "string") {
    const v = new Validator();
    v.add("refresh_token", b.refresh_token === undefined ? "required" : "invalid");
    v.throwIfAny();
  }
  const h = tokenHash(b.refresh_token as string);
  const out = await tx(async (cl) => {
    const r = await cl.query("SELECT * FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE", [h]);
    if (!r.rowCount) return { err: invalidToken() };
    const t = r.rows[0];
    if (t.used_at) {
      await cl.query("UPDATE refresh_tokens SET revoked_at = now() WHERE chain_id = $1 AND revoked_at IS NULL", [
        t.chain_id,
      ]);
      return { err: new ApiError(401, "token_reused", "refresh token reuse detected") };
    }
    if (t.revoked_at || t.expires_at.getTime() <= Date.now()) return { err: invalidToken() };
    await cl.query("UPDATE refresh_tokens SET used_at = now() WHERE id = $1", [t.id]);
    return { pair: await issuePair(cl, t.user_id, t.chain_id) };
  });
  if (out.err) throw out.err;
  return { status: 200, body: out.pair };
}

export async function logout(c: Ctx): Promise<Result> {
  const b = c.body();
  if (typeof b.refresh_token !== "string") {
    const v = new Validator();
    v.add("refresh_token", b.refresh_token === undefined ? "required" : "invalid");
    v.throwIfAny();
  }
  const r = await pool.query("SELECT chain_id FROM refresh_tokens WHERE token_hash = $1", [
    tokenHash(b.refresh_token as string),
  ]);
  if (!r.rowCount) throw invalidToken();
  await pool.query("UPDATE refresh_tokens SET revoked_at = now() WHERE chain_id = $1 AND revoked_at IS NULL", [
    r.rows[0].chain_id,
  ]);
  return { status: 204 };
}

export async function getMe(c: Ctx): Promise<Result> {
  const r = await pool.query("SELECT * FROM users WHERE id = $1", [c.userId]);
  return { status: 200, body: userJson(r.rows[0]) };
}

export async function patchMe(c: Ctx): Promise<Result> {
  const b = c.body();
  const v = new Validator();
  const name = optStr(v, b, "name", { min: 1, max: 100, trim: true });
  v.throwIfAny();
  if (typeof name === "string") {
    await pool.query("UPDATE users SET name = $2 WHERE id = $1", [c.userId, name]);
  }
  return getMe(c);
}
