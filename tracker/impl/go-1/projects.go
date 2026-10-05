package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// project roles
const (
	prViewer    = 1
	prDeveloper = 2
	prAdmin     = 3
)

var projRoleNames = []string{"", "viewer", "developer", "admin"}
var visNames = []string{"org", "private"}

var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

func effRole(orgRole, vis, pmRole int16) int16 {
	if orgRole >= orgAdmin {
		return prAdmin
	}
	r := pmRole
	if orgRole == orgMember && vis == 0 && r < prDeveloper {
		r = prDeveloper
	}
	return r
}

func parseProjRole(raw json.RawMessage) int16 {
	s, _ := str(raw)
	switch s {
	case "viewer":
		return prViewer
	case "developer":
		return prDeveloper
	case "admin":
		return prAdmin
	}
	return 0
}

type project struct {
	OrgID   UUID
	OrgRole int16
	ID      UUID
	Key     string
	Name    string
	Desc    pgtype.Text
	Vis     int16
	Created time.Time
	Role    int16 // effective
}

func appendProject(b []byte, p *project) []byte {
	b = append(b, `{"id":"`...)
	b = appendUUID(b, p.ID)
	b = append(b, `","key":`...)
	b = appendStr(b, p.Key)
	b = append(b, `,"name":`...)
	b = appendStr(b, p.Name)
	b = append(b, `,"description":`...)
	if p.Desc.Valid {
		b = appendStr(b, p.Desc.String)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"visibility":"`...)
	b = append(b, visNames[p.Vis]...)
	b = append(b, `","created_at":`...)
	b = appendTime(b, p.Created)
	b = append(b, `,"my_role":"`...)
	b = append(b, projRoleNames[p.Role]...)
	return append(b, `"}`...)
}

// projectAccess loads a project visible to the caller; writes 404 otherwise.
func projectAccess(w http.ResponseWriter, ctx context.Context, slug, key string, uid UUID) (*project, bool) {
	p := &project{}
	var pmRole int16
	err := db.QueryRow(ctx, `SELECT o.id, om.role, p.id, p.key, p.name, p.description, p.visibility, p.created_at,
		COALESCE(pm.role, 0)
		FROM orgs o JOIN org_members om ON om.org_id=o.id AND om.user_id=$3
		JOIN projects p ON p.org_id=o.id AND p.key=$2
		LEFT JOIN project_members pm ON pm.project_id=p.id AND pm.user_id=$3
		WHERE o.slug=$1`, slug, strings.ToUpper(key), uid).
		Scan(&p.OrgID, &p.OrgRole, &p.ID, &p.Key, &p.Name, &p.Desc, &p.Vis, &p.Created, &pmRole)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return nil, false
	}
	if err != nil {
		errInternal(w, err)
		return nil, false
	}
	p.Role = effRole(p.OrgRole, p.Vis, pmRole)
	if p.Role == 0 {
		errNotFound(w)
		return nil, false
	}
	return p, true
}

func parseVis(raw json.RawMessage) (int16, bool) {
	s, ok := str(raw)
	if !ok {
		return 0, false
	}
	switch s {
	case "org":
		return 0, true
	case "private":
		return 1, true
	}
	return 0, false
}

func optDesc(v *verrs, field string, raw json.RawMessage, max int) (pgtype.Text, bool) {
	if len(raw) == 0 || isNull(raw) {
		return pgtype.Text{}, true
	}
	s, ok := str(raw)
	if !ok {
		v.add(field, "invalid")
		return pgtype.Text{}, false
	}
	if runeLen(s) > max {
		v.add(field, "too_long")
		return pgtype.Text{}, false
	}
	return pgtype.Text{String: s, Valid: true}, true
}

