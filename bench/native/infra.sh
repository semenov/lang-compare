#!/bin/bash
# Native (no Docker) infra: a throwaway Postgres 17 cluster in /tmp/lc-pg on :15432
# (doesn't touch any Postgres already running on the machine) and the catalog mock on :9000.
# Usage: infra.sh up|down|reset-db
set -euo pipefail
cd "$(dirname "$0")/../.."
PGBIN=/opt/homebrew/opt/postgresql@17/bin
PGDATA=/tmp/lc-pg
PORT=15432
export LC_ALL=C

db_up() {
  rm -rf "$PGDATA"
  "$PGBIN/initdb" -D "$PGDATA" --locale=C -E UTF8 -U app --auth=trust >/dev/null
  cat infra/postgresql.conf >> "$PGDATA/postgresql.conf"
  printf "port = $PORT\nlisten_addresses = 'localhost'\n" >> "$PGDATA/postgresql.conf"
  "$PGBIN/pg_ctl" -D "$PGDATA" -l /tmp/lc-pg.log -w start >/dev/null
  "$PGBIN/createdb" -p $PORT -U app app
  "$PGBIN/psql" -q -p $PORT -U app -d app -f infra/schema.sql >/dev/null
}
db_down() { [ -d "$PGDATA" ] && "$PGBIN/pg_ctl" -D "$PGDATA" -m fast stop >/dev/null 2>&1 || true; }

case "${1:-up}" in
  up)
    db_down; db_up
    (cd catalog && [ -f go.mod ] || go mod init catalog >/dev/null 2>&1; go build -o /tmp/lc-catalog .)
    pkill -f /tmp/lc-catalog || true
    nohup /tmp/lc-catalog >/tmp/lc-catalog.log 2>&1 &
    sleep 0.5; curl -sf localhost:9000/products/SKU-1 >/dev/null && echo "infra up" ;;
  reset-db) db_down; db_up ;;
  down) db_down; pkill -f /tmp/lc-catalog || true; rm -rf "$PGDATA"; echo "infra down" ;;
esac
