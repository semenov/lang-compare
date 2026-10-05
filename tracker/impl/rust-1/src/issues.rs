use std::collections::HashSet;
use std::sync::LazyLock;

use chrono::{DateTime, NaiveDate, Utc};
use deadpool_postgres::GenericClient;
use serde::Serialize;
use serde_json::{json as jv, Map, Value};
use sha2::{Digest, Sha256};
use tokio_postgres::Row;
use uuid::Uuid;

use crate::access::{eligible_assignees, proj_ctx};
use crate::util::*;
use crate::webhooks;

pub const TYPES: [&str; 3] = ["task", "bug", "story"];
pub const STATUSES: [&str; 3] = ["todo", "in_progress", "done"];
pub const PRIOS: [&str; 5] = ["lowest", "low", "medium", "high", "highest"];

pub const ISSUE_COLS: &str = "i.id, i.project_key, i.number, i.type, i.title, i.description, i.status, i.priority, \
    i.assignee_id, i.reporter_id, i.labels, i.due_date, i.version, i.comment_count, i.created_at, i.updated_at, i.resolved_at";

#[derive(Serialize, Clone, Debug)]
pub struct Issue {
    pub id: Uuid,
    pub key: String,
    pub number: i32,
    pub project_key: String,
    #[serde(rename = "type")]
    pub typ: &'static str,
    pub title: String,
    pub description: Option<String>,
    pub status: &'static str,
    pub priority: &'static str,
    pub assignee_id: Option<Uuid>,
    pub reporter_id: Uuid,
    pub labels: Vec<String>,
    pub due_date: Option<Dt>,
    pub version: i32,
    pub comment_count: i32,
    pub created_at: Ts,
    pub updated_at: Ts,
    pub resolved_at: Option<Ts>,
}

fn code_of(opts: &[&str], s: &str) -> i16 {
    opts.iter().position(|o| *o == s).unwrap_or(0) as i16
}

pub fn issue_from_row(r: &Row, o: usize) -> Issue {
    let pk: String = r.get(o + 1);
    let number: i32 = r.get(o + 2);
    Issue {
        id: r.get(o),
        key: format!("{pk}-{number}"),
        number,
        project_key: pk,
        typ: TYPES[r.get::<_, i16>(o + 3) as usize],
        title: r.get(o + 4),
        description: r.get(o + 5),
        status: STATUSES[r.get::<_, i16>(o + 6) as usize],
        priority: PRIOS[r.get::<_, i16>(o + 7) as usize],
        assignee_id: r.get(o + 8),
        reporter_id: r.get(o + 9),
        labels: r.get(o + 10),
        due_date: r.get::<_, Option<NaiveDate>>(o + 11).map(Dt),
        version: r.get(o + 12),
        comment_count: r.get(o + 13),
        created_at: Ts(r.get(o + 14)),
        updated_at: Ts(r.get(o + 15)),
        resolved_at: r.get::<_, Option<DateTime<Utc>>>(o + 16).map(Ts),
    }
}

pub fn issue_resp(status: u16, i: &Issue) -> Resp {
    let mut r = json(status, i);
    r.headers_mut().insert("etag", format!("\"{}\"", i.version).parse().unwrap());
    r
}

/// Words for full-text search: maximal runs of letters/digits, lower-cased, deduplicated.
pub fn words_of(parts: &[Option<&str>]) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    for p in parts.iter().flatten() {
        let mut cur = String::new();
        for ch in p.chars() {
            if ch.is_alphanumeric() {
                cur.extend(ch.to_lowercase());
            } else if !cur.is_empty() {
                out.push(std::mem::take(&mut cur));
            }
        }
        if !cur.is_empty() {
            out.push(cur);
        }
    }
    out.sort_unstable();
    out.dedup();
    out
}

