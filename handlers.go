package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── POST /db/new ────────────────────────────────────────────────────────────

// provisionRequest is the minimal JSON body accepted by every /{service}/new
// endpoint. Name is required — a human label the owner will see in the
// dashboard and in the upgrade URL.
type provisionRequest struct {
	Name string `json:"name"`
}

// parseProvisionRequest reads the JSON body (empty body tolerated) and
// validates the name. Returns a 400-ready error string when invalid.
func parseProvisionRequest(r *http.Request) (string, string) {
	var req provisionRequest
	if r.Body != nil {
		// Ignore decode errors on empty/invalid body — we'll error below
		// on the missing name.
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return "", "name is required: include {\"name\":\"<label>\"} in the JSON body"
	}
	if len(name) > 64 {
		return "", "name must be 64 characters or fewer"
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return "", "name must not contain control characters"
		}
	}
	return name, ""
}

func (s *server) handleNewDB(w http.ResponseWriter, r *http.Request) {
	// Bound every platform-PG / Redis call in this handler to 5s so a stuck
	// downstream (e.g. DO managed-PG firewall change) can't hang the App
	// Platform instance indefinitely.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	fp := s.fingerprint(r)

	name, errMsg := parseProvisionRequest(r)
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "name_required", "message": errMsg,
		})
		return
	}

	// Authenticated paid users skip the per-fingerprint anon cap and get
	// permanent resources linked to their account automatically. Accepts
	// session cookie (from the browser) or Authorization: Bearer <JWT>
	// (from CLI / agents).
	paidUser := s.authUser(r)
	isPaid := paidUser != nil && paidUser.PlanTier == "paid"

	if !isPaid {
		exceeded, existing := s.checkLimitAndIncrement(ctx, fp, "postgres")
		if exceeded {
			if existing != nil {
				writeJSON(w, http.StatusOK, map[string]any{
					"ok":             true,
					"id":             existing.id,
					"token":          existing.token,
					"connection_url": existing.connectionURL,
					"tier":           "anonymous",
					"limits":         map[string]any{"storage_mb": s.cfg.Postgres.StorageMB, "connections": s.cfg.Postgres.ConnLimit, "expires_in": s.cfg.Limits.AnonTTL},
					"note":           fmt.Sprintf("Returning your existing database. Keep it forever: %s/start?token=%s", s.baseURL, existing.token),
				})
			} else {
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"ok": false, "error": "rate_limited", "message": fmt.Sprintf("Daily provision limit reached (%d/day). Keep resources forever: %s/start", s.cfg.Limits.MaxProvisionsPerDay, s.baseURL),
				})
			}
			return
		}
	}

	token := uuid.New()
	anonTTL := s.cfg.ParsedAnonTTL()
	var expiresAt *time.Time
	tier := "anonymous"
	if isPaid {
		tier = "paid"
	} else {
		t := time.Now().UTC().Add(anonTTL)
		expiresAt = &t
	}

	// Customer-PG round trip (CREATE USER + CREATE DATABASE) is slower than
	// a platform query, so it gets its own 10s budget instead of inheriting
	// the 5s request-scoped ctx above.
	provCtx, provCancel := context.WithTimeout(r.Context(), 10*time.Second)
	connURL, err := provisionPostgres(provCtx, s.custDBURL, token.String(), s.cfg)
	provCancel()
	if err != nil {
		slog.ErrorContext(ctx, "db provision failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "provision_failed", "message": "Failed to provision Postgres database",
		})
		return
	}

	id := uuid.New()
	if isPaid {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO resources (id, token, resource_type, name, tier, fingerprint, connection_url, expires_at, migrated_to_user_id)
			 VALUES ($1, $2, 'postgres', $3, 'paid', $4, $5, NULL, $6)`,
			id, token, name, fp, connURL, paidUser.ID)
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO resources (id, token, resource_type, name, tier, fingerprint, connection_url, expires_at)
			 VALUES ($1, $2, 'postgres', $3, 'anonymous', $4, $5, $6)`,
			id, token, name, fp, connURL, expiresAt)
	}
	if err != nil {
		slog.ErrorContext(ctx, "db resource insert failed", "error", err)
		// Compensating cleanup: the tenant PG user + database were created by
		// provisionPostgres above, but the resources-table INSERT failed, so
		// nothing points at them and the reaper will never find them. Drop
		// them now. Use a fresh context.Background() with a 10s timeout —
		// the caller's request context may already be cancelled, but we
		// still need this rollback to run. Log both errors for observability.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if cleanupErr := dropPostgresDB(cleanupCtx, s.custDBURL, sanitizeToken(token.String())); cleanupErr != nil {
			slog.ErrorContext(ctx, "db provision rollback failed; orphaned tenant objects",
				"insert_error", err, "cleanup_error", cleanupErr, "token", token.String())
		} else {
			slog.WarnContext(ctx, "db provision rolled back after insert failure",
				"insert_error", err, "token", token.String())
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "internal_error", "message": "Failed to save resource",
		})
		return
	}

	slog.InfoContext(ctx, "provision.success", "service", "postgres", "token", token.String(), "fingerprint", fp, "tier", tier)

	resp := map[string]any{
		"ok":             true,
		"id":             id.String(),
		"token":          token.String(),
		"name":           name,
		"connection_url": connURL,
		"tier":           tier,
		"limits":         map[string]any{"storage_mb": s.cfg.Postgres.StorageMB, "connections": s.cfg.Postgres.ConnLimit},
	}
	if isPaid {
		resp["note"] = "Permanent database (Developer tier). Manage it at " + s.baseURL + "/dashboard.html"
	} else {
		resp["expires_at"] = expiresAt
		resp["limits"].(map[string]any)["expires_in"] = s.cfg.Limits.AnonTTL
		resp["note"] = fmt.Sprintf("Works now. Keep it forever (free 14-day trial): %s/start?token=%s", s.baseURL, token.String())
	}
	writeJSON(w, http.StatusCreated, resp)
}

