# PayFlow (paylo)

A payment gateway built around exactly-once processing and a financially
correct double-entry ledger. Go monorepo, Postgres, Redis, Redpanda, LocalStack.

Design spec: `docs/payment-gateway-system-design_updated.md`. Section refs in
code comments (§4, §14.3) point there. The `_updated` file is authoritative —
the other is the earlier draft.

---

## Working principles

Adapted from Andrej Karpathy's observations on where LLM coding goes wrong.
These override the instinct to be helpful by doing more.

### 1. Think before coding

Don't assume. Don't hide confusion. Surface tradeoffs.

If a request has multiple readings, say so and pick one explicitly — don't
choose silently. If you're uncertain whether something is a bug or intended,
ask. In this codebase specifically: if you can't tell whether a change affects
money movement, stop and ask rather than guessing.

### 2. Simplicity first

The minimum code that solves the problem. Nothing speculative.

No abstractions for single-use code. No configurability that wasn't asked for.
No interface with one implementation "in case we need another later" — the one
exception already in the tree is `vault.KeyManager`, which exists because the
local and KMS implementations are both real and both needed.

Ask: would a senior engineer call this overcomplicated? If yes, cut it.

### 3. Surgical changes

Every changed line should trace to the request. Touch only what you must.

Match the surrounding style. Don't reformat, don't rename, don't "improve"
adjacent code you happened to read. Clean up only the mess your own change
made. If you spot an unrelated problem, mention it — don't fix it in the same
change.

### 4. Goal-driven execution

Define what "done" looks like before you start, in terms that can be checked.

For anything touching payments, "done" means a test proves it. `go build`
passing is not verification. The success criterion for a money-path change is
a test that fails without the fix and passes with it.

---

## Non-negotiables

These are correctness rules, not style preferences. Violating one is a bug
even if the code compiles and the tests pass.

**Money is never a float.** `int64` minor units via `internal/money`. If you
find yourself writing `float64` near an amount, you've made a mistake.

**The ledger is append-only.** Corrections are new balanced entries that
reference the original transaction — never an UPDATE. A database trigger
enforces this; if you hit that error, the fix is to write a reversal, not to
drop the trigger.

**Debits equal credits, per currency, per transaction.** Enforced in Go, by a
deferred Postgres constraint at COMMIT, and by the reconciliation query. Don't
remove a layer because the other two exist.

**The idempotency record and the business write commit together.** Same
`pgx.Tx`, always. Splitting them allows a completed charge with no ledger
entry, which is unrecoverable.

**Raw PANs exist only in the Vault.** The Payments API, Ledger, dashboard, and
every log line see an opaque token, `card_last4`, and `card_bin` — never a full
number. Adding a column or a log field that could hold a PAN expands PCI scope
and is a blocking review failure.

**Ambiguous is not failed.** If a downstream call times out, the money may have
moved. Mark the charge `requires_reconciliation` and let reconciliation resolve
it against the processor's log. Never blind-retry, never assume failure.

**Time comes from Postgres.** Use `now()` for anything with financial or
security meaning (expiry, locks, deadlines). Pod clocks drift.

---

## Layout

```
cmd/            service entrypoints, one binary each
internal/
  money/        integer minor-unit amounts
  ledger/       double-entry posting, balances
  idempotency/  exactly-once claim/replay
  payments/     charge flow, card rules, bank client
  vault/        PAN encryption + tokenization (§2.4)
  db/           pool and transaction helpers
  httpx/        error envelope, auth, tracing middleware
  testsupport/  testcontainers harness
migrations/     numbered SQL, applied in filename order
ml/             fraud model: data, features, training, serving
```

## Commands

```
make test           unit + integration (needs Docker running)
make test-short     unit only, no containers
make up             full local stack
make lint           vet + staticcheck
```

## Testing

Integration tests run against real Postgres via testcontainers, not mocks.
This is deliberate: idempotency and ledger correctness depend on unique
constraints, row locks, deferred triggers, and `ON CONFLICT` semantics that a
mock doesn't reproduce. A mocked suite here passes while the system
double-charges people.

New money-path code needs a concurrency test, not just a happy-path one. The
model is `TestConcurrentDuplicateIdempotencyKey` — fire N simultaneous
operations, assert the invariant held exactly once.

## Conventions

- Errors wrap with `%w` and name the operation: `fmt.Errorf("ledger: post entry: %w", err)`
- `log/slog`, structured, always with `trace_id`. Never log a PAN, CVV, full API key, or token secret — key prefixes only.
- Comments explain *why*. The code already says what.
- Public API response shapes are a frozen contract once released (§22.2). Add fields; never change or remove one.