pub fn parse_issue_key(s: &str) -> Option<(String, i32)> {
    let u = s.to_ascii_uppercase();
    let (k, n) = u.rsplit_once('-')?;
    if !valid_project_key(k) || n.is_empty() || n.len() > 9 || !n.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    let n: i32 = n.parse().ok()?;
    if n < 1 {
        return None;
    }
    Some((k.to_string(), n))
}

pub struct IssueCtx {
    pub issue: Issue,
    pub org_id: Uuid,
    pub project_id: Uuid,
    pub private: bool,
    pub role: i16,
}

pub async fn issue_ctx<C: GenericClient>(c: &C, slug: &str, ikey: &str, uid: Uuid, lock: bool) -> R<IssueCtx> {
    let (pk, num) = parse_issue_key(ikey).ok_or_else(not_found)?;
    static SQL_PLAIN: LazyLock<String> = LazyLock::new(|| const_format_issue_ctx(false));
    static SQL_LOCK: LazyLock<String> = LazyLock::new(|| const_format_issue_ctx(true));
    let sql: &str = if lock { &SQL_LOCK } else { &SQL_PLAIN };
    let r = qopt(c, sql, &[&slug, &uid, &pk, &num]).await?.ok_or_else(not_found)?;
    let org_role: i16 = r.get(20);
    let vis: i16 = r.get(19);
    let role = eff_role(org_role, vis == 1, r.get(21));
    if role == 0 {
        return Err(not_found());
    }
    Ok(IssueCtx { issue: issue_from_row(&r, 0), org_id: r.get(17), project_id: r.get(18), private: vis == 1, role })
}

fn const_format_issue_ctx(lock: bool) -> String {
    format!(
        "SELECT {ISSUE_COLS}, i.org_id, i.project_id, p.visibility, om.role, pm.role \
         FROM orgs o JOIN org_members om ON om.org_id = o.id AND om.user_id = $2 \
         JOIN issues i ON i.org_id = o.id AND i.project_key = $3 AND i.number = $4 \
         JOIN projects p ON p.id = i.project_id \
         LEFT JOIN project_members pm ON pm.project_id = i.project_id AND pm.user_id = $2 \
         WHERE o.slug = $1{}",
        if lock { " FOR UPDATE OF i" } else { "" }
    )
}

// ---------------------------------------------------------------------------------------------
// validation

pub struct NewIssue {
    pub typ: i16,
    pub title: String,
    pub description: Option<String>,
    pub priority: i16,
    pub assignee: Option<Uuid>,
    pub labels: Vec<String>,
    pub due: Option<NaiveDate>,
}

fn valid_label(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty()
        && b.len() <= 50
        && (b[0].is_ascii_lowercase() || b[0].is_ascii_digit())
        && b[1..].iter().all(|&c| c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, b'_' | b'.' | b'-'))
}

pub fn parse_labels(e: &mut Errs, name: &str, v: &Value) -> Option<Vec<String>> {
    let Value::Array(a) = v else {
        e.add(name, "invalid");
        return None;
    };
    let mut out = Vec::with_capacity(a.len());
    for x in a {
        let Value::String(s) = x else {
            e.add(name, "invalid");
            return None;
        };
        let l = s.to_lowercase();
        if !valid_label(&l) {
            e.add(name, "invalid");
            return None;
        }
        out.push(l);
    }
    out.sort();
    out.dedup();
    if out.len() > 20 {
        e.add(name, "too_long");
        return None;
    }
    Some(out)
}

fn parse_due(e: &mut Errs, name: &str, v: &Value) -> Option<NaiveDate> {
    match v {
        Value::String(s) => parse_date(s).or_else(|| {
            e.add(name, "invalid");
            None
        }),
        _ => {
            e.add(name, "invalid");
            None
        }
    }
}

fn parse_assignee(e: &mut Errs, name: &str, v: &Value) -> Option<Uuid> {
    match v {
        Value::String(s) => Uuid::parse_str(s).ok().or_else(|| {
            e.add(name, "invalid");
            None
        }),
        _ => {
            e.add(name, "invalid");
            None
        }
    }
}

