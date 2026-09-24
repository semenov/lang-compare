package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
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
	client  *http.Client
}

func NewCatalog(baseURL string) *Catalog {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 512
	tr.MaxIdleConnsPerHost = 512
	return &Catalog{baseURL: baseURL, client: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

// Get performs a raw request; the caller owns resp.Body.
func (c *Catalog) Get(sku string) (*http.Response, error) {
	return c.client.Get(c.baseURL + "/products/" + url.PathEscape(sku))
}

func (c *Catalog) Product(sku string) (*Product, error) {
	resp, err := c.Get(sku)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var p Product
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			return nil, err
		}
		return &p, nil
	case http.StatusNotFound:
		return nil, ErrProductNotFound
	default:
		return nil, fmt.Errorf("catalog returned %d", resp.StatusCode)
	}
}
