# instant-lite-api

Zero-setup database provisioning over HTTP. Single binary, one curl, real
Postgres connection string. Built for AI coding agents and local prototyping.

```bash
curl -X POST http://localhost:8080/db/new
# → { "connection_url": "postgres://...", "expires_at": "...", ... }
```

Same shape for webhook receivers (`POST /webhook/new`). No signup, no
Docker required in the caller's environment, no dashboard to navigate.

## What ships today

- `POST /db/new` — provisions an isolated Postgres database (per-token
  CREATE DATABASE + CREATE USER + CONNECTION LIMIT)
- `POST /webhook/new` + `GET/POST /webhook/receive/{token}` — webhook
  receiver for testing third-party callbacks
- GitHub OAuth login, `/claim` to convert anonymous 24h resources into
  permanent ones
- Razorpay-based billing (optional — runs fine without it)

Redis, Mongo, queue, and object storage provisioning are on the roadmap.
See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the production deployment
layout.

## Quick start

### Docker Compose

```bash
git clone https://github.com/InstaNode-dev/instant-lite-api
cd instant-lite-api
make docker-up

curl -s -X POST http://localhost:18080/db/new | jq
curl -s -X POST http://localhost:18080/webhook/new | jq
curl -s http://localhost:18080/healthz | jq

make docker-down
```

`docker-compose.yml` mounts `config.docker.yaml` into the container
automatically — it's pre-configured for the bundled Postgres + Redis.

### Bare Go

Prereqs: Go 1.25+, Postgres, Redis (optional).

```bash
cp config.example.yaml config.yaml        # edit the DB urls
createdb instant_lite
psql instant_lite < internal/server/schema.sql

make run
```

## Configuration

Everything lives in `config.yaml`. `CONFIG_PATH` is the only env var the
binary reads directly; secrets are picked up from their documented env
overrides (see [`config.example.yaml`](config.example.yaml) for the full
list).

Three commonly-adjusted fields for self-hosters:

| Field | What it controls |
|---|---|
| `server.base_url` | Public API URL this binary serves. Baked into webhook receive URLs emitted to clients. |
| `server.marketing_url` | Public website URL for post-OAuth redirects + upgrade CTAs. Empty ⇒ those paths 404, binary runs fine. |
| `server.cookie_domain` | Session cookie `Domain`. Empty = host-only. Set to a registrable domain to share across `api.example.com` + `example.com`. |

Billing is optional: leave `razorpay.*` empty and the payment endpoints
return 503 instead of calling out.

## Project layout

```
cmd/server/              thin main() entrypoint
internal/server/         everything else — handlers, auth, billing, db, etc.
  paths.go               route-path constants
  payment.go             billing-provider interface
  razorpay_client.go     razorpayPayment impl + SDK helpers
  payment_noop.go        no-op impl (when billing unconfigured)
  billing*.go            orders / webhook / subscriptions / change-plan / reconciler
  handlers.go            /db/new, /webhook/new provisioning
  auth.go                GitHub OAuth, JWT sessions
  reaper.go              background cleanup of expired resources
```

## Shipped endpoints

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/db/new` | Provision a Postgres database |
| `POST` | `/webhook/new` | Provision a webhook receiver |
| `POST`/`GET` | `/webhook/receive/{token}` | Receive webhook payloads |
| `GET` | `/auth/github/login` | Start OAuth login |
| `GET` | `/auth/me` | Current session user |
| `POST` | `/api/me/claim` | Claim an anonymous resource into an account |
| `GET` | `/api/me/resources` | List the caller's resources |
| `GET` | `/healthz` | Liveness |
| `GET` | `/readyz` | Readiness (pings all downstream deps) |
| `GET` | `/openapi.json` | OpenAPI 3.1 schema |
| `GET` | `/llms.txt` | Machine-readable docs for AI agents |

Full spec at `GET /openapi.json`.

## Deploying

- **Fly.io**: `fly.toml` and `spec.yaml.tpl` included; `fly launch
  --copy-config --no-deploy` to start.
- **DigitalOcean App Platform**: `spec.yaml.tpl` is a DO App spec template;
  `deploy-do.sh` shows the full path.
- **Bare VPS**: `make build` → copy `bin/instant-lite` + `config.yaml` +
  `schema.sql` → run behind Caddy or any reverse proxy.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for dev setup, PR conventions, and
code style. TL;DR: `make test && make vet` before every push.

## License

MIT. See [`LICENSE`](LICENSE).
