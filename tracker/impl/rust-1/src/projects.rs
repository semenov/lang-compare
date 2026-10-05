use serde_json::Value;
use uuid::Uuid;

use crate::access::{org_ctx, proj_ctx, ProjectOut};
use crate::orgs::MemberOut;
use crate::util::*;

fn parse_visibility(e: &mut Errs, f: F) -> Option<Option<bool>> {
    match f {
        F::Absent => Some(None),
        F::Val(Value::String(s)) if s == "org" => Some(Some(false)),
        F::Val(Value::String(s)) if s == "private" => Some(Some(true)),
        _ => {
            e.add("visibility", "invalid");
            None
        }
    }
}

pub async fn create(c: &Ctx, slug: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let mut client = c.app.pool.get().await?;
    let o = org_ctx(&client, slug, c.uid).await?;
    let mut e = Errs::default();
    let key = match field(&m, "key") {
        F::Absent | F::Null => {
            e.add("key", "required");
            None
        }
        F::Val(Value::String(s)) if valid_project_key(s) => Some(s.clone()),
        F::Val(_) => {
            e.add("key", "invalid");
            None
        }
    };
    let name = v_str(&mut e, "name", field(&m, "name"), true, 1, 100);
    let desc = v_opt_text(&mut e, "description", field(&m, "description"), 65536).ok().flatten().flatten();
    let vis = parse_visibility(&mut e, field(&m, "visibility"));
    e.check()?;
    let (key, name, private) = (key.unwrap(), name.unwrap(), vis.unwrap().unwrap_or(false));
    let id = Uuid::now_v7();
    let now = now();
    let tx = client.transaction().await?;
    match exec(
        &tx,
        "INSERT INTO projects (id, org_id, key, name, description, visibility, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)",
        &[&id, &o.id, &key, &name, &desc, &(private as i16), &now],
    )
    .await
    {
        Ok(_) => {}
        Err(e) if is_unique_violation(&e) => return Err(conflict("key_taken", "project key already taken")),
        Err(e) => return Err(e.into()),
    }
    exec(&tx, "INSERT INTO project_members (project_id, user_id, role) VALUES ($1, $2, 3)", &[&id, &c.uid]).await?;
    tx.commit().await?;
    Ok(json(
        201,
        &ProjectOut {
            id,
            key: &key,
            name: &name,
            description: desc.as_deref(),
            visibility: if private { "private" } else { "org" },
            created_at: Ts(now),
            my_role: "admin",
        },
    ))
}

pub async fn list(c: &Ctx, slug: &str) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let cursor = cursor.unwrap_or_default();
    let client = c.app.pool.get().await?;
    let o = org_ctx(&client, slug, c.uid).await?;
    let rows = q(
        &client,
        "SELECT p.id, p.key, p.name, p.description, p.visibility, p.created_at, pm.role FROM projects p \
         LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $2 \
         WHERE p.org_id = $1 AND p.key > $3 AND ($4::int2 >= 2 OR p.visibility = 0 OR pm.role IS NOT NULL) \
         ORDER BY p.key LIMIT $5",
        &[&o.id, &c.uid, &cursor, &o.role, &(limit + 1)],
    )
    .await?;
    let mut items: Vec<ProjectOut> = rows
        .iter()
        .map(|r| {
            let vis: i16 = r.get(4);
            ProjectOut {
                id: r.get(0),
                key: r.get(1),
                name: r.get(2),
                description: r.get(3),
                visibility: if vis == 1 { "private" } else { "org" },
                created_at: Ts(r.get(5)),
                my_role: proj_role_str(eff_role(o.role, vis == 1, r.get(6))),
            }
        })
        .collect();
    let next = if items.len() as i64 > limit {
        items.truncate(limit as usize);
        Some(enc_cursor(items.last().unwrap().key))
    } else {
        None
    };
    Ok(json(200, &Page { items, next_cursor: next }))
}

pub async fn get(c: &Ctx, slug: &str, key: &str) -> R<Resp> {
    let client = c.app.pool.get().await?;
    let p = proj_ctx(&client, slug, key, c.uid).await?;
    Ok(json(200, &p.out()))
}

