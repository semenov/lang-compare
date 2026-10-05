package main

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id uuid PRIMARY KEY,
	email text NOT NULL UNIQUE,
	name text NOT NULL,
	password_hash text NOT NULL,
	created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS refresh_tokens (
	token_hash text PRIMARY KEY,
	user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
	chain_id uuid NOT NULL,
	expires_at timestamptz NOT NULL,
	used boolean NOT NULL DEFAULT false,
	revoked boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS refresh_tokens_chain ON refresh_tokens (chain_id);
CREATE TABLE IF NOT EXISTS orgs (
	id uuid PRIMARY KEY,
	slug text NOT NULL UNIQUE,
	name text NOT NULL,
	created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS org_members (
	org_id uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
	user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
	role text NOT NULL,
	PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS org_members_user ON org_members (user_id);
CREATE TABLE IF NOT EXISTS projects (
	id uuid PRIMARY KEY,
	org_id uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
	key text NOT NULL,
	name text NOT NULL,
	description text,
	visibility text NOT NULL,
	created_at timestamptz NOT NULL,
	last_number integer NOT NULL DEFAULT 0,
	UNIQUE (org_id, key)
);
CREATE TABLE IF NOT EXISTS project_members (
	project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
	user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
	role text NOT NULL,
	PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS project_members_user ON project_members (user_id);
CREATE TABLE IF NOT EXISTS issues (
	id uuid PRIMARY KEY,
	project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
	number integer NOT NULL,
	type text NOT NULL,
	title text NOT NULL,
	description text,
	status text NOT NULL,
	priority text NOT NULL,
	priority_rank smallint NOT NULL,
	assignee_id uuid REFERENCES users,
	reporter_id uuid NOT NULL REFERENCES users,
	labels text[] NOT NULL,
	due_date text,
	version integer NOT NULL,
	comment_count integer NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL,
	updated_at timestamptz NOT NULL,
	resolved_at timestamptz,
	search_words text[] NOT NULL,
	UNIQUE (project_id, number)
);
CREATE INDEX IF NOT EXISTS issues_created ON issues (project_id, created_at);
CREATE INDEX IF NOT EXISTS issues_assignee ON issues (assignee_id);
CREATE INDEX IF NOT EXISTS issues_words ON issues USING gin (search_words);
CREATE INDEX IF NOT EXISTS issues_labels ON issues USING gin (labels);
CREATE TABLE IF NOT EXISTS comments (
	id uuid PRIMARY KEY,
	seq bigserial,
	issue_id uuid NOT NULL REFERENCES issues ON DELETE CASCADE,
	author_id uuid NOT NULL REFERENCES users,
	body text NOT NULL,
	created_at timestamptz NOT NULL,
	updated_at timestamptz NOT NULL,
	edited boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS comments_issue ON comments (issue_id, seq);
CREATE TABLE IF NOT EXISTS issue_history (
	id uuid PRIMARY KEY,
	seq bigserial,
	issue_id uuid NOT NULL REFERENCES issues ON DELETE CASCADE,
	actor_id uuid NOT NULL,
	created_at timestamptz NOT NULL,
	changes jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS issue_history_issue ON issue_history (issue_id, seq);
CREATE TABLE IF NOT EXISTS idempotency_keys (
	user_id uuid NOT NULL,
	key text NOT NULL,
	request_hash text NOT NULL,
	status integer,
	response bytea,
	created_at timestamptz NOT NULL,
	PRIMARY KEY (user_id, key)
);
CREATE TABLE IF NOT EXISTS webhooks (
	id uuid PRIMARY KEY,
	seq bigserial,
	org_id uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
	url text NOT NULL,
	events text[] NOT NULL,
	secret text NOT NULL,
	active boolean NOT NULL,
	created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS webhooks_org ON webhooks (org_id);
CREATE TABLE IF NOT EXISTS deliveries (
	id uuid PRIMARY KEY,
	seq bigserial,
	webhook_id uuid NOT NULL REFERENCES webhooks ON DELETE CASCADE,
	event text NOT NULL,
	issue_id uuid NOT NULL,
	body bytea NOT NULL,
	status text NOT NULL,
	attempts integer NOT NULL DEFAULT 0,
	last_status_code integer,
	next_attempt_at timestamptz NOT NULL,
	created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS deliveries_webhook ON deliveries (webhook_id, seq);
CREATE INDEX IF NOT EXISTS deliveries_pending ON deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS deliveries_queue ON deliveries (webhook_id, issue_id, seq) WHERE status = 'pending';
`

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(727274)"); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(727274)")
	_, err = conn.Exec(ctx, schema)
	return err
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Server) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
