use std::sync::LazyLock;

use serde::Serialize;
use uuid::Uuid;

use crate::issues::{issue_ctx, issue_from_row, Issue, ISSUE_COLS};
use crate::util::*;
use crate::webhooks;

#[derive(Serialize)]
pub struct CommentOut {
    pub id: Uuid,
    pub issue_key: String,
    pub author_id: Uuid,
    pub body: String,
    pub created_at: Ts,
    pub updated_at: Ts,
    pub edited: bool,
}

#[derive(Serialize)]
struct CommentData<'a> {
    issue: &'a Issue,
    comment: &'a CommentOut,
}

fn body_field(c: &Ctx) -> R<String> {
    let m = parse_obj(&c.body)?;
    let mut e = Errs::default();
    let b = v_str(&mut e, "body", field(&m, "body"), false, 1, 20000);
    if let Some(s) = &b {
        if s.trim().is_empty() {
            e.add("body", "required");
        }
    }
    e.check()?;
    Ok(b.unwrap())
}

pub async fn create(c: &Ctx, slug: &str, ik: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let mut client = c.app.pool.get().await?;
    let ic = issue_ctx(&client, slug, ik, c.uid, false).await?;
    drop(m);
    let body = body_field(c)?;
    let now = now();
    let com = CommentOut {
        id: Uuid::now_v7(),
        issue_key: ic.issue.key.clone(),
        author_id: c.uid,
        body,
        created_at: Ts(now),
        updated_at: Ts(now),
        edited: false,
    };
    static UPD: LazyLock<String> = LazyLock::new(|| {
        format!("UPDATE issues i SET comment_count = comment_count + 1 WHERE id = $1 RETURNING {ISSUE_COLS}")
    });
    let tx = client.transaction().await?;
    let (_, row, hooks) = tokio::try_join!(
        async {
            exec(
                &tx,
                "INSERT INTO comments (id, issue_id, author_id, body, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5)",
                &[&com.id, &ic.issue.id, &c.uid, &com.body, &now],
            )
            .await
            .map_err(ApiError::from)
        },
        async { qopt(&tx, &UPD, &[&ic.issue.id]).await.map_err(ApiError::from) },
        webhooks::hooks_for(&tx, ic.org_id, "comment.created"),
    )?;
    let Some(row) = row else { return Err(not_found()) };
    if !hooks.is_empty() {
        let issue = issue_from_row(&row, 0);
        let payload =
            webhooks::payload("comment.created", now, slug, c.uid, &CommentData { issue: &issue, comment: &com });
        webhooks::insert_deliveries(&tx, &hooks, &[(issue.id, payload)], "comment.created", now).await?;
    }
    tx.commit().await?;
    if !hooks.is_empty() {
        c.app.notify.notify_one();
    }
    Ok(json(201, &com))
}

pub async fn list(c: &Ctx, slug: &str, ik: &str) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let cur: i64 = match cursor {
        None => 0,
        Some(s) => s.parse().map_err(|_| vfail1("cursor", "invalid"))?,
    };
    let client = c.app.pool.get().await?;
    let ic = issue_ctx(&client, slug, ik, c.uid, false).await?;
    let rows = q(
        &client,
        "SELECT id, author_id, body, created_at, updated_at, edited, seq FROM comments \
         WHERE issue_id = $1 AND seq > $2 ORDER BY seq LIMIT $3",
        &[&ic.issue.id, &cur, &(limit + 1)],
    )
    .await?;
    let more = rows.len() as i64 > limit;
    let rows = &rows[..rows.len().min(limit as usize)];
    let next = if more { Some(enc_cursor(&rows.last().unwrap().get::<_, i64>(6).to_string())) } else { None };
    let items: Vec<CommentOut> = rows
        .iter()
        .map(|r| CommentOut {
            id: r.get(0),
            issue_key: ic.issue.key.clone(),
            author_id: r.get(1),
            body: r.get(2),
            created_at: Ts(r.get(3)),
            updated_at: Ts(r.get(4)),
            edited: r.get(5),
        })
        .collect();
    Ok(json(200, &Page { items, next_cursor: next }))
}

struct CommentCtx {
    out: CommentOut,
    issue_id: Uuid,
    role: i16,
}

async fn comment_ctx(client: &deadpool_postgres::Client, slug: &str, id: &str, uid: Uuid) -> R<CommentCtx> {
    let cid = parse_uuid(id).ok_or_else(not_found)?;
    let r = qopt(
        client,
        "SELECT c.id, c.author_id, c.body, c.created_at, c.updated_at, c.edited, i.id, i.project_key, i.number, \
           p.visibility, om.role, pm.role \
         FROM comments c JOIN issues i ON i.id = c.issue_id JOIN projects p ON p.id = i.project_id \
         JOIN orgs o ON o.id = i.org_id \
         JOIN org_members om ON om.org_id = o.id AND om.user_id = $2 \
         LEFT JOIN project_members pm ON pm.project_id = i.project_id AND pm.user_id = $2 \
         WHERE c.id = $3 AND o.slug = $1",
        &[&slug, &uid, &cid],
    )
    .await?
    .ok_or_else(not_found)?;
    let vis: i16 = r.get(9);
    let role = eff_role(r.get(10), vis == 1, r.get(11));
    if role == 0 {
        return Err(not_found());
    }
    let pk: String = r.get(7);
    let num: i32 = r.get(8);
    Ok(CommentCtx {
        out: CommentOut {
            id: r.get(0),
            issue_key: format!("{pk}-{num}"),
            author_id: r.get(1),
            body: r.get(2),
            created_at: Ts(r.get(3)),
            updated_at: Ts(r.get(4)),
            edited: r.get(5),
        },
        issue_id: r.get(6),
        role,
    })
}

pub async fn patch(c: &Ctx, slug: &str, id: &str) -> R<Resp> {
    let _ = parse_obj(&c.body)?;
    let client = c.app.pool.get().await?;
    let mut cc = comment_ctx(&client, slug, id, c.uid).await?;
    if cc.out.author_id != c.uid {
        return Err(forbidden());
    }
    let body = body_field(c)?;
    let now = now();
    exec(
        &client,
        "UPDATE comments SET body = $2, updated_at = $3, edited = true WHERE id = $1",
        &[&cc.out.id, &body, &now],
    )
    .await?;
    cc.out.body = body;
    cc.out.updated_at = Ts(now);
    cc.out.edited = true;
    Ok(json(200, &cc.out))
}

pub async fn delete(c: &Ctx, slug: &str, id: &str) -> R<Resp> {
    let mut client = c.app.pool.get().await?;
    let cc = comment_ctx(&client, slug, id, c.uid).await?;
    if cc.out.author_id != c.uid && cc.role < P_ADMIN {
        return Err(forbidden());
    }
    let tx = client.transaction().await?;
    let n = exec(&tx, "DELETE FROM comments WHERE id = $1", &[&cc.out.id]).await?;
    if n == 0 {
        return Err(not_found());
    }
    exec(&tx, "UPDATE issues SET comment_count = comment_count - 1 WHERE id = $1", &[&cc.issue_id]).await?;
    tx.commit().await?;
    Ok(no_content())
}
