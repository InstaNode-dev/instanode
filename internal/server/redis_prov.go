package server

// Cache-as-a-service is intentionally not offered. The managed Valkey is
// internal-only (rate limits + webhook storage). If you reintroduce a Redis
// provisioning endpoint, host a self-hosted Redis with ACL support and proxy
// it behind a domain — never hand out the shared default-user credentials
// of the internal Valkey.
