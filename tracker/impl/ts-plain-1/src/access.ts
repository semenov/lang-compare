import { pool, type Client } from "./db.js";
import { forbidden, notFound } from "./http.js";

export const ORG_ROLES = ["owner", "admin", "member"] as const;
export const PROJECT_ROLES = ["admin", "developer", "viewer"] as const;
export type OrgRole = (typeof ORG_ROLES)[number];
export type ProjectRole = (typeof PROJECT_ROLES)[number];

const PRANK: Record<string, number> = { viewer: 1, developer: 2, admin: 3 };

export interface OrgCtx {
  id: string;
  slug: string;
  name: string;
  created_at: Date;
  role: OrgRole;
}

export async function loadOrg(slug: string, userId: string, c: Client = pool): Promise<OrgCtx> {
  const r = await c.query(
    `SELECT o.*, m.role FROM orgs o JOIN org_members m ON m.org_id = o.id AND m.user_id = $2 WHERE o.slug = $1`,
    [slug, userId],
  );
  if (!r.rowCount) throw notFound("organization not found");
  return r.rows[0];
}

export function requireOrgAdmin(o: OrgCtx): void {
  if (o.role !== "owner" && o.role !== "admin") throw forbidden("org admin role required");
}

export function effectiveRole(
  orgRole: string | null | undefined,
  visibility: string,
  explicit: string | null | undefined,
): ProjectRole | null {
  let best = 0;
  if (explicit) best = PRANK[explicit];
  if (orgRole === "owner" || orgRole === "admin") best = Math.max(best, 3);
  else if (orgRole === "member" && visibility === "org") best = Math.max(best, 2);
  if (!orgRole) return null;
  return best === 0 ? null : (["", "viewer", "developer", "admin"][best] as ProjectRole);
}

export function atLeast(role: string | null, min: ProjectRole): boolean {
  return !!role && PRANK[role] >= PRANK[min];
}

export function requireRole(role: string | null, min: ProjectRole): void {
  if (!atLeast(role, min)) throw forbidden(`project role ${min} required`);
}

export interface ProjectCtx {
  id: string;
  org_id: string;
  key: string;
  name: string;
  description: string | null;
  visibility: string;
  created_at: Date;
  role: ProjectRole;
}

export async function loadProject(org: OrgCtx, key: string, userId: string, c: Client = pool): Promise<ProjectCtx> {
  const r = await c.query(
    `SELECT p.*, pm.role AS explicit_role FROM projects p
       LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $3
      WHERE p.org_id = $1 AND p.key = $2`,
    [org.id, key, userId],
  );
  if (!r.rowCount) throw notFound("project not found");
  const p = r.rows[0];
  const role = effectiveRole(org.role, p.visibility, p.explicit_role);
  if (!role) throw notFound("project not found");
  p.role = role;
  return p;
}

/** Effective role of an arbitrary user in a project (for assignee checks). */
export async function userProjectRole(projectId: string, userId: string, c: Client = pool): Promise<ProjectRole | null> {
  const r = await c.query(
    `SELECT p.visibility, om.role AS org_role, pm.role AS explicit_role
       FROM projects p
       LEFT JOIN org_members om ON om.org_id = p.org_id AND om.user_id = $2
       LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $2
      WHERE p.id = $1`,
    [projectId, userId],
  );
  if (!r.rowCount) return null;
  const x = r.rows[0];
  return effectiveRole(x.org_role, x.visibility, x.explicit_role);
}

/** Ids and roles of all projects in the org visible to the user. */
export async function visibleProjects(org: OrgCtx, userId: string): Promise<Map<string, { key: string; role: ProjectRole }>> {
  const r = await pool.query(
    `SELECT p.id, p.key, p.visibility, pm.role AS explicit_role FROM projects p
       LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $2
      WHERE p.org_id = $1`,
    [org.id, userId],
  );
  const m = new Map<string, { key: string; role: ProjectRole }>();
  for (const p of r.rows) {
    const role = effectiveRole(org.role, p.visibility, p.explicit_role);
    if (role) m.set(p.id, { key: p.key, role });
  }
  return m;
}