pub fn parse_new(m: &Map<String, Value>, pre: &str, e: &mut Errs) -> Option<NewIssue> {
    let n0 = e.0.len();
    let typ = v_enum(e, &format!("{pre}type"), field(m, "type"), &TYPES);
    let title = v_str(e, &format!("{pre}title"), field(m, "title"), true, 1, 255);
    let description = v_opt_text(e, &format!("{pre}description"), field(m, "description"), 65536).ok().flatten().flatten();
    let priority = match field(m, "priority") {
        F::Absent | F::Null => Some(2),
        f => v_enum(e, &format!("{pre}priority"), f, &PRIOS),
    };
    let assignee = match field(m, "assignee_id") {
        F::Absent | F::Null => None,
        F::Val(v) => parse_assignee(e, &format!("{pre}assignee_id"), v),
    };
    let labels = match field(m, "labels") {
        F::Absent | F::Null => Some(Vec::new()),
        F::Val(v) => parse_labels(e, &format!("{pre}labels"), v),
    };
    let due = match field(m, "due_date") {
        F::Absent | F::Null => None,
        F::Val(v) => parse_due(e, &format!("{pre}due_date"), v),
    };
    if e.0.len() > n0 {
        return None;
    }
    Some(NewIssue {
        typ: typ?,
        title: title?,
        description,
        priority: priority?,
        assignee,
        labels: labels?,
        due,
    })
}

// ---------------------------------------------------------------------------------------------
// create

const CREATED_CHANGES: &str = r#"[{"field":"created","from":null,"to":null}]"#;

enum Created {
    Issues(Vec<Issue>),
    Replay(i32, String),
}

#[derive(Serialize)]
struct IssueData<'a> {
    issue: &'a Issue,
}

