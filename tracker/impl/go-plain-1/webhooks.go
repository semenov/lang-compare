package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

var webhookEvents = []string{"issue.created", "issue.updated", "issue.deleted", "comment.created"}

const maxAttempts = 6

type Webhook struct {
	ID        string
	URL       string
	Events    []string
	Secret    string
	Active    bool
	CreatedAt time.Time
}

func (h *Webhook) JSON() map[string]any {
	return map[string]any{"id": h.ID, "url": h.URL, "events": h.Events, "active": h.Active, "created_at": fmtTS(h.CreatedAt)}
}

// ---- event recording (inside the transaction of the change)

type hookRef struct{ ID string }

func hooksFor(ctx context.Context, q querier, orgID, event string) ([]hookRef, error) {
	rows, err := q.Query(ctx, `SELECT id FROM webhooks WHERE org_id = $1 AND active AND $2 = ANY(events) ORDER BY seq`,
		orgID, event)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hookRef
	for rows.Next() {
		var h hookRef
		if err := rows.Scan(&h.ID); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

const insertDeliverySQL = `INSERT INTO deliveries (id, webhook_id, event, issue_id, body, status, next_attempt_at, created_at)
	VALUES ($1, $2, $3, $4, $5, 'pending', $6, $6)`

func deliveryBody(id, event string, org *Org, actor string, data any, at time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "event": event, "created_at": fmtTS(at), "org": org.Slug,
		"actor_id": actor, "data": data})
	return b
}

func queueDelivery(b *pgx.Batch, h hookRef, event string, org *Org, actor, issueID string, data any, at time.Time) {
	id := newID()
	b.Queue(insertDeliverySQL, id, h.ID, event, issueID, deliveryBody(id, event, org, actor, data, at), at)
}

func enqueue(ctx context.Context, tx pgx.Tx, org *Org, event, actor, issueID string, data any, at time.Time) error {
	hooks, err := hooksFor(ctx, tx, org.ID, event)
	if err != nil {
		return err
	}
	for _, h := range hooks {
		id := newID()
		if _, err := tx.Exec(ctx, insertDeliverySQL, id, h.ID, event, issueID,
			deliveryBody(id, event, org, actor, data, at), at); err != nil {
			return err
		}
	}
	return nil
}

// ---- dispatcher

type pendingDelivery struct {
	ID, WebhookID, IssueID, Event, URL, Secret string
	Body                                       []byte
	Attempts                                   int
}

func (d *pendingDelivery) queue() string { return d.WebhookID + "/" + d.IssueID }

// runDispatcher sends pending deliveries. Per (webhook, issue) queue only the oldest pending delivery is
// eligible, and at most one attempt per queue is in flight, which keeps per-issue event order.
func (s *Server) runDispatcher(ctx context.Context) {
	inflight := map[string]bool{}
	done := make(chan string, 64)
	var wg sync.WaitGroup
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Let in-flight attempts finish (bounded by the 5 s client timeout) so their outcome is recorded.
			wg.Wait()
			return
		case k := <-done:
			delete(inflight, k)
		case <-s.wake:
		case <-ticker.C:
		}
	drain:
		for {
			select {
			case k := <-done:
				delete(inflight, k)
			default:
				break drain
			}
		}
		ready, err := s.readyDeliveries(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("dispatcher: %v", err)
			}
			continue
		}
		for _, d := range ready {
			k := d.queue()
			if inflight[k] {
				continue
			}
			inflight[k] = true
			wg.Add(1)
			go func(d *pendingDelivery) {
				defer wg.Done()
				s.attempt(d)
				select {
				case done <- d.queue():
				case <-ctx.Done():
				}
			}(d)
		}
	}
}

