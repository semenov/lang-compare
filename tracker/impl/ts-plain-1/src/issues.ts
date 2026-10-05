import crypto from "node:crypto";
import type pg from "pg";
import { pool, tx } from "./db.js";
import {
  ApiError,
  type Ctx,
  type Result,
  Validator,
  charLen,
  conflict,
  forbidden,
  iso,
  notFound,
  optEnum,
  optStr,
  page,
  pageParams,
  reqStr,
  UUID_RE,
  validation,
} from "./http.js";
import {
  type OrgCtx,
  type ProjectCtx,
  atLeast,
  loadOrg,
  loadProject,
  requireRole,
  userProjectRole,
  visibleProjects,
} from "./access.js";
import { emit, emitMany } from "./webhooks.js";

export const TYPES = ["task", "bug", "story"] as const;
export const STATUSES = ["todo", "in_progress", "done"] as const;
export const PRIORITIES = ["highest", "high", "medium", "low", "lowest"] as const;
const PRI_RANK: Record<string, number> = { lowest: 0, low: 1, medium: 2, high: 3, highest: 4 };
const TRANSITIONS: Record<string, string[]> = {
  todo: ["in_progress"],
  in_progress: ["todo", "done"],
  done: ["in_progress"],
};
const LABEL_RE = /^[a-z0-9][a-z0-9_.-]{0,49}$/;
const DATE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

/** Maximal runs of Unicode letters and digits, lower-cased. */
export function words(text: string | null | undefined): string[] {
  if (!text) return [];
  const m = text.match(/[\p{L}\p{Nd}]+/gu) ?? [];
  return m.map((w) => Array.from(w, (ch) => ch.toLowerCase()).join(""));
}

function searchWords(title: string, description: string | null): string[] {
  return [...new Set([...words(title), ...words(description)])];
}

export function issueJson(i: any) {
  return {
    id: i.id,
    key: `${i.project_key}-${i.number}`,
    number: i.number,
    project_key: i.project_key,
    type: i.type,
    title: i.title,
    description: i.description ?? null,
    status: i.status,
    priority: i.priority,
    assignee_id: i.assignee_id ?? null,
    reporter_id: i.reporter_id,
    labels: i.labels,
    due_date: i.due_date ?? null,
    version: i.version,
    comment_count: i.comment_count,
    created_at: iso(i.created_at),
    updated_at: iso(i.updated_at),
    resolved_at: iso(i.resolved_at),
  };
}

function validDate(s: string): boolean {
  const m = DATE_RE.exec(s);
  if (!m) return false;
  const [y, mo, d] = [Number(m[1]), Number(m[2]), Number(m[3])];
  if (mo < 1 || mo > 12 || d < 1) return false;
  const dim = new Date(Date.UTC(y, mo, 0)).getUTCDate();
  return d <= dim;
}

interface IssueFields {
  type?: string;
  title?: string;
  description?: string | null;
  priority?: string;
  assignee_id?: string | null;
  labels?: string[];
  due_date?: string | null;
}

/** Synchronous field validation; create=true enforces required fields. */
function validateFields(v: Validator, b: Record<string, unknown>, create: boolean): IssueFields {
  const f: IssueFields = {};
  f.type = optEnum(v, b, "type", TYPES, create);
  f.title = create
    ? reqStr(v, b, "title", { min: 1, max: 255, trim: true })
    : (optStr(v, b, "title", { min: 1, max: 255, trim: true }) ?? undefined);
  const d = optStr(v, b, "description", { max: 65536, nullable: true });
  if (d !== undefined) f.description = d;
  f.priority = optEnum(v, b, "priority", PRIORITIES);
  if ("assignee_id" in b) {
    const a = b.assignee_id;
    if (a === null) f.assignee_id = null;
    else if (typeof a !== "string" || !UUID_RE.test(a)) v.add("assignee_id", "invalid");
    else f.assignee_id = a.toLowerCase();
  }
  if ("labels" in b) {
    const l = b.labels;
    if (l === null) f.labels = [];
    else if (!Array.isArray(l)) v.add("labels", "invalid");
    else {
      const out = new Set<string>();
      let ok = true;
      l.forEach((x, i) => {
        if (typeof x !== "string") {
          v.add(`labels.${i}`, "invalid");
          ok = false;
          return;
        }
        const s = x.toLowerCase();
        if (!LABEL_RE.test(s)) {
          v.add(`labels.${i}`, charLen(s) > 50 ? "too_long" : "invalid");
          ok = false;
          return;
        }
        out.add(s);
      });
      if (ok && out.size > 20) {
        v.add("labels", "too_long");
        ok = false;
      }
      if (ok) f.labels = [...out].sort();
    }
  }
  if ("due_date" in b) {
    const x = b.due_date;
    if (x === null) f.due_date = null;
    else if (typeof x !== "string" || !validDate(x)) v.add("due_date", "invalid");
    else f.due_date = x;
  }
  return f;
}

