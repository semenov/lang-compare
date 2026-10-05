use std::{sync::Arc, time::Duration};

use bytes::Bytes;
use chrono::{DateTime, Utc};
use deadpool_postgres::GenericClient;
use hmac::{Hmac, Mac};
use http_body_util::{BodyExt, Full};
use hyper::Request;
use serde::Serialize;
use serde_json::Value;
use sha2::Sha256;
use uuid::Uuid;

use crate::access::org_ctx;
use crate::util::*;
use crate::App;

pub const EVENTS: [&str; 4] = ["issue.created", "issue.updated", "issue.deleted", "comment.created"];
const MAX_ATTEMPTS: i32 = 6;

// ---------------------------------------------------------------------------------------------
// event recording (inside the caller's transaction)

pub async fn hooks_for<C: GenericClient>(c: &C, org_id: Uuid, event: &str) -> R<Vec<Uuid>> {
    let rows = q(c, "SELECT id FROM webhooks WHERE org_id = $1 AND active AND $2 = ANY(events)", &[&org_id, &event]).await?;
    Ok(rows.iter().map(|r| r.get(0)).collect())
}

#[derive(Serialize)]
struct EventBody<'a, D: Serialize> {
    event: &'a str,
    created_at: Ts,
    org: &'a str,
    actor_id: Uuid,
    data: D,
}

/// The event body without its leading `{` and the delivery id; the id is prepended at send time.
pub fn payload<D: Serialize>(event: &str, now: DateTime<Utc>, org: &str, actor: Uuid, data: D) -> String {
    let s = serde_json::to_string(&EventBody { event, created_at: Ts(now), org, actor_id: actor, data }).unwrap();
    s[1..].to_string()
}

pub async fn insert_deliveries<C: GenericClient>(
    c: &C,
    hooks: &[Uuid],
    items: &[(Uuid, String)],
    event: &str,
    now: DateTime<Utc>,
) -> R<()> {
    let n = hooks.len() * items.len();
    let mut ids = Vec::with_capacity(n);
    let mut whs = Vec::with_capacity(n);
    let mut iids = Vec::with_capacity(n);
    let mut pls: Vec<&str> = Vec::with_capacity(n);
    for h in hooks {
        for (iid, p) in items {
            ids.push(Uuid::now_v7());
            whs.push(*h);
            iids.push(*iid);
            pls.push(p);
        }
    }
    exec(
        c,
        "INSERT INTO deliveries (id, webhook_id, issue_id, event, payload, created_at) \
         SELECT x.id, x.wh, x.iid, $5, x.p, $6 FROM unnest($1::uuid[], $2::uuid[], $3::uuid[], $4::text[]) AS x(id, wh, iid, p)",
        &[&ids, &whs, &iids, &pls, &event, &now],
    )
    .await?;
    Ok(())
}

// ---------------------------------------------------------------------------------------------
// CRUD

#[derive(Serialize)]
struct HookOut {
    id: Uuid,
    url: String,
    events: Vec<String>,
    active: bool,
    created_at: Ts,
    #[serde(skip_serializing_if = "Option::is_none")]
    secret: Option<String>,
}

fn valid_url(s: &str) -> bool {
    if s.len() > 2048 {
        return false;
    }
    match s.parse::<hyper::Uri>() {
        Ok(u) => matches!(u.scheme_str(), Some("http") | Some("https")) && u.host().is_some_and(|h| !h.is_empty()),
        Err(_) => false,
    }
}

fn parse_url(e: &mut Errs, f: F) -> Option<String> {
    match f {
        F::Absent | F::Null => {
            e.add("url", "required");
            None
        }
        F::Val(Value::String(s)) if valid_url(s) => Some(s.clone()),
        F::Val(_) => {
            e.add("url", "invalid");
            None
        }
    }
}

