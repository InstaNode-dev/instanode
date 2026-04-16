#!/usr/bin/env bash
set -euo pipefail

# deploy.sh — One-command deploy to Fly.io
#
# Prerequisites:
#   brew install flyctl
#   fly auth login
#
# First-time setup (run once):
#   fly launch --copy-config --no-deploy --name instant-lite-api --region iad
#   fly postgres create --name instant-lite-db --region iad --vm-size shared-cpu-1x
#   fly redis create --name instant-lite-redis --region iad --plan free
#   fly postgres attach instant-lite-db -a instant-lite-api
#   fly redis attach instant-lite-redis -a instant-lite-api
#   fly secrets set BASE_URL=https://api.instant.dev
#
# After first-time setup, just run:
#   ./deploy.sh

echo "==> Building and deploying instant-lite-api to Fly.io..."

# Run schema migration against Fly Postgres
echo "==> Running schema migration..."
fly ssh console -a instant-lite-api -C "psql \$DATABASE_URL -f /app/schema.sql" 2>/dev/null || {
    echo "    (migration skipped — run manually if first deploy)"
}

# Deploy
fly deploy

echo ""
echo "==> Deployed! Verifying health..."
sleep 3

HEALTH=$(curl -sf https://api.instant.dev/healthz 2>/dev/null || echo '{"ok":false}')
echo "    $HEALTH"

echo ""
echo "==> Test provisioning:"
echo '    curl -s -X POST https://api.instant.dev/db/new | jq'
