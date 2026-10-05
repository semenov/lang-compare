package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	pool         *pgxpool.Pool
	jwtSecret    []byte
	backoffScale float64
	wake         chan struct{}
	client       *http.Client
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dbURL := os.Getenv("DATABASE_URL")
	secret := os.Getenv("JWT_SECRET")
	if dbURL == "" || secret == "" {
		log.Fatal("DATABASE_URL and JWT_SECRET are required")
	}
	scale := 1.0
	if v := os.Getenv("WEBHOOK_BACKOFF_SCALE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			log.Fatalf("invalid WEBHOOK_BACKOFF_SCALE %q", v)
		}
		scale = f
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		log.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 40
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	if err := migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	s := &Server{
		pool:         pool,
		jwtSecret:    []byte(secret),
		backoffScale: scale,
		wake:         make(chan struct{}, 1),
		client: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	srv := &http.Server{Addr: "0.0.0.0:" + port, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}

	dctx, dcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runDispatcher(dctx)
	}()

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Printf("listening on :%s", port)

	select {
	case <-sigCtx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
			dcancel()
			wg.Wait()
			os.Exit(1)
		}
	}
	log.Printf("shutting down")
	shCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	dcancel()
	wg.Wait()
	pool.Close()
}

// kick wakes the webhook dispatcher.
func (s *Server) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.pool.Ping(ctx); err != nil {
			writeProblem(w, newErr(503, "unavailable", "database unreachable"))
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, notFound())
	})

	const p = "/api/v1"
	s.handle(mux, "POST "+p+"/auth/register", false, s.register)
	s.handle(mux, "POST "+p+"/auth/login", false, s.login)
	s.handle(mux, "POST "+p+"/auth/refresh", false, s.refresh)
	s.handle(mux, "POST "+p+"/auth/logout", false, s.logout)
	s.handle(mux, "GET "+p+"/me", true, s.getMe)
	s.handle(mux, "PATCH "+p+"/me", true, s.patchMe)

	s.handle(mux, "POST "+p+"/orgs", true, s.createOrg)
	s.handle(mux, "GET "+p+"/orgs", true, s.listOrgs)
	s.handle(mux, "GET "+p+"/orgs/{slug}", true, s.getOrg)
	s.handle(mux, "GET "+p+"/orgs/{slug}/members", true, s.listOrgMembers)
	s.handle(mux, "POST "+p+"/orgs/{slug}/members", true, s.addOrgMember)
	s.handle(mux, "PATCH "+p+"/orgs/{slug}/members/{user_id}", true, s.patchOrgMember)
	s.handle(mux, "DELETE "+p+"/orgs/{slug}/members/{user_id}", true, s.deleteOrgMember)

	s.handle(mux, "POST "+p+"/orgs/{slug}/projects", true, s.createProject)
	s.handle(mux, "GET "+p+"/orgs/{slug}/projects", true, s.listProjects)
	s.handle(mux, "GET "+p+"/orgs/{slug}/projects/{key}", true, s.getProject)
	s.handle(mux, "PATCH "+p+"/orgs/{slug}/projects/{key}", true, s.patchProject)
	s.handle(mux, "DELETE "+p+"/orgs/{slug}/projects/{key}", true, s.deleteProject)
	s.handle(mux, "GET "+p+"/orgs/{slug}/projects/{key}/members", true, s.listProjectMembers)
	s.handle(mux, "PUT "+p+"/orgs/{slug}/projects/{key}/members/{user_id}", true, s.putProjectMember)
	s.handle(mux, "DELETE "+p+"/orgs/{slug}/projects/{key}/members/{user_id}", true, s.deleteProjectMember)

	s.handle(mux, "POST "+p+"/orgs/{slug}/projects/{key}/issues", true, s.createIssue)
	s.handle(mux, "POST "+p+"/orgs/{slug}/projects/{key}/issues/bulk", true, s.bulkCreateIssues)
	s.handle(mux, "GET "+p+"/orgs/{slug}/issues", true, s.searchIssues)
	s.handle(mux, "GET "+p+"/orgs/{slug}/issues/{ikey}", true, s.getIssue)
	s.handle(mux, "PATCH "+p+"/orgs/{slug}/issues/{ikey}", true, s.patchIssue)
	s.handle(mux, "DELETE "+p+"/orgs/{slug}/issues/{ikey}", true, s.deleteIssue)
	s.handle(mux, "POST "+p+"/orgs/{slug}/issues/{ikey}/transition", true, s.transitionIssue)
	s.handle(mux, "GET "+p+"/orgs/{slug}/issues/{ikey}/history", true, s.listHistory)
	s.handle(mux, "POST "+p+"/orgs/{slug}/issues/{ikey}/comments", true, s.createComment)
	s.handle(mux, "GET "+p+"/orgs/{slug}/issues/{ikey}/comments", true, s.listComments)
	s.handle(mux, "PATCH "+p+"/orgs/{slug}/comments/{id}", true, s.patchComment)
	s.handle(mux, "DELETE "+p+"/orgs/{slug}/comments/{id}", true, s.deleteComment)

	s.handle(mux, "POST "+p+"/orgs/{slug}/webhooks", true, s.createWebhook)
	s.handle(mux, "GET "+p+"/orgs/{slug}/webhooks", true, s.listWebhooks)
	s.handle(mux, "GET "+p+"/orgs/{slug}/webhooks/{id}", true, s.getWebhook)
	s.handle(mux, "PATCH "+p+"/orgs/{slug}/webhooks/{id}", true, s.patchWebhook)
	s.handle(mux, "DELETE "+p+"/orgs/{slug}/webhooks/{id}", true, s.deleteWebhook)
	s.handle(mux, "GET "+p+"/orgs/{slug}/webhooks/{id}/deliveries", true, s.listDeliveries)
	return mux
}
