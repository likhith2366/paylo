# PayFlow

A payment gateway built around two problems: **processing a charge exactly
once**, and keeping a **financially correct double-entry ledger** while doing
it.

Go monorepo, Postgres, Redis, Redpanda, LocalStack. Runs entirely on a laptop.

```
100 concurrent requests · 1 idempotency key → 1 bank authorization, 1 charge
```

That line is the point of the project. Everything else exists to make it true
under real conditions.

---

## What this is, and what it is not

PayFlow does what Stripe does for the businesses built on it: a merchant
integrates one API and one webhook endpoint, and gets charges, refunds,
disputes, fraud screening and payouts.

**It is not a licensed money transmitter and does not move real money.** The
acquiring bank is a simulator I wrote. That is the one piece that is
structurally impossible to build for real, so it is built as production
software with deterministic test hooks rather than faked with a random number
(§25.2).

The line I drew: anything that is *an API or an algorithm* is built for real,
even where a shortcut existed. Anything requiring a **contract with a human
institution** — sponsor bank, KYC vendor, PCI assessor — is stubbed and
documented as such.

Full design: [`docs/payment-gateway-system-design_updated.md`](docs/payment-gateway-system-design_updated.md).
Section refs in code comments (§4, §14.3) point there.

---

## Run it

```bash
make up                    # 11 services
make seed                  # prints an sk_test_... key, shown once
make test                  # unit + integration, needs Docker
```

Ports: payments `8080`, vault `8081`, fraud scoring `8000`, bank simulator
`8090`, Mailhog UI `8025`.

```bash
# The Payments API has no field that can carry a card number.
# Get a token from the vault first.
TOKEN=$(curl -s -X POST localhost:8081/vault/tokenize \
  -H 'Content-Type: application/json' \
  -d '{"number":"4242424242424242","exp_month":12,"exp_year":2030}' \
  | python -c 'import sys,json;print(json.load(sys.stdin)["token"])')

curl -X POST localhost:8080/v1/charges \
  -H "Authorization: Bearer $PAYLO_KEY" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H 'Content-Type: application/json' \
  -d "{\"amount\":4500,\"currency\":\"USD\",\"payment_token\":\"$TOKEN\"}"
```

Send the same `Idempotency-Key` twice and the second response is the first
charge, byte for byte, with no second authorization.

---

## The four decisions worth defending

### 1. Postgres is the lock, not Redis

Exactly-once is built on `INSERT ... ON CONFLICT DO NOTHING` against a
`UNIQUE (merchant_id, idempotency_key)`. One caller gets the row; everyone else
falls through. There is no window between checking and claiming, so there is
nothing to race.

There is no Redlock here on purpose. A distributed lock on Redis is hard to
make correct and is not something to bet money on — and the database that
*already has to be in the transaction* for the ledger write does the job with
stronger guarantees. Redis is a read-through cache for replays, nothing more.

The claim, the ledger write and the outbox row commit in **one** `pgx.Tx`.
Splitting them allows a completed charge with no ledger entry, which is
unrecoverable.

### 2. The ledger is enforced by the database, not by discipline

Debits equal credits, per transaction, per currency — checked three times, by
code that shares nothing:

- in Go, before the write
- by a `DEFERRABLE INITIALLY DEFERRED` constraint trigger at COMMIT
- by reconciliation, re-deriving from the entries themselves

Deferred is what makes the second one usable: legs are inserted as separate
statements and validated as a unit. A separate trigger rejects `UPDATE` and
`DELETE` on `ledger_entries` — append-only is enforced, not documented.
Corrections are new balanced entries referencing the original.

A bug in Go cannot corrupt the ledger.

### 3. Ambiguous is not failed

If the processor call times out, the money may have moved. The charge is
marked `requires_reconciliation`, **no ledger entries are written**, and the
hourly job asks the processor afterwards and posts what it finds.

Never blind-retry, never assume failure. The same rule applies to refunds and
to ACH transfers — an ambiguous payout keeps the funds in `in_transit` rather
than returning them, because returning them while the transfer may be underway
pays the merchant twice.

Reconciliation **records discrepancies and alerts; it never auto-corrects.**
Silently fixing a financial mismatch buries the bug instead of finding it.

### 4. A raw card number exists in exactly one service

The Payments API, ledger, dashboard and every log line see an opaque token,
`card_last4` and `card_bin`. The card itself is typed into an iframe served
from the vault's origin, so the merchant's own JavaScript cannot read it —
that browser boundary *is* the PCI-scope firewall (§2.4).

The type system enforces it: `CardMetadata` has no number field. And the
charge endpoint scans bodies for a Luhn-valid digit run and rejects them, so a
merchant cannot drag themselves into PCI scope by mistake.

Verified rather than claimed — searching every text and JSONB column in every
table for a test PAN returns nothing.

---

## Layout

