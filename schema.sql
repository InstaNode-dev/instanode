-- schema.sql — minimal platform schema for instant-lite
-- Run once against the platform database before starting the server.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

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
