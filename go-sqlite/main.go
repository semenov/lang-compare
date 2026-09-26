package main

import (
	"database/sql"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func open(path string, maxConns int, extra string) *sql.DB {
	dsn := "file:" + path + "?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000" + extra
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)
	return db
}

func main() {
	path := env("SQLITE_PATH", "/tmp/lc-sqlite.db")
	_, statErr := os.Stat(path)

	// SQLite allows one writer at a time: a single write connection (transactions start with
	// BEGIN IMMEDIATE) plus a pool of read connections, which WAL lets run concurrently.
	writer := open(path, 1, "&_txlock=immediate")
	readers, _ := strconv.Atoi(env("DB_POOL_SIZE", strconv.Itoa(2*runtime.GOMAXPROCS(0))))
	reader := open(path, readers, "&mode=ro")

	if os.IsNotExist(statErr) {
		script, err := os.ReadFile(env("SQLITE_INIT", "infra/sqlite.sql"))
		if err != nil {
			log.Fatal(err)
		}
		if _, err := writer.Exec(string(script)); err != nil {
			log.Fatal(err)
		}
	}

	s := &Server{
		store:   &Store{w: writer, r: reader},
		catalog: NewCatalog(env("CATALOG_URL", "http://localhost:9000")),
	}
	addr := ":" + env("PORT", "8080")
	srv := &http.Server{Addr: addr, Handler: s.Routes(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
