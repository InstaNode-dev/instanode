package server

import (
	"strings"
	"testing"
)

func TestSanitizeToken(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"a1b2c3d4-e5f6-7890-abcd-ef1234567890", "a1b2c3d4e5f6"},
		// sanitizeToken doesn't lowercase — uppercase letters fall outside
		// the [a-z0-9] keep-set so they get stripped. Only digits survive.
		{"A1B2C3D4-E5F6-7890-ABCD-EF1234567890", "123456"},
		{"00000000-0000-0000-0000-000000000000", "000000000000"},
		{"abc!@#def-hij$%^", "abcdefhij"},
		{"short", "short"},
		{"", ""},
	}
	for _, tc := range tests {
		got := sanitizeToken(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeToken_DoesNotExceed12Chars(t *testing.T) {
	// All inputs must produce at most 12 chars — dbName becomes "db_<12chars>"
	// and PG identifier limit is 63.
	long := strings.Repeat("abcd", 20)
	if got := sanitizeToken(long); len(got) > 12 {
		t.Errorf("sanitizeToken produced %d chars (max 12): %q", len(got), got)
	}
}

func TestRandomPassword(t *testing.T) {
	for _, n := range []int{8, 16, 24, 32} {
		got := randomPassword(n)
		if len(got) != n {
			t.Errorf("randomPassword(%d) returned len %d", n, len(got))
		}
		// Must be hex — a-f0-9.
		for _, c := range got {
			isHex := (c >= 'a' && c <= 'f') || (c >= '0' && c <= '9')
			if !isHex {
				t.Errorf("randomPassword returned non-hex char %q in %q", c, got)
			}
		}
	}
	// Two calls should not collide (probabilistic but 48 hex chars is 192 bits).
	if randomPassword(24) == randomPassword(24) {
		t.Error("randomPassword(24) returned identical values on two calls")
	}
}

func TestReplaceDBName(t *testing.T) {
	tests := []struct {
		name, in, newDB, want string
	}{
		{"simple", "postgres://u:p@h:5432/olddb", "newdb", "postgres://u:p@h:5432/newdb"},
		{"with_query", "postgres://u:p@h/olddb?sslmode=require", "newdb", "postgres://u:p@h/newdb?sslmode=require"},
		{"multi_query", "postgres://u:p@h/olddb?sslmode=require&connect_timeout=5", "newdb", "postgres://u:p@h/newdb?sslmode=require&connect_timeout=5"},
		{"no_path", "postgres://u:p@h", "newdb", "postgres://u:p@h"}, // no slash → unchanged
		{"malformed", "not-a-url", "newdb", "not-a-url"},             // no @ → unchanged
	}
	for _, tc := range tests {
		got := replaceDBName(tc.in, tc.newDB)
		if got != tc.want {
			t.Errorf("%s: replaceDBName(%q, %q) = %q, want %q", tc.name, tc.in, tc.newDB, got, tc.want)
		}
	}
}

func TestBuildConnURL_UsesPublicHost(t *testing.T) {
	cfg := &Config{
		Postgres: ProvisionConfig{
			PublicHost: "pg.instanode.dev",
			PublicPort: 5432,
		},
	}
	got := buildConnURL("postgres://root:secret@10.0.0.1:5432/postgres", "db_abc", "usr_abc", "pwd", cfg)
	if !strings.Contains(got, "pg.instanode.dev:5432") {
		t.Errorf("buildConnURL should substitute PublicHost; got %q", got)
	}
	if strings.Contains(got, "10.0.0.1") {
		t.Errorf("buildConnURL leaked raw PG IP into customer URL: %q", got)
	}
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("buildConnURL should default sslmode=require; got %q", got)
	}
}

func TestBuildConnURL_RequireTLSFalse(t *testing.T) {
	require := false
	cfg := &Config{Postgres: ProvisionConfig{PublicHost: "localhost", PublicPort: 5432, RequireTLS: &require}}
	got := buildConnURL("postgres://u:p@h/postgres", "db", "u", "p", cfg)
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("RequireTLS=false should produce sslmode=disable; got %q", got)
	}
}

func TestBuildConnURL_PreservesExistingSSLMode(t *testing.T) {
	cfg := &Config{Postgres: ProvisionConfig{PublicHost: "pg.example.com"}}
	got := buildConnURL("postgres://u:p@h/postgres?sslmode=verify-full", "db", "u", "p", cfg)
	// Explicit sslmode in base URL must win.
	if !strings.Contains(got, "sslmode=verify-full") {
		t.Errorf("buildConnURL should preserve explicit sslmode; got %q", got)
	}
	if strings.Count(got, "sslmode=") != 1 {
		t.Errorf("buildConnURL wrote multiple sslmode params: %q", got)
	}
}
