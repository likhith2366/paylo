DROP TRIGGER IF EXISTS trg_charges_terminal_is_final ON charges;
DROP FUNCTION IF EXISTS charges_terminal_is_final();
ALTER TABLE disputes ALTER COLUMN processor_reference DROP NOT NULL;
DROP INDEX IF EXISTS uq_refunds_merchant_idempotency;
DROP INDEX IF EXISTS uq_charges_merchant_idempotency;

CREATE OR REPLACE VIEW charge_refund_totals AS
SELECT c.id AS charge_id,
       c.amount_cents,
       COALESCE(SUM(r.amount_cents) FILTER (WHERE r.status = 'succeeded'), 0) AS refunded_cents,
       COALESCE(SUM(r.amount_cents) FILTER (WHERE r.status IN ('succeeded', 'pending')), 0) AS committed_cents
FROM charges c
LEFT JOIN refunds r ON r.charge_id = c.id
GROUP BY c.id, c.amount_cents;