func (s *Server) readyDeliveries(ctx context.Context) ([]*pendingDelivery, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.id, d.webhook_id, d.issue_id, d.event, w.url, w.secret, d.body, d.attempts
		FROM deliveries d JOIN webhooks w ON w.id = d.webhook_id
		WHERE d.status = 'pending' AND d.next_attempt_at <= $1
		AND NOT EXISTS (SELECT 1 FROM deliveries e WHERE e.webhook_id = d.webhook_id AND e.issue_id = d.issue_id
			AND e.status = 'pending' AND e.seq < d.seq)
		ORDER BY d.seq LIMIT 500`, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pendingDelivery
	for rows.Next() {
		d := &pendingDelivery{}
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.IssueID, &d.Event, &d.URL, &d.Secret, &d.Body, &d.Attempts); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Server) backoff(attempt int) time.Duration {
	secs := float64(int(1) << (attempt - 1)) // 1, 2, 4, 8, 16
	return time.Duration(secs * s.backoffScale * float64(time.Second))
}

func (s *Server) attempt(d *pendingDelivery) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(d.Secret))
	mac.Write([]byte(ts + "."))
	mac.Write(d.Body)
	var code *int
	ok := false
	req, err := http.NewRequest(http.MethodPost, d.URL, bytes.NewReader(d.Body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tracker-Event", d.Event)
		req.Header.Set("X-Tracker-Delivery", d.ID)
		req.Header.Set("X-Tracker-Timestamp", ts)
		req.Header.Set("X-Tracker-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := s.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			c := resp.StatusCode
			code = &c
			ok = c >= 200 && c < 300
		}
	}
	attempts := d.Attempts + 1
	status := "pending"
	next := time.Now()
	switch {
	case ok:
		status = "succeeded"
	case attempts >= maxAttempts:
		status = "failed"
	default:
		next = next.Add(s.backoff(attempts))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx, `UPDATE deliveries SET status = $2, attempts = $3, last_status_code = $4,
		next_attempt_at = $5 WHERE id = $1`, d.ID, status, attempts, code, next); err != nil {
		log.Printf("record delivery %s: %v", d.ID, err)
	}
}

// ---- CRUD

func (s *Server) adminOrg(r *http.Request, uid string) (*Org, error) {
	org, err := s.loadOrg(r.Context(), r.PathValue("slug"), uid)
	if err != nil {
		return nil, err
	}
	if !org.isAdmin() {
		return nil, forbidden()
	}
	return org, nil
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func parseEvents(o obj, v *verrs, required bool) ([]string, bool) {
	raw, ok := o["events"]
	if !ok {
		if required {
			v.add("events", "required")
		}
		return nil, false
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil || arr == nil {
		v.add("events", "invalid")
		return nil, false
	}
	if len(arr) == 0 {
		v.add("events", "too_short")
		return nil, false
	}
	var out []string
	for _, e := range arr {
		if !contains(webhookEvents, e) {
			v.add("events", "invalid")
			return nil, false
		}
		if !contains(out, e) {
			out = append(out, e)
		}
	}
	return out, true
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request, uid string) error {
	org, err := s.adminOrg(r, uid)
	if err != nil {
		return err
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	u, ust := v.str(o, "url", strSpec{required: true, trim: true, max: 2048})
	if ust == fSet && !validURL(u) {
		v.add("url", "invalid")
	}
	events, _ := parseEvents(o, v, true)
	secret, sst := v.str(o, "secret", strSpec{nullable: true, min: 1, max: 255})
	if !v.ok() {
		return v.err()
	}
	if sst != fSet {
		b := make([]byte, 24)
		rand.Read(b)
		secret = hex.EncodeToString(b)
	}
	h := &Webhook{ID: newID(), URL: u, Events: events, Secret: secret, Active: true, CreatedAt: nowMS()}
	if _, err := s.pool.Exec(r.Context(), `INSERT INTO webhooks (id, org_id, url, events, secret, active, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, h.ID, org.ID, h.URL, h.Events, h.Secret, h.Active, h.CreatedAt); err != nil {
		return err
	}
	resp := h.JSON()
	resp["secret"] = h.Secret
	writeJSON(w, 201, resp)
	return nil
}

const webhookSelect = `SELECT id, url, events, secret, active, created_at FROM webhooks`

func scanWebhook(row pgx.Row) (*Webhook, error) {
	h := &Webhook{}
	err := row.Scan(&h.ID, &h.URL, &h.Events, &h.Secret, &h.Active, &h.CreatedAt)
	return h, err
}

func (s *Server) loadWebhook(r *http.Request, uid string) (*Webhook, error) {
	org, err := s.adminOrg(r, uid)
	if err != nil {
		return nil, err
	}
	id := strings.ToLower(r.PathValue("id"))
	if !isUUID(id) {
		return nil, notFound()
	}
	h, err := scanWebhook(s.pool.QueryRow(r.Context(), webhookSelect+` WHERE id = $1 AND org_id = $2`, id, org.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	return h, err
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request, uid string) error {
	org, err := s.adminOrg(r, uid)
	if err != nil {
		return err
	}
	pg, err := pageFromRequest(r)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(r.Context(), webhookSelect+` WHERE org_id = $1 ORDER BY seq`+pg.sql(), org.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []map[string]any
	for rows.Next() {
		h, err := scanWebhook(rows)
		if err != nil {
			return err
		}
		items = append(items, h.JSON())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, finish(pg, items))
	return nil
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request, uid string) error {
	h, err := s.loadWebhook(r, uid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, h.JSON())
	return nil
}

func (s *Server) patchWebhook(w http.ResponseWriter, r *http.Request, uid string) error {
	h, err := s.loadWebhook(r, uid)
	if err != nil {
		return err
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	u, ust := v.str(o, "url", strSpec{trim: true, max: 2048})
	if ust == fSet && !validURL(u) {
		v.add("url", "invalid")
	}
	events, evOK := parseEvents(o, v, false)
	active, ast := v.boolean(o, "active")
	if !v.ok() {
		return v.err()
	}
	if ust == fSet {
		h.URL = u
	}
	if evOK {
		h.Events = events
	}
	if ast == fSet {
		h.Active = active
	}
	if _, err := s.pool.Exec(r.Context(), `UPDATE webhooks SET url = $2, events = $3, active = $4 WHERE id = $1`,
		h.ID, h.URL, h.Events, h.Active); err != nil {
		return err
	}
	writeJSON(w, 200, h.JSON())
	return nil
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request, uid string) error {
	h, err := s.loadWebhook(r, uid)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM webhooks WHERE id = $1`, h.ID); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request, uid string) error {
	h, err := s.loadWebhook(r, uid)
	if err != nil {
		return err
	}
	pg, err := pageFromRequest(r)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT id, event, status, attempts, last_status_code, created_at
		FROM deliveries WHERE webhook_id = $1 ORDER BY seq DESC`+pg.sql(), h.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []map[string]any
	for rows.Next() {
		var id, event, status string
		var attempts int
		var code *int
		var at time.Time
		if err := rows.Scan(&id, &event, &status, &attempts, &code, &at); err != nil {
			return err
		}
		items = append(items, map[string]any{"id": id, "event": event, "status": status, "attempts": attempts,
			"last_status_code": code, "created_at": fmtTS(at)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, finish(pg, items))
	return nil
}
