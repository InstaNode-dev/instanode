#!/bin/bash
set -e

echo "Updating system..."
apt-get update
apt-get install -y docker.io

echo "Deploying Postgres 17 with pgvector..."
docker run -d \
  --name instant-postgres \
  --restart always \
  -e POSTGRES_PASSWORD='***REDACTED-DROPLET-PG-ROOT***' \
  -p 5432:5432 \
  ankane/pgvector:v0.6.0-pg16

echo "Postgres started successfully!"
