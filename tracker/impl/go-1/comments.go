package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type comment struct {
	ID      UUID
	Author  UUID
	Body    string
	Created time.Time
	Updated time.Time
	Edited  bool
}

func appendComment(b []byte, c *comment, issueKey []byte) []byte {
	b = append(b, `{"id":"`...)
	b = appendUUID(b, c.ID)
	b = append(b, `","issue_key":"`...)
	b = append(b, issueKey...)
	b = append(b, `","author_id":"`...)
	b = appendUUID(b, c.Author)
	b = append(b, `","body":`...)
	b = appendStr(b, c.Body)
	b = append(b, `,"created_at":`...)
	b = appendTime(b, c.Created)
	b = append(b, `,"updated_at":`...)
	b = appendTime(b, c.Updated)
	b = append(b, `,"edited":`...)
	if c.Edited {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	return append(b, '}')
}

func commentBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		Body json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return "", false
	}
	var v verrs
	if len(in.Body) == 0 || isNull(in.Body) {
		v.add("body", "required")
	} else if s, ok := str(in.Body); !ok {
		v.add("body", "invalid")
	} else if strings.TrimSpace(s) == "" {
		v.add("body", "too_short")
	} else if runeLen(s) > 20000 {
		v.add("body", "too_long")
	} else {
		return s, true
	}
	errValidation(w, v)
	return "", false
}

