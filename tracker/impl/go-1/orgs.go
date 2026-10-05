package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// org roles
const (
	orgMember = 1
	orgAdmin  = 2
	orgOwner  = 3
)

var orgRoleNames = []string{"", "member", "admin", "owner"}

func parseOrgRole(raw json.RawMessage) int16 {
	s, _ := str(raw)
	switch s {
	case "member":
		return orgMember
	case "admin":
		return orgAdmin
	case "owner":
		return orgOwner
	}
	return 0
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// orgAccess resolves an org by slug for a caller; writes 404 and returns ok=false if not a member.
func orgAccess(w http.ResponseWriter, ctx context.Context, slug string, uid UUID) (UUID, int16, bool) {
	var oid UUID
	var role int16
	err := db.QueryRow(ctx, `SELECT o.id, m.role FROM orgs o JOIN org_members m ON m.org_id=o.id AND m.user_id=$2
		WHERE o.slug=$1`, slug, uid).Scan(&oid, &role)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return oid, 0, false
	}
	if err != nil {
		errInternal(w, err)
		return oid, 0, false
	}
	return oid, role, true
}

func appendOrg(b []byte, id UUID, slug, name string, created time.Time, role int16) []byte {
	b = append(b, `{"id":"`...)
	b = appendUUID(b, id)
	b = append(b, `","slug":`...)
	b = appendStr(b, slug)
	b = append(b, `,"name":`...)
	b = appendStr(b, name)
	b = append(b, `,"created_at":`...)
	b = appendTime(b, created)
	b = append(b, `,"my_role":"`...)
	b = append(b, orgRoleNames[role]...)
	return append(b, `"}`...)
}

func hCreateOrg(w http.ResponseWriter, r *http.Request, uid UUID) {
	var in struct {
		Name, Slug json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	var v verrs
	name, _ := reqString(&v, "name", in.Name, true, 1, 100)
	slug := ""
	if len(in.Slug) == 0 || isNull(in.Slug) {
		v.add("slug", "required")
	} else if s, ok := str(in.Slug); !ok || !slugRe.MatchString(s) {
		v.add("slug", "invalid")
	} else {
		slug = s
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	ctx := r.Context()
	id := newID()
	now := nowMs()
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "INSERT INTO orgs (id, slug, name, created_at) VALUES ($1,$2,$3,$4)", id, slug, name,
		now); err != nil {
		if isUnique(err) {
			errConflict(w, "slug_taken", "slug already taken")
			return
		}
		errInternal(w, err)
		return
	}
	if _, err = tx.Exec(ctx, "INSERT INTO org_members (org_id, user_id, role) VALUES ($1,$2,3)", id, uid); err != nil {
		errInternal(w, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 201, appendOrg(nil, id, slug, name, now, orgOwner))
}

func hListOrgs(w http.ResponseWriter, r *http.Request, uid UUID) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	cur, _ := decCursor(r.URL.Query().Get("cursor"))
	rows, err := db.Query(r.Context(), `SELECT o.id, o.slug, o.name, o.created_at, m.role FROM org_members m
		JOIN orgs o ON o.id=m.org_id WHERE m.user_id=$1 AND o.slug > $2 ORDER BY o.slug LIMIT $3`, uid, cur, limit+1)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer rows.Close()
	var items [][]byte
	var last, next string
	for rows.Next() {
		var id UUID
		var slug, name string
		var created time.Time
		var role int16
		if err := rows.Scan(&id, &slug, &name, &created, &role); err != nil {
			errInternal(w, err)
			return
		}
		if len(items) == limit {
			next = last
			break
		}
		items = append(items, appendOrg(nil, id, slug, name, created, role))
		last = slug
	}
	if err := rows.Err(); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendPage(nil, items, next))
}

