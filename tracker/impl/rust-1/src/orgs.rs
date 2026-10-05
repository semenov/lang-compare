use serde::Serialize;
use serde_json::Value;
use uuid::Uuid;

use crate::access::org_ctx;
use crate::util::*;

#[derive(Serialize)]
struct OrgOut {
    id: Uuid,
    slug: String,
    name: String,
    created_at: Ts,
    my_role: &'static str,
}

#[derive(Serialize)]
pub struct MemberOut {
    pub user_id: Uuid,
    pub email: String,
    pub name: String,
    pub role: &'static str,
}

pub async fn create(c: &Ctx) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let mut e = Errs::default();
    let name = v_str(&mut e, "name", field(&m, "name"), true, 1, 100);
    let slug = match field(&m, "slug") {
        F::Absent | F::Null => {
            e.add("slug", "required");
            None
        }
        F::Val(Value::String(s)) if valid_slug(s) => Some(s.clone()),
        F::Val(_) => {
            e.add("slug", "invalid");
            None
        }
    };
    e.check()?;
    let (name, slug) = (name.unwrap(), slug.unwrap());
    let mut client = c.app.pool.get().await?;
    let tx = client.transaction().await?;
    let id = Uuid::now_v7();
    let now = now();
    match exec(&tx, "INSERT INTO orgs (id, slug, name, created_at) VALUES ($1, $2, $3, $4)", &[&id, &slug, &name, &now])
        .await
    {
        Ok(_) => {}
        Err(e) if is_unique_violation(&e) => return Err(conflict("slug_taken", "slug already taken")),
        Err(e) => return Err(e.into()),
    }
    exec(&tx, "INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 3)", &[&id, &c.uid]).await?;
    tx.commit().await?;
    Ok(json(201, &OrgOut { id, slug, name, created_at: Ts(now), my_role: "owner" }))
}

pub async fn list(c: &Ctx) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let cursor = cursor.unwrap_or_default();
    let client = c.app.pool.get().await?;
    let rows = q(
        &client,
        "SELECT o.id, o.slug, o.name, o.created_at, m.role FROM org_members m JOIN orgs o ON o.id = m.org_id \
         WHERE m.user_id = $1 AND o.slug > $2 ORDER BY o.slug LIMIT $3",
        &[&c.uid, &cursor, &(limit + 1)],
    )
    .await?;
    let mut items: Vec<OrgOut> = rows
        .iter()
        .map(|r| OrgOut {
            id: r.get(0),
            slug: r.get(1),
            name: r.get(2),
            created_at: Ts(r.get(3)),
            my_role: org_role_str(r.get(4)),
        })
        .collect();
    let next = if items.len() as i64 > limit {
        items.truncate(limit as usize);
        Some(enc_cursor(&items.last().unwrap().slug))
    } else {
        None
    };
    Ok(json(200, &Page { items, next_cursor: next }))
}

pub async fn get(c: &Ctx, slug: &str) -> R<Resp> {
    let client = c.app.pool.get().await?;
    let r = qopt(
        &client,
        "SELECT o.id, o.slug, o.name, o.created_at, m.role FROM orgs o \
         JOIN org_members m ON m.org_id = o.id AND m.user_id = $2 WHERE o.slug = $1",
        &[&slug, &c.uid],
    )
    .await?
    .ok_or_else(not_found)?;
    Ok(json(
        200,
        &OrgOut { id: r.get(0), slug: r.get(1), name: r.get(2), created_at: Ts(r.get(3)), my_role: org_role_str(r.get(4)) },
    ))
}

pub async fn members(c: &Ctx, slug: &str) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let cursor = cursor.unwrap_or_default();
    let client = c.app.pool.get().await?;
    let o = org_ctx(&client, slug, c.uid).await?;
    let rows = q(
        &client,
        "SELECT u.id, u.email, u.name, m.role FROM org_members m JOIN users u ON u.id = m.user_id \
         WHERE m.org_id = $1 AND u.email > $2 ORDER BY u.email LIMIT $3",
        &[&o.id, &cursor, &(limit + 1)],
    )
    .await?;
    let mut items: Vec<MemberOut> = rows
        .iter()
        .map(|r| MemberOut { user_id: r.get(0), email: r.get(1), name: r.get(2), role: org_role_str(r.get(3)) })
        .collect();
    let next = if items.len() as i64 > limit {
        items.truncate(limit as usize);
        Some(enc_cursor(&items.last().unwrap().email))
    } else {
        None
    };
    Ok(json(200, &Page { items, next_cursor: next }))
}

fn parse_role(e: &mut Errs, m: &serde_json::Map<String, Value>) -> Option<i16> {
    match field(m, "role") {
        F::Absent | F::Null => {
            e.add("role", "required");
            None
        }
        F::Val(Value::String(s)) => {
            let r = org_role_parse(s);
            if r.is_none() {
                e.add("role", "invalid");
            }
            r
        }
        F::Val(_) => {
            e.add("role", "invalid");
            None
        }
    }
}