async function checkAssignee(v: Validator, projectId: string, f: IssueFields, cl: pg.PoolClient | pg.Pool = pool) {
  if (!f.assignee_id) return;
  const role = await userProjectRole(projectId, f.assignee_id, cl);
  if (!atLeast(role, "developer")) v.add("assignee_id", "invalid");
}

/** Assignee checks for many items with a single query. */
async function eligibleAssignees(projectId: string, ids: string[]): Promise<Set<string>> {
  const out = new Set<string>();
  for (const id of new Set(ids)) {
    if (atLeast(await userProjectRole(projectId, id), "developer")) out.add(id);
  }
  return out;
}

// ---------------------------------------------------------------------------------------------------------------
// lookup

function parseIssueKey(s: string): { key: string; number: number } {
  const m = /^([A-Za-z][A-Za-z0-9]*)-(\d{1,9})$/.exec(s);
  if (!m) throw notFound("issue not found");
  return { key: m[1].toUpperCase(), number: Number(m[2]) };
}

interface IssueCtx {
  org: OrgCtx;
  project: ProjectCtx;
  issue: any;
}

async function loadIssue(c: Ctx, cl: pg.PoolClient | pg.Pool = pool, lock = false): Promise<IssueCtx> {
  const org = await loadOrg(c.params[0], c.userId, cl);
  const k = parseIssueKey(c.params[1]);
  const project = await loadProject(org, k.key, c.userId, cl);
  const r = await cl.query(
    `SELECT * FROM issues WHERE project_id = $1 AND number = $2${lock ? " FOR UPDATE" : ""}`,
    [project.id, k.number],
  );
  if (!r.rowCount) throw notFound("issue not found");
  return { org, project, issue: r.rows[0] };
}

function parseIfMatch(h: string | undefined): number | null | "bad" {
  if (h === undefined) return null;
  const m = /^\s*(?:W\/)?"?(\d+)"?\s*$/.exec(h);
  if (!m) return "bad";
  return Number(m[1]);
}

// ---------------------------------------------------------------------------------------------------------------
// create

const INSERT_COLS = [
  "id",
  "project_id",
  "org_id",
  "project_key",
  "number",
  "type",
  "title",
  "description",
  "status",
  "priority",
  "priority_rank",
  "assignee_id",
  "reporter_id",
  "labels",
  "due_date",
  "version",
  "comment_count",
  "created_at",
  "updated_at",
  "resolved_at",
  "words",
];

function buildRow(project: ProjectCtx, f: IssueFields, number: number, userId: string, now: Date) {
  const description = f.description ?? null;
  return {
    id: crypto.randomUUID(),
    project_id: project.id,
    org_id: project.org_id,
    project_key: project.key,
    number,
    type: f.type!,
    title: f.title!,
    description,
    status: "todo",
    priority: f.priority ?? "medium",
    priority_rank: PRI_RANK[f.priority ?? "medium"],
    assignee_id: f.assignee_id ?? null,
    reporter_id: userId,
    labels: f.labels ?? [],
    due_date: f.due_date ?? null,
    version: 1,
    comment_count: 0,
    created_at: now,
    updated_at: now,
    resolved_at: null,
    words: searchWords(f.title!, description),
  } as Record<string, any>;
}

