package server

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all tunable parameters for instant-lite-api.
// All values are set via config.yaml. The only environment variable
// respected is CONFIG_PATH (to locate the YAML file itself).
type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Database      DatabaseConfig      `yaml:"database"`
	Redis         RedisConfig         `yaml:"redis"`
	GitHub        GitHubConfig        `yaml:"github"`
	Razorpay      RazorpayConfig      `yaml:"razorpay"`
	JWT           JWTConfig           `yaml:"jwt"`
	Limits        LimitsConfig        `yaml:"limits"`
	Reaper        ReaperConfig        `yaml:"reaper"`
	Postgres      ProvisionConfig     `yaml:"postgres"`
	Email         EmailConfig         `yaml:"email"`
	Admin         AdminConfig         `yaml:"admin"`
	Observability ObservabilityConfig `yaml:"observability"`
	Features      FeaturesConfig      `yaml:"features"`
}

// FeaturesConfig gates feature flags that default off for safe rollout.
// Toggle via env var (ENABLE_PLAN_SWITCH=1) or config YAML.
type FeaturesConfig struct {
	// EnablePlanSwitch controls POST/DELETE /billing/change-plan + the
	// pending-switch fields on GET /api/me/plan. When false, the endpoints
	// return 404 and the plan-switch reconciler pass is skipped.
	EnablePlanSwitch bool `yaml:"enable_plan_switch"`
}

// AdminConfig gates admin-only endpoints (e.g. /admin/inbox) to a single
// operator email. Only a signed-in user whose users.email matches
// Admin.Email is allowed in; everyone else (including other paid users) sees 403.
type AdminConfig struct {
	Email string `yaml:"email"`
}

type EmailConfig struct {
	// SMTP transport (Brevo/SendGrid/any provider exposing SMTP on 587+STARTTLS).
	// When SMTPHost is empty, email sending is disabled and Send() becomes a no-op
	// (logs a debug line). Never blocks request paths; callers always use SendAsync.
	SMTPHost    string `yaml:"smtp_host"`
	SMTPPort    int    `yaml:"smtp_port"`
	SMTPUser    string `yaml:"smtp_user"`
	SMTPPass    string `yaml:"smtp_pass"`
	FromAddress string `yaml:"from_address"`
	FromName    string `yaml:"from_name"`
	// BrevoInboundSecret gates POST /webhooks/brevo-inbound. Brevo appends it
	// as `?token=<secret>` and we compare in constant time. Set via the
	// BREVO_INBOUND_SECRET env var in prod; empty disables the endpoint (401s).
	BrevoInboundSecret string `yaml:"brevo_inbound_secret"`
	// BrevoAPIKey (xkeysib-…) authenticates our reconciler calls against
	// https://api.brevo.com/v3/inbound/events so we can backfill metadata
	// for any email Brevo received but whose webhook delivery failed.
	// Empty disables the reconciler goroutine.
	BrevoAPIKey string `yaml:"brevo_api_key"`
	// ReconcileInterval is how often the reconciler polls Brevo. Default 10m
	// so a lost webhook shows up within 10 minutes. Parsed with time.ParseDuration.
	ReconcileInterval string `yaml:"reconcile_interval"`
}

type ObservabilityConfig struct {
	Enabled      bool              `yaml:"enabled"`
	ServiceName  string            `yaml:"service_name"`
	Environment  string            `yaml:"environment"`
	Exporter     string            `yaml:"exporter"`      // "otlp" or "stdout"
	OTLPEndpoint string            `yaml:"otlp_endpoint"`
	OTLPHeaders  map[string]string `yaml:"otlp_headers"`
	OTLPInsecure bool              `yaml:"otlp_insecure"` // true for local collectors
	SampleRate   float64           `yaml:"sample_rate"`   // 0.0 to 1.0
}

type ServerConfig struct {
	Port         string `yaml:"port"`
	BaseURL      string `yaml:"base_url"`
	ReadTimeout  string `yaml:"read_timeout"`
	WriteTimeout string `yaml:"write_timeout"`
	IdleTimeout  string `yaml:"idle_timeout"`
}

