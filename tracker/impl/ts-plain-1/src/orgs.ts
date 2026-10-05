import crypto from "node:crypto";
import { pool, tx } from "./db.js";
import {
  ApiError,
  type Ctx,
  type Result,
  Validator,
  conflict,
  fieldError,
  forbidden,
  iso,
  notFound,
  optEnum,
  optStr,
  page,
  pageParams,
  reqStr,
  UUID_RE,
} from "./http.js";
import {
  ORG_ROLES,
  PROJECT_ROLES,
  type OrgCtx,
  loadOrg,
  loadProject,
  requireOrgAdmin,
  requireRole,
  effectiveRole,
} from "./access.js";

const SLUG_RE = /^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$/;
const KEY_RE = /^[A-Z][A-Z0-9]{1,9}$/;

const orgJson = (o: any, role: string) => ({
  id: o.id,
  slug: o.slug,
  name: o.name,
  created_at: iso(o.created_at),
  my_role: role,
});

const memberJson = (m: any) => ({ user_id: m.user_id, email: m.email, name: m.name, role: m.role });

// ---------------------------------------------------------------------------------------------------------------
// orgs

export async function createOrg(c: Ctx): Promise<Result> {
  const b = c.body();
  const v = new Validator();
  const name = reqStr(v, b, "name", { min: 1, max: 100, trim: true });
  const slug = reqStr(v, b, "slug", { max: 1000 });
  if (slug !== undefined && !SLUG_RE.test(slug)) v.add("slug", "invalid");
  v.throwIfAny();
  const id = crypto.randomUUID();
  try {
    const o = await tx(async (cl) => {
      const r = await cl.query("INSERT INTO orgs (id, slug, name, created_at) VALUES ($1,$2,$3,$4) RETURNING *", [
        id,
        slug,
        name,
        new Date(),
      ]);
      await cl.query("INSERT INTO org_members (org_id, user_id, role) VALUES ($1,$2,'owner')", [id, c.userId]);
      return r.rows[0];
    });
    return { status: 201, body: orgJson(o, "owner") };
  } catch (e: any) {
    if (e.code === "23505") throw conflict("slug_taken", "slug already taken");
    throw e;
  }
}

export async function listOrgs(c: Ctx): Promise<Result> {
  const { limit, offset } = pageParams(c.query);
  const r = await pool.query(
    `SELECT o.*, m.role FROM orgs o JOIN org_members m ON m.org_id = o.id WHERE m.user_id = $1
      ORDER BY o.slug LIMIT $2 OFFSET $3`,
    [c.userId, limit + 1, offset],
  );
  const p = page(r.rows, limit, offset);
  return { status: 200, body: { items: p.items.map((o) => orgJson(o, o.role)), next_cursor: p.next_cursor } };
}

export async function getOrg(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  return { status: 200, body: orgJson(o, o.role) };
}

// ---------------------------------------------------------------------------------------------------------------
// org members

export async function listMembers(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const { limit, offset } = pageParams(c.query);
  const r = await pool.query(
    `SELECT m.user_id, u.email, u.name, m.role FROM org_members m JOIN users u ON u.id = m.user_id
      WHERE m.org_id = $1 ORDER BY u.email LIMIT $2 OFFSET $3`,
    [o.id, limit + 1, offset],
  );
  const p = page(r.rows, limit, offset);
  return { status: 200, body: { items: p.items.map(memberJson), next_cursor: p.next_cursor } };
}

export async function addMember(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  requireOrgAdmin(o);
  const b = c.body();
  const v = new Validator();
  const email = reqStr(v, b, "email", { min: 1, max: 1000, trim: true });
  const role = optEnum(v, b, "role", ORG_ROLES, true);
  v.throwIfAny();
  if ((role === "owner" || role === "admin") && o.role !== "owner") {
    throw forbidden("only owners may grant owner or admin");
  }
  const u = await pool.query("SELECT id, email, name FROM users WHERE email = $1", [email!.toLowerCase()]);
  if (!u.rowCount) throw fieldError("email", "invalid");
  const user = u.rows[0];
  const r = await pool.query(
    "INSERT INTO org_members (org_id, user_id, role) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING",
    [o.id, user.id, role],
  );
  if (!r.rowCount) throw conflict("already_member", "user is already a member");
  return { status: 201, body: { user_id: user.id, email: user.email, name: user.name, role } };
}

