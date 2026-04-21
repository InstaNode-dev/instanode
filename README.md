# instant-lite-api

Backend API for instanode.dev — provisions real Postgres databases, Redis caches, and webhook
receivers with one HTTP call. No account, no Docker, no configuration.

## Git hooks

`.githooks/pre-push` blocks direct pushes to `master` to keep changes flowing
through PRs. Server-side protection is paywalled on GitHub's Free plan for
private repos, so this hook is the local stand-in. Opt in once after cloning:

```sh
git config core.hooksPath .githooks
```

Emergency bypass: `git push --no-verify`.

## Architecture

```
instanode.dev (GitHub Pages)  ←  Static HTML/CSS/JS (instant-lite-web/)
       │
       │  curl commands point to:
       ▼
api.instanode.dev (bare metal / Fly.io)  ←  This repo
       │
       ├── Postgres (CREATE DATABASE per token)
       └── Redis (ACL SETUSER per token)
```

The website is hosted separately on GitHub Pages. This repo is the API only.
Website traffic surges never affect provisioning.

## Quick start (docker compose)

```bash
docker compose up -d --build

curl -s -X POST http://localhost:18080/db/new | jq
curl -s -X POST http://localhost:18080/cache/new | jq
curl -s -X POST http://localhost:18080/webhook/new | jq
curl -s http://localhost:18080/healthz | jq

docker compose down -v
```

Docker Compose mounts `config.docker.yaml` into the container automatically.

## Quick start (local, no Docker)

```bash
# Prerequisites: Go 1.24+, Postgres, Redis
createdb instant_lite
psql instant_lite < schema.sql
redis-server &

# Edit config.yaml to match your local setup, then:
go run .
```

## Configuration

All settings live in `config.yaml`. The only environment variable is `CONFIG_PATH`
(defaults to `config.yaml`) to locate the config file.

```yaml
server:
  port: "8080"
  base_url: ""                  # Auto-derived if empty
  read_timeout: "10s"
  write_timeout: "30s"
  idle_timeout: "60s"

database:
  platform_url: "postgres://instant:instant@localhost:5432/instant_lite?sslmode=disable"
  customer_url: ""              # Falls back to platform_url if empty
  max_open_conns: 20
  max_idle_conns: 5

redis:
  url: "redis://localhost:6379"

limits:
  rate_requests_per_second: 10  # HTTP rate limit (token bucket)
  rate_burst: 20                # Max burst
  max_provisions_per_day: 5     # Per-IP/subnet daily cap
  anon_ttl: "24h"               # TTL for anonymous resources
  max_request_body_bytes: 1048576
  webhook_max_body_bytes: 1048576
  webhook_max_stored: 100
  ipv4_cidr_prefix: 24          # Subnet grouping for fingerprinting
  ipv6_cidr_prefix: 48

reaper:
  interval: "5m"                # Cleanup frequency
  batch_size: 50                # Max resources cleaned per cycle
  timeout: "60s"                # Context timeout per cycle

postgres:
  conn_limit: 2                 # CONNECTION LIMIT per provisioned DB
  storage_mb: 10                # Storage quota hint
  statement_timeout: "30s"      # Max query time per provisioned user
```

For Docker deployments, edit `config.docker.yaml` (hostnames differ inside containers).

## Deploy to Fly.io

```bash
fly launch --copy-config --no-deploy --name instant-lite-api --region iad
fly postgres create --name instant-lite-db --region iad --vm-size shared-cpu-1x
fly redis create --name instant-lite-redis --region iad --plan free
fly postgres attach instant-lite-db -a instant-lite-api
fly redis attach instant-lite-redis -a instant-lite-api
# Upload your config.yaml as a secret or mount via fly.toml
fly deploy
fly ssh console -a instant-lite-api -C "psql \$DATABASE_URL -f /app/schema.sql"
```

## Deploy to bare metal / VPS

```bash
CGO_ENABLED=0 go build -o instant-lite-api .
scp instant-lite-api schema.sql config.yaml user@yourserver:~/

# On server:
sudo -u postgres createdb instant_lite
psql instant_lite < schema.sql

# Edit config.yaml with production values, then:
./instant-lite-api
```

Put Caddy in front for automatic HTTPS:
```
# /etc/caddy/Caddyfile
api.instanode.dev {
    reverse_proxy localhost:8080
}
```

## Endpoints

| Method | Path | What it does |
|--------|------|-------------|
| POST | `/db/new` | Provision a Postgres database |
| POST | `/cache/new` | Provision a Redis cache |
| POST | `/webhook/new` | Provision a webhook receiver |
| POST | `/webhook/receive/{token}` | Receive a webhook payload |
| GET | `/healthz` | Health check |
| GET | `/llms.txt` | Machine-readable docs for AI agents |
| GET | `/` | 302 redirect to https://instanode.dev |

## Security

All security parameters are configurable via `config.yaml`:

- **Daily provision limit:** Configurable per IP/subnet (atomic Redis counter, Postgres fallback)
- **HTTP rate limit:** Configurable req/s per IP (token bucket)
- **Request body limit:** Configurable max bytes
- **Postgres isolation:** Separate database + user per token, configurable CONNECTION LIMIT and statement_timeout
- **Redis isolation:** ACL user per token, key-prefix enforcement
- **Expired resource cleanup:** Background reaper with configurable interval and batch size