// ── POST /webhook/new ───────────────────────────────────────────────────────

func (s *server) handleNewWebhook(w http.ResponseWriter, r *http.Request) {
	// Bound every platform-PG / Redis call in this handler to 5s so a stuck
	// downstream can't hang the App Platform instance indefinitely.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	fp := s.fingerprint(r)

	name, errMsg := parseProvisionRequest(r)
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "name_required", "message": errMsg,
		})
		return
	}

	paidUser := s.authUser(r)
	isPaid := paidUser != nil && paidUser.PlanTier == "paid"

	if !isPaid {
		exceeded, existing := s.checkLimitAndIncrement(ctx, fp, "webhook")
		if exceeded {
			if existing != nil {
				writeJSON(w, http.StatusOK, map[string]any{
					"ok":          true,
					"id":          existing.id,
					"token":       existing.token,
					"receive_url": existing.connectionURL,
					"tier":        "anonymous",
					"limits":      map[string]any{"requests_stored": s.cfg.Limits.WebhookMaxStored, "expires_in": s.cfg.Limits.AnonTTL},
					"note":        "Returning your existing webhook. Keep it forever: " + s.baseURL + "/start",
				})
			} else {
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"ok": false, "error": "rate_limited", "message": fmt.Sprintf("Daily provision limit reached (%d/day). Keep resources forever: %s/start", s.cfg.Limits.MaxProvisionsPerDay, s.baseURL),
				})
			}
			return
		}
	}

	token := uuid.New()
	anonTTL := s.cfg.ParsedAnonTTL()
	var expiresAt *time.Time
	tier := "anonymous"
	if isPaid {
		tier = "paid"
	} else {
		t := time.Now().UTC().Add(anonTTL)
		expiresAt = &t
	}
	// baseURL is the marketing host (https://instanode.dev) which serves
	// static pages on GitHub Pages — POST requests to it return 405. Emit
	// webhook receive URLs against the API host instead.
	receiveBase := s.cfg.Server.BaseURL
	if strings.HasPrefix(receiveBase, "https://instanode.dev") {
		receiveBase = "https://api.instanode.dev"
	} else if strings.HasPrefix(receiveBase, "http://instanode.dev") {
		receiveBase = "http://api.instanode.dev"
	}
	receiveURL := fmt.Sprintf("%s/webhook/receive/%s", receiveBase, token.String())

	id := uuid.New()
	var err error
	if isPaid {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO resources (id, token, resource_type, name, tier, fingerprint, connection_url, expires_at, migrated_to_user_id)
			 VALUES ($1, $2, 'webhook', $3, 'paid', $4, $5, NULL, $6)`,
			id, token, name, fp, receiveURL, paidUser.ID)
	} else {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO resources (id, token, resource_type, name, tier, fingerprint, connection_url, expires_at)
			 VALUES ($1, $2, 'webhook', $3, 'anonymous', $4, $5, $6)`,
			id, token, name, fp, receiveURL, expiresAt)
	}
	if err != nil {
		slog.ErrorContext(ctx, "webhook resource insert failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "internal_error", "message": "Failed to save resource",
		})
		return
	}

	slog.InfoContext(ctx, "provision.success", "service", "webhook", "token", token.String(), "fingerprint", fp, "tier", tier)

	resp := map[string]any{
		"ok":          true,
		"id":          id.String(),
		"token":       token.String(),
		"name":        name,
		"receive_url": receiveURL,
		"tier":        tier,
		"limits":      map[string]any{"requests_stored": s.cfg.Limits.WebhookMaxStored},
	}
	if isPaid {
		resp["note"] = "Permanent webhook (Developer tier). Manage it at " + s.baseURL + "/dashboard.html"
	} else {
		resp["expires_at"] = expiresAt
		resp["limits"].(map[string]any)["expires_in"] = s.cfg.Limits.AnonTTL
		resp["note"] = fmt.Sprintf("Works now. Keep it forever (free 14-day trial): %s/start?token=%s", s.baseURL, token.String())
	}
	writeJSON(w, http.StatusCreated, resp)
}

