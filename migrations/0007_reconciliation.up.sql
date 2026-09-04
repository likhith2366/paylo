-- Reconciliation (§24.3).
--
-- Two jobs. Resolve charges and refunds parked as requires_reconciliation by
-- asking the processor what actually happened, and independently re-derive the
-- ledger invariants to catch anything the write path missed.
--
-- Discrepancies are RECORDED AND ALERTED, never auto-corrected. Silently
-- "fixing" a financial mismatch is how a real accounting bug gets buried
-- instead of found.

CREATE TABLE reconciliation_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    charges_checked INT NOT NULL DEFAULT 0,
    charges_resolved INT NOT NULL DEFAULT 0,
    refunds_checked INT NOT NULL DEFAULT 0,
    refunds_resolved INT NOT NULL DEFAULT 0,
    discrepancies   INT NOT NULL DEFAULT 0,
    error           TEXT
);

CREATE TABLE reconciliation_discrepancies (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        UUID NOT NULL REFERENCES reconciliation_runs(id),
    kind          TEXT NOT NULL CHECK (kind IN (
                      'unbalanced_transaction',   -- debits != credits
                      'balance_drift',            -- cached balance != derived
                      'processor_mismatch',       -- our state != theirs
                      'orphaned_charge')),        -- stuck with no processor record
    subject_type  TEXT NOT NULL,
    subject_id    TEXT NOT NULL,
    detail        JSONB NOT NULL,

    -- Resolution is a human action. Nothing here clears itself.
    resolved_at   TIMESTAMPTZ,
    resolved_by   TEXT,
    resolution_note TEXT,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_discrepancies_open ON reconciliation_discrepancies (created_at)
    WHERE resolved_at IS NULL;
