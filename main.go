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
	db        *sql.DB
	rdb       *redis.Client
	cfg       *Config
	baseURL   string
	custDBURL string // customer Postgres (where we CREATE DATABASE)
}

func main() {
	// CONFIG_PATH is the only environment variable used — everything else lives in config.yaml.
	configPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		configPath = v
	}
	cfg := LoadConfig(configPath)

	slog.Info("instant-lite config loaded", "summary", cfg.Summary())

	db, err := sql.Open("postgres", cfg.Database.PlatformURL)
	if err != nil {
		slog.Error("failed to connect to platform database", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		slog.Error("platform database unreachable", "error", err)
		os.Exit(1)
	}

	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		slog.Error("invalid redis url", "url", cfg.Redis.URL, "error", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Warn("redis unreachable — rate limiting and webhooks will be degraded", "error", err)
	}

	s := &server{
		db:        db,
		rdb:       rdb,
		cfg:       cfg,
		baseURL:   cfg.Server.BaseURL,
		custDBURL: cfg.Database.CustomerURL,
	}

	// Start the expired resource reaper.
	startReaper(db, rdb, cfg, cfg.Database.CustomerURL)

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

	limiter := newIPRateLimiter(cfg.Limits.RateRequestsPerSecond, cfg.Limits.RateBurst)
	handler := rateLimitMiddleware(limiter, withMiddleware(mux, cfg))

	srv := &http.Server{
		Addr:         ":" + cfg.Server.Port,
		Handler:      handler,
		ReadTimeout:  cfg.ParsedReadTimeout(),
		WriteTimeout: cfg.ParsedWriteTimeout(),
		IdleTimeout:  cfg.ParsedIdleTimeout(),
	}

	go func() {
		slog.Info("instant-lite starting", "port", cfg.Server.Port, "base_url", cfg.Server.BaseURL)
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

func withMiddleware(next http.Handler, cfg *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cap request body to prevent memory exhaustion.
		r.Body = http.MaxBytesReader(w, r.Body, cfg.Limits.MaxRequestBodyBytes)

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
