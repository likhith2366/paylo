# PayFlow — Idempotent Payment Gateway at Scale
### System Design Document (target: ~10M users, high-throughput, PCI-adjacent)

---

## 0. Product Overview: What We're Building & Who It's For

**What PayFlow is:** a payment processing platform that businesses (merchants) integrate into their own product to accept payments from *their* customers — the same role Stripe plays for the businesses built on top of it. PayFlow itself has no end-consumer-facing app; its customers are developers/businesses, and its "users" in the traditional sense are those businesses' account holders on the merchant dashboard.

**What it delivers, concretely:**

| Capability | What a merchant gets |
|---|---|
| **Payments API** | Accept a card charge from their own checkout page via a simple API call, with guaranteed exactly-once processing even if their frontend retries on a flaky connection |
| **Ledger & reporting** | An accurate, auditable record of every transaction, refund, and dispute — "what's my actual balance right now" is always a correct, instant answer |
| **Webhooks** | Real-time, reliable notification when a payment succeeds, fails, or gets disputed — so the merchant's own system (e.g., "mark order as paid," "ship the item") reacts automatically |
| **Subscriptions** | Recurring billing without the merchant having to build renewal logic, retry-on-failed-card logic, or proration themselves |
| **Fraud protection** | Charges are risk-scored automatically — a small merchant gets fraud tooling they could never build or afford in-house |
| **Payouts** | Money collected from their customers actually lands in their bank account on a predictable schedule, without them touching banking infrastructure directly |
| **Dashboard** | A place to see transactions, disputes, and payout history without writing a single query |

**Primary use case:** a developer building an online store, SaaS product, or marketplace who wants to accept payments **without becoming a payments company themselves** — they integrate one API and one webhook endpoint, and everything from fraud screening to bank settlement is handled for them. This is the core value proposition of the entire category (Stripe/Adyen/Braintree/etc.): payments are hard and regulated, so most businesses would rather rent this capability than build it.

**Secondary use case (marketplaces/platforms):** a business that itself has multiple sub-sellers (e.g., an Etsy-style marketplace) needs **split payments** — one customer charge that gets divided between the platform and multiple sub-merchants, each with their own ledger balance and payout schedule. Worth naming as a "Connect"-style extension (mirroring Stripe Connect) even if you don't build it — it's a natural v2 direction and a good thing to mention you've thought about.

**What this project is *not* (stated plainly, per §1):** a licensed money transmitter, a real bank integration, or something you could legally put real customers' money through. It's a technically faithful simulation of the hard engineering problems a real payments company solves — which is exactly what it needs to be for a portfolio/interview context.

### 0.1 Build vs. Bypass — explicit scope decisions

Every real payments company has to solve everything in §0's overview. For this project, some of that is genuinely built, some is faked convincingly, and some is deliberately out of scope — documented as such rather than silently missing. Stating this table explicitly in the README is itself a signal of maturity: it shows you know the difference between "I didn't think of this" and "I decided not to build this, and here's why."