fn parse_events(e: &mut Errs, f: F) -> Option<Vec<String>> {
    match f {
        F::Absent | F::Null => {
            e.add("events", "required");
            None
        }
        F::Val(Value::Array(a)) => {
            if a.is_empty() {
                e.add("events", "too_short");
                return None;
            }
            let mut out: Vec<String> = Vec::new();
            for v in a {
                match v {
                    Value::String(s) if EVENTS.contains(&s.as_str()) => {
                        if !out.contains(s) {
                            out.push(s.clone());
                        }
                    }
                    _ => {
                        e.add("events", "invalid");
                        return None;
                    }
                }
            }
            Some(out)
        }
        F::Val(_) => {
            e.add("events", "invalid");
            None
        }
    }
}

async fn admin_org(c: &Ctx, client: &deadpool_postgres::Client, slug: &str) -> R<Uuid> {
    let o = org_ctx(client, slug, c.uid).await?;
    if o.role < ORG_ADMIN {
        return Err(forbidden());
    }
    Ok(o.id)
}

pub async fn create(c: &Ctx, slug: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let client = c.app.pool.get().await?;
    let org_id = admin_org(c, &client, slug).await?;
    let mut e = Errs::default();
    let url = parse_url(&mut e, field(&m, "url"));
    let events = parse_events(&mut e, field(&m, "events"));
    let secret = match field(&m, "secret") {
        F::Absent | F::Null => Some(hex::encode(rand_bytes::<24>())),
        F::Val(Value::String(s)) if !s.is_empty() && s.len() <= 1024 => Some(s.clone()),
        F::Val(_) => {
            e.add("secret", "invalid");
            None
        }
    };
    e.check()?;
    let h = HookOut {
        id: Uuid::now_v7(),
        url: url.unwrap(),
        events: events.unwrap(),
        active: true,
        created_at: Ts(now()),
        secret,
    };
    exec(
        &client,
        "INSERT INTO webhooks (id, org_id, url, events, secret, active, created_at) VALUES ($1, $2, $3, $4, $5, true, $6)",
        &[&h.id, &org_id, &h.url, &h.events, &h.secret, &h.created_at.0],
    )
    .await?;
    Ok(json(201, &h))
}

fn hook_from_row(r: &tokio_postgres::Row) -> HookOut {
    HookOut { id: r.get(0), url: r.get(1), events: r.get(2), active: r.get(3), created_at: Ts(r.get(4)), secret: None }
}

pub async fn list(c: &Ctx, slug: &str) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let cur = match cursor {
        None => Uuid::nil(),
        Some(s) => parse_uuid(&s).ok_or_else(|| vfail1("cursor", "invalid"))?,
    };
    let client = c.app.pool.get().await?;
    let org_id = admin_org(c, &client, slug).await?;
    let rows = q(
        &client,
        "SELECT id, url, events, active, created_at FROM webhooks WHERE org_id = $1 AND id > $2 ORDER BY id LIMIT $3",
        &[&org_id, &cur, &(limit + 1)],
    )
    .await?;
    let mut items: Vec<HookOut> = rows.iter().map(hook_from_row).collect();
    let next = if items.len() as i64 > limit {
        items.truncate(limit as usize);
        Some(enc_cursor(&items.last().unwrap().id.to_string()))
    } else {
        None
    };
    Ok(json(200, &Page { items, next_cursor: next }))
}

pub async fn get(c: &Ctx, slug: &str, id: &str) -> R<Resp> {
    let client = c.app.pool.get().await?;
    let org_id = admin_org(c, &client, slug).await?;
    let id = parse_uuid(id).ok_or_else(not_found)?;
    let r = qopt(
        &client,
        "SELECT id, url, events, active, created_at FROM webhooks WHERE id = $1 AND org_id = $2",
        &[&id, &org_id],
    )
    .await?
    .ok_or_else(not_found)?;
    Ok(json(200, &hook_from_row(&r)))
}