#[allow(clippy::too_many_arguments)]
async fn insert_issues(
    c: &Ctx,
    p: &crate::access::ProjCtx,
    slug: &str,
    items: Vec<NewIssue>,
    idem: Option<(&str, Vec<u8>)>,
) -> R<Created> {
    let mut client = c.app.pool.get().await?;
    let tx = client.transaction().await?;
    let now = now();
    if let Some((key, hash)) = &idem {
        let claimed = qopt(
            &tx,
            "INSERT INTO idempotency (user_id, key, req_hash, created_at) VALUES ($1, $2, $3, $4) \
             ON CONFLICT (user_id, key) DO UPDATE SET req_hash = EXCLUDED.req_hash, status = 0, body = NULL, \
               created_at = EXCLUDED.created_at \
             WHERE idempotency.created_at < EXCLUDED.created_at - interval '24 hours' RETURNING 1",
            &[&c.uid, key, hash, &now],
        )
        .await?
        .is_some();
        if !claimed {
            drop(tx);
            let r = qopt(
                &client,
                "SELECT req_hash, status, body FROM idempotency WHERE user_id = $1 AND key = $2",
                &[&c.uid, key],
            )
            .await?
            .ok_or_else(|| conflict("conflict", "concurrent request with the same idempotency key"))?;
            let h: Vec<u8> = r.get(0);
            if &h != hash {
                return Err(ApiError::new(422, "idempotency_key_reused", "idempotency key used with a different request"));
            }
            let body: Option<String> = r.get(2);
            return match body {
                Some(b) => Ok(Created::Replay(r.get(1), b)),
                None => Err(conflict("conflict", "request with this idempotency key is in progress")),
            };
        }
    }
    let n = items.len() as i32;
    let (seq_row, hooks) = tokio::try_join!(
        async {
            q1(
                &tx,
                "UPDATE projects SET issue_seq = issue_seq + $2, issue_count = issue_count + $2 WHERE id = $1 RETURNING issue_seq",
                &[&p.id, &n],
            )
            .await
            .map_err(ApiError::from)
        },
        webhooks::hooks_for(&tx, p.org_id, "issue.created"),
    )?;
    let last: i32 = seq_row.get(0);
    let first = last - n + 1;

    let len = items.len();
    let mut ids = Vec::with_capacity(len);
    let mut nums = Vec::with_capacity(len);
    let mut typs = Vec::with_capacity(len);
    let mut titles = Vec::with_capacity(len);
    let mut descs = Vec::with_capacity(len);
    let mut prios = Vec::with_capacity(len);
    let mut asgs = Vec::with_capacity(len);
    let mut labels = Vec::with_capacity(len);
    let mut dues = Vec::with_capacity(len);
    let mut words = Vec::with_capacity(len);
    let mut hist_ids = Vec::with_capacity(len);
    let mut out = Vec::with_capacity(len);
    for (k, it) in items.into_iter().enumerate() {
        let id = Uuid::now_v7();
        let number = first + k as i32;
        ids.push(id);
        hist_ids.push(Uuid::now_v7());
        nums.push(number);
        typs.push(it.typ);
        words.push(words_of(&[Some(&it.title), it.description.as_deref()]).join(" "));
        prios.push(it.priority);
        asgs.push(it.assignee);
        labels.push(it.labels.join(","));
        dues.push(it.due);
        out.push(Issue {
            id,
            key: format!("{}-{}", p.key, number),
            number,
            project_key: p.key.clone(),
            typ: TYPES[it.typ as usize],
            title: it.title.clone(),
            description: it.description.clone(),
            status: "todo",
            priority: PRIOS[it.priority as usize],
            assignee_id: it.assignee,
            reporter_id: c.uid,
            labels: it.labels,
            due_date: it.due.map(Dt),
            version: 1,
            comment_count: 0,
            created_at: Ts(now),
            updated_at: Ts(now),
            resolved_at: None,
        });
        titles.push(it.title);
        descs.push(it.description);
    }
    let ins_issues = async { exec(
        &tx,
        "INSERT INTO issues (id, org_id, project_id, project_key, number, type, title, description, status, priority, \
           assignee_id, reporter_id, labels, due_date, version, comment_count, created_at, updated_at, words) \
         SELECT x.id, $1, $2, $3, x.num, x.typ, x.title, x.descr, 0, x.prio, x.asg, $4, \
           string_to_array(x.labels, ','), x.due, 1, 0, $5, $5, string_to_array(x.words, ' ') \
         FROM unnest($6::uuid[], $7::int4[], $8::int2[], $9::text[], $10::text[], $11::int2[], $12::uuid[], \
           $13::text[], $14::date[], $15::text[]) AS x(id, num, typ, title, descr, prio, asg, labels, due, words)",
        &[&p.org_id, &p.id, &p.key, &c.uid, &now, &ids, &nums, &typs, &titles, &descs, &prios, &asgs, &labels, &dues, &words],
    ).await.map_err(ApiError::from) };
    let ins_hist = async { exec(
        &tx,
        "INSERT INTO issue_history (id, issue_id, actor_id, created_at, changes) \
         SELECT x.id, x.iid, $1, $2, $3 FROM unnest($4::uuid[], $5::uuid[]) AS x(id, iid)",
        &[&c.uid, &now, &CREATED_CHANGES, &hist_ids, &ids],
    ).await.map_err(ApiError::from) };
    ins_issues.await?;
    ins_hist.await?;
    if !hooks.is_empty() {
        let evs: Vec<(Uuid, String)> = out
            .iter()
            .map(|i| (i.id, webhooks::payload("issue.created", now, slug, c.uid, &IssueData { issue: i })))
            .collect();
        webhooks::insert_deliveries(&tx, &hooks, &evs, "issue.created", now).await?;
    }
    if let Some((key, _)) = &idem {
        let body = serde_json::to_string(&out[0]).unwrap();
        exec(&tx, "UPDATE idempotency SET status = 201, body = $3 WHERE user_id = $1 AND key = $2", &[&c.uid, key, &body])
            .await?;
    }
    tx.commit().await?;
    if !hooks.is_empty() {
        c.app.notify.notify_one();
    }
    Ok(Created::Issues(out))
}