async function insertIssues(cl: pg.PoolClient, rows: Record<string, any>[], actorId: string): Promise<void> {
  const CHUNK = 500;
  for (let s = 0; s < rows.length; s += CHUNK) {
    const chunk = rows.slice(s, s + CHUNK);
    const params: unknown[] = [];
    const tuples = chunk.map((row) => {
      const ph = INSERT_COLS.map((col) => {
        params.push(row[col]);
        return `$${params.length}`;
      });
      return `(${ph.join(",")})`;
    });
    await cl.query(`INSERT INTO issues (${INSERT_COLS.join(",")}) VALUES ${tuples.join(",")}`, params);
    await cl.query(
      `INSERT INTO history (id, issue_id, actor_id, created_at, changes)
       SELECT gen_random_uuid(), a.id, $2, a.created_at, '[{"field":"created","from":null,"to":null}]'::jsonb
         FROM unnest($1::uuid[], $3::timestamptz[]) WITH ORDINALITY AS a(id, created_at, ord) ORDER BY a.ord`,
      [chunk.map((r) => r.id), actorId, chunk.map((r) => r.created_at)],
    );
  }
}

async function createIn(
  cl: pg.PoolClient,
  org: OrgCtx,
  project: ProjectCtx,
  items: IssueFields[],
  userId: string,
): Promise<Record<string, any>[]> {
  const r = await cl.query("UPDATE projects SET issue_counter = issue_counter + $2 WHERE id = $1 RETURNING issue_counter", [
    project.id,
    items.length,
  ]);
  const last: number = r.rows[0].issue_counter;
  const first = last - items.length + 1;
  const now = new Date();
  const rows = items.map((f, i) => buildRow(project, f, first + i, userId, now));
  await insertIssues(cl, rows, userId);
  if (rows.length === 1) {
    await emit(cl, org, "issue.created", userId, rows[0].id, { issue: issueJson(rows[0]) });
  } else {
    await emitMany(
      cl,
      org,
      "issue.created",
      userId,
      rows.map((row) => ({ groupKey: row.id, data: { issue: issueJson(row) } })),
    );
  }
  return rows;
}

export async function createIssue(c: Ctx): Promise<Result> {
  const org = await loadOrg(c.params[0], c.userId);
  const project = await loadProject(org, c.params[1], c.userId);
  requireRole(project.role, "developer");
  const idemKey = c.header("idempotency-key");
  if (idemKey !== undefined && (idemKey.length < 1 || idemKey.length > 255)) {
    throw validation([{ field: "Idempotency-Key", code: idemKey.length ? "too_long" : "too_short" }]);
  }
  const reqHash = crypto.createHash("sha256").update(`${c.req.url}\n${c.rawBody}`).digest("hex");
  const reused = () => new ApiError(422, "idempotency_key_reused", "idempotency key was used with a different request");

  const stored = async (): Promise<Result | null> => {
    const r = await pool.query(
      `SELECT request_hash, status, body FROM idempotency_keys
        WHERE user_id = $1 AND key = $2 AND created_at > now() - interval '24 hours' AND status IS NOT NULL`,
      [c.userId, idemKey],
    );
    if (!r.rowCount) return null;
    const s = r.rows[0];
    if (s.request_hash !== reqHash) throw reused();
    return { status: s.status, raw: s.body };
  };

  if (idemKey !== undefined) {
    const s = await stored();
    if (s) return s;
  }
  const b = c.body();
  const v = new Validator();
  const f = validateFields(v, b, true);
  if (!v.errors.some((e) => e.field === "assignee_id")) await checkAssignee(v, project.id, f);
  v.throwIfAny();

  const out = await tx(async (cl) => {
    if (idemKey !== undefined) {
      const claim = await cl.query(
        `INSERT INTO idempotency_keys (user_id, key, request_hash, created_at) VALUES ($1,$2,$3,now())
         ON CONFLICT (user_id, key) DO UPDATE SET request_hash = EXCLUDED.request_hash, status = NULL, body = NULL,
                created_at = now()
          WHERE idempotency_keys.created_at <= now() - interval '24 hours'
         RETURNING 1`,
        [c.userId, idemKey, reqHash],
      );
      if (!claim.rowCount) return null; // someone else owns this key
    }
    const [row] = await createIn(cl, org, project, [f], c.userId);
    const body = JSON.stringify(issueJson(row));
    if (idemKey !== undefined) {
      await cl.query("UPDATE idempotency_keys SET status = 201, body = $3 WHERE user_id = $1 AND key = $2", [
        c.userId,
        idemKey,
        body,
      ]);
    }
    return body;
  });
  if (out === null) {
    const s = await stored();
    if (s) return s;
    throw new ApiError(409, "conflict", "a request with this idempotency key is in progress");
  }
  return { status: 201, raw: out, headers: { ETag: '"1"' } };
}