const isPrivileged = (r: string) => r === "owner" || r === "admin";

export async function patchMember(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  requireOrgAdmin(o);
  const b = c.body();
  const v = new Validator();
  const role = optEnum(v, b, "role", ORG_ROLES, true);
  v.throwIfAny();
  const targetId = c.params[1];
  if (!UUID_RE.test(targetId)) throw notFound("member not found");
  const out = await tx(async (cl) => {
    await cl.query("SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE", [o.id]);
    const t = await cl.query(
      `SELECT m.user_id, u.email, u.name, m.role FROM org_members m JOIN users u ON u.id = m.user_id
        WHERE m.org_id = $1 AND m.user_id = $2`,
      [o.id, targetId],
    );
    if (!t.rowCount) throw notFound("member not found");
    const cur = t.rows[0];
    if (o.role !== "owner" && (isPrivileged(cur.role) || isPrivileged(role!))) {
      throw forbidden("only owners may grant or take away owner or admin");
    }
    if (cur.role === "owner" && role !== "owner") {
      const n = await cl.query("SELECT count(*)::int AS n FROM org_members WHERE org_id = $1 AND role = 'owner'", [
        o.id,
      ]);
      if (n.rows[0].n <= 1) throw conflict("last_owner", "cannot demote the last owner");
    }
    await cl.query("UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2", [o.id, targetId, role]);
    return { ...cur, role };
  });
  return { status: 200, body: memberJson(out) };
}

export async function deleteMember(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const targetId = c.params[1];
  const self = targetId === c.userId;
  if (!self) requireOrgAdmin(o);
  if (!UUID_RE.test(targetId)) throw notFound("member not found");
  await tx(async (cl) => {
    await cl.query("SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE", [o.id]);
    const t = await cl.query("SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2", [o.id, targetId]);
    if (!t.rowCount) throw notFound("member not found");
    const cur = t.rows[0].role;
    if (!self && o.role !== "owner" && isPrivileged(cur)) {
      throw forbidden("only owners may remove owners or admins");
    }
    if (cur === "owner") {
      const n = await cl.query("SELECT count(*)::int AS n FROM org_members WHERE org_id = $1 AND role = 'owner'", [
        o.id,
      ]);
      if (n.rows[0].n <= 1) throw conflict("last_owner", "cannot remove the last owner");
    }
    await cl.query("DELETE FROM org_members WHERE org_id = $1 AND user_id = $2", [o.id, targetId]);
    await cl.query(
      `DELETE FROM project_members WHERE user_id = $2 AND project_id IN (SELECT id FROM projects WHERE org_id = $1)`,
      [o.id, targetId],
    );
    await cl.query("UPDATE issues SET assignee_id = NULL WHERE org_id = $1 AND assignee_id = $2", [o.id, targetId]);
  });
  return { status: 204 };
}

// ---------------------------------------------------------------------------------------------------------------
// projects

const projectJson = (p: any, role: string) => ({
  id: p.id,
  key: p.key,
  name: p.name,
  description: p.description ?? null,
  visibility: p.visibility,
  created_at: iso(p.created_at),
  my_role: role,
});

const VISIBILITIES = ["org", "private"] as const;

export async function createProject(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const b = c.body();
  const v = new Validator();
  const key = reqStr(v, b, "key", { max: 1000 });
  if (key !== undefined && !KEY_RE.test(key)) v.add("key", "invalid");
  const name = reqStr(v, b, "name", { min: 1, max: 100, trim: true });
  const description = optStr(v, b, "description", { max: 65536, nullable: true });
  const visibility = optEnum(v, b, "visibility", VISIBILITIES) ?? "org";
  v.throwIfAny();
  const id = crypto.randomUUID();
  try {
    const p = await tx(async (cl) => {
      const r = await cl.query(
        `INSERT INTO projects (id, org_id, key, name, description, visibility, created_at)
         VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *`,
        [id, o.id, key, name, description ?? null, visibility, new Date()],
      );
      await cl.query("INSERT INTO project_members (project_id, user_id, role) VALUES ($1,$2,'admin')", [id, c.userId]);
      return r.rows[0];
    });
    return { status: 201, body: projectJson(p, "admin") };
  } catch (e: any) {
    if (e.code === "23505") throw conflict("key_taken", "project key already taken");
    throw e;
  }
}