pub async fn patch(c: &Ctx, slug: &str, id: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let client = c.app.pool.get().await?;
    let org_id = admin_org(c, &client, slug).await?;
    let id = parse_uuid(id).ok_or_else(not_found)?;
    let mut e = Errs::default();
    let url = match field(&m, "url") {
        F::Absent => None,
        f => parse_url(&mut e, f),
    };
    let events = match field(&m, "events") {
        F::Absent => None,
        f => parse_events(&mut e, f),
    };
    let active = match field(&m, "active") {
        F::Absent => None,
        F::Val(Value::Bool(b)) => Some(*b),
        _ => {
            e.add("active", "invalid");
            None
        }
    };
    e.check()?;
    let r = qopt(
        &client,
        "UPDATE webhooks SET url = COALESCE($3, url), events = COALESCE($4, events), active = COALESCE($5, active) \
         WHERE id = $1 AND org_id = $2 RETURNING id, url, events, active, created_at",
        &[&id, &org_id, &url, &events, &active],
    )
    .await?
    .ok_or_else(not_found)?;
    Ok(json(200, &hook_from_row(&r)))
}

pub async fn delete(c: &Ctx, slug: &str, id: &str) -> R<Resp> {
    let client = c.app.pool.get().await?;
    let org_id = admin_org(c, &client, slug).await?;
    let id = parse_uuid(id).ok_or_else(not_found)?;
    let n = exec(&client, "DELETE FROM webhooks WHERE id = $1 AND org_id = $2", &[&id, &org_id]).await?;
    if n == 0 {
        return Err(not_found());
    }
    Ok(no_content())
}

#[derive(Serialize)]
struct DeliveryOut {
    id: Uuid,
    event: String,
    status: &'static str,
    attempts: i32,
    last_status_code: Option<i32>,
    created_at: Ts,
}

pub async fn deliveries(c: &Ctx, slug: &str, id: &str) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let cur: i64 = match cursor {
        None => i64::MAX,
        Some(s) => s.parse().map_err(|_| vfail1("cursor", "invalid"))?,
    };
    let client = c.app.pool.get().await?;
    let org_id = admin_org(c, &client, slug).await?;
    let id = parse_uuid(id).ok_or_else(not_found)?;
    qopt(&client, "SELECT 1 FROM webhooks WHERE id = $1 AND org_id = $2", &[&id, &org_id]).await?.ok_or_else(not_found)?;
    let rows = q(
        &client,
        "SELECT id, event, status, attempts, last_status_code, created_at, seq FROM deliveries \
         WHERE webhook_id = $1 AND seq < $2 ORDER BY seq DESC LIMIT $3",
        &[&id, &cur, &(limit + 1)],
    )
    .await?;
    let more = rows.len() as i64 > limit;
    let rows = &rows[..rows.len().min(limit as usize)];
    let next = if more { Some(enc_cursor(&rows.last().unwrap().get::<_, i64>(6).to_string())) } else { None };
    let items: Vec<DeliveryOut> = rows
        .iter()
        .map(|r| DeliveryOut {
            id: r.get(0),
            event: r.get(1),
            status: match r.get::<_, i16>(2) {
                0 => "pending",
                1 => "succeeded",
                _ => "failed",
            },
            attempts: r.get(3),
            last_status_code: r.get(4),
            created_at: Ts(r.get(5)),
        })
        .collect();
    Ok(json(200, &Page { items, next_cursor: next }))
}

// ---------------------------------------------------------------------------------------------
// dispatcher
//
// One task per (webhook, issue) queue, so deliveries about the same issue are strictly ordered
// while different issues proceed in parallel. State lives in the database; `inflight` only
// prevents two tasks from working the same queue.