export async function bulkCreate(c: Ctx): Promise<Result> {
  const org = await loadOrg(c.params[0], c.userId);
  const project = await loadProject(org, c.params[1], c.userId);
  requireRole(project.role, "developer");
  const b = c.body();
  const list = b.issues;
  if (!Array.isArray(list)) throw validation([{ field: "issues", code: list === undefined ? "required" : "invalid" }]);
  if (list.length < 1) throw validation([{ field: "issues", code: "too_short" }]);
  if (list.length > 1000) throw validation([{ field: "issues", code: "too_long" }]);
  const v = new Validator();
  const fields: IssueFields[] = [];
  list.forEach((item, i) => {
    v.prefix = `issues.${i}.`;
    if (item === null || typeof item !== "object" || Array.isArray(item)) {
      v.errors.push({ field: `issues.${i}`, code: "invalid" });
      fields.push({});
      return;
    }
    fields.push(validateFields(v, item as Record<string, unknown>, true));
  });
  const assignees = fields.map((f) => f.assignee_id).filter((x): x is string => !!x);
  if (assignees.length) {
    const ok = await eligibleAssignees(project.id, assignees);
    fields.forEach((f, i) => {
      if (f.assignee_id && !ok.has(f.assignee_id)) v.errors.push({ field: `issues.${i}.assignee_id`, code: "invalid" });
    });
  }
  v.throwIfAny();
  const rows = await tx((cl) => createIn(cl, org, project, fields, c.userId));
  return { status: 201, body: { keys: rows.map((r) => `${r.project_key}-${r.number}`) } };
}

// ---------------------------------------------------------------------------------------------------------------
// read / update / transition / delete

export async function getIssue(c: Ctx): Promise<Result> {
  const { issue } = await loadIssue(c);
  return { status: 200, body: issueJson(issue), headers: { ETag: `"${issue.version}"` } };
}

type Change = { field: string; from: unknown; to: unknown };

async function applyChanges(
  cl: pg.PoolClient,
  ic: IssueCtx,
  next: Record<string, any>,
  changes: Change[],
  actorId: string,
): Promise<any> {
  const now = new Date();
  const i = ic.issue;
  const r = await cl.query(
    `UPDATE issues SET type = $2, title = $3, description = $4, status = $5, priority = $6, priority_rank = $7,
            assignee_id = $8, labels = $9, due_date = $10, resolved_at = $11, words = $12,
            version = version + 1, updated_at = $13
      WHERE id = $1 RETURNING *`,
    [
      i.id,
      next.type,
      next.title,
      next.description,
      next.status,
      next.priority,
      PRI_RANK[next.priority],
      next.assignee_id,
      next.labels,
      next.due_date,
      next.resolved_at,
      searchWords(next.title, next.description),
      now,
    ],
  );
  const updated = r.rows[0];
  await cl.query("INSERT INTO history (id, issue_id, actor_id, created_at, changes) VALUES ($1,$2,$3,$4,$5)", [
    crypto.randomUUID(),
    i.id,
    actorId,
    now,
    JSON.stringify(changes),
  ]);
  await emit(cl, ic.org, "issue.updated", actorId, i.id, { issue: issueJson(updated), changes });
  return updated;
}

const sameArr = (a: string[], b: string[]) => a.length === b.length && a.every((x, i) => x === b[i]);

