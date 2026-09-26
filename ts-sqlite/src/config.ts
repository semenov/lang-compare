export const config = {
  port: Number(process.env.PORT ?? 8080),
  sqlitePath: process.env.SQLITE_PATH ?? "/tmp/lc-sqlite.db",
  sqliteInit: process.env.SQLITE_INIT ?? "infra/sqlite.sql",
  catalogUrl: process.env.CATALOG_URL ?? "http://localhost:9000",
  workers: Number(process.env.WORKERS ?? 1),
};
