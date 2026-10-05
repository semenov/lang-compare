use std::sync::Arc;

use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use bytes::Bytes;
use chrono::{DateTime, Datelike, NaiveDate, Timelike, Utc};
use deadpool_postgres::GenericClient;
use http_body_util::Full;
use hyper::{HeaderMap, Response};
use serde::{Serialize, Serializer};
use serde_json::{Map, Value};
use tokio_postgres::{types::ToSql, Row};
use uuid::Uuid;

use crate::App;

pub type Resp = Response<Full<Bytes>>;
pub type R<T> = Result<T, ApiError>;

#[derive(Debug)]
pub struct ApiError {
    pub status: u16,
    pub code: &'static str,
    pub detail: String,
    pub errors: Vec<(String, &'static str)>,
}

impl ApiError {
    pub fn new(status: u16, code: &'static str, detail: impl Into<String>) -> Self {
        ApiError { status, code, detail: detail.into(), errors: Vec::new() }
    }
}

pub fn not_found() -> ApiError {
    ApiError::new(404, "not_found", "resource not found")
}
pub fn forbidden() -> ApiError {
    ApiError::new(403, "forbidden", "not allowed")
}
pub fn unauth() -> ApiError {
    ApiError::new(401, "unauthenticated", "authentication required")
}
pub fn bad_request(d: &str) -> ApiError {
    ApiError::new(400, "bad_request", d)
}
pub fn conflict(code: &'static str, d: &str) -> ApiError {
    ApiError::new(409, code, d)
}
pub fn vfail1(field: &str, code: &'static str) -> ApiError {
    ApiError { status: 422, code: "validation_failed", detail: "validation failed".into(), errors: vec![(field.into(), code)] }
}

impl From<tokio_postgres::Error> for ApiError {
    fn from(e: tokio_postgres::Error) -> Self {
        eprintln!("db error: {e:?}");
        ApiError::new(500, "internal", "internal error")
    }
}
impl From<deadpool_postgres::PoolError> for ApiError {
    fn from(e: deadpool_postgres::PoolError) -> Self {
        eprintln!("pool error: {e:?}");
        ApiError::new(503, "unavailable", "database unavailable")
    }
}

#[derive(Serialize)]
struct ErrField<'a> {
    field: &'a str,
    code: &'a str,
}
#[derive(Serialize)]
struct Problem<'a> {
    status: u16,
    code: &'a str,
    detail: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    errors: Option<Vec<ErrField<'a>>>,
}

pub fn err_resp(e: &ApiError) -> Resp {
    let errors = if e.code == "validation_failed" {
        Some(e.errors.iter().map(|(f, c)| ErrField { field: f, code: c }).collect())
    } else {
        None
    };
    let body = serde_json::to_vec(&Problem { status: e.status, code: e.code, detail: &e.detail, errors }).unwrap();
    Response::builder()
        .status(e.status)
        .header("content-type", "application/problem+json")
        .body(Full::new(Bytes::from(body)))
        .unwrap()
}

pub fn json_bytes(status: u16, body: impl Into<Bytes>) -> Resp {
    Response::builder()
        .status(status)
        .header("content-type", "application/json")
        .body(Full::new(body.into()))
        .unwrap()
}

pub fn json<T: Serialize + ?Sized>(status: u16, v: &T) -> Resp {
    json_bytes(status, serde_json::to_vec(v).unwrap())
}

pub fn no_content() -> Resp {
    Response::builder().status(204).body(Full::new(Bytes::new())).unwrap()
}

#[derive(Serialize)]
pub struct Page<T: Serialize> {
    pub items: Vec<T>,
    pub next_cursor: Option<String>,
}

// ---------------------------------------------------------------------------------------------
// time

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Ts(pub DateTime<Utc>);

impl Serialize for Ts {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        let d = self.0;
        s.collect_str(&format_args!(
            "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}.{:03}Z",
            d.year(),
            d.month(),
            d.day(),
            d.hour(),
            d.minute(),
            d.second(),
            d.timestamp_subsec_millis()
        ))
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Dt(pub NaiveDate);

impl Serialize for Dt {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        let d = self.0;
        s.collect_str(&format_args!("{:04}-{:02}-{:02}", d.year(), d.month(), d.day()))
    }
}

pub fn now() -> DateTime<Utc> {
    let n = Utc::now();
    DateTime::from_timestamp_millis(n.timestamp_millis()).unwrap()
}

pub fn parse_date(s: &str) -> Option<NaiveDate> {
    let b = s.as_bytes();
    if b.len() != 10 || b[4] != b'-' || b[7] != b'-' {
        return None;
    }
    let digits = |r: std::ops::Range<usize>| -> Option<u32> {
        let mut v = 0u32;
        for &c in &b[r] {
            if !c.is_ascii_digit() {
                return None;
            }
            v = v * 10 + (c - b'0') as u32;
        }
        Some(v)
    };
    NaiveDate::from_ymd_opt(digits(0..4)? as i32, digits(5..7)?, digits(8..10)?)
}

