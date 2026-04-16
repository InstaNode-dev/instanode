package main

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

//go:embed llms.txt
var llmsTxt []byte

type server struct {
	db       *sql.DB
	rdb      *redis.Client
	baseURL  string
	custDBURL string // customer Postgres (where we CREATE DATABASE)
}

func main() {
	port := env("PORT", "8080")
	baseURL := env("BASE_URL", "http://localhost:"+port)
	platformDBURL := env("DATABASE_URL", "postgres://instant:instant@localhost:5432/instant_lite?sslmode=disable")
	custDBURL := env("CUSTOMER_DATABASE_URL", platformDBURL)
	redisURL := env("REDIS_URL", "redis://localhost:6379")

	db, err := sql.Open("postgres", platformDBURL)
	if err != nil {
		slog.Error("failed to connect to platform database", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		slog.Error("platform database unreachable", "error", err)
		os.Exit(1)
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		slog.Error("invalid REDIS_URL", "error", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Warn("redis unreachable — rate limiting and webhooks will be degraded", "error", err)
	}

	s := &server{db: db, rdb: rdb, baseURL: baseURL, custDBURL: custDBURL}

	// Start the expired resource reaper (runs every 5 minutes).
	startReaper(db, rdb, custDBURL, 5*time.Minute)

	mux := http.NewServeMux()

	// Provisioning endpoints
	mux.HandleFunc("POST /db/new", s.handleNewDB)
	mux.HandleFunc("POST /cache/new", s.handleNewCache)
	mux.HandleFunc("POST /webhook/new", s.handleNewWebhook)
	mux.HandleFunc("POST /webhook/receive/{token}", s.handleWebhookReceive)
	mux.HandleFunc("GET /webhook/receive/{token}", s.handleWebhookReceive)

	// Health
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "instant-lite"})
	})

	// Machine-readable docs
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(llmsTxt)
	})

	// Root — redirect to website (hosted separately on GitHub Pages).
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "https://instant.dev", http.StatusFound)
	})

	// 10 req/s per IP with burst of 20 — prevents HTTP-level flooding.
	limiter := newIPRateLimiter(10, 20)
	handler := rateLimitMiddleware(limiter, withMiddleware(mux))

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("instant-lite starting", "port", port, "base_url", baseURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	db.Close()
	rdb.Close()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const maxRequestBodyBytes = 1 << 20 // 1 MB — applies to all endpoints

func withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cap request body to prevent memory exhaustion.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

		w.Header().Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
