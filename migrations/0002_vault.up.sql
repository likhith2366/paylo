-- Card Vault (§2.4).
--
-- In production these tables live in a SEPARATE database, in a separate VPC
-- subnet, reachable only by the Vault service — not by the Payments API, the
-- Ledger, or the dashboard. Keeping them in the same Postgres instance here is
-- a local-development convenience only; the isolation that actually reduces
-- PCI scope is network- and IAM-level, and is documented in the Terraform.
--
-- The invariant that makes the whole scheme work: no other table in this
-- system has a column that could hold a PAN, in any form.

CREATE TABLE vault_tokens (
    -- The opaque token handed back to the browser, e.g. tok_1a2b3c4d.
    -- Random, not derived from the card, so it leaks nothing even if logged.
    token             TEXT PRIMARY KEY,

    -- AES-256-GCM ciphertext of the PAN. The nonce is prepended to the
    -- ciphertext rather than stored separately — it must be unique per
    -- encryption but is not secret, and keeping them together makes it
    -- impossible to pair the wrong nonce with the wrong ciphertext.
    pan_ciphertext    BYTEA       NOT NULL,

    -- Envelope encryption: the per-record data key, itself encrypted under the
    -- KMS master key. Rotating the master key then means re-wrapping these
    -- small blobs rather than re-encrypting every card.
    encrypted_data_key BYTEA      NOT NULL,
    key_id            TEXT        NOT NULL,

    -- Non-sensitive metadata, safe to return to the Payments API so it can
    -- display and risk-score a card without ever seeing the number (§14.5).
    card_brand        TEXT        NOT NULL,
    card_last4        CHAR(4)     NOT NULL,
    card_bin          CHAR(6)     NOT NULL,
    card_fingerprint  TEXT        NOT NULL,
    exp_month         INT         NOT NULL CHECK (exp_month BETWEEN 1 AND 12),
    exp_year          INT         NOT NULL,

    merchant_id       UUID,

    -- Single-use tokens are consumed by the first charge that presents them,
    -- so a leaked token from a browser session cannot be replayed. Tokens for
    -- saved cards ("keep this card on file") are multi-use and long-lived.
    single_use        BOOLEAN     NOT NULL DEFAULT true,
    consumed_at       TIMESTAMPTZ,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Short by default: a checkout token only needs to survive the few minutes
    -- between typing the card and submitting the order.
    expires_at        TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '15 minutes'
);

CREATE INDEX idx_vault_expiry ON vault_tokens (expires_at);
CREATE INDEX idx_vault_fingerprint ON vault_tokens (card_fingerprint);

-- Every access to a PAN is recorded. In a real vault this is the audit trail an
-- assessor asks for first: who detokenized what, when, and why.
CREATE TABLE vault_access_log (
    id          BIGSERIAL PRIMARY KEY,
    token       TEXT        NOT NULL,
    operation   TEXT        NOT NULL CHECK (operation IN ('tokenize', 'detokenize', 'expire')),
    caller      TEXT        NOT NULL,
    reason      TEXT,
    succeeded   BOOLEAN     NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_vault_access_token ON vault_access_log (token, created_at DESC);

-- The Payments API references a card only by token from here on.
ALTER TABLE charges ADD COLUMN payment_token TEXT;
