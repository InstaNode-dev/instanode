package main

import (
	"testing"
	"time"
)

// TestDefaultConfig verifies the documented default values set in DefaultConfig().
func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg == nil {
		t.Fatal("DefaultConfig() returned nil")
	}

	if cfg.Server.Port != "8080" {
		t.Errorf("Server.Port = %q, want %q", cfg.Server.Port, "8080")
	}
	if cfg.Limits.MaxProvisionsPerDay != 5 {
		t.Errorf("Limits.MaxProvisionsPerDay = %d, want 5", cfg.Limits.MaxProvisionsPerDay)
	}
	if cfg.Limits.AnonTTL != "24h" {
		t.Errorf("Limits.AnonTTL = %q, want %q", cfg.Limits.AnonTTL, "24h")
	}
	if cfg.Limits.IPv4CIDRPrefix != 24 {
		t.Errorf("Limits.IPv4CIDRPrefix = %d, want 24", cfg.Limits.IPv4CIDRPrefix)
	}
	if cfg.Limits.IPv6CIDRPrefix != 48 {
		t.Errorf("Limits.IPv6CIDRPrefix = %d, want 48", cfg.Limits.IPv6CIDRPrefix)
	}
	if cfg.Postgres.ConnLimit != 2 {
		t.Errorf("Postgres.ConnLimit = %d, want 2", cfg.Postgres.ConnLimit)
	}
	if cfg.Postgres.StorageMB != 10 {
		t.Errorf("Postgres.StorageMB = %d, want 10", cfg.Postgres.StorageMB)
	}
	if cfg.Postgres.PublicPort != 5432 {
		t.Errorf("Postgres.PublicPort = %d, want 5432", cfg.Postgres.PublicPort)
	}
	if cfg.Postgres.RequireTLS == nil {
		t.Fatal("Postgres.RequireTLS is nil, want non-nil pointer to true")
	}
	if *cfg.Postgres.RequireTLS != true {
		t.Errorf("*Postgres.RequireTLS = %v, want true", *cfg.Postgres.RequireTLS)
	}
	if cfg.Email.SMTPPort != 587 {
		t.Errorf("Email.SMTPPort = %d, want 587", cfg.Email.SMTPPort)
	}
	if cfg.Email.FromAddress != "no-reply@instanode.dev" {
		t.Errorf("Email.FromAddress = %q, want %q", cfg.Email.FromAddress, "no-reply@instanode.dev")
	}
}

// TestOverrideWithEnv_FillsEmptySecrets verifies env vars populate Config fields
// that are empty/default. The "only-if-empty" secret fields are: GitHub, Razorpay,
// JWT, Database.PlatformURL, Database.CustomerURL, Redis.URL.
func TestOverrideWithEnv_FillsEmptySecrets(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "gh-client-id")
	t.Setenv("GITHUB_CLIENT_SECRET", "gh-client-secret")
	t.Setenv("RAZORPAY_KEY_ID", "rzp-key-id")
	t.Setenv("RAZORPAY_KEY_SECRET", "rzp-key-secret")
	t.Setenv("RAZORPAY_WEBHOOK_SECRET", "rzp-webhook-secret")
	t.Setenv("RAZORPAY_PLAN_ID_MONTHLY", "plan-monthly")
	t.Setenv("RAZORPAY_PLAN_ID_ANNUAL", "plan-annual")
	t.Setenv("JWT_SECRET", "jwt-secret")
	t.Setenv("CUSTOMER_DATABASE_URL", "postgres://cust@host/db")
	// PlatformURL and Redis.URL are non-empty in DefaultConfig — they must be
	// cleared first. We build a blank Config to isolate behavior.

	cfg := &Config{}
	cfg.overrideWithEnv()

	if cfg.GitHub.ClientID != "gh-client-id" {
		t.Errorf("GitHub.ClientID = %q, want %q", cfg.GitHub.ClientID, "gh-client-id")
	}
	if cfg.GitHub.ClientSecret != "gh-client-secret" {
		t.Errorf("GitHub.ClientSecret = %q, want %q", cfg.GitHub.ClientSecret, "gh-client-secret")
	}
	if cfg.Razorpay.KeyID != "rzp-key-id" {
		t.Errorf("Razorpay.KeyID = %q, want %q", cfg.Razorpay.KeyID, "rzp-key-id")
	}
	if cfg.Razorpay.KeySecret != "rzp-key-secret" {
		t.Errorf("Razorpay.KeySecret = %q, want %q", cfg.Razorpay.KeySecret, "rzp-key-secret")
	}
	if cfg.Razorpay.WebhookSecret != "rzp-webhook-secret" {
		t.Errorf("Razorpay.WebhookSecret = %q, want %q", cfg.Razorpay.WebhookSecret, "rzp-webhook-secret")
	}
	if cfg.Razorpay.PlanIDMonthly != "plan-monthly" {
		t.Errorf("Razorpay.PlanIDMonthly = %q, want %q", cfg.Razorpay.PlanIDMonthly, "plan-monthly")
	}
	if cfg.Razorpay.PlanIDAnnual != "plan-annual" {
		t.Errorf("Razorpay.PlanIDAnnual = %q, want %q", cfg.Razorpay.PlanIDAnnual, "plan-annual")
	}
	if cfg.JWT.Secret != "jwt-secret" {
		t.Errorf("JWT.Secret = %q, want %q", cfg.JWT.Secret, "jwt-secret")
	}
	if cfg.Database.CustomerURL != "postgres://cust@host/db" {
		t.Errorf("Database.CustomerURL = %q, want %q", cfg.Database.CustomerURL, "postgres://cust@host/db")
	}
}

