-- PayFlow initial schema.
-- Design references are to docs/payment-gateway-system-design.md.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Merchants & API keys (§8)
-- ---------------------------------------------------------------------------

CREATE TABLE merchants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT        NOT NULL,
    email        TEXT        NOT NULL UNIQUE,
    country      CHAR(2)     NOT NULL DEFAULT 'US',
    -- Default settlement currency; charges may be in other currencies (§20).
    currency     CHAR(3)     NOT NULL DEFAULT 'USD',
    risk_tier    TEXT        NOT NULL DEFAULT 'standard'
                 CHECK (risk_tier IN ('standard', 'elevated', 'high')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only the hash is stored; the raw key is shown exactly once at creation (§8).
CREATE TABLE api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id  UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    key_hash     TEXT        NOT NULL UNIQUE,
    -- Stored so the dashboard can show "sk_test_abc1...". Never the full key.
    key_prefix   TEXT        NOT NULL,
    mode         TEXT        NOT NULL CHECK (mode IN ('test', 'live')),
    scope        TEXT        NOT NULL DEFAULT 'write' CHECK (scope IN ('read', 'write')),
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_api_keys_merchant ON api_keys (merchant_id) WHERE revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- Idempotency (§4.2)
-- ---------------------------------------------------------------------------

CREATE TABLE idempotency_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key VARCHAR(255) NOT NULL,
    merchant_id     UUID         NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    -- SHA-256 of the normalized request body. A different hash under the same
    -- key means the client reused a key for a different request → 422 (§4.2).
    request_hash    VARCHAR(64)  NOT NULL,
    endpoint        TEXT         NOT NULL,
    -- 'requires_action' exists because 3DS step-up resumes the *original*
    -- key rather than starting a new request (§16).
    status          VARCHAR(20)  NOT NULL
                    CHECK (status IN ('processing', 'requires_action', 'completed', 'failed')),
    response_body   JSONB,
    response_status INT,
    -- Set while a worker owns the row; a stale value means the worker died
    -- and the lock can be stolen (§4.2 step 4).
    locked_at       TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- Computed from Postgres now(), never an app pod's clock (§22.1).
    expires_at      TIMESTAMPTZ  NOT NULL DEFAULT now() + INTERVAL '24 hours',
    UNIQUE (merchant_id, idempotency_key)
);
CREATE INDEX idx_idem_expiry ON idempotency_keys (expires_at);
CREATE INDEX idx_idem_stale_locks ON idempotency_keys (locked_at)
    WHERE status = 'processing';

-- ---------------------------------------------------------------------------
-- Ledger (§5) — append-only double-entry
-- ---------------------------------------------------------------------------

CREATE TABLE accounts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id  UUID REFERENCES merchants(id) ON DELETE CASCADE,
    -- merchant_balance / platform_fees / customer_receivable / in_transit /
    -- paid_out / merchant_debt (§19) / reserve (§19).
    account_type TEXT        NOT NULL,
    currency     CHAR(3)     NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A merchant has at most one account of each type per currency. Platform
    -- accounts have merchant_id IS NULL, handled by the partial index below.
    UNIQUE (merchant_id, account_type, currency)
);
CREATE UNIQUE INDEX idx_accounts_platform
    ON accounts (account_type, currency) WHERE merchant_id IS NULL;

