mod catalog;
mod error;
mod handlers;
mod store;

use std::env;

use sqlx::postgres::PgPoolOptions;

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
    let pool_size: u32 = env_or("DB_POOL_SIZE", "20").parse().expect("DB_POOL_SIZE");
    let pool = PgPoolOptions::new()
        .max_connections(pool_size)
        // sqlx pings every connection on checkout by default (an extra round trip per query);
        // pgx and node-postgres don't, so turn it off for a like-for-like comparison.
        .test_before_acquire(false)
        .connect(&env_or("DATABASE_URL", "postgres://app:app@localhost:15432/app"))
        .await
        .expect("connect to postgres");

    let state = AppState {
        store: store::Store::new(pool),
        catalog: catalog::Catalog::new(env_or("CATALOG_URL", "http://localhost:9000")),
    };

    let addr = format!("0.0.0.0:{}", env_or("PORT", "8080"));
    let listener = tokio::net::TcpListener::bind(&addr).await.expect("bind");
    println!("listening on {addr}");
    axum::serve(listener, handlers::router(state)).await.expect("server");
}
