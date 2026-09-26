package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mattn/go-sqlite3"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("duplicate")
)

type User struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
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
	CreatedAt  string       `json:"created_at"`
	Items      *[]OrderItem `json:"items,omitempty"`
}

const orderCols = "id, user_id, status, total_cents, currency, created_at"

type scanner interface{ Scan(dest ...any) error }

func scanOrder(row scanner, o *Order) error {
	return row.Scan(&o.ID, &o.UserID, &o.Status, &o.TotalCents, &o.Currency, &o.CreatedAt)
}

// Store keeps separate write (single connection) and read pools; see main.go.
type Store struct {
	w, r *sql.DB
}

func (s *Store) CreateUser(ctx context.Context, email, name string) (*User, error) {
	var u User
	err := s.w.QueryRowContext(ctx,
		"INSERT INTO users (email, name) VALUES (?, ?) RETURNING id, email, name, created_at",
		email, name).Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	var sqlErr sqlite3.Error
	if errors.As(err, &sqlErr) && sqlErr.ExtendedCode == sqlite3.ErrConstraintUnique {
		return nil, ErrDuplicate
	}
	return &u, err
}

func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.r.QueryRowContext(ctx, "SELECT id, email, name, created_at FROM users WHERE id = ?", id).
		Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func (s *Store) UserExists(ctx context.Context, id int64) (bool, error) {
	var one int
	err := s.r.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id = ?", id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) CreateOrder(ctx context.Context, userID int64, currency string, total int64, items []OrderItem) (*Order, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	o := Order{Items: &items}
	err = scanOrder(tx.QueryRowContext(ctx,
		"INSERT INTO orders (user_id, total_cents, currency) VALUES (?, ?, ?) RETURNING "+orderCols,
		userID, total, currency), &o)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, it := range items {
		if _, err := stmt.ExecContext(ctx, o.ID, it.SKU, it.Name, it.Qty, it.UnitPriceCents); err != nil {
			return nil, err
		}
	}
	return &o, tx.Commit()
}

func (s *Store) GetOrder(ctx context.Context, id int64) (*Order, error) {
	var o Order
	err := scanOrder(s.r.QueryRowContext(ctx, "SELECT "+orderCols+" FROM orders WHERE id = ?", id), &o)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx,
		"SELECT sku, name, qty, unit_price_cents FROM order_items WHERE order_id = ? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []OrderItem{}
	for rows.Next() {
		var it OrderItem
		if err := rows.Scan(&it.SKU, &it.Name, &it.Qty, &it.UnitPriceCents); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	o.Items = &items
	return &o, rows.Err()
}

func (s *Store) ListOrders(ctx context.Context, userID int64, limit, offset int) ([]Order, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT "+orderCols+" FROM orders WHERE user_id = ? ORDER BY id DESC LIMIT ? OFFSET ?",
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
