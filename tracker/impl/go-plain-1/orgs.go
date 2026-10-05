package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	orgRank  = map[string]int{"member": 1, "admin": 2, "owner": 3}
	projRank = map[string]int{"viewer": 1, "developer": 2, "admin": 3}
	slugRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
)

type Org struct {
	ID        string
	Slug      string
	Name      string
	CreatedAt time.Time
	Role      string // caller's role
}

func (o *Org) JSON() map[string]any {
	return map[string]any{"id": o.ID, "slug": o.Slug, "name": o.Name, "created_at": fmtTS(o.CreatedAt), "my_role": o.Role}
}

// loadOrg returns the org if the caller is a member, else 404.
func (s *Server) loadOrg(ctx context.Context, slug, uid string) (*Org, error) {
	o := &Org{}
	err := s.pool.QueryRow(ctx, `SELECT o.id, o.slug, o.name, o.created_at, m.role FROM orgs o
		JOIN org_members m ON m.org_id = o.id AND m.user_id = $2 WHERE o.slug = $1`, slug, uid).
		Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt, &o.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	return o, err
}

func (o *Org) isAdmin() bool { return orgRank[o.Role] >= orgRank["admin"] }

type member struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Role   string `json:"role"`
}

func scanMembers(rows pgx.Rows) ([]member, error) {
	defer rows.Close()
	var out []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request, uid string) error {
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	name, _ := v.str(o, "name", strSpec{required: true, trim: true, min: 1, max: 100})
	slug, st := v.str(o, "slug", strSpec{required: true})
	if st == fSet && !slugRe.MatchString(slug) {
		v.add("slug", "invalid")
	}
	if !v.ok() {
		return v.err()
	}
	org := &Org{ID: newID(), Slug: slug, Name: name, CreatedAt: nowMS(), Role: "owner"}
	ctx := r.Context()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO orgs (id, slug, name, created_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (slug) DO NOTHING`, org.ID, org.Slug, org.Name, org.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return newErr(409, "slug_taken", "slug already taken")
		}
		_, err = tx.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, org.ID, uid)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, 201, org.JSON())
	return nil
}

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request, uid string) error {
	p, err := pageFromRequest(r)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT o.id, o.slug, o.name, o.created_at, m.role FROM orgs o
		JOIN org_members m ON m.org_id = o.id AND m.user_id = $1 ORDER BY o.slug`+p.sql(), uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []map[string]any
	for rows.Next() {
		o := &Org{}
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt, &o.Role); err != nil {
			return err
		}
		items = append(items, o.JSON())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, finish(p, items))
	return nil
}

func (s *Server) getOrg(w http.ResponseWriter, r *http.Request, uid string) error {
	org, err := s.loadOrg(r.Context(), r.PathValue("slug"), uid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, org.JSON())
	return nil
}

func (s *Server) listOrgMembers(w http.ResponseWriter, r *http.Request, uid string) error {
	org, err := s.loadOrg(r.Context(), r.PathValue("slug"), uid)
	if err != nil {
		return err
	}
	p, err := pageFromRequest(r)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT u.id, u.email, u.name, m.role FROM org_members m
		JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 ORDER BY u.email, u.id`+p.sql(), org.ID)
	if err != nil {
		return err
	}
	items, err := scanMembers(rows)
	if err != nil {
		return err
	}
	writeJSON(w, 200, finish(p, items))
	return nil
}

var orgRoles = []string{"owner", "admin", "member"}

func (s *Server) addOrgMember(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	org, err := s.loadOrg(ctx, r.PathValue("slug"), uid)
	if err != nil {
		return err
	}
	if !org.isAdmin() {
		return forbidden()
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	email, est := v.str(o, "email", strSpec{required: true, trim: true})
	role, _ := v.enum(o, "role", true, orgRoles...)
	var m member
	if est == fSet {
		err := s.pool.QueryRow(ctx, `SELECT id, email, name FROM users WHERE email = $1`, strings.ToLower(email)).
			Scan(&m.UserID, &m.Email, &m.Name)
		if errors.Is(err, pgx.ErrNoRows) {
			v.add("email", "invalid")
		} else if err != nil {
			return err
		}
	}
	if !v.ok() {
		return v.err()
	}
	if role != "member" && org.Role != "owner" {
		return forbidden()
	}
	m.Role = role
	tag, err := s.pool.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, org.ID, m.UserID, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return newErr(409, "already_member", "user is already a member")
	}
	writeJSON(w, 201, m)
	return nil
}

// lockOrgMember locks the org (serializing membership changes) and returns the target's role.
func lockOrgMember(ctx context.Context, tx pgx.Tx, orgID, target string) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE`, orgID); err != nil {
		return "", err
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, target).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", notFound()
	}
	return role, err
}

