#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

mod access;
mod auth;
mod comments;
mod issues;
mod orgs;
mod projects;
mod schema;
mod search;
mod util;
mod webhooks;

use std::{collections::HashSet, convert::Infallible, sync::Arc, time::Duration};

use bytes::Bytes;
use deadpool_postgres::{Manager, ManagerConfig, Pool, RecyclingMethod};
use hmac::{Hmac, Mac};
use http_body_util::{BodyExt, Full, Limited};
use hyper::{body::Incoming, server::conn::http1, service::service_fn, Method, Request};
use hyper_util::{
    client::legacy::{connect::HttpConnector, Client},
    rt::{TokioExecutor, TokioIo},
};
use sha2::Sha256;
use tokio::{
    net::TcpListener,
    signal::unix::{signal, SignalKind},
    sync::{Notify, Semaphore},
};
use uuid::Uuid;

use util::*;

pub struct App {
    pub pool: Pool,
    pub jwt: Hmac<Sha256>,
    pub backoff: f64,
    pub notify: Notify,
    pub hash_sem: Semaphore,
    pub inflight: std::sync::Mutex<HashSet<(Uuid, Uuid)>>,
    pub http: Client<HttpConnector, Full<Bytes>>,
}

fn main() {
    let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build().expect("runtime");
    rt.block_on(run());
}

async fn run() {
    let port: u16 = std::env::var("PORT").ok().and_then(|s| s.parse().ok()).unwrap_or(8080);
    let db_url = std::env::var("DATABASE_URL").expect("DATABASE_URL is required");
    let secret = std::env::var("JWT_SECRET").expect("JWT_SECRET is required");
    let backoff = std::env::var("WEBHOOK_BACKOFF_SCALE")
        .ok()
        .and_then(|s| s.trim().parse::<f64>().ok())
        .filter(|v| v.is_finite() && *v >= 0.0)
        .unwrap_or(1.0);
    let cpus = std::thread::available_parallelism().map(|n| n.get()).unwrap_or(2);
    let pool_size = std::env::var("DB_POOL_SIZE").ok().and_then(|s| s.parse().ok()).unwrap_or(32usize);

    let pg: tokio_postgres::Config = db_url.parse().expect("invalid DATABASE_URL");
    let mgr = Manager::from_config(pg, tokio_postgres::NoTls, ManagerConfig { recycling_method: RecyclingMethod::Fast });
    let pool = Pool::builder(mgr).max_size(pool_size).build().expect("pool");

    let mut tries = 0;
    loop {
        match schema::migrate(&pool).await {
            Ok(()) => break,
            Err(e) => {
                tries += 1;
                if tries > 120 {
                    eprintln!("migration failed: {e}");
                    std::process::exit(1);
                }
                tokio::time::sleep(Duration::from_millis(250)).await;
            }
        }
    }

    let mut connector = HttpConnector::new();
    connector.set_nodelay(true);
    connector.set_connect_timeout(Some(Duration::from_secs(5)));
    let http = Client::builder(TokioExecutor::new())
        .pool_idle_timeout(Duration::from_secs(30))
        .pool_max_idle_per_host(8)
        .build(connector);

    let app = Arc::new(App {
        pool,
        jwt: Hmac::<Sha256>::new_from_slice(secret.as_bytes()).expect("hmac"),
        backoff,
        notify: Notify::new(),
        hash_sem: Semaphore::new(cpus.max(1)),
        inflight: std::sync::Mutex::new(HashSet::new()),
        http,
    });

    tokio::spawn(webhooks::dispatcher(app.clone()));

    let listener = TcpListener::bind(("0.0.0.0", port)).await.expect("bind");
    let graceful = hyper_util::server::graceful::GracefulShutdown::new();
    let mut sigterm = signal(SignalKind::terminate()).expect("signal");
    let mut sigint = signal(SignalKind::interrupt()).expect("signal");

    loop {
        tokio::select! {
            r = listener.accept() => {
                let Ok((stream, _)) = r else { continue };
                let _ = stream.set_nodelay(true);
                let app = app.clone();
                let conn = http1::Builder::new()
                    .keep_alive(true)
                    .serve_connection(TokioIo::new(stream), service_fn(move |req| handle(app.clone(), req)));
                let fut = graceful.watch(conn);
                tokio::spawn(async move {
                    let _ = fut.await;
                });
            }
            _ = sigterm.recv() => break,
            _ = sigint.recv() => break,
        }
    }
    drop(listener);
    tokio::select! {
        _ = graceful.shutdown() => {}
        _ = tokio::time::sleep(Duration::from_secs(10)) => {}
    }
    std::process::exit(0);
}

async fn handle(app: Arc<App>, req: Request<Incoming>) -> Result<Resp, Infallible> {
    Ok(match route(app, req).await {
        Ok(r) => r,
        Err(e) => err_resp(&e),
    })
}

