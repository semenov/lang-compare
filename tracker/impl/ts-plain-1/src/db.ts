import pg from "pg";

// Return DATE columns as plain 'YYYY-MM-DD' strings and bigints as numbers.
pg.types.setTypeParser(1082, (v: string) => v);
pg.types.setTypeParser(20, (v: string) => Number(v));

export type Client = pg.PoolClient | pg.Pool;

export let pool: pg.Pool;

export function initPool(url: string): void {
  pool = new pg.Pool({ connectionString: url, max: 30 });
  pool.on("error", () => {});
}

export async function tx<T>(fn: (c: pg.PoolClient) => Promise<T>): Promise<T> {
  const c = await pool.connect();
  try {
    await c.query("BEGIN");
    const r = await fn(c);
    await c.query("COMMIT");
    return r;
  } catch (e) {
    try {
      await c.query("ROLLBACK");
    } catch {}
    throw e;
  } finally {
    c.release();
  }
}

const SCHEMA = `
CREATE TABLE IF NOT EXISTS users (
  id uuid PRIMARY KEY,
  email text COLLATE "C" UNIQUE NOT NULL,
  name text NOT NULL,
  password_hash text NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS refresh_tokens (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  chain_id uuid NOT NULL,
  token_hash text UNIQUE NOT NULL,
  expires_at timestamptz NOT NULL,
  used_at timestamptz,
  revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS refresh_tokens_chain ON refresh_tokens (chain_id);
CREATE TABLE IF NOT EXISTS orgs (
  id uuid PRIMARY KEY,
  slug text COLLATE "C" UNIQUE NOT NULL,
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
  key text COLLATE "C" NOT NULL,
  name text NOT NULL,
  description text,
  visibility text NOT NULL,
  issue_counter int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL,
  UNIQUE (org_id, key)
);
CREATE TABLE IF NOT EXISTS project_members (
  project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  role text NOT NULL,
  PRIMARY KEY (project_id, user_id)
);
CREATE TABLE IF NOT EXISTS issues (
  id uuid PRIMARY KEY,
  project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
  org_id uuid NOT NULL,
  project_key text COLLATE "C" NOT NULL,
  number int NOT NULL,
  type text NOT NULL,
  title text NOT NULL,
  description text,
  status text NOT NULL,
  priority text NOT NULL,
  priority_rank int NOT NULL,
  assignee_id uuid,
  reporter_id uuid NOT NULL,
  labels text[] NOT NULL,
  due_date date,
  version int NOT NULL,
  comment_count int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  resolved_at timestamptz,
  words text[] NOT NULL,
  UNIQUE (project_id, number)
);
CREATE INDEX IF NOT EXISTS issues_org ON issues (org_id);
CREATE INDEX IF NOT EXISTS issues_words ON issues USING gin (words);
CREATE INDEX IF NOT EXISTS issues_labels ON issues USING gin (labels);
CREATE TABLE IF NOT EXISTS comments (
  seq bigserial,
  id uuid PRIMARY KEY,
  issue_id uuid NOT NULL REFERENCES issues ON DELETE CASCADE,
  author_id uuid NOT NULL,
  body text NOT NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  edited boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS comments_issue ON comments (issue_id, seq);
CREATE TABLE IF NOT EXISTS history (
  seq bigserial,
  id uuid PRIMARY KEY,
  issue_id uuid NOT NULL REFERENCES issues ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  created_at timestamptz NOT NULL,
  changes jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS history_issue ON history (issue_id, seq);
CREATE TABLE IF NOT EXISTS idempotency_keys (
  user_id uuid NOT NULL,
  key text NOT NULL,
  request_hash text NOT NULL,
  status int,
  body text,
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
CREATE TABLE IF NOT EXISTS deliveries (
  seq bigserial,
  id uuid PRIMARY KEY,
  webhook_id uuid NOT NULL REFERENCES webhooks ON DELETE CASCADE,
  group_key text NOT NULL,
  event text NOT NULL,
  payload text NOT NULL,
  status text NOT NULL,
  attempts int NOT NULL DEFAULT 0,
  last_status_code int,
  next_attempt_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS deliveries_pending ON deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS deliveries_group ON deliveries (webhook_id, group_key, seq) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS deliveries_webhook ON deliveries (webhook_id, seq);
`;

export async function migrate(): Promise<void> {
  const c = await pool.connect();
  try {
    await c.query("SELECT pg_advisory_lock(727274)");
    try {
      await c.query(SCHEMA);
    } finally {
      await c.query("SELECT pg_advisory_unlock(727274)");
    }
  } finally {
    c.release();
  }
}
