# Production config template — secrets injected at deploy time via envsubst.
# DO NOT put real credentials here. This file is committed to Git.

server:
  port: "8080"
  base_url: "${APP_URL}"
  read_timeout: "10s"
  write_timeout: "30s"
  idle_timeout: "120s"

database:
  platform_url: "${DATABASE_URL}"
  customer_url: "${DATABASE_URL}"
  max_open_conns: 20
  max_idle_conns: 5

redis:
  url: "${REDIS_URL}"

limits:
  rate_requests_per_second: 10
  rate_burst: 20
  max_provisions_per_day: 5
  anon_ttl: "24h"
  max_request_body_bytes: 1048576
  webhook_max_body_bytes: 1048576
  webhook_max_stored: 100
  ipv4_cidr_prefix: 24
  ipv6_cidr_prefix: 48

reaper:
  interval: "5m"
  batch_size: 50
  timeout: "60s"

postgres:
  conn_limit: 2
  storage_mb: 10
  statement_timeout: "30s"

observability:
  enabled: true
  service_name: "instant-lite-api"
  environment: "production"
  exporter: "otlp"
  otlp_endpoint: "otlp.nr-data.net"
  otlp_headers:
    api-key: "${NEWRELIC_LICENSE_KEY}"
  otlp_insecure: false
  sample_rate: 1.0
