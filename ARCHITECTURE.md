# Architecture & Deployment State

This document outlines the current technical architecture, observability state, and production deployment configuration for the `instant-lite-api`.

## 1. Core Architecture
The `instant-lite-api` is a Go-based server designed to provision ephemeral databases (PostgreSQL) and proxy webhooks. 

### Graceful Degradation (Redis Fallback)
The application natively supports Redis for caching and advanced webhook proxying. However, it is explicitly designed for **cost-effective horizontal scalability via "Graceful Degradation."**
* The production configuration currently sets `redis.url: ""` in `config.prod.yaml.tpl`.
* When the Go engine parses an empty Redis connection string, it securely boots up using **strictly PostgreSQL**.
* All rate-limiting rules and logic fall back to Postgres seamlessly. 
* The API endpoints related to Redis provisioning (`/cache/new`) will simply fail-open or return degradation notices safely.

## 2. Production Observability (New Relic + OpenTelemetry)
We have fully eradicated vendor-lock by utilizing native Go OpenTelemetry (OTel) pipelines targeting New Relic as the backend.

* **Trace Injections:** Every HTTP request natively generates an OpenTelemetry Trace ID and Span ID.
* **Smart Middleware:** We implemented `s.traceEnrichmentMiddleware()` which grabs the unique hashed `fingerprint` (the User ID) and permanently injects it as `user.id` onto the Trace Span. This allows unified User Journey tracking in New Relic.
* **Context-Aware Logging:** All `slog` logs are wrapped in `slog.InfoContext` or `slog.ErrorContext` to bind trace attributes to standard logging. New Relic perfectly correlates your APM Trace graphs with the exact log texts.
* **Panic Recovery Bridge:** The custom `panicRecoveryMiddleware` intercepts fatal container crashes before they kill the HTTP handler, capturing the crash stack trace and exporting it safely to New Relic with a `500 Server Error`.
* **Startup Hardening:** `main.go` uses explicit standard-error (`os.Stderr`) outputs for fatal startup traps (e.g., Unreachable Database) to guarantee cloud providers like Digital Ocean capture the logs before the container shuts down.

## 3. DigitalOcean App Platform Deployment
The entire App Platform uses Infrastructure-as-code principles.

* **Deployment Blueprint:** The `Dockerfile` natively installs the Linux `gettext` package to utilize `envsubst`.
* **Automated Secrets:** Upon boot on DigitalOcean, the container executes `envsubst < /app/config.prod.yaml.tpl > /app/config.yaml`. This dynamically searches for the Digital Ocean `$DATABASE_URL`, `$APP_URL`, and `$NEWRELIC_LICENSE_KEY` runtime variables and injects them securely directly into RAM before executing the API.
* **Base URL Alignment:** The production webhook replies automatically use DO's magic `${APP_URL}` variable, meaning attaching custom domains in DigitalOcean natively updates all JSON outputs across the API.

## Costs
The current DO App Spec (`master` branch) operates on the following structure:
* **~ $20 / mo**: Web Service Load Balancing (2x `apps-s-1vcpu-1gb` instances on Professional tier).
* **~ $7 / mo**: PostgreSQL App Platform Dev Database (`dev-db-667010`).

Total operating cost: **~$27 / mo**.
*Note: A standalone Managed Valkey/Redis instance would cost an additional $15/mo. We disabled this constraint to preserve costs.*
