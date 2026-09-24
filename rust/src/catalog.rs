use bytes::Bytes;
use reqwest::{Client, StatusCode};
use serde::Deserialize;

#[derive(Debug, Deserialize)]
pub struct Product {
    pub name: String,
    pub price_cents: i64,
    pub currency: String,
    pub stock: i64,
}

pub enum ProductError {
    NotFound,
    Upstream(String),
}

#[derive(Clone)]
pub struct Catalog {
    base_url: String,
    client: Client,
}

impl Catalog {
    pub fn new(base_url: String) -> Self {
        let client = Client::builder()
            .pool_max_idle_per_host(512)
            .timeout(std::time::Duration::from_secs(10))
            .build()
            .expect("http client");
        Self { base_url, client }
    }

    fn url(&self, sku: &str) -> String {
        let mut url = reqwest::Url::parse(&self.base_url).expect("CATALOG_URL");
        url.path_segments_mut().expect("base url").push("products").push(sku);
        url.into()
    }

    /// Raw pass-through fetch for the proxy endpoint.
    pub async fn get_raw(&self, sku: &str) -> reqwest::Result<(StatusCode, Bytes)> {
        let resp = self.client.get(self.url(sku)).send().await?;
        let status = resp.status();
        Ok((status, resp.bytes().await?))
    }

    pub async fn product(&self, sku: &str) -> Result<Product, ProductError> {
        let resp = self
            .client
            .get(self.url(sku))
            .send()
            .await
            .map_err(|e| ProductError::Upstream(e.to_string()))?;
        match resp.status() {
            StatusCode::OK => resp.json().await.map_err(|e| ProductError::Upstream(e.to_string())),
            StatusCode::NOT_FOUND => Err(ProductError::NotFound),
            s => Err(ProductError::Upstream(format!("catalog returned {s}"))),
        }
    }
}
