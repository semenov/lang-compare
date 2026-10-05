use chrono::{DateTime, Utc};
use serde::Serialize;
use tokio_postgres::types::ToSql;
use uuid::Uuid;

use crate::issues::{issue_from_row, words_of, Issue, ISSUE_COLS, PRIOS, STATUSES, TYPES};
use crate::util::*;

#[derive(Clone, Copy, PartialEq)]
enum SortF {
    Created,
    Updated,
    Priority,
    Key,
}

#[derive(Serialize)]
struct ListOut {
    total: i64,
    items: Vec<Issue>,
    next_cursor: Option<String>,
}

fn split(v: &str) -> impl Iterator<Item = &str> {
    v.split(',').map(str::trim).filter(|s| !s.is_empty())
}

fn enum_list(v: Option<&str>, opts: &[&str], name: &str, e: &mut Errs) -> Option<Vec<i16>> {
    let v = v?;
    let mut out = Vec::new();
    for s in split(v) {
        match opts.iter().position(|o| *o == s) {
            Some(i) => out.push(i as i16),
            None => {
                e.add(name, "invalid");
                return None;
            }
        }
    }
    if out.is_empty() {
        None
    } else {
        Some(out)
    }
}

fn ts_param(v: Option<&str>, name: &str, e: &mut Errs) -> Option<DateTime<Utc>> {
    let v = v?;
    match DateTime::parse_from_rfc3339(v.trim()) {
        Ok(t) => Some(t.with_timezone(&Utc)),
        Err(_) => {
            e.add(name, "invalid");
            None
        }
    }
}

type Param = Box<dyn ToSql + Sync + Send>;

