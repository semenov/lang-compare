use argon2::{
    password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString},
    Algorithm, Argon2, Params, Version,
};
use chrono::{DateTime, Utc};
use hmac::Mac;
use hyper::HeaderMap;
use serde::Serialize;
use serde_json::Value;
use sha2::{Digest, Sha256};
use uuid::Uuid;

use crate::util::*;
use crate::App;

// {"alg":"HS256","typ":"JWT"}
const JWT_HEADER: &str = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9";
const REFRESH_TTL_DAYS: i64 = 30;

fn argon() -> Argon2<'static> {
    Argon2::new(Algorithm::Argon2id, Version::V0x13, Params::new(19456, 2, 1, None).unwrap())
}

async fn hash_password(app: &App, pw: String) -> R<String> {
    let _permit = app.hash_sem.acquire().await.map_err(|_| ApiError::new(500, "internal", "shutting down"))?;
    tokio::task::spawn_blocking(move || {
        let salt = SaltString::encode_b64(&rand_bytes::<16>()).expect("salt");
        argon().hash_password(pw.as_bytes(), &salt).map(|h| h.to_string())
    })
    .await
    .ok()
    .and_then(|r| r.ok())
    .ok_or_else(|| ApiError::new(500, "internal", "hashing failed"))
}

async fn verify_password(app: &App, pw: String, hash: String) -> bool {
    let Ok(_permit) = app.hash_sem.acquire().await else { return false };
    tokio::task::spawn_blocking(move || {
        PasswordHash::new(&hash).map(|h| argon().verify_password(pw.as_bytes(), &h).is_ok()).unwrap_or(false)
    })
    .await
    .unwrap_or(false)
}