pub async fn dispatcher(app: Arc<App>) {
    loop {
        if let Err(e) = scan(&app).await {
            eprintln!("webhook dispatcher: {e}");
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
        tokio::select! {
            _ = app.notify.notified() => {}
            _ = tokio::time::sleep(Duration::from_secs(30)) => {}
        }
    }
}

async fn scan(app: &Arc<App>) -> Result<(), String> {
    let client = app.pool.get().await.map_err(|e| e.to_string())?;
    let rows = client
        .query("SELECT DISTINCT webhook_id, issue_id FROM deliveries WHERE status = 0", &[])
        .await
        .map_err(|e| e.to_string())?;
    drop(client);
    for r in rows {
        let key: (Uuid, Uuid) = (r.get(0), r.get(1));
        let fresh = app.inflight.lock().unwrap().insert(key);
        if fresh {
            tokio::spawn(run_queue(app.clone(), key));
        }
    }
    Ok(())
}

async fn run_queue(app: Arc<App>, key: (Uuid, Uuid)) {
    loop {
        match next_pending(&app, key).await {
            Ok(Some(d)) => deliver(&app, d).await,
            Ok(None) => break,
            Err(e) => {
                eprintln!("webhook queue: {e}");
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
        }
    }
    app.inflight.lock().unwrap().remove(&key);
    // a delivery may have been recorded while we were finishing
    app.notify.notify_one();
}

struct Pending {
    id: Uuid,
    event: String,
    payload: String,
    attempts: i32,
    url: String,
    secret: String,
}

async fn next_pending(app: &App, key: (Uuid, Uuid)) -> Result<Option<Pending>, String> {
    let client = app.pool.get().await.map_err(|e| e.to_string())?;
    let r = qopt(
        &client,
        "SELECT d.id, d.event, d.payload, d.attempts, w.url, w.secret FROM deliveries d \
         JOIN webhooks w ON w.id = d.webhook_id \
         WHERE d.webhook_id = $1 AND d.issue_id = $2 AND d.status = 0 ORDER BY d.seq LIMIT 1",
        &[&key.0, &key.1],
    )
    .await
    .map_err(|e| e.to_string())?;
    Ok(r.map(|r| Pending {
        id: r.get(0),
        event: r.get(1),
        payload: r.get(2),
        attempts: r.get(3),
        url: r.get(4),
        secret: r.get(5),
    }))
}

async fn deliver(app: &App, d: Pending) {
    let body = Bytes::from(format!("{{\"id\":\"{}\",{}", d.id, d.payload));
    let did = d.id.to_string();
    let mut attempts = d.attempts;
    loop {
        attempts += 1;
        let code = attempt(app, &d, &did, &body).await;
        let ok = matches!(code, Some(c) if (200..300).contains(&c));
        let status: i16 = if ok {
            1
        } else if attempts >= MAX_ATTEMPTS {
            2
        } else {
            0
        };
        let code = code.map(|c| c as i32);
        loop {
            let res = async {
                let client = app.pool.get().await.map_err(|e| e.to_string())?;
                exec(
                    &client,
                    "UPDATE deliveries SET attempts = $2, last_status_code = $3, status = $4 WHERE id = $1",
                    &[&d.id, &attempts, &code, &status],
                )
                .await
                .map_err(|e| e.to_string())
            }
            .await;
            match res {
                Ok(_) => break,
                Err(e) => {
                    eprintln!("webhook update: {e}");
                    tokio::time::sleep(Duration::from_secs(1)).await;
                }
            }
        }
        if status != 0 {
            return;
        }
        let delay = app.backoff * f64::from(1u32 << (attempts - 1).clamp(0, 30));
        tokio::time::sleep(Duration::from_secs_f64(delay)).await;
    }
}

async fn attempt(app: &App, d: &Pending, did: &str, body: &Bytes) -> Option<u16> {
    let ts = Utc::now().timestamp().to_string();
    let mut mac = Hmac::<Sha256>::new_from_slice(d.secret.as_bytes()).ok()?;
    mac.update(ts.as_bytes());
    mac.update(b".");
    mac.update(body);
    let sig = format!("sha256={}", hex::encode(mac.finalize().into_bytes()));
    let req = Request::post(d.url.as_str())
        .header("content-type", "application/json")
        .header("x-tracker-event", d.event.as_str())
        .header("x-tracker-delivery", did)
        .header("x-tracker-timestamp", ts.as_str())
        .header("x-tracker-signature", sig)
        .body(Full::new(body.clone()))
        .ok()?;
    let fut = async {
        let resp = app.http.request(req).await.ok()?;
        let st = resp.status().as_u16();
        let _ = resp.into_body().collect().await;
        Some(st)
    };
    tokio::time::timeout(Duration::from_secs(5), fut).await.ok().flatten()
}
