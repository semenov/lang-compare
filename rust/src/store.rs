use chrono::{DateTime, Utc};
use serde::Serialize;
use sqlx::{FromRow, PgPool};

#[derive(Serialize, FromRow)]
pub struct User {
    pub id: i64,
    pub email: String,
    pub name: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Serialize, FromRow)]
pub struct OrderItem {
    pub sku: String,
    pub name: String,
    pub qty: i32,
    pub unit_price_cents: i64,
}

#[derive(Serialize, FromRow)]
pub struct Order {
    pub id: i64,
    pub user_id: i64,
    pub status: String,
    pub total_cents: i64,
    pub currency: String,
    pub created_at: DateTime<Utc>,
    #[sqlx(skip)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub items: Option<Vec<OrderItem>>,
}

const ORDER_COLS: &str = "id, user_id, status, total_cents, currency, created_at";

pub enum CreateUserError {
    Duplicate,
    Db(sqlx::Error),
}

#[derive(Clone)]
pub struct Store {
    db: PgPool,
}

impl Store {
    pub fn new(db: PgPool) -> Self {
        Self { db }
    }

    pub async fn create_user(&self, email: &str, name: &str) -> Result<User, CreateUserError> {
        sqlx::query_as("INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id, email, name, created_at")
            .bind(email)
            .bind(name)
            .fetch_one(&self.db)
            .await
            .map_err(|e| match &e {
                sqlx::Error::Database(d) if d.is_unique_violation() => CreateUserError::Duplicate,
                _ => CreateUserError::Db(e),
            })
    }

    pub async fn get_user(&self, id: i64) -> sqlx::Result<Option<User>> {
        sqlx::query_as("SELECT id, email, name, created_at FROM users WHERE id = $1")
            .bind(id)
            .fetch_optional(&self.db)
            .await
    }

    pub async fn user_exists(&self, id: i64) -> sqlx::Result<bool> {
        let row: Option<(i32,)> = sqlx::query_as("SELECT 1 FROM users WHERE id = $1")
            .bind(id)
            .fetch_optional(&self.db)
            .await?;
        Ok(row.is_some())
    }

    pub async fn create_order(
        &self,
        user_id: i64,
        currency: &str,
        total: i64,
        items: Vec<OrderItem>,
    ) -> sqlx::Result<Order> {
        let mut tx = self.db.begin().await?;
        let mut order: Order = sqlx::query_as(&format!(
            "INSERT INTO orders (user_id, total_cents, currency) VALUES ($1, $2, $3) RETURNING {ORDER_COLS}"
        ))
        .bind(user_id)
        .bind(total)
        .bind(currency)
        .fetch_one(&mut *tx)
        .await?;

        let skus: Vec<&str> = items.iter().map(|i| i.sku.as_str()).collect();
        let names: Vec<&str> = items.iter().map(|i| i.name.as_str()).collect();
        let qtys: Vec<i32> = items.iter().map(|i| i.qty).collect();
        let prices: Vec<i64> = items.iter().map(|i| i.unit_price_cents).collect();
        sqlx::query(
            "INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents)
             SELECT $1, * FROM unnest($2::text[], $3::text[], $4::int[], $5::bigint[])",
        )
        .bind(order.id)
        .bind(&skus)
        .bind(&names)
        .bind(&qtys)
        .bind(&prices)
        .execute(&mut *tx)
        .await?;
        tx.commit().await?;

        order.items = Some(items);
        Ok(order)
    }

    pub async fn get_order(&self, id: i64) -> sqlx::Result<Option<Order>> {
        let order: Option<Order> = sqlx::query_as(&format!("SELECT {ORDER_COLS} FROM orders WHERE id = $1"))
            .bind(id)
            .fetch_optional(&self.db)
            .await?;
        let Some(mut order) = order else { return Ok(None) };
        let items = sqlx::query_as(
            "SELECT sku, name, qty, unit_price_cents FROM order_items WHERE order_id = $1 ORDER BY id",
        )
        .bind(id)
        .fetch_all(&self.db)
        .await?;
        order.items = Some(items);
        Ok(Some(order))
    }

    pub async fn list_orders(&self, user_id: i64, limit: i64, offset: i64) -> sqlx::Result<Vec<Order>> {
        sqlx::query_as(&format!(
            "SELECT {ORDER_COLS} FROM orders WHERE user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3"
        ))
        .bind(user_id)
        .bind(limit)
        .bind(offset)
        .fetch_all(&self.db)
        .await
    }
}