pub fn make_access(app: &App, uid: Uuid) -> String {
    let iat = Utc::now().timestamp();
    let payload = format!(r#"{{"sub":"{}","iat":{},"exp":{},"typ":"access"}}"#, uid, iat, iat + 900);
    let mut s = String::with_capacity(256);
    s.push_str(JWT_HEADER);
    s.push('.');
    s.push_str(&b64u(payload.as_bytes()));
    let mut mac = app.jwt.clone();
    mac.update(s.as_bytes());
    let sig = mac.finalize().into_bytes();
    s.push('.');
    s.push_str(&b64u(&sig));
    s
}

pub fn authenticate(app: &App, headers: &HeaderMap) -> R<Uuid> {
    let h = headers.get(hyper::header::AUTHORIZATION).and_then(|v| v.to_str().ok()).ok_or_else(unauth)?;
    let tok = h.strip_prefix("Bearer ").or_else(|| h.strip_prefix("bearer ")).ok_or_else(unauth)?.trim();
    let mut it = tok.split('.');
    let (Some(h64), Some(p64), Some(s64), None) = (it.next(), it.next(), it.next(), it.next()) else {
        return Err(unauth());
    };
    let sig = b64u_dec(s64).ok_or_else(unauth)?;
    let mut mac = app.jwt.clone();
    mac.update(h64.as_bytes());
    mac.update(b".");
    mac.update(p64.as_bytes());
    mac.verify_slice(&sig).map_err(|_| unauth())?;
    let hdr: Value = serde_json::from_slice(&b64u_dec(h64).ok_or_else(unauth)?).map_err(|_| unauth())?;
    if hdr.get("alg").and_then(|v| v.as_str()) != Some("HS256") {
        return Err(unauth());
    }
    let claims: Value = serde_json::from_slice(&b64u_dec(p64).ok_or_else(unauth)?).map_err(|_| unauth())?;
    if claims.get("typ").and_then(|v| v.as_str()) != Some("access") {
        return Err(unauth());
    }
    let uid = claims.get("sub").and_then(|v| v.as_str()).and_then(|s| Uuid::parse_str(s).ok()).ok_or_else(unauth)?;
    let exp = claims.get("exp").and_then(|v| v.as_f64()).ok_or_else(unauth)?;
    if exp <= Utc::now().timestamp() as f64 {
        return Err(ApiError::new(401, "token_expired", "access token expired"));
    }
    Ok(uid)
}

#[derive(Serialize)]
pub struct UserOut {
    pub id: Uuid,
    pub email: String,
    pub name: String,
    pub created_at: Ts,
}

#[derive(Serialize)]
struct TokenPair {
    access_token: String,
    refresh_token: String,
    token_type: &'static str,
    expires_in: i64,
}

fn valid_email(s: &str) -> bool {
    match s.split_once('@') {
        Some((l, d)) => !l.is_empty() && !d.is_empty() && !s.chars().any(char::is_whitespace),
        None => false,
    }
}

pub async fn register(app: &App, body: &[u8]) -> R<Resp> {
    let m = parse_obj(body)?;
    let mut e = Errs::default();
    let email = match field(&m, "email") {
        F::Val(Value::String(s)) => {
            let s = s.trim().to_lowercase();
            if s.chars().count() > 254 {
                e.add("email", "too_long");
                None
            } else if !valid_email(&s) {
                e.add("email", "invalid");
                None
            } else {
                Some(s)
            }
        }
        F::Absent | F::Null => {
            e.add("email", "required");
            None
        }
        F::Val(_) => {
            e.add("email", "invalid");
            None
        }
    };
    let password = v_str(&mut e, "password", field(&m, "password"), false, 10, 128);
    let name = v_str(&mut e, "name", field(&m, "name"), true, 1, 100);
    e.check()?;
    let (email, password, name) = (email.unwrap(), password.unwrap(), name.unwrap());
    let client = app.pool.get().await?;
    // cheap pre-check to avoid hashing for an obviously taken email
    if qopt(&client, "SELECT 1 FROM users WHERE email = $1", &[&email]).await?.is_some() {
        return Err(conflict("email_taken", "email already registered"));
    }
    let hash = hash_password(app, password).await?;
    let id = Uuid::now_v7();
    let now = now();
    match exec(
        &client,
        "INSERT INTO users (id, email, name, password_hash, created_at) VALUES ($1, $2, $3, $4, $5)",
        &[&id, &email, &name, &hash, &now],
    )
    .await
    {
        Ok(_) => {}
        Err(e) if is_unique_violation(&e) => return Err(conflict("email_taken", "email already registered")),
        Err(e) => return Err(e.into()),
    }
    Ok(json(201, &UserOut { id, email, name, created_at: Ts(now) }))
}

fn token_hash(t: &str) -> Vec<u8> {
    Sha256::digest(t.as_bytes()).to_vec()
}

async fn new_pair<C: deadpool_postgres::GenericClient>(app: &App, c: &C, uid: Uuid, chain: Option<Uuid>) -> R<Resp> {
    let refresh = b64u(&rand_bytes::<32>());
    let h = token_hash(&refresh);
    let exp: DateTime<Utc> = Utc::now() + chrono::Duration::days(REFRESH_TTL_DAYS);
    match chain {
        Some(ch) => {
            exec(c, "INSERT INTO refresh_tokens (hash, chain_id, expires_at) VALUES ($1, $2, $3)", &[&h, &ch, &exp])
                .await?;
        }
        None => {
            let ch = Uuid::now_v7();
            exec(
                c,
                "WITH c AS (INSERT INTO refresh_chains (id, user_id) VALUES ($1, $2)) \
                 INSERT INTO refresh_tokens (hash, chain_id, expires_at) VALUES ($3, $1, $4)",
                &[&ch, &uid, &h, &exp],
            )
            .await?;
        }
    }
    Ok(json(
        200,
        &TokenPair { access_token: make_access(app, uid), refresh_token: refresh, token_type: "Bearer", expires_in: 900 },
    ))
}

fn invalid_credentials() -> ApiError {
    ApiError::new(401, "invalid_credentials", "wrong email or password")
}

pub async fn login(app: &App, body: &[u8]) -> R<Resp> {
    let m = parse_obj(body)?;
    let mut e = Errs::default();
    let email = v_str(&mut e, "email", field(&m, "email"), true, 1, 1000);
    let password = v_str(&mut e, "password", field(&m, "password"), false, 1, 10000);
    e.check()?;
    let email = email.unwrap().to_lowercase();
    let client = app.pool.get().await?;
    let row = qopt(&client, "SELECT id, password_hash FROM users WHERE email = $1", &[&email])
        .await?
        .ok_or_else(invalid_credentials)?;
    let uid: Uuid = row.get(0);
    let hash: String = row.get(1);
    if !verify_password(app, password.unwrap(), hash).await {
        return Err(invalid_credentials());
    }
    new_pair(app, &client, uid, None).await
}

fn refresh_token_field(body: &[u8]) -> R<String> {
    let m = parse_obj(body)?;
    let mut e = Errs::default();
    let t = v_str(&mut e, "refresh_token", field(&m, "refresh_token"), false, 1, 10000);
    e.check()?;
    Ok(t.unwrap())
}

fn invalid_token() -> ApiError {
    ApiError::new(401, "invalid_token", "invalid refresh token")
}

pub async fn refresh(app: &App, body: &[u8]) -> R<Resp> {
    let tok = refresh_token_field(body)?;
    let h = token_hash(&tok);
    let client = app.pool.get().await?;
    let row = qopt(
        &client,
        "UPDATE refresh_tokens t SET used = true FROM refresh_chains c \
         WHERE t.hash = $1 AND c.id = t.chain_id AND NOT t.used AND NOT c.revoked AND t.expires_at > now() \
         RETURNING c.id, c.user_id",
        &[&h],
    )
    .await?;
    if let Some(r) = row {
        let chain: Uuid = r.get(0);
        let uid: Uuid = r.get(1);
        return new_pair(app, &client, uid, Some(chain)).await;
    }
    let row = qopt(
        &client,
        "SELECT t.used, c.revoked, c.id FROM refresh_tokens t JOIN refresh_chains c ON c.id = t.chain_id WHERE t.hash = $1",
        &[&h],
    )
    .await?
    .ok_or_else(invalid_token)?;
    let used: bool = row.get(0);
    let revoked: bool = row.get(1);
    let chain: Uuid = row.get(2);
    if revoked {
        return Err(invalid_token());
    }
    if used {
        exec(&client, "UPDATE refresh_chains SET revoked = true WHERE id = $1", &[&chain]).await?;
        return Err(ApiError::new(401, "token_reused", "refresh token already used"));
    }
    Err(invalid_token())
}

pub async fn logout(app: &App, body: &[u8]) -> R<Resp> {
    let tok = refresh_token_field(body)?;
    let h = token_hash(&tok);
    let client = app.pool.get().await?;
    let n = exec(
        &client,
        "UPDATE refresh_chains c SET revoked = true FROM refresh_tokens t WHERE t.hash = $1 AND c.id = t.chain_id",
        &[&h],
    )
    .await?;
    if n == 0 {
        return Err(invalid_token());
    }
    Ok(no_content())
}

pub async fn get_me(c: &Ctx) -> R<Resp> {
    let client = c.app.pool.get().await?;
    let r = qopt(&client, "SELECT id, email, name, created_at FROM users WHERE id = $1", &[&c.uid])
        .await?
        .ok_or_else(unauth)?;
    Ok(json(200, &UserOut { id: r.get(0), email: r.get(1), name: r.get(2), created_at: Ts(r.get(3)) }))
}

pub async fn patch_me(c: &Ctx) -> R<Resp> {
    let m = parse_obj(&c.body)?;
    let mut e = Errs::default();
    let name = match field(&m, "name") {
        F::Absent => None,
        f => v_str(&mut e, "name", f, true, 1, 100),
    };
    e.check()?;
    let client = c.app.pool.get().await?;
    let r = match name {
        Some(n) => {
            qopt(&client, "UPDATE users SET name = $2 WHERE id = $1 RETURNING id, email, name, created_at", &[&c.uid, &n])
                .await?
        }
        None => qopt(&client, "SELECT id, email, name, created_at FROM users WHERE id = $1", &[&c.uid]).await?,
    }
    .ok_or_else(unauth)?;
    Ok(json(200, &UserOut { id: r.get(0), email: r.get(1), name: r.get(2), created_at: Ts(r.get(3)) }))
}