func hCreateComment(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	body, ok := commentBody(w, r)
	if !ok {
		return
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
	cm := &comment{ID: newID(), Author: uid, Body: body, Created: nowMs()}
	cm.Updated = cm.Created
	if _, err = tx.Exec(ctx, `INSERT INTO comments (id, issue_id, author_id, body, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$5)`, cm.ID, c.ID, uid, body, cm.Created); err != nil {
		errInternal(w, err)
		return
	}
	if _, err = tx.Exec(ctx, "UPDATE issues SET comment_count=comment_count+1 WHERE id=$1", c.ID); err != nil {
		errInternal(w, err)
		return
	}
	c.CommentCount++
	key := appendIssueKey(nil, &c.Issue)
	out := appendComment(make([]byte, 0, 256+len(body)), cm, key)
	var pend []pendingDelivery
	if hasHooks(ctx, c.OrgID, evCommentCreated) {
		data := make([]byte, 0, 1024+len(out))
		data = append(data, `{"issue":`...)
		data = appendIssue(data, &c.Issue)
		data = append(data, `,"comment":`...)
		data = append(data, out...)
		data = append(data, '}')
		if pend, err = addEvents(ctx, tx, c.OrgID, slug, evCommentCreated, uid, cm.Created,
			[]evItem{{c.ID, data}}); err != nil {
			errInternal(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		errInternal(w, err)
		return
	}
	disp.enqueue(pend)
	writeJSON(w, 201, out)
}

func hListComments(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	c, ok := issueAccess(w, ctx, db, r.PathValue("slug"), r.PathValue("ikey"), uid, false)
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
	rows, err := db.Query(ctx, `SELECT id, author_id, body, created_at, updated_at, edited FROM comments
		WHERE issue_id=$1 AND id > $2 ORDER BY id LIMIT $3`, c.ID, after, limit+1)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer rows.Close()
	key := appendIssueKey(nil, &c.Issue)
	var items [][]byte
	var last UUID
	next := ""
	for rows.Next() {
		var cm comment
		if err := rows.Scan(&cm.ID, &cm.Author, &cm.Body, &cm.Created, &cm.Updated, &cm.Edited); err != nil {
			errInternal(w, err)
			return
		}
		if len(items) == limit {
			next = uuidStr(last)
			break
		}
		items = append(items, appendComment(nil, &cm, key))
		last = cm.ID
	}
	if err := rows.Err(); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendPage(nil, items, next))
}

// commentAccess loads a comment (locking it) and the caller's effective role on its project.
func commentAccess(w http.ResponseWriter, r *http.Request, tx pgx.Tx, uid UUID) (*comment, []byte, UUID, int16,
	bool) {
	ctx := r.Context()
	cid, ok := parseUUID(r.PathValue("id"))
	if !ok {
		errNotFound(w)
		return nil, nil, UUID{}, 0, false
	}
	cm := &comment{ID: cid}
	var issueID UUID
	var pkey string
	var num int32
	var orgRole, vis, pm int16
	err := tx.QueryRow(ctx, `SELECT c.author_id, c.body, c.created_at, c.updated_at, c.edited, i.id, i.project_key,
		i.number, om.role, p.visibility, COALESCE(pm.role, 0)
		FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id
		JOIN orgs o ON o.id=p.org_id JOIN org_members om ON om.org_id=o.id AND om.user_id=$3
		LEFT JOIN project_members pm ON pm.project_id=p.id AND pm.user_id=$3
		WHERE c.id=$1 AND o.slug=$2 FOR UPDATE OF c`, cid, r.PathValue("slug"), uid).
		Scan(&cm.Author, &cm.Body, &cm.Created, &cm.Updated, &cm.Edited, &issueID, &pkey, &num, &orgRole, &vis, &pm)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return nil, nil, UUID{}, 0, false
	}
	if err != nil {
		errInternal(w, err)
		return nil, nil, UUID{}, 0, false
	}
	role := effRole(orgRole, vis, pm)
	if role == 0 {
		errNotFound(w)
		return nil, nil, UUID{}, 0, false
	}
	key := appendInt(append([]byte(pkey), '-'), int64(num))
	return cm, key, issueID, role, true
}

func hPatchComment(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		Body json.RawMessage
	}
	raw, ok := decodeBody(w, r, &in)
	if !ok {
		return
	}
	_ = raw
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	cm, key, _, _, ok := commentAccess(w, r, tx, uid)
	if !ok {
		return
	}
	if cm.Author != uid {
		errForbidden(w)
		return
	}
	var v verrs
	body := ""
	if len(in.Body) == 0 || isNull(in.Body) {
		v.add("body", "required")
	} else if s, ok := str(in.Body); !ok {
		v.add("body", "invalid")
	} else if strings.TrimSpace(s) == "" {
		v.add("body", "too_short")
	} else if runeLen(s) > 20000 {
		v.add("body", "too_long")
	} else {
		body = s
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	now := nowMs()
	if !now.After(cm.Updated) {
		now = cm.Updated.Add(time.Millisecond)
	}
	cm.Body = body
	cm.Updated = now
	cm.Edited = true
	if _, err = tx.Exec(ctx, "UPDATE comments SET body=$2, updated_at=$3, edited=true WHERE id=$1", cm.ID, body,
		now); err != nil {
		errInternal(w, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendComment(nil, cm, key))
}

func hDeleteComment(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	cm, _, issueID, role, ok := commentAccess(w, r, tx, uid)
	if !ok {
		return
	}
	if cm.Author != uid && role < prAdmin {
		errForbidden(w)
		return
	}
	if _, err = tx.Exec(ctx, "DELETE FROM comments WHERE id=$1", cm.ID); err == nil {
		_, err = tx.Exec(ctx, "UPDATE issues SET comment_count=comment_count-1 WHERE id=$1", issueID)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	w.WriteHeader(204)
}

func hHistory(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	c, ok := issueAccess(w, ctx, db, r.PathValue("slug"), r.PathValue("ikey"), uid, false)
	if !ok {
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
	rows, err := db.Query(ctx, `SELECT id, actor_id, created_at, changes FROM history
		WHERE issue_id=$1 AND id < $2 ORDER BY id DESC LIMIT $3`, c.ID, before, limit+1)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer rows.Close()
	var items [][]byte
	var last UUID
	next := ""
	for rows.Next() {
		var id, actor UUID
		var created time.Time
		var changes string
		if err := rows.Scan(&id, &actor, &created, &changes); err != nil {
			errInternal(w, err)
			return
		}
		if len(items) == limit {
			next = uuidStr(last)
			break
		}
		b := make([]byte, 0, 128+len(changes))
		b = append(b, `{"id":"`...)
		b = appendUUID(b, id)
		b = append(b, `","actor_id":"`...)
		b = appendUUID(b, actor)
		b = append(b, `","created_at":`...)
		b = appendTime(b, created)
		b = append(b, `,"changes":`...)
		b = append(b, changes...)
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
