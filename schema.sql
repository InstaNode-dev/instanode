-- schema.sql — minimal platform schema for instant-lite
-- Run once against the platform database before starting the server.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS users (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    github_id       BIGINT UNIQUE NOT NULL,
    email           TEXT UNIQUE NOT NULL,
    razorpay_customer_id TEXT,
    plan_tier       TEXT NOT NULL DEFAULT 'free',
    plan_period     TEXT NOT NULL DEFAULT 'monthly',
    plan_paid_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Additive migrations for existing installs (idempotent).
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_tier   TEXT NOT NULL DEFAULT 'free';
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_period TEXT NOT NULL DEFAULT 'monthly';
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_paid_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS resources (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    token           UUID NOT NULL UNIQUE DEFAULT uuid_generate_v4(),
    resource_type   TEXT NOT NULL CHECK (resource_type IN ('postgres', 'redis', 'webhook')),
    name            TEXT NOT NULL DEFAULT '',
    tier            TEXT NOT NULL DEFAULT 'anonymous',
    status          TEXT NOT NULL DEFAULT 'active',
    fingerprint     TEXT NOT NULL DEFAULT '',
    connection_url  TEXT NOT NULL DEFAULT '',
    key_prefix      TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ,
    migrated_to_user_id UUID REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_resources_fingerprint_type
    ON resources (fingerprint, resource_type)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_resources_token
    ON resources (token);

CREATE INDEX IF NOT EXISTS idx_resources_expires_at
    ON resources (expires_at)
    WHERE expires_at IS NOT NULL AND status = 'active';

CREATE TABLE IF NOT EXISTS processed_webhooks (
    event_id        TEXT PRIMARY KEY,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