// ---------------------------------------------------------------------------------------------
// request

pub struct Ctx {
    pub app: Arc<App>,
    pub uid: Uuid,
    pub query: Vec<(String, String)>,
    pub headers: HeaderMap,
    pub body: Bytes,
}

impl Ctx {
    pub fn q(&self, k: &str) -> Option<&str> {
        self.query.iter().find(|(a, _)| a == k).map(|(_, v)| v.as_str())
    }
    pub fn header(&self, k: &str) -> Option<&str> {
        self.headers.get(k).and_then(|v| v.to_str().ok())
    }
}

fn hexval(c: u8) -> Option<u8> {
    match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        b'A'..=b'F' => Some(c - b'A' + 10),
        _ => None,
    }
}

pub fn pct_decode(s: &str) -> String {
    if !s.contains('%') {
        return s.to_string();
    }
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() {
            if let (Some(h), Some(l)) = (hexval(b[i + 1]), hexval(b[i + 2])) {
                out.push(h * 16 + l);
                i += 3;
                continue;
            }
        }
        out.push(b[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

pub fn parse_query(q: Option<&str>) -> Vec<(String, String)> {
    let Some(q) = q else { return Vec::new() };
    q.split('&')
        .filter(|p| !p.is_empty())
        .map(|p| match p.split_once('=') {
            Some((k, v)) => (pct_decode(k), pct_decode(v)),
            None => (pct_decode(p), String::new()),
        })
        .collect()
}

pub fn parse_obj(b: &[u8]) -> R<Map<String, Value>> {
    match serde_json::from_slice::<Value>(b) {
        Ok(Value::Object(m)) => Ok(m),
        Ok(_) => Err(bad_request("request body must be a JSON object")),
        Err(_) => Err(bad_request("malformed JSON")),
    }
}

// ---------------------------------------------------------------------------------------------
// validation

pub enum F<'a> {
    Absent,
    Null,
    Val(&'a Value),
}

pub fn field<'a>(m: &'a Map<String, Value>, k: &str) -> F<'a> {
    match m.get(k) {
        None => F::Absent,
        Some(Value::Null) => F::Null,
        Some(v) => F::Val(v),
    }
}

#[derive(Default)]
pub struct Errs(pub Vec<(String, &'static str)>);

impl Errs {
    pub fn add(&mut self, f: impl Into<String>, c: &'static str) {
        self.0.push((f.into(), c));
    }
    pub fn check(self) -> R<()> {
        if self.0.is_empty() {
            Ok(())
        } else {
            Err(ApiError { status: 422, code: "validation_failed", detail: "validation failed".into(), errors: self.0 })
        }
    }
}

/// A required string with length bounds (in characters).
pub fn v_str(e: &mut Errs, name: &str, f: F, trim: bool, min: usize, max: usize) -> Option<String> {
    match f {
        F::Absent | F::Null => {
            e.add(name, "required");
            None
        }
        F::Val(Value::String(s)) => {
            let s = if trim { s.trim() } else { s.as_str() };
            let n = s.chars().count();
            if n == 0 && min > 0 {
                e.add(name, "required");
                None
            } else if n < min {
                e.add(name, "too_short");
                None
            } else if n > max {
                e.add(name, "too_long");
                None
            } else {
                Some(s.to_string())
            }
        }
        F::Val(_) => {
            e.add(name, "invalid");
            None
        }
    }
}

/// An optional nullable text: None = absent, Some(None) = null.
pub fn v_opt_text(e: &mut Errs, name: &str, f: F, max: usize) -> Result<Option<Option<String>>, ()> {
    match f {
        F::Absent => Ok(None),
        F::Null => Ok(Some(None)),
        F::Val(Value::String(s)) => {
            if s.chars().count() > max {
                e.add(name, "too_long");
                Err(())
            } else {
                Ok(Some(Some(s.clone())))
            }
        }
        F::Val(_) => {
            e.add(name, "invalid");
            Err(())
        }
    }
}

pub fn v_enum(e: &mut Errs, name: &str, f: F, opts: &[&str]) -> Option<i16> {
    match f {
        F::Absent | F::Null => {
            e.add(name, "required");
            None
        }
        F::Val(Value::String(s)) => match opts.iter().position(|o| o == s) {
            Some(i) => Some(i as i16),
            None => {
                e.add(name, "invalid");
                None
            }
        },
        F::Val(_) => {
            e.add(name, "invalid");
            None
        }
    }
}

pub fn valid_slug(s: &str) -> bool {
    let b = s.as_bytes();
    let an = |c: u8| c.is_ascii_lowercase() || c.is_ascii_digit();
    b.len() >= 3 && b.len() <= 40 && an(b[0]) && an(b[b.len() - 1]) && b.iter().all(|&c| an(c) || c == b'-')
}

pub fn valid_project_key(s: &str) -> bool {
    let b = s.as_bytes();
    b.len() >= 2 && b.len() <= 10 && b[0].is_ascii_uppercase() && b.iter().all(|&c| c.is_ascii_uppercase() || c.is_ascii_digit())
}

// ---------------------------------------------------------------------------------------------
// pagination

pub fn page_params(q: &[(String, String)]) -> R<(i64, Option<String>)> {
    let get = |k: &str| q.iter().find(|(a, _)| a == k).map(|(_, v)| v.as_str());
    let mut limit = 50;
    if let Some(v) = get("limit") {
        if v.is_empty() || !v.bytes().all(|b| b.is_ascii_digit() || b == b'-') {
            return Err(vfail1("limit", "invalid"));
        }
        match v.parse::<i64>() {
            Ok(n) if (1..=100).contains(&n) => limit = n,
            Ok(_) => return Err(vfail1("limit", "out_of_range")),
            Err(_) => return Err(vfail1("limit", "invalid")),
        }
    }
    let cursor = match get("cursor") {
        None | Some("") => None,
        Some(c) => {
            let raw = URL_SAFE_NO_PAD.decode(c).map_err(|_| vfail1("cursor", "invalid"))?;
            Some(String::from_utf8(raw).map_err(|_| vfail1("cursor", "invalid"))?)
        }
    };
    Ok((limit, cursor))
}

pub fn enc_cursor(s: &str) -> String {
    URL_SAFE_NO_PAD.encode(s)
}

pub fn b64u(b: &[u8]) -> String {
    URL_SAFE_NO_PAD.encode(b)
}

pub fn b64u_dec(s: &str) -> Option<Vec<u8>> {
    URL_SAFE_NO_PAD.decode(s).ok()
}

pub fn rand_bytes<const N: usize>() -> [u8; N] {
    let mut b = [0u8; N];
    getrandom::fill(&mut b).expect("rng");
    b
}

// ---------------------------------------------------------------------------------------------
// roles

pub const ORG_MEMBER: i16 = 1;
pub const ORG_ADMIN: i16 = 2;
pub const ORG_OWNER: i16 = 3;
pub const P_DEV: i16 = 2;
pub const P_ADMIN: i16 = 3;

pub fn org_role_str(r: i16) -> &'static str {
    match r {
        3 => "owner",
        2 => "admin",
        _ => "member",
    }
}
pub fn org_role_parse(s: &str) -> Option<i16> {
    match s {
        "owner" => Some(3),
        "admin" => Some(2),
        "member" => Some(1),
        _ => None,
    }
}
pub fn proj_role_str(r: i16) -> &'static str {
    match r {
        3 => "admin",
        2 => "developer",
        _ => "viewer",
    }
}
pub fn proj_role_parse(s: &str) -> Option<i16> {
    match s {
        "admin" => Some(3),
        "developer" => Some(2),
        "viewer" => Some(1),
        _ => None,
    }
}

/// Effective project role: strongest of explicit membership, org admin+ => admin, org member on
/// org-visible project => developer.
pub fn eff_role(org_role: i16, private: bool, explicit: Option<i16>) -> i16 {
    let mut r = explicit.unwrap_or(0);
    if org_role >= ORG_ADMIN {
        r = P_ADMIN;
    } else if org_role >= ORG_MEMBER && !private && r < P_DEV {
        r = P_DEV;
    }
    if org_role < ORG_MEMBER {
        0
    } else {
        r
    }
}

// ---------------------------------------------------------------------------------------------
// db helpers (prepared statement cache)

pub type P<'a> = &'a (dyn ToSql + Sync);

pub async fn q<C: GenericClient>(c: &C, sql: &str, p: &[P<'_>]) -> Result<Vec<Row>, tokio_postgres::Error> {
    let st = c.prepare_cached(sql).await?;
    c.query(&st, p).await
}
pub async fn q1<C: GenericClient>(c: &C, sql: &str, p: &[P<'_>]) -> Result<Row, tokio_postgres::Error> {
    let st = c.prepare_cached(sql).await?;
    c.query_one(&st, p).await
}
pub async fn qopt<C: GenericClient>(c: &C, sql: &str, p: &[P<'_>]) -> Result<Option<Row>, tokio_postgres::Error> {
    let st = c.prepare_cached(sql).await?;
    c.query_opt(&st, p).await
}
pub async fn exec<C: GenericClient>(c: &C, sql: &str, p: &[P<'_>]) -> Result<u64, tokio_postgres::Error> {
    let st = c.prepare_cached(sql).await?;
    c.execute(&st, p).await
}

pub fn is_unique_violation(e: &tokio_postgres::Error) -> bool {
    e.code() == Some(&tokio_postgres::error::SqlState::UNIQUE_VIOLATION)
}

pub fn parse_uuid(s: &str) -> Option<Uuid> {
    if s.len() != 36 {
        return None;
    }
    Uuid::parse_str(s).ok()
}
