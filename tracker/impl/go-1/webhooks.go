package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	evIssueCreated   = "issue.created"
	evIssueUpdated   = "issue.updated"
	evIssueDeleted   = "issue.deleted"
	evCommentCreated = "comment.created"
)

var allEvents = []string{evIssueCreated, evIssueUpdated, evIssueDeleted, evCommentCreated}

// ---------------------------------------------------------------------------------------------------------------
// per-org cache of active webhook subscriptions

type hookSub struct {
	id     UUID
	events []string
}

var hookCache = struct {
	sync.Mutex
	m   map[UUID][]hookSub
	gen map[UUID]uint64
}{m: map[UUID][]hookSub{}, gen: map[UUID]uint64{}}

func invalidateHooks(org UUID) {
	hookCache.Lock()
	delete(hookCache.m, org)
	hookCache.gen[org]++
	hookCache.Unlock()
}

func orgHooks(ctx context.Context, org UUID) ([]hookSub, error) {
	hookCache.Lock()
	hs, ok := hookCache.m[org]
	gen := hookCache.gen[org]
	hookCache.Unlock()
	if ok {
		return hs, nil
	}
	rows, err := db.Query(ctx, "SELECT id, events FROM webhooks WHERE org_id=$1 AND active ORDER BY id", org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hs = []hookSub{}
	for rows.Next() {
		var h hookSub
		if err := rows.Scan(&h.id, &h.events); err != nil {
			return nil, err
		}
		hs = append(hs, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hookCache.Lock()
	if hookCache.gen[org] == gen {
		hookCache.m[org] = hs
	}
	hookCache.Unlock()
	return hs, nil
}

func hasHooks(ctx context.Context, org UUID, event string) bool {
	hs, err := orgHooks(ctx, org)
	if err != nil {
		return true // let addEvents surface the error
	}
	for _, h := range hs {
		if slices.Contains(h.events, event) {
			return true
		}
	}
	return false
}

type evItem struct {
	issueID UUID
	data    []byte
}

type pendingDelivery struct {
	id, webhook, issue UUID
}

// addEvents records deliveries for every subscribed webhook inside the caller's transaction.
func addEvents(ctx context.Context, tx pgx.Tx, org UUID, slug, event string, actor UUID, at time.Time,
	items []evItem) ([]pendingDelivery, error) {
	hs, err := orgHooks(ctx, org)
	if err != nil {
		return nil, err
	}
	var subs []UUID
	for _, h := range hs {
		if slices.Contains(h.events, event) {
			subs = append(subs, h.id)
		}
	}
	if len(subs) == 0 {
		return nil, nil
	}
	n := len(subs) * len(items)
	ids := make([]UUID, 0, n)
	hooks := make([]UUID, 0, n)
	issues := make([]UUID, 0, n)
	bodies := make([][]byte, 0, n)
	pend := make([]pendingDelivery, 0, n)
	prefix := make([]byte, 0, 200)
	prefix = append(prefix, `","event":"`...)
	prefix = append(prefix, event...)
	prefix = append(prefix, `","created_at":`...)
	prefix = appendTime(prefix, at)
	prefix = append(prefix, `,"org":`...)
	prefix = appendStr(prefix, slug)
	prefix = append(prefix, `,"actor_id":"`...)
	prefix = appendUUID(prefix, actor)
	prefix = append(prefix, `","data":`...)
	for _, it := range items {
		for _, h := range subs {
			id := newID()
			b := make([]byte, 0, len(prefix)+len(it.data)+50)
			b = append(b, `{"id":"`...)
			b = appendUUID(b, id)
			b = append(b, prefix...)
			b = append(b, it.data...)
			b = append(b, '}')
			ids = append(ids, id)
			hooks = append(hooks, h)
			issues = append(issues, it.issueID)
			bodies = append(bodies, b)
			pend = append(pend, pendingDelivery{id, h, it.issueID})
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO deliveries (id, webhook_id, issue_id, event, body, created_at, next_at)
		SELECT unnest($1::uuid[]), unnest($2::uuid[]), unnest($3::uuid[]), $4, unnest($5::bytea[]), $6, $6`,
		ids, hooks, issues, event, bodies, at)
	if err != nil {
		return nil, err
	}
	return pend, nil
}

// ---------------------------------------------------------------------------------------------------------------
// CRUD

func appendHook(b []byte, id UUID, u string, events []string, active bool, created time.Time, secret *string) []byte {
	b = append(b, `{"id":"`...)
	b = appendUUID(b, id)
	b = append(b, `","url":`...)
	b = appendStr(b, u)
	b = append(b, `,"events":`...)
	b = appendStrList(b, events)
	b = append(b, `,"active":`...)
	b = strconv.AppendBool(b, active)
	b = append(b, `,"created_at":`...)
	b = appendTime(b, created)
	if secret != nil {
		b = append(b, `,"secret":`...)
		b = appendStr(b, *secret)
	}
	return append(b, '}')
}

func validURL(v *verrs, raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isNull(raw) {
		v.add("url", "required")
		return "", false
	}
	s, ok := str(raw)
	if !ok {
		v.add("url", "invalid")
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(s) > 2048 {
		v.add("url", "invalid")
		return "", false
	}
	return s, true
}

func validEvents(v *verrs, raw json.RawMessage) ([]string, bool) {
	var arr []json.RawMessage
	if len(raw) == 0 || isNull(raw) {
		v.add("events", "required")
		return nil, false
	}
	if raw[0] != '[' || json.Unmarshal(raw, &arr) != nil {
		v.add("events", "invalid")
		return nil, false
	}
	if len(arr) == 0 {
		v.add("events", "too_short")
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for i, e := range arr {
		s, ok := str(e)
		if !ok || !slices.Contains(allEvents, s) {
			v.add("events."+strconv.Itoa(i), "invalid")
			return nil, false
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, true
}

func hookAdmin(w http.ResponseWriter, r *http.Request, uid UUID) (UUID, bool) {
	oid, role, ok := orgAccess(w, r.Context(), r.PathValue("slug"), uid)
	if !ok {
		return oid, false
	}
	if role < orgAdmin {
		errForbidden(w)
		return oid, false
	}
	return oid, true
}

func hCreateHook(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		URL, Events, Secret json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	oid, ok := hookAdmin(w, r, uid)
	if !ok {
		return
	}
	var v verrs
	u, _ := validURL(&v, in.URL)
	events, _ := validEvents(&v, in.Events)
	secret := ""
	if len(in.Secret) > 0 && !isNull(in.Secret) {
		s, ok := str(in.Secret)
		if !ok || len(s) > 1024 {
			v.add("secret", "invalid")
		}
		secret = s
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	if secret == "" {
		secret = "whsec_" + randToken(24)
	}
	id := newID()
	now := nowMs()
	if _, err := db.Exec(ctx, `INSERT INTO webhooks (id, org_id, url, events, secret, active, created_at)
		VALUES ($1,$2,$3,$4,$5,true,$6)`, id, oid, u, events, secret, now); err != nil {
		errInternal(w, err)
		return
	}
	invalidateHooks(oid)
	writeJSON(w, 201, appendHook(nil, id, u, events, true, now, &secret))
}

func hListHooks(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	oid, ok := hookAdmin(w, r, uid)
	if !ok {
		return
	}
	var after UUID
	if cs := r.URL.Query().Get("cursor"); cs != "" {
		s, _ := decCursor(cs)
		if after, ok = parseUUID(s); !ok {
			errValidation1(w, "cursor", "invalid")
			return
		}
	}
	rows, err := db.Query(ctx, `SELECT id, url, events, active, created_at FROM webhooks WHERE org_id=$1 AND id > $2
		ORDER BY id LIMIT $3`, oid, after, limit+1)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer rows.Close()
	var items [][]byte
	var last UUID
	next := ""
	for rows.Next() {
		var id UUID
		var u string
		var events []string
		var active bool
		var created time.Time
		if err := rows.Scan(&id, &u, &events, &active, &created); err != nil {
			errInternal(w, err)
			return
		}
		if len(items) == limit {
			next = uuidStr(last)
			break
		}
		items = append(items, appendHook(nil, id, u, events, active, created, nil))
		last = id
	}
	if err := rows.Err(); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendPage(nil, items, next))
}

func loadHook(w http.ResponseWriter, r *http.Request, oid UUID) (UUID, string, []string, bool, time.Time, bool) {
	var u string
	var events []string
	var active bool
	var created time.Time
	id, ok := parseUUID(r.PathValue("id"))
	if !ok {
		errNotFound(w)
		return id, "", nil, false, created, false
	}
	err := db.QueryRow(r.Context(), "SELECT url, events, active, created_at FROM webhooks WHERE id=$1 AND org_id=$2",
		id, oid).Scan(&u, &events, &active, &created)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return id, "", nil, false, created, false
	}
	if err != nil {
		errInternal(w, err)
		return id, "", nil, false, created, false
	}
	return id, u, events, active, created, true
}

func hGetHook(w http.ResponseWriter, r *http.Request, uid UUID) {
	oid, ok := hookAdmin(w, r, uid)
	if !ok {
		return
	}
	id, u, events, active, created, ok := loadHook(w, r, oid)
	if !ok {
		return
	}
	writeJSON(w, 200, appendHook(nil, id, u, events, active, created, nil))
}

func hPatchHook(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		URL, Events, Active json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	oid, ok := hookAdmin(w, r, uid)
	if !ok {
		return
	}
	id, u, events, active, created, ok := loadHook(w, r, oid)
	if !ok {
		return
	}
	var v verrs
	if len(in.URL) > 0 {
		if s, ok := validURL(&v, in.URL); ok {
			u = s
		}
	}
	if len(in.Events) > 0 {
		if e, ok := validEvents(&v, in.Events); ok {
			events = e
		}
	}
	if len(in.Active) > 0 {
		switch string(in.Active) {
		case "true":
			active = true
		case "false":
			active = false
		default:
			v.add("active", "invalid")
		}
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	if _, err := db.Exec(ctx, "UPDATE webhooks SET url=$2, events=$3, active=$4 WHERE id=$1", id, u, events,
		active); err != nil {
		errInternal(w, err)
		return
	}
	invalidateHooks(oid)
	writeJSON(w, 200, appendHook(nil, id, u, events, active, created, nil))
}

func hDeleteHook(w http.ResponseWriter, r *http.Request, uid UUID) {
	oid, ok := hookAdmin(w, r, uid)
	if !ok {
		return
	}
	id, ok := parseUUID(r.PathValue("id"))
	if !ok {
		errNotFound(w)
		return
	}
	tag, err := db.Exec(r.Context(), "DELETE FROM webhooks WHERE id=$1 AND org_id=$2", id, oid)
	if err != nil {
		errInternal(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		errNotFound(w)
		return
	}
	invalidateHooks(oid)
	w.WriteHeader(204)
}

var deliveryStatusNames = []string{"pending", "succeeded", "failed"}

func hListDeliveries(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	oid, ok := hookAdmin(w, r, uid)
	if !ok {
		return
	}
	hid, ok := parseUUID(r.PathValue("id"))
	if !ok {
		errNotFound(w)
		return
	}
	var exists bool
	if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM webhooks WHERE id=$1 AND org_id=$2)", hid, oid).
		Scan(&exists); err != nil {
		errInternal(w, err)
		return
	}
	if !exists {
		errNotFound(w)
		return
	}
	before := UUID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	if cs := r.URL.Query().Get("cursor"); cs != "" {
		s, _ := decCursor(cs)
		if before, ok = parseUUID(s); !ok {
			errValidation1(w, "cursor", "invalid")
			return
		}
	}
	rows, err := db.Query(ctx, `SELECT id, event, status, attempts, last_status_code, created_at FROM deliveries
		WHERE webhook_id=$1 AND id < $2 ORDER BY id DESC LIMIT $3`, hid, before, limit+1)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer rows.Close()
	var items [][]byte
	var last UUID
	next := ""
	for rows.Next() {
		var id UUID
		var event string
		var status int16
		var attempts int32
		var code *int32
		var created time.Time
		if err := rows.Scan(&id, &event, &status, &attempts, &code, &created); err != nil {
			errInternal(w, err)
			return
		}
		if len(items) == limit {
			next = uuidStr(last)
			break
		}
		b := make([]byte, 0, 160)
		b = append(b, `{"id":"`...)
		b = appendUUID(b, id)
		b = append(b, `","event":`...)
		b = appendStr(b, event)
		b = append(b, `,"status":"`...)
		b = append(b, deliveryStatusNames[status]...)
		b = append(b, `","attempts":`...)
		b = appendInt(b, int64(attempts))
		b = append(b, `,"last_status_code":`...)
		if code != nil {
			b = appendInt(b, int64(*code))
		} else {
			b = append(b, "null"...)
		}
		b = append(b, `,"created_at":`...)
		b = appendTime(b, created)
		b = append(b, '}')
		items = append(items, b)
		last = id
	}
	if err := rows.Err(); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendPage(nil, items, next))
}

// ---------------------------------------------------------------------------------------------------------------
// dispatcher: one sequential worker per (webhook, issue) queue; state is durable in the deliveries table

type qkey struct{ hook, issue UUID }

type dispatcher struct {
	ctx    context.Context
	scale  float64
	client *http.Client
	mu     sync.Mutex
	queues map[qkey][]UUID
	wg     sync.WaitGroup
	sem    chan struct{}
}

func newDispatcher(ctx context.Context, scale float64) *dispatcher {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
	}
	return &dispatcher{
		ctx:    ctx,
		scale:  scale,
		client: &http.Client{Transport: tr, Timeout: 5 * time.Second},
		queues: map[qkey][]UUID{},
		sem:    make(chan struct{}, 64),
	}
}

func (d *dispatcher) loadPending() error {
	rows, err := db.Query(d.ctx, "SELECT id, webhook_id, issue_id FROM deliveries WHERE status=0 ORDER BY id")
	if err != nil {
		return err
	}
	var pend []pendingDelivery
	for rows.Next() {
		var p pendingDelivery
		if err := rows.Scan(&p.id, &p.webhook, &p.issue); err != nil {
			rows.Close()
			return err
		}
		pend = append(pend, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	d.enqueue(pend)
	return nil
}

func (d *dispatcher) enqueue(pend []pendingDelivery) {
	if len(pend) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range pend {
		k := qkey{p.webhook, p.issue}
		q, running := d.queues[k]
		// keep each queue ordered by delivery id (= event order)
		i, _ := slices.BinarySearchFunc(q, p.id, func(a, b UUID) int { return bytes.Compare(a[:], b[:]) })
		q = slices.Insert(q, i, p.id)
		d.queues[k] = q
		if !running {
			d.wg.Add(1)
			go d.worker(k)
		}
	}
}

func (d *dispatcher) worker(k qkey) {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		q := d.queues[k]
		if len(q) == 0 {
			delete(d.queues, k)
			d.mu.Unlock()
			return
		}
		id := q[0]
		d.queues[k] = q[1:]
		d.mu.Unlock()
		if !d.process(id) {
			return // shutting down
		}
	}
}

func (d *dispatcher) backoff(attempts int) time.Duration {
	return time.Duration(float64(time.Second) * float64(int(1)<<(attempts-1)) * d.scale)
}

// process delivers one delivery until success or final failure; returns false on shutdown.
func (d *dispatcher) process(id UUID) bool {
	var body []byte
	var event, u, secret string
	var attempts int
	var status int16
	var next time.Time
	err := db.QueryRow(d.ctx, `SELECT d.body, d.event, d.attempts, d.status, d.next_at, w.url, w.secret
		FROM deliveries d JOIN webhooks w ON w.id=d.webhook_id WHERE d.id=$1`, id).
		Scan(&body, &event, &attempts, &status, &next, &u, &secret)
	if err != nil {
		return d.ctx.Err() == nil
	}
	if status != 0 {
		return true
	}
	idStr := uuidStr(id)
	for {
		if wait := time.Until(next); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-d.ctx.Done():
				t.Stop()
				return false
			case <-t.C:
			}
		}
		d.sem <- struct{}{}
		code, ok := d.attempt(u, secret, event, idStr, body)
		<-d.sem
		if d.ctx.Err() != nil {
			return false
		}
		attempts++
		var codeArg *int32
		if code > 0 {
			c := int32(code)
			codeArg = &c
		}
		if ok || attempts >= 6 {
			st := 1
			if !ok {
				st = 2
			}
			if _, err := db.Exec(d.ctx, "UPDATE deliveries SET status=$2, attempts=$3, last_status_code=$4 WHERE id=$1",
				id, st, attempts, codeArg); err != nil && d.ctx.Err() != nil {
				return false
			}
			return true
		}
		next = time.Now().Add(d.backoff(attempts))
		if _, err := db.Exec(d.ctx, "UPDATE deliveries SET attempts=$2, last_status_code=$3, next_at=$4 WHERE id=$1",
			id, attempts, codeArg, next); err != nil && d.ctx.Err() != nil {
			return false
		}
	}
}

func (d *dispatcher) attempt(u, secret, event, id string, body []byte) (int, bool) {
	req, err := http.NewRequestWithContext(d.ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	h := req.Header
	h["Content-Type"] = []string{"application/json"}
	h["X-Tracker-Event"] = []string{event}
	h["X-Tracker-Delivery"] = []string{id}
	h["X-Tracker-Timestamp"] = []string{ts}
	h["X-Tracker-Signature"] = []string{"sha256=" + hex.EncodeToString(mac.Sum(nil))}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, false
	}
	var buf [512]byte
	for {
		if _, err := resp.Body.Read(buf[:]); err != nil {
			break
		}
	}
	resp.Body.Close()
	return resp.StatusCode, resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (d *dispatcher) wait(timeout time.Duration) {
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
