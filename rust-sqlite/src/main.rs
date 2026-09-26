mod catalog;
mod error;
mod handlers;
mod store;

use std::{env, path::Path};

// musl's malloc scales poorly across threads; this is standard for static musl builds.
#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

fn env_or(key: &str, default: &str) -> String {
    env::var(key).unwrap_or_else(|_| default.to_string())
}

#[derive(Clone)]
pub struct AppState {
    pub store: store::Store,
    pub catalog: catalog::Catalog,
}

#[tokio::main]
async fn main() {
    let path = env_or("SQLITE_PATH", "/tmp/lc-sqlite.db");
    let fresh = !Path::new(&path).exists();
    // One reader per runtime worker: rusqlite calls run on tokio's blocking pool, which isn't bounded by
    // the worker count, so a larger pool just has more threads fighting over the CPU (and SQLite's locks).
    let readers = match env::var("SQLITE_READERS") {
        Ok(v) => v.parse().expect("SQLITE_READERS"),
        Err(_) => tokio::runtime::Handle::current().metrics().num_workers(),
    };
    let store = store::Store::new(&path, readers);
    if fresh {
        let script = std::fs::read_to_string(env_or("SQLITE_INIT", "infra/sqlite.sql")).expect("SQLITE_INIT");
        store.init(script).await.expect("init database");
    }

    let state = AppState {
        store,
        catalog: catalog::Catalog::new(env_or("CATALOG_URL", "http://localhost:9000")),
    };

    let addr = format!("0.0.0.0:{}", env_or("PORT", "8080"));
    let listener = tokio::net::TcpListener::bind(&addr).await.expect("bind");
    println!("listening on {addr}");
    axum::serve(listener, handlers::router(state)).await.expect("server");
}
