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
	"runtime/debug"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

//go:embed llms.txt
var llmsTxt []byte

//go:embed schema.sql
var schemaSQL string

type server struct {
	db        *sql.DB
	rdb       *redis.Client // Valkey (rate limits, webhook storage, and where
	                        // per-tenant ACL users are provisioned)
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

	// Initialize OpenTelemetry (vendor-agnostic — works with New Relic, Datadog, Grafana, etc.)
	shutdownOtel := initObservability(cfg)

	db, err := sql.Open("postgres", cfg.Database.PlatformURL)
	if err != nil {
		slog.Error("failed to connect to platform database", "error", err)
		fmt.Fprintf(os.Stderr, "FATAL: failed to open platform db: %v\n", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		slog.Error("platform database unreachable", "error", err)
		fmt.Fprintf(os.Stderr, "FATAL: platform database unreachable: %v\n", err)
		time.Sleep(2 * time.Second)
		os.Exit(1)
	}

	// Make sure schemas are fully instantiated natively inside App Platform
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		slog.Error("platform database schema init failed", "error", err)
		fmt.Fprintf(os.Stderr, "FATAL: schema init failed: %v\n", err)
		time.Sleep(2 * time.Second)
		os.Exit(1)
	}

	var opts *redis.Options
	if cfg.Redis.URL != "" {
		var err error
		opts, err = redis.ParseURL(cfg.Redis.URL)
		if err != nil {
			slog.Error("invalid redis url", "url", cfg.Redis.URL, "error", err)
			fmt.Fprintf(os.Stderr, "FATAL: invalid redis url: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Mock options if we are intentionally skipping Redis
		opts = &redis.Options{Addr: "localhost:0"}
	}
	rdb := redis.NewClient(opts)
	ctxR, cancelR := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelR()
	if err := rdb.Ping(ctxR).Err(); err != nil {
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
	mux.HandleFunc("POST /webhook/new", s.handleNewWebhook)
	mux.HandleFunc("POST /webhook/receive/{token}", s.handleWebhookReceive)
	mux.HandleFunc("GET /webhook/receive/{token}", s.handleWebhookReceive)

	// Auth endpoints
	mux.HandleFunc("GET /auth/github/login", s.handleGitHubLogin)
	mux.HandleFunc("GET /auth/github/callback", s.handleGitHubCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/me", s.handleMe)

	// Billing endpoints
	mux.HandleFunc("POST /billing/create-order", s.handleCreateOrder)
	mux.HandleFunc("POST /webhooks/razorpay", s.handleRazorpayWebhook)
	mux.HandleFunc("POST /billing/migrate", s.handleMigrateResource)

	// Dashboard endpoints
	mux.HandleFunc("GET /api/me/resources", s.handleGetResources)
	mux.HandleFunc("POST /api/me/claim", s.handleClaimToken)
	mux.HandleFunc("GET /api/me/token", s.handleGetAPIToken)
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://instanode.dev/dashboard", http.StatusFound)
	})
	mux.HandleFunc("GET /pricing", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://instanode.dev/pricing", http.StatusFound)
	})

	// Health
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "instant-lite"})
	})

	// Machine-readable docs
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(llmsTxt)
	})
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)

	// Root — redirect to website (hosted separately on GitHub Pages).
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "https://instanode.dev", http.StatusFound)
			return
		}
		if r.URL.Path == "/start" {
			// Serve the start page or redirect to frontend
			http.Redirect(w, r, "https://instanode.dev/start" + r.URL.RawQuery, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	limiter := newIPRateLimiter(cfg.Limits.RateRequestsPerSecond, cfg.Limits.RateBurst)
	handler := rateLimitMiddleware(limiter, withMiddleware(panicRecoveryMiddleware(s.traceEnrichmentMiddleware(otelhttp.NewHandler(mux, "instant-lite"))), cfg))

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
			fmt.Fprintf(os.Stderr, "FATAL: server failed: %v\n", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownOtel(ctx)
	srv.Shutdown(ctx)
	db.Close()
	rdb.Close()
}

func withMiddleware(next http.Handler, cfg *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cap request body to prevent memory exhaustion.
		r.Body = http.MaxBytesReader(w, r.Body, cfg.Limits.MaxRequestBodyBytes)

		w.Header().Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))
		// Browsers reject Access-Control-Allow-Credentials: true together with
		// a wildcard origin. Echo the request Origin when it's one of ours,
		// otherwise fall back to wildcard for non-browser (curl/SDK) clients.
		origin := r.Header.Get("Origin")
		switch origin {
		case "https://instanode.dev", "http://localhost:5173", "http://localhost:3000":
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		case "":
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-Requested-With")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// panicRecoveryMiddleware catches panics, logs the stack trace to New Relic with the Trace ID, and returns a 500.
func panicRecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// We use ErrorContext specifically so otelslog can automatically grab the 
				// Trace ID / Span ID strictly injected by the otelhttp middleware.
				slog.ErrorContext(r.Context(), "FATAL PANIC", "error", err, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "internal_server_error", "message": "An unexpected error occurred."})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// traceEnrichmentMiddleware injects the user's fingerprint (User ID) directly into the APM Trace Span
func (s *server) traceEnrichmentMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp := s.fingerprint(r)
		
		// Grab the OpenTelemetry span from the request context
		span := trace.SpanFromContext(r.Context())
		if span.SpanContext().IsValid() {
			span.SetAttributes(attribute.String("user.id", fp))
		}
		
		next.ServeHTTP(w, r)
	})
}