type DatabaseConfig struct {
	PlatformURL  string `yaml:"platform_url"`
	CustomerURL  string `yaml:"customer_url"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

type RedisConfig struct {
	// URL is the Valkey/Redis backend for internal state only
	// (rate limits + webhook storage). Not exposed to customers.
	URL string `yaml:"url"`
}

type GitHubConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	RedirectURI  string `yaml:"redirect_uri"`
}

type RazorpayConfig struct {
	KeyID         string `yaml:"key_id"`
	KeySecret     string `yaml:"key_secret"`
	WebhookSecret string `yaml:"webhook_secret"`
	// Legacy (USD-only) plan IDs. Kept as the fallback when a currency-scoped
	// plan ID below is missing, so rollouts that forget the new envs don't
	// break /billing/create-subscription.
	PlanIDMonthly string `yaml:"plan_id_monthly"`
	PlanIDAnnual  string `yaml:"plan_id_annual"`
	// Currency-scoped plan IDs. Razorpay subscriptions are bound to a single
	// currency at plan-creation time, so USD vs INR users subscribe to
	// distinct plan IDs. When empty, planConfig falls back to PlanIDMonthly /
	// PlanIDAnnual above (USD).
	PlanIDUSDMonthly string `yaml:"plan_id_usd_monthly"`
	PlanIDUSDYearly  string `yaml:"plan_id_usd_yearly"`
	PlanIDINRMonthly string `yaml:"plan_id_inr_monthly"`
	PlanIDINRYearly  string `yaml:"plan_id_inr_yearly"`
}

type JWTConfig struct {
	Secret string `yaml:"secret"`
}

type LimitsConfig struct {
	RateRequestsPerSecond float64 `yaml:"rate_requests_per_second"`
	RateBurst             int     `yaml:"rate_burst"`
	MaxProvisionsPerDay   int     `yaml:"max_provisions_per_day"`
	AnonTTL               string  `yaml:"anon_ttl"`
	MaxRequestBodyBytes   int64   `yaml:"max_request_body_bytes"`
	WebhookMaxBodyBytes   int64   `yaml:"webhook_max_body_bytes"`
	WebhookMaxStored      int64   `yaml:"webhook_max_stored"`
	IPv4CIDRPrefix        int     `yaml:"ipv4_cidr_prefix"`
	IPv6CIDRPrefix        int     `yaml:"ipv6_cidr_prefix"`
}

type ReaperConfig struct {
	Interval  string `yaml:"interval"`
	BatchSize int    `yaml:"batch_size"`
	Timeout   string `yaml:"timeout"`
}

type ProvisionConfig struct {
	ConnLimit        int    `yaml:"conn_limit"`
	StorageMB        int    `yaml:"storage_mb"`
	StatementTimeout string `yaml:"statement_timeout"`
	// PublicHost is the hostname (domain) exposed to customers in
	// connection URLs. The customer never sees the raw PG IP — only
	// a domain that points to our proxy/bouncer (real PG lives on
	// a private VPC address). If empty, falls back to the host in
	// Database.CustomerURL (dev mode only).
	PublicHost string `yaml:"public_host"`
	// PublicPort is the TCP port to embed in connection URLs. Defaults
	// to 5432 when unset. Use this when the proxy/bouncer listens on
	// a non-default port (e.g. PgBouncer on 6432).
	PublicPort int `yaml:"public_port"`
	// RequireTLS forces sslmode=require in generated connection URLs
	// when no sslmode is present in the base URL. Defaults to true.
	// Set false only for local dev.
	RequireTLS *bool `yaml:"require_tls"`
}

// DefaultConfig returns the configuration with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Port:         "8080",
			BaseURL:      "",
			ReadTimeout:  "10s",
			WriteTimeout: "30s",
			IdleTimeout:  "60s",
		},
		Database: DatabaseConfig{
			PlatformURL:  "postgres://instant:instant@localhost:5432/instant_lite?sslmode=disable",
			CustomerURL:  "",
			MaxOpenConns: 20,
			MaxIdleConns: 5,
		},
		Redis: RedisConfig{
			URL: "redis://localhost:6379",
		},
		Limits: LimitsConfig{
			RateRequestsPerSecond: 10,
			RateBurst:             20,
			MaxProvisionsPerDay:   5,
			AnonTTL:               "24h",
			MaxRequestBodyBytes:   1 << 20, // 1 MB
			WebhookMaxBodyBytes:   1 << 20, // 1 MB
			WebhookMaxStored:      100,
			IPv4CIDRPrefix:        24,
			IPv6CIDRPrefix:        48,
		},
		Reaper: ReaperConfig{
			Interval:  "5m",
			BatchSize: 50,
			Timeout:   "60s",
		},
		Postgres: ProvisionConfig{
			ConnLimit:        2,
			StorageMB:        10,
			StatementTimeout: "30s",
			PublicPort:       5432,
			RequireTLS:       boolPtr(true),
		},
		Email: EmailConfig{
			SMTPPort:    587,
			FromAddress: "no-reply@instanode.dev",
			FromName:    "instanode",
		},
		Observability: ObservabilityConfig{
			Enabled:      false,
			ServiceName:  "instant-lite-api",
			Environment:  "development",
			Exporter:     "otlp",
			OTLPEndpoint: "localhost:4318",
			OTLPHeaders:  map[string]string{},
			OTLPInsecure: true,
			SampleRate:   1.0,
		},
	}
}

// LoadConfig loads configuration from the YAML file at path.
// If the file is missing, defaults are used.
func LoadConfig(path string) *Config {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("config: file not found, using defaults", "path", path)
	} else {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			slog.Error("config: failed to parse config file", "path", path, "error", err)
			os.Exit(1)
		}
		slog.Info("config: loaded from file", "path", path)
	}

	// Override with environment variables for secrets
	cfg.overrideWithEnv()

	// Derived defaults.
	if cfg.Server.BaseURL == "" {
		cfg.Server.BaseURL = "http://localhost:" + cfg.Server.Port
	}
	if cfg.Database.CustomerURL == "" {
		slog.Error("config: customer_url is required and must not fall back to platform_url")
		os.Exit(1)
	}

	return cfg
}

// overrideWithEnv overrides config values with environment variables for secrets if not set in config
func (c *Config) overrideWithEnv() {
	if c.GitHub.ClientID == "" {
		if v := os.Getenv("GITHUB_CLIENT_ID"); v != "" {
			c.GitHub.ClientID = v
		}
	}
	if c.GitHub.ClientSecret == "" {
		if v := os.Getenv("GITHUB_CLIENT_SECRET"); v != "" {
			c.GitHub.ClientSecret = v
		}
	}
	if c.Razorpay.KeyID == "" {
		if v := os.Getenv("RAZORPAY_KEY_ID"); v != "" {
			c.Razorpay.KeyID = v
		}
	}
	if c.Razorpay.KeySecret == "" {
		if v := os.Getenv("RAZORPAY_KEY_SECRET"); v != "" {
			c.Razorpay.KeySecret = v
		}
	}
	if c.Razorpay.WebhookSecret == "" {
		if v := os.Getenv("RAZORPAY_WEBHOOK_SECRET"); v != "" {
			c.Razorpay.WebhookSecret = v
		}
	}
	if c.Razorpay.PlanIDMonthly == "" {
		if v := os.Getenv("RAZORPAY_PLAN_ID_MONTHLY"); v != "" {
			c.Razorpay.PlanIDMonthly = v
		}
	}
	if c.Razorpay.PlanIDAnnual == "" {
		if v := os.Getenv("RAZORPAY_PLAN_ID_ANNUAL"); v != "" {
			c.Razorpay.PlanIDAnnual = v
		}
	}
	if c.Razorpay.PlanIDUSDMonthly == "" {
		if v := os.Getenv("RAZORPAY_PLAN_ID_USD_MONTHLY"); v != "" {
			c.Razorpay.PlanIDUSDMonthly = v
		}
	}
	if c.Razorpay.PlanIDUSDYearly == "" {
		if v := os.Getenv("RAZORPAY_PLAN_ID_USD_YEARLY"); v != "" {
			c.Razorpay.PlanIDUSDYearly = v
		}
	}
	if c.Razorpay.PlanIDINRMonthly == "" {
		if v := os.Getenv("RAZORPAY_PLAN_ID_INR_MONTHLY"); v != "" {
			c.Razorpay.PlanIDINRMonthly = v
		}
	}
	if c.Razorpay.PlanIDINRYearly == "" {
		if v := os.Getenv("RAZORPAY_PLAN_ID_INR_YEARLY"); v != "" {
			c.Razorpay.PlanIDINRYearly = v
		}
	}
	if c.JWT.Secret == "" {
		if v := os.Getenv("JWT_SECRET"); v != "" {
			c.JWT.Secret = v
		}
	}
		if c.Database.PlatformURL == "" {
		if v := os.Getenv("DATABASE_URL"); v != "" {
			c.Database.PlatformURL = v
		}
	}
	if c.Database.CustomerURL == "" {
		if v := os.Getenv("CUSTOMER_DATABASE_URL"); v != "" {
			c.Database.CustomerURL = v
		}
	}
	if c.Redis.URL == "" {
		if v := os.Getenv("REDIS_URL"); v != "" {
			c.Redis.URL = v
		}
	}
	if v := os.Getenv("POSTGRES_PUBLIC_HOST"); v != "" {
		c.Postgres.PublicHost = v
	}
	if v := os.Getenv("POSTGRES_PUBLIC_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Postgres.PublicPort = p
		}
	}
	if v := os.Getenv("POSTGRES_REQUIRE_TLS"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		c.Postgres.RequireTLS = &b
	}
	if v := os.Getenv("BREVO_SMTP_HOST"); v != "" {
		c.Email.SMTPHost = v
	}
	if v := os.Getenv("BREVO_SMTP_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Email.SMTPPort = p
		}
	}
	if v := os.Getenv("BREVO_SMTP_USER"); v != "" {
		c.Email.SMTPUser = v
	}
	if v := os.Getenv("BREVO_SMTP_PASS"); v != "" {
		c.Email.SMTPPass = v
	}
	if v := os.Getenv("EMAIL_FROM_ADDRESS"); v != "" {
		c.Email.FromAddress = v
	}
	if v := os.Getenv("EMAIL_FROM_NAME"); v != "" {
		c.Email.FromName = v
	}
	if v := os.Getenv("BREVO_INBOUND_SECRET"); v != "" {
		c.Email.BrevoInboundSecret = v
	}
	if v := os.Getenv("BREVO_API_KEY"); v != "" {
		c.Email.BrevoAPIKey = v
	}
	if v := os.Getenv("BREVO_RECONCILE_INTERVAL"); v != "" {
		c.Email.ReconcileInterval = v
	}
	if v := os.Getenv("ADMIN_EMAIL"); v != "" {
		c.Admin.Email = v
	}
	if v := os.Getenv("ENABLE_PLAN_SWITCH"); v != "" {
		c.Features.EnablePlanSwitch = strings.EqualFold(v, "true") || v == "1"
	}
}

func boolPtr(b bool) *bool { return &b }

// Parsed duration helpers — called once at startup so handlers don't parse repeatedly.

func (c *Config) ParsedAnonTTL() time.Duration {
	d, err := time.ParseDuration(c.Limits.AnonTTL)
	if err != nil {
		slog.Warn("config: invalid anon_ttl, defaulting to 24h", "error", err)
		return 24 * time.Hour
	}
	return d
}

func (c *Config) ParsedReaperInterval() time.Duration {
	d, err := time.ParseDuration(c.Reaper.Interval)
	if err != nil {
		slog.Warn("config: invalid reaper interval, defaulting to 5m", "error", err)
		return 5 * time.Minute
	}
	return d
}

func (c *Config) ParsedReaperTimeout() time.Duration {
	d, err := time.ParseDuration(c.Reaper.Timeout)
	if err != nil {
		slog.Warn("config: invalid reaper timeout, defaulting to 60s", "error", err)
		return 60 * time.Second
	}
	return d
}

func (c *Config) ParsedReadTimeout() time.Duration {
	d, _ := time.ParseDuration(c.Server.ReadTimeout)
	if d == 0 {
		return 10 * time.Second
	}
	return d
}

func (c *Config) ParsedWriteTimeout() time.Duration {
	d, _ := time.ParseDuration(c.Server.WriteTimeout)
	if d == 0 {
		return 30 * time.Second
	}
	return d
}

func (c *Config) ParsedIdleTimeout() time.Duration {
	d, _ := time.ParseDuration(c.Server.IdleTimeout)
	if d == 0 {
		return 60 * time.Second
	}
	return d
}

func (c *Config) Summary() string {
	return fmt.Sprintf("port=%s base_url=%s reaper=%s anon_ttl=%s rate=%.0f/%d provisions/day=%d",
		c.Server.Port, c.Server.BaseURL, c.Reaper.Interval, c.Limits.AnonTTL,
		c.Limits.RateRequestsPerSecond, c.Limits.RateBurst, c.Limits.MaxProvisionsPerDay)
}
