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
// and marks resource rows as 'expired'.
func startReaper(db *sql.DB, rdb *redis.Client, cfg *Config, custDBURL string) {
	interval := cfg.ParsedReaperInterval()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			timeout := cfg.ParsedReaperTimeout()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			reapExpired(ctx, db, rdb, cfg, custDBURL)
			cancel()
		}
	}()
	slog.Info("reaper started", "interval", interval.String())
}

func reapExpired(ctx context.Context, db *sql.DB, rdb *redis.Client, cfg *Config, custDBURL string) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, token, resource_type FROM resources
		 WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at < NOW()
		 LIMIT $1`, cfg.Reaper.BatchSize)
	if err != nil {
		slog.Error("reaper: query failed", "error", err)
		return
	}
	defer rows.Close()

	var reaped int
	for rows.Next() {
		var id, token, resType string
		if err := rows.Scan(&id, &token, &resType); err != nil {
			slog.Error("reaper: scan failed", "error", err)
			continue
		}

		safe := sanitizeToken(token)

		switch resType {
		case "postgres":
			if err := dropPostgresDB(ctx, custDBURL, safe); err != nil {
				slog.Error("reaper: drop postgres failed", "token", token, "error", err)
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

		_, err := db.ExecContext(ctx, `UPDATE resources SET status = 'expired' WHERE id = $1`, id)
		if err != nil {
			slog.Error("reaper: mark expired failed", "id", id, "error", err)
			continue
		}
		reaped++
	}

	if reaped > 0 {
		slog.Info("reaper: cleaned up expired resources", "count", reaped)
	}
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