```
cmd/
  payments-api      public REST API
  vault             tokenization — the only service that sees a PAN
  bank-simulator    the acquiring bank, with deterministic test hooks
  webhook-worker    outbox poller + delivery with retry/backoff/DLQ
  scheduler         reconciliation, payouts
internal/
  money             integer minor units — never a float
  ledger            double-entry posting, balances
  idempotency       exactly-once claim and replay
  payments          charge, refund, dispute flows
  vault             AES-256-GCM envelope encryption, hosted iframe
  risk              rule engine, velocity counters, ML client
  reconcile         resolves ambiguity, re-derives invariants
  payouts           money out, T+2 hold, dispute reserves
  webhook           HMAC signing, at-least-once delivery
ml/                 fraud model: data, features, training, serving
migrations/         numbered SQL, applied in order
```

---

## Fraud detection

Two layers, and the ordering matters.

**Rules first** (`internal/risk`) — 17 rules at **422ns** per transaction,
each explainable to a customer or a regulator. Velocity, geo, BIN risk,
disposable email, and behavioral biometrics from the checkout iframe.

The behavioral rules are the interesting ones. We own the iframe, so we can see
*how* a card was entered. The strongest signal is not speed — it is **paste**:
a cardholder types the card in their hand, a fraudster pastes from a list.
Paste alone deliberately does not block (password managers paste); paste plus
machine-speed entry does.

**Model second** — XGBoost, trained on IEEE-CIS (real Vesta transactions).
It can raise a verdict but never clear a rule block, and it is **never a hard
dependency**: a slow or dead scorer falls open to rules behind a circuit
breaker. Failing closed would turn a model outage into a total outage.

**Test PR-AUC 0.1880.** Low, and honestly so —
[`ml/MODEL_CARD.md`](ml/MODEL_CARD.md) explains why, including two mistakes
worth reading:

- A first model scored **0.97** on synthetic data whose generator gave fraud
  category-bounded amounts. It had reverse-engineered a simulator.
- A second scored 0.97 offline and **0.29 in production**, because the Go
  client sent 6 of 18 features and the rest defaulted silently.

The deployed model uses only features the charge path can actually produce, and
the service refuses to start if its column list disagrees with the trained
model.

---

## Testing

Integration tests run against real Postgres via testcontainers, never mocks.
Idempotency and ledger correctness depend on unique constraints, row locks,
deferred triggers and `ON CONFLICT` semantics that a mock does not reproduce —
a mocked suite passes while the system double-charges people.

```bash
make test              # all of it
make test-idempotency  # just the concurrency proof
make test-race         # under the race detector
k6 run -e API_KEY=sk_test_... test/load/idempotency.js
```

Every money path has a concurrency test, not just a happy-path one:

| Test | Asserts |
|---|---|
| 100 concurrent, one key | 1 bank authorization |
| 10 concurrent refunds of 2000 on a 10000 charge | exactly 5 succeed |
| 20 concurrent chargeback redeliveries | funds reversed once |
| 30 concurrent detokenizations of a single-use token | exactly 1 succeeds |
| 50 concurrent ledger writes to one account | no lost updates |

---

## Bugs this codebase has already paid for

Kept because the failures are more instructive than the design:

**A refund left in `requires_reconciliation` released its reservation.** A
second refund under a different key could take a 10000 charge to 20000. The
ledger stayed balanced throughout — only the second refund posted entries — so
no balance assertion could have caught it.

**A test double more permissive than the real thing.** `fakeVault` accepted
consumed tokens while the real vault rejected them, so the suite stayed green
over an idempotency bug that broke on the first real retry.

**A row-ordering bug in feature engineering.** Sorting by card and returning
rows in that order, while the caller paired them with labels in the original
order, attached every label to a different transaction. The model trained
cleanly and scored at chance.

**Velocity counters that slid their window forever.** An unconditional
`EXPIRE` instead of `EXPIRE NX` meant a device used daily never expired and
accumulated cards without bound — eventually blocking a shared family tablet
permanently.

Migration `0005_invariant_hardening` turns several of these from conventions
into database constraints.

---

## Deliberately not built

Per §0.1, and documented rather than silently missing:

| | |
|---|---|
| Sponsor bank, money transmitter licence | Not a technical problem |
| KYC / identity verification | Stubbed `always_approved` in test mode |
| Sanctions/AML screening, PCI QSA audit | Needs vendor contracts |
| Tax calculation and remittance | Avalara-shaped, out of scope |
| 3DS step-up (§16) | Designed; `requires_action` exists in the idempotency state machine, flow not built |
| Merchant dashboard | Not built |

---

## Scaling past this

The design doc covers this in depth; the short version of what changes at 10M
users:

- **Sharding by `merchant_id`** — merchants do not share financial state, so
  cross-shard queries are rare. Trigger: sustained write IOPS above ~70%.
- **PgBouncer in transaction mode** — hundreds of pods exhaust
  `max_connections` long before they exhaust the database.
- **Read replicas for reporting** — analytics queries must never touch the
  primary.
- **Redpanda → MSK, Postgres → Aurora** — both are configuration changes here,
  which is the point of building against the real AWS SDK via LocalStack.

Zero-downtime migrations follow expand/contract: additive changes, batched
backfills, drop only once all readers have moved.

---

## License

MIT
