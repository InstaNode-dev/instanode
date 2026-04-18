package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
)

func provisionPostgres(ctx context.Context, custDBURL, token string, cfg *Config) (string, error) {
	safe := sanitizeToken(token)
	dbName := "db_" + safe
	userName := "usr_" + safe
	password := randomPassword(24)

	conn, err := sql.Open("postgres", custDBURL)
	if err != nil {
		return "", fmt.Errorf("connect to customer postgres: %w", err)
	}
	defer conn.Close()

	// Use parameterized-safe identifiers. Postgres DDL doesn't support $1 for
	// identifiers, but our dbName/userName are derived from UUID hex (alphanumeric
	// + underscore only). The password is hex-encoded, so no quote injection.
	// We still double any single quotes in the password as defense-in-depth.
	safePassword := strings.ReplaceAll(password, "'", "''")

	connLimit := cfg.Postgres.ConnLimit
	stmts := []string{
		fmt.Sprintf(`CREATE USER %s WITH PASSWORD '%s' CONNECTION LIMIT %d`, userName, safePassword, connLimit),
		// PG15+ requires CREATE DATABASE creator to be a member of the target
		// OWNER role. provisioner_admin has CREATEDB + CREATEROLE but isn't a
		// superuser, so we grant membership in the per-tenant role to ourselves.
		// We deliberately keep this membership so the reaper / quota-lock paths
		// can later act on the tenant DB as owner (DROP / REVOKE CONNECT).
		fmt.Sprintf(`GRANT %s TO CURRENT_USER`, userName),
		fmt.Sprintf(`CREATE DATABASE %s OWNER %s CONNECTION LIMIT %d`, dbName, userName, connLimit),
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				continue
			}
			return "", fmt.Errorf("exec %q: %w", stmt[:min(len(stmt), 40)], err)
		}
	}

	// Enable pgvector and set storage quota on the new database.
	newDBURL := replaceDBName(custDBURL, dbName)
	newConn, err := sql.Open("postgres", newDBURL)
	if err == nil {
		newConn.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS vector")

		// Revoke ability to create new schemas (limits attack surface).
		newConn.ExecContext(ctx, fmt.Sprintf("REVOKE CREATE ON DATABASE %s FROM PUBLIC", dbName))

		// Set statement timeout to prevent long-running queries from hogging resources.
		newConn.ExecContext(ctx, fmt.Sprintf("ALTER ROLE %s SET statement_timeout = '%s'", userName, cfg.Postgres.StatementTimeout))

		// Set a tablespace quota isn't natively supported in Postgres, but we can
		// enforce it by revoking temporary table creation and setting a trigger-based
		// or periodic check. For Phase 0, we rely on the periodic reaper + monitoring.
		// However, we CAN set a hard limit via ALTER DATABASE ... SET temp_file_limit.
		newConn.ExecContext(ctx, fmt.Sprintf("ALTER DATABASE %s SET temp_file_limit = '%dMB'", dbName, cfg.Postgres.StorageMB*2))

		newConn.Close()
	}

	connURL := buildConnURL(custDBURL, dbName, userName, password, cfg)
	return connURL, nil
}

func sanitizeToken(token string) string {
	clean := strings.ReplaceAll(token, "-", "")
	if len(clean) > 12 {
		clean = clean[:12]
	}
	// Defense-in-depth: strip anything that isn't alphanumeric.
	var safe strings.Builder
	for _, c := range clean {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			safe.WriteRune(c)
		}
	}
	return safe.String()
}

func randomPassword(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	return hex.EncodeToString(b)[:length]
}

// buildConnURL produces the connection string handed to the customer.
// The host/port are taken from cfg.Postgres.PublicHost/PublicPort when set
// — the customer never sees the raw customer-PG IP. sslmode defaults to
// require (override with cfg.Postgres.RequireTLS=false for local dev).
func buildConnURL(baseURL, dbName, user, password string, cfg *Config) string {
	// Fallback host parsed from baseURL (dev mode only).
	fallbackHost := "localhost:5432"
	baseQuery := ""
	if parts := strings.SplitN(baseURL, "@", 2); len(parts) == 2 {
		hostAndDB := parts[1]
		if slashIdx := strings.Index(hostAndDB, "/"); slashIdx > 0 {
			fallbackHost = hostAndDB[:slashIdx]
			remainder := hostAndDB[slashIdx+1:]
			if qIdx := strings.Index(remainder, "?"); qIdx >= 0 {
				baseQuery = remainder[qIdx+1:]
			}
		} else {
			fallbackHost = hostAndDB
		}
	}

	host := fallbackHost
	if cfg != nil && cfg.Postgres.PublicHost != "" {
		port := cfg.Postgres.PublicPort
		if port == 0 {
			port = 5432
		}
		host = fmt.Sprintf("%s:%d", cfg.Postgres.PublicHost, port)
	}

	// sslmode resolution: explicit base-URL query wins; else RequireTLS toggle.
	query := baseQuery
	if !strings.Contains(query, "sslmode=") {
		require := true
		if cfg != nil && cfg.Postgres.RequireTLS != nil {
			require = *cfg.Postgres.RequireTLS
		}
		mode := "require"
		if !require {
			mode = "disable"
		}
		if query != "" {
			query += "&"
		}
		query += "sslmode=" + mode
	}

	return fmt.Sprintf("postgres://%s:%s@%s/%s?%s", user, password, host, dbName, query)
}

func replaceDBName(baseURL, newDB string) string {
	parts := strings.SplitN(baseURL, "@", 2)
	if len(parts) != 2 {
		return baseURL
	}
	hostAndDB := parts[1]
	slashIdx := strings.Index(hostAndDB, "/")
	if slashIdx < 0 {
		return baseURL
	}
	host := hostAndDB[:slashIdx]
	qIdx := strings.Index(hostAndDB[slashIdx+1:], "?")
	queryStr := ""
	if qIdx >= 0 {
		queryStr = hostAndDB[slashIdx+1+qIdx:]
	}
	return parts[0] + "@" + host + "/" + newDB + queryStr
}
