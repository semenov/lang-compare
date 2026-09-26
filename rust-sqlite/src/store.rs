use deadpool_sqlite::{
    Config, Hook, HookError, Pool, Runtime,
    rusqlite::{self, OptionalExtension, TransactionBehavior, params},
};
use serde::Serialize;

#[derive(Serialize)]
pub struct User {
    pub id: i64,
    pub email: String,
    pub name: String,
    pub created_at: String,
}

#[derive(Serialize)]
pub struct OrderItem {
    pub sku: String,
    pub name: String,
    pub qty: i32,
    pub unit_price_cents: i64,
}

#[derive(Serialize)]
pub struct Order {
    pub id: i64,
    pub user_id: i64,
    pub status: String,
    pub total_cents: i64,
    pub currency: String,
    pub created_at: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub items: Option<Vec<OrderItem>>,
}

const ORDER_COLS: &str = "id, user_id, status, total_cents, currency, created_at";

fn order_from_row(r: &rusqlite::Row) -> rusqlite::Result<Order> {
    Ok(Order {
        id: r.get(0)?,
        user_id: r.get(1)?,
        status: r.get(2)?,
        total_cents: r.get(3)?,
        currency: r.get(4)?,
        created_at: r.get(5)?,
        items: None,
    })
}

#[derive(Debug)]
pub struct StoreError(pub String);

impl<E: std::fmt::Display> From<E> for StoreError {
    fn from(e: E) -> Self {
        StoreError(e.to_string())
    }
}

pub type StoreResult<T> = Result<T, StoreError>;

pub enum CreateUserError {
    Duplicate,
    Db(StoreError),
}

/// rusqlite is blocking; deadpool-sqlite runs each closure on tokio's blocking thread pool.
/// SQLite allows one writer at a time: a single write connection plus a pool of readers (WAL).
#[derive(Clone)]
pub struct Store {
    w: Pool,
    r: Pool,
}

fn pool(path: &str, size: usize) -> Pool {
    Config::new(path)
        .builder(Runtime::Tokio1)
        .expect("sqlite pool config")
        .max_size(size)
        .post_create(Hook::async_fn(|conn, _| {
            Box::pin(async move {
                conn.interact(|c| {
                    c.execute_batch("PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL; PRAGMA busy_timeout = 5000;")
                })
                .await
                .map_err(|e| HookError::Message(e.to_string().into()))?
                .map_err(HookError::Backend)
            })
        }))
        .build()
        .expect("sqlite pool")
}

impl Store {
    pub fn new(path: &str, readers: usize) -> Self {
        Self { w: pool(path, 1), r: pool(path, readers) }
    }

    async fn read<T: Send + 'static>(
        &self,
        f: impl FnOnce(&mut rusqlite::Connection) -> rusqlite::Result<T> + Send + 'static,
    ) -> StoreResult<T> {
        Ok(self.r.get().await?.interact(f).await??)
    }

    async fn write<T: Send + 'static>(
        &self,
        f: impl FnOnce(&mut rusqlite::Connection) -> rusqlite::Result<T> + Send + 'static,
    ) -> Result<rusqlite::Result<T>, StoreError> {
        Ok(self.w.get().await?.interact(f).await?)
    }

    pub async fn init(&self, script: String) -> StoreResult<()> {
        Ok(self.write(move |c| c.execute_batch(&script)).await??)
    }

    pub async fn create_user(&self, email: String, name: String) -> Result<User, CreateUserError> {
        let res = self
            .write(move |c| {
                c.prepare_cached("INSERT INTO users (email, name) VALUES (?1, ?2) RETURNING id, email, name, created_at")?
                    .query_row(params![email, name], |r| {
                        Ok(User { id: r.get(0)?, email: r.get(1)?, name: r.get(2)?, created_at: r.get(3)? })
                    })
            })
            .await
            .map_err(CreateUserError::Db)?;
        res.map_err(|e| match e.sqlite_error_code() {
            Some(rusqlite::ErrorCode::ConstraintViolation) => CreateUserError::Duplicate,
            _ => CreateUserError::Db(e.into()),
        })
    }

    pub async fn get_user(&self, id: i64) -> StoreResult<Option<User>> {
        self.read(move |c| {
            c.prepare_cached("SELECT id, email, name, created_at FROM users WHERE id = ?1")?
                .query_row([id], |r| {
                    Ok(User { id: r.get(0)?, email: r.get(1)?, name: r.get(2)?, created_at: r.get(3)? })
                })
                .optional()
        })
        .await
    }

    pub async fn user_exists(&self, id: i64) -> StoreResult<bool> {
        self.read(move |c| {
            c.prepare_cached("SELECT 1 FROM users WHERE id = ?1")?
                .query_row([id], |_| Ok(()))
                .optional()
                .map(|r| r.is_some())
        })
        .await
    }

    pub async fn create_order(
        &self,
        user_id: i64,
        currency: String,
        total: i64,
        items: Vec<OrderItem>,
    ) -> StoreResult<Order> {
        Ok(self
            .write(move |c| {
                let tx = c.transaction_with_behavior(TransactionBehavior::Immediate)?;
                let mut order = tx
                    .prepare_cached(&format!(
                        "INSERT INTO orders (user_id, total_cents, currency) VALUES (?1, ?2, ?3) RETURNING {ORDER_COLS}"
                    ))?
                    .query_row(params![user_id, total, currency], order_from_row)?;
                {
                    let mut stmt = tx.prepare_cached(
                        "INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents) VALUES (?1, ?2, ?3, ?4, ?5)",
                    )?;
                    for i in &items {
                        stmt.execute(params![order.id, i.sku, i.name, i.qty, i.unit_price_cents])?;
                    }
                }
                tx.commit()?;
                order.items = Some(items);
                Ok(order)
            })
            .await??)
    }

    pub async fn get_order(&self, id: i64) -> StoreResult<Option<Order>> {
        self.read(move |c| {
            let Some(mut order) = c
                .prepare_cached(&format!("SELECT {ORDER_COLS} FROM orders WHERE id = ?1"))?
                .query_row([id], order_from_row)
                .optional()?
            else {
                return Ok(None);
            };
            let items = c
                .prepare_cached("SELECT sku, name, qty, unit_price_cents FROM order_items WHERE order_id = ?1 ORDER BY id")?
                .query_map([id], |r| {
                    Ok(OrderItem { sku: r.get(0)?, name: r.get(1)?, qty: r.get(2)?, unit_price_cents: r.get(3)? })
                })?
                .collect::<rusqlite::Result<Vec<_>>>()?;
            order.items = Some(items);
            Ok(Some(order))
        })
        .await
    }

    pub async fn list_orders(&self, user_id: i64, limit: i64, offset: i64) -> StoreResult<Vec<Order>> {
        self.read(move |c| {
            c.prepare_cached(&format!(
                "SELECT {ORDER_COLS} FROM orders WHERE user_id = ?1 ORDER BY id DESC LIMIT ?2 OFFSET ?3"
            ))?
            .query_map([user_id, limit, offset], order_from_row)?
            .collect()
        })
        .await
    }
}