| Area | Decision | Notes |
|---|---|---|
| **Core payments API, idempotency, ledger** | **Build, for real** | This is the actual point of the project |
| **AWS infrastructure** (EKS/Fargate, Aurora/RDS, ElastiCache, SQS, MSK/Redpanda, Secrets Manager) | **Build, for real** | Real infra, real deploy, real load-testing target |
| **Fraud detection (rule engine + ML model)** | **Build, for real** | Trained model on a public dataset, real inference service — this is a differentiator, worth the investment |
| **FX conversion** | **Build, for real** | If it's just calling a free-tier FX rate API (e.g., exchangerate-api.com, Frankfurter) and applying the conversion + recording the rate on the ledger — this is a thin, honest integration, not a simulation |
| **Card network detection** ("what card is this while typing") | **Build, for real** | This is a solved, deterministic algorithm (§2.3) — not something that needs faking or a paid API at all |
| **Bank/card-network settlement** | **Simulate — but build a strong, controllable simulator** | This is the one piece that's structurally impossible to build for real (§25.2). Since you plan to load-test to ~1M requests, the simulator itself needs to be production-grade software (stateless, horizontally scalable, deterministic test hooks) — not a toy. It's the load-bearing fake in the whole system |
| **UPI, card payments** | **Build/simulate as above** | Keep to just these two rails for v1 — real breadth (wallets, BNPL, bank transfers) adds integration surface without adding to the core engineering story |
| **Other alternative payment methods** | **Skip** | Documented as "v2, not built" |
| **Email & SMS delivery** | **Bypass for now** | Documented as: architecture supports it (§23's SQS+SES design stands as the plan), but not wired up to a real provider yet — OTPs/notifications can log to console or a local Mailhog inbox during this phase |
| **Tax reporting/remittance** | **Bypass** | Documented: "a real system would integrate Avalara/Stripe-Tax-style calculation here; out of scope" |
| **KYC / identity verification** | **Bypass** | Documented as a stub (`always_approved` in test mode) with a clear note on what a real implementation would require |
| **Merchant risk screening / underwriting** | **Bypass** | Same treatment — stubbed, documented |
| **Sanctions/AML screening, credit bureaus, PCI QSA audit** | **Bypass** | These require actual vendor relationships/contracts and can't be meaningfully faked *or* built — document as "out of scope, here's who you'd talk to in reality" |
| **Money transmitter licensing, sponsor bank relationship** | **Bypass** | Not a technical problem at all — one paragraph in the README acknowledging this is the actual gap between this project and a real business |

**The throughline:** anything that's "just an API or an algorithm" gets built for real, even if it's a free-tier integration. Anything that requires **talking to a human institution, signing a contract, or paying for a compliance relationship** gets bypassed and documented. That's a clean, defensible line to draw and explain.

---

## 1. Goals & Non-Goals

**Goals**
- Process payments exactly once, even under client retries, network partitions, and duplicate webhook deliveries.
- Maintain a financially correct, auditable double-entry ledger — money is never created or destroyed.
- Handle async payment lifecycle (pending → processing → succeeded/failed) via webhooks with guaranteed, ordered-enough delivery.
- Scale horizontally to handle spiky traffic (flash sales, month-end billing runs) without manual intervention.
- Be secure enough to reason about PCI DSS scope even if you never formally certify.

**Non-goals (for a portfolio project — call this out explicitly in your README)**
- You are not actually moving money. You'll integrate with a real processor in test mode (Stripe test API, or a mocked "bank simulator" you build) — building actual card processing/PCI infra is not realistic or advisable to fake.
- You won't run literal 10M concurrent users — you'll **design and load-test** for that scale (e.g., using k6/Locust to simulate it), which is exactly what interviewers want to see: can you reason about scale, not did you rent enough AWS to prove it.

---

## 2. Language & Stack Choices: Backend, Frontend, and Card Detection

### 2.1 Backend: Go vs Rust

**Recommendation: Go**, with Rust as an optional "hot path" optimization you can mention in interviews.

| Factor | Go | Rust |
|---|---|---|
| Concurrency model | Goroutines + channels — trivial to reason about, cheap to spawn thousands | async/await + tokio — more powerful but steeper learning curve |
| Ecosystem for payments/backend | Mature: `pgx`, `go-redis`, `sarama`/`kafka-go`, AWS SDK v2, `chi`/`gin`, excellent gRPC support | Growing but thinner; `sqlx`, `redis-rs`, `rdkafka` all exist but less battle-tested at this layer |
| Compile/iteration speed | Fast — matters a lot when you're building 6+ microservices solo | Slower compile times, more friction while iterating |
| Memory safety | GC-managed, "good enough" — no data races by default with proper channel discipline | Best-in-class — no GC pauses, guaranteed no data races |
| Where it actually matters for payments | Idempotency/ledger correctness comes from **design** (DB constraints, locks), not language | Rust's guarantees shine in a narrow high-frequency matching/settlement engine |
| Hiring-market relevance | Stripe, Uber, Twitch, most fintech infra teams run Go for exactly this kind of service | Rust is used at the edges (e.g. `libsignal`, some HFT) — less common for CRUD-adjacent payment APIs |

**Practical plan:** Build the whole system in **Go**. If you want a standout differentiator, rewrite just the **idempotency-key-checking hot path** (the single highest-QPS, lowest-latency-budget component) as a small **Rust** service behind gRPC, and benchmark it against the Go version. That gives you a legitimate "I evaluated Rust for a latency-critical path and measured the tradeoff" story — much stronger than "I built it all in Rust because it's fast."

### 2.2 Frontend: not one answer — you actually need two different things

This is a detail people miss: a payments product needs **two separate frontend surfaces with different constraints**, and treating them as the same problem leads to the wrong tech choice.

| Surface | What it is | Recommended stack | Why |
|---|---|---|---|
| **Merchant Dashboard** | Internal-facing SPA where merchants view transactions, disputes, payouts, API keys | **React + TypeScript + Vite**, **Tailwind CSS** for styling, **Recharts** or **Chart.js** for volume/revenue graphs, **TanStack Query** for API data fetching/caching | It's a standard authenticated SPA — React's ecosystem (tables, charts, forms) is the fastest path, and it's the most in-demand frontend skill for job-market relevance |
| **Checkout / Payment Form widget** | The embeddable component *merchants* put on *their own* checkout page to collect card details | **Vanilla JavaScript (or a tiny framework-agnostic web component)** — **not React** | Critical constraint: your merchants build their sites in React, Vue, Angular, plain HTML, WordPress — anything. If your embeddable checkout widget itself requires React, you force a dependency on every merchant's stack. This is exactly why the real **Stripe.js/Elements** is vanilla JS distributed as a `<script>` tag, rendering into an iframe — framework-agnostic by necessity, not by accident. Mirror that: build `payflow.js` as a standalone script exposing a small API (`PayFlow.createCardElement(...)`) that works regardless of what the merchant's site is built in |

**Why this distinction matters in an interview:** "I used React for everything" is a fine answer for a normal web app; for a payments product specifically, correctly identifying that the checkout widget can't assume a framework is a much stronger signal that you understand who actually *uses* each surface.

### 2.3 Card network detection ("what card is this while typing") — build this for real, it's not a fake

Good instinct to ask about this one, but it's worth knowing: **this needs no external API and no simulation at all** — it's a fully deterministic, offline algorithm that real systems (including Stripe.js) run entirely client-side, instantly, on every keystroke.

**How it actually works:**
1. **IIN/BIN prefix ranges** — the first 1-8 digits of a card number (the "Issuer Identification Number," historically called BIN) are publicly documented and map to a network:
   ```
   Visa:            starts with 4
   Mastercard:       starts with 51–55, or 2221–2720
   American Express: starts with 34 or 37
   Discover:         starts with 6011, 622126–622925, 644–649, or 65
   RuPay (for UPI/India context): starts with 60, 65, 81, 82, 508
   ```
   A simple prefix-matching function (a handful of regexes) run on every keystroke tells you the network **before the user even finishes typing** — exactly like the little card-logo icon that lights up in Stripe/real checkout forms.
2. **Luhn algorithm** — a simple checksum (mod-10 algorithm) validates that the card number is *structurally* well-formed (catches typos) without knowing anything about whether the card is real or has funds. This is public, standard, and about 10 lines of code.
3. **Length/format validation** per network (Amex is 15 digits and formatted 4-6-5; most others are 16 digits, 4-4-4-4) — also just a lookup table.

**None of this requires a network call, an API key, or any "faking."** It's genuinely real, production-grade logic — the same logic Stripe Elements actually ships to browsers. Implement it directly in your `payflow.js` checkout widget (§2.2). If you want to *additionally* enrich with issuing-bank name/country (not needed for network detection, just a nice-to-have), there are free BIN-lookup APIs (e.g., binlist.net) you could optionally call server-side — but the core "which network is this" detection should stay client-side, instant, and dependency-free.

---

## 3. High-Level Architecture

```
                                    ┌─────────────────┐
                                    │   Route53 (DNS)  │
                                    └────────┬─────────┘
                                             │
                                    ┌────────▼─────────┐
                                    │   CloudFront CDN  │  (static assets, docs, dashboard)
                                    └────────┬─────────┘
                                             │
                                    ┌────────▼─────────┐
                                    │   ALB (L7 LB)     │  ← TLS termination, path-based routing
                                    └────────┬─────────┘
                    ┌────────────────────────┼────────────────────────┐
                    │                        │                        │
            ┌───────▼───────┐       ┌────────▼────────┐      ┌────────▼────────┐
            │  Auth Service  │       │ Payments API     │      │  Dashboard/API   │
            │  (EKS pods)    │       │  Gateway (EKS)   │      │  (merchant UI)   │
            └───────┬───────┘       └────────┬────────┘      └─────────────────┘
                    │                        │
        ┌───────────┴──────┐       ┌─────────┴──────────────────────┐
        │                  │       │                                │
┌───────▼──────┐  ┌────────▼───┐  ┌▼───────────┐   ┌────────────────▼───┐
│  Redis        │  │  SES/SNS   │  │ Idempotency │   │  Ledger Service     │
│ (sessions,    │  │  (email    │  │  Service    │   │  (double-entry,     │
│  rate limit)  │  │  OTP)      │  │  (Redis +   │   │  Postgres/Aurora)   │
└───────────────┘  └────────────┘  │  Postgres)  │   └──────────┬──────────┘
                                    └──────┬──────┘              │
                                           │                     │
                                    ┌──────▼─────────────────────▼──────┐
                                    │        Kafka (event bus)           │
                                    │  topics: payment.created,          │
                                    │  payment.succeeded, payment.failed,│
                                    │  ledger.posted, webhook.pending    │
                                    └──────┬──────────────────┬─────────┘
                                           │                  │
                              ┌────────────▼───────┐  ┌───────▼────────────┐
                              │  Webhook Delivery    │  │  Reconciliation     │
                              │  Worker (retries,    │  │  Worker (batch,     │
                              │  exp. backoff, DLQ)  │  │  runs hourly)       │
                              └──────────────────────┘  └─────────────────────┘
                                           │
                                    ┌──────▼──────┐
                                    │  Merchant's  │
                                    │  webhook URL │
                                    └──────────────┘
```

**Core services (each independently deployable, own DB schema at minimum, own DB instance once you outgrow shared Postgres):**

1. **Auth Service** — signup, login, email verification, JWT issuance, API key management for merchants.
2. **Payments API Gateway** — public-facing REST/gRPC API: `POST /v1/payment_intents`, `POST /v1/charges`, etc.
3. **Idempotency Service** — the heart of "exactly-once." Every mutating request carries an `Idempotency-Key` header.
4. **Ledger Service** — append-only double-entry bookkeeping. Single source of financial truth.
5. **Webhook Delivery Service** — async, at-least-once delivery with signed payloads and retry/backoff.
6. **Reconciliation Service** — periodic batch job comparing ledger state against the (simulated) processor/bank statement.
7. **Notification Service** — transactional email (verification, receipts, failed-payment alerts) via SES.
8. **Scheduler Service** — cron-like: subscription renewals, retry sweeps, reconciliation triggers.

---

## 4. Idempotency Design (the centerpiece)

This is what interviewers will drill into. Get this right.

### 4.1 The problem
A client calls `POST /v1/charges` with `Idempotency-Key: abc123`. The request succeeds server-side, but the response is lost (network blip). The client, following Stripe's own guidance, retries with the **same key**. You must **not** charge the customer twice.

### 4.2 Design

**Storage:** Postgres table (source of truth) + Redis (fast-path cache/lock).

```sql
CREATE TABLE idempotency_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key VARCHAR(255) NOT NULL,
    merchant_id     UUID NOT NULL,
    request_hash    VARCHAR(64) NOT NULL,   -- SHA-256 of normalized request body
    status          VARCHAR(20) NOT NULL,   -- 'processing' | 'completed' | 'failed'
    response_body   JSONB,
    response_status INT,
    locked_at       TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,   -- e.g. now() + 24h
    UNIQUE (merchant_id, idempotency_key)
);
CREATE INDEX idx_idem_expiry ON idempotency_keys (expires_at);
```

**Flow:**
1. Request arrives with `Idempotency-Key`.
2. Try `INSERT ... ON CONFLICT DO NOTHING` with status `processing`. Postgres's unique constraint is your real lock — this is atomic and race-safe even across multiple app instances.
3. If insert succeeds → you own this request. Proceed to business logic.
4. If insert conflicts → row already exists:
   - If `status = 'completed'` → **replay the stored response verbatim**. Don't reprocess.
   - If `status = 'processing'` and `locked_at` is recent (< 30s) → return `409 Conflict` ("request already in flight") or block briefly and poll.
   - If `status = 'processing'` and `locked_at` is stale (crashed worker) → steal the lock, reprocess.
   - If `request_hash` differs from the stored hash for the same key → **reject with 422** (client reused a key for a different request — this is a client bug, and Stripe does exactly this).
5. On completion, `UPDATE` the row to `status = 'completed'` with the response body, inside the **same DB transaction** as the ledger write (see §5). This is critical: idempotency-record-write and ledger-write must be atomically consistent, or you can have a "completed" charge with no ledger entry, or vice versa.

**Why Redis alone isn't enough:** Redis gives you speed but not durability guarantees you'd bet money on (literally). Use Redis as a **read-through cache** in front of Postgres (cache the completed response for fast replay), not as the system of record. For the lock itself, Postgres's unique constraint + row lock is simpler and safer than implementing Redlock correctly.

**Key expiry:** Stripe uses 24 hours. Run a cheap scheduled job (or Postgres partitioning + `pg_cron`) to purge expired keys.

---

## 5. Ledger Design (double-entry)

Every financial event is two balanced rows: a debit and a credit. Never mutate a row — only append.

```sql
CREATE TABLE ledger_entries (
    id              BIGSERIAL PRIMARY KEY,
    transaction_id  UUID NOT NULL,          -- groups the debit+credit pair
    account_id      UUID NOT NULL,          -- e.g. merchant_balance, platform_fees, customer_receivable
    direction       VARCHAR(6) NOT NULL CHECK (direction IN ('debit','credit')),
    amount_cents    BIGINT NOT NULL CHECK (amount_cents > 0),
    currency        CHAR(3) NOT NULL,
    idempotency_key VARCHAR(255),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata        JSONB
);
CREATE INDEX idx_ledger_account ON ledger_entries (account_id, created_at);
CREATE INDEX idx_ledger_txn ON ledger_entries (transaction_id);
```

**Invariant enforced at write time (in a DB transaction, and re-verified nightly by the reconciliation job):** for every `transaction_id`, `SUM(CASE WHEN direction='debit' THEN amount_cents ELSE -amount_cents END) = 0`.

**Balance is a derived value, never stored directly** — compute `SUM` over `ledger_entries` for an account (with a materialized/cached running balance per account, refreshed on write, for read performance at scale — this is your one deliberate denormalization, and you should be able to explain the tradeoff in an interview).

**Why append-only matters:** it gives you a perfect audit trail for free, makes reconciliation a pure query problem, and means bugs can't silently corrupt historical state — you can always replay.

---

## 6. Database Strategy

**Primary DB: PostgreSQL (via Amazon Aurora PostgreSQL)** — not MySQL, not a NoSQL store, for money. You want ACID transactions, strong consistency, and mature constraint/locking support.

- **Aurora over vanilla RDS**: faster failover (~30s), up to 15 read replicas, storage auto-scales to 128TB, and it's what most real fintechs actually use.
- **Write scaling**: at genuine 10M-user scale, a single Aurora writer will eventually bottleneck. Plan for **sharding by merchant_id** (each merchant's data — payments, ledger — lives in one shard; cross-shard queries are rare in payments because merchants don't share financial state). Use a routing layer (e.g., Vitess-style, or simpler: a `merchant_shard_map` table + connection pool per shard) — but **don't build this on day one**. Call it out in your design doc as "Phase 2" and explain the trigger condition (e.g., write IOPS > 70% sustained on primary).
- **Read scaling**: Aurora read replicas for the Dashboard/reporting service — reporting queries should *never* hit the primary.
- **Connection pooling**: PgBouncer (transaction mode) in front of Aurora — Postgres connections are expensive; with hundreds of pods you'll exhaust `max_connections` fast without this.

**Redis (Amazon ElastiCache)**:
- Idempotency response cache (short TTL read-through).
- Rate limiting (sliding window counters, `INCR` + `EXPIRE`).
- Session/JWT-blocklist storage for Auth.
- Distributed locks for non-DB-transactional operations (e.g., "only one subscription-renewal worker should process merchant X's batch at a time") — via Redlock or, more simply, `SET key NX EX 30`.
- Use **Redis Cluster mode** for horizontal scaling once single-node memory/throughput becomes limiting.

**Kafka (Amazon MSK) — the event backbone**:
- Topics: `payment.created`, `payment.succeeded`, `payment.failed`, `ledger.posted`, `webhook.pending`, `email.requested`.
- Why Kafka over SQS here: you need multiple independent consumers of the same event (webhook delivery, reconciliation, analytics, fraud scoring) — Kafka's pub/sub-with-replay model fits; SQS is better for point-to-point work queues (see below).
- Partition key: `merchant_id` — guarantees ordering of events *within* a merchant, which is what actually matters (you don't need global ordering across merchants).

**SQS** — used for internal work queues where exactly one consumer should process a job (e.g., "send this specific email," "retry this specific webhook attempt"). Pair with a **DLQ (dead-letter queue)** after N failed attempts, alertable via CloudWatch.

---

## 7. Webhook Delivery (async, at-least-once, ordered-enough)

1. Business event occurs → row written to `webhook_events` table (Postgres) + published to Kafka `webhook.pending`.
2. Webhook Delivery Worker (horizontally scaled, Kafka consumer group) picks up event, signs payload (HMAC-SHA256 with merchant's webhook secret, à la Stripe-Signature header), POSTs to merchant's registered URL.
3. **Retry policy**: exponential backoff with jitter — e.g. 1s, 5s, 30s, 2m, 10m, 1h, up to 24h, then mark `failed` and alert the merchant via dashboard/email.
4. **Idempotency for the merchant's side too**: include an `event_id` in every payload so merchants can dedupe (you *will* deliver duplicates under retry — that's the "at-least-once" contract, document it clearly like Stripe does).
5. Track delivery attempts in a `webhook_delivery_attempts` table for observability and merchant-facing "delivery logs" in the dashboard.

---

## 8. Auth & Email Verification (don't skip this — it's graded like the rest)

Even though it's "just login," at 10M users this needs the same rigor:

**Signup → Verification flow:**
1. `POST /signup` → validate email format + password strength (bcrypt/argon2 hash, never store plaintext).
2. Insert user with `email_verified = false`. Generate a **verification token**: cryptographically random 32-byte value, store **hashed** (SHA-256) in DB with an expiry (e.g., 1 hour) — never store the raw token server-side (same principle as password reset tokens).
3. Publish `email.requested` event → Notification Service consumes → sends via **Amazon SES** with the raw token embedded in a link.
4. `GET /verify?token=...` → hash incoming token, look up, check expiry, mark `email_verified = true`, invalidate token (delete row or mark used).
5. Rate-limit verification-email resends (Redis, e.g. 1 per 60s per account) to prevent SES abuse/spam-vector.

**Login:**
- Password check via bcrypt/argon2 (constant-time compare, built into the library).
- Issue a short-lived **JWT access token** (~15 min) + long-lived **refresh token** (rotated on use, stored hashed in Redis with device/session metadata — enables "log out of all devices").
- Rate-limit login attempts per-IP and per-account (Redis sliding window) to blunt credential stuffing.
- 2FA (TOTP) as a stretch goal — very interview-impressive for a payments product given the stakes.

**API keys (for merchants calling your Payments API):**
- Generate as `sk_live_<random>` / `sk_test_<random>` (mirroring Stripe's convention — signals you understand the UX of this).
- Store only a hash; show the raw key exactly once at creation.
- Support key rotation and scoped permissions (read-only vs write) per key.

---

## 9. Scheduling (cron-like jobs)

Use a dedicated **Scheduler Service** backed by something like `github.com/robfig/cron` in Go for lightweight jobs, or **AWS EventBridge Scheduler → triggers Lambda/ECS task** for anything that needs to survive service restarts and scale independently:

- Subscription renewal sweep (every hour, find subscriptions due, enqueue charge attempts).
- Idempotency key cleanup (daily).
- Reconciliation job (hourly — compare ledger totals vs simulated processor statement, alert on mismatch).
- Webhook DLQ review/alerting (every 15 min).
- Dunning retries for failed subscription payments (daily, with backoff schedule: day 1, 3, 7, 14).

---

## 10. AWS Infrastructure Map

| Concern | Service |
|---|---|
| Compute | **EKS** (Kubernetes) running Go services as pods; HPA (Horizontal Pod Autoscaler) on CPU + custom Kafka-consumer-lag metric |
| Load balancing | **ALB** (L7, path-based routing, TLS termination) in front of EKS; **NLB** if you need raw TCP/gRPC passthrough |
| Database | **Aurora PostgreSQL** (Multi-AZ, read replicas) |
| Cache | **ElastiCache for Redis** (cluster mode) |
| Event bus | **MSK** (managed Kafka) |
| Work queues | **SQS** + DLQ |
| Email | **SES** (transactional email — verification, receipts) |
| Secrets | **AWS Secrets Manager** (DB creds, webhook signing secrets, API key salts) — never in env vars in plaintext |
| Encryption | **KMS** for encryption-at-rest on RDS/S3; TLS 1.2+ everywhere in transit |
| Object storage | **S3** (invoices/receipts as PDFs, audit log exports) |
| CDN/DNS | **CloudFront** + **Route53** |
| Observability | **CloudWatch** (metrics/alarms) + **OpenTelemetry** → **Grafana/Prometheus** (self-hosted or Amazon Managed Prometheus) for dashboards; **X-Ray** or Jaeger for distributed tracing |
| CI/CD | **CodePipeline** or GitHub Actions → ECR → EKS rolling deploy (or ArgoCD for GitOps) |
| Infra-as-code | **Terraform** (strongly preferred over hand-clicking — also a resume line item) |

---

## 11. Scaling & Resilience Patterns to Implement

- **Circuit breakers** on all outbound calls (e.g., to the simulated bank/processor) — use a library like `sony/gobreaker`. Prevents cascading failures when a downstream dependency is slow/down.
- **Bulkheads**: separate connection pools/thread budgets per downstream dependency so one slow dependency can't starve the whole service.
- **Rate limiting** at the API Gateway layer (token bucket per API key) — protects you from a single merchant's traffic spike or bug hammering the system.
- **Backpressure**: if Kafka consumer lag grows past a threshold, scale consumers via HPA; if DB write latency spikes, shed load (return 429s) rather than falling over.
- **Graceful degradation**: if the Notification Service is down, payments should still succeed — email sending must never block the payment critical path (this is exactly why it's event-driven/async, not a synchronous call).
- **Multi-AZ everywhere**, and if you want to go further, design (on paper) for **multi-region active-passive** with Aurora Global Database — good to discuss even if you don't implement it.

---

## 12. Observability

- **Structured logging** (JSON) with a `trace_id` propagated through every service via context — this is what actually lets you debug "why did charge X fail" across 6 microservices.
- **Metrics**: RED method (Rate, Errors, Duration) per service/endpoint; business metrics too (payment success rate, webhook delivery success rate, idempotency conflict rate).
- **Distributed tracing** across the whole charge → ledger → webhook chain.
- **Alerting**: reconciliation mismatches and webhook DLQ growth should page immediately (these are correctness/money issues, not just uptime issues) — everything else can be a dashboard.

---

## 13. Security Notes (mention these even if you don't fully implement)

- Never log raw card numbers, CVVs, or full API keys/tokens — log key *prefixes* only.
- Tokenize card data immediately at the edge (or don't touch raw PANs at all — proxy straight to the test processor and store only the returned token). This is what keeps your actual PCI DSS scope small.
- All secrets in Secrets Manager, rotated, injected at runtime — never baked into images or env files in git.
- Least-privilege IAM roles per service (each EKS service account gets its own IAM role via IRSA, not a shared god-role).

---

## 14. Fraud Detection & ML Risk Scoring

Every real processor scores a charge *before* authorizing it. This sits as a synchronous step inside the payment flow (with a strict latency budget — you can't add 2 seconds to checkout) plus an async, deeper-analysis path for catching things after the fact.

### 14.1 Where it sits in the flow
```
POST /v1/charges
   │
   ▼
Idempotency check (§4)
   │
   ▼
┌─────────────────────┐
│  Risk Engine (sync)  │  ← must respond in <100ms, budget matters
│  - rule engine        │
│  - fast ML model      │
└─────────┬───────────┘
          │
   score ─┼─ low risk  → proceed to charge
          ├─ medium    → step-up auth (3DS challenge, §16) or manual review queue
          └─ high risk → decline immediately
          │
          ▼ (async, doesn't block response)
┌──────────────────────┐
│  Deep Analysis Worker │  ← heavier model, graph features, runs post-charge
│  can flag for review  │     or trigger a hold/reversal if charge already succeeded
└──────────────────────┘
```

### 14.2 Rule engine (build this first — it's most of the value for least effort)
Fast, explainable, and what most fraud systems lean on more than people expect:
- **Velocity rules**: >N charges from same card/IP/device in X minutes → flag.
- **Amount anomalies**: charge is >5x the merchant's/customer's historical average.
- **Geo mismatch**: billing country vs IP geolocation vs card-issuing country disagree.
- **BIN (card bank identification number) risk lists**: prepaid cards, high-risk-country issuers.
- **Blocklists**: known-bad card fingerprints, emails, device IDs (shared across your whole platform — this is powerful precisely because you see fraud patterns across *all* merchants, which a single merchant never could).
- Implement as a small DSL or just versioned config (YAML rules → compiled to Go structs) so risk analysts (later, a real team) could tune without deploys.

### 14.3 ML model (the sync fast-path model)
- **Features**: transaction amount, time-of-day, card age (first-seen date in your system), velocity counters (precomputed in Redis, not recomputed per request — e.g. `charges_last_1h:{card_fingerprint}` as a Redis counter with TTL), merchant risk category, device fingerprint entropy, email domain reputation (free-mail vs corporate vs disposable-email-detector), distance between shipping/billing address.
- **Model choice**: gradient-boosted trees (**XGBoost / LightGBM**) — not deep learning. Fraud teams overwhelmingly use GBTs because they're fast at inference (<10ms), work well on tabular data, and are explainable enough to justify a decline to a customer/regulator ("feature X contributed Y to the score").
- **Serving**: export the trained model, serve via a lightweight Go inference wrapper (using `leaves` or `treelite` for GBT inference in Go — no need for a Python service in the hot path, which would add network hop + latency) or a small dedicated Python **FastAPI + ONNX Runtime** service if you want ML tooling in Python but keep it behind a tight gRPC contract with a hard timeout + circuit breaker + a rule-engine fallback if the model service is slow/down. **Never let the ML service be a hard dependency for payments to work** — always fail open to the rule engine.
- **Training pipeline**: label data from your own chargeback outcomes (a charge that later got disputed as "fraudulent" = positive label) — this is why disputes (§15) and fraud scoring are deeply linked; disputes are literally your training labels. Retrain periodically (weekly/monthly) as a batch job; version models; A/B test new model versions on a % of live traffic before full rollout, logging both old and new scores to compare offline before trusting the new one.

### 14.4 Deep/async analysis (graph-based, catches organized fraud)
- Build a **graph** of entities — cards, emails, devices, IPs, shipping addresses — and edges between transactions that share any of these. Ring/fraud-network detection (e.g., 50 "different" customers all sharing one device fingerprint) is much easier to see as a graph than as isolated rows.
- Can run as a nightly batch job (Spark/or just SQL window functions at your scale) rather than real-time — this feeds back into blocklists consumed by the sync rule engine (§14.2), which is the actual mechanism that stops the *next* fraud attempt.

### 14.5 What to log for this to even be possible
Every charge needs: device fingerprint (client-side JS SDK generates this, à la Stripe.js), IP, precise timestamp, card fingerprint (hashed PAN, not raw), and the risk score + which rules fired — stored alongside the charge record so disputes/audits can show *why* a decision was made.

---

## 15. Chargebacks & Disputes

A chargeback is the cardholder's bank forcibly reversing a charge — this touches the ledger, the merchant relationship, and your fraud model's training data all at once.

**Schema:**
```sql
CREATE TABLE disputes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    charge_id       UUID NOT NULL REFERENCES charges(id),
    reason          VARCHAR(50) NOT NULL,   -- 'fraudulent', 'product_not_received', etc.
    status          VARCHAR(20) NOT NULL,   -- 'needs_response' | 'under_review' | 'won' | 'lost'
    amount_cents    BIGINT NOT NULL,
    evidence_due_by TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**Flow:**
1. Dispute event arrives (in your simulator: a random async event from the "bank simulator"; in reality: a webhook from the card network/acquirer).
2. **Immediately reverse the funds in the ledger** — a new balanced debit/credit pair referencing the original `transaction_id`, *not* a mutation of the original entry (append-only, §5 still holds).
3. Notify the merchant (webhook + email) with the evidence deadline (~7-21 days depending on network).
4. Merchant submits evidence (receipts, delivery confirmation) via API/dashboard → stored in S3, linked to the dispute.
5. Outcome (`won`/`lost`) arrives async → if won, reverse the reversal (funds go back); if lost, the reversal stands and you may also charge a **dispute fee** (real processors do — another ledger entry).
6. **Feed the outcome back to the fraud model** as a labeled training example (§14.3) — this is the feedback loop that makes the fraud system improve over time instead of going stale.

---

## 16. 3D Secure / Step-Up Authentication

When the risk engine (§14) returns "medium risk," or when required by regulation (EU's PSD2/SCA for European cards), you need a **challenge flow** instead of a silent decline or accept:

1. Charge request comes in → risk score = medium → instead of an immediate charge, respond with a `requires_action` status and a redirect URL.
2. Customer is redirected to their card issuer's challenge page (OTP sent to their phone, etc.) — your simulator can fake this with a simple "click to confirm" page.
3. Issuer redirects back to you with a cryptographic authentication result.
4. You resume the original charge with the 3DS result attached — **this must reuse the original idempotency key**, which means your idempotency design (§4) needs a `requires_action` intermediate status, not just processing/completed/failed. Add that state now while you're at it.

This turns your "synchronous charge" model into a genuine multi-step state machine — worth drawing as its own diagram in the README; it's one of the more interesting state-management problems in the whole system.

---

## 17. Refunds

- Full or partial — partial refunds mean a charge can have **multiple** associated refund ledger entries, so track `amount_refunded_cents` as a derived sum, never a mutable field on the charge.
- **Idempotency applies here too** — refund requests need their own idempotency keys; without it, a retried refund request could refund twice.
- **Guard against over-refunding**: `SUM(refunds) <= original_charge_amount`, enforced at the DB layer (a `CHECK` via a trigger, or an application-level transaction with `SELECT ... FOR UPDATE` on the charge row) — this is a classic race condition if two refund requests land concurrently.
- Refunds reduce the merchant's available balance — if that pushes it negative, see §19.

---

## 18. Payouts (getting money *out* to merchants)

You've only modeled money coming in — a real system needs the other half:

- **Payout schedule**: merchants don't get paid instantly; standard is **T+2 business days** rolling, or a fixed weekly/monthly schedule — configurable per merchant risk tier (new/unverified merchants get longer holds, which is itself a fraud control).
- **Payout batch job** (Scheduler Service, §9): daily, compute each merchant's available balance (`ledger balance` minus pending holds minus reserved-for-disputes amount), initiate an ACH transfer (simulated), write a `payouts` table row, and post the corresponding ledger entries (debit merchant balance, credit "in transit" account, then credit "paid out" once the simulated bank confirms).
- **Failure handling**: ACH transfers can fail days later (bad routing number) — needs its own async reconciliation and merchant notification path, same retry/backoff pattern as webhooks.

---

## 19. Reserves & Negative Balances

- Hold back a **rolling reserve** (e.g., 5-10% of each charge) for high-risk merchant categories, released after a delay — protects you when disputes come in after a payout already happened.
- If disputes/refunds exceed a merchant's available balance, their ledger account goes **negative** — track this explicitly (`account_type = 'merchant_debt'`), attempt to collect via their next payout or a linked bank debit, and flag the account for manual review above a threshold. This is a real edge case worth explicitly designing rather than assuming balances are always ≥0.

---

## 20. Multi-Currency / FX

- Store `amount_cents` **and** `currency` on every ledger entry (already in the schema, §5) — never assume USD.
- If a merchant accepts EUR but wants payouts in USD, you need an FX rate source (simulate with a fixed daily rate table) and must record the **exact rate used** at conversion time on the ledger entry — rates fluctuate, and for audit/dispute purposes you need to prove exactly what rate was applied when.
- Ledger balance invariant (§5) must be checked **per currency**, not globally — a transaction can't mix currencies in one balanced entry pair.

---

## 21. Tax Reporting

- Track cumulative annual payment volume per merchant; in the US, generate **1099-K** data once a merchant crosses the reporting threshold — this is a straightforward aggregation job over the ledger, but worth a line item so the doc doesn't look like it forgot revenue-adjacent obligations entirely (even though actual tax-form filing is a "real business" concern, not a portfolio-build one).

---

## 22. Technical Debt & Operational Hardening

These are the gaps that don't show up when you're designing the system — only when you're actually running it. Worth naming explicitly even if you don't fully implement all of them, because acknowledging them is itself the signal.

### 22.1 Correctness & consistency

- **Transactional outbox pattern** — the charge → ledger → Kafka flow is a distributed transaction across a DB and a message broker. If the ledger write commits but the Kafka publish fails (broker blip, network partition), you get a silent inconsistency: money moved, but no downstream event fired. Fix: write the ledger entry **and** an `outbox_events` row in the *same* Postgres transaction; a separate poller reads unsent outbox rows and publishes them to Kafka, marking them sent only after ack. This guarantees "commit to DB" and "eventually published" are never decoupled by an infra failure.
  ```sql
  CREATE TABLE outbox_events (
      id           BIGSERIAL PRIMARY KEY,
      aggregate_id UUID NOT NULL,
      event_type   VARCHAR(50) NOT NULL,
      payload      JSONB NOT NULL,
      published    BOOLEAN NOT NULL DEFAULT false,
      created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
  );
  ```
- **Clock skew** — any time-critical logic (`idempotency_keys.expires_at`, `disputes.evidence_due_by`) must be computed from **Postgres's `now()`**, not `time.Now()` on individual app pods. Wall clocks drift across a fleet; the DB is your single source of temporal truth.
- **GDPR "right to erasure" vs. append-only ledger** — a genuinely unresolved tension worth naming rather than hand-waving: you're legally required to retain financial records (7 years in most jurisdictions) but a user can request PII deletion. Resolution: **pseudonymize, don't delete** — swap name/email on historical rows for a reference into a `deleted_users` table, leave the financial data intact. One paragraph in the README; shows you know these two obligations aren't always compatible and that "just delete it" isn't a real answer.
- **TOCTOU race in balance checks** — `SELECT balance` then `UPDATE` is a check-then-act race under concurrency (two refund requests landing simultaneously). Fix with `SELECT ... FOR UPDATE` (row lock) around the check+write, or a single atomic `UPDATE ledger_balances SET balance = balance - $1 WHERE account_id = $2 AND balance >= $1` and check `rows_affected`.

### 22.2 Deployment & schema evolution

- **Zero-downtime migrations** — you can't take Postgres offline to `ALTER TABLE` at scale. Discipline: additive-only changes, backfill in batches, dual-write during the transition window, drop old columns only once all readers are migrated (the "expand/contract" pattern).
- **API versioning from day one** — once a merchant integrates against `/v1/charges`, that contract is frozen forever. Plan the `/v2` path conceptually now rather than mutating v1 response shapes later.
- **Canary/blue-green deploys** — a bad deploy that silently double-charges people is catastrophic in a way most outages aren't. Route 1% of traffic to a new version behind a flag before full rollout. Even if the portfolio build just does rolling restarts, name this as the production-grade approach.
- **Secrets rotation grace periods** — rotating a webhook signing secret naively breaks in-flight webhook validation (old signatures suddenly "invalid"). Real rotation needs a window where **both old and new secrets validate**, then retire the old one.

### 22.3 Testing & failure injection

- **Concurrency test suite** — fire N simultaneous requests with the *same* idempotency key and assert exactly one charge/ledger-entry was created. This is the single most convincing artifact you can show in an interview — more valuable than any diagram in this doc.
- **Integration tests against real Postgres** (via `testcontainers-go`) rather than mocks — idempotency and ledger correctness are exactly the kind of logic that mocked DB tests give false confidence about.
- **Chaos testing** — kill the DB connection mid-transaction, kill a pod mid-webhook-retry-attempt, inject latency with `toxiproxy` on the "bank simulator" call — and verify nothing double-processes or leaves the ledger unbalanced. A short recorded demo of this is a strong portfolio artifact.
- **Backup/restore drills** — having automated Aurora backups isn't the same as *knowing your restore works* and how long it takes. An untested backup is technical debt wearing a checkbox. Actually run a restore once and record your RTO/RPO.

---

## 23. Case Study: Email/OTP Delivery Pipeline at Scale

This one flow — "user types their email to log in, gets sent an OTP" — is a perfect small-scale mirror of every reliability pattern in this whole doc. Worth understanding deeply, because the same shape reapplies everywhere (webhooks, payouts, disputes).

### 23.1 Why this can't be a synchronous call

The naive version: `POST /login` → API handler directly calls SES to send the email → returns response. **This is wrong at any real scale**, for two reasons:
- SES calls take 100-500ms+ and can occasionally hang/timeout — your login endpoint's latency becomes hostage to your email provider's latency.
- If SES is degraded (rate-limited, regional issue) your **entire login flow goes down**, even though logging in and sending an email are logically separate concerns. A dependency that isn't on the critical path should never be able to take down the critical path.

So: the API's job is only to **validate the request and enqueue a job**. It returns success immediately (`202 Accepted`, or `200` with "check your email"). A separate worker fleet does the actual sending, decoupled in time from the user's request.

### 23.2 End-to-end flow

```
User submits email
        │
        ▼
POST /auth/otp/request
        │
        ├─ Rate-limit check (Redis: max 1 request per email per 60s,
        │   max 5 per email per hour) → reject with 429 if exceeded
        │   (blocks OTP-spam / email-bombing attacks — real attack vector)
        │
        ├─ Generate OTP (6-digit random, crypto-secure)
        │
        ├─ Store HASHED OTP in Redis: key = "otp:{email}", value = sha256(otp),
        │   TTL = 5 minutes, single-use (delete on successful verify)
        │
        ├─ Enqueue send-job → SQS queue "otp-email-delivery"
        │      { email, otp_plaintext, request_id, enqueued_at }
        │      (OTP travels in the queue payload — this is the ONLY place
        │       the plaintext OTP exists outside Redis; queue must be encrypted
        │       at rest, e.g. SQS with a KMS key)
        │
        └─ Return 200 to user immediately ── this is the key move: the user
           gets a fast response regardless of SES's current health.

   ┌─────────────────────────── async, off the request path ───────────────────────────┐
   │                                                                                      │
   │  SQS Queue (otp-email-delivery)                                                     │
   │         │                                                                            │
   │         ▼                                                                            │
   │  Worker fleet (Go, horizontally scaled, auto-scales on queue depth — §23.6)          │
   │         │                                                                            │
   │         ├─ pulls message (SQS makes it invisible to other workers for                │
   │         │   a "visibility timeout" window, e.g. 30s, while this worker has it)        │
   │         │                                                                            │
   │         ├─ calls SES.SendEmail()                                                     │
   │         │                                                                            │
   │    success? ──── yes ──→ DELETE message from queue (ack) → done                      │
   │         │                                                                            │
   │         no (SES error / timeout / worker crash mid-send)                             │
   │         │                                                                            │
   │         ▼                                                                            │
   │  Message is NOT deleted → after visibility timeout expires, SQS                      │
   │  automatically makes it visible again → another worker (or the same one)             │
   │  picks it up and retries. This is SQS's built-in redelivery — you get                │
   │  "at least once" processing for free just by not deleting on failure.                │
   │         │                                                                            │
   │  SQS's "receive count" tracks how many times this message has been redelivered.      │
   │  A redrive policy says: after N receives (e.g. 5) without a successful delete,        │
   │  automatically move the message to a Dead Letter Queue (DLQ) instead of retrying     │
   │  forever.                                                                             │
   │         │                                                                            │
   │         ▼                                                                            │
   │  DLQ (otp-email-delivery-dlq) → CloudWatch alarm fires on DLQ depth > 0               │
   │  → pages on-call (this is user-facing login failure — treat it as urgent)             │
   │                                                                                        │
   └────────────────────────────────────────────────────────────────────────────────────┘
```

### 23.3 Why SQS here, not Kafka

This is a genuinely important distinction to be able to articulate: **SQS for work queues (exactly one consumer should handle each job), Kafka for event streams (multiple independent consumers need the same event).** Sending an OTP email is a single unit of work that exactly one worker should complete once — nobody else needs to "subscribe" to that event. SQS's native visibility-timeout + redrive-to-DLQ behavior is purpose-built for this and requires zero custom code, unlike Kafka where you'd hand-roll retry topics and delay logic (as noted in §22's queue-failure discussion). Use Kafka for `payment.succeeded`-style events where the webhook worker, the reconciliation worker, and analytics all need to independently consume the same event.

### 23.4 Bounce & complaint handling (the part people forget)

Email delivery isn't binary success/fail at send-time — SES can accept the send but the email later **bounces** (bad address) or gets marked as **spam/complaint** by the recipient, and these arrive asynchronously, sometimes minutes later, via **SES → SNS notification → your own SQS queue**.

- **Hard bounce** (invalid address) → mark that email as `undeliverable` in your Auth DB, stop sending to it, surface an error to the user next time they try ("this email can't receive mail — check for typos").
- **Complaint** (marked as spam) → suppress future sends to that address entirely; repeated complaints can get your **SES sending reputation** throttled or your account suspended — this directly threatens your ability to send *any* transactional email, including receipts and password resets, so it's treated as a serious signal, not a shrug.
- Maintain a `suppressed_emails` table checked **before** every send attempt — this is a required gate in the flow diagram above, easy to forget when whiteboarding the happy path.

### 23.5 Idempotency & abuse prevention on this specific flow

- **Duplicate-click protection**: if a user double-clicks "send code," the Redis rate limit (§23.2) prevents two OTPs/two emails going out — the second request just returns "code already sent, check your email" rather than generating a new one and invalidating the first (which would be confusing if the user already has the first one open).
- **Enumeration protection**: `POST /auth/otp/request` should return the **same response** ("if that email exists, we've sent a code") regardless of whether the email is actually registered — otherwise you leak which emails are valid accounts, a real security issue for a payments product specifically (attackers profiling which businesses use your platform).

### 23.6 Scaling the worker fleet to real load

The worker fleet size should track **queue depth**, not be fixed. In AWS this is either:
- **KEDA** (if on EKS) scaling pod replicas directly off the SQS `ApproximateNumberOfMessagesVisible` CloudWatch metric, or
- A **Lambda function** triggered directly by SQS (AWS manages the "worker fleet" for you entirely — for a job this lightweight and bursty, Lambda is arguably the *better* choice than a standing EKS deployment, since you pay per invocation and it scales to thousands of concurrent executions automatically with zero capacity planning).

**Worth stating explicitly in an interview:** this is a case where "just use Lambda" is the *more* sophisticated answer, not the lazy one — a bursty, stateless, short-duration job like "send one email" is precisely Lambda's ideal use case, and running a permanently-provisioned EKS fleet sized for OTP-email peak load would be wasted spend most of the day.

### 23.7 What "reliability" actually means here, end to end

Walking the full path, here's every point where something can fail and how it's handled — this table is the real answer to "how is it handled":

| Failure point | What happens |
|---|---|
| User double-submits | Redis rate limit blocks duplicate OTP generation |
| Redis is down when storing OTP | Request fails fast, returns 503 — **do not** silently proceed to send an OTP nobody can verify (fail closed, not open) |
| SQS enqueue fails | Retry inline 2-3x with tiny backoff (sub-second) before giving up and returning an error to the user — this is the one synchronous step left, keep it minimal |
| Worker crashes mid-send | SQS visibility timeout expires → message redelivered to a healthy worker automatically, no manual intervention |
| SES is degraded/rate-limited | Worker's retry (with backoff) absorbs transient failures; sustained failure exhausts SQS receive count → DLQ → page on-call |
| Email bounces | SNS notification → suppression list updated → future sends blocked, user sees a clear error next attempt |
| DLQ has messages | CloudWatch alarm → paged immediately (this is "user cannot log in," a P1) — distinct from a webhook-delivery DLQ, which can usually wait for business hours |
| OTP never arrives even though email "succeeded" (spam filter, delay) | UX-level mitigation, not infra: offer a "resend" after 30s (rate-limited), and consider SMS as a fallback channel for users who report repeated email issues — a second delivery channel is real resilience, not just a nicety |

---

## 24. Core Feature Failure Modes (What Breaks, and What Happens)

§23.7 walked this for the OTP flow specifically. Here's the same treatment for the actual money-moving core features — this is the table you should be able to recite in an interview, because "what happens when X fails" is the question that separates a design that looks complete from one that actually is.

### 24.1 The charge flow (`POST /v1/charges`)

| Failure point | What happens |
|---|---|
| Client's network drops after sending the request but before getting the response | Client retries with the **same idempotency key** (§4). Server finds the existing `processing` or `completed` row and either replays the stored response or safely resumes — the customer is never double-charged. This is the entire reason §4 exists. |
| Idempotency-check insert and the actual charge-processing crash in between (app pod dies mid-request) | Row is left in `processing` with a `locked_at` timestamp. A retry (or a background sweep) sees the stale lock (past a threshold, e.g. 30s) and safely reprocesses — but only after confirming with the "bank simulator"/processor whether the original attempt actually completed on their side (query-before-retry, not blind-retry, for anything that already left your system) |
| Ledger write succeeds, but the app crashes before the outbox/event publish | Doesn't matter — the outbox row was written in the **same transaction** as the ledger write (§22.1), so it's durably persisted regardless of what happens next; the outbox poller picks it up whenever it runs next, even after a full restart |
| Risk engine (§14) times out or the ML service is down | **Fail open to the rule engine only** — never fail open to "no risk check at all," and never fail closed (block all payments) just because one scoring path is degraded. This exact tradeoff (availability vs. thoroughness of fraud-checking) is worth being able to defend out loud |
| The downstream "bank simulator"/processor times out (you sent the charge request but never got a response — the ambiguous "did it work?" case) | This is the hardest one in payments generally. Never blindly retry — a blind retry into an ambiguous timeout can cause a real double-charge on the *processor's* side, which idempotency keys on *your* API don't protect against (that key protects your API from your client's retries, not your calls to your own downstream processor). Handle it by: (1) using your **own** idempotency key when calling the downstream processor too, if they support it (real processors do), or (2) on ambiguous timeout, mark the charge `requires_reconciliation` and let the hourly reconciliation job (§24.3) resolve it against the processor's actual transaction log rather than guessing |
| Postgres primary fails over (Aurora Multi-AZ) | ~30 second window where writes fail; in-flight requests should return a clear retryable error (503), not hang; client-side retry-with-backoff (using the same idempotency key) picks up cleanly once the new primary is live |

### 24.2 The ledger

| Failure point | What happens |
|---|---|
| Two concurrent writes try to update the same account's cached balance | Row-level lock (`SELECT ... FOR UPDATE`) on the balance row serializes the two writes — the underlying `ledger_entries` rows are append-only and never conflict; only the *derived* cached balance needs locking |
| A bug produces an unbalanced transaction (debit ≠ credit) | Should be structurally very hard to produce if all writes go through one code path that always writes both sides in one DB transaction — but as a second line of defense, the reconciliation job (§24.3) runs a `SUM(...) = 0` check per `transaction_id` and alerts loudly if it ever finds a mismatch. Treat any hit here as a P1 — it means money is unaccounted for |
| A ledger write partially fails (debit row written, credit row's insert fails) | Impossible if both are written inside a single Postgres transaction with proper error handling — this is the whole argument for using a real ACID database instead of two separate calls/services for "debit" and "credit" |

### 24.3 Reconciliation itself failing

| Failure point | What happens |
|---|---|
| The nightly/hourly reconciliation job itself crashes partway through | It's a **read-only, idempotent job** (it only compares and flags, never mutates the ledger) — safe to simply re-run from scratch on failure; no partial-state cleanup needed, which is a deliberate design choice worth calling out (idempotent batch jobs are much easier to operate than stateful ones) |
| Reconciliation finds a mismatch between your ledger and the processor's statement | Never auto-correct silently — write a `reconciliation_discrepancies` row and alert a human. Auto-"fixing" financial discrepancies without a human in the loop is how real money-accounting bugs get hidden instead of caught |

### 24.4 Webhooks (recap, tied to the same pattern)

Already detailed in §7 and §22 — the shape is identical to the OTP case: don't ack until delivered, bounded retries with backoff, DLQ + alert past the retry budget. The one addition specific to webhooks: **merchants can also just be down/slow to respond**, which is a *normal*, expected case (not a bug) — your retry budget (up to ~24h/several days, mirroring Stripe's own published behavior) exists specifically because merchant infrastructure has its own outages, and you shouldn't give up on delivering a `payment.succeeded` event just because their server was down for twenty minutes.

---

## 25. End-to-End Testing Strategy & External Dependency Simulation

Everything in §22.3 covered unit/integration/concurrency tests for individual pieces. This section answers the bigger question: **how do you run the whole system, start to finish, and prove it works** — without a real bank, a real card network, or real customers.

### 25.1 Every external dependency you need to fake, and how

This is the honest list of "outside things" beyond the bank — there are more than people usually expect:

| External dependency | What it's for | How to simulate it |
|---|---|---|
| **Card network / acquiring bank / processor** | Actually authorizing and settling a charge | **Your own "Bank Simulator" service** (§25.2 below) — this is the one you build yourself, since it's the whole point of the exercise |
| **Email (SES)** | OTP, receipts, dispute notifications | Locally: **Mailhog** or **Mailtrap** (catches emails in a fake inbox you can inspect, no real sending). In a real AWS staging account: SES provides special **test addresses** — `success@simulator.amazonses.com`, `bounce@simulator.amazonses.com`, `complaint@simulator.amazonses.com` — sending to these deterministically triggers success/bounce/complaint without spamming real people. Use these directly in your bounce-handling tests (§23.4) |
| **SMS (if you add a 2FA/fallback channel)** | Backup OTP delivery | Twilio's **test credentials** produce fake sends without charging or delivering — same idea as SES's simulator addresses |
| **FX rate provider** | Currency conversion (§20) | A fixed/mock rate table you control — never call a real FX API in tests, you need deterministic rates to assert exact ledger amounts |
| **Fraud/ML inference service** | Risk scoring (§14) | This one you can actually run **for real** in tests — it's your own model, not external. Feed it synthetic transactions engineered to trip specific risk levels (known-fraud test vectors) so tests are deterministic rather than relying on a live model's variable output |
| **3DS issuer challenge page** | Step-up authentication (§16) | A trivial fake HTML page in your Bank Simulator that always "approves" or "declines" based on a query param — you're not implementing real 3DS, just the state-machine shape |
| **The merchant's webhook receiver** | Where webhooks (§7) get delivered | You need a **fake merchant server** in your test environment — either a tiny purpose-built receiver service you control (so you can assert what it received, and toggle it on/off to test retry/DLQ behavior), or a tool like `webhook.site` for manual/exploratory testing |
| **KYC/identity verification** | Merchant onboarding checks | Explicitly out of scope per your call earlier — stub it as "always approved" in test mode and note in the README that this is intentionally unimplemented, not forgotten |

### 25.2 Build the Bank Simulator as a real, controllable service — not just randomness

This deserves more rigor than "randomly succeed or fail," because **deterministic tests need deterministic control** over the simulator's behavior. Design it as an actual small service with test hooks:

```
POST /simulator/charge
Headers: X-Simulate-Outcome: success | decline | timeout | delayed_success | network_error

- success           → returns 200 immediately, "money" moves
- decline           → returns 200 with a decline reason (insufficient_funds, etc.)
- timeout           → simulator sleeps past your client's timeout threshold, then would have
                       succeeded — lets you test the ambiguous-timeout/reconciliation path (§24.1)
- delayed_success    → returns "pending" immediately, then fires an async webhook-style callback
                       30s later — lets you test async settlement handling
- network_error      → connection reset / no response at all — different from a clean timeout,
                       tests your circuit breaker (§11)
```

Also give it endpoints to **inject a dispute** (`POST /simulator/disputes`) and **trigger a payout failure** (`POST /simulator/payouts/fail`) on demand, rather than waiting for randomness to eventually produce these cases. Randomness is good for chaos/soak testing (§25.6); **explicit control is what makes your core E2E suite reliable and fast**, since a flaky test suite that only sometimes exercises the dispute path is close to useless.

**Since the plan is to eventually load-test toward ~1M requests, the simulator itself must be built as real production software, not a quick script:**
- **Stateless and horizontally scalable** — no in-memory state that ties a request to a specific instance; if it needs to remember something (e.g., "this delayed_success charge should fire its callback in 30s"), that state goes in Redis/a DB, not a Go map in a single process — otherwise it becomes the bottleneck you're supposedly testing *around*.
- **Deployed the same way as everything else** (containerized, behind its own ALB target group, autoscaled) — the simulator needs to be able to absorb 1M req/s of *inbound* calls just as much as your real services do, or it becomes an artificial ceiling that makes your load test measure the simulator instead of your system.
- **Deterministic-by-header still works at scale** — `X-Simulate-Outcome` doesn't conflict with load-testing; run your load test with a realistic *mix* (e.g., 95% success, 3% decline, 1.5% timeout, 0.5% network_error) by having your load-test script (k6) vary the header per request according to that distribution, rather than needing the simulator to guess.

### 25.3 Local environment: docker-compose stack

Everything needed to run the full system on a laptop, no AWS account required:

```yaml
services:
  postgres:        # primary DB
  redis:           # idempotency cache, rate limits, sessions
  redpanda:        # Kafka-API-compatible, much lighter than real Kafka for local dev
  localstack:      # emulates SQS, SES, S3, Secrets Manager — real AWS SDK calls, fake backend
  mailhog:         # catches "sent" emails for inspection in tests
  bank-simulator:  # your own service, as above
  payments-api:
  ledger-service:
  auth-service:
  webhook-worker:
  fraud-service:
  fake-merchant-server:   # receives webhooks, exposes an API for tests to query "what did you get"
```

**LocalStack is the key piece that makes this realistic** — your actual application code calls the real AWS SDK (SQS, SES, Secrets Manager) exactly as it would in production, just pointed at `localhost` instead of `amazonaws.com`. This means your tests exercise the *real* integration code path, not a mocked-out stand-in, which is exactly the gap that lets bugs slip through in less rigorous setups.

### 25.4 The core E2E test scenarios to actually write

These are the scripts that prove the system, each driving the stack above through the real HTTP/API surface (no internal shortcuts):

1. **Happy path**: signup → verify email (read the OTP out of Mailhog programmatically) → login → create a charge → assert ledger entries balance → assert webhook received by the fake merchant server.
2. **Duplicate idempotency key, concurrent**: fire 100+ simultaneous identical charge requests (same key) → assert exactly one charge exists in the ledger, and all 100 responses are identical. **This is the single most important test in the whole suite** — it's the literal thesis of the project.
3. **Ambiguous timeout → reconciliation resolves it**: `X-Simulate-Outcome: timeout` → charge is marked `requires_reconciliation` → run the reconciliation job → assert it correctly resolves against the simulator's actual (delayed) outcome.
4. **Webhook retry and DLQ**: stop the fake-merchant-server → trigger a payment event → assert retries happen with backoff → restart the server → assert eventual delivery. Separately, test the DLQ path by leaving it down past the retry budget and asserting the event lands in the DLQ with an alert fired.
5. **OTP end-to-end with bounce**: request OTP to a normal test address (verify via Mailhog) and separately to SES's `bounce@simulator.amazonses.com` (in the staging/AWS-connected test tier) → assert the second address gets suppressed for future sends.
6. **Refund guard**: attempt to refund more than the original charge amount → assert rejection; attempt two concurrent partial refunds that together would exceed the charge → assert only the valid one succeeds.
7. **Dispute lifecycle**: inject a dispute via the simulator → assert ledger reversal entry appears → submit evidence → resolve as "won" → assert funds reversed back correctly.
8. **Fraud rule trip**: send a transaction engineered to trip a velocity rule (e.g., 20 rapid charges from one card) → assert the Nth one gets declined by the rule engine, not the ML model, and that the reason is logged.
9. **3DS step-up**: trigger a medium-risk score → assert `requires_action` response → hit the fake challenge endpoint → assert the original charge resumes and completes using the same idempotency key.
10. **Payout batch**: seed a merchant with a settled balance → run the payout job → assert correct ledger entries (merchant balance debited, "in transit" credited) and a payout record created.

### 25.5 CI pipeline integration

- **On every PR**: run the unit + integration test suite (fast, uses `testcontainers-go` for an ephemeral Postgres — no shared state between runs) plus scenarios 1-3 above from the E2E list (the highest-value, fastest ones).
- **Nightly / pre-release**: full E2E suite (all 10 scenarios) against the full docker-compose stack, plus a chaos run (§22.3) — kill a container mid-test and confirm recovery.
- **Load test as a separate, scheduled job** (not on every PR — too slow/expensive): `k6` script hammering the idempotency endpoint with concurrent duplicate keys at increasing volume, tracking p50/p99 latency and confirming zero duplicate charges even at your target load.

### 25.6 What real chaos/soak testing adds on top

The scripted E2E scenarios above are deterministic and fast — good for CI. Separately, run a **longer soak test** (hours, not seconds) with the Bank Simulator set to genuinely random outcomes (mix of success/decline/timeout/network-error) at sustained load, watching for things scripted tests miss: connection pool exhaustion over time, memory growth, Kafka consumer lag creeping up, and DLQ depth trending upward instead of staying near zero. This is what actually validates the reliability claims in §24, rather than just the individual failure-handling branches.

---

## 26. What to Actually Build (portfolio-realistic scope)

Building all of this literally is a multi-year team effort — pick a **vertical slice** that proves you understand the hard parts, and document the rest (like this doc) to show you've thought through the full system:

**Build (in Go, unless noted):**
1. Payments API + Idempotency Service (Postgres-backed, as designed above) — this is the star of the project.
2. Double-entry Ledger Service.
3. Webhook delivery worker with retry/backoff + signed payloads.
4. Auth service with real email verification (use SES sandbox or Mailhog locally) and JWT auth.
5. A simple "bank simulator" service that randomly succeeds/fails/delays/disputes, so you can actually demo retries, failures, and chargebacks end to end.
6. Reconciliation job comparing ledger vs bank simulator's transaction log.
7. **Rule-based risk engine** (§14.2) — genuinely buildable in a day or two and demos well: show a charge get declined live because it tripped a velocity rule.
8. **ML fraud model** (§14.3) — train an XGBoost model in a Jupyter notebook on a synthetic/Kaggle fraud dataset (e.g., the classic "Credit Card Fraud Detection" dataset on Kaggle), export it, serve it from a small Python FastAPI service behind gRPC with a strict timeout + fallback to the rule engine. You don't need real transaction volume to have a legitimate ML story — a well-documented offline evaluation (precision/recall, ROC curve) on a public dataset is exactly what interviewers expect from a portfolio project, not live-traffic-trained accuracy claims.
9. **Disputes + refunds** (§15, §17) — both are mostly ledger-entry logic you already have the primitives for; implementing them shows you understand the full charge lifecycle, not just the happy path.
10. **Payouts batch job** (§18) — even a simplified version (no reserves/negative-balance handling required for v1) closes the loop from "merchant gets paid" back to the ledger.
11. **Transactional outbox** (§22.1) for the ledger→Kafka publish path — cheap to implement, directly prevents the exact silent-inconsistency bug that idempotency alone doesn't cover.
12. **Concurrency test suite** (§22.3) — this is the highest-value/lowest-effort item on this entire list. A test (or `k6` script) that fires 100+ concurrent duplicate-idempotency-key requests and asserts exactly one charge was created is the single most convincing thing you can put in a README or show live in an interview.
13. **OTP/email delivery pipeline with real SQS + DLQ** (§23) — genuinely a half-day build (LocalStack for SQS locally, or real SQS on a free-tier AWS account) and it's the single clearest, most demoable illustration of "queue-based reliability" in the whole project: kill your email worker mid-send, watch SQS redeliver it; force N failures, watch it land in the DLQ and trigger an alert. This is a better live demo than almost anything else in the system because it's small enough to fully show in 2 minutes.
14. **A controllable Bank Simulator with test hooks** (§25.2), not just random success/failure — this is what makes everything else testable deterministically, and is arguably a prerequisite for building items 1-13 properly rather than an afterthought.
15. **The core E2E scenario suite** (§25.4), running against a docker-compose stack with LocalStack/Mailhog/Redpanda (§25.3) — this is what actually proves the system works end to end, and "here's my CI pipeline green with 10 E2E scenarios including 100-concurrent-duplicate-charges" is a stronger portfolio artifact than the architecture doc itself.

**Deploy:** Docker Compose locally is fine for the repo; deploy a real version to a single EKS cluster or even just ECS Fargate + RDS (cheaper) to have a live demo link — you don't need MSK/full Aurora at portfolio scale, use self-hosted Kafka (or Redpanda) and single-instance RDS Postgres, and **document** how you'd swap in MSK/Aurora at scale. Interviewers care that you know why and when, not that you paid for it.

**Load test:** Use `k6` to simulate concurrent duplicate requests hitting the idempotency endpoint — this is your best demo moment ("here's 1000 concurrent retries of the same idempotency key, and here's proof only one charge was created").

**README should include:** this architecture diagram, the idempotency sequence diagram, your Go-vs-Rust reasoning, and a "scaling to 10M users" section — hiring managers read READMEs, not just code.
