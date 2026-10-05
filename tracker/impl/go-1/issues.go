package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var typeNames = []string{"task", "bug", "story"}
var statusNames = []string{"todo", "in_progress", "done"}
var priorityNames = []string{"lowest", "low", "medium", "high", "highest"}

func enumIndex(names []string, s string) int16 {
	for i, n := range names {
		if n == s {
			return int16(i)
		}
	}
	return -1
}

type Issue struct {
	ID           UUID
	ProjectID    UUID
	ProjectKey   string
	Number       int32
	Type         int16
	Title        string
	Desc         pgtype.Text
	Status       int16
	Priority     int16
	Assignee     pgtype.UUID
	Reporter     UUID
	Labels       []string
	Due          pgtype.Date
	Version      int32
	CommentCount int32
	Created      time.Time
	Updated      time.Time
	Resolved     pgtype.Timestamptz
}

const issueCols = `i.id, i.project_id, i.project_key, i.number, i.type, i.title, i.description, i.status, i.priority,
	i.assignee_id, i.reporter_id, i.labels, i.due_date, i.version, i.comment_count, i.created_at, i.updated_at,
	i.resolved_at`

func (x *Issue) dests(extra ...any) []any {
	return append(extra, &x.ID, &x.ProjectID, &x.ProjectKey, &x.Number, &x.Type, &x.Title, &x.Desc, &x.Status,
		&x.Priority, &x.Assignee, &x.Reporter, &x.Labels, &x.Due, &x.Version, &x.CommentCount, &x.Created,
		&x.Updated, &x.Resolved)
}

func appendIssueKey(b []byte, x *Issue) []byte {
	b = append(b, x.ProjectKey...)
	b = append(b, '-')
	return appendInt(b, int64(x.Number))
}

func appendDate(b []byte, d pgtype.Date) []byte {
	if !d.Valid {
		return append(b, "null"...)
	}
	b = append(b, '"')
	b = d.Time.AppendFormat(b, "2006-01-02")
	return append(b, '"')
}

func appendNullUUID(b []byte, u pgtype.UUID) []byte {
	if !u.Valid {
		return append(b, "null"...)
	}
	b = append(b, '"')
	b = appendUUID(b, u.Bytes)
	return append(b, '"')
}

func appendNullText(b []byte, t pgtype.Text) []byte {
	if !t.Valid {
		return append(b, "null"...)
	}
	return appendStr(b, t.String)
}

func appendIssue(b []byte, x *Issue) []byte {
	b = append(b, `{"id":"`...)
	b = appendUUID(b, x.ID)
	b = append(b, `","key":"`...)
	b = appendIssueKey(b, x)
	b = append(b, `","number":`...)
	b = appendInt(b, int64(x.Number))
	b = append(b, `,"project_key":"`...)
	b = append(b, x.ProjectKey...)
	b = append(b, `","type":"`...)
	b = append(b, typeNames[x.Type]...)
	b = append(b, `","title":`...)
	b = appendStr(b, x.Title)
	b = append(b, `,"description":`...)
	b = appendNullText(b, x.Desc)
	b = append(b, `,"status":"`...)
	b = append(b, statusNames[x.Status]...)
	b = append(b, `","priority":"`...)
	b = append(b, priorityNames[x.Priority]...)
	b = append(b, `","assignee_id":`...)
	b = appendNullUUID(b, x.Assignee)
	b = append(b, `,"reporter_id":"`...)
	b = appendUUID(b, x.Reporter)
	b = append(b, `","labels":`...)
	b = appendStrList(b, x.Labels)
	b = append(b, `,"due_date":`...)
	b = appendDate(b, x.Due)
	b = append(b, `,"version":`...)
	b = appendInt(b, int64(x.Version))
	b = append(b, `,"comment_count":`...)
	b = appendInt(b, int64(x.CommentCount))
	b = append(b, `,"created_at":`...)
	b = appendTime(b, x.Created)
	b = append(b, `,"updated_at":`...)
	b = appendTime(b, x.Updated)
	b = append(b, `,"resolved_at":`...)
	if x.Resolved.Valid {
		b = appendTime(b, x.Resolved.Time)
	} else {
		b = append(b, "null"...)
	}
	return append(b, '}')
}