func countOwners(ctx context.Context, tx pgx.Tx, orgID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id = $1 AND role = 'owner'`, orgID).Scan(&n)
	return n, err
}

func (s *Server) patchOrgMember(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	org, err := s.loadOrg(ctx, r.PathValue("slug"), uid)
	if err != nil {
		return err
	}
	if !org.isAdmin() {
		return forbidden()
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	role, _ := v.enum(o, "role", true, orgRoles...)
	if !v.ok() {
		return v.err()
	}
	target := strings.ToLower(r.PathValue("user_id"))
	if !isUUID(target) {
		return notFound()
	}
	var m member
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockOrgMember(ctx, tx, org.ID, target)
		if err != nil {
			return err
		}
		if (cur != "member" || role != "member") && org.Role != "owner" {
			return forbidden()
		}
		if cur == "owner" && role != "owner" {
			n, err := countOwners(ctx, tx, org.ID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return newErr(409, "last_owner", "the organization must keep at least one owner")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2`,
			org.ID, target, role); err != nil {
			return err
		}
		m.Role = role
		return tx.QueryRow(ctx, `SELECT id, email, name FROM users WHERE id = $1`, target).Scan(&m.UserID, &m.Email, &m.Name)
	})
	if err != nil {
		return err
	}
	writeJSON(w, 200, m)
	return nil
}

func (s *Server) deleteOrgMember(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	org, err := s.loadOrg(ctx, r.PathValue("slug"), uid)
	if err != nil {
		return err
	}
	target := strings.ToLower(r.PathValue("user_id"))
	self := target == uid
	if !self && !org.isAdmin() {
		return forbidden()
	}
	if !isUUID(target) {
		return notFound()
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockOrgMember(ctx, tx, org.ID, target)
		if err != nil {
			return err
		}
		if !self && cur != "member" && org.Role != "owner" {
			return forbidden()
		}
		if cur == "owner" {
			n, err := countOwners(ctx, tx, org.ID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return newErr(409, "last_owner", "the organization must keep at least one owner")
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, org.ID, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM project_members WHERE user_id = $2 AND project_id IN
			(SELECT id FROM projects WHERE org_id = $1)`, org.ID, target); err != nil {
			return err
		}
		return s.unassignInOrg(ctx, tx, org, uid, target)
	})
	if err != nil {
		return err
	}
	s.kick()
	w.WriteHeader(204)
	return nil
}

// unassignInOrg clears the assignee on every issue of the org assigned to target (a regular issue update).
func (s *Server) unassignInOrg(ctx context.Context, tx pgx.Tx, org *Org, actor, target string) error {
	issues, err := queryIssues(ctx, tx, issueSelect+` WHERE p.org_id = $1 AND i.assignee_id = $2 ORDER BY i.id FOR UPDATE OF i`,
		org.ID, target)
	if err != nil {
		return err
	}
	for _, old := range issues {
		nw := *old
		nw.AssigneeID = nil
		if err := s.saveIssueUpdate(ctx, tx, org, actor, old, &nw, diffIssues(old, &nw)); err != nil {
			return err
		}
	}
	return nil
}
