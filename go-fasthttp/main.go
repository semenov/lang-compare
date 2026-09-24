package main

import (
	"context"
	"log"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valyala/fasthttp"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	poolSize, _ := strconv.Atoi(env("DB_POOL_SIZE", "20"))
	cfg, err := pgxpool.ParseConfig(env("DATABASE_URL", "postgres://app:app@localhost:15432/app"))
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = int32(poolSize)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	s := &Server{
		store:   &Store{db: pool},
		catalog: NewCatalog(env("CATALOG_URL", "http://localhost:9000")),
	}
	addr := ":" + env("PORT", "8080")
	log.Printf("listening on %s", addr)
	log.Fatal((&fasthttp.Server{Handler: s.Routes(), Name: "orders-api"}).ListenAndServe(addr))
}