pub async fn create(c: &Ctx, slug: &str, key: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let idem = match c.headers.get("idempotency-key") {
        None => None,
        Some(v) => {
            let s = v.to_str().map_err(|_| vfail1("Idempotency-Key", "invalid"))?;
            if s.is_empty() || s.len() > 255 {
                return Err(vfail1("Idempotency-Key", if s.is_empty() { "too_short" } else { "too_long" }));
            }
            let mut h = Sha256::new();
            h.update(slug.as_bytes());
            h.update(b"/");
            h.update(key.as_bytes());
            h.update(b"\n");
            h.update(&c.body);
            Some((s, h.finalize().to_vec()))
        }
    };
    let p = {
        let client = c.app.pool.get().await?;
        let p = proj_ctx(&client, slug, key, c.uid).await?;
        if p.role < P_DEV {
            return Err(forbidden());
        }
        let mut e = Errs::default();
        let ni = parse_new(&m, "", &mut e);
        e.check()?;
        let ni = ni.unwrap();
        if let Some(a) = ni.assignee {
            if !eligible_assignees(&client, p.org_id, p.id, p.private, &[a]).await?.contains(&a) {
                return Err(vfail1("assignee_id", "invalid"));
            }
        }
        (p, ni)
    };
    let (p, ni) = p;
    match insert_issues(c, &p, slug, vec![ni], idem).await? {
        Created::Issues(v) => Ok(issue_resp(201, &v[0])),
        Created::Replay(status, body) => Ok(json_bytes(status as u16, body)),
    }
}

pub async fn bulk(c: &Ctx, slug: &str, key: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let client = c.app.pool.get().await?;
    let p = proj_ctx(&client, slug, key, c.uid).await?;
    if p.role < P_DEV {
        return Err(forbidden());
    }
    let arr = match field(&m, "issues") {
        F::Val(Value::Array(a)) => a,
        F::Absent | F::Null => return Err(vfail1("issues", "required")),
        F::Val(_) => return Err(vfail1("issues", "invalid")),
    };
    if arr.is_empty() {
        return Err(vfail1("issues", "too_short"));
    }
    if arr.len() > 1000 {
        return Err(vfail1("issues", "too_long"));
    }
    let mut e = Errs::default();
    let mut items = Vec::with_capacity(arr.len());
    for (i, v) in arr.iter().enumerate() {
        match v {
            Value::Object(o) => items.push(parse_new(o, &format!("issues.{i}."), &mut e)),
            _ => {
                e.add(format!("issues.{i}"), "invalid");
                items.push(None);
            }
        }
    }
    // assignee eligibility (also reported alongside other errors)
    let mut asg: Vec<(usize, Uuid)> = Vec::new();
    for (i, v) in arr.iter().enumerate() {
        if let Some(Value::String(s)) = v.get("assignee_id") {
            if let Ok(u) = Uuid::parse_str(s) {
                asg.push((i, u));
            }
        }
    }
    if !asg.is_empty() {
        let mut ids: Vec<Uuid> = asg.iter().map(|x| x.1).collect::<HashSet<_>>().into_iter().collect();
        ids.sort();
        let ok = eligible_assignees(&client, p.org_id, p.id, p.private, &ids).await?;
        for (i, u) in asg {
            if !ok.contains(&u) {
                e.add(format!("issues.{i}.assignee_id"), "invalid");
            }
        }
    }
    e.check()?;
    drop(client);
    let items: Vec<NewIssue> = items.into_iter().map(|x| x.unwrap()).collect();
    match insert_issues(c, &p, slug, items, None).await? {
        Created::Issues(v) => {
            #[derive(Serialize)]
            struct Keys {
                keys: Vec<String>,
            }
            Ok(json(201, &Keys { keys: v.into_iter().map(|i| i.key).collect() }))
        }
        Created::Replay(status, body) => Ok(json_bytes(status as u16, body)),
    }
}

