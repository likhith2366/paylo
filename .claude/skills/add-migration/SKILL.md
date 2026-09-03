---
name: add-migration
description: Write a database migration for PayFlow following the expand/contract discipline required for zero-downtime schema changes on a live payments system. Use when adding a table or column, changing a constraint, or backfilling data.
---

# Writing a migration

Migrations live in `migrations/`, numbered and paired:

```
0003_disputes.up.sql
0003_disputes.down.sql
```

They apply in filename order. Postgres runs them automatically on first boot of
an empty volume; `internal/testsupport` applies them for every integration test.

## The rule that governs everything here

You cannot take a payments database offline to `ALTER TABLE` (§22.2). Every
migration must be safe to apply while the previous version of the code is still
running and serving traffic. That forces **expand/contract**:

1. **Expand** — add the new thing. Nullable columns, new tables, new indexes.
   Old code ignores it; new code can use it.
2. **Backfill** — populate in batches, never one statement over millions of
   rows (it holds a lock and blocks writes).
3. **Migrate readers** — deploy code that reads the new shape.
4. **Contract** — only once nothing reads the old shape, drop it. A separate,
   later migration.

A migration that adds a `NOT NULL` column with no default to a populated table
is a locking rewrite and an outage. Add it nullable, backfill, then add the
constraint with `NOT VALID` followed by `VALIDATE CONSTRAINT`.

## Rules for this schema specifically

**Money columns are `BIGINT` minor units** with `CHECK (amount_cents > 0)`.
Never `NUMERIC`, never `FLOAT`. Currency is a separate `CHAR(3)`.

**Timestamps are `TIMESTAMPTZ`** and default to `now()`. Never `TIMESTAMP`
without a zone.

**Nothing outside `vault_tokens` may hold a PAN.** A column that could store a
full card number expands PCI scope. `card_last4 CHAR(4)`, `card_bin CHAR(6)`,
and a `card_fingerprint` are the permitted representations.

**Don't add UPDATE or DELETE paths to `ledger_entries`.** The immutability
trigger will reject them, and that is the intended behaviour.

**Index partially where the query is partial.** The outbox poller only reads
unpublished rows, so:

```sql
CREATE INDEX idx_outbox_unpublished ON outbox_events (id) WHERE NOT published;
```

The index stays small as the table grows into millions of published rows.

**`CREATE INDEX CONCURRENTLY` in production**, which cannot run inside a
transaction block. Note it in a comment if the migration runner wraps
statements.

## Comment the why

Every non-obvious choice gets a comment naming the design-doc section:

```sql
-- DEFERRABLE INITIALLY DEFERRED so the check runs at COMMIT: a transaction
-- inserts its debit and credit legs as separate statements and must be
-- validated as a unit (§5).
```

## Verifying

The down migration must actually reverse the up — drop triggers and functions,
not just tables:

```bash
make reset && make up          # up migrations apply cleanly from scratch
go test ./internal/ledger/     # schema-dependent tests still pass
```

Test the new constraint by trying to violate it. A constraint nothing has
attempted to break is a constraint you haven't verified:

```go
if _, err := pool.Exec(ctx, `INSERT ... deliberately bad row`); err == nil {
    t.Error("expected the constraint to reject this")
}
```