func hCreateProject(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		Key, Name, Description, Visibility json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	oid, orgRole, ok := orgAccess(w, ctx, r.PathValue("slug"), uid)
	if !ok {
		return
	}
	var v verrs
	key := ""
	if len(in.Key) == 0 || isNull(in.Key) {
		v.add("key", "required")
	} else if s, ok := str(in.Key); !ok || !keyRe.MatchString(s) {
		v.add("key", "invalid")
	} else {
		key = s
	}
	name, _ := reqString(&v, "name", in.Name, true, 1, 100)
	desc, _ := optDesc(&v, "description", in.Description, 65536)
	var vis int16
	if len(in.Visibility) > 0 && !isNull(in.Visibility) {
		var ok bool
		if vis, ok = parseVis(in.Visibility); !ok {
			v.add("visibility", "invalid")
		}
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	p := &project{OrgID: oid, OrgRole: orgRole, ID: newID(), Key: key, Name: name, Desc: desc, Vis: vis,
		Created: nowMs(), Role: prAdmin}
	tx, err := db.Begin(ctx)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO projects (id, org_id, key, name, description, visibility, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, p.ID, oid, key, name, desc, vis, p.Created)
	if err != nil {
		if isUnique(err) {
			errConflict(w, "key_taken", "project key already taken")
			return
		}
		errInternal(w, err)
		return
	}
	if _, err = tx.Exec(ctx, "INSERT INTO project_members (project_id, user_id, role) VALUES ($1,$2,3)", p.ID,
		uid); err != nil {
		errInternal(w, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 201, appendProject(nil, p))
}

func hListProjects(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	oid, orgRole, ok := orgAccess(w, ctx, r.PathValue("slug"), uid)
	if !ok {
		return
	}
	cur, _ := decCursor(r.URL.Query().Get("cursor"))
	rows, err := db.Query(ctx, `SELECT p.id, p.key, p.name, p.description, p.visibility, p.created_at,
		COALESCE(pm.role, 0) FROM projects p
		LEFT JOIN project_members pm ON pm.project_id=p.id AND pm.user_id=$2
		WHERE p.org_id=$1 AND p.key > $3 AND ($4 >= 2 OR p.visibility=0 OR pm.role IS NOT NULL)
		ORDER BY p.key LIMIT $5`, oid, uid, cur, orgRole, limit+1)
	if err != nil {
		errInternal(w, err)
		return
	}
	defer rows.Close()
	var items [][]byte
	var last, next string
	for rows.Next() {
		p := project{OrgRole: orgRole}
		var pm int16
		if err := rows.Scan(&p.ID, &p.Key, &p.Name, &p.Desc, &p.Vis, &p.Created, &pm); err != nil {
			errInternal(w, err)
			return
		}
		if len(items) == limit {
			next = last
			break
		}
		p.Role = effRole(orgRole, p.Vis, pm)
		items = append(items, appendProject(nil, &p))
		last = p.Key
	}
	if err := rows.Err(); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendPage(nil, items, next))
}

func hGetProject(w http.ResponseWriter, r *http.Request, uid UUID) {
	p, ok := projectAccess(w, r.Context(), r.PathValue("slug"), r.PathValue("key"), uid)
	if !ok {
		return
	}
	writeJSON(w, 200, appendProject(nil, p))
}

func hPatchProject(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		Name, Description, Visibility json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	p, ok := projectAccess(w, ctx, r.PathValue("slug"), r.PathValue("key"), uid)
	if !ok {
		return
	}
	if p.Role < prAdmin {
		errForbidden(w)
		return
	}
	var v verrs
	if len(in.Name) > 0 {
		if n, ok := reqString(&v, "name", in.Name, true, 1, 100); ok {
			p.Name = n
		}
	}
	if len(in.Description) > 0 {
		if d, ok := optDesc(&v, "description", in.Description, 65536); ok {
			p.Desc = d
		}
	}
	if len(in.Visibility) > 0 {
		if vis, ok := parseVis(in.Visibility); ok {
			p.Vis = vis
		} else {
			v.add("visibility", "invalid")
		}
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	if _, err := db.Exec(ctx, "UPDATE projects SET name=$2, description=$3, visibility=$4 WHERE id=$1", p.ID, p.Name,
		p.Desc, p.Vis); err != nil {
		errInternal(w, err)
		return
	}
	// my_role may change with visibility; re-derive from explicit membership
	var pm int16
	db.QueryRow(ctx, "SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2", p.ID, uid).Scan(&pm)
	p.Role = effRole(p.OrgRole, p.Vis, pm)
	writeJSON(w, 200, appendProject(nil, p))
}

func hDeleteProject(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	p, ok := projectAccess(w, ctx, r.PathValue("slug"), r.PathValue("key"), uid)
	if !ok {
		return
	}
	if p.Role < prAdmin {
		errForbidden(w)
		return
	}
	if _, err := db.Exec(ctx, "DELETE FROM projects WHERE id=$1", p.ID); err != nil {
		errInternal(w, err)
		return
	}
	w.WriteHeader(204)
}

func hListProjectMembers(w http.ResponseWriter, r *http.Request, uid UUID) {
	p, ok := projectAccess(w, r.Context(), r.PathValue("slug"), r.PathValue("key"), uid)
	if !ok {
		return
	}
	listMembers(w, r, `SELECT u.id, u.email, u.name, m.role FROM project_members m JOIN users u ON u.id=m.user_id
		WHERE m.project_id=$1 AND u.email > $2 ORDER BY u.email LIMIT $3`, projRoleNames, p.ID)
}

func hPutProjectMember(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	var in struct {
		Role json.RawMessage
	}
	if _, ok := decodeBody(w, r, &in); !ok {
		return
	}
	p, ok := projectAccess(w, ctx, r.PathValue("slug"), r.PathValue("key"), uid)
	if !ok {
		return
	}
	if p.Role < prAdmin {
		errForbidden(w)
		return
	}
	role := parseProjRole(in.Role)
	if role == 0 {
		errValidation1(w, "role", "invalid")
		return
	}
	tid, ok := parseUUID(r.PathValue("user_id"))
	if !ok {
		errValidation1(w, "user_id", "invalid")
		return
	}
	var email, name string
	err := db.QueryRow(ctx, `SELECT u.email, u.name FROM org_members m JOIN users u ON u.id=m.user_id
		WHERE m.org_id=$1 AND m.user_id=$2`, p.OrgID, tid).Scan(&email, &name)
	if err == pgx.ErrNoRows {
		errValidation1(w, "user_id", "invalid")
		return
	}
	if err != nil {
		errInternal(w, err)
		return
	}
	if _, err = db.Exec(ctx, `INSERT INTO project_members (project_id, user_id, role) VALUES ($1,$2,$3)
		ON CONFLICT (project_id, user_id) DO UPDATE SET role=EXCLUDED.role`, p.ID, tid, role); err != nil {
		errInternal(w, err)
		return
	}
	writeJSON(w, 200, appendMember(nil, tid, email, name, projRoleNames[role]))
}

func hDeleteProjectMember(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	p, ok := projectAccess(w, ctx, r.PathValue("slug"), r.PathValue("key"), uid)
	if !ok {
		return
	}
	if p.Role < prAdmin {
		errForbidden(w)
		return
	}
	tid, ok := parseUUID(r.PathValue("user_id"))
	if !ok {
		errNotFound(w)
		return
	}
	tag, err := db.Exec(ctx, "DELETE FROM project_members WHERE project_id=$1 AND user_id=$2", p.ID, tid)
	if err != nil {
		errInternal(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		errNotFound(w)
		return
	}
	w.WriteHeader(204)
}
