#!/usr/bin/env bash
set -euo pipefail

# deploy-do.sh — Deploy instant-lite-api to Digital Ocean Droplet
#
# Prerequisites:
#   1. Droplet created with Docker pre-installed (docker-24-04 image)
#   2. SSH key added to Droplet
#   3. DNS pointing api.instant.dev → Droplet IP
#   4. On server: .env file with POSTGRES_PASSWORD and REDIS_PASSWORD
#   5. On server: config.prod.yaml with actual passwords filled in
#
# Usage:
#   ./deploy-do.sh <droplet-ip>
#   ./deploy-do.sh 164.90.xxx.xxx

if [ -z "${1:-}" ]; then
    echo "Usage: ./deploy-do.sh <droplet-ip>"
    exit 1
fi

SERVER="root@$1"
REMOTE_DIR="/root/instant-lite"
DOMAIN="api.instant.dev"

echo "==> Syncing files to $SERVER:$REMOTE_DIR ..."
rsync -avz --delete \
    --exclude '.git' \
    --exclude '.env' \
    --exclude 'config.prod.yaml' \
    ./ "$SERVER:$REMOTE_DIR/"

echo ""
echo "==> Building and starting containers..."
ssh "$SERVER" "cd $REMOTE_DIR && docker compose -f docker-compose.prod.yml up -d --build"

echo ""
echo "==> Waiting for services to start..."
sleep 5

echo "==> Health check..."
HEALTH=$(ssh "$SERVER" "curl -sf http://localhost:8080/healthz 2>/dev/null" || echo '{"ok":false}')
echo "    $HEALTH"

if echo "$HEALTH" | grep -q '"ok":true'; then
    echo ""
    echo "==> ✅ Deployed successfully!"
    echo ""
    echo "    Test externally:"
    echo "    curl -s https://$DOMAIN/healthz | jq"
    echo "    curl -s -X POST https://$DOMAIN/db/new | jq"
else
    echo ""
    echo "==> ❌ Health check failed. Check logs:"
    echo "    ssh $SERVER 'docker logs instant-lite-app-1 --tail 20'"
fi
