package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("duplicate")
)

type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type OrderItem struct {
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	Qty            int32  `json:"qty"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

type Order struct {
	ID         int64        `json:"id"`
	UserID     int64        `json:"user_id"`
	Status     string       `json:"status"`
	TotalCents int64        `json:"total_cents"`
	Currency   string       `json:"currency"`
	CreatedAt  time.Time    `json:"created_at"`
	Items      *[]OrderItem `json:"items,omitempty"`
}

const orderCols = "id, user_id, status, total_cents, currency, created_at"

type Store struct {
	db *pgxpool.Pool
}

func scanOrder(row pgx.Row, o *Order) error {
	return row.Scan(&o.ID, &o.UserID, &o.Status, &o.TotalCents, &o.Currency, &o.CreatedAt)
}

func (s *Store) CreateUser(ctx context.Context, email, name string) (*User, error) {
	var u User
	err := s.db.QueryRow(ctx,
		"INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id, email, name, created_at",
		email, name).Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrDuplicate
	}
	return &u, err
}

func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.db.QueryRow(ctx, "SELECT id, email, name, created_at FROM users WHERE id = $1", id).
		Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func (s *Store) UserExists(ctx context.Context, id int64) (bool, error) {
	var one int
	err := s.db.QueryRow(ctx, "SELECT 1 FROM users WHERE id = $1", id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) CreateOrder(ctx context.Context, userID int64, currency string, total int64, items []OrderItem) (*Order, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	o := Order{Items: &items}
	err = scanOrder(tx.QueryRow(ctx,
		"INSERT INTO orders (user_id, total_cents, currency) VALUES ($1, $2, $3) RETURNING "+orderCols,
		userID, total, currency), &o)
	if err != nil {
		return nil, err
	}
	skus := make([]string, len(items))
	names := make([]string, len(items))
	qtys := make([]int32, len(items))
	prices := make([]int64, len(items))
	for i, it := range items {
		skus[i], names[i], qtys[i], prices[i] = it.SKU, it.Name, it.Qty, it.UnitPriceCents
	}
	_, err = tx.Exec(ctx, `INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents)
		SELECT $1, * FROM unnest($2::text[], $3::text[], $4::int[], $5::bigint[])`,
		o.ID, skus, names, qtys, prices)
	if err != nil {
		return nil, err
	}
	return &o, tx.Commit(ctx)
}

func (s *Store) GetOrder(ctx context.Context, id int64) (*Order, error) {
	var o Order
	err := scanOrder(s.db.QueryRow(ctx, "SELECT "+orderCols+" FROM orders WHERE id = $1", id), &o)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx,
		"SELECT sku, name, qty, unit_price_cents FROM order_items WHERE order_id = $1 ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[OrderItem])
	if err != nil {
		return nil, err
	}
	o.Items = &items
	return &o, nil
}

func (s *Store) ListOrders(ctx context.Context, userID int64, limit, offset int) ([]Order, error) {
	rows, err := s.db.Query(ctx,
		"SELECT "+orderCols+" FROM orders WHERE user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3",
		userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var o Order
		if err := scanOrder(rows, &o); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}