func hGetOrg(w http.ResponseWriter, r *http.Request, uid UUID) {
	var id UUID
	var slug, name string
	var created time.Time
	var role int16
	err := db.QueryRow(r.Context(), `SELECT o.id, o.slug, o.name, o.created_at, m.role FROM orgs o
		JOIN org_members m ON m.org_id=o.id AND m.user_id=$2 WHERE o.slug=$1`, r.PathValue("slug"), uid).
		Scan(&id, &slug, &name, &created, &role)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendOrg(nil, id, slug, name, created, role))
}

func appendMember(b []byte, id UUID, email, name, role string) []byte {
	b = append(b, `{"user_id":"`...)
	b = appendUUID(b, id)
	b = append(b, `","email":`...)
	b = appendStr(b, email)
	b = append(b, `,"name":`...)
	b = appendStr(b, name)
	b = append(b, `,"role":"`...)
	b = append(b, role...)
	return append(b, `"}`...)
}

// listMembers renders a members page from a query selecting (user_id, email, name, role) ordered by email.
func listMembers(w http.ResponseWriter, r *http.Request, sql string, names []string, args ...any) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	cur, _ := decCursor(r.URL.Query().Get("cursor"))
	args = append(args, cur, limit+1)
	rows, err := db.Query(r.Context(), sql, args...)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer rows.Close()
	var items [][]byte
	var last, next string
	for rows.Next() {
		var id UUID
		var email, name string
		var role int16
		if err := rows.Scan(&id, &email, &name, &role); err != nil {
			errInternal(w, err)
			return
		}
		if len(items) == limit {
			next = last
			break
		}
		items = append(items, appendMember(nil, id, email, name, names[role]))
		last = email
	}
	if err := rows.Err(); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendPage(nil, items, next))
}

func hListOrgMembers(w http.ResponseWriter, r *http.Request, uid UUID) {
	oid, _, ok := orgAccess(w, r.Context(), r.PathValue("slug"), uid)
	if !ok {
		return
	}
	listMembers(w, r, `SELECT u.id, u.email, u.name, m.role FROM org_members m JOIN users u ON u.id=m.user_id
		WHERE m.org_id=$1 AND u.email > $2 ORDER BY u.email LIMIT $3`, orgRoleNames, oid)
}