// ── POST /webhook/receive/:token ────────────────────────────────────────────

func (s *server) handleWebhookReceive(w http.ResponseWriter, r *http.Request) {
	// Bound platform-PG lookup + Redis pipeline to 5s.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tokenStr := r.PathValue("token")

	tokenUUID, err := uuid.Parse(tokenStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_token"})
		return
	}

	var status string
	err = s.db.QueryRowContext(ctx,
		`SELECT status FROM resources WHERE token = $1 AND resource_type = 'webhook'`, tokenUUID).Scan(&status)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not_found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "lookup_failed"})
		return
	}
	if status != "active" {
		writeJSON(w, http.StatusGone, map[string]any{"ok": false, "error": "webhook_inactive"})
		return
	}

	// Global middleware already caps bodies at MaxRequestBodyBytes; tighten further
	// for webhook receive if WebhookMaxBodyBytes is lower, otherwise inherit the cap.
	if s.cfg.Limits.WebhookMaxBodyBytes < s.cfg.Limits.MaxRequestBodyBytes {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Limits.WebhookMaxBodyBytes)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("webhook body exceeds %d bytes", s.cfg.Limits.WebhookMaxBodyBytes))
		return
	}

	headers := make(map[string]string)
	for k, v := range r.Header {
		headers[k] = strings.Join(v, ", ")
	}

	reqID := uuid.New().String()
	payload, _ := json.Marshal(map[string]any{
		"id":          reqID,
		"method":      r.Method,
		"headers":     headers,
		"body":        string(body),
		"received_at": time.Now().UTC().Format(time.RFC3339),
	})

	anonTTL := s.cfg.ParsedAnonTTL()
	listKey := "wh:list:" + tokenStr
	pipe := s.rdb.Pipeline()
	pipe.LPush(ctx, listKey, string(payload))
	pipe.LTrim(ctx, listKey, 0, s.cfg.Limits.WebhookMaxStored-1)
	pipe.Expire(ctx, listKey, anonTTL)
	if _, pipeErr := pipe.Exec(ctx); pipeErr != nil {
		slog.WarnContext(ctx, "webhook store failed (fail-open)", "error", pipeErr)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": reqID})
}

