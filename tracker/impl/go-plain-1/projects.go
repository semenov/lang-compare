package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var projKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

type Project struct {
	ID          string
	Key         string
	Name        string
	Description *string
	Visibility  string
	CreatedAt   time.Time
	Role        string // caller's effective role, "" = no access
	Org         *Org
}

func (p *Project) JSON() map[string]any {
	return map[string]any{"id": p.ID, "key": p.Key, "name": p.Name, "description": p.Description,
		"visibility": p.Visibility, "created_at": fmtTS(p.CreatedAt), "my_role": p.Role}
}

func (p *Project) can(role string) bool { return projRank[p.Role] >= projRank[role] }

// effectiveRole is the strongest of the explicit membership and what the org role implies.
func effectiveRole(orgRole string, explicit *string, visibility string) string {
	best := ""
	if explicit != nil {
		best = *explicit
	}
	implied := ""
	switch {
	case orgRole == "owner" || orgRole == "admin":
		implied = "admin"
	case orgRole == "member" && visibility == "org":
		implied = "developer"
	}
	if projRank[implied] > projRank[best] {
		best = implied
	}
	return best
}

const projectSelect = `SELECT p.id, p.key, p.name, p.description, p.visibility, p.created_at, pm.role
	FROM projects p LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $2`

func scanProject(row pgx.Row, org *Org) (*Project, error) {
	p := &Project{Org: org}
	var explicit *string
	if err := row.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.Visibility, &p.CreatedAt, &explicit); err != nil {
		return nil, err
	}
	p.Role = effectiveRole(org.Role, explicit, p.Visibility)
	return p, nil
}

// loadProject returns the project if the caller has an effective role in it, else 404.
func (s *Server) loadProject(ctx context.Context, org *Org, key, uid string) (*Project, error) {
	p, err := scanProject(s.pool.QueryRow(ctx, projectSelect+` WHERE p.org_id = $1 AND p.key = $3`,
		org.ID, uid, strings.ToUpper(key)), org)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, err
	}
	if p.Role == "" {
		return nil, notFound()
	}
	return p, nil
}

