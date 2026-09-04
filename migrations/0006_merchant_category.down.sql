ALTER TABLE merchants
    DROP COLUMN IF EXISTS avg_amount_updated_at,
    DROP COLUMN IF EXISTS avg_amount_cents,
    DROP COLUMN IF EXISTS category;