CREATE TABLE ledger_entries (
    id              BIGSERIAL PRIMARY KEY,
    -- Groups the debit and credit legs of one financial event.
    transaction_id  UUID         NOT NULL,
    account_id      UUID         NOT NULL REFERENCES accounts(id),
    direction       VARCHAR(6)   NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount_cents    BIGINT       NOT NULL CHECK (amount_cents > 0),
    currency        CHAR(3)      NOT NULL,
    -- What caused this entry: charge / refund / dispute / dispute_reversal /
    -- payout / fee / fx_conversion.
    entry_type      TEXT         NOT NULL,
    idempotency_key VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    metadata        JSONB        NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX idx_ledger_account ON ledger_entries (account_id, created_at);
CREATE INDEX idx_ledger_txn ON ledger_entries (transaction_id);

-- Append-only is enforced, not merely documented (§5). Without this, a bug or
-- a stray UPDATE could silently rewrite financial history.
CREATE OR REPLACE FUNCTION ledger_entries_immutable() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % denied on id %',
        TG_OP, COALESCE(OLD.id, NEW.id);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_ledger_entries_no_update
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_immutable();

-- The core invariant: debits equal credits, per transaction AND per currency
-- (§20 — a balanced pair may never mix currencies).
--
-- DEFERRABLE INITIALLY DEFERRED is what makes this usable: the check runs at
-- COMMIT, so a transaction can insert its debit leg and credit leg as separate
-- statements and still be validated as a unit. A non-deferred constraint would
-- fire after the first leg and reject every legitimate write.
CREATE OR REPLACE FUNCTION ledger_balanced() RETURNS TRIGGER AS $$
DECLARE
    imbalance RECORD;
BEGIN
    FOR imbalance IN
        SELECT currency,
               SUM(CASE WHEN direction = 'debit' THEN amount_cents
                        ELSE -amount_cents END) AS delta
        FROM ledger_entries
        WHERE transaction_id = NEW.transaction_id
        GROUP BY currency
        HAVING SUM(CASE WHEN direction = 'debit' THEN amount_cents
                        ELSE -amount_cents END) <> 0
    LOOP
        RAISE EXCEPTION
            'unbalanced ledger transaction %: currency % is off by % cents',
            NEW.transaction_id, imbalance.currency, imbalance.delta;
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER trg_ledger_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_balanced();

-- Cached running balance — the one deliberate denormalization (§5). The
-- authoritative value is always SUM(ledger_entries); this exists so reads
-- don't scan history. Reconciliation re-derives and compares (§24.3).
CREATE TABLE ledger_balances (
    account_id     UUID PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    balance_cents  BIGINT      NOT NULL DEFAULT 0,
    currency       CHAR(3)     NOT NULL,
    -- Guards against out-of-order application of concurrent updates.
    version        BIGINT      NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Charges (§4, §14)
-- ---------------------------------------------------------------------------

CREATE TABLE charges (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id       UUID        NOT NULL REFERENCES merchants(id),
    amount_cents      BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency          CHAR(3)     NOT NULL,
    status            TEXT        NOT NULL CHECK (status IN (
                          'pending', 'requires_action', 'succeeded', 'failed',
                          -- Set on ambiguous downstream timeout; the hourly
                          -- reconciliation job resolves it (§24.1).
                          'requires_reconciliation')),
    failure_code      TEXT,
    failure_message   TEXT,

    -- Card data: never the PAN. Fingerprint is a salted hash, used for
    -- velocity rules and blocklists (§14.5).
    card_fingerprint  TEXT,
    card_last4        CHAR(4),
    card_brand        TEXT,
    card_bin          CHAR(6),

    -- Fraud signals, stored so a decision can be explained after the fact (§14.5).
    risk_score        NUMERIC(5,4),
    risk_level        TEXT CHECK (risk_level IN ('low', 'medium', 'high')),
    risk_rules_fired  JSONB NOT NULL DEFAULT '[]'::jsonb,
    device_fingerprint TEXT,
    ip_address        INET,

    ledger_transaction_id UUID,
    processor_reference   TEXT,
    idempotency_key   VARCHAR(255),
    metadata          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_charges_merchant ON charges (merchant_id, created_at DESC);
CREATE INDEX idx_charges_fingerprint ON charges (card_fingerprint, created_at DESC);
CREATE INDEX idx_charges_reconcile ON charges (created_at)
    WHERE status = 'requires_reconciliation';

-- ---------------------------------------------------------------------------
-- Transactional outbox (§22.1)
-- ---------------------------------------------------------------------------
-- Written in the SAME transaction as the ledger entry, so "money moved" and
-- "event will be published" can never diverge because a broker was down.

CREATE TABLE outbox_events (
    id           BIGSERIAL PRIMARY KEY,
    aggregate_id UUID        NOT NULL,
    event_type   VARCHAR(50) NOT NULL,
    payload      JSONB       NOT NULL,
    published    BOOLEAN     NOT NULL DEFAULT false,
    published_at TIMESTAMPTZ,
    attempts     INT         NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Partial index: the poller only ever scans unpublished rows, so the index
-- stays small even as the table grows to millions of published events.
CREATE INDEX idx_outbox_unpublished ON outbox_events (id) WHERE NOT published;
