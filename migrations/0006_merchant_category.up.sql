-- Merchant category for fraud scoring (§14.3).
--
-- The audit measured category_encoded as the second most important feature
-- (permutation importance 0.463), and the Go client could not send it because
-- nothing stored it. It arrived at the model as -1 on every single charge.
--
-- Values match the training data's vocabulary so the encoding learned during
-- training applies directly. Real systems use ISO 18245 MCC codes; these are
-- the coarser buckets Sparkov uses, and mapping MCC -> bucket is the migration
-- path if real merchant data arrives later.
ALTER TABLE merchants
    ADD COLUMN category TEXT NOT NULL DEFAULT 'misc_pos';

COMMENT ON COLUMN merchants.category IS
    'Business category, matching the fraud model vocabulary. Feeds category_encoded.';

-- Rolling average charge size, refreshed periodically by the scheduler.
-- The amount_anomaly rule already reads this; it was always zero because
-- nothing ever computed it, so the rule silently never fired.
ALTER TABLE merchants
    ADD COLUMN avg_amount_cents BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN avg_amount_updated_at TIMESTAMPTZ;
