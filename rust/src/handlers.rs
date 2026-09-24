use std::collections::HashMap;

use axum::{
    Json, Router,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{StatusCode, header},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use futures::future::join_all;
use serde::Deserialize;
use serde_json::json;

use crate::{
    AppState,
    catalog::ProductError,
    error::AppError,
    store::{CreateUserError, OrderItem},
};

type AppResult<T> = Result<T, AppError>;

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/health", get(|| async { Json(json!({ "status": "ok" })) }))
        .route("/users", post(create_user))
        .route("/users/{id}", get(get_user))
        .route("/users/{id}/orders", get(list_orders))
        .route("/products/{sku}", get(proxy_product))
        .route("/orders", post(create_order))
        .route("/orders/{id}", get(get_order))
        .with_state(state)
}

fn parse_id(raw: &str) -> AppResult<i64> {
    match raw.parse::<i64>() {
        Ok(id) if id > 0 => Ok(id),
        _ => Err(AppError::BadRequest("invalid id".into())),
    }
}

fn body<T>(payload: Result<Json<T>, JsonRejection>) -> AppResult<T> {
    payload
        .map(|Json(v)| v)
        .map_err(|e| AppError::BadRequest(e.body_text()))
}

#[derive(Deserialize)]
struct CreateUser {
    email: String,
    name: String,
}

async fn create_user(
    State(s): State<AppState>,
    payload: Result<Json<CreateUser>, JsonRejection>,
) -> AppResult<impl IntoResponse> {
    let req = body(payload)?;
    if req.email.len() < 3 || req.email.len() > 254 || !req.email.contains('@') {
        return Err(AppError::BadRequest("invalid email".into()));
    }
    let name_len = req.name.chars().count();
    if !(1..=100).contains(&name_len) {
        return Err(AppError::BadRequest("name must be 1..100 chars".into()));
    }
    match s.store.create_user(&req.email, &req.name).await {
        Ok(u) => Ok((StatusCode::CREATED, Json(u))),
        Err(CreateUserError::Duplicate) => Err(AppError::Conflict("email already exists")),
        Err(CreateUserError::Db(e)) => Err(e.into()),
    }
}

async fn get_user(State(s): State<AppState>, Path(id): Path<String>) -> AppResult<impl IntoResponse> {
    let id = parse_id(&id)?;
    let user = s.store.get_user(id).await?.ok_or(AppError::NotFound("user not found"))?;
    Ok(Json(user))
}

fn query_int(q: &HashMap<String, String>, key: &str, default: i64, min: i64, max: i64) -> AppResult<i64> {
    match q.get(key) {
        None => Ok(default),
        Some(raw) => match raw.parse::<i64>() {
            Ok(v) if (min..=max).contains(&v) => Ok(v),
            _ => Err(AppError::BadRequest(format!("invalid {key}"))),
        },
    }
}

async fn list_orders(
    State(s): State<AppState>,
    Path(id): Path<String>,
    Query(q): Query<HashMap<String, String>>,
) -> AppResult<impl IntoResponse> {
    let id = parse_id(&id)?;
    let limit = query_int(&q, "limit", 20, 1, 100)?;
    let offset = query_int(&q, "offset", 0, 0, i32::MAX as i64)?;
    let (exists, orders) = tokio::join!(s.store.user_exists(id), s.store.list_orders(id, limit, offset));
    if !exists? {
        return Err(AppError::NotFound("user not found"));
    }
    Ok(Json(json!({ "orders": orders?, "limit": limit, "offset": offset })))
}

async fn proxy_product(State(s): State<AppState>, Path(sku): Path<String>) -> Response {
    match s.catalog.get_raw(&sku).await {
        Ok((status, bytes)) => (status, [(header::CONTENT_TYPE, "application/json")], bytes).into_response(),
        Err(e) => AppError::BadGateway(e.to_string()).into_response(),
    }
}

#[derive(Deserialize)]
struct CreateOrderItem {
    sku: String,
    qty: i32,
}

#[derive(Deserialize)]
struct CreateOrder {
    user_id: i64,
    items: Vec<CreateOrderItem>,
}

async fn create_order(
    State(s): State<AppState>,
    payload: Result<Json<CreateOrder>, JsonRejection>,
) -> AppResult<impl IntoResponse> {
    let req = body(payload)?;
    if req.user_id <= 0 || req.items.is_empty() || req.items.len() > 20 {
        return Err(AppError::BadRequest(
            "user_id must be positive and items must have 1..20 entries".into(),
        ));
    }
    if req.items.iter().any(|i| i.sku.is_empty() || !(1..=1000).contains(&i.qty)) {
        return Err(AppError::BadRequest("each item needs a sku and qty in 1..1000".into()));
    }

    // user lookup and all catalog lookups run concurrently
    let (exists, products) = tokio::join!(
        s.store.user_exists(req.user_id),
        join_all(req.items.iter().map(|i| s.catalog.product(&i.sku)))
    );
    if !exists? {
        return Err(AppError::NotFound("user not found"));
    }

    let mut total = 0i64;
    let mut items = Vec::with_capacity(req.items.len());
    let mut currency = String::new();
    for (item, product) in req.items.into_iter().zip(products) {
        let p = match product {
            Ok(p) => p,
            Err(ProductError::NotFound) => return Err(AppError::Unprocessable(format!("unknown sku: {}", item.sku))),
            Err(ProductError::Upstream(e)) => return Err(AppError::BadGateway(e)),
        };
        if i64::from(item.qty) > p.stock {
            return Err(AppError::Unprocessable(format!("insufficient stock: {}", item.sku)));
        }
        total += p.price_cents * i64::from(item.qty);
        currency = p.currency;
        items.push(OrderItem { sku: item.sku, name: p.name, qty: item.qty, unit_price_cents: p.price_cents });
    }

    let order = s.store.create_order(req.user_id, &currency, total, items).await?;
    Ok((StatusCode::CREATED, Json(order)))
}

async fn get_order(State(s): State<AppState>, Path(id): Path<String>) -> AppResult<impl IntoResponse> {
    let id = parse_id(&id)?;
    let order = s.store.get_order(id).await?.ok_or(AppError::NotFound("order not found"))?;
    Ok(Json(order))
}
