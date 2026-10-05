use std::collections::HashSet;

use chrono::{DateTime, Utc};
use deadpool_postgres::GenericClient;
use serde::Serialize;
use uuid::Uuid;

use crate::util::*;

pub struct OrgCtx {
    pub id: Uuid,
    pub role: i16,
}

pub async fn org_ctx<C: GenericClient>(c: &C, slug: &str, uid: Uuid) -> R<OrgCtx> {
    let r = qopt(
        c,
        "SELECT o.id, m.role FROM orgs o JOIN org_members m ON m.org_id = o.id AND m.user_id = $2 WHERE o.slug = $1",
        &[&slug, &uid],
    )
    .await?
    .ok_or_else(not_found)?;
    Ok(OrgCtx { id: r.get(0), role: r.get(1) })
}

pub struct ProjCtx {
    pub org_id: Uuid,
    pub org_role: i16,
    pub id: Uuid,
    pub key: String,
    pub name: String,
    pub description: Option<String>,
    pub private: bool,
    pub created_at: DateTime<Utc>,
    pub role: i16,
}

#[derive(Serialize)]
pub struct ProjectOut<'a> {
    pub id: Uuid,
    pub key: &'a str,
    pub name: &'a str,
    pub description: Option<&'a str>,
    pub visibility: &'static str,
    pub created_at: Ts,
    pub my_role: &'static str,
}

impl ProjCtx {
    pub fn out(&self) -> ProjectOut<'_> {
        ProjectOut {
            id: self.id,
            key: &self.key,
            name: &self.name,
            description: self.description.as_deref(),
            visibility: if self.private { "private" } else { "org" },
            created_at: Ts(self.created_at),
            my_role: proj_role_str(self.role),
        }
    }
}

pub async fn proj_ctx<C: GenericClient>(c: &C, slug: &str, key: &str, uid: Uuid) -> R<ProjCtx> {
    let r = qopt(
        c,
        "SELECT o.id, om.role, p.id, p.key, p.name, p.description, p.visibility, p.created_at, pm.role \
         FROM orgs o JOIN org_members om ON om.org_id = o.id AND om.user_id = $2 \
         JOIN projects p ON p.org_id = o.id AND p.key = $3 \
         LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $2 \
         WHERE o.slug = $1",
        &[&slug, &uid, &key],
    )
    .await?
    .ok_or_else(not_found)?;
    let org_role: i16 = r.get(1);
    let vis: i16 = r.get(6);
    let explicit: Option<i16> = r.get(8);
    let role = eff_role(org_role, vis == 1, explicit);
    if role == 0 {
        return Err(not_found());
    }
    Ok(ProjCtx {
        org_id: r.get(0),
        org_role,
        id: r.get(2),
        key: r.get(3),
        name: r.get(4),
        description: r.get(5),
        private: vis == 1,
        created_at: r.get(7),
        role,
    })
}

/// Users among `ids` whose effective role in the project is at least developer.
pub async fn eligible_assignees<C: GenericClient>(
    c: &C,
    org_id: Uuid,
    project_id: Uuid,
    private: bool,
    ids: &[Uuid],
) -> R<HashSet<Uuid>> {
    if ids.is_empty() {
        return Ok(HashSet::new());
    }
    let ids: Vec<Uuid> = ids.to_vec();
    let rows = q(
        c,
        "SELECT om.user_id, om.role, pm.role FROM org_members om \
         LEFT JOIN project_members pm ON pm.project_id = $2 AND pm.user_id = om.user_id \
         WHERE om.org_id = $1 AND om.user_id = ANY($3)",
        &[&org_id, &project_id, &ids],
    )
    .await?;
    Ok(rows
        .iter()
        .filter(|r| eff_role(r.get(1), private, r.get(2)) >= P_DEV)
        .map(|r| r.get::<_, Uuid>(0))
        .collect())
}
