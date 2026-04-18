package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// startReaper launches a background goroutine that periodically cleans up
// expired resources: drops Postgres databases, deletes Redis ACL users,
// and marks resource rows as 'expired'. It also enforces storage quotas
// on active Postgres resources (Postgres has no native per-DB disk quota,
// so we scan pg_database_size() periodically and lock over-limit DBs).
func startReaper(db *sql.DB, rdb *redis.Client, cfg *Config, custDBURL string) {
	interval := cfg.ParsedReaperInterval()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			timeout := cfg.ParsedReaperTimeout()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			reapExpired(ctx, db, rdb, cfg, custDBURL)
			enforceStorageQuota(ctx, db, cfg, custDBURL)
			cancel()
		}
	}()
	slog.Info("reaper started", "interval", interval.String())
}

func reapExpired(ctx context.Context, db *sql.DB, rdb *redis.Client, cfg *Config, custDBURL string) {
	// Two sources of reapable rows:
	//   (a) TTL expired      — status='active', expires_at < NOW()
	//   (b) User soft-deleted — status='deleted' (no TTL guard)
	// Both drop the underlying DB and transition the row to a terminal state
	// (expired or reaped respectively) so the next tick skips them.
	rows, err := db.QueryContext(ctx,
		`SELECT id, token, resource_type, status FROM resources
		 WHERE (status = 'active' AND expires_at IS NOT NULL AND expires_at < NOW())
		    OR status = 'deleted'
		 LIMIT $1`, cfg.Reaper.BatchSize)
	if err != nil {
		slog.Error("reaper: query failed", "error", err)
		return
	}
	defer rows.Close()

	var reapedExpired, reapedDeleted int
	for rows.Next() {
		var id, token, resType, status string
		if err := rows.Scan(&id, &token, &resType, &status); err != nil {
			slog.Error("reaper: scan failed", "error", err)
			continue
		}

		safe := sanitizeToken(token)

		switch resType {
		case "postgres":
			if err := dropPostgresDB(ctx, custDBURL, safe); err != nil {
				slog.Error("reaper: drop postgres failed", "token", token, "status", status, "error", err)
				continue
			}
		case "redis":
			userName := "usr_" + safe
			if err := rdb.Do(ctx, "ACL", "DELUSER", userName).Err(); err != nil {
				slog.Warn("reaper: delete redis acl failed (may not exist)", "user", userName, "error", err)
			}
		case "webhook":
			listKey := "wh:list:" + token
			rdb.Del(ctx, listKey)
		}

		// Transition to a terminal state so the next tick skips this row.
		// We keep the row around for audit / dashboard history queries; a
		// separate purge policy can prune very old ones later.
		newStatus := "expired"
		if status == "deleted" {
			newStatus = "reaped"
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE resources SET status = $1 WHERE id = $2`, newStatus, id,
		); err != nil {
			slog.Error("reaper: mark terminal failed", "id", id, "new_status", newStatus, "error", err)
			continue
		}
		if status == "deleted" {
			reapedDeleted++
		} else {
			reapedExpired++
		}
	}

	if reapedExpired > 0 || reapedDeleted > 0 {
		slog.Info("reaper: cleaned up resources",
			"expired", reapedExpired, "user_deleted", reapedDeleted)
	}
}

// enforceStorageQuota scans active Postgres resources and locks any whose
// on-disk size exceeds the tier's storage_mb cap. Locking = REVOKE CONNECT
// + pg_terminate_backend + mark status='quota_exceeded'. The DB is not
// dropped (data preserved so the user can upgrade + keep it); subsequent
// connection attempts return a permission error.
//
// Lag window: up to reaper.Interval of overage. Acceptable for Phase 0.
// For stronger enforcement, reduce reaper.Interval or move to per-tenant
// disk isolation (LVM) in a later phase.
func enforceStorageQuota(ctx context.Context, db *sql.DB, cfg *Config, custDBURL string) {
	limitMB := cfg.Postgres.StorageMB
	if limitMB <= 0 {
		return
	}
	limitBytes := int64(limitMB) * 1024 * 1024

	rows, err := db.QueryContext(ctx,
		`SELECT id, token FROM resources
		 WHERE status = 'active' AND resource_type = 'postgres'`)
	if err != nil {
		slog.Error("reaper: quota scan query failed", "error", err)
		return
	}
	defer rows.Close()

	type target struct {
		id, token string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.token); err != nil {
			continue
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		return
	}

	custConn, err := sql.Open("postgres", custDBURL)
	if err != nil {
		slog.Error("reaper: quota customer-pg connect failed", "error", err)
		return
	}
	defer custConn.Close()

	var locked int
	for _, t := range targets {
		safe := sanitizeToken(t.token)
		dbName := "db_" + safe
		var sizeBytes int64
		err := custConn.QueryRowContext(ctx, `SELECT pg_database_size($1)`, dbName).Scan(&sizeBytes)
		if err != nil {
			// Database may have been dropped between SELECT and size query — skip.
			continue
		}
		if sizeBytes <= limitBytes {
			continue
		}

		if err := lockOverQuotaDB(ctx, custConn, safe); err != nil {
			slog.Error("reaper: lock over-quota db failed", "token", t.token, "error", err)
			continue
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE resources SET status = 'quota_exceeded' WHERE id = $1 AND status = 'active'`, t.id); err != nil {
			slog.Error("reaper: mark quota_exceeded failed", "id", t.id, "error", err)
			continue
		}
		slog.Warn("reaper: locked over-quota db",
			"token", t.token, "size_bytes", sizeBytes, "limit_bytes", limitBytes)
		locked++
	}

	if locked > 0 {
		slog.Info("reaper: locked over-quota databases", "count", locked)
	}
}

// lockOverQuotaDB revokes CONNECT and terminates active sessions for the
// owning user. Data is left intact — dropping only happens on TTL expiry
// via the normal reap path.
func lockOverQuotaDB(ctx context.Context, conn *sql.DB, safe string) error {
	dbName := "db_" + safe
	userName := "usr_" + safe
	stmts := []string{
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM %s`, dbName, userName),
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM PUBLIC`, dbName),
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(err.Error(), "does not exist") {
				continue
			}
			return fmt.Errorf("exec %q: %w", stmt, err)
		}
	}
	// Kill live sessions so the lock takes effect immediately.
	if _, err := conn.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE usename = $1 AND pid <> pg_backend_pid()`, userName); err != nil {
		slog.Warn("reaper: terminate backends failed (advisory)", "user", userName, "error", err)
	}
	return nil
}

func dropPostgresDB(ctx context.Context, custDBURL, safe string) error {
	dbName := "db_" + safe
	userName := "usr_" + safe

	conn, err := sql.Open("postgres", custDBURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	stmts := []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName),
		fmt.Sprintf(`DROP USER IF EXISTS %s`, userName),
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(err.Error(), "does not exist") {
				continue
			}
			return fmt.Errorf("exec %q: %w", stmt, err)
		}
	}
	return nil
}