func parseIssueKey(s string) (string, int32, bool) {
	s = strings.ToUpper(s)
	i := strings.LastIndexByte(s, '-')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(s[i+1:], 10, 32)
	if err != nil || n <= 0 || !keyRe.MatchString(s[:i]) {
		return "", 0, false
	}
	return s[:i], int32(n), true
}

type issueCtx struct {
	Issue
	OrgID   UUID
	OrgRole int16
	Vis     int16
	Role    int16
}

// issueAccess loads an issue visible to the caller (optionally locking it); writes 404 otherwise.
func issueAccess(w http.ResponseWriter, ctx context.Context, q querier, slug, ikey string, uid UUID,
	lock bool) (*issueCtx, bool) {
	pkey, num, ok := parseIssueKey(ikey)
	if !ok {
		errNotFound(w)
		return nil, false
	}
	sql := `SELECT o.id, om.role, p.visibility, COALESCE(pm.role, 0), ` + issueCols + `
		FROM orgs o JOIN org_members om ON om.org_id=o.id AND om.user_id=$4
		JOIN projects p ON p.org_id=o.id AND p.key=$2
		JOIN issues i ON i.project_id=p.id AND i.number=$3
		LEFT JOIN project_members pm ON pm.project_id=p.id AND pm.user_id=$4
		WHERE o.slug=$1`
	if lock {
		sql += " FOR UPDATE OF i"
	}
	c := &issueCtx{}
	var pm int16
	err := q.QueryRow(ctx, sql, slug, pkey, num, uid).Scan(c.dests(&c.OrgID, &c.OrgRole, &c.Vis, &pm)...)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return nil, false
	}
	if err != nil {
		errInternal(w, err)
		return nil, false
	}
	c.Role = effRole(c.OrgRole, c.Vis, pm)
	if c.Role == 0 {
		errNotFound(w)
		return nil, false
	}
	return c, true
}

// ---------------------------------------------------------------------------------------------------------------
// field validation

var labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,49}$`)

func parseLabels(v *verrs, field string, raw json.RawMessage) ([]string, bool) {
	if isNull(raw) {
		return []string{}, true
	}
	var arr []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &arr) != nil {
		v.add(field, "invalid")
		return nil, false
	}
	out := make([]string, 0, len(arr))
	ok := true
	for i, e := range arr {
		s, isS := str(e)
		if !isS {
			v.add(field+"."+strconv.Itoa(i), "invalid")
			ok = false
			continue
		}
		s = strings.ToLower(s)
		if !labelRe.MatchString(s) {
			if runeLen(s) > 50 {
				v.add(field+"."+strconv.Itoa(i), "too_long")
			} else {
				v.add(field+"."+strconv.Itoa(i), "invalid")
			}
			ok = false
			continue
		}
		out = append(out, s)
	}
	if !ok {
		return nil, false
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > 20 {
		v.add(field, "too_long")
		return nil, false
	}
	return out, true
}

func parseDue(v *verrs, field string, raw json.RawMessage) (pgtype.Date, bool) {
	if isNull(raw) {
		return pgtype.Date{}, true
	}
	s, ok := str(raw)
	if !ok || len(s) != 10 {
		v.add(field, "invalid")
		return pgtype.Date{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		v.add(field, "invalid")
		return pgtype.Date{}, false
	}
	return pgtype.Date{Time: t, Valid: true}, true
}

func parseEnum(v *verrs, field string, raw json.RawMessage, names []string, required bool) (int16, bool) {
	if len(raw) == 0 {
		if required {
			v.add(field, "required")
		}
		return -1, !required
	}
	if isNull(raw) {
		v.add(field, "required")
		return -1, false
	}
	s, ok := str(raw)
	if !ok {
		v.add(field, "invalid")
		return -1, false
	}
	i := enumIndex(names, s)
	if i < 0 {
		v.add(field, "invalid")
		return -1, false
	}
	return i, true
}

func parseAssignee(v *verrs, field string, raw json.RawMessage) (pgtype.UUID, bool) {
	if isNull(raw) {
		return pgtype.UUID{}, true
	}
	s, ok := str(raw)
	if !ok {
		v.add(field, "invalid")
		return pgtype.UUID{}, false
	}
	u, ok := parseUUID(s)
	if !ok {
		v.add(field, "invalid")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: u, Valid: true}, true
}

type createIn struct {
	Type, Title, Description, Priority json.RawMessage
	AssigneeID                         json.RawMessage `json:"assignee_id"`
	Labels                             json.RawMessage
	DueDate                            json.RawMessage `json:"due_date"`
}

// validateCreate checks the static rules of a create body and fills an Issue (without number/ids).
func validateCreate(v *verrs, prefix string, in *createIn, x *Issue) {
	if t, ok := parseEnum(v, prefix+"type", in.Type, typeNames, true); ok {
		x.Type = t
	}
	if s, ok := reqString(v, prefix+"title", in.Title, true, 1, 255); ok {
		x.Title = s
	}
	if d, ok := optDesc(v, prefix+"description", in.Description, 65536); ok {
		x.Desc = d
	}
	x.Priority = 2
	if len(in.Priority) > 0 && !isNull(in.Priority) {
		if p, ok := parseEnum(v, prefix+"priority", in.Priority, priorityNames, false); ok {
			x.Priority = p
		}
	}
	if len(in.AssigneeID) > 0 {
		if a, ok := parseAssignee(v, prefix+"assignee_id", in.AssigneeID); ok {
			x.Assignee = a
		}
	}
	x.Labels = []string{}
	if len(in.Labels) > 0 {
		if l, ok := parseLabels(v, prefix+"labels", in.Labels); ok {
			x.Labels = l
		}
	}
	if len(in.DueDate) > 0 {
		if d, ok := parseDue(v, prefix+"due_date", in.DueDate); ok {
			x.Due = d
		}
	}
}

// eligibleAssignees returns which of ids have an effective role >= developer in the project.
func eligibleAssignees(ctx context.Context, q querier, orgID, projID UUID, vis int16, ids []UUID) (map[UUID]bool,
	error) {
	rows, err := q.Query(ctx, `SELECT om.user_id, om.role, COALESCE(pm.role, 0) FROM org_members om
		LEFT JOIN project_members pm ON pm.project_id=$2 AND pm.user_id=om.user_id
		WHERE om.org_id=$1 AND om.user_id = ANY($3)`, orgID, projID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[UUID]bool, len(ids))
	for rows.Next() {
		var id UUID
		var or, pm int16
		if err := rows.Scan(&id, &or, &pm); err != nil {
			return nil, err
		}
		if effRole(or, vis, pm) >= prDeveloper {
			out[id] = true
		}
	}
	return out, rows.Err()
}

func descPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

const createdChanges = `[{"field":"created","from":null,"to":null}]`

const insertIssueSQL = `INSERT INTO issues (id, org_id, project_id, project_key, number, type, title, description,
	status, priority, assignee_id, reporter_id, labels, due_date, version, comment_count, created_at, updated_at,
	resolved_at, words) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,0,$9,$10,$11,$12,$13,1,0,$14,$14,NULL,$15)`

// ---------------------------------------------------------------------------------------------------------------
// create

func hCreateIssue(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	idemVals, hasIdem := r.Header["Idempotency-Key"]
	idemKey := ""
	if hasIdem {
		idemKey = idemVals[0]
		if len(idemKey) == 0 || len(idemKey) > 255 {
			errBadRequest(w, "Idempotency-Key must be 1..255 characters")
			return
		}
	}
	var in createIn
	raw, ok := decodeBody(w, r, &in)
	if !ok {
		return
	}
	slug := r.PathValue("slug")
	p, ok := projectAccess(w, ctx, slug, r.PathValue("key"), uid)
	if !ok {
		return
	}
	if p.Role < prDeveloper {
		errForbidden(w)
		return
	}
	var v verrs
	x := &Issue{}
	validateCreate(&v, "", &in, x)
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	if x.Assignee.Valid {
		el, err := eligibleAssignees(ctx, db, p.OrgID, p.ID, p.Vis, []UUID{x.Assignee.Bytes})
		if err != nil {
			errInternal(w, err)
			return
		}
		if !el[x.Assignee.Bytes] {
			errValidation1(w, "assignee_id", "invalid")
			return
		}
	}
	var hash []byte
	if hasIdem {
		h := sha256.New()
		h.Write([]byte(r.URL.Path))
		h.Write([]byte{0})
		h.Write(raw)
		hash = h.Sum(nil)
	}
	x.ID = newID()
	x.ProjectID = p.ID
	x.ProjectKey = p.Key
	x.Reporter = uid
	x.Version = 1
	x.Created = nowMs()
	x.Updated = x.Created
	words := issueWords(x.Title, descPtr(x.Desc))

	for attempt := 0; attempt < 3; attempt++ {
		tx, err := db.Begin(ctx)
		if err != nil {
			errInternal(w, err)
			return
		}
		if hasIdem {
			tag, err := tx.Exec(ctx, `INSERT INTO idempotency (user_id, key, hash, created_at) VALUES ($1,$2,$3,now())
				ON CONFLICT (user_id, key) DO NOTHING`, uid, idemKey, hash)
			if err != nil {
				tx.Rollback(ctx)
				errInternal(w, err)
				return
			}
			if tag.RowsAffected() == 0 {
				tx.Rollback(ctx)
				var oh, body []byte
				var status int
				var fresh bool
				err := db.QueryRow(ctx, `SELECT hash, status, body, created_at > now() - interval '24 hours'
					FROM idempotency WHERE user_id=$1 AND key=$2`, uid, idemKey).Scan(&oh, &status, &body, &fresh)
				if err == pgx.ErrNoRows {
					continue
				}
				if err != nil {
					errInternal(w, err)
					return
				}
				if !fresh {
					db.Exec(ctx, "DELETE FROM idempotency WHERE user_id=$1 AND key=$2", uid, idemKey)
					continue
				}
				if string(oh) != string(hash) {
					writeProblem(w, 422, "idempotency_key_reused", "Idempotency-Key reused with a different request", nil)
					return
				}
				writeJSON(w, status, body)
				return
			}
		}
		pend, body, err := createOne(ctx, tx, p, slug, uid, x, words)
		if err == nil && hasIdem {
			_, err = tx.Exec(ctx, "UPDATE idempotency SET status=201, body=$3 WHERE user_id=$1 AND key=$2", uid,
				idemKey, body)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			tx.Rollback(ctx)
			errInternal(w, err)
			return
		}
		disp.enqueue(pend)
		writeJSON(w, 201, body)
		return
	}
	errConflict(w, "conflict", "concurrent request with the same Idempotency-Key")
}

func createOne(ctx context.Context, tx pgx.Tx, p *project, slug string, uid UUID, x *Issue,
	words []string) ([]pendingDelivery, []byte, error) {
	if err := tx.QueryRow(ctx, "UPDATE projects SET seq=seq+1 WHERE id=$1 RETURNING seq", p.ID).
		Scan(&x.Number); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, insertIssueSQL, x.ID, p.OrgID, p.ID, p.Key, x.Number, x.Type, x.Title, x.Desc,
		x.Priority, x.Assignee, uid, x.Labels, x.Due, x.Created, words); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO history (id, issue_id, actor_id, created_at, changes)
		VALUES ($1,$2,$3,$4,$5)`, newID(), x.ID, uid, x.Created, createdChanges); err != nil {
		return nil, nil, err
	}
	body := appendIssue(make([]byte, 0, 512), x)
	pend, err := addEvents(ctx, tx, p.OrgID, slug, evIssueCreated, uid, x.Created,
		[]evItem{{x.ID, issueData(body, nil)}})
	return pend, body, err
}