// visibleProjects lists the org's projects the caller can see, sorted by key.
func (s *Server) visibleProjects(ctx context.Context, org *Org, uid string) ([]*Project, error) {
	rows, err := s.pool.Query(ctx, projectSelect+` WHERE p.org_id = $1 ORDER BY p.key`, org.ID, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows, org)
		if err != nil {
			return nil, err
		}
		if p.Role != "" {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func (s *Server) orgAndProject(r *http.Request, uid string) (*Project, error) {
	org, err := s.loadOrg(r.Context(), r.PathValue("slug"), uid)
	if err != nil {
		return nil, err
	}
	return s.loadProject(r.Context(), org, r.PathValue("key"), uid)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	org, err := s.loadOrg(ctx, r.PathValue("slug"), uid)
	if err != nil {
		return err
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	key, kst := v.str(o, "key", strSpec{required: true})
	if kst == fSet && !projKeyRe.MatchString(key) {
		v.add("key", "invalid")
	}
	name, _ := v.str(o, "name", strSpec{required: true, trim: true, min: 1, max: 100})
	desc, dst := v.str(o, "description", strSpec{nullable: true, max: 65536})
	vis, vst := v.enum(o, "visibility", false, "org", "private")
	if !v.ok() {
		return v.err()
	}
	if vst != fSet {
		vis = "org"
	}
	p := &Project{ID: newID(), Key: key, Name: name, Visibility: vis, CreatedAt: nowMS(), Role: "admin", Org: org}
	if dst == fSet {
		p.Description = &desc
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO projects (id, org_id, key, name, description, visibility, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (org_id, key) DO NOTHING`,
			p.ID, org.ID, p.Key, p.Name, p.Description, p.Visibility, p.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return newErr(409, "key_taken", "project key already taken")
		}
		_, err = tx.Exec(ctx, `INSERT INTO project_members (project_id, user_id, role) VALUES ($1, $2, 'admin')`, p.ID, uid)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, 201, p.JSON())
	return nil
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request, uid string) error {
	org, err := s.loadOrg(r.Context(), r.PathValue("slug"), uid)
	if err != nil {
		return err
	}
	pg, err := pageFromRequest(r)
	if err != nil {
		return err
	}
	ps, err := s.visibleProjects(r.Context(), org, uid)
	if err != nil {
		return err
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Key < ps[j].Key })
	var items []map[string]any
	for i := pg.offset; i < len(ps) && i <= pg.offset+pg.limit; i++ {
		items = append(items, ps[i].JSON())
	}
	writeJSON(w, 200, finish(pg, items))
	return nil
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, uid string) error {
	p, err := s.orgAndProject(r, uid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, p.JSON())
	return nil
}

func (s *Server) patchProject(w http.ResponseWriter, r *http.Request, uid string) error {
	p, err := s.orgAndProject(r, uid)
	if err != nil {
		return err
	}
	if !p.can("admin") {
		return forbidden()
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	name, nst := v.str(o, "name", strSpec{trim: true, min: 1, max: 100})
	desc, dst := v.str(o, "description", strSpec{nullable: true, max: 65536})
	vis, vst := v.enum(o, "visibility", false, "org", "private")
	if !v.ok() {
		return v.err()
	}
	if nst == fSet {
		p.Name = name
	}
	if dst == fSet {
		p.Description = &desc
	} else if dst == fNull {
		p.Description = nil
	}
	if vst == fSet {
		p.Visibility = vis
	}
	if _, err := s.pool.Exec(r.Context(), `UPDATE projects SET name = $2, description = $3, visibility = $4 WHERE id = $1`,
		p.ID, p.Name, p.Description, p.Visibility); err != nil {
		return err
	}
	// The caller's role may change with visibility.
	p, err = s.loadProject(r.Context(), p.Org, p.Key, uid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, p.JSON())
	return nil
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, uid string) error {
	p, err := s.orgAndProject(r, uid)
	if err != nil {
		return err
	}
	if !p.can("admin") {
		return forbidden()
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM projects WHERE id = $1`, p.ID); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) listProjectMembers(w http.ResponseWriter, r *http.Request, uid string) error {
	p, err := s.orgAndProject(r, uid)
	if err != nil {
		return err
	}
	pg, err := pageFromRequest(r)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT u.id, u.email, u.name, m.role FROM project_members m
		JOIN users u ON u.id = m.user_id WHERE m.project_id = $1 ORDER BY u.email, u.id`+pg.sql(), p.ID)
	if err != nil {
		return err
	}
	items, err := scanMembers(rows)
	if err != nil {
		return err
	}
	writeJSON(w, 200, finish(pg, items))
	return nil
}

func (s *Server) putProjectMember(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	p, err := s.orgAndProject(r, uid)
	if err != nil {
		return err
	}
	if !p.can("admin") {
		return forbidden()
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	role, _ := v.enum(o, "role", true, "admin", "developer", "viewer")
	target := strings.ToLower(r.PathValue("user_id"))
	m := member{Role: role}
	if !isUUID(target) {
		v.add("user_id", "invalid")
	} else {
		err := s.pool.QueryRow(ctx, `SELECT u.id, u.email, u.name FROM users u
			JOIN org_members om ON om.user_id = u.id AND om.org_id = $2 WHERE u.id = $1`, target, p.Org.ID).
			Scan(&m.UserID, &m.Email, &m.Name)
		if errors.Is(err, pgx.ErrNoRows) {
			v.add("user_id", "invalid")
		} else if err != nil {
			return err
		}
	}
	if !v.ok() {
		return v.err()
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO project_members (project_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role`, p.ID, target, role); err != nil {
		return err
	}
	writeJSON(w, 200, m)
	return nil
}

func (s *Server) deleteProjectMember(w http.ResponseWriter, r *http.Request, uid string) error {
	p, err := s.orgAndProject(r, uid)
	if err != nil {
		return err
	}
	if !p.can("admin") {
		return forbidden()
	}
	target := strings.ToLower(r.PathValue("user_id"))
	if !isUUID(target) {
		return notFound()
	}
	tag, err := s.pool.Exec(r.Context(), `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, p.ID, target)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return notFound()
	}
	w.WriteHeader(204)
	return nil
}
