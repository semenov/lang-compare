export const config = {
  port: Number(process.env.PORT ?? 8080),
  databaseUrl: process.env.DATABASE_URL ?? "postgres://app:app@localhost:5432/app",
  catalogUrl: process.env.CATALOG_URL ?? "http://localhost:9000",
  dbPoolSize: Number(process.env.DB_POOL_SIZE ?? 20),
  workers: Number(process.env.WORKERS ?? 1),
};
