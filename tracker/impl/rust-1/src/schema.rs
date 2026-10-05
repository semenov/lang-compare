use deadpool_postgres::Pool;

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS users (
    id uuid PRIMARY KEY,
    email text NOT NULL UNIQUE,
    name text NOT NULL,
    password_hash text NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS refresh_chains (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    revoked boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS refresh_tokens (
    hash bytea PRIMARY KEY,
    chain_id uuid NOT NULL,
    used boolean NOT NULL DEFAULT false,
    expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS orgs (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE,
    name text NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS org_members (
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id uuid NOT NULL,
    role smallint NOT NULL,
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS org_members_user ON org_members (user_id);
CREATE TABLE IF NOT EXISTS projects (
    id uuid PRIMARY KEY,
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    key text NOT NULL,
    name text NOT NULL,
    description text,
    visibility smallint NOT NULL,
    created_at timestamptz NOT NULL,
    issue_seq integer NOT NULL DEFAULT 0,
    issue_count integer NOT NULL DEFAULT 0,
    UNIQUE (org_id, key)
);
CREATE TABLE IF NOT EXISTS project_members (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id uuid NOT NULL,
    role smallint NOT NULL,
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS project_members_user ON project_members (user_id);
CREATE TABLE IF NOT EXISTS issues (
    id uuid PRIMARY KEY,
    org_id uuid NOT NULL,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    project_key text NOT NULL,
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
    words text[] NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS issues_key ON issues (org_id, project_key, number);
CREATE INDEX IF NOT EXISTS issues_project ON issues (project_id);
CREATE INDEX IF NOT EXISTS issues_created ON issues (org_id, created_at DESC, project_key, number);
CREATE INDEX IF NOT EXISTS issues_updated ON issues (org_id, updated_at DESC, project_key, number);
CREATE INDEX IF NOT EXISTS issues_prio_a ON issues (org_id, priority, project_key, number);
CREATE INDEX IF NOT EXISTS issues_prio_d ON issues (org_id, priority DESC, project_key, number);
CREATE INDEX IF NOT EXISTS issues_assignee ON issues (assignee_id) WHERE assignee_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS issues_words ON issues USING gin (words);
CREATE INDEX IF NOT EXISTS issues_labels ON issues USING gin (labels);
CREATE TABLE IF NOT EXISTS comments (
    id uuid PRIMARY KEY,
    seq bigserial,
    issue_id uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    author_id uuid NOT NULL,
    body text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    edited boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS comments_issue ON comments (issue_id, seq);
CREATE TABLE IF NOT EXISTS issue_history (
    seq bigserial,
    id uuid NOT NULL,
    issue_id uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    actor_id uuid NOT NULL,
    created_at timestamptz NOT NULL,
    changes text NOT NULL
);
CREATE INDEX IF NOT EXISTS issue_history_issue ON issue_history (issue_id, seq);
CREATE TABLE IF NOT EXISTS webhooks (
    id uuid PRIMARY KEY,
    org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    url text NOT NULL,
    events text[] NOT NULL,
    secret text NOT NULL,
    active boolean NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS webhooks_org ON webhooks (org_id, id);
CREATE TABLE IF NOT EXISTS deliveries (
    id uuid PRIMARY KEY,
    seq bigserial,
    webhook_id uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    issue_id uuid NOT NULL,
    event text NOT NULL,
    payload text NOT NULL,
    status smallint NOT NULL DEFAULT 0,
    attempts integer NOT NULL DEFAULT 0,
    last_status_code integer,
    created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS deliveries_hook ON deliveries (webhook_id, seq);
CREATE INDEX IF NOT EXISTS deliveries_pending ON deliveries (webhook_id, issue_id, seq) WHERE status = 0;
CREATE TABLE IF NOT EXISTS idempotency (
    user_id uuid NOT NULL,
    key text NOT NULL,
    req_hash bytea NOT NULL,
    status integer NOT NULL DEFAULT 0,
    body text,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, key)
);
"#;

pub async fn migrate(pool: &Pool) -> Result<(), String> {
    let c = pool.get().await.map_err(|e| e.to_string())?;
    // serialize concurrent migrations from several instances
    c.batch_execute(&format!("SELECT pg_advisory_lock(727274); {SCHEMA} SELECT pg_advisory_unlock(727274);"))
        .await
        .map_err(|e| e.to_string())
}