export async function patchIssue(c: Ctx): Promise<Result> {
  const b = c.body();
  const res = await tx(async (cl) => {
    const ic = await loadIssue(c, cl, true);
    requireRole(ic.project.role, "developer");
    const im = parseIfMatch(c.header("if-match"));
    if (im === null) throw new ApiError(428, "precondition_required", "If-Match header is required");
    const v = new Validator();
    if ("status" in b) v.add("status", "invalid");
    const f = validateFields(v, b, false);
    if (!v.errors.some((e) => e.field === "assignee_id")) await checkAssignee(v, ic.project.id, f, cl);
    v.throwIfAny();
    const i = ic.issue;
    if (im === "bad" || im !== i.version) throw new ApiError(412, "version_mismatch", "version mismatch");
    const next: Record<string, any> = {
      type: i.type,
      title: i.title,
      description: i.description,
      status: i.status,
      priority: i.priority,
      assignee_id: i.assignee_id,
      labels: i.labels,
      due_date: i.due_date,
      resolved_at: i.resolved_at,
    };
    const changes: Change[] = [];
    for (const field of ["title", "description", "type", "priority", "assignee_id", "labels", "due_date"] as const) {
      const nv = f[field];
      if (nv === undefined) continue;
      const ov = i[field] ?? null;
      const same = field === "labels" ? sameArr(ov, nv as string[]) : ov === nv;
      if (same) continue;
      changes.push({ field, from: ov, to: nv });
      next[field] = nv;
    }
    if (!changes.length) return i;
    return applyChanges(cl, ic, next, changes, c.userId);
  });
  return { status: 200, body: issueJson(res), headers: { ETag: `"${res.version}"` } };
}

export async function transitionIssue(c: Ctx): Promise<Result> {
  const b = c.body();
  const res = await tx(async (cl) => {
    const ic = await loadIssue(c, cl, true);
    requireRole(ic.project.role, "developer");
    const v = new Validator();
    const status = optEnum(v, b, "status", STATUSES, true);
    v.throwIfAny();
    const i = ic.issue;
    const im = parseIfMatch(c.header("if-match"));
    if (im !== null && (im === "bad" || im !== i.version)) {
      throw new ApiError(412, "version_mismatch", "version mismatch");
    }
    if (!TRANSITIONS[i.status].includes(status!)) {
      throw conflict("transition_not_allowed", `cannot move from ${i.status} to ${status}`);
    }
    const next: Record<string, any> = {
      type: i.type,
      title: i.title,
      description: i.description,
      status,
      priority: i.priority,
      assignee_id: i.assignee_id,
      labels: i.labels,
      due_date: i.due_date,
      resolved_at: status === "done" ? new Date() : null,
    };
    return applyChanges(cl, ic, next, [{ field: "status", from: i.status, to: status }], c.userId);
  });
  return { status: 200, body: issueJson(res), headers: { ETag: `"${res.version}"` } };
}

export async function deleteIssue(c: Ctx): Promise<Result> {
  await tx(async (cl) => {
    const ic = await loadIssue(c, cl, true);
    requireRole(ic.project.role, "admin");
    await cl.query("DELETE FROM issues WHERE id = $1", [ic.issue.id]);
    await emit(cl, ic.org, "issue.deleted", c.userId, ic.issue.id, { issue: issueJson(ic.issue) });
  });
  return { status: 204 };
}

// ---------------------------------------------------------------------------------------------------------------
// comments

const commentJson = (cm: any, issueKey: string) => ({
  id: cm.id,
  issue_key: issueKey,
  author_id: cm.author_id,
  body: cm.body,
  created_at: iso(cm.created_at),
  updated_at: iso(cm.updated_at),
  edited: cm.edited,
});

const issueKeyOf = (i: any) => `${i.project_key}-${i.number}`;

function commentBody(b: Record<string, unknown>): string {
  const v = new Validator();
  const body = reqStr(v, b, "body", { min: 1, max: 20000 });
  if (body !== undefined && body.trim() === "") v.add("body", "too_short");
  v.throwIfAny();
  return body!;
}

export async function createComment(c: Ctx): Promise<Result> {
  const b = c.body();
  const out = await tx(async (cl) => {
    const ic = await loadIssue(c, cl, true);
    requireRole(ic.project.role, "viewer");
    const body = commentBody(b);
    const now = new Date();
    const r = await cl.query(
      `INSERT INTO comments (id, issue_id, author_id, body, created_at, updated_at, edited)
       VALUES ($1,$2,$3,$4,$5,$5,false) RETURNING *`,
      [crypto.randomUUID(), ic.issue.id, c.userId, body, now],
    );
    const iss = await cl.query("UPDATE issues SET comment_count = comment_count + 1 WHERE id = $1 RETURNING *", [
      ic.issue.id,
    ]);
    const cj = commentJson(r.rows[0], issueKeyOf(ic.issue));
    await emit(cl, ic.org, "comment.created", c.userId, ic.issue.id, { issue: issueJson(iss.rows[0]), comment: cj });
    return cj;
  });
  return { status: 201, body: out };
}