pub async fn list(c: &Ctx, slug: &str) -> R<Resp> {
    let (limit, cursor) = page_params(&c.query)?;
    let mut e = Errs::default();
    let f_projects: Option<Vec<String>> = c.q("project").map(|v| split(v).map(|s| s.to_ascii_uppercase()).collect());
    let statuses = enum_list(c.q("status"), &STATUSES, "status", &mut e);
    let prios = enum_list(c.q("priority"), &PRIOS, "priority", &mut e);
    let types = enum_list(c.q("type"), &TYPES, "type", &mut e);
    let mut asg_ids: Vec<Uuid> = Vec::new();
    let mut asg_none = false;
    let mut asg_given = false;
    if let Some(v) = c.q("assignee") {
        for s in split(v) {
            asg_given = true;
            match s {
                "me" => asg_ids.push(c.uid),
                "none" => asg_none = true,
                _ => match Uuid::parse_str(s) {
                    Ok(u) => asg_ids.push(u),
                    Err(_) => {
                        e.add("assignee", "invalid");
                        break;
                    }
                },
            }
        }
    }
    let mut rep_ids: Vec<Uuid> = Vec::new();
    if let Some(v) = c.q("reporter") {
        for s in split(v) {
            match s {
                "me" => rep_ids.push(c.uid),
                _ => match Uuid::parse_str(s) {
                    Ok(u) => rep_ids.push(u),
                    Err(_) => {
                        e.add("reporter", "invalid");
                        break;
                    }
                },
            }
        }
    }
    let labels: Vec<String> = c.q("label").map(|v| split(v).map(|s| s.to_lowercase()).collect()).unwrap_or_default();
    let created_after = ts_param(c.q("created_after"), "created_after", &mut e);
    let created_before = ts_param(c.q("created_before"), "created_before", &mut e);
    let updated_after = ts_param(c.q("updated_after"), "updated_after", &mut e);
    let qwords = c.q("q").map(|v| words_of(&[Some(v)])).unwrap_or_default();
    let sort_s = c.q("sort").unwrap_or("-created");
    let (desc, sname) = match sort_s.strip_prefix('-') {
        Some(r) => (true, r),
        None => (false, sort_s),
    };
    let sf = match sname {
        "created" => Some(SortF::Created),
        "updated" => Some(SortF::Updated),
        "priority" => Some(SortF::Priority),
        "key" => Some(SortF::Key),
        _ => {
            e.add("sort", "invalid");
            None
        }
    };
    e.check()?;
    let sf = sf.unwrap();

    // cursor: "<sort>|<value>|<project key>|<number>"
    let cur: Option<(i64, String, i32)> = match &cursor {
        None => None,
        Some(s) => {
            let bad = || vfail1("cursor", "invalid");
            let mut it = s.splitn(4, '|');
            let (Some(cs), Some(v), Some(k), Some(n)) = (it.next(), it.next(), it.next(), it.next()) else {
                return Err(bad());
            };
            if cs != sort_s {
                return Err(bad());
            }
            Some((v.parse().map_err(|_| bad())?, k.to_string(), n.parse().map_err(|_| bad())?))
        }
    };

    let client = c.app.pool.get().await?;
    let rows = q(
        &client,
        "SELECT om.role, o.id, p.id, p.key, p.visibility, pm.role, p.issue_count \
         FROM orgs o JOIN org_members om ON om.org_id = o.id AND om.user_id = $2 \
         LEFT JOIN projects p ON p.org_id = o.id \
         LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $2 \
         WHERE o.slug = $1",
        &[&slug, &c.uid],
    )
    .await?;
    if rows.is_empty() {
        return Err(not_found());
    }
    let org_role: i16 = rows[0].get(0);
    let org_id: Uuid = rows[0].get(1);
    let mut all_visible = true;
    let mut sel: Vec<Uuid> = Vec::new();
    let mut sel_count: i64 = 0;
    for r in &rows {
        let Some(pid) = r.get::<_, Option<Uuid>>(2) else { continue };
        let key: &str = r.get(3);
        let vis: i16 = r.get(4);
        if eff_role(org_role, vis == 1, r.get(5)) == 0 {
            all_visible = false;
            continue;
        }
        if let Some(fp) = &f_projects {
            if !fp.iter().any(|k| k == key) {
                continue;
            }
        }
        sel.push(pid);
        sel_count += r.get::<_, i32>(6) as i64;
    }
    let restrict = f_projects.is_some() || !all_visible;
    if restrict && sel.is_empty() {
        return Ok(json(200, &ListOut { total: 0, items: Vec::new(), next_cursor: None }));
    }

    let mut conds = String::from("i.org_id = $1");
    let mut ps: Vec<Param> = vec![Box::new(org_id)];
    fn push(ps: &mut Vec<Param>, v: Param) -> String {
        ps.push(v);
        format!("${}", ps.len())
    }
    if restrict {
        let n = push(&mut ps, Box::new(sel));
        conds.push_str(&format!(" AND i.project_id = ANY({n})"));
    }
    let mut other = false;
    if let Some(v) = statuses {
        other = true;
        let n = push(&mut ps, Box::new(v));
        conds.push_str(&format!(" AND i.status = ANY({n}::int2[])"));
    }
    if let Some(v) = prios {
        other = true;
        let n = push(&mut ps, Box::new(v));
        conds.push_str(&format!(" AND i.priority = ANY({n}::int2[])"));
    }
    if let Some(v) = types {
        other = true;
        let n = push(&mut ps, Box::new(v));
        conds.push_str(&format!(" AND i.type = ANY({n}::int2[])"));
    }
    if asg_given {
        other = true;
        match (asg_ids.is_empty(), asg_none) {
            (true, true) => conds.push_str(" AND i.assignee_id IS NULL"),
            (false, false) => {
                let n = push(&mut ps, Box::new(asg_ids));
                conds.push_str(&format!(" AND i.assignee_id = ANY({n}::uuid[])"));
            }
            (false, true) => {
                let n = push(&mut ps, Box::new(asg_ids));
                conds.push_str(&format!(" AND (i.assignee_id = ANY({n}::uuid[]) OR i.assignee_id IS NULL)"));
            }
            (true, false) => {}
        }
    }
    if !rep_ids.is_empty() {
        other = true;
        let n = push(&mut ps, Box::new(rep_ids));
        conds.push_str(&format!(" AND i.reporter_id = ANY({n}::uuid[])"));
    }
    if !labels.is_empty() {
        other = true;
        let n = push(&mut ps, Box::new(labels));
        conds.push_str(&format!(" AND i.labels && {n}::text[]"));
    }
    if let Some(t) = created_after {
        other = true;
        let n = push(&mut ps, Box::new(t));
        conds.push_str(&format!(" AND i.created_at > {n}"));
    }
    if let Some(t) = created_before {
        other = true;
        let n = push(&mut ps, Box::new(t));
        conds.push_str(&format!(" AND i.created_at < {n}"));
    }
    if let Some(t) = updated_after {
        other = true;
        let n = push(&mut ps, Box::new(t));
        conds.push_str(&format!(" AND i.updated_at > {n}"));
    }
    if !qwords.is_empty() {
        other = true;
        let n = push(&mut ps, Box::new(qwords));
        conds.push_str(&format!(" AND i.words @> {n}::text[]"));
    }
    let ncount = ps.len();

    let col = match sf {
        SortF::Created => "i.created_at",
        SortF::Updated => "i.updated_at",
        SortF::Priority => "i.priority",
        SortF::Key => "",
    };
    let mut page_conds = conds.clone();
    if let Some((v, k, n)) = cur {
        let vp: Param = match sf {
            SortF::Created | SortF::Updated => {
                Box::new(DateTime::<Utc>::from_timestamp_micros(v).ok_or_else(|| vfail1("cursor", "invalid"))?)
            }
            SortF::Priority => Box::new(v as i16),
            SortF::Key => Box::new(0i16),
        };
        let kp = push(&mut ps, Box::new(k));
        let np = push(&mut ps, Box::new(n));
        if sf == SortF::Key {
            let op = if desc { "<" } else { ">" };
            page_conds.push_str(&format!(" AND (i.project_key, i.number) {op} ({kp}, {np})"));
        } else {
            let vp = push(&mut ps, vp);
            if desc {
                page_conds.push_str(&format!(
                    " AND ({col} < {vp} OR ({col} = {vp} AND (i.project_key, i.number) > ({kp}, {np})))"
                ));
            } else {
                page_conds.push_str(&format!(" AND ({col}, i.project_key, i.number) > ({vp}, {kp}, {np})"));
            }
        }
    }
    let order = match (sf, desc) {
        (SortF::Key, false) => "i.project_key, i.number".to_string(),
        (SortF::Key, true) => "i.project_key DESC, i.number DESC".to_string(),
        (_, false) => format!("{col}, i.project_key, i.number"),
        (_, true) => format!("{col} DESC, i.project_key, i.number"),
    };
    let lp = push(&mut ps, Box::new(limit + 1));
    let page_sql = format!("SELECT {ISSUE_COLS} FROM issues i WHERE {page_conds} ORDER BY {order} LIMIT {lp}");
    let refs: Vec<&(dyn ToSql + Sync)> = ps.iter().map(|b| &**b as &(dyn ToSql + Sync)).collect();
    let st_page = client.prepare_cached(&page_sql).await?;
    let (rows, total) = if other {
        let count_sql = format!("SELECT count(*) FROM issues i WHERE {conds}");
        let st_count = client.prepare_cached(&count_sql).await?;
        let (rows, cnt) = tokio::try_join!(client.query(&st_page, &refs), client.query_one(&st_count, &refs[..ncount]))?;
        (rows, cnt.get::<_, i64>(0))
    } else {
        (client.query(&st_page, &refs).await?, sel_count)
    };
    let more = rows.len() as i64 > limit;
    let items: Vec<Issue> = rows.iter().take(limit as usize).map(|r| issue_from_row(r, 0)).collect();
    let next = if more {
        let last = items.last().unwrap();
        let v: i64 = match sf {
            SortF::Created => last.created_at.0.timestamp_micros(),
            SortF::Updated => last.updated_at.0.timestamp_micros(),
            SortF::Priority => PRIOS.iter().position(|p| *p == last.priority).unwrap_or(0) as i64,
            SortF::Key => 0,
        };
        Some(enc_cursor(&format!("{sort_s}|{v}|{}|{}", last.project_key, last.number)))
    } else {
        None
    };
    Ok(json(200, &ListOut { total, items, next_cursor: next }))
}