func issueData(issue []byte, changes []byte) []byte {
	b := make([]byte, 0, len(issue)+len(changes)+32)
	b = append(b, `{"issue":`...)
	b = append(b, issue...)
	if changes != nil {
		b = append(b, `,"changes":`...)
		b = append(b, changes...)
	}
	return append(b, '}')
}

func hBulkCreate(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		Issues json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	slug := r.PathValue("slug")
	p, ok := projectAccess(w, ctx, slug, r.PathValue("key"), uid)
	if !ok {
		return
	}
	if p.Role < prDeveloper {
		errForbidden(w)
		return
	}
	var arr []json.RawMessage
	if len(in.Issues) == 0 || in.Issues[0] != '[' || json.Unmarshal(in.Issues, &arr) != nil {
		errValidation1(w, "issues", "required")
		return
	}
	if len(arr) == 0 {
		errValidation1(w, "issues", "too_short")
		return
	}
	if len(arr) > 1000 {
		errValidation1(w, "issues", "too_long")
		return
	}
	var v verrs
	xs := make([]Issue, len(arr))
	assignees := map[UUID][]int{}
	for i, raw := range arr {
		prefix := "issues." + strconv.Itoa(i) + "."
		var ci createIn
		if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &ci) != nil {
			v.add("issues."+strconv.Itoa(i), "invalid")
			continue
		}
		validateCreate(&v, prefix, &ci, &xs[i])
		if xs[i].Assignee.Valid {
			assignees[xs[i].Assignee.Bytes] = append(assignees[xs[i].Assignee.Bytes], i)
		}
	}
	if len(assignees) > 0 {
		ids := make([]UUID, 0, len(assignees))
		for id := range assignees {
			ids = append(ids, id)
		}
		el, err := eligibleAssignees(ctx, db, p.OrgID, p.ID, p.Vis, ids)
		if err != nil {
			errInternal(w, err)
			return
		}
		for id, idxs := range assignees {
			if !el[id] {
				for _, i := range idxs {
					v.add("issues."+strconv.Itoa(i)+".assignee_id", "invalid")
				}
			}
		}
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	now := nowMs()
	n := len(xs)
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var last int32
	if err := tx.QueryRow(ctx, "UPDATE projects SET seq=seq+$2 WHERE id=$1 RETURNING seq", p.ID, n).
		Scan(&last); err != nil {
		errInternal(w, err)
		return
	}
	first := last - int32(n) + 1
	issueRows := make([][]any, n)
	histRows := make([][]any, n)
	evs := make([]evItem, n)
	keys := make([]byte, 0, n*12+16)
	keys = append(keys, `{"keys":[`...)
	wantEvents := hasHooks(ctx, p.OrgID, evIssueCreated)
	for i := range xs {
		x := &xs[i]
		x.ID = newID()
		x.ProjectID = p.ID
		x.ProjectKey = p.Key
		x.Number = first + int32(i)
		x.Reporter = uid
		x.Version = 1
		x.Created = now
		x.Updated = now
		issueRows[i] = []any{x.ID, p.OrgID, p.ID, p.Key, x.Number, x.Type, x.Title, x.Desc, int16(0), x.Priority,
			x.Assignee, uid, x.Labels, x.Due, int32(1), int32(0), now, now, pgtype.Timestamptz{},
			issueWords(x.Title, descPtr(x.Desc))}
		histRows[i] = []any{newID(), x.ID, uid, now, createdChanges}
		if wantEvents {
			evs[i] = evItem{x.ID, issueData(appendIssue(nil, x), nil)}
		}
		if i > 0 {
			keys = append(keys, ',')
		}
		keys = append(keys, '"')
		keys = appendIssueKey(keys, x)
		keys = append(keys, '"')
	}
	keys = append(keys, "]}"...)
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"issues"}, []string{"id", "org_id", "project_id", "project_key",
		"number", "type", "title", "description", "status", "priority", "assignee_id", "reporter_id", "labels",
		"due_date", "version", "comment_count", "created_at", "updated_at", "resolved_at", "words"},
		pgx.CopyFromRows(issueRows)); err != nil {
		errInternal(w, err)
		return
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"history"}, []string{"id", "issue_id", "actor_id", "created_at",
		"changes"}, pgx.CopyFromRows(histRows)); err != nil {
		errInternal(w, err)
		return
	}
	var pend []pendingDelivery
	if wantEvents {
		if pend, err = addEvents(ctx, tx, p.OrgID, slug, evIssueCreated, uid, now, evs); err != nil {
			errInternal(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		errInternal(w, err)
		return
	}
	disp.enqueue(pend)
	writeJSON(w, 201, keys)
}

// ---------------------------------------------------------------------------------------------------------------
// read

func hGetIssue(w http.ResponseWriter, r *http.Request, uid UUID) {
	c, ok := issueAccess(w, r.Context(), db, r.PathValue("slug"), r.PathValue("ikey"), uid, false)
	if !ok {
		return
	}
	w.Header()["Etag"] = []string{`"` + strconv.Itoa(int(c.Version)) + `"`}
	writeJSON(w, 200, appendIssue(make([]byte, 0, 512), &c.Issue))
}

// ---------------------------------------------------------------------------------------------------------------
// update

func parseIfMatch(r *http.Request) (int32, bool, bool) {
	s := r.Header.Get("If-Match")
	if s == "" {
		return 0, false, true
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	s = strings.Trim(s, `"`)
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return -1, true, false
	}
	return int32(n), true, true
}

type changeList struct{ b []byte }

func (c *changeList) add(field string, from, to []byte) {
	if c.b == nil {
		c.b = append(c.b, '[')
	} else {
		c.b = append(c.b, ',')
	}
	c.b = append(c.b, `{"field":"`...)
	c.b = append(c.b, field...)
	c.b = append(c.b, `","from":`...)
	c.b = append(c.b, from...)
	c.b = append(c.b, `,"to":`...)
	c.b = append(c.b, to...)
	c.b = append(c.b, '}')
}

func (c *changeList) bytes() []byte {
	if c.b == nil {
		return nil
	}
	return append(c.b, ']')
}

func quoted(s string) []byte { return appendStr(nil, s) }

func hPatchIssue(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		createIn
		Status json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	var v verrs
	var nType, nPri int16 = -1, -1
	var nTitle string
	var nDesc pgtype.Text
	var nAssignee pgtype.UUID
	var nLabels []string
	var nDue pgtype.Date
	if len(in.Status) > 0 {
		v.add("status", "invalid")
	}
	if len(in.Type) > 0 {
		nType, _ = parseEnum(&v, "type", in.Type, typeNames, true)
	}
	if len(in.Priority) > 0 {
		nPri, _ = parseEnum(&v, "priority", in.Priority, priorityNames, true)
	}
	if len(in.Title) > 0 {
		nTitle, _ = reqString(&v, "title", in.Title, true, 1, 255)
	}
	if len(in.Description) > 0 {
		nDesc, _ = optDesc(&v, "description", in.Description, 65536)
	}
	if len(in.AssigneeID) > 0 {
		nAssignee, _ = parseAssignee(&v, "assignee_id", in.AssigneeID)
	}
	if len(in.Labels) > 0 {
		nLabels, _ = parseLabels(&v, "labels", in.Labels)
	}
	if len(in.DueDate) > 0 {
		nDue, _ = parseDue(&v, "due_date", in.DueDate)
	}
	slug := r.PathValue("slug")
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	c, ok := issueAccess(w, ctx, tx, slug, r.PathValue("ikey"), uid, true)
	if !ok {
		return
	}
	if c.Role < prDeveloper {
		errForbidden(w)
		return
	}
	ver, present, okv := parseIfMatch(r)
	if !present {
		writeProblem(w, 428, "precondition_required", "If-Match header is required", nil)
		return
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	if !okv || ver != c.Version {
		writeProblem(w, 412, "version_mismatch", "version mismatch", nil)
		return
	}
	x := c.Issue
	var ch changeList
	descChanged := false
	if len(in.Title) > 0 && nTitle != x.Title {
		ch.add("title", quoted(x.Title), quoted(nTitle))
		x.Title = nTitle
		descChanged = true
	}
	if len(in.Description) > 0 && nDesc != x.Desc {
		ch.add("description", appendNullText(nil, x.Desc), appendNullText(nil, nDesc))
		x.Desc = nDesc
		descChanged = true
	}
	if nType >= 0 && nType != x.Type {
		ch.add("type", quoted(typeNames[x.Type]), quoted(typeNames[nType]))
		x.Type = nType
	}
	if nPri >= 0 && nPri != x.Priority {
		ch.add("priority", quoted(priorityNames[x.Priority]), quoted(priorityNames[nPri]))
		x.Priority = nPri
	}
	if len(in.AssigneeID) > 0 && nAssignee != x.Assignee {
		if nAssignee.Valid {
			el, err := eligibleAssignees(ctx, tx, c.OrgID, x.ProjectID, c.Vis, []UUID{nAssignee.Bytes})
			if err != nil {
				errInternal(w, err)
				return
			}
			if !el[nAssignee.Bytes] {
				errValidation1(w, "assignee_id", "invalid")
				return
			}
		}
		ch.add("assignee_id", appendNullUUID(nil, x.Assignee), appendNullUUID(nil, nAssignee))
		x.Assignee = nAssignee
	}
	if len(in.Labels) > 0 && !slices.Equal(nLabels, x.Labels) {
		ch.add("labels", appendStrList(nil, x.Labels), appendStrList(nil, nLabels))
		x.Labels = nLabels
	}
	if len(in.DueDate) > 0 && !sameDate(nDue, x.Due) {
		ch.add("due_date", appendDate(nil, x.Due), appendDate(nil, nDue))
		x.Due = nDue
	}
	changes := ch.bytes()
	if changes == nil {
		tx.Rollback(ctx)
		w.Header()["Etag"] = []string{`"` + strconv.Itoa(int(x.Version)) + `"`}
		writeJSON(w, 200, appendIssue(nil, &x))
		return
	}
	x.Version++
	x.Updated = nowMs()
	if !x.Updated.After(c.Updated) {
		x.Updated = c.Updated.Add(time.Millisecond)
	}
	var words []string
	if descChanged {
		words = issueWords(x.Title, descPtr(x.Desc))
	}
	if _, err = tx.Exec(ctx, `UPDATE issues SET type=$2, title=$3, description=$4, priority=$5, assignee_id=$6,
		labels=$7, due_date=$8, version=$9, updated_at=$10, words=COALESCE($11, words) WHERE id=$1`,
		x.ID, x.Type, x.Title, x.Desc, x.Priority, x.Assignee, x.Labels, x.Due, x.Version, x.Updated,
		words); err != nil {
		errInternal(w, err)
		return
	}
	finishUpdate(w, ctx, tx, c, &x, slug, uid, changes)
}

func sameDate(a, b pgtype.Date) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Time.Equal(b.Time)
}

// finishUpdate records history and the issue.updated event, commits and responds.
func finishUpdate(w http.ResponseWriter, ctx context.Context, tx pgx.Tx, c *issueCtx, x *Issue, slug string,
	uid UUID, changes []byte) {
	if _, err := tx.Exec(ctx, `INSERT INTO history (id, issue_id, actor_id, created_at, changes)
		VALUES ($1,$2,$3,$4,$5)`, newID(), x.ID, uid, x.Updated, string(changes)); err != nil {
		errInternal(w, err)
		return
	}
	body := appendIssue(make([]byte, 0, 512), x)
	pend, err := addEvents(ctx, tx, c.OrgID, slug, evIssueUpdated, uid, x.Updated,
		[]evItem{{x.ID, issueData(body, changes)}})
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	disp.enqueue(pend)
	w.Header()["Etag"] = []string{`"` + strconv.Itoa(int(x.Version)) + `"`}
	writeJSON(w, 200, body)
}

func hTransition(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		Status json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	var v verrs
	st, _ := parseEnum(&v, "status", in.Status, statusNames, true)
	slug := r.PathValue("slug")
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	c, ok := issueAccess(w, ctx, tx, slug, r.PathValue("ikey"), uid, true)
	if !ok {
		return
	}
	if c.Role < prDeveloper {
		errForbidden(w)
		return
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	if ver, present, okv := parseIfMatch(r); present && (!okv || ver != c.Version) {
		writeProblem(w, 412, "version_mismatch", "version mismatch", nil)
		return
	}
	from := c.Status
	allowed := (from == 0 && st == 1) || (from == 1 && (st == 0 || st == 2)) || (from == 2 && st == 1)
	if !allowed {
		errConflict(w, "transition_not_allowed", "transition "+statusNames[from]+" → "+statusNames[st]+" not allowed")
		return
	}
	x := c.Issue
	x.Status = st
	x.Version++
	x.Updated = nowMs()
	if !x.Updated.After(c.Updated) {
		x.Updated = c.Updated.Add(time.Millisecond)
	}
	if st == 2 {
		x.Resolved = pgtype.Timestamptz{Time: x.Updated, Valid: true}
	} else {
		x.Resolved = pgtype.Timestamptz{}
	}
	if _, err = tx.Exec(ctx, "UPDATE issues SET status=$2, version=$3, updated_at=$4, resolved_at=$5 WHERE id=$1",
		x.ID, x.Status, x.Version, x.Updated, x.Resolved); err != nil {
		errInternal(w, err)
		return
	}
	var ch changeList
	ch.add("status", quoted(statusNames[from]), quoted(statusNames[st]))
	finishUpdate(w, ctx, tx, c, &x, slug, uid, ch.bytes())
}

func hDeleteIssue(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	c, ok := issueAccess(w, ctx, tx, slug, r.PathValue("ikey"), uid, true)
	if !ok {
		return
	}
	if c.Role < prAdmin {
		errForbidden(w)
		return
	}
	if _, err = tx.Exec(ctx, "DELETE FROM issues WHERE id=$1", c.ID); err != nil {
		errInternal(w, err)
		return
	}
	pend, err := addEvents(ctx, tx, c.OrgID, slug, evIssueDeleted, uid, nowMs(),
		[]evItem{{c.ID, issueData(appendIssue(nil, &c.Issue), nil)}})
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	disp.enqueue(pend)
	w.WriteHeader(204)
}
