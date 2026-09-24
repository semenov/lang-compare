package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/valyala/fasthttp"
)

type Product struct {
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	PriceCents int64  `json:"price_cents"`
	Currency   string `json:"currency"`
	Stock      int64  `json:"stock"`
}

var ErrProductNotFound = errors.New("product not found")

type Catalog struct {
	baseURL string
	client  *fasthttp.Client
}

func NewCatalog(baseURL string) *Catalog {
	return &Catalog{baseURL: baseURL, client: &fasthttp.Client{MaxConnsPerHost: 512}}
}

// Get fetches /products/{sku} into resp (caller acquires/releases it).
func (c *Catalog) Get(sku string, resp *fasthttp.Response) error {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(c.baseURL + "/products/" + url.PathEscape(sku))
	return c.client.DoTimeout(req, resp, 10*time.Second)
}

func (c *Catalog) Product(sku string) (*Product, error) {
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	if err := c.Get(sku, resp); err != nil {
		return nil, err
	}
	switch resp.StatusCode() {
	case fasthttp.StatusOK:
		var p Product
		if err := json.Unmarshal(resp.Body(), &p); err != nil {
			return nil, err
		}
		return &p, nil
	case fasthttp.StatusNotFound:
		return nil, ErrProductNotFound
	default:
		return nil, fmt.Errorf("catalog returned %d", resp.StatusCode())
	}
}