// ---------------------------------------------------------------------------------------------
// read

pub async fn get(c: &Ctx, slug: &str, ik: &str) -> R<Resp> {
    let client = c.app.pool.get().await?;
    let ic = issue_ctx(&client, slug, ik, c.uid, false).await?;
    Ok(issue_resp(200, &ic.issue))
}

#[derive(Serialize)]
struct HistOut {
    id: Uuid,
    actor_id: Uuid,
    created_at: Ts,
    changes: Box<serde_json::value::RawValue>,
}

pub async fn history(c: &Ctx, slug: &str, ik: &str) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let cur: i64 = match cursor {
        None => i64::MAX,
        Some(s) => s.parse().map_err(|_| vfail1("cursor", "invalid"))?,
    };
    let client = c.app.pool.get().await?;
    let ic = issue_ctx(&client, slug, ik, c.uid, false).await?;
    let rows = q(
        &client,
        "SELECT id, actor_id, created_at, changes, seq FROM issue_history WHERE issue_id = $1 AND seq < $2 \
         ORDER BY seq DESC LIMIT $3",
        &[&ic.issue.id, &cur, &(limit + 1)],
    )
    .await?;
    let more = rows.len() as i64 > limit;
    let rows = &rows[..rows.len().min(limit as usize)];
    let next = if more { Some(enc_cursor(&rows.last().unwrap().get::<_, i64>(4).to_string())) } else { None };
    let items: Vec<HistOut> = rows
        .iter()
        .map(|r| HistOut {
            id: r.get(0),
            actor_id: r.get(1),
            created_at: Ts(r.get(2)),
            changes: serde_json::value::RawValue::from_string(r.get(3)).unwrap(),
        })
        .collect();
    Ok(json(200, &Page { items, next_cursor: next }))
}

// ---------------------------------------------------------------------------------------------
// update

fn parse_if_match(c: &Ctx) -> Option<Result<i32, ()>> {
    let v = c.header("if-match")?;
    let v = v.trim();
    let v = v.strip_prefix("W/").unwrap_or(v);
    let v = v.trim_matches('"');
    Some(v.parse::<i32>().map_err(|_| ()))
}

struct Change {
    field: &'static str,
    from: Value,
    to: Value,
}

fn changes_json(ch: &[Change]) -> String {
    let v: Vec<Value> = ch.iter().map(|c| jv!({"field": c.field, "from": c.from, "to": c.to})).collect();
    serde_json::to_string(&v).unwrap()
}

#[derive(Serialize)]
struct UpdatedData<'a> {
    issue: &'a Issue,
    changes: &'a serde_json::value::RawValue,
}

/// Persist a changed issue: row update, history entry and webhook deliveries.
async fn write_update<C: GenericClient>(
    c: &Ctx,
    tx: &C,
    slug: &str,
    org_id: Uuid,
    new: &Issue,
    words_changed: bool,
    changes: &[Change],
    now: DateTime<Utc>,
) -> R<bool> {
    let words: Option<Vec<String>> =
        if words_changed { Some(words_of(&[Some(&new.title), new.description.as_deref()])) } else { None };
    let ch = changes_json(changes);
    let typ = code_of(&TYPES, new.typ);
    let st = code_of(&STATUSES, new.status);
    let pr = code_of(&PRIOS, new.priority);
    let due = new.due_date.map(|d| d.0);
    let resolved = new.resolved_at.map(|t| t.0);
    let hid = Uuid::now_v7();
    let upd = async {
        exec(
            tx,
            "UPDATE issues SET type = $2, title = $3, description = $4, status = $5, priority = $6, assignee_id = $7, \
               labels = $8, due_date = $9, version = $10, updated_at = $11, resolved_at = $12, words = COALESCE($13, words) \
             WHERE id = $1",
            &[&new.id, &typ, &new.title, &new.description, &st, &pr, &new.assignee_id, &new.labels, &due, &new.version, &now, &resolved, &words],
        )
        .await
        .map_err(ApiError::from)
    };
    let hist = async {
        exec(
            tx,
            "INSERT INTO issue_history (id, issue_id, actor_id, created_at, changes) VALUES ($1, $2, $3, $4, $5)",
            &[&hid, &new.id, &c.uid, &now, &ch],
        )
        .await
        .map_err(ApiError::from)
    };
    let (_, _, hooks) = tokio::try_join!(upd, hist, webhooks::hooks_for(tx, org_id, "issue.updated"))?;
    if !hooks.is_empty() {
        let raw = serde_json::value::RawValue::from_string(ch).unwrap();
        let payload = webhooks::payload("issue.updated", now, slug, c.uid, &UpdatedData { issue: new, changes: &raw });
        webhooks::insert_deliveries(tx, &hooks, &[(new.id, payload)], "issue.updated", now).await?;
    }
    Ok(!hooks.is_empty())
}

