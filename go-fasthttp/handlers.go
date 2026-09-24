package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

type Server struct {
	store   *Store
	catalog *Catalog
}

func (s *Server) Routes() fasthttp.RequestHandler {
	r := router.New()
	r.GET("/health", func(ctx *fasthttp.RequestCtx) {
		writeJSON(ctx, fasthttp.StatusOK, map[string]string{"status": "ok"})
	})
	r.POST("/users", s.createUser)
	r.GET("/users/{id}", s.getUser)
	r.GET("/users/{id}/orders", s.listOrders)
	r.GET("/products/{sku}", s.proxyProduct)
	r.POST("/orders", s.createOrder)
	r.GET("/orders/{id}", s.getOrder)
	r.NotFound = func(ctx *fasthttp.RequestCtx) { writeError(ctx, fasthttp.StatusNotFound, "not found") }
	return r.Handler
}

func writeJSON(ctx *fasthttp.RequestCtx, status int, v any) {
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(status)
	json.NewEncoder(ctx).Encode(v)
}

func writeError(ctx *fasthttp.RequestCtx, status int, msg string) {
	writeJSON(ctx, status, map[string]string{"error": msg})
}

func internalError(ctx *fasthttp.RequestCtx, err error) {
	log.Printf("internal error: %v", err)
	writeError(ctx, fasthttp.StatusInternalServerError, "internal error")
}

func pathID(ctx *fasthttp.RequestCtx) (int64, bool) {
	raw, _ := ctx.UserValue("id").(string)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(ctx, fasthttp.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func decodeBody(ctx *fasthttp.RequestCtx, v any) bool {
	if err := json.Unmarshal(ctx.PostBody(), v); err != nil {
		writeError(ctx, fasthttp.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}

func (s *Server) createUser(ctx *fasthttp.RequestCtx) {
	var req struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if !decodeBody(ctx, &req) {
		return
	}
	if len(req.Email) < 3 || len(req.Email) > 254 || !strings.Contains(req.Email, "@") {
		writeError(ctx, fasthttp.StatusBadRequest, "invalid email")
		return
	}
	if n := len([]rune(req.Name)); n < 1 || n > 100 {
		writeError(ctx, fasthttp.StatusBadRequest, "name must be 1..100 chars")
		return
	}
	u, err := s.store.CreateUser(ctx, req.Email, req.Name)
	switch {
	case errors.Is(err, ErrDuplicate):
		writeError(ctx, fasthttp.StatusConflict, "email already exists")
	case err != nil:
		internalError(ctx, err)
	default:
		writeJSON(ctx, fasthttp.StatusCreated, u)
	}
}

func (s *Server) getUser(ctx *fasthttp.RequestCtx) {
	id, ok := pathID(ctx)
	if !ok {
		return
	}
	u, err := s.store.GetUser(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(ctx, fasthttp.StatusNotFound, "user not found")
	case err != nil:
		internalError(ctx, err)
	default:
		writeJSON(ctx, fasthttp.StatusOK, u)
	}
}

func queryInt(ctx *fasthttp.RequestCtx, key string, def, min, max int) (int, bool) {
	raw := ctx.QueryArgs().Peek(key)
	if len(raw) == 0 {
		return def, true
	}
	v, err := strconv.Atoi(string(raw))
	if err != nil || v < min || v > max {
		return 0, false
	}
	return v, true
}

func (s *Server) listOrders(ctx *fasthttp.RequestCtx) {
	id, ok := pathID(ctx)
	if !ok {
		return
	}
	limit, ok1 := queryInt(ctx, "limit", 20, 1, 100)
	offset, ok2 := queryInt(ctx, "offset", 0, 0, 1<<31-1)
	if !ok1 || !ok2 {
		writeError(ctx, fasthttp.StatusBadRequest, "invalid limit/offset")
		return
	}
	var (
		wg        sync.WaitGroup
		exists    bool
		existsErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		exists, existsErr = s.store.UserExists(ctx, id)
	}()
	orders, err := s.store.ListOrders(ctx, id, limit, offset)
	wg.Wait()
	if err == nil {
		err = existsErr
	}
	if err != nil {
		internalError(ctx, err)
		return
	}
	if !exists {
		writeError(ctx, fasthttp.StatusNotFound, "user not found")
		return
	}
	writeJSON(ctx, fasthttp.StatusOK, map[string]any{"orders": orders, "limit": limit, "offset": offset})
}

func (s *Server) proxyProduct(ctx *fasthttp.RequestCtx) {
	sku, _ := ctx.UserValue("sku").(string)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	if err := s.catalog.Get(sku, resp); err != nil {
		writeError(ctx, fasthttp.StatusBadGateway, "upstream unavailable")
		return
	}
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(resp.StatusCode())
	ctx.SetBody(resp.Body())
}

type createOrderReq struct {
	UserID int64 `json:"user_id"`
	Items  []struct {
		SKU string `json:"sku"`
		Qty int32  `json:"qty"`
	} `json:"items"`
}

func (s *Server) createOrder(ctx *fasthttp.RequestCtx) {
	var req createOrderReq
	if !decodeBody(ctx, &req) {
		return
	}
	if req.UserID <= 0 || len(req.Items) < 1 || len(req.Items) > 20 {
		writeError(ctx, fasthttp.StatusBadRequest, "user_id must be positive and items must have 1..20 entries")
		return
	}
	for _, it := range req.Items {
		if it.SKU == "" || it.Qty < 1 || it.Qty > 1000 {
			writeError(ctx, fasthttp.StatusBadRequest, "each item needs a sku and qty in 1..1000")
			return
		}
	}

	// user lookup and all catalog lookups run concurrently
	var wg sync.WaitGroup
	products := make([]*Product, len(req.Items))
	errs := make([]error, len(req.Items))
	for i, it := range req.Items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			products[i], errs[i] = s.catalog.Product(it.SKU)
		}()
	}
	exists, err := s.store.UserExists(ctx, req.UserID)
	wg.Wait()
	if err != nil {
		internalError(ctx, err)
		return
	}
	if !exists {
		writeError(ctx, fasthttp.StatusNotFound, "user not found")
		return
	}

	items := make([]OrderItem, len(req.Items))
	var total int64
	for i, it := range req.Items {
		if errors.Is(errs[i], ErrProductNotFound) {
			writeError(ctx, fasthttp.StatusUnprocessableEntity, "unknown sku: "+it.SKU)
			return
		}
		if errs[i] != nil {
			log.Printf("catalog error: %v", errs[i])
			writeError(ctx, fasthttp.StatusBadGateway, "upstream error")
			return
		}
		p := products[i]
		if int64(it.Qty) > p.Stock {
			writeError(ctx, fasthttp.StatusUnprocessableEntity, fmt.Sprintf("insufficient stock: %s", it.SKU))
			return
		}
		total += p.PriceCents * int64(it.Qty)
		items[i] = OrderItem{SKU: it.SKU, Name: p.Name, Qty: it.Qty, UnitPriceCents: p.PriceCents}
	}

	o, err := s.store.CreateOrder(ctx, req.UserID, products[0].Currency, total, items)
	if err != nil {
		internalError(ctx, err)
		return
	}
	writeJSON(ctx, fasthttp.StatusCreated, o)
}

func (s *Server) getOrder(ctx *fasthttp.RequestCtx) {
	id, ok := pathID(ctx)
	if !ok {
		return
	}
	o, err := s.store.GetOrder(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(ctx, fasthttp.StatusNotFound, "order not found")
	case err != nil:
		internalError(ctx, err)
	default:
		writeJSON(ctx, fasthttp.StatusOK, o)
	}
}