pub async fn patch(c: &Ctx, slug: &str, key: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let client = c.app.pool.get().await?;
    let mut p = proj_ctx(&client, slug, key, c.uid).await?;
    if p.role < P_ADMIN {
        return Err(forbidden());
    }
    let mut e = Errs::default();
    let name = match field(&m, "name") {
        F::Absent => None,
        f => v_str(&mut e, "name", f, true, 1, 100),
    };
    let desc = v_opt_text(&mut e, "description", field(&m, "description"), 65536).unwrap_or(None);
    let vis = parse_visibility(&mut e, field(&m, "visibility"));
    e.check()?;
    if let Some(n) = name {
        p.name = n;
    }
    if let Some(d) = desc {
        p.description = d;
    }
    if let Some(Some(v)) = vis {
        p.private = v;
    }
    exec(
        &client,
        "UPDATE projects SET name = $2, description = $3, visibility = $4 WHERE id = $1",
        &[&p.id, &p.name, &p.description, &(p.private as i16)],
    )
    .await?;
    let explicit: Option<i16> =
        qopt(&client, "SELECT role FROM project_members WHERE project_id = $1 AND user_id = $2", &[&p.id, &c.uid])
            .await?
            .map(|r| r.get(0));
    p.role = eff_role(p.org_role, p.private, explicit);
    Ok(json(200, &p.out()))
}

pub async fn delete(c: &Ctx, slug: &str, key: &str) -> R<Resp> {
    let client = c.app.pool.get().await?;
    let p = proj_ctx(&client, slug, key, c.uid).await?;
    if p.role < P_ADMIN {
        return Err(forbidden());
    }
    exec(&client, "DELETE FROM projects WHERE id = $1", &[&p.id]).await?;
    Ok(no_content())
}

pub async fn members(c: &Ctx, slug: &str, key: &str) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let cursor = cursor.unwrap_or_default();
    let client = c.app.pool.get().await?;
    let p = proj_ctx(&client, slug, key, c.uid).await?;
    let rows = q(
        &client,
        "SELECT u.id, u.email, u.name, m.role FROM project_members m JOIN users u ON u.id = m.user_id \
         WHERE m.project_id = $1 AND u.email > $2 ORDER BY u.email LIMIT $3",
        &[&p.id, &cursor, &(limit + 1)],
    )
    .await?;
    let mut items: Vec<MemberOut> = rows
        .iter()
        .map(|r| MemberOut { user_id: r.get(0), email: r.get(1), name: r.get(2), role: proj_role_str(r.get(3)) })
        .collect();
    let next = if items.len() as i64 > limit {
        items.truncate(limit as usize);
        Some(enc_cursor(&items.last().unwrap().email))
    } else {
        None
    };
    Ok(json(200, &Page { items, next_cursor: next }))
}

pub async fn put_member(c: &Ctx, slug: &str, key: &str, id: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let client = c.app.pool.get().await?;
    let p = proj_ctx(&client, slug, key, c.uid).await?;
    if p.role < P_ADMIN {
        return Err(forbidden());
    }
    let mut e = Errs::default();
    let role = match field(&m, "role") {
        F::Val(Value::String(s)) => proj_role_parse(s),
        _ => None,
    };
    if role.is_none() {
        e.add("role", "invalid");
    }
    e.check()?;
    let role = role.unwrap();
    let target = parse_uuid(id).ok_or_else(|| vfail1("user_id", "invalid"))?;
    let u = qopt(
        &client,
        "SELECT u.email, u.name FROM org_members m JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 AND m.user_id = $2",
        &[&p.org_id, &target],
    )
    .await?
    .ok_or_else(|| vfail1("user_id", "invalid"))?;
    exec(
        &client,
        "INSERT INTO project_members (project_id, user_id, role) VALUES ($1, $2, $3) \
         ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role",
        &[&p.id, &target, &role],
    )
    .await?;
    Ok(json(200, &MemberOut { user_id: target, email: u.get(0), name: u.get(1), role: proj_role_str(role) }))
}

pub async fn delete_member(c: &Ctx, slug: &str, key: &str, id: &str) -> R<Resp> {
    let client = c.app.pool.get().await?;
    let p = proj_ctx(&client, slug, key, c.uid).await?;
    if p.role < P_ADMIN {
        return Err(forbidden());
    }
    let target = parse_uuid(id).ok_or_else(not_found)?;
    let n = exec(&client, "DELETE FROM project_members WHERE project_id = $1 AND user_id = $2", &[&p.id, &target]).await?;
    if n == 0 {
        return Err(not_found());
    }
    Ok(no_content())
}
