import type { IncomingMessage, ServerResponse } from "node:http";

export interface FieldError {
  field: string;
  code: string;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    public detail: string = code,
    public errors?: FieldError[],
  ) {
    super(detail);
  }
}

export const notFound = (what = "not found") => new ApiError(404, "not_found", what);
export const forbidden = (what = "forbidden") => new ApiError(403, "forbidden", what);
export const conflict = (code: string, detail = code) => new ApiError(409, code, detail);
export const validation = (errors: FieldError[]) =>
  new ApiError(422, "validation_failed", "request validation failed", errors);
export const fieldError = (field: string, code: string) => validation([{ field, code }]);

export interface Result {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
  raw?: string;
}

export interface Ctx {
  req: IncomingMessage;
  method: string;
  params: string[];
  query: URLSearchParams;
  rawBody: string;
  userId: string;
  body(): Record<string, unknown>;
  header(name: string): string | undefined;
}

export function sendProblem(res: ServerResponse, e: ApiError): void {
  const body: Record<string, unknown> = { status: e.status, code: e.code, detail: e.detail };
  if (e.errors) body.errors = e.errors;
  const s = JSON.stringify(body);
  res.writeHead(e.status, {
    "Content-Type": "application/problem+json",
    "Content-Length": Buffer.byteLength(s),
  });
  res.end(s);
}

export function sendResult(res: ServerResponse, r: Result): void {
  const headers: Record<string, string | number> = { ...(r.headers ?? {}) };
  if (r.status === 204 || (r.body === undefined && r.raw === undefined)) {
    res.writeHead(r.status, headers);
    res.end();
    return;
  }
  const s = r.raw ?? JSON.stringify(r.body);
  headers["Content-Type"] = "application/json";
  headers["Content-Length"] = Buffer.byteLength(s);
  res.writeHead(r.status, headers);
  res.end(s);
}

export function parseJson(raw: string): Record<string, unknown> {
  if (raw.trim() === "") return {};
  let v: unknown;
  try {
    v = JSON.parse(raw);
  } catch {
    throw new ApiError(400, "bad_request", "malformed JSON");
  }
  if (v === null || typeof v !== "object" || Array.isArray(v)) {
    throw new ApiError(400, "bad_request", "JSON body must be an object");
  }
  return v as Record<string, unknown>;
}

// ---------------------------------------------------------------------------------------------------------------
// validation helpers

export class Validator {
  errors: FieldError[] = [];
  constructor(public prefix = "") {}
  add(field: string, code: string): void {
    this.errors.push({ field: this.prefix + field, code });
  }
  throwIfAny(): void {
    if (this.errors.length) throw validation(this.errors);
  }
}

export const charLen = (s: string) => {
  let n = 0;
  for (const _ of s) n++;
  return n;
};

interface StrOpts {
  min?: number;
  max: number;
  trim?: boolean;
  nullable?: boolean;
}

/** Returns undefined when absent (or invalid), null when explicitly null and nullable, else the string. */
export function optStr(v: Validator, body: Record<string, unknown>, field: string, o: StrOpts): string | null | undefined {
  if (!(field in body)) return undefined;
  const x = body[field];
  if (x === null) {
    if (o.nullable) return null;
    v.add(field, "required");
    return undefined;
  }
  if (typeof x !== "string") {
    v.add(field, "invalid");
    return undefined;
  }
  const s = o.trim ? x.trim() : x;
  const n = charLen(s);
  const min = o.min ?? 0;
  if (n < min) {
    v.add(field, n === 0 ? "too_short" : "too_short");
    return undefined;
  }
  if (n > o.max) {
    v.add(field, "too_long");
    return undefined;
  }
  return s;
}

export function reqStr(v: Validator, body: Record<string, unknown>, field: string, o: StrOpts): string | undefined {
  if (!(field in body) || body[field] === null) {
    v.add(field, "required");
    return undefined;
  }
  const r = optStr(v, body, field, o);
  return r ?? undefined;
}

export function optEnum<T extends string>(
  v: Validator,
  body: Record<string, unknown>,
  field: string,
  values: readonly T[],
  required = false,
): T | undefined {
  if (!(field in body) || body[field] === null) {
    if (required || field in body) v.add(field, "required");
    return undefined;
  }
  const x = body[field];
  if (typeof x !== "string" || !(values as readonly string[]).includes(x)) {
    v.add(field, "invalid");
    return undefined;
  }
  return x as T;
}

export const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

// ---------------------------------------------------------------------------------------------------------------
// pagination (opaque offset cursors)

export function pageParams(q: URLSearchParams): { limit: number; offset: number } {
  let limit = 50;
  const l = q.get("limit");
  if (l !== null) {
    if (!/^\d+$/.test(l)) throw fieldError("limit", "invalid");
    limit = Number(l);
    if (limit < 1 || limit > 100) throw fieldError("limit", "out_of_range");
  }
  let offset = 0;
  const c = q.get("cursor");
  if (c !== null && c !== "") {
    let ok = false;
    try {
      const d = JSON.parse(Buffer.from(c, "base64url").toString("utf8"));
      if (d && Number.isInteger(d.o) && d.o >= 0) {
        offset = d.o;
        ok = true;
      }
    } catch {}
    if (!ok) throw fieldError("cursor", "invalid");
  }
  return { limit, offset };
}

/** rows: fetched with LIMIT limit+1 OFFSET offset. */
export function page<T>(rows: T[], limit: number, offset: number): { items: T[]; next_cursor: string | null } {
  const more = rows.length > limit;
  const items = more ? rows.slice(0, limit) : rows;
  return {
    items,
    next_cursor: more ? Buffer.from(JSON.stringify({ o: offset + limit })).toString("base64url") : null,
  };
}

export const iso = (d: Date | null | undefined): string | null => (d ? d.toISOString() : null);