// TestOverrideWithEnv_DatabaseURL verifies DATABASE_URL fills PlatformURL when blank.
func TestOverrideWithEnv_DatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://platform@host/db")
	cfg := &Config{}
	cfg.overrideWithEnv()
	if cfg.Database.PlatformURL != "postgres://platform@host/db" {
		t.Errorf("Database.PlatformURL = %q, want %q", cfg.Database.PlatformURL, "postgres://platform@host/db")
	}
}

// TestOverrideWithEnv_RedisURL verifies REDIS_URL fills Redis.URL when blank.
func TestOverrideWithEnv_RedisURL(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://custom:6379")
	cfg := &Config{}
	cfg.overrideWithEnv()
	if cfg.Redis.URL != "redis://custom:6379" {
		t.Errorf("Redis.URL = %q, want %q", cfg.Redis.URL, "redis://custom:6379")
	}
}

// TestOverrideWithEnv_DoesNotOverrideNonEmpty verifies that when a secret field
// is already populated (e.g. from YAML), the env var does NOT clobber it.
func TestOverrideWithEnv_DoesNotOverrideNonEmpty(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "from-env")
	t.Setenv("GITHUB_CLIENT_SECRET", "from-env")
	t.Setenv("RAZORPAY_KEY_ID", "from-env")
	t.Setenv("RAZORPAY_KEY_SECRET", "from-env")
	t.Setenv("RAZORPAY_WEBHOOK_SECRET", "from-env")
	t.Setenv("RAZORPAY_PLAN_ID_MONTHLY", "from-env")
	t.Setenv("RAZORPAY_PLAN_ID_ANNUAL", "from-env")
	t.Setenv("JWT_SECRET", "from-env")
	t.Setenv("DATABASE_URL", "postgres://env/db")
	t.Setenv("CUSTOMER_DATABASE_URL", "postgres://env/cust")
	t.Setenv("REDIS_URL", "redis://env:6379")

	cfg := &Config{
		GitHub: GitHubConfig{
			ClientID:     "yaml-id",
			ClientSecret: "yaml-secret",
		},
		Razorpay: RazorpayConfig{
			KeyID:         "yaml-key-id",
			KeySecret:     "yaml-key-secret",
			WebhookSecret: "yaml-webhook",
			PlanIDMonthly: "yaml-monthly",
			PlanIDAnnual:  "yaml-annual",
		},
		JWT: JWTConfig{Secret: "yaml-jwt"},
		Database: DatabaseConfig{
			PlatformURL: "postgres://yaml/plat",
			CustomerURL: "postgres://yaml/cust",
		},
		Redis: RedisConfig{URL: "redis://yaml:6379"},
	}
	cfg.overrideWithEnv()

	if cfg.GitHub.ClientID != "yaml-id" {
		t.Errorf("GitHub.ClientID overridden: got %q", cfg.GitHub.ClientID)
	}
	if cfg.GitHub.ClientSecret != "yaml-secret" {
		t.Errorf("GitHub.ClientSecret overridden: got %q", cfg.GitHub.ClientSecret)
	}
	if cfg.Razorpay.KeyID != "yaml-key-id" {
		t.Errorf("Razorpay.KeyID overridden: got %q", cfg.Razorpay.KeyID)
	}
	if cfg.Razorpay.KeySecret != "yaml-key-secret" {
		t.Errorf("Razorpay.KeySecret overridden: got %q", cfg.Razorpay.KeySecret)
	}
	if cfg.Razorpay.WebhookSecret != "yaml-webhook" {
		t.Errorf("Razorpay.WebhookSecret overridden: got %q", cfg.Razorpay.WebhookSecret)
	}
	if cfg.Razorpay.PlanIDMonthly != "yaml-monthly" {
		t.Errorf("Razorpay.PlanIDMonthly overridden: got %q", cfg.Razorpay.PlanIDMonthly)
	}
	if cfg.Razorpay.PlanIDAnnual != "yaml-annual" {
		t.Errorf("Razorpay.PlanIDAnnual overridden: got %q", cfg.Razorpay.PlanIDAnnual)
	}
	if cfg.JWT.Secret != "yaml-jwt" {
		t.Errorf("JWT.Secret overridden: got %q", cfg.JWT.Secret)
	}
	if cfg.Database.PlatformURL != "postgres://yaml/plat" {
		t.Errorf("Database.PlatformURL overridden: got %q", cfg.Database.PlatformURL)
	}
	if cfg.Database.CustomerURL != "postgres://yaml/cust" {
		t.Errorf("Database.CustomerURL overridden: got %q", cfg.Database.CustomerURL)
	}
	if cfg.Redis.URL != "redis://yaml:6379" {
		t.Errorf("Redis.URL overridden: got %q", cfg.Redis.URL)
	}
}

