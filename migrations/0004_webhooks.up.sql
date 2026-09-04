-- Webhook delivery (§7).
--
-- At-least-once, not exactly-once. That is a deliberate, documented contract:
-- guaranteeing exactly-once delivery to a third party we do not control is not
-- possible, so we guarantee delivery happens and give merchants an event_id to
-- dedupe on. Stripe publishes the same contract for the same reason.

CREATE TABLE webhook_endpoints (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id  UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    url          TEXT        NOT NULL,

    -- HMAC-SHA256 signing secret. Stored so we can sign; a merchant verifies
    -- with the same value, so unlike an API key this cannot be one-way hashed.
    -- In production it lives in Secrets Manager and this column holds a
    -- reference rather than the secret itself.
    secret       TEXT        NOT NULL,

    -- Rotation needs a window where BOTH secrets produce valid signatures,
    -- or rotating breaks every in-flight delivery (§22.2). The old secret
    -- lives here until it expires.
    previous_secret            TEXT,
    previous_secret_expires_at TIMESTAMPTZ,

    -- Empty means every event. Otherwise only these types are delivered.
    subscribed_events TEXT[] NOT NULL DEFAULT '{}',

    enabled      BOOLEAN     NOT NULL DEFAULT true,
    -- Set when the retry budget is exhausted repeatedly; surfaced in the
    -- dashboard so a merchant can see their endpoint is broken.
    disabled_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_webhook_endpoints_merchant ON webhook_endpoints (merchant_id)
    WHERE enabled;

-- One row per (event, endpoint) pair: the same event fans out to every
-- subscribed endpoint, and each delivery succeeds or fails independently.
CREATE TABLE webhook_deliveries (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id   UUID        NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    merchant_id   UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,

    -- The merchant-visible event id, sent in the payload so they can dedupe
    -- our at-least-once retries (§7).
    event_id      UUID        NOT NULL,
    event_type    VARCHAR(50) NOT NULL,
    payload       JSONB       NOT NULL,

    status        TEXT        NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'delivered', 'failed', 'dead')),
    attempts      INT         NOT NULL DEFAULT 0,

    -- When this delivery becomes eligible again. Backoff is implemented by
    -- pushing this forward rather than by sleeping, so a worker holds no state
    -- between attempts and can die at any point without losing the schedule.
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    last_status_code INT,
    last_error       TEXT,
    last_attempt_at  TIMESTAMPTZ,
    delivered_at     TIMESTAMPTZ,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The same event must never be queued twice for one endpoint. This is what
    -- makes the outbox poller safe to run concurrently and safe to re-run after
    -- a crash: a redelivered outbox row cannot create a second delivery.
    UNIQUE (endpoint_id, event_id)
);

-- The worker's claim query: due, not finished, oldest first. Partial so the
-- index stays small as delivered rows accumulate into the millions.
CREATE INDEX idx_webhook_deliveries_due
    ON webhook_deliveries (next_attempt_at)
    WHERE status = 'pending';

CREATE INDEX idx_webhook_deliveries_merchant
    ON webhook_deliveries (merchant_id, created_at DESC);

-- Per-attempt history, for the merchant-facing delivery log (§7). Separate
-- from the delivery row because merchants need to see WHY it failed three
-- times, not just that it did.
CREATE TABLE webhook_delivery_attempts (
    id            BIGSERIAL PRIMARY KEY,
    delivery_id   UUID        NOT NULL REFERENCES webhook_deliveries(id) ON DELETE CASCADE,
    attempt       INT         NOT NULL,
    status_code   INT,
    error         TEXT,
    duration_ms   INT,
    attempted_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_webhook_attempts_delivery
    ON webhook_delivery_attempts (delivery_id, attempt);

-- Tracks how far the outbox poller has read, so it does not rescan published
-- rows on every tick. A single row.
CREATE TABLE outbox_cursor (
    id            INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    last_event_id BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO outbox_cursor (id, last_event_id) VALUES (1, 0);
