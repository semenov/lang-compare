package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	db        *pgxpool.Pool
	jwtSecret []byte
	disp      *dispatcher
)

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().UTC().Format("15:04:05.000 ")+format+"\n", a...)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logf("DATABASE_URL is required")
		os.Exit(1)
	}
	sec := os.Getenv("JWT_SECRET")
	if sec == "" {
		logf("JWT_SECRET is required")
		os.Exit(1)
	}
	jwtSecret = []byte(sec)
	scale := 1.0
	if s := os.Getenv("WEBHOOK_BACKOFF_SCALE"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 {
			scale = f
		}
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		logf("bad DATABASE_URL: %v", err)
		os.Exit(1)
	}
	maxConns := int32(runtime.GOMAXPROCS(0) * 4)
	if maxConns < 16 {
		maxConns = 16
	}
	if s := os.Getenv("DB_MAX_CONNS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxConns = int32(n)
		}
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement

	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for {
		db, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			err = db.Ping(ctx)
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logf("cannot connect to database: %v", err)
			os.Exit(1)
		}
		if db != nil {
			db.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := migrate(ctx); err != nil {
		logf("migration failed: %v", err)
		os.Exit(1)
	}
	initArgon()

	dctx, dcancel := context.WithCancel(context.Background())
	disp = newDispatcher(dctx, scale)
	if err := disp.loadPending(); err != nil {
		logf("loading pending deliveries: %v", err)
	}

	srv := &http.Server{
		Handler:           routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		logf("listen: %v", err)
		os.Exit(1)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logf("serve: %v", err)
			os.Exit(1)
		}
	}()
	logf("listening on :%s", port)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	srv.Shutdown(sctx)
	cancel()
	dcancel()
	disp.wait(2 * time.Second)
	db.Close()
	os.Exit(0)
}
