package webhook_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/testsupport"
	"github.com/likhith2366/paylo/internal/webhook"
)

// --- signing ---------------------------------------------------------------

func TestSignAndVerifyRoundTrip(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"payment.succeeded"}`)
	const secret = "whsec_test"

	sig := webhook.Sign(payload, secret, time.Now())
	if err := webhook.Verify(payload, sig, secret, 5*time.Minute); err != nil {
		t.Errorf("a freshly signed payload failed verification: %v", err)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	const secret = "whsec_test"
	sig := webhook.Sign([]byte(`{"amount":100}`), secret, time.Now())

	// The exact attack the signature exists to stop: a merchant must not accept
	// an amount someone changed in transit.
	if err := webhook.Verify([]byte(`{"amount":999999}`), sig, secret, 5*time.Minute); err == nil {
		t.Error("a tampered payload verified successfully")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	sig := webhook.Sign(payload, "whsec_real", time.Now())

	if err := webhook.Verify(payload, sig, "whsec_attacker", 5*time.Minute); err == nil {
		t.Error("a signature verified under the wrong secret")
	}
}

// Signing the timestamp alongside the body is what makes replay detectable.
// Without it a captured webhook would verify forever.
func TestVerifyRejectsStaleTimestamp(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	const secret = "whsec_test"

	old := webhook.Sign(payload, secret, time.Now().Add(-1*time.Hour))
	if err := webhook.Verify(payload, old, secret, 5*time.Minute); err == nil {
		t.Error("an hour-old signature was accepted; replays are possible")
	}

	// Still valid inside the tolerance.
	recent := webhook.Sign(payload, secret, time.Now().Add(-1*time.Minute))
	if err := webhook.Verify(payload, recent, secret, 5*time.Minute); err != nil {
		t.Errorf("a one-minute-old signature was rejected: %v", err)
	}
}

// testWorker permits loopback delivery, since httptest servers bind to
// 127.0.0.1. Production leaves AllowPrivateTargets false.
func testWorker(pool *pgxpool.Pool) *webhook.Worker {
	w := webhook.NewWorker(pool, 50, 5, time.Second)
	w.AllowPrivateTargets = true
	return w
}

// --- delivery --------------------------------------------------------------

// receiver is a fake merchant server (§25.1) that verifies signatures exactly
// as a real merchant would — a signing scheme never checked from the other side
// is a scheme that has not been tested.
type receiver struct {
	server   *httptest.Server
	mu       sync.Mutex
	received []map[string]any
	badSigs  int
	status   int
	secret   string
}

func newReceiver(t *testing.T, secret string) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK, secret: secret}

	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := make([]byte, req.ContentLength)
		_, _ = req.Body.Read(body)

		if err := webhook.Verify(body, req.Header.Get(webhook.SignatureHeader),
			r.secret, 5*time.Minute); err != nil {
			r.mu.Lock()
			r.badSigs++
			r.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var event map[string]any
		_ = json.Unmarshal(body, &event)

		r.mu.Lock()
		r.received = append(r.received, event)
		status := r.status
		r.mu.Unlock()

		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.received)
}

// distinctEventIDs counts unique event ids across everything received. The
// at-least-once contract only works if a redelivery is recognisable as one.
func (r *receiver) distinctEventIDs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]struct{}{}
	for _, e := range r.received {
		if id, ok := e["id"].(string); ok {
			seen[id] = struct{}{}
		}
	}
	return len(seen)
}

func (r *receiver) setStatus(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = code
}

// seed creates a merchant, a succeeded charge, an endpoint, and an outbox event.
func seed(t *testing.T, pool *pgxpool.Pool, url, secret string) (merchantID, chargeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	merchantID = testsupport.CreateMerchant(t, pool, "acme")

	err := pool.QueryRow(ctx, `
		INSERT INTO charges (merchant_id, amount_cents, currency, status)
		VALUES ($1, 10000, 'USD', 'succeeded') RETURNING id`,
		merchantID,
	).Scan(&chargeID)
	if err != nil {
		t.Fatalf("seed charge: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_endpoints (merchant_id, url, secret)
		VALUES ($1, $2, $3)`,
		merchantID, url, secret,
	); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO outbox_events (aggregate_id, event_type, payload)
		VALUES ($1, 'payment.succeeded', $2)`,
		chargeID, []byte(`{"id":"ch_1","amount":10000}`),
	); err != nil {
		t.Fatalf("seed outbox event: %v", err)
	}
	return merchantID, chargeID
}

func TestOutboxEventIsDeliveredAndVerified(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()

	const secret = "whsec_delivery"
	rcv := newReceiver(t, secret)
	seed(t, pool, rcv.server.URL, secret)

	n, err := webhook.NewPoller(pool, 100, time.Second).Drain(ctx)
	if err != nil {
		t.Fatalf("drain outbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("drained %d events, want 1", n)
	}

	if _, err := testWorker(pool).DeliverBatch(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if rcv.count() != 1 {
		t.Fatalf("merchant received %d events, want 1", rcv.count())
	}
	if rcv.badSigs != 0 {
		t.Errorf("%d deliveries failed signature verification", rcv.badSigs)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM webhook_deliveries`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" {
		t.Errorf("delivery status = %q, want delivered", status)
	}
}