// ── Shared helpers ──────────────────────────────────────────────────────────

type existingResource struct {
	id            string
	token         string
	connectionURL string
	keyPrefix     string
}

// checkLimitAndIncrement atomically increments the provision counter and checks
// whether the limit is exceeded. Returns (exceeded, existingResource).
// If Redis is down, falls back to counting resources in Postgres.
func (s *server) checkLimitAndIncrement(ctx context.Context, fp, resourceType string) (bool, *existingResource) {
	date := time.Now().UTC().Format("2006-01-02")
	key := fmt.Sprintf("prov:%s:%s", fp, date)
	maxProvisions := int64(s.cfg.Limits.MaxProvisionsPerDay)

	// Atomic increment-then-check: no race window between read and write.
	newCount, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		// Redis down — fall back to Postgres count (hard cap, never fail-open).
		slog.WarnContext(ctx, "redis unavailable, falling back to postgres count", "error", err)
		return s.checkLimitPostgresFallback(ctx, fp)
	}

	// Set expiry on first increment only (idempotent via TTL check).
	if newCount == 1 {
		s.rdb.Expire(ctx, key, 25*time.Hour)
	}

	if newCount <= maxProvisions {
		return false, nil
	}

	// Limit exceeded — undo the increment so counter stays accurate.
	s.rdb.Decr(ctx, key)

	var res existingResource
	err = s.db.QueryRowContext(ctx,
		`SELECT id, token, connection_url, key_prefix FROM resources
		 WHERE fingerprint = $1 AND resource_type = $2 AND status = 'active'
		 ORDER BY created_at DESC LIMIT 1`, fp, resourceType).
		Scan(&res.id, &res.token, &res.connectionURL, &res.keyPrefix)
	if err != nil {
		return true, nil
	}
	return true, &res
}

// checkLimitPostgresFallback counts today's provisions in Postgres when Redis
// is unavailable. Never fail-open — always enforce the limit.
func (s *server) checkLimitPostgresFallback(ctx context.Context, fp string) (bool, *existingResource) {
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM resources
		 WHERE fingerprint = $1 AND created_at >= $2`, fp, todayStart).Scan(&count)
	if err != nil {
		slog.ErrorContext(ctx, "postgres fallback count failed — blocking provision", "error", err)
		return true, nil // fail closed: block if we can't count
	}
	return count >= s.cfg.Limits.MaxProvisionsPerDay, nil
}

func (s *server) fingerprint(r *http.Request) string {
	ip := clientIP(r)
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(ip)))[:16]
	}
	var subnet string
	if parsed.To4() != nil {
		mask := net.CIDRMask(s.cfg.Limits.IPv4CIDRPrefix, 32)
		subnet = fmt.Sprintf("%s/%d", parsed.Mask(mask).String(), s.cfg.Limits.IPv4CIDRPrefix)
	} else {
		mask := net.CIDRMask(s.cfg.Limits.IPv6CIDRPrefix, 128)
		subnet = fmt.Sprintf("%s/%d", parsed.Mask(mask).String(), s.cfg.Limits.IPv6CIDRPrefix)
	}
	h := sha256.Sum256([]byte(subnet))
	return fmt.Sprintf("%x", h)[:16]
}

// clientIP resolves the real client IP from the reverse-proxy chain.
// Preference order:
//  1. CF-Connecting-IP (CloudFlare sets this to the original client)
//  2. True-Client-IP (CloudFlare enterprise / some CDNs)
//  3. X-Forwarded-For first element (RFC 7239 — client is first, proxies append)
//  4. X-Real-IP
//  5. RemoteAddr
//
// Previously we took the LAST element of XFF, which returned a different DO
// edge IP per request and broke per-subnet rate limiting entirely.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("True-Client-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError emits the standard JSON error shape. `code` is a stable
// machine-readable identifier (snake_case); `message` is a short, SAFE
// human-readable string — never embed `err.Error()` or internal detail.
// Log the real error with slog before calling this.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"ok":      false,
		"error":   code,
		"message": message,
	})
}
