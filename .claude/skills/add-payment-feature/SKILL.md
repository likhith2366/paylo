---
name: add-payment-feature
description: Build a new money-moving feature in PayFlow (refunds, disputes, payouts, subscriptions, FX) following the established charge-flow pattern. Use when adding an endpoint or worker that creates, reverses, or transfers money.
---

# Adding a money-moving feature

Refunds, disputes, payouts, and subscription renewals are all the same shape as
the charge flow. Follow it rather than inventing a new structure — the pattern
exists because each piece of it prevents a specific, expensive failure.

## The pattern

Read `internal/payments/service.go` `CreateCharge` first. The sequencing is the
point:

```
tx1   claim the idempotency key + record intent, commit
      → a crash mid-flight leaves a durable record to reconcile against

      call the external service OUTSIDE any transaction
      → a network call inside a transaction holds row locks for the
        duration of someone else's latency; under load that alone
        exhausts the connection pool

tx2   ledger entries + outbox row + status + idempotency response,
      ALL in one transaction
      → money moving and the event announcing it cannot diverge
```

## Checklist

**Idempotency.** Every mutating endpoint requires an `Idempotency-Key`. A
retried refund without one refunds twice. Use `idempotency.Begin` /
`idempotency.Complete`, and make sure `Complete` shares a transaction with the
ledger write.

**Ledger legs that balance.** Per currency, per transaction. Decide the
accounts first and write them down before coding:

```
refund:   debit merchant_balance, credit customer_receivable
dispute:  debit merchant_balance, credit customer_receivable  (+ dispute fee)
payout:   debit merchant_balance, credit in_transit
          then credit paid_out when the bank confirms
```

Reversals are **new balanced entries referencing the original
`transaction_id`** — never an UPDATE of the original. The database will reject
the UPDATE anyway.

**Guard against over-reversal.** `SUM(refunds) <= original_charge_amount` is a
check-then-act race if written as a SELECT followed by an INSERT. Two
concurrent partial refunds both pass and together exceed the charge. Use
`SELECT ... FOR UPDATE` on the charge row, or a single conditional statement
whose `rows_affected` you check.

**Outbox event** in the same transaction, so the webhook fires even if the
broker was down.

**Ambiguous outcomes** get `requires_reconciliation`, no ledger entries, and no
automatic retry.

**Negative balances are real.** Refunds and disputes can push a merchant
negative (§19). Don't assume balances are ≥ 0 — decide explicitly whether the
operation is permitted and record `merchant_debt` if it proceeds.

## Tests required before this is done

Happy path alone doesn't demonstrate correctness for money.

1. **Concurrency** — N simultaneous operations with one idempotency key produce
   exactly one effect. Model: `TestConcurrentDuplicateIdempotencyKey`.
2. **Over-reversal race** — two concurrent partial refunds that together exceed
   the charge; assert only the valid one succeeds.
3. **Ledger balance** — assert the transaction sums to zero and the specific
   per-account amounts are right, not just that entries exist.
4. **Ambiguous path** — assert `requires_reconciliation` and **zero** ledger
   entries.
5. **Outbox** — assert the event row exists, unpublished.

All against real Postgres via `internal/testsupport`, not mocks. Unique
constraints, row locks, and deferred triggers are exactly what a mock doesn't
reproduce, and exactly what these features depend on.

## Before calling it finished

```bash
go test ./internal/... -timeout 20m
```

Then have the `money-auditor` agent review the diff. It checks the invariants
that compile and pass tests but still lose money.
