-- Refunds (§17) and disputes (§15).
--
-- Both are reversals of money that already moved, and both follow the same
-- rule as everything else in this ledger: a reversal is a NEW balanced pair of
-- entries referencing the original transaction, never a mutation of it.

CREATE TABLE refunds (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    charge_id       UUID        NOT NULL REFERENCES charges(id),
    merchant_id     UUID        NOT NULL REFERENCES merchants(id),
    amount_cents    BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency        CHAR(3)     NOT NULL,
    status          TEXT        NOT NULL CHECK (status IN (
                        'pending', 'succeeded', 'failed',
                        'requires_reconciliation')),
    reason          TEXT        CHECK (reason IN (
                        'requested_by_customer', 'duplicate', 'fraudulent')),
    failure_code    TEXT,
    failure_message TEXT,

    idempotency_key VARCHAR(255),
    ledger_transaction_id UUID,
    processor_reference   TEXT,
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_refunds_charge ON refunds (charge_id);
CREATE INDEX idx_refunds_merchant ON refunds (merchant_id, created_at DESC);

CREATE TABLE disputes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    charge_id       UUID        NOT NULL REFERENCES charges(id),
    merchant_id     UUID        NOT NULL REFERENCES merchants(id),
    amount_cents    BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency        CHAR(3)     NOT NULL,
    reason          TEXT        NOT NULL,
    status          TEXT        NOT NULL CHECK (status IN (
                        'needs_response', 'under_review', 'won', 'lost')),

    -- Networks allow 7-21 days to respond depending on the card scheme (§15).
    evidence_due_by TIMESTAMPTZ NOT NULL,
    evidence        JSONB,
    evidence_submitted_at TIMESTAMPTZ,
    resolved_at     TIMESTAMPTZ,

    -- The funds reversal posted when the dispute opened, and the counter-
    -- reversal posted if it is later won. Kept separate so the audit trail
    -- shows both movements distinctly.
    ledger_transaction_id     UUID,
    resolution_transaction_id UUID,

    -- The acquirer's reference for this chargeback.
    processor_reference TEXT UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_disputes_charge ON disputes (charge_id);
CREATE INDEX idx_disputes_merchant ON disputes (merchant_id, created_at DESC);
-- The worklist the dashboard shows: disputes still awaiting a response,
-- soonest deadline first.
CREATE INDEX idx_disputes_open ON disputes (evidence_due_by)
    WHERE status IN ('needs_response', 'under_review');

-- A charge may be disputed only once by the network, so the processor
-- reference is unique above. A second chargeback on the same charge would be
-- a distinct dispute with its own reference.

-- Amount refunded is DERIVED, never a mutable column on charges (§17).
-- Storing a running total invites it to drift from the refunds that produced
-- it; a view cannot.
CREATE VIEW charge_refund_totals AS
SELECT c.id AS charge_id,
       c.amount_cents,
       COALESCE(SUM(r.amount_cents) FILTER (WHERE r.status = 'succeeded'), 0) AS refunded_cents,
       -- Pending refunds are reserved against the balance too: two concurrent
       -- partial refunds must not both pass the "is there room?" check.
       COALESCE(SUM(r.amount_cents) FILTER (WHERE r.status IN ('succeeded', 'pending')), 0) AS committed_cents
FROM charges c
LEFT JOIN refunds r ON r.charge_id = c.id
GROUP BY c.id, c.amount_cents;
