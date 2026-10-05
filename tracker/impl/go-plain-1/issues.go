package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

var (
	issueTypes   = []string{"task", "bug", "story"}
	issueStatus  = []string{"todo", "in_progress", "done"}
	priorities   = []string{"highest", "high", "medium", "low", "lowest"}
	priorityRank = map[string]int{"lowest": 0, "low": 1, "medium": 2, "high": 3, "highest": 4}
	transitions  = map[string][]string{"todo": {"in_progress"}, "in_progress": {"todo", "done"}, "done": {"in_progress"}}
	labelRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,49}$`)
	dateRe       = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	issueKeyRe   = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]{1,9})-([0-9]{1,9})$`)
)

type Issue struct {
	ID           string
	ProjectID    string
	ProjectKey   string
	Number       int
	Type         string
	Title        string
	Description  *string
	Status       string
	Priority     string
	AssigneeID   *string
	ReporterID   string
	Labels       []string
	DueDate      *string
	Version      int
	CommentCount int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ResolvedAt   *time.Time
}

func (i *Issue) Key() string { return i.ProjectKey + "-" + strconv.Itoa(i.Number) }

func (i *Issue) JSON() map[string]any {
	return map[string]any{
		"id": i.ID, "key": i.Key(), "number": i.Number, "project_key": i.ProjectKey,
		"type": i.Type, "title": i.Title, "description": i.Description, "status": i.Status,
		"priority": i.Priority, "assignee_id": i.AssigneeID, "reporter_id": i.ReporterID,
		"labels": i.Labels, "due_date": i.DueDate, "version": i.Version, "comment_count": i.CommentCount,
		"created_at": fmtTS(i.CreatedAt), "updated_at": fmtTS(i.UpdatedAt), "resolved_at": fmtTSPtr(i.ResolvedAt),
	}
}

const issueSelect = `SELECT i.id, i.project_id, p.key, i.number, i.type, i.title, i.description, i.status, i.priority,
	i.assignee_id, i.reporter_id, i.labels, i.due_date, i.version, i.comment_count, i.created_at, i.updated_at,
	i.resolved_at FROM issues i JOIN projects p ON p.id = i.project_id`

func scanIssue(row pgx.Row) (*Issue, error) {
	i := &Issue{}
	err := row.Scan(&i.ID, &i.ProjectID, &i.ProjectKey, &i.Number, &i.Type, &i.Title, &i.Description, &i.Status,
		&i.Priority, &i.AssigneeID, &i.ReporterID, &i.Labels, &i.DueDate, &i.Version, &i.CommentCount,
		&i.CreatedAt, &i.UpdatedAt, &i.ResolvedAt)
	if err != nil {
		return nil, err
	}
	if i.Labels == nil {
		i.Labels = []string{}
	}
	return i, nil
}

