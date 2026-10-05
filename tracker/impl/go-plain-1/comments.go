package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Comment struct {
	ID        string
	IssueKey  string
	AuthorID  string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
	Edited    bool
}

func (c *Comment) JSON() map[string]any {
	return map[string]any{"id": c.ID, "issue_key": c.IssueKey, "author_id": c.AuthorID, "body": c.Body,
		"created_at": fmtTS(c.CreatedAt), "updated_at": fmtTS(c.UpdatedAt), "edited": c.Edited}
}

var commentBody = strSpec{required: true, min: 1, max: 20000}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	p, cur, err := s.loadIssueCtx(r, uid)
	if err != nil {
		return err
	}
	if !p.can("viewer") {
		return forbidden()
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	body, _ := v.str(o, "body", commentBody)
	if !v.ok() {
		return v.err()
	}
	now := nowMS()
	c := &Comment{ID: newID(), IssueKey: cur.Key(), AuthorID: uid, Body: body, CreatedAt: now, UpdatedAt: now}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE issues SET comment_count = comment_count + 1 WHERE id = $1`, cur.ID); err != nil {
			return err
		}
		i, err := queryIssue(ctx, tx, issueSelect+` WHERE i.id = $1`, cur.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO comments (id, issue_id, author_id, body, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, c.ID, cur.ID, uid, body, now, now); err != nil {
			return err
		}
		return enqueue(ctx, tx, p.Org, "comment.created", uid, i.ID,
			map[string]any{"issue": i.JSON(), "comment": c.JSON()}, now)
	})
	if err != nil {
		return err
	}
	s.kick()
	writeJSON(w, 201, c.JSON())
	return nil
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request, uid string) error {
	_, i, err := s.loadIssueCtx(r, uid)
	if err != nil {
		return err
	}
	pg, err := pageFromRequest(r)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT id, author_id, body, created_at, updated_at, edited FROM comments
		WHERE issue_id = $1 ORDER BY seq`+pg.sql(), i.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []map[string]any
	for rows.Next() {
		c := &Comment{IssueKey: i.Key()}
		if err := rows.Scan(&c.ID, &c.AuthorID, &c.Body, &c.CreatedAt, &c.UpdatedAt, &c.Edited); err != nil {
			return err
		}
		items = append(items, c.JSON())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, finish(pg, items))
	return nil
}

// loadComment finds a comment in the org whose project the caller can see.
func (s *Server) loadComment(r *http.Request, uid string) (*Project, *Comment, string, error) {
	ctx := r.Context()
	org, err := s.loadOrg(ctx, r.PathValue("slug"), uid)
	if err != nil {
		return nil, nil, "", err
	}
	id := strings.ToLower(r.PathValue("id"))
	if !isUUID(id) {
		return nil, nil, "", notFound()
	}
	c := &Comment{}
	var pkey, issueID string
	var num int
	err = s.pool.QueryRow(ctx, `SELECT c.id, c.author_id, c.body, c.created_at, c.updated_at, c.edited, p.key, i.number, i.id
		FROM comments c JOIN issues i ON i.id = c.issue_id JOIN projects p ON p.id = i.project_id
		WHERE c.id = $1 AND p.org_id = $2`, id, org.ID).
		Scan(&c.ID, &c.AuthorID, &c.Body, &c.CreatedAt, &c.UpdatedAt, &c.Edited, &pkey, &num, &issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, "", notFound()
	} else if err != nil {
		return nil, nil, "", err
	}
	p, err := s.loadProject(ctx, org, pkey, uid)
	if err != nil {
		return nil, nil, "", err
	}
	c.IssueKey = (&Issue{ProjectKey: pkey, Number: num}).Key()
	return p, c, issueID, nil
}

func (s *Server) patchComment(w http.ResponseWriter, r *http.Request, uid string) error {
	_, c, _, err := s.loadComment(r, uid)
	if err != nil {
		return err
	}
	if c.AuthorID != uid {
		return forbidden()
	}
	o, _, err := readObj(r)
	if err != nil {
		return err
	}
	v := &verrs{}
	body, _ := v.str(o, "body", commentBody)
	if !v.ok() {
		return v.err()
	}
	c.Body, c.UpdatedAt, c.Edited = body, nowMS(), true
	if _, err := s.pool.Exec(r.Context(), `UPDATE comments SET body = $2, updated_at = $3, edited = true WHERE id = $1`,
		c.ID, c.Body, c.UpdatedAt); err != nil {
		return err
	}
	writeJSON(w, 200, c.JSON())
	return nil
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	p, c, issueID, err := s.loadComment(r, uid)
	if err != nil {
		return err
	}
	if c.AuthorID != uid && !p.can("admin") {
		return forbidden()
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM comments WHERE id = $1`, c.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return notFound()
		}
		_, err = tx.Exec(ctx, `UPDATE issues SET comment_count = comment_count - 1 WHERE id = $1`, issueID)
		return err
	})
	if err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}