pub async fn patch(c: &Ctx, slug: &str, ik: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let mut client = c.app.pool.get().await?;
    let tx = client.transaction().await?;
    let ic = issue_ctx(&tx, slug, ik, c.uid, true).await?;
    if ic.role < P_DEV {
        return Err(forbidden());
    }
    let if_match = match parse_if_match(c) {
        None => return Err(ApiError::new(428, "precondition_required", "If-Match header is required")),
        Some(v) => v,
    };
    let mut e = Errs::default();
    if m.contains_key("status") {
        e.add("status", "invalid");
    }
    let title = match field(&m, "title") {
        F::Absent => None,
        f => v_str(&mut e, "title", f, true, 1, 255),
    };
    let desc = v_opt_text(&mut e, "description", field(&m, "description"), 65536).unwrap_or(None);
    let typ = match field(&m, "type") {
        F::Absent => None,
        f => v_enum(&mut e, "type", f, &TYPES),
    };
    let prio = match field(&m, "priority") {
        F::Absent => None,
        f => v_enum(&mut e, "priority", f, &PRIOS),
    };
    let asg: Option<Option<Uuid>> = match field(&m, "assignee_id") {
        F::Absent => None,
        F::Null => Some(None),
        F::Val(v) => parse_assignee(&mut e, "assignee_id", v).map(Some),
    };
    let labels = match field(&m, "labels") {
        F::Absent => None,
        F::Null => Some(Vec::new()),
        F::Val(v) => parse_labels(&mut e, "labels", v),
    };
    let due: Option<Option<NaiveDate>> = match field(&m, "due_date") {
        F::Absent => None,
        F::Null => Some(None),
        F::Val(v) => parse_due(&mut e, "due_date", v).map(Some),
    };
    e.check()?;
    if if_match != Ok(ic.issue.version) {
        return Err(ApiError::new(412, "version_mismatch", "version mismatch"));
    }
    let old = &ic.issue;
    let mut new = old.clone();
    let mut ch: Vec<Change> = Vec::new();
    let mut words_changed = false;
    if let Some(t) = title {
        if t != old.title {
            ch.push(Change { field: "title", from: jv!(old.title), to: jv!(t) });
            new.title = t;
            words_changed = true;
        }
    }
    if let Some(d) = desc {
        if d != old.description {
            ch.push(Change { field: "description", from: jv!(old.description), to: jv!(d) });
            new.description = d;
            words_changed = true;
        }
    }
    if let Some(t) = typ {
        let t = TYPES[t as usize];
        if t != old.typ {
            ch.push(Change { field: "type", from: jv!(old.typ), to: jv!(t) });
            new.typ = t;
        }
    }
    if let Some(p) = prio {
        let p = PRIOS[p as usize];
        if p != old.priority {
            ch.push(Change { field: "priority", from: jv!(old.priority), to: jv!(p) });
            new.priority = p;
        }
    }
    if let Some(a) = asg {
        if a != old.assignee_id {
            if let Some(u) = a {
                if !eligible_assignees(&tx, ic.org_id, ic.project_id, ic.private, &[u]).await?.contains(&u) {
                    return Err(vfail1("assignee_id", "invalid"));
                }
            }
            ch.push(Change { field: "assignee_id", from: jv!(old.assignee_id), to: jv!(a) });
            new.assignee_id = a;
        }
    }
    if let Some(l) = labels {
        if l != old.labels {
            ch.push(Change { field: "labels", from: jv!(old.labels), to: jv!(l) });
            new.labels = l;
        }
    }
    if let Some(d) = due {
        let d = d.map(Dt);
        if d != old.due_date {
            ch.push(Change { field: "due_date", from: jv!(old.due_date), to: jv!(d) });
            new.due_date = d;
        }
    }
    if ch.is_empty() {
        tx.commit().await?;
        return Ok(issue_resp(200, &new));
    }
    let now = now();
    new.version += 1;
    new.updated_at = Ts(now);
    let notify = write_update(c, &tx, slug, ic.org_id, &new, words_changed, &ch, now).await?;
    tx.commit().await?;
    if notify {
        c.app.notify.notify_one();
    }
    Ok(issue_resp(200, &new))
}