func queryIssue(ctx context.Context, q querier, sql string, args ...any) (*Issue, error) {
	i, err := scanIssue(q.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	return i, err
}

func queryIssues(ctx context.Context, q querier, sql string, args ...any) ([]*Issue, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func lockIssue(ctx context.Context, tx pgx.Tx, id string) (*Issue, error) {
	return queryIssue(ctx, tx, issueSelect+` WHERE i.id = $1 FOR UPDATE OF i`, id)
}

// words splits text into maximal runs of Unicode letters and digits, lower-cased.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.Is(unicode.Nd, r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func searchWords(i *Issue) []string {
	text := i.Title
	if i.Description != nil {
		text += " " + *i.Description
	}
	ws := words(text)
	sort.Strings(ws)
	return slices.Compact(ws)
}

// ---- input parsing

type issueInput struct {
	typ, title, priority string
	desc, assignee, due  *string
	labels               []string
	has                  map[string]bool
}

func parseIssueInput(o obj, v *verrs, create bool) *issueInput {
	in := &issueInput{has: map[string]bool{}}
	if s, st := v.enum(o, "type", create, issueTypes...); st == fSet {
		in.typ, in.has["type"] = s, true
	}
	if s, st := v.str(o, "title", strSpec{required: create, trim: true, min: 1, max: 255}); st == fSet {
		in.title, in.has["title"] = s, true
	}
	if s, st := v.str(o, "description", strSpec{nullable: true, max: 65536}); st == fSet {
		in.desc, in.has["description"] = &s, true
	} else if st == fNull {
		in.has["description"] = true
	}
	if s, st := v.enum(o, "priority", false, priorities...); st == fSet {
		in.priority, in.has["priority"] = s, true
	} else if create {
		in.priority = "medium"
	}
	if s, st := v.str(o, "assignee_id", strSpec{nullable: true}); st == fSet {
		if isUUID(s) {
			s = strings.ToLower(s)
			in.assignee, in.has["assignee_id"] = &s, true
		} else {
			v.add("assignee_id", "invalid")
		}
	} else if st == fNull {
		in.has["assignee_id"] = true
	}
	if raw, ok := o["labels"]; ok {
		if labels, ok := parseLabels(raw, v); ok {
			in.labels, in.has["labels"] = labels, true
		}
	} else if create {
		in.labels = []string{}
	}
	if s, st := v.str(o, "due_date", strSpec{nullable: true}); st == fSet {
		if _, err := time.Parse("2006-01-02", s); err != nil || !dateRe.MatchString(s) {
			v.add("due_date", "invalid")
		} else {
			in.due, in.has["due_date"] = &s, true
		}
	} else if st == fNull {
		in.has["due_date"] = true
	}
	return in
}

func parseLabels(raw json.RawMessage, v *verrs) ([]string, bool) {
	if isNull(raw) {
		return []string{}, true
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		v.add("labels", "invalid")
		return nil, false
	}
	n := len(v.list)
	seen := map[string]bool{}
	out := []string{}
	for i, el := range arr {
		var s string
		field := fmt.Sprintf("labels.%d", i)
		if err := json.Unmarshal(el, &s); err != nil {
			v.add(field, "invalid")
			continue
		}
		s = strings.ToLower(s)
		if len([]rune(s)) > 50 {
			v.add(field, "too_long")
			continue
		}
		if !labelRe.MatchString(s) {
			v.add(field, "invalid")
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) > 20 {
		v.add("labels", "too_long")
	}
	if len(v.list) > n {
		return nil, false
	}
	sort.Strings(out)
	return out, true
}

func applyInput(old *Issue, in *issueInput) *Issue {
	nw := *old
	if in.has["type"] {
		nw.Type = in.typ
	}
	if in.has["title"] {
		nw.Title = in.title
	}
	if in.has["description"] {
		nw.Description = in.desc
	}
	if in.has["priority"] {
		nw.Priority = in.priority
	}
	if in.has["assignee_id"] {
		nw.AssigneeID = in.assignee
	}
	if in.has["labels"] {
		nw.Labels = in.labels
	}
	if in.has["due_date"] {
		nw.DueDate = in.due
	}
	return &nw
}

type change struct {
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

func ptrVal(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrEq(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func diffIssues(a, b *Issue) []change {
	var ch []change
	if a.Title != b.Title {
		ch = append(ch, change{"title", a.Title, b.Title})
	}
	if !ptrEq(a.Description, b.Description) {
		ch = append(ch, change{"description", ptrVal(a.Description), ptrVal(b.Description)})
	}
	if a.Type != b.Type {
		ch = append(ch, change{"type", a.Type, b.Type})
	}
	if a.Status != b.Status {
		ch = append(ch, change{"status", a.Status, b.Status})
	}
	if a.Priority != b.Priority {
		ch = append(ch, change{"priority", a.Priority, b.Priority})
	}
	if !ptrEq(a.AssigneeID, b.AssigneeID) {
		ch = append(ch, change{"assignee_id", ptrVal(a.AssigneeID), ptrVal(b.AssigneeID)})
	}
	if !slices.Equal(a.Labels, b.Labels) {
		ch = append(ch, change{"labels", a.Labels, b.Labels})
	}
	if !ptrEq(a.DueDate, b.DueDate) {
		ch = append(ch, change{"due_date", ptrVal(a.DueDate), ptrVal(b.DueDate)})
	}
	return ch
}

// assigneeEligible: the user must have an effective role of at least developer in the project.
func assigneeEligible(ctx context.Context, q querier, p *Project, uid string) (bool, error) {
	var orgRole string
	var explicit *string
	err := q.QueryRow(ctx, `SELECT om.role, pm.role FROM org_members om
		LEFT JOIN project_members pm ON pm.project_id = $1 AND pm.user_id = om.user_id
		WHERE om.org_id = $2 AND om.user_id = $3`, p.ID, p.Org.ID, uid).Scan(&orgRole, &explicit)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return projRank[effectiveRole(orgRole, explicit, p.Visibility)] >= projRank["developer"], nil
}

// ---- writes

const insertIssueSQL = `INSERT INTO issues (id, project_id, number, type, title, description, status, priority,
	priority_rank, assignee_id, reporter_id, labels, due_date, version, comment_count, created_at, updated_at,
	resolved_at, search_words) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`

const insertHistorySQL = `INSERT INTO issue_history (id, issue_id, actor_id, created_at, changes) VALUES ($1, $2, $3, $4, $5)`

var createdChange = `[{"field":"created","from":null,"to":null}]`

// insertIssues allocates consecutive numbers and creates the issues with their history and events.
func (s *Server) insertIssues(ctx context.Context, tx pgx.Tx, p *Project, actor string, ins []*issueInput) ([]*Issue, error) {
	var last int
	if err := tx.QueryRow(ctx, `UPDATE projects SET last_number = last_number + $2 WHERE id = $1 RETURNING last_number`,
		p.ID, len(ins)).Scan(&last); err != nil {
		return nil, err
	}
	hooks, err := hooksFor(ctx, tx, p.Org.ID, "issue.created")
	if err != nil {
		return nil, err
	}
	now := nowMS()
	batch := &pgx.Batch{}
	out := make([]*Issue, len(ins))
	for n, in := range ins {
		i := &Issue{ID: newID(), ProjectID: p.ID, ProjectKey: p.Key, Number: last - len(ins) + 1 + n, Type: in.typ,
			Title: in.title, Description: in.desc, Status: "todo", Priority: in.priority, AssigneeID: in.assignee,
			ReporterID: actor, Labels: in.labels, DueDate: in.due, Version: 1, CreatedAt: now, UpdatedAt: now}
		out[n] = i
		batch.Queue(insertIssueSQL, i.ID, i.ProjectID, i.Number, i.Type, i.Title, i.Description, i.Status, i.Priority,
			priorityRank[i.Priority], i.AssigneeID, i.ReporterID, i.Labels, i.DueDate, i.Version, 0, i.CreatedAt,
			i.UpdatedAt, nil, searchWords(i))
		batch.Queue(insertHistorySQL, newID(), i.ID, actor, now, createdChange)
		for _, h := range hooks {
			queueDelivery(batch, h, "issue.created", p.Org, actor, i.ID, map[string]any{"issue": i.JSON()}, now)
		}
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// saveIssueUpdate persists nw (a modified copy of old), bumping the version, with history and an event.
func (s *Server) saveIssueUpdate(ctx context.Context, tx pgx.Tx, org *Org, actor string, old, nw *Issue, ch []change) error {
	now := nowMS()
	nw.Version = old.Version + 1
	nw.UpdatedAt = now
	if _, err := tx.Exec(ctx, `UPDATE issues SET type = $2, title = $3, description = $4, status = $5, priority = $6,
		priority_rank = $7, assignee_id = $8, labels = $9, due_date = $10, version = $11, updated_at = $12,
		resolved_at = $13, search_words = $14 WHERE id = $1`,
		nw.ID, nw.Type, nw.Title, nw.Description, nw.Status, nw.Priority, priorityRank[nw.Priority], nw.AssigneeID,
		nw.Labels, nw.DueDate, nw.Version, nw.UpdatedAt, nw.ResolvedAt, searchWords(nw)); err != nil {
		return err
	}
	cj, _ := json.Marshal(ch)
	if _, err := tx.Exec(ctx, insertHistorySQL, newID(), nw.ID, actor, now, string(cj)); err != nil {
		return err
	}
	return enqueue(ctx, tx, org, "issue.updated", actor, nw.ID, map[string]any{"issue": nw.JSON(), "changes": ch}, now)
}

// ---- handlers

var errReplay = errors.New("idempotent replay")

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	p, err := s.orgAndProject(r, uid)
	if err != nil {
		return err
	}
	if !p.can("developer") {
		return forbidden()
	}
	o, raw, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	_, hasIdem := r.Header["Idempotency-Key"]
	idem := r.Header.Get("Idempotency-Key")
	if hasIdem && (len(idem) < 1 || len(idem) > 255) {
		v.add("Idempotency-Key", "invalid")
	}
	in := parseIssueInput(o, v, true)
	if !v.ok() {
		return v.err()
	}
	var status int
	var body []byte
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var reqHash string
		if idem != "" {
			sum := sha256.Sum256(append([]byte(p.ID+"\n"), raw...))
			reqHash = hex.EncodeToString(sum[:])
			now := time.Now()
			tag, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, request_hash, created_at)
				VALUES ($1, $2, $3, $4) ON CONFLICT (user_id, key) DO UPDATE SET request_hash = EXCLUDED.request_hash,
				status = NULL, response = NULL, created_at = EXCLUDED.created_at WHERE idempotency_keys.created_at < $5`,
				uid, idem, reqHash, now, now.Add(-24*time.Hour))
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				var oldHash string
				var st *int
				if err := tx.QueryRow(ctx, `SELECT request_hash, status, response FROM idempotency_keys
					WHERE user_id = $1 AND key = $2`, uid, idem).Scan(&oldHash, &st, &body); err != nil {
					return err
				}
				if oldHash != reqHash {
					return newErr(422, "idempotency_key_reused", "Idempotency-Key was used with a different request")
				}
				if st == nil {
					return newErr(409, "conflict", "a request with this Idempotency-Key is in progress")
				}
				status = *st
				return errReplay
			}
		}
		if in.assignee != nil {
			ok, err := assigneeEligible(ctx, tx, p, *in.assignee)
			if err != nil {
				return err
			}
			if !ok {
				v.add("assignee_id", "invalid")
				return v.err()
			}
		}
		issues, err := s.insertIssues(ctx, tx, p, uid, []*issueInput{in})
		if err != nil {
			return err
		}
		status = 201
		body, _ = json.Marshal(issues[0].JSON())
		if idem != "" {
			if _, err := tx.Exec(ctx, `UPDATE idempotency_keys SET status = $3, response = $4 WHERE user_id = $1 AND key = $2`,
				uid, idem, status, body); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return err
	}
	s.kick()
	writeRaw(w, status, "application/json", body)
	return nil
}

func (s *Server) bulkCreateIssues(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	p, err := s.orgAndProject(r, uid)
	if err != nil {
		return err
	}
	if !p.can("developer") {
		return forbidden()
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	var items []json.RawMessage
	raw, ok := o["issues"]
	switch {
	case !ok:
		v.add("issues", "required")
	case json.Unmarshal(raw, &items) != nil || items == nil:
		v.add("issues", "invalid")
	case len(items) < 1:
		v.add("issues", "too_short")
	case len(items) > 1000:
		v.add("issues", "too_long")
	}
	if !v.ok() {
		return v.err()
	}
	ins := make([]*issueInput, len(items))
	for n, it := range items {
		iv := &verrs{prefix: fmt.Sprintf("issues.%d.", n)}
		var io obj
		if err := json.Unmarshal(it, &io); err != nil || io == nil {
			v.list = append(v.list, fieldErr{Field: fmt.Sprintf("issues.%d", n), Code: "invalid"})
			continue
		}
		ins[n] = parseIssueInput(io, iv, true)
		v.list = append(v.list, iv.list...)
	}
	var keys []string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		eligible := map[string]bool{}
		for n, in := range ins {
			if in == nil || in.assignee == nil {
				continue
			}
			ok, seen := eligible[*in.assignee]
			if !seen {
				var err error
				if ok, err = assigneeEligible(ctx, tx, p, *in.assignee); err != nil {
					return err
				}
				eligible[*in.assignee] = ok
			}
			if !ok {
				v.list = append(v.list, fieldErr{Field: fmt.Sprintf("issues.%d.assignee_id", n), Code: "invalid"})
			}
		}
		if !v.ok() {
			return v.err()
		}
		issues, err := s.insertIssues(ctx, tx, p, uid, ins)
		if err != nil {
			return err
		}
		for _, i := range issues {
			keys = append(keys, i.Key())
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.kick()
	writeJSON(w, 201, map[string]any{"keys": keys})
	return nil
}

// loadIssueCtx resolves {slug}/{ikey} to a visible project and issue.
func (s *Server) loadIssueCtx(r *http.Request, uid string) (*Project, *Issue, error) {
	ctx := r.Context()
	org, err := s.loadOrg(ctx, r.PathValue("slug"), uid)
	if err != nil {
		return nil, nil, err
	}
	m := issueKeyRe.FindStringSubmatch(r.PathValue("ikey"))
	if m == nil {
		return nil, nil, notFound()
	}
	p, err := s.loadProject(ctx, org, m[1], uid)
	if err != nil {
		return nil, nil, err
	}
	num, _ := strconv.Atoi(m[2])
	i, err := queryIssue(ctx, s.pool, issueSelect+` WHERE i.project_id = $1 AND i.number = $2`, p.ID, num)
	if err != nil {
		return nil, nil, err
	}
	return p, i, nil
}

func writeIssue(w http.ResponseWriter, i *Issue) {
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, i.Version))
	writeJSON(w, 200, i.JSON())
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request, uid string) error {
	_, i, err := s.loadIssueCtx(r, uid)
	if err != nil {
		return err
	}
	writeIssue(w, i)
	return nil
}

func parseIfMatch(h string) (int, bool) {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "W/")
	h = strings.Trim(h, `"`)
	n, err := strconv.Atoi(h)
	return n, err == nil
}

func versionMismatch() error { return newErr(412, "version_mismatch", "the issue was modified") }

func (s *Server) patchIssue(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	p, cur, err := s.loadIssueCtx(r, uid)
	if err != nil {
		return err
	}
	if !p.can("developer") {
		return forbidden()
	}
	ifm := r.Header.Get("If-Match")
	if ifm == "" {
		return newErr(428, "precondition_required", "If-Match header is required")
	}
	ver, verOK := parseIfMatch(ifm)
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	if _, ok := o["status"]; ok {
		v.add("status", "invalid")
	}
	in := parseIssueInput(o, v, false)
	if !v.ok() {
		return v.err()
	}
	var out *Issue
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		old, err := lockIssue(ctx, tx, cur.ID)
		if err != nil {
			return err
		}
		if !verOK || old.Version != ver {
			return versionMismatch()
		}
		if in.assignee != nil && !ptrEq(in.assignee, old.AssigneeID) {
			ok, err := assigneeEligible(ctx, tx, p, *in.assignee)
			if err != nil {
				return err
			}
			if !ok {
				v.add("assignee_id", "invalid")
				return v.err()
			}
		}
		nw := applyInput(old, in)
		ch := diffIssues(old, nw)
		if len(ch) == 0 {
			out = old
			return nil
		}
		out = nw
		return s.saveIssueUpdate(ctx, tx, p.Org, uid, old, nw, ch)
	})
	if err != nil {
		return err
	}
	s.kick()
	writeIssue(w, out)
	return nil
}

func (s *Server) transitionIssue(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	p, cur, err := s.loadIssueCtx(r, uid)
	if err != nil {
		return err
	}
	if !p.can("developer") {
		return forbidden()
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	status, _ := v.enum(o, "status", true, issueStatus...)
	if !v.ok() {
		return v.err()
	}
	ifm := r.Header.Get("If-Match")
	var out *Issue
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		old, err := lockIssue(ctx, tx, cur.ID)
		if err != nil {
			return err
		}
		if ifm != "" {
			if ver, ok := parseIfMatch(ifm); !ok || ver != old.Version {
				return versionMismatch()
			}
		}
		if !contains(transitions[old.Status], status) {
			return newErr(409, "transition_not_allowed", fmt.Sprintf("cannot move from %s to %s", old.Status, status))
		}
		nw := *old
		nw.Status = status
		if status == "done" {
			t := nowMS()
			nw.ResolvedAt = &t
		} else {
			nw.ResolvedAt = nil
		}
		out = &nw
		return s.saveIssueUpdate(ctx, tx, p.Org, uid, old, &nw, diffIssues(old, &nw))
	})
	if err != nil {
		return err
	}
	s.kick()
	writeIssue(w, out)
	return nil
}

func (s *Server) deleteIssue(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	p, cur, err := s.loadIssueCtx(r, uid)
	if err != nil {
		return err
	}
	if !p.can("admin") {
		return forbidden()
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		old, err := lockIssue(ctx, tx, cur.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM issues WHERE id = $1`, old.ID); err != nil {
			return err
		}
		return enqueue(ctx, tx, p.Org, "issue.deleted", uid, old.ID, map[string]any{"issue": old.JSON()}, nowMS())
	})
	if err != nil {
		return err
	}
	s.kick()
	w.WriteHeader(204)
	return nil
}

func (s *Server) listHistory(w http.ResponseWriter, r *http.Request, uid string) error {
	_, i, err := s.loadIssueCtx(r, uid)
	if err != nil {
		return err
	}
	pg, err := pageFromRequest(r)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT id, actor_id, created_at, changes FROM issue_history
		WHERE issue_id = $1 ORDER BY seq DESC`+pg.sql(), i.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []map[string]any
	for rows.Next() {
		var id, actor string
		var at time.Time
		var changes json.RawMessage
		if err := rows.Scan(&id, &actor, &at, &changes); err != nil {
			return err
		}
		items = append(items, map[string]any{"id": id, "actor_id": actor, "created_at": fmtTS(at), "changes": changes})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, finish(pg, items))
	return nil
}