export async function listComments(c: Ctx): Promise<Result> {
  const { issue } = await loadIssue(c);
  const { limit, offset } = pageParams(c.query);
  const r = await pool.query("SELECT * FROM comments WHERE issue_id = $1 ORDER BY seq LIMIT $2 OFFSET $3", [
    issue.id,
    limit + 1,
    offset,
  ]);
  const p = page(r.rows, limit, offset);
  const key = issueKeyOf(issue);
  return { status: 200, body: { items: p.items.map((cm) => commentJson(cm, key)), next_cursor: p.next_cursor } };
}

async function loadComment(c: Ctx, cl: pg.PoolClient) {
  const org = await loadOrg(c.params[0], c.userId, cl);
  const id = c.params[1];
  if (!UUID_RE.test(id)) throw notFound("comment not found");
  const r = await cl.query(
    `SELECT cm.*, i.project_key, i.number, p.key AS pkey FROM comments cm
       JOIN issues i ON i.id = cm.issue_id JOIN projects p ON p.id = i.project_id
      WHERE cm.id = $1 AND p.org_id = $2 FOR UPDATE OF cm`,
    [id, org.id],
  );
  if (!r.rowCount) throw notFound("comment not found");
  const cm = r.rows[0];
  const project = await loadProject(org, cm.pkey, c.userId, cl);
  return { org, project, cm };
}

export async function patchComment(c: Ctx): Promise<Result> {
  const b = c.body();
  const out = await tx(async (cl) => {
    const { cm } = await loadComment(c, cl);
    if (cm.author_id !== c.userId) throw forbidden("only the author may edit a comment");
    const body = commentBody(b);
    const r = await cl.query("UPDATE comments SET body = $2, edited = true, updated_at = $3 WHERE id = $1 RETURNING *", [
      cm.id,
      body,
      new Date(),
    ]);
    return commentJson(r.rows[0], issueKeyOf(cm));
  });
  return { status: 200, body: out };
}

export async function deleteComment(c: Ctx): Promise<Result> {
  await tx(async (cl) => {
    const { cm, project } = await loadComment(c, cl);
    if (cm.author_id !== c.userId && project.role !== "admin") throw forbidden("not allowed to delete this comment");
    await cl.query("DELETE FROM comments WHERE id = $1", [cm.id]);
    await cl.query("UPDATE issues SET comment_count = comment_count - 1 WHERE id = $1", [cm.issue_id]);
  });
  return { status: 204 };
}

// ---------------------------------------------------------------------------------------------------------------
// history

export async function listHistory(c: Ctx): Promise<Result> {
  const { issue } = await loadIssue(c);
  const { limit, offset } = pageParams(c.query);
  const r = await pool.query("SELECT * FROM history WHERE issue_id = $1 ORDER BY seq DESC LIMIT $2 OFFSET $3", [
    issue.id,
    limit + 1,
    offset,
  ]);
  const p = page(r.rows, limit, offset);
  return {
    status: 200,
    body: {
      items: p.items.map((h) => ({ id: h.id, actor_id: h.actor_id, created_at: iso(h.created_at), changes: h.changes })),
      next_cursor: p.next_cursor,
    },
  };
}

// ---------------------------------------------------------------------------------------------------------------
// listing and search

const TS_RE = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(\.\d+)?([Zz]|[+-](\d{2}):(\d{2}))$/;

function parseTs(s: string): string | null {
  const t = s.replace(" ", "+");
  const m = TS_RE.exec(t);
  if (!m) return null;
  const [y, mo, d, h, mi, se] = m.slice(1, 7).map(Number);
  if (mo < 1 || mo > 12 || d < 1 || d > new Date(Date.UTC(y, mo, 0)).getUTCDate()) return null;
  if (h > 23 || mi > 59 || se > 60) return null;
  if (m[9] !== undefined && (Number(m[9]) > 23 || Number(m[10]) > 59)) return null;
  return t;
}

const SORTS: Record<string, (dir: string) => string> = {
  created: (d) => `created_at ${d}, project_key ASC, number ASC`,
  updated: (d) => `updated_at ${d}, project_key ASC, number ASC`,
  priority: (d) => `priority_rank ${d}, project_key ASC, number ASC`,
  key: (d) => `project_key ${d}, number ${d}`,
};

