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

-- Razorpay Subscriptions (recurring billing).
-- subscription_status mirrors Razorpay's lifecycle: created → authenticated →
-- active → (charged repeatedly) → cancelled | halted | completed.
ALTER TABLE users ADD COLUMN IF NOT EXISTS razorpay_subscription_id TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS subscription_status TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS current_period_end TIMESTAMPTZ;

-- Email idempotency markers. The webhook handlers, the one-time-order handler,
-- and the billing reconciler all race to send confirmation / cancellation
-- emails after a state change. An atomic UPDATE ... WHERE receipt_email_sent_at
-- IS NULL OR receipt_email_sent_at < plan_paid_at acts as the claim lock: only
-- the caller whose UPDATE affects a row is allowed to send. Without these we
-- saw duplicate receipts when Razorpay retried webhooks + the reconciler tick
-- ran simultaneously.
ALTER TABLE users ADD COLUMN IF NOT EXISTS receipt_email_sent_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS cancel_email_sent_at  TIMESTAMPTZ;

-- Plan-switch (monthly ↔ annual). The switch is scheduled — we don't tear
-- down the current subscription the moment the user clicks, because the
-- charge they already paid for is still running. pending_plan_effective_at
-- is set to the current_period_end at request time, so the switch fires
-- cleanly at the boundary. pending_plan_sub_id holds the Razorpay sub id
-- for the new plan once the reconciler has created it; until then it's
-- NULL and the switch can still be cancelled by DELETE /billing/change-plan.
ALTER TABLE users ADD COLUMN IF NOT EXISTS pending_plan_change        TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS pending_plan_effective_at  TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS pending_plan_sub_id        TEXT;

-- Plan-switch email idempotency. Same claim-lock pattern as receipt/cancel.
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_switch_scheduled_email_sent_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_switch_activated_email_sent_at TIMESTAMPTZ;

-- Currency lock-in. Set when the user first subscribes and thereafter
-- determines which Razorpay plan id pool (USD vs INR) they can switch
-- within. NULL for free users; populated at subscription.charged time from
-- notes.currency. VARCHAR(3) is the ISO 4217 shape (USD, INR, ...).
ALTER TABLE users ADD COLUMN IF NOT EXISTS plan_currency VARCHAR(3);

CREATE TABLE IF NOT EXISTS resources (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    token           UUID NOT NULL UNIQUE DEFAULT uuid_generate_v4(),
    resource_type   TEXT NOT NULL CHECK (resource_type IN ('postgres', 'redis', 'webhook')),
    name            TEXT NOT NULL DEFAULT '',
    tier            TEXT NOT NULL DEFAULT 'anonymous',
    -- Status transitions:
    --   active   → expired  (reaper; TTL elapsed)
    --   active   → deleted  (user-initiated DELETE /api/me/resources/{token})
    --   deleted  → reaped   (reaper; underlying DB has been dropped)
    status          TEXT NOT NULL DEFAULT 'active',
    fingerprint     TEXT NOT NULL DEFAULT '',
    connection_url  TEXT NOT NULL DEFAULT '',
    key_prefix      TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    migrated_to_user_id UUID REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE resources ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_resources_status_deleted
    ON resources (deleted_at)
    WHERE status = 'deleted';

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

-- Inbound email (Brevo Inbound Parsing webhook). Every Brevo POST to
-- /webhooks/brevo-inbound inserts one row per item. provider_id is Brevo's
-- MessageId (or a fallback hash) and is UNIQUE so redeliveries are idempotent.
CREATE TABLE IF NOT EXISTS inbound_messages (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider_id TEXT UNIQUE,
    from_email  TEXT NOT NULL,
    from_name   TEXT,
    to_email    TEXT NOT NULL,
    subject     TEXT NOT NULL DEFAULT '',
    body_text   TEXT NOT NULL DEFAULT '',
    body_html   TEXT NOT NULL DEFAULT '',
    spam_score  REAL,
    raw_headers JSONB,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    read_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_inbound_messages_received_at ON inbound_messages (received_at DESC);
