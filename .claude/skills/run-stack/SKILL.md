---
name: run-stack
description: Bring up the local PayFlow stack (Postgres, Redis, Redpanda, LocalStack, Mailhog, bank simulator, payments API), seed a test merchant with an API key, and smoke-test a charge end to end. Use when asked to run the app, start the stack, demo a charge, or check that the system works locally.
---

# Running the local stack

Everything runs on the laptop — no AWS account. LocalStack backs the AWS SDK
calls so the integration code under test is the same code that runs in
production (§25.3).

## Prerequisites

Docker Desktop must be running. Check before anything else — a stopped daemon
is the most common cause of a confusing failure here:

```bash
docker info --format '{{.ServerVersion}}'
```

On Windows, launch it with:

```powershell
Start-Process "$env:ProgramFiles\Docker\Docker\Docker Desktop.exe"
```

It takes 30-60 seconds to accept connections after launch.

## Bring it up

```bash
make up          # docker compose up -d --build
make ps          # service health
make logs        # follow everything
```

Wait for health, don't guess:

```bash
docker compose ps --format 'table {{.Service}}\t{{.Status}}'
```

Ports: payments API `8080`, bank simulator `8090`, Postgres `5432`, Redis
`6379`, Redpanda `9092`, LocalStack `4566`, Mailhog UI `8025`.

Migrations apply automatically on first boot of an empty `pgdata` volume. If
you changed a migration, the volume already exists and it will NOT re-run —
`make reset` (destroys data) or apply it by hand.

## Seed a merchant

```bash
make seed
```

Prints a merchant ID and a raw `sk_test_...` key. That key is shown once —
only its hash is stored (§8). Capture it.

## Smoke test

```bash
export PAYLO_KEY=sk_test_...

curl -s localhost:8080/healthz
curl -s localhost:8090/healthz

curl -s -X POST localhost:8080/v1/charges \
  -H "Authorization: Bearer $PAYLO_KEY" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{"amount":5000,"currency":"USD","payment_token":"tok_..."}'
```

A token comes from the Vault, not from a raw card number — the Payments API
rejects PANs by design (§2.4). Mint one first:

```bash
curl -s -X POST localhost:8081/vault/tokenize \
  -H "Content-Type: application/json" \
  -d '{"number":"4242424242424242","exp_month":12,"exp_year":2030,"cvc":"123"}'
```

## Driving specific outcomes

Test-mode keys may set `X-Simulate-Outcome` to steer the bank simulator
(§25.2). Live keys cannot — the header is ignored.

`success` · `decline` · `timeout` · `delayed_success` · `network_error`

```bash
curl -s -X POST localhost:8080/v1/charges \
  -H "Authorization: Bearer $PAYLO_KEY" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "X-Simulate-Outcome: timeout" \
  -H "Content-Type: application/json" \
  -d '{"amount":5000,"currency":"USD","payment_token":"tok_..."}'
```

`timeout` returns `202 requires_reconciliation` — the charge authorized on the
simulator's side but the response never arrived. That is correct behaviour, not
a failure.

## Demonstrating idempotency

The point of the whole system. Same key, twice:

```bash
KEY=$(uuidgen)
for i in 1 2; do
  curl -s -X POST localhost:8080/v1/charges \
    -H "Authorization: Bearer $PAYLO_KEY" \
    -H "Idempotency-Key: $KEY" \
    -H "Content-Type: application/json" \
    -d '{"amount":5000,"currency":"USD","payment_token":"tok_..."}' | jq -r '.id'
done
```

Both print the same charge ID. The second response carries
`Idempotent-Replay: true`.

## Inspecting state

```bash
make psql

-- ledger must balance, per transaction, per currency
SELECT transaction_id, currency,
       SUM(CASE WHEN direction='debit' THEN amount_cents ELSE -amount_cents END) AS delta
FROM ledger_entries GROUP BY 1,2 HAVING SUM(...) <> 0;
-- any row here is a P1

SELECT id, status, amount_cents, currency FROM charges ORDER BY created_at DESC LIMIT 10;
SELECT event_type, published, count(*) FROM outbox_events GROUP BY 1,2;
```

Mailhog inbox: http://localhost:8025

## Teardown

```bash
make down     # stop, keep data
make reset    # stop and destroy volumes — migrations re-run on next up
```

## When something is wrong

- **API up but every charge 500s** — check `docker compose logs payments-api`;
  usually the DB isn't reachable or migrations didn't apply.
- **`readyz` returns 503** — Postgres is down or still starting. `healthz`
  stays 200 by design, so the pod isn't killed while a dependency recovers.
- **Migration changes not applied** — the `pgdata` volume already existed.
  `make reset`.
- **Port already allocated** — something else holds 5432 or 8080. `docker ps`.