export async function listProjects(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const { limit, offset } = pageParams(c.query);
  const privileged = isPrivileged(o.role);
  const r = await pool.query(
    `SELECT p.*, pm.role AS explicit_role FROM projects p
       LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $2
      WHERE p.org_id = $1 AND ($3 OR p.visibility = 'org' OR pm.role IS NOT NULL)
      ORDER BY p.key LIMIT $4 OFFSET $5`,
    [o.id, c.userId, privileged, limit + 1, offset],
  );
  const pg = page(r.rows, limit, offset);
  return {
    status: 200,
    body: {
      items: pg.items.map((p) => projectJson(p, effectiveRole(o.role, p.visibility, p.explicit_role)!)),
      next_cursor: pg.next_cursor,
    },
  };
}

export async function getProject(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const p = await loadProject(o, c.params[1], c.userId);
  return { status: 200, body: projectJson(p, p.role) };
}

export async function patchProject(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const p = await loadProject(o, c.params[1], c.userId);
  requireRole(p.role, "admin");
  const b = c.body();
  const v = new Validator();
  const name = optStr(v, b, "name", { min: 1, max: 100, trim: true });
  const description = optStr(v, b, "description", { max: 65536, nullable: true });
  const visibility = optEnum(v, b, "visibility", VISIBILITIES);
  v.throwIfAny();
  const r = await pool.query(
    `UPDATE projects SET name = COALESCE($2, name),
            description = CASE WHEN $3 THEN $4 ELSE description END,
            visibility = COALESCE($5, visibility)
      WHERE id = $1 RETURNING *`,
    [p.id, name ?? null, description !== undefined, description ?? null, visibility ?? null],
  );
  const np = await loadProject(o, p.key, c.userId);
  return { status: 200, body: projectJson(r.rows[0], np.role) };
}

export async function deleteProject(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const p = await loadProject(o, c.params[1], c.userId);
  requireRole(p.role, "admin");
  await pool.query("DELETE FROM projects WHERE id = $1", [p.id]);
  return { status: 204 };
}

export async function listProjectMembers(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const p = await loadProject(o, c.params[1], c.userId);
  const { limit, offset } = pageParams(c.query);
  const r = await pool.query(
    `SELECT m.user_id, u.email, u.name, m.role FROM project_members m JOIN users u ON u.id = m.user_id
      WHERE m.project_id = $1 ORDER BY u.email LIMIT $2 OFFSET $3`,
    [p.id, limit + 1, offset],
  );
  const pg = page(r.rows, limit, offset);
  return { status: 200, body: { items: pg.items.map(memberJson), next_cursor: pg.next_cursor } };
}

export async function putProjectMember(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const p = await loadProject(o, c.params[1], c.userId);
  requireRole(p.role, "admin");
  const b = c.body();
  const v = new Validator();
  const role = optEnum(v, b, "role", PROJECT_ROLES, true);
  v.throwIfAny();
  const uid = c.params[2];
  if (!UUID_RE.test(uid)) throw fieldError("user_id", "invalid");
  const u = await pool.query(
    `SELECT u.id, u.email, u.name FROM users u JOIN org_members m ON m.user_id = u.id AND m.org_id = $1
      WHERE u.id = $2`,
    [o.id, uid],
  );
  if (!u.rowCount) throw fieldError("user_id", "invalid");
  await pool.query(
    `INSERT INTO project_members (project_id, user_id, role) VALUES ($1,$2,$3)
     ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
    [p.id, uid, role],
  );
  const x = u.rows[0];
  return { status: 200, body: { user_id: x.id, email: x.email, name: x.name, role } };
}

export async function deleteProjectMember(c: Ctx): Promise<Result> {
  const o = await loadOrg(c.params[0], c.userId);
  const p = await loadProject(o, c.params[1], c.userId);
  requireRole(p.role, "admin");
  const uid = c.params[2];
  if (!UUID_RE.test(uid)) throw notFound("member not found");
  const r = await pool.query("DELETE FROM project_members WHERE project_id = $1 AND user_id = $2", [p.id, uid]);
  if (!r.rowCount) throw notFound("member not found");
  return { status: 204 };
}

