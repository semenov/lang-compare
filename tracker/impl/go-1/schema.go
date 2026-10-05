package main

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS users (
	id uuid PRIMARY KEY,
	email text COLLATE "C" NOT NULL UNIQUE,
	name text NOT NULL,
	pw text NOT NULL,
	created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS refresh_tokens (
	hash bytea PRIMARY KEY,
	chain uuid NOT NULL,
	user_id uuid NOT NULL,
	expires_at timestamptz NOT NULL,
	used boolean NOT NULL DEFAULT false,
	revoked boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS refresh_tokens_chain ON refresh_tokens (chain);
CREATE TABLE IF NOT EXISTS orgs (
	id uuid PRIMARY KEY,
	slug text COLLATE "C" NOT NULL UNIQUE,
	name text NOT NULL,
	created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS org_members (
	org_id uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
	user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
	role smallint NOT NULL,
	PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS org_members_user ON org_members (user_id);
CREATE TABLE IF NOT EXISTS projects (
	id uuid PRIMARY KEY,
	org_id uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
	key text COLLATE "C" NOT NULL,
	name text NOT NULL,
	description text,
	visibility smallint NOT NULL,
	created_at timestamptz NOT NULL,
	seq integer NOT NULL DEFAULT 0,
	UNIQUE (org_id, key)
);
CREATE TABLE IF NOT EXISTS project_members (
	project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
	user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
	role smallint NOT NULL,
	PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS project_members_user ON project_members (user_id);
CREATE TABLE IF NOT EXISTS issues (
	id uuid PRIMARY KEY,
	org_id uuid NOT NULL,
	project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
	project_key text COLLATE "C" NOT NULL,
	number integer NOT NULL,
	type smallint NOT NULL,
	title text NOT NULL,
	description text,
	status smallint NOT NULL,
	priority smallint NOT NULL,
	assignee_id uuid,
	reporter_id uuid NOT NULL,
	labels text[] NOT NULL,
	due_date date,
	version integer NOT NULL,
	comment_count integer NOT NULL,
	created_at timestamptz NOT NULL,
	updated_at timestamptz NOT NULL,
	resolved_at timestamptz,
	words text[] NOT NULL,
	UNIQUE (project_id, number)
);
CREATE INDEX IF NOT EXISTS issues_org_created ON issues (org_id, created_at);
CREATE INDEX IF NOT EXISTS issues_org_updated ON issues (org_id, updated_at);
CREATE INDEX IF NOT EXISTS issues_org_priority ON issues (org_id, priority, project_key, number);
CREATE INDEX IF NOT EXISTS issues_org_priority_desc ON issues (org_id, priority DESC, project_key, number);
CREATE INDEX IF NOT EXISTS issues_org_key ON issues (org_id, project_key, number);
CREATE INDEX IF NOT EXISTS issues_assignee ON issues (assignee_id) WHERE assignee_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS issues_reporter ON issues (reporter_id);
CREATE INDEX IF NOT EXISTS issues_words ON issues USING gin (words);
CREATE INDEX IF NOT EXISTS issues_labels ON issues USING gin (labels);
CREATE TABLE IF NOT EXISTS comments (
	id uuid PRIMARY KEY,
	issue_id uuid NOT NULL REFERENCES issues ON DELETE CASCADE,
	author_id uuid NOT NULL,
	body text NOT NULL,
	created_at timestamptz NOT NULL,
	updated_at timestamptz NOT NULL,
	edited boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS comments_issue ON comments (issue_id, id);
CREATE TABLE IF NOT EXISTS history (
	id uuid PRIMARY KEY,
	issue_id uuid NOT NULL REFERENCES issues ON DELETE CASCADE,
	actor_id uuid NOT NULL,
	created_at timestamptz NOT NULL,
	changes text NOT NULL
);
CREATE INDEX IF NOT EXISTS history_issue ON history (issue_id, id);
CREATE TABLE IF NOT EXISTS idempotency (
	user_id uuid NOT NULL,
	key text NOT NULL,
	hash bytea NOT NULL,
	status integer NOT NULL DEFAULT 0,
	body bytea,
	created_at timestamptz NOT NULL,
	PRIMARY KEY (user_id, key)
);
CREATE TABLE IF NOT EXISTS webhooks (
	id uuid PRIMARY KEY,
	org_id uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
	url text NOT NULL,
	events text[] NOT NULL,
	secret text NOT NULL,
	active boolean NOT NULL,
	created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS webhooks_org ON webhooks (org_id, id);
CREATE TABLE IF NOT EXISTS deliveries (
	id uuid PRIMARY KEY,
	webhook_id uuid NOT NULL REFERENCES webhooks ON DELETE CASCADE,
	issue_id uuid NOT NULL,
	event text NOT NULL,
	body bytea NOT NULL,
	status smallint NOT NULL DEFAULT 0,
	attempts integer NOT NULL DEFAULT 0,
	last_status_code integer,
	created_at timestamptz NOT NULL,
	next_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS deliveries_webhook ON deliveries (webhook_id, id);
CREATE INDEX IF NOT EXISTS deliveries_pending ON deliveries (id) WHERE status = 0;
`

func migrate(ctx context.Context) error {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// serialize concurrent starts against the same database
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(7428301)"); err != nil {
		return err
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock(7428301)")
	_, err = conn.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol)
	return err
}
