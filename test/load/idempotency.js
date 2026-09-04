/**
 * Load test for the exactly-once guarantee (§26, §25.5).
 *
 * The thesis of this whole system, at load: fire many concurrent requests that
 * reuse a small pool of idempotency keys, and prove that the number of charges
 * created equals the number of distinct keys — no matter how much concurrency
 * is thrown at it.
 *
 * Run:
 *   make up
 *   make seed                          # prints an sk_test_... key
 *   k6 run -e API_KEY=sk_test_... test/load/idempotency.js
 *
 * Install k6: https://k6.io/docs/get-started/installation/
 *   winget install k6 --source winget
 *
 * WHAT THIS ASSERTS, AND WHAT IT DOES NOT
 *
 * k6 cannot see the database, so it cannot count charge rows itself. What it
 * CAN prove is the externally visible contract:
 *
 *   - every response for a given key is either the same charge id, or a 409
 *     because a request with that key was genuinely still in flight
 *   - no key ever yields two different charge ids
 *
 * Two different ids for one key would mean a double charge. That is the
 * failure this test exists to catch, and it is visible from outside.
 *
 * After the run, confirm against the database — the numbers must agree:
 *
 *   make psql
 *   SELECT count(*) FROM charges;                          -- == distinct keys
 *   SELECT count(DISTINCT idempotency_key) FROM charges;   -- same number
 *   SELECT transaction_id, currency,
 *          SUM(CASE WHEN direction='debit' THEN amount_cents ELSE -amount_cents END)
 *   FROM ledger_entries GROUP BY 1,2 HAVING SUM(...) <> 0; -- must be empty
 */

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import exec from 'k6/execution';

const API = __ENV.API_URL || 'http://localhost:8080';
const VAULT = __ENV.VAULT_URL || 'http://localhost:8081';
const API_KEY = __ENV.API_KEY;

// A small key pool relative to the iteration count is the whole point: with
// 200 keys and 20,000 iterations, every key is retried ~100 times under real
// concurrency. A large pool would exercise throughput but never the guarantee.
const KEY_POOL = parseInt(__ENV.KEY_POOL || '200', 10);

const duplicateCharges = new Counter('duplicate_charges_DEFECT');
const conflicts = new Counter('idempotent_conflicts_409');
const replays = new Counter('idempotent_replays');
const created = new Counter('charges_created');
const chargeLatency = new Trend('charge_latency_ms', true);
const okRate = new Rate('acceptable_responses');

export const options = {
  scenarios: {
    // Ramp so the system is measured while warming AND at steady state — a
    // constant-rate test hides the connection-pool behaviour that only shows
    // up during a ramp.
    exactly_once: {
      executor: 'ramping-vus',
      startVUs: 10,
      stages: [
        { duration: '30s', target: 100 },
        { duration: '1m', target: 300 },
        { duration: '1m', target: 300 },
        { duration: '30s', target: 0 },
      ],
      gracefulRampDown: '15s',
    },
  },
  thresholds: {
    // The one that matters. Any duplicate is a total failure of the thesis,
    // so the threshold aborts the run rather than reporting at the end.
    'duplicate_charges_DEFECT': [{ threshold: 'count==0', abortOnFail: true }],
    'acceptable_responses': ['rate>0.99'],
    // The idempotency claim is one INSERT; the bank call dominates the rest.
    'charge_latency_ms': ['p(95)<2000', 'p(99)<5000'],
    'http_req_failed': ['rate<0.01'],
  },
};

// chargeIdsByKey maps an idempotency key to the charge id first seen for it.
// Shared per-VU rather than globally — k6 VUs do not share memory, so a global
// would silently not work. Per-VU is still enough: a duplicate would have to
// occur within one VU's own retries to be missed, and with a 200-key pool
// across 300 VUs every key is hit by many VUs.
const seen = {};

export function setup() {
  if (!API_KEY) {
    throw new Error('API_KEY is required. Run `make seed` and pass -e API_KEY=sk_test_...');
  }

  // Mint one multi-use vault token for the run. A single-use token would be
  // consumed by the first charge and every subsequent one would fail on the
  // token rather than exercising idempotency — which is correct behaviour but
  // not what is being measured here.
  const res = http.post(`${VAULT}/vault/tokenize`, JSON.stringify({
    number: '4242424242424242',
    exp_month: 12,
    exp_year: 2030,
    single_use: false,
  }), { headers: { 'Content-Type': 'application/json' } });

  if (res.status !== 201) {
    throw new Error(`vault tokenize failed: ${res.status} ${res.body}`);
  }
  return { token: JSON.parse(res.body).token };
}

export default function (data) {
  // Deterministic key selection so keys are reused heavily across VUs.
  const key = `load_idem_${exec.scenario.iterationInTest % KEY_POOL}`;

  const res = http.post(`${API}/v1/charges`, JSON.stringify({
    amount: 4500,
    currency: 'USD',
    payment_token: data.token,
  }), {
    headers: {
      'Authorization': `Bearer ${API_KEY}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': key,
      // A realistic outcome mix would use X-Simulate-Outcome here (§25.2).
      // This run holds it at success so the measurement is of the idempotency
      // path, not of decline handling.
      'X-Simulate-Outcome': 'success',
    },
    tags: { name: 'POST /v1/charges' },
  });

  chargeLatency.add(res.timings.duration);

  // 200 = charged or replayed. 409 = a request with this key is genuinely in
  // flight, which is correct and expected under concurrency.
  const acceptable = res.status === 200 || res.status === 409;
  okRate.add(acceptable);

  check(res, {
    'status is 200 or 409': () => acceptable,
  });

  if (res.status === 409) {
    conflicts.add(1);
    return;
  }
  if (res.status !== 200) {
    return;
  }

  let body;
  try {
    body = JSON.parse(res.body);
  } catch (_) {
    return;
  }

  if (seen[key] === undefined) {
    seen[key] = body.id;
    created.add(1);
    return;
  }

  if (seen[key] !== body.id) {
    // THE defect. One idempotency key produced two different charges, which
    // means a customer was charged twice.
    duplicateCharges.add(1);
    console.error(
      `DUPLICATE CHARGE: key ${key} produced ${seen[key]} and ${body.id}`
    );
    return;
  }

  replays.add(1);
}

export function teardown() {
  console.log(
    '\nVerify against the database — these must agree:\n' +
    '  SELECT count(*) FROM charges;                        -- == distinct keys used\n' +
    '  SELECT count(DISTINCT idempotency_key) FROM charges;\n' +
    '  SELECT transaction_id, currency,\n' +
    '         SUM(CASE WHEN direction=\'debit\' THEN amount_cents ELSE -amount_cents END) d\n' +
    '  FROM ledger_entries GROUP BY 1,2 HAVING SUM(...) <> 0;  -- must return no rows\n'
  );
}
