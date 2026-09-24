use axum::{
    Json,
    http::StatusCode,
    response::{IntoResponse, Response},
};
use serde_json::json;

#[derive(Debug)]
pub enum AppError {
    BadRequest(String),
    NotFound(&'static str),
    Conflict(&'static str),
    Unprocessable(String),
    BadGateway(String),
    Internal(String),
}

impl IntoResponse for AppError {
    fn into_response(self) -> Response {
        let (status, msg) = match self {
            AppError::BadRequest(m) => (StatusCode::BAD_REQUEST, m),
            AppError::NotFound(m) => (StatusCode::NOT_FOUND, m.to_string()),
            AppError::Conflict(m) => (StatusCode::CONFLICT, m.to_string()),
            AppError::Unprocessable(m) => (StatusCode::UNPROCESSABLE_ENTITY, m),
            AppError::BadGateway(m) => {
                eprintln!("upstream error: {m}");
                (StatusCode::BAD_GATEWAY, "upstream error".to_string())
            }
            AppError::Internal(m) => {
                eprintln!("internal error: {m}");
                (StatusCode::INTERNAL_SERVER_ERROR, "internal error".to_string())
            }
        };
        (status, Json(json!({ "error": msg }))).into_response()
    }
}

impl From<sqlx::Error> for AppError {
    fn from(e: sqlx::Error) -> Self {
        AppError::Internal(e.to_string())
    }
}