// The outbox poller must be safe to re-run. A crash between fanning out and
// marking published would otherwise duplicate every delivery.
func TestDrainingTwiceDoesNotDuplicateDeliveries(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()

	rcv := newReceiver(t, "whsec_dupe")
	_, chargeID := seed(t, pool, rcv.server.URL, "whsec_dupe")

	poller := webhook.NewPoller(pool, 100, time.Second)
	if _, err := poller.Drain(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}

	// Simulate the crash: the fan-out committed but the row was not marked.
	if _, err := pool.Exec(ctx,
		`UPDATE outbox_events SET published = false WHERE aggregate_id = $1`, chargeID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := poller.Drain(ctx); err != nil {
		t.Fatalf("second drain: %v", err)
	}

	var deliveries int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Errorf("%d deliveries queued after a re-drain, want 1", deliveries)
	}
}

// A merchant being down for a while is normal, not exceptional (§24.4). The
// event must survive and be delivered once they recover.
func TestFailedDeliveryRetriesAndEventuallySucceeds(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()

	const secret = "whsec_retry"
	rcv := newReceiver(t, secret)
	rcv.setStatus(http.StatusInternalServerError) // merchant is down
	seed(t, pool, rcv.server.URL, secret)

	if _, err := webhook.NewPoller(pool, 100, time.Second).Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	worker := testWorker(pool)
	if _, err := worker.DeliverBatch(ctx); err != nil {
		t.Fatalf("first attempt: %v", err)
	}

	var status string
	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM webhook_deliveries`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("status = %q after a failure, want pending (a retry must be scheduled)", status)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}

	// The merchant comes back. Make the row due again, as backoff would.
	rcv.setStatus(http.StatusOK)
	if _, err := pool.Exec(ctx,
		`UPDATE webhook_deliveries SET next_attempt_at = now() - INTERVAL '1 second'`,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := worker.DeliverBatch(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM webhook_deliveries`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" {
		t.Errorf("status = %q after recovery, want delivered", status)
	}

	// The merchant received it TWICE — once for the attempt they 500'd, once on
	// retry. That is the at-least-once contract working as designed, not a bug:
	// we cannot tell "they never got it" from "they got it and then failed", and
	// giving up would be worse than delivering twice.
	//
	// What must hold is that both carry the SAME event id, so the merchant can
	// deduplicate. That is the guarantee we actually make (§7).
	if rcv.count() != 2 {
		t.Errorf("merchant received %d deliveries, want 2 (the failure and the retry)", rcv.count())
	}
	if n := rcv.distinctEventIDs(); n != 1 {
		t.Errorf("merchant saw %d distinct event ids, want 1 — retries must be dedupable", n)
	}
}

