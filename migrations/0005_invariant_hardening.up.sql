-- Turns four invariants that were enforced only by convention into constraints
-- the database enforces. Each closes a bug found in architecture review.

-- ---------------------------------------------------------------------------
-- 1. An ambiguous refund must keep reserving its amount (CRITICAL)
-- ---------------------------------------------------------------------------
-- The previous filter counted only 'succeeded' and 'pending' toward the
-- committed total, so a refund left in 'requires_reconciliation' — meaning the
-- processor call timed out and THE MONEY MAY HAVE MOVED — released its
-- reservation. A second full refund under a different idempotency key then
-- passed the remaining-amount check, and a 10000 charge could be refunded
-- 20000.
--
-- The ledger stayed balanced throughout (only the second refund posted legs),
-- so the balance invariant did not catch it. Only 'failed' — where we know the
-- request provably never landed — releases capacity now.

CREATE OR REPLACE VIEW charge_refund_totals AS
SELECT c.id AS charge_id,
       c.amount_cents,
       COALESCE(SUM(r.amount_cents) FILTER (WHERE r.status = 'succeeded'), 0) AS refunded_cents,
       -- Reserve unless proven not to have moved. 'pending' is in flight,
       -- 'requires_reconciliation' is unknown; both must hold their amount.
       COALESCE(SUM(r.amount_cents) FILTER (WHERE r.status <> 'failed'), 0) AS committed_cents
FROM charges c
LEFT JOIN refunds r ON r.charge_id = c.id
GROUP BY c.id, c.amount_cents;

-- ---------------------------------------------------------------------------
-- 2. One charge per idempotency key, enforced (HIGH)
-- ---------------------------------------------------------------------------
-- The stale-lock steal path (§4.2) mints a fresh charge UUID, so a stalled
-- attempt and the worker that steals its lock could each insert a charge row
-- for one logical request. Nothing prevented it: the ON CONFLICT (id) guard in
-- the insert is dead code, because a fresh UUID never conflicts.
--
-- Partial, because internal charges may legitimately have no key.
CREATE UNIQUE INDEX uq_charges_merchant_idempotency
    ON charges (merchant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Same reasoning for refunds.
CREATE UNIQUE INDEX uq_refunds_merchant_idempotency
    ON refunds (merchant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 3. A dispute's processor reference must exist (HIGH)
-- ---------------------------------------------------------------------------
-- The UNIQUE constraint on processor_reference was the only thing deduplicating
-- redelivered chargeback notifications. Postgres treats every NULL as distinct,
-- so a notification arriving twice with no reference inserted twice and
-- reversed the funds twice — debiting the merchant 23000 for one 10000
-- chargeback, with both reversals individually balanced.
UPDATE disputes
SET processor_reference = 'legacy_' || id::text
WHERE processor_reference IS NULL;

ALTER TABLE disputes ALTER COLUMN processor_reference SET NOT NULL;

-- ---------------------------------------------------------------------------
-- 4. A terminal charge stays terminal (HIGH)
-- ---------------------------------------------------------------------------
-- Nothing stopped a charge moving from 'succeeded' back to 'failed' or being
-- finalized twice. Mirrors the ledger's append-only trigger: make the illegal
-- write impossible rather than merely unreached.
CREATE OR REPLACE FUNCTION charges_terminal_is_final() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('succeeded', 'failed') AND NEW.status <> OLD.status THEN
        RAISE EXCEPTION
            'charge % is already terminal (%), cannot transition to %',
            OLD.id, OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_charges_terminal_is_final
    BEFORE UPDATE OF status ON charges
    FOR EACH ROW EXECUTE FUNCTION charges_terminal_is_final();
