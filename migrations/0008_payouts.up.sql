-- Payouts (§18) — money leaving for the merchant's bank.
--
-- The half of the system that was missing: everything so far models money
-- coming in. A payout is the merchant actually getting paid.

CREATE TABLE payout_accounts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id   UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
    -- Bank details are as sensitive as card data. Only the last four are
    -- stored in the clear, exactly as with PANs (§2.4); the full account
    -- number belongs in the vault.
    account_last4 CHAR(4)     NOT NULL,
    routing_last4 CHAR(4)     NOT NULL,
    account_token TEXT        NOT NULL,
    currency      CHAR(3)     NOT NULL DEFAULT 'USD',
    verified_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, currency)
);

CREATE TABLE payouts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id     UUID        NOT NULL REFERENCES merchants(id),
    payout_account_id UUID      NOT NULL REFERENCES payout_accounts(id),
    amount_cents    BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency        CHAR(3)     NOT NULL,

    status          TEXT        NOT NULL CHECK (status IN (
                        'pending',     -- ledger posted, ACH initiated
                        'paid',        -- bank confirmed
                        'failed',      -- bank rejected, funds returned
                        'requires_reconciliation')),
    failure_code    TEXT,

    -- The window this payout settles. Charges after period_end belong to the
    -- next run, which is what makes a re-run produce the same batch.
    period_end      TIMESTAMPTZ NOT NULL,

    ledger_transaction_id  UUID,
    -- Posted separately when the bank confirms: in_transit -> paid_out.
    settlement_transaction_id UUID,
    processor_reference TEXT,

    initiated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One payout per merchant per period. This is what makes the batch job
    -- safe to re-run after a crash — a second attempt at the same window
    -- conflicts instead of paying twice.
    UNIQUE (merchant_id, currency, period_end)
);
CREATE INDEX idx_payouts_merchant ON payouts (merchant_id, created_at DESC);
CREATE INDEX idx_payouts_pending ON payouts (initiated_at) WHERE status = 'pending';