// TestOverrideWithEnv_AlwaysOverrideFields verifies the fields that the code
// unconditionally overrides (no "if empty" guard): POSTGRES_PUBLIC_HOST,
// POSTGRES_PUBLIC_PORT, POSTGRES_REQUIRE_TLS, BREVO_SMTP_*, EMAIL_FROM_*.
func TestOverrideWithEnv_AlwaysOverrideFields(t *testing.T) {
	t.Setenv("POSTGRES_PUBLIC_HOST", "db.example.com")
	t.Setenv("POSTGRES_PUBLIC_PORT", "6432")
	t.Setenv("POSTGRES_REQUIRE_TLS", "false")
	t.Setenv("BREVO_SMTP_HOST", "smtp.example.com")
	t.Setenv("BREVO_SMTP_PORT", "2525")
	t.Setenv("BREVO_SMTP_USER", "smtp-user")
	t.Setenv("BREVO_SMTP_PASS", "smtp-pass")
	t.Setenv("EMAIL_FROM_ADDRESS", "hello@example.com")
	t.Setenv("EMAIL_FROM_NAME", "Example")

	// Pre-populate with non-default YAML values to prove env wins.
	yamlTLS := true
	cfg := &Config{
		Postgres: ProvisionConfig{
			PublicHost: "yaml-host",
			PublicPort: 1111,
			RequireTLS: &yamlTLS,
		},
		Email: EmailConfig{
			SMTPHost:    "yaml-smtp",
			SMTPPort:    999,
			SMTPUser:    "yaml-user",
			SMTPPass:    "yaml-pass",
			FromAddress: "yaml@example.com",
			FromName:    "YAML",
		},
	}
	cfg.overrideWithEnv()

	if cfg.Postgres.PublicHost != "db.example.com" {
		t.Errorf("Postgres.PublicHost = %q, want %q", cfg.Postgres.PublicHost, "db.example.com")
	}
	if cfg.Postgres.PublicPort != 6432 {
		t.Errorf("Postgres.PublicPort = %d, want 6432", cfg.Postgres.PublicPort)
	}
	if cfg.Postgres.RequireTLS == nil || *cfg.Postgres.RequireTLS != false {
		t.Errorf("Postgres.RequireTLS = %v, want false", cfg.Postgres.RequireTLS)
	}
	if cfg.Email.SMTPHost != "smtp.example.com" {
		t.Errorf("Email.SMTPHost = %q, want %q", cfg.Email.SMTPHost, "smtp.example.com")
	}
	if cfg.Email.SMTPPort != 2525 {
		t.Errorf("Email.SMTPPort = %d, want 2525", cfg.Email.SMTPPort)
	}
	if cfg.Email.SMTPUser != "smtp-user" {
		t.Errorf("Email.SMTPUser = %q, want %q", cfg.Email.SMTPUser, "smtp-user")
	}
	if cfg.Email.SMTPPass != "smtp-pass" {
		t.Errorf("Email.SMTPPass = %q, want %q", cfg.Email.SMTPPass, "smtp-pass")
	}
	if cfg.Email.FromAddress != "hello@example.com" {
		t.Errorf("Email.FromAddress = %q, want %q", cfg.Email.FromAddress, "hello@example.com")
	}
	if cfg.Email.FromName != "Example" {
		t.Errorf("Email.FromName = %q, want %q", cfg.Email.FromName, "Example")
	}
}