pub async fn transition(c: &Ctx, slug: &str, ik: &str) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let mut client = c.app.pool.get().await?;
    let tx = client.transaction().await?;
    let ic = issue_ctx(&tx, slug, ik, c.uid, true).await?;
    if ic.role < P_DEV {
        return Err(forbidden());
    }
    let mut e = Errs::default();
    let st = v_enum(&mut e, "status", field(&m, "status"), &STATUSES);
    e.check()?;
    let to = STATUSES[st.unwrap() as usize];
    if let Some(v) = parse_if_match(c) {
        if v != Ok(ic.issue.version) {
            return Err(ApiError::new(412, "version_mismatch", "version mismatch"));
        }
    }
    let from = ic.issue.status;
    let allowed = matches!(
        (from, to),
        ("todo", "in_progress") | ("in_progress", "todo") | ("in_progress", "done") | ("done", "in_progress")
    );
    if !allowed {
        return Err(conflict("transition_not_allowed", "transition not allowed"));
    }
    let now = now();
    let mut new = ic.issue.clone();
    new.status = to;
    new.version += 1;
    new.updated_at = Ts(now);
    new.resolved_at = if to == "done" { Some(Ts(now)) } else { None };
    let ch = [Change { field: "status", from: jv!(from), to: jv!(to) }];
    let notify = write_update(c, &tx, slug, ic.org_id, &new, false, &ch, now).await?;
    tx.commit().await?;
    if notify {
        c.app.notify.notify_one();
    }
    Ok(issue_resp(200, &new))
}

pub async fn delete(c: &Ctx, slug: &str, ik: &str) -> R<Resp> {
    let mut client = c.app.pool.get().await?;
    let tx = client.transaction().await?;
    let ic = issue_ctx(&tx, slug, ik, c.uid, true).await?;
    if ic.role < P_ADMIN {
        return Err(forbidden());
    }
    let now = now();
    let (_, _, hooks) = tokio::try_join!(
        async { exec(&tx, "DELETE FROM issues WHERE id = $1", &[&ic.issue.id]).await.map_err(ApiError::from) },
        async {
            exec(&tx, "UPDATE projects SET issue_count = issue_count - 1 WHERE id = $1", &[&ic.project_id])
                .await
                .map_err(ApiError::from)
        },
        webhooks::hooks_for(&tx, ic.org_id, "issue.deleted"),
    )?;
    if !hooks.is_empty() {
        let payload = webhooks::payload("issue.deleted", now, slug, c.uid, &IssueData { issue: &ic.issue });
        webhooks::insert_deliveries(&tx, &hooks, &[(ic.issue.id, payload)], "issue.deleted", now).await?;
    }
    tx.commit().await?;
    if !hooks.is_empty() {
        c.app.notify.notify_one();
    }
    Ok(no_content())
}
