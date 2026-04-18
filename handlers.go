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

func (s *server) handleNewDB(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fp := s.fingerprint(r)

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

	token := uuid.New()
	anonTTL := s.cfg.ParsedAnonTTL()
	expiresAt := time.Now().UTC().Add(anonTTL)

	connURL, err := provisionPostgres(ctx, s.custDBURL, token.String(), s.cfg)
	if err != nil {
		slog.ErrorContext(ctx, "db provision failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "provision_failed", "message": "Failed to provision Postgres database",
		})
		return
	}

	id := uuid.New()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO resources (id, token, resource_type, tier, fingerprint, connection_url, expires_at)
		 VALUES ($1, $2, 'postgres', 'anonymous', $3, $4, $5)`,
		id, token, fp, connURL, expiresAt)
	if err != nil {
		slog.ErrorContext(ctx, "db resource insert failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "internal_error", "message": "Failed to save resource",
		})
		return
	}

	slog.InfoContext(ctx, "provision.success", "service", "postgres", "token", token.String(), "fingerprint", fp)

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":             true,
		"id":             id.String(),
		"token":          token.String(),
		"connection_url": connURL,
		"tier":           "anonymous",
		"limits":         map[string]any{"storage_mb": s.cfg.Postgres.StorageMB, "connections": s.cfg.Postgres.ConnLimit, "expires_in": s.cfg.Limits.AnonTTL},
		"note":           fmt.Sprintf("Works now. Keep it forever (free 14-day trial): %s/start?token=%s", s.baseURL, token.String()),
	})
}

// ── POST /webhook/new ───────────────────────────────────────────────────────

func (s *server) handleNewWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fp := s.fingerprint(r)

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

	token := uuid.New()
	anonTTL := s.cfg.ParsedAnonTTL()
	expiresAt := time.Now().UTC().Add(anonTTL)
	receiveURL := fmt.Sprintf("%s/webhook/receive/%s", s.baseURL, token.String())

	id := uuid.New()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO resources (id, token, resource_type, tier, fingerprint, connection_url, expires_at)
		 VALUES ($1, $2, 'webhook', 'anonymous', $3, $4, $5)`,
		id, token, fp, receiveURL, expiresAt)
	if err != nil {
		slog.ErrorContext(ctx, "webhook resource insert failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "internal_error", "message": "Failed to save resource",
		})
		return
	}

	slog.InfoContext(ctx, "provision.success", "service", "webhook", "token", token.String(), "fingerprint", fp)

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":          true,
		"id":          id.String(),
		"token":       token.String(),
		"receive_url": receiveURL,
		"tier":        "anonymous",
		"expires_at":  expiresAt,
		"limits":      map[string]any{"requests_stored": s.cfg.Limits.WebhookMaxStored, "expires_in": s.cfg.Limits.AnonTTL},
		"note":        fmt.Sprintf("Works now. Keep it forever (free 14-day trial): %s/start", s.baseURL),
	})
}

// ── POST /webhook/receive/:token ────────────────────────────────────────────

func (s *server) handleWebhookReceive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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

	body, _ := io.ReadAll(io.LimitReader(r.Body, s.cfg.Limits.WebhookMaxBodyBytes))

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

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
