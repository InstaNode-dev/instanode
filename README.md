# instant-lite-api

Backend API for instant.dev — provisions real Postgres databases, Redis caches, and webhook
receivers with one HTTP call. No account, no Docker, no configuration.

## Architecture

```
instant.dev (GitHub Pages)  ←  Static HTML/CSS/JS (instant-lite-web/)
       │
       │  curl commands point to:
       ▼
api.instant.dev (bare metal / Fly.io)  ←  This repo
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

## Quick start (local, no Docker)

```bash
# Prerequisites: Go 1.24+, Postgres, Redis
createdb instant_lite
psql instant_lite < schema.sql
redis-server &

DATABASE_URL="postgres://localhost:5432/instant_lite?sslmode=disable" \
REDIS_URL="redis://localhost:6379" \
BASE_URL="http://localhost:8080" \
go run .
```

## Deploy to Fly.io

```bash
fly launch --copy-config --no-deploy --name instant-lite-api --region iad
fly postgres create --name instant-lite-db --region iad --vm-size shared-cpu-1x
fly redis create --name instant-lite-redis --region iad --plan free
fly postgres attach instant-lite-db -a instant-lite-api
fly redis attach instant-lite-redis -a instant-lite-api
fly secrets set BASE_URL=https://api.instant.dev
fly deploy
fly ssh console -a instant-lite-api -C "psql \$DATABASE_URL -f /app/schema.sql"
```

## Deploy to bare metal / VPS

```bash
CGO_ENABLED=0 go build -o instant-lite-api .
scp instant-lite-api schema.sql user@yourserver:~/

# On server:
sudo -u postgres createdb instant_lite
psql instant_lite < schema.sql

DATABASE_URL="postgres://localhost:5432/instant_lite?sslmode=disable" \
CUSTOMER_DATABASE_URL="postgres://localhost:5432/instant_lite?sslmode=disable" \
REDIS_URL="redis://localhost:6379" \
BASE_URL="https://api.instant.dev" \
PORT=8080 \
./instant-lite-api
```

Put Caddy in front for automatic HTTPS:
```
# /etc/caddy/Caddyfile
api.instant.dev {
    reverse_proxy localhost:8080
}
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | 8080 | HTTP listen port |
| `BASE_URL` | http://localhost:8080 | Public URL (used in response `note` and webhook `receive_url`) |
| `DATABASE_URL` | postgres://instant:instant@localhost:5432/instant_lite?sslmode=disable | Platform Postgres |
| `CUSTOMER_DATABASE_URL` | same as DATABASE_URL | Customer Postgres (where CREATE DATABASE runs) |
| `REDIS_URL` | redis://localhost:6379 | Redis (rate limiting + webhook storage) |

## Endpoints

| Method | Path | What it does |
|--------|------|-------------|
| POST | `/db/new` | Provision a Postgres database |
| POST | `/cache/new` | Provision a Redis cache |
| POST | `/webhook/new` | Provision a webhook receiver |
| POST | `/webhook/receive/{token}` | Receive a webhook payload |
| GET | `/healthz` | Health check |
| GET | `/llms.txt` | Machine-readable docs for AI agents |
| GET | `/` | 302 redirect to https://instant.dev |

## Security

- **Daily provision limit:** 5 per IP/subnet (atomic Redis counter, Postgres fallback)
- **HTTP rate limit:** 10 req/s per IP (token bucket)
- **Request body limit:** 1 MB
- **Postgres isolation:** Separate database + user per token, CONNECTION LIMIT 2, statement_timeout 30s
- **Redis isolation:** ACL user per token, key-prefix enforcement
- **Expired resource cleanup:** Background reaper every 5 minutes (drops DBs, deletes ACL users)