// TestOverrideWithEnv_PostgresRequireTLSParsing verifies the truthy variants
// accepted for POSTGRES_REQUIRE_TLS: "true" (any case) and "1" map to true,
// anything else maps to false.
func TestOverrideWithEnv_PostgresRequireTLSParsing(t *testing.T) {
	cases := []struct {
		envVal string
		want   bool
	}{
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"yes", false}, // only "true"/"1" are truthy per the code
	}
	for _, tc := range cases {
		t.Run(tc.envVal, func(t *testing.T) {
			t.Setenv("POSTGRES_REQUIRE_TLS", tc.envVal)
			cfg := &Config{}
			cfg.overrideWithEnv()
			if cfg.Postgres.RequireTLS == nil {
				t.Fatal("Postgres.RequireTLS is nil")
			}
			if *cfg.Postgres.RequireTLS != tc.want {
				t.Errorf("POSTGRES_REQUIRE_TLS=%q → *RequireTLS = %v, want %v",
					tc.envVal, *cfg.Postgres.RequireTLS, tc.want)
			}
		})
	}
}

// TestOverrideWithEnv_InvalidPortsIgnored verifies that non-numeric POSTGRES_PUBLIC_PORT
// and BREVO_SMTP_PORT values are silently ignored (the original value is kept).
func TestOverrideWithEnv_InvalidPortsIgnored(t *testing.T) {
	t.Setenv("POSTGRES_PUBLIC_PORT", "not-a-number")
	t.Setenv("BREVO_SMTP_PORT", "also-bad")
	cfg := &Config{
		Postgres: ProvisionConfig{PublicPort: 5432},
		Email:    EmailConfig{SMTPPort: 587},
	}
	cfg.overrideWithEnv()
	if cfg.Postgres.PublicPort != 5432 {
		t.Errorf("Postgres.PublicPort = %d, want 5432 (unchanged)", cfg.Postgres.PublicPort)
	}
	if cfg.Email.SMTPPort != 587 {
		t.Errorf("Email.SMTPPort = %d, want 587 (unchanged)", cfg.Email.SMTPPort)
	}
}

