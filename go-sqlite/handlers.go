package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type Server struct {
	store   *Store
	catalog *Catalog
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /users", s.createUser)
	mux.HandleFunc("GET /users/{id}", s.getUser)
	mux.HandleFunc("GET /users/{id}/orders", s.listOrders)
	mux.HandleFunc("GET /products/{sku}", s.proxyProduct)
	mux.HandleFunc("POST /orders", s.createOrder)
	mux.HandleFunc("GET /orders/{id}", s.getOrder)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func internalError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Email) < 3 || len(req.Email) > 254 || !strings.Contains(req.Email, "@") {
		writeError(w, http.StatusBadRequest, "invalid email")
		return
	}
	if n := len([]rune(req.Name)); n < 1 || n > 100 {
		writeError(w, http.StatusBadRequest, "name must be 1..100 chars")
		return
	}
	u, err := s.store.CreateUser(r.Context(), req.Email, req.Name)
	switch {
	case errors.Is(err, ErrDuplicate):
		writeError(w, http.StatusConflict, "email already exists")
	case err != nil:
		internalError(w, err)
	default:
		writeJSON(w, http.StatusCreated, u)
	}
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	u, err := s.store.GetUser(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case err != nil:
		internalError(w, err)
	default:
		writeJSON(w, http.StatusOK, u)
	}
}

func queryInt(r *http.Request, key string, def, min, max int) (int, bool) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		return 0, false
	}
	return v, true
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	limit, ok1 := queryInt(r, "limit", 20, 1, 100)
	offset, ok2 := queryInt(r, "offset", 0, 0, 1<<31-1)
	if !ok1 || !ok2 {
		writeError(w, http.StatusBadRequest, "invalid limit/offset")
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
		exists, existsErr = s.store.UserExists(r.Context(), id)
	}()
	orders, err := s.store.ListOrders(r.Context(), id, limit, offset)
	wg.Wait()
	if err == nil {
		err = existsErr
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders, "limit": limit, "offset": offset})
}

func (s *Server) proxyProduct(w http.ResponseWriter, r *http.Request) {
	resp, err := s.catalog.Get(r.PathValue("sku"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

type createOrderReq struct {
	UserID int64 `json:"user_id"`
	Items  []struct {
		SKU string `json:"sku"`
		Qty int32  `json:"qty"`
	} `json:"items"`
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	var req createOrderReq
	if !decodeBody(w, r, &req) {
		return
	}
	if req.UserID <= 0 || len(req.Items) < 1 || len(req.Items) > 20 {
		writeError(w, http.StatusBadRequest, "user_id must be positive and items must have 1..20 entries")
		return
	}
	for _, it := range req.Items {
		if it.SKU == "" || it.Qty < 1 || it.Qty > 1000 {
			writeError(w, http.StatusBadRequest, "each item needs a sku and qty in 1..1000")
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
	exists, err := s.store.UserExists(r.Context(), req.UserID)
	wg.Wait()
	if err != nil {
		internalError(w, err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	items := make([]OrderItem, len(req.Items))
	var total int64
	for i, it := range req.Items {
		if errors.Is(errs[i], ErrProductNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "unknown sku: "+it.SKU)
			return
		}
		if errs[i] != nil {
			log.Printf("catalog error: %v", errs[i])
			writeError(w, http.StatusBadGateway, "upstream error")
			return
		}
		p := products[i]
		if int64(it.Qty) > p.Stock {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("insufficient stock: %s", it.SKU))
			return
		}
		total += p.PriceCents * int64(it.Qty)
		items[i] = OrderItem{SKU: it.SKU, Name: p.Name, Qty: it.Qty, UnitPriceCents: p.PriceCents}
	}

	o, err := s.store.CreateOrder(r.Context(), req.UserID, products[0].Currency, total, items)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, err := s.store.GetOrder(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "order not found")
	case err != nil:
		internalError(w, err)
	default:
		writeJSON(w, http.StatusOK, o)
	}
}