async fn route(app: Arc<App>, req: Request<Incoming>) -> R<Resp> {
    let (parts, body) = req.into_parts();
    let path = parts.uri.path();
    if path == "/healthz" {
        return Ok(json_bytes(200, &br#"{"status":"ok"}"#[..]));
    }
    if path == "/readyz" {
        let ok = async {
            let c = app.pool.get().await.ok()?;
            c.simple_query("SELECT 1").await.ok()
        };
        return Ok(match tokio::time::timeout(Duration::from_secs(2), ok).await {
            Ok(Some(_)) => json_bytes(200, &br#"{"status":"ok"}"#[..]),
            _ => json_bytes(503, &br#"{"status":"unavailable"}"#[..]),
        });
    }
    let Some(rest) = path.strip_prefix("/api/v1/") else { return Err(not_found()) };
    let segs: Vec<&str> = rest.trim_end_matches('/').split('/').collect();
    let method = parts.method.clone();
    let body = if matches!(method, Method::POST | Method::PATCH | Method::PUT) {
        Limited::new(body, 64 << 20)
            .collect()
            .await
            .map_err(|_| bad_request("cannot read request body"))?
            .to_bytes()
    } else {
        Bytes::new()
    };

    if segs[0] == "auth" {
        return match (&method, segs.get(1).copied(), segs.len()) {
            (&Method::POST, Some("register"), 2) => auth::register(&app, &body).await,
            (&Method::POST, Some("login"), 2) => auth::login(&app, &body).await,
            (&Method::POST, Some("refresh"), 2) => auth::refresh(&app, &body).await,
            (&Method::POST, Some("logout"), 2) => auth::logout(&app, &body).await,
            _ => Err(not_found()),
        };
    }

    let uid = auth::authenticate(&app, &parts.headers)?;
    let c = Ctx { app, uid, query: parse_query(parts.uri.query()), headers: parts.headers, body };
    use Method as M;
    match (method, &segs[..]) {
        (M::GET, ["me"]) => auth::get_me(&c).await,
        (M::PATCH, ["me"]) => auth::patch_me(&c).await,
        (M::GET, ["orgs"]) => orgs::list(&c).await,
        (M::POST, ["orgs"]) => orgs::create(&c).await,
        (M::GET, ["orgs", slug]) => orgs::get(&c, slug).await,
        (M::GET, ["orgs", slug, "members"]) => orgs::members(&c, slug).await,
        (M::POST, ["orgs", slug, "members"]) => orgs::add_member(&c, slug).await,
        (M::PATCH, ["orgs", slug, "members", id]) => orgs::patch_member(&c, slug, id).await,
        (M::DELETE, ["orgs", slug, "members", id]) => orgs::delete_member(&c, slug, id).await,
        (M::GET, ["orgs", slug, "projects"]) => projects::list(&c, slug).await,
        (M::POST, ["orgs", slug, "projects"]) => projects::create(&c, slug).await,
        (M::GET, ["orgs", slug, "projects", key]) => projects::get(&c, slug, key).await,
        (M::PATCH, ["orgs", slug, "projects", key]) => projects::patch(&c, slug, key).await,
        (M::DELETE, ["orgs", slug, "projects", key]) => projects::delete(&c, slug, key).await,
        (M::GET, ["orgs", slug, "projects", key, "members"]) => projects::members(&c, slug, key).await,
        (M::PUT, ["orgs", slug, "projects", key, "members", id]) => projects::put_member(&c, slug, key, id).await,
        (M::DELETE, ["orgs", slug, "projects", key, "members", id]) => {
            projects::delete_member(&c, slug, key, id).await
        }
        (M::POST, ["orgs", slug, "projects", key, "issues"]) => issues::create(&c, slug, key).await,
        (M::POST, ["orgs", slug, "projects", key, "issues", "bulk"]) => issues::bulk(&c, slug, key).await,
        (M::GET, ["orgs", slug, "issues"]) => search::list(&c, slug).await,
        (M::GET, ["orgs", slug, "issues", ik]) => issues::get(&c, slug, ik).await,
        (M::PATCH, ["orgs", slug, "issues", ik]) => issues::patch(&c, slug, ik).await,
        (M::DELETE, ["orgs", slug, "issues", ik]) => issues::delete(&c, slug, ik).await,
        (M::POST, ["orgs", slug, "issues", ik, "transition"]) => issues::transition(&c, slug, ik).await,
        (M::GET, ["orgs", slug, "issues", ik, "history"]) => issues::history(&c, slug, ik).await,
        (M::GET, ["orgs", slug, "issues", ik, "comments"]) => comments::list(&c, slug, ik).await,
        (M::POST, ["orgs", slug, "issues", ik, "comments"]) => comments::create(&c, slug, ik).await,
        (M::PATCH, ["orgs", slug, "comments", id]) => comments::patch(&c, slug, id).await,
        (M::DELETE, ["orgs", slug, "comments", id]) => comments::delete(&c, slug, id).await,
        (M::GET, ["orgs", slug, "webhooks"]) => webhooks::list(&c, slug).await,
        (M::POST, ["orgs", slug, "webhooks"]) => webhooks::create(&c, slug).await,
        (M::GET, ["orgs", slug, "webhooks", id]) => webhooks::get(&c, slug, id).await,
        (M::PATCH, ["orgs", slug, "webhooks", id]) => webhooks::patch(&c, slug, id).await,
        (M::DELETE, ["orgs", slug, "webhooks", id]) => webhooks::delete(&c, slug, id).await,
        (M::GET, ["orgs", slug, "webhooks", id, "deliveries"]) => webhooks::deliveries(&c, slug, id).await,
        _ => Err(not_found()),
    }
}