// TestParsedAnonTTL covers valid, empty, and invalid inputs.
func TestParsedAnonTTL(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := &Config{Limits: LimitsConfig{AnonTTL: "45m"}}
		if got := cfg.ParsedAnonTTL(); got != 45*time.Minute {
			t.Errorf("ParsedAnonTTL() = %v, want %v", got, 45*time.Minute)
		}
	})
	t.Run("empty defaults to 24h", func(t *testing.T) {
		cfg := &Config{Limits: LimitsConfig{AnonTTL: ""}}
		if got := cfg.ParsedAnonTTL(); got != 24*time.Hour {
			t.Errorf("ParsedAnonTTL() = %v, want %v", got, 24*time.Hour)
		}
	})
	t.Run("invalid defaults to 24h", func(t *testing.T) {
		cfg := &Config{Limits: LimitsConfig{AnonTTL: "not-a-duration"}}
		if got := cfg.ParsedAnonTTL(); got != 24*time.Hour {
			t.Errorf("ParsedAnonTTL() = %v, want %v", got, 24*time.Hour)
		}
	})
}

// TestParsedReaperInterval covers valid, empty, and invalid inputs.
func TestParsedReaperInterval(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := &Config{Reaper: ReaperConfig{Interval: "2m"}}
		if got := cfg.ParsedReaperInterval(); got != 2*time.Minute {
			t.Errorf("ParsedReaperInterval() = %v, want %v", got, 2*time.Minute)
		}
	})
	t.Run("empty defaults to 5m", func(t *testing.T) {
		cfg := &Config{Reaper: ReaperConfig{Interval: ""}}
		if got := cfg.ParsedReaperInterval(); got != 5*time.Minute {
			t.Errorf("ParsedReaperInterval() = %v, want %v", got, 5*time.Minute)
		}
	})
	t.Run("invalid defaults to 5m", func(t *testing.T) {
		cfg := &Config{Reaper: ReaperConfig{Interval: "bad"}}
		if got := cfg.ParsedReaperInterval(); got != 5*time.Minute {
			t.Errorf("ParsedReaperInterval() = %v, want %v", got, 5*time.Minute)
		}
	})
}

// TestParsedReaperTimeout covers valid, empty, and invalid inputs.
func TestParsedReaperTimeout(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := &Config{Reaper: ReaperConfig{Timeout: "90s"}}
		if got := cfg.ParsedReaperTimeout(); got != 90*time.Second {
			t.Errorf("ParsedReaperTimeout() = %v, want %v", got, 90*time.Second)
		}
	})
	t.Run("empty defaults to 60s", func(t *testing.T) {
		cfg := &Config{Reaper: ReaperConfig{Timeout: ""}}
		if got := cfg.ParsedReaperTimeout(); got != 60*time.Second {
			t.Errorf("ParsedReaperTimeout() = %v, want %v", got, 60*time.Second)
		}
	})
	t.Run("invalid defaults to 60s", func(t *testing.T) {
		cfg := &Config{Reaper: ReaperConfig{Timeout: "bogus"}}
		if got := cfg.ParsedReaperTimeout(); got != 60*time.Second {
			t.Errorf("ParsedReaperTimeout() = %v, want %v", got, 60*time.Second)
		}
	})
}

// TestParsedReadTimeout covers valid, empty, and invalid inputs.
// ParsedReadTimeout uses a different pattern than the reaper helpers: it
// ignores parse errors and returns 10s when the parsed value is zero.
func TestParsedReadTimeout(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := &Config{Server: ServerConfig{ReadTimeout: "15s"}}
		if got := cfg.ParsedReadTimeout(); got != 15*time.Second {
			t.Errorf("ParsedReadTimeout() = %v, want %v", got, 15*time.Second)
		}
	})
	t.Run("empty defaults to 10s", func(t *testing.T) {
		cfg := &Config{Server: ServerConfig{ReadTimeout: ""}}
		if got := cfg.ParsedReadTimeout(); got != 10*time.Second {
			t.Errorf("ParsedReadTimeout() = %v, want %v", got, 10*time.Second)
		}
	})
	t.Run("invalid defaults to 10s", func(t *testing.T) {
		cfg := &Config{Server: ServerConfig{ReadTimeout: "not-a-duration"}}
		if got := cfg.ParsedReadTimeout(); got != 10*time.Second {
			t.Errorf("ParsedReadTimeout() = %v, want %v", got, 10*time.Second)
		}
	})
}