func hAddOrgMember(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		Email, Role json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	oid, myRole, ok := orgAccess(w, ctx, r.PathValue("slug"), uid)
	if !ok {
		return
	}
	if myRole < orgAdmin {
		errForbidden(w)
		return
	}
	var v verrs
	email, _ := validEmail(&v, in.Email)
	role := parseOrgRole(in.Role)
	if role == 0 {
		if len(in.Role) == 0 {
			v.add("role", "required")
		} else {
			v.add("role", "invalid")
		}
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	if role >= orgAdmin && myRole < orgOwner {
		errForbidden(w)
		return
	}
	var tid UUID
	var name string
	err := db.QueryRow(ctx, "SELECT id, name FROM users WHERE email=$1", email).Scan(&tid, &name)
	if err == pgx.ErrNoRows {
		errValidation1(w, "email", "invalid")
		return
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	_, err = db.Exec(ctx, "INSERT INTO org_members (org_id, user_id, role) VALUES ($1,$2,$3)", oid, tid, role)
	if err != nil {
		if isUnique(err) {
			errConflict(w, "already_member", "user is already a member")
			return
		}
		errInternal(w, err)
		return
	}
	writeJSON(w, 201, appendMember(nil, tid, email, name, orgRoleNames[role]))
}

func hPatchOrgMember(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		Role json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	oid, myRole, ok := orgAccess(w, ctx, r.PathValue("slug"), uid)
	if !ok {
		return
	}
	if myRole < orgAdmin {
		errForbidden(w)
		return
	}
	role := parseOrgRole(in.Role)
	if role == 0 {
		errValidation1(w, "role", "invalid")
		return
	}
	tid, ok := parseUUID(r.PathValue("user_id"))
	if !ok {
		errNotFound(w)
		return
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	// lock the org's membership set so last-owner checks are race free
	if _, err = tx.Exec(ctx, "SELECT 1 FROM orgs WHERE id=$1 FOR UPDATE", oid); err != nil {
		errInternal(w, err)
		return
	}
	var cur int16
	var email, name string
	err = tx.QueryRow(ctx, `SELECT m.role, u.email, u.name FROM org_members m JOIN users u ON u.id=m.user_id
		WHERE m.org_id=$1 AND m.user_id=$2`, oid, tid).Scan(&cur, &email, &name)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	if (role >= orgAdmin || cur >= orgAdmin) && myRole < orgOwner && role != cur {
		errForbidden(w)
		return
	}
	if cur == orgOwner && role != orgOwner {
		var n int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM org_members WHERE org_id=$1 AND role=3", oid).Scan(&n); err != nil {
			errInternal(w, err)
			return
		}
		if n <= 1 {
			errConflict(w, "last_owner", "cannot remove the last owner")
			return
		}
	}
	if role != cur {
		if _, err = tx.Exec(ctx, "UPDATE org_members SET role=$3 WHERE org_id=$1 AND user_id=$2", oid, tid, role); err != nil {
			errInternal(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendMember(nil, tid, email, name, orgRoleNames[role]))
}

func hDeleteOrgMember(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	oid, myRole, ok := orgAccess(w, ctx, r.PathValue("slug"), uid)
	if !ok {
		return
	}
	tid, ok := parseUUID(r.PathValue("user_id"))
	if !ok {
		errNotFound(w)
		return
	}
	self := tid == uid
	if !self && myRole < orgAdmin {
		errForbidden(w)
		return
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT 1 FROM orgs WHERE id=$1 FOR UPDATE", oid); err != nil {
		errInternal(w, err)
		return
	}
	var cur int16
	err = tx.QueryRow(ctx, "SELECT role FROM org_members WHERE org_id=$1 AND user_id=$2", oid, tid).Scan(&cur)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	if !self && cur >= orgAdmin && myRole < orgOwner {
		errForbidden(w)
		return
	}
	if cur == orgOwner {
		var n int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM org_members WHERE org_id=$1 AND role=3", oid).Scan(&n); err != nil {
			errInternal(w, err)
			return
		}
		if n <= 1 {
			errConflict(w, "last_owner", "cannot remove the last owner")
			return
		}
	}
	now := nowMs()
	_, err = tx.Exec(ctx, "DELETE FROM org_members WHERE org_id=$1 AND user_id=$2", oid, tid)
	if err == nil {
		_, err = tx.Exec(ctx, `DELETE FROM project_members WHERE user_id=$2
			AND project_id IN (SELECT id FROM projects WHERE org_id=$1)`, oid, tid)
	}
	if err == nil {
		err = unassignAll(ctx, tx, oid, tid, uid, now)
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

// unassignAll clears the assignee of every issue in the org assigned to tid, recording history.
func unassignAll(ctx context.Context, tx pgx.Tx, oid, tid, actor UUID, now time.Time) error {
	rows, err := tx.Query(ctx, `UPDATE issues SET assignee_id=NULL, version=version+1, updated_at=$3
		WHERE org_id=$1 AND assignee_id=$2 RETURNING id`, oid, tid, now)
	if err != nil {
		return err
	}
	var ids []UUID
	for rows.Next() {
		var id UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	ch := make([]byte, 0, 80)
	ch = append(ch, `[{"field":"assignee_id","from":"`...)
	ch = appendUUID(ch, tid)
	ch = append(ch, `","to":null}]`...)
	hids := make([]UUID, len(ids))
	for i := range ids {
		hids[i] = newID()
	}
	_, err = tx.Exec(ctx, `INSERT INTO history (id, issue_id, actor_id, created_at, changes)
		SELECT unnest($1::uuid[]), unnest($2::uuid[]), $3, $4, $5`, hids, ids, actor, now, string(ch))
	return err
}