pub async fn add_member(c: &Ctx, slug: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let client = c.app.pool.get().await?;
    let o = org_ctx(&client, slug, c.uid).await?;
    if o.role < ORG_ADMIN {
        return Err(forbidden());
    }
    let mut e = Errs::default();
    let email = v_str(&mut e, "email", field(&m, "email"), true, 1, 254);
    let role = parse_role(&mut e, &m);
    e.check()?;
    let role = role.unwrap();
    if role >= ORG_ADMIN && o.role < ORG_OWNER {
        return Err(forbidden());
    }
    let email = email.unwrap().to_lowercase();
    let u = qopt(&client, "SELECT id, email, name FROM users WHERE email = $1", &[&email])
        .await?
        .ok_or_else(|| vfail1("email", "invalid"))?;
    let uid: Uuid = u.get(0);
    let n = exec(
        &client,
        "INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
        &[&o.id, &uid, &role],
    )
    .await?;
    if n == 0 {
        return Err(conflict("already_member", "user is already a member"));
    }
    Ok(json(201, &MemberOut { user_id: uid, email: u.get(1), name: u.get(2), role: org_role_str(role) }))
}

pub async fn patch_member(c: &Ctx, slug: &str, id: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let mut client = c.app.pool.get().await?;
    let o = org_ctx(&client, slug, c.uid).await?;
    if o.role < ORG_ADMIN {
        return Err(forbidden());
    }
    let mut e = Errs::default();
    let role = parse_role(&mut e, &m);
    e.check()?;
    let role = role.unwrap();
    let target = parse_uuid(id).ok_or_else(not_found)?;
    let tx = client.transaction().await?;
    exec(&tx, "SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE", &[&o.id]).await?;
    let r = qopt(
        &tx,
        "SELECT m.role, u.email, u.name FROM org_members m JOIN users u ON u.id = m.user_id \
         WHERE m.org_id = $1 AND m.user_id = $2",
        &[&o.id, &target],
    )
    .await?
    .ok_or_else(not_found)?;
    let cur: i16 = r.get(0);
    if (cur >= ORG_ADMIN || role >= ORG_ADMIN) && o.role < ORG_OWNER {
        return Err(forbidden());
    }
    if cur == ORG_OWNER && role != ORG_OWNER {
        let owners: i64 =
            q1(&tx, "SELECT count(*) FROM org_members WHERE org_id = $1 AND role = 3", &[&o.id]).await?.get(0);
        if owners <= 1 {
            return Err(conflict("last_owner", "cannot remove the last owner"));
        }
    }
    if cur != role {
        exec(&tx, "UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2", &[&o.id, &target, &role]).await?;
    }
    tx.commit().await?;
    Ok(json(200, &MemberOut { user_id: target, email: r.get(1), name: r.get(2), role: org_role_str(role) }))
}

pub async fn delete_member(c: &Ctx, slug: &str, id: &str) -> R<Resp> {
    let mut client = c.app.pool.get().await?;
    let o = org_ctx(&client, slug, c.uid).await?;
    let target = parse_uuid(id).ok_or_else(not_found)?;
    let is_self = target == c.uid;
    if !is_self && o.role < ORG_ADMIN {
        return Err(forbidden());
    }
    let tx = client.transaction().await?;
    exec(&tx, "SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE", &[&o.id]).await?;
    let cur: i16 = qopt(&tx, "SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2", &[&o.id, &target])
        .await?
        .ok_or_else(not_found)?
        .get(0);
    if !is_self && cur >= ORG_ADMIN && o.role < ORG_OWNER {
        return Err(forbidden());
    }
    if cur == ORG_OWNER {
        let owners: i64 =
            q1(&tx, "SELECT count(*) FROM org_members WHERE org_id = $1 AND role = 3", &[&o.id]).await?.get(0);
        if owners <= 1 {
            return Err(conflict("last_owner", "cannot remove the last owner"));
        }
    }
    exec(&tx, "DELETE FROM org_members WHERE org_id = $1 AND user_id = $2", &[&o.id, &target]).await?;
    exec(
        &tx,
        "DELETE FROM project_members WHERE user_id = $2 AND project_id IN (SELECT id FROM projects WHERE org_id = $1)",
        &[&o.id, &target],
    )
    .await?;
    let now = now();
    let changes = serde_json::json!([{"field": "assignee_id", "from": target, "to": null}]).to_string();
    exec(
        &tx,
        "WITH u AS (UPDATE issues SET assignee_id = NULL, version = version + 1, updated_at = $3 \
                    WHERE org_id = $1 AND assignee_id = $2 RETURNING id) \
         INSERT INTO issue_history (id, issue_id, actor_id, created_at, changes) \
         SELECT gen_random_uuid(), u.id, $4, $3, $5 FROM u",
        &[&o.id, &target, &now, &c.uid, &changes],
    )
    .await?;
    tx.commit().await?;
    Ok(no_content())
}