// Past the retry budget a delivery lands in the DLQ rather than retrying
// forever, and is loud enough to alert on (§7).
func TestExhaustedRetriesLandInTheDeadLetterQueue(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()

	const secret = "whsec_dead"
	rcv := newReceiver(t, secret)
	rcv.setStatus(http.StatusInternalServerError)
	seed(t, pool, rcv.server.URL, secret)

	if _, err := webhook.NewPoller(pool, 100, time.Second).Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	worker := testWorker(pool)
	for i := 0; i < webhook.MaxAttempts; i++ {
		if _, err := pool.Exec(ctx,
			`UPDATE webhook_deliveries SET next_attempt_at = now() - INTERVAL '1 second'
			 WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.DeliverBatch(ctx); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}

	var status string
	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM webhook_deliveries`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "dead" {
		t.Errorf("status = %q after %d attempts, want dead", status, attempts)
	}

	// Every attempt must be logged for the merchant's delivery log.
	var recorded int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_delivery_attempts`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded < webhook.MaxAttempts {
		t.Errorf("%d attempts recorded, want at least %d", recorded, webhook.MaxAttempts)
	}
}

// A dead delivery must not be picked up again, or the DLQ is not a DLQ.
func TestDeadDeliveriesAreNotRetried(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()

	rcv := newReceiver(t, "whsec_stop")
	seed(t, pool, rcv.server.URL, "whsec_stop")

	if _, err := webhook.NewPoller(pool, 100, time.Second).Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE webhook_deliveries SET status = 'dead', next_attempt_at = now() - INTERVAL '1 hour'`,
	); err != nil {
		t.Fatal(err)
	}

	n, err := testWorker(pool).DeliverBatch(ctx)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n != 0 {
		t.Errorf("worker claimed %d dead deliveries, want 0", n)
	}
	if rcv.count() != 0 {
		t.Errorf("a dead delivery was sent to the merchant")
	}
}

// Rotation needs a window where both secrets verify, or rotating breaks every
// in-flight delivery (§22.2).
func TestSecretRotationKeepsOldSignatureValid(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()

	const oldSecret = "whsec_old"
	const newSecret = "whsec_new"

	// The merchant has NOT updated their config yet — still verifying with old.
	rcv := newReceiver(t, oldSecret)
	merchantID, _ := seed(t, pool, rcv.server.URL, oldSecret)

	// We rotate: new secret signs, old one still included.
	if _, err := pool.Exec(ctx, `
		UPDATE webhook_endpoints
		SET secret = $2, previous_secret = $3,
		    previous_secret_expires_at = now() + INTERVAL '24 hours'
		WHERE merchant_id = $1`,
		merchantID, newSecret, oldSecret,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := webhook.NewPoller(pool, 100, time.Second).Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := testWorker(pool).DeliverBatch(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if rcv.badSigs != 0 {
		t.Errorf("%d signature failures during rotation — the old secret should still verify", rcv.badSigs)
	}
	if rcv.count() != 1 {
		t.Errorf("merchant received %d events, want 1", rcv.count())
	}
}

// Merchants can register a URL, so the worker must not be usable as a proxy
// into internal infrastructure.
func TestInternalAddressesAreRejected(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()

	seed(t, pool, "http://127.0.0.1:9999/hook", "whsec_ssrf")

	if _, err := webhook.NewPoller(pool, 100, time.Second).Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Guard deliberately left ON here — this is the test for it.
	if _, err := webhook.NewWorker(pool, 50, 5, time.Second).DeliverBatch(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT last_error FROM webhook_deliveries`).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if lastError == nil {
		t.Fatal("a loopback endpoint was delivered to without error")
	}
	if !contains(*lastError, "not routable") {
		t.Errorf("last_error = %q, want an SSRF rejection", *lastError)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