export async function listIssues(c: Ctx): Promise<Result> {
  const org = await loadOrg(c.params[0], c.userId);
  const q = c.query;
  const errors: { field: string; code: string }[] = [];
  const where: string[] = ["org_id = $1"];
  const params: unknown[] = [org.id];
  const add = (sql: (p: string) => string, val: unknown) => {
    params.push(val);
    where.push(sql(`$${params.length}`));
  };
  const list = (name: string): string[] | null => {
    const raw = q.get(name);
    if (raw === null) return null;
    const vals = raw.split(",").map((s) => s.trim()).filter((s) => s !== "");
    if (!vals.length) {
      errors.push({ field: name, code: "invalid" });
      return null;
    }
    return vals;
  };
  const enumFilter = (name: string, col: string, allowed: readonly string[]) => {
    const vals = list(name);
    if (!vals) return;
    if (vals.some((x) => !allowed.includes(x))) errors.push({ field: name, code: "invalid" });
    else add((p) => `${col} = ANY(${p}::text[])`, vals);
  };

  const projects = list("project");
  if (projects) add((p) => `project_key = ANY(${p}::text[])`, projects.map((x) => x.toUpperCase()));
  enumFilter("status", "status", STATUSES);
  enumFilter("priority", "priority", PRIORITIES);
  enumFilter("type", "type", TYPES);

  const people = (name: string, col: string, allowNone: boolean) => {
    const vals = list(name);
    if (!vals) return;
    const ids: string[] = [];
    let none = false;
    for (const x of vals) {
      if (x === "me") ids.push(c.userId);
      else if (allowNone && x === "none") none = true;
      else if (UUID_RE.test(x)) ids.push(x.toLowerCase());
      else {
        errors.push({ field: name, code: "invalid" });
        return;
      }
    }
    params.push(ids);
    const p = `$${params.length}`;
    where.push(none ? `(${col} = ANY(${p}::uuid[]) OR ${col} IS NULL)` : `${col} = ANY(${p}::uuid[])`);
  };
  people("assignee", "assignee_id", true);
  people("reporter", "reporter_id", false);

  const labels = list("label");
  if (labels) add((p) => `labels && ${p}::text[]`, labels.map((x) => x.toLowerCase()));

  for (const [name, col, op] of [
    ["created_after", "created_at", ">"],
    ["created_before", "created_at", "<"],
    ["updated_after", "updated_at", ">"],
  ] as const) {
    const raw = q.get(name);
    if (raw === null) continue;
    const t = parseTs(raw);
    if (!t) errors.push({ field: name, code: "invalid" });
    else add((p) => `${col} ${op} ${p}::timestamptz`, t);
  }

  const text = q.get("q");
  if (text !== null) {
    const ws = [...new Set(words(text))];
    if (ws.length) add((p) => `words @> ${p}::text[]`, ws);
  }

  let order = SORTS.created("DESC");
  const sort = q.get("sort");
  if (sort !== null) {
    const desc = sort.startsWith("-");
    const fn = SORTS[desc ? sort.slice(1) : sort];
    if (!fn) errors.push({ field: "sort", code: "invalid" });
    else order = fn(desc ? "DESC" : "ASC");
  }

  let pg: { limit: number; offset: number } = { limit: 50, offset: 0 };
  try {
    pg = pageParams(q);
  } catch (e) {
    if (e instanceof ApiError && e.errors) errors.push(...e.errors);
    else throw e;
  }
  if (errors.length) throw validation(errors);

  const vis = await visibleProjects(org, c.userId);
  add((p) => `project_id = ANY(${p}::uuid[])`, [...vis.keys()]);
  const w = where.join(" AND ");
  const [rows, count] = await Promise.all([
    pool.query(`SELECT * FROM issues WHERE ${w} ORDER BY ${order} LIMIT ${pg.limit + 1} OFFSET ${pg.offset}`, params),
    pool.query(`SELECT count(*)::int AS n FROM issues WHERE ${w}`, params),
  ]);
  const p = page(rows.rows, pg.limit, pg.offset);
  return { status: 200, body: { total: count.rows[0].n, items: p.items.map(issueJson), next_cursor: p.next_cursor } };
}
