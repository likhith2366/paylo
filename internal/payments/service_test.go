package payments_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/idempotency"
	"github.com/likhith2366/paylo/internal/payments"
	"github.com/likhith2366/paylo/internal/testsupport"
)

const testSalt = "test-salt"

// fakeVault stands in for the vault service. It holds card numbers in memory
// so tests can drive the charge flow without running the vault, while keeping
// the same contract: metadata is freely readable, single-use tokens are
// consumed exactly once, and the PAN is only available via Detokenize.
type fakeVault struct {
	mu              sync.Mutex
	cards           map[string]string // token -> PAN
	consumed        map[string]bool
	detokenizeCalls atomic.Int64
}

func newFakeVault() *fakeVault {
	return &fakeVault{cards: map[string]string{}, consumed: map[string]bool{}}
}

// issue mints a token for a card, mirroring what the vault's tokenize endpoint
// would return.
func (f *fakeVault) issue(token, pan string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards[token] = pan
	return token
}

func (f *fakeVault) Metadata(_ context.Context, token string) (*payments.CardMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pan, ok := f.cards[token]
	if !ok {
		return nil, payments.ErrTokenUnusable
	}
	// Rejecting a consumed token here mirrors the real vault. An earlier
	// version of this fake did not, and that single divergence hid a live bug:
	// the retry path called Metadata before claiming the idempotency key, so a
	// retry failed against the real vault while passing against this fake.
	// A fake more permissive than the real thing is worse than no fake.
	if f.consumed[token] {
		return nil, payments.ErrTokenUnusable
	}
	return &payments.CardMetadata{
		Brand:       string(payments.DetectNetwork(pan)),
		Last4:       payments.Last4(pan),
		BIN:         payments.BIN(pan),
		Fingerprint: payments.Fingerprint(pan, testSalt),
		ExpMonth:    12,
		ExpYear:     2030,
	}, nil
}

func (f *fakeVault) Detokenize(_ context.Context, token, _, _ string) (string, error) {
	f.detokenizeCalls.Add(1)

	f.mu.Lock()
	defer f.mu.Unlock()
	pan, ok := f.cards[token]
	if !ok {
		return "", payments.ErrTokenUnusable
	}
	if f.consumed[token] {
		return "", payments.ErrTokenUnusable
	}
	f.consumed[token] = true
	return pan, nil
}

// fakeBank counts how many times it was actually asked to authorize a charge.
// This counter is the real assertion in the concurrency test: no matter what
// the API returns, the customer's card must be hit exactly once.
type fakeBank struct {
	server      *httptest.Server
	authCalls   atomic.Int64
	refundCalls atomic.Int64
	delay       time.Duration
}

func newFakeBank(t *testing.T, delay time.Duration) *fakeBank {
	t.Helper()
	fb := &fakeBank{delay: delay}

	mux := http.NewServeMux()
	mux.HandleFunc("/simulator/charge", func(w http.ResponseWriter, r *http.Request) {
		fb.authCalls.Add(1)
		// A deliberate delay widens the window in which concurrent duplicates
		// can collide, making the race far more likely to be exercised.
		if fb.delay > 0 {
			time.Sleep(fb.delay)
		}
		if r.Header.Get("X-Simulate-Outcome") == "decline" {
			writeJSON(w, map[string]any{
				"processor_reference": "bank_declined",
				"status":              "declined",
				"decline_code":        "insufficient_funds",
				"decline_message":     "The card has insufficient funds.",
			})
			return
		}
		writeJSON(w, map[string]any{
			"processor_reference": "bank_" + uuid.NewString(),
			"status":              "authorized",
			"authorized_at":       time.Now().UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/simulator/refund", func(w http.ResponseWriter, r *http.Request) {
		fb.refundCalls.Add(1)
		if fb.delay > 0 {
			time.Sleep(fb.delay)
		}
		if r.Header.Get("X-Simulate-Outcome") == "decline" {
			writeJSON(w, map[string]any{
				"status": "failed", "failure_code": "refund_not_permitted",
			})
			return
		}
		writeJSON(w, map[string]any{
			"refund_reference": "rfnd_" + uuid.NewString(),
			"status":           "succeeded",
		})
	})

	fb.server = httptest.NewServer(mux)
	t.Cleanup(fb.server.Close)
	return fb
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func newService(t *testing.T, pool *pgxpool.Pool, bank *fakeBank, v *fakeVault) *payments.Service {
	t.Helper()
	return payments.NewService(pool, payments.NewBankClient(bank.server.URL, 10*time.Second), v, nil)
}

func chargeInput(merchantID uuid.UUID, token string) payments.ChargeInput {
	return payments.ChargeInput{
		MerchantID:   merchantID,
		AmountCents:  10_000,
		Currency:     "USD",
		PaymentToken: token,
	}
}

// TestConcurrentDuplicateIdempotencyKey is the thesis of this entire project.
//
// 100 goroutines fire the identical charge with the identical idempotency key
// at the same instant. Exactly one customer charge must result.
//
// Note what is asserted and what is not. The design doc suggests all 100
// responses should be identical; that is true of *sequential* retries (covered
// by the test below), but not of a simultaneous burst. A request that arrives
// while another is genuinely mid-flight cannot be given a response that does
// not exist yet, so it receives 409 Conflict — which is exactly what Stripe
// does. The invariant that actually matters, and that is asserted here, is
// that the bank was called once and one charge exists.
func TestConcurrentDuplicateIdempotencyKey(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	// A slow bank keeps the winner "processing" while the other 99 arrive.
	bank := newFakeBank(t, 150*time.Millisecond)
	vlt := newFakeVault()
	token := vlt.issue("tok_concurrent", "4242424242424242")
	svc := newService(t, pool, bank, vlt)

	const attempts = 100
	const idemKey = "idem_concurrent_burst_001"

	body := []byte(`{"amount":10000,"currency":"USD","card":{"number":"4242424242424242"}}`)
	requestHash, err := idempotency.HashRequest(body)
	if err != nil {
		t.Fatalf("hash request: %v", err)
	}

	type result struct {
		charge *payments.Charge
		status int
		err    error
	}
	results := make([]result, attempts)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all at once
			c, status, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), idemKey, requestHash, body)
			results[i] = result{charge: c, status: status, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	// --- the assertion that matters -----------------------------------------
	if got := bank.authCalls.Load(); got != 1 {
		t.Fatalf("bank was asked to authorize %d times, want exactly 1 — "+
			"a customer would have been charged %d times", got, got)
	}

	var succeeded, inFlight, other int
	var chargeIDs = map[uuid.UUID]bool{}
	for i, r := range results {
		switch {
		case r.err == nil && r.charge != nil:
			succeeded++
			chargeIDs[r.charge.ID] = true
		case errors.Is(r.err, idempotency.ErrInFlight):
			inFlight++
		default:
			other++
			t.Errorf("attempt %d: unexpected error: %v", i, r.err)
		}
	}

	if other != 0 {
		t.Fatalf("%d attempts failed unexpectedly", other)
	}
	if succeeded+inFlight != attempts {
		t.Fatalf("accounted for %d of %d attempts", succeeded+inFlight, attempts)
	}
	if len(chargeIDs) > 1 {
		t.Errorf("attempts produced %d distinct charge IDs, want at most 1", len(chargeIDs))
	}

	// Every successful response must describe the same charge.
	var first *payments.Charge
	for _, r := range results {
		if r.err != nil || r.charge == nil {
			continue
		}
		if first == nil {
			first = r.charge
			continue
		}
		if r.charge.ID != first.ID || r.charge.AmountCents != first.AmountCents ||
			r.charge.Status != first.Status {
			t.Errorf("responses diverge: %+v vs %+v", first, r.charge)
		}
	}

	// --- and the database agrees --------------------------------------------
	assertSingleCharge(t, pool, merchantID)
	assertLedgerBalanced(t, pool)

	t.Logf("%d concurrent attempts → 1 bank authorization, %d completed, %d got 409",
		attempts, succeeded, inFlight)
}

// TestSequentialRetryReplaysIdenticalResponse covers the case the idempotency
// design actually exists for: a client whose response was lost to a network
// blip retries with the same key (§4.1). It must get the original response
// back, byte for byte, without the card being charged again.
func TestSequentialRetryReplaysIdenticalResponse(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	bank := newFakeBank(t, 0)
	vlt := newFakeVault()
	token := vlt.issue("tok_retry", "4242424242424242")
	svc := newService(t, pool, bank, vlt)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)
	const idemKey = "idem_sequential_retry_001"

	first, firstStatus, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), idemKey, hash, body)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if first.Status != payments.StatusSucceeded {
		t.Fatalf("first attempt status = %q, want succeeded", first.Status)
	}

	// The client never saw that response. It retries, five times over.
	for i := 0; i < 5; i++ {
		replay, replayStatus, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), idemKey, hash, body)
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if replay.ID != first.ID {
			t.Errorf("retry %d returned charge %s, want %s", i, replay.ID, first.ID)
		}
		if replayStatus != firstStatus {
			t.Errorf("retry %d returned HTTP %d, want %d", i, replayStatus, firstStatus)
		}
		if replay.Status != first.Status || replay.AmountCents != first.AmountCents {
			t.Errorf("retry %d response differs from the original", i)
		}
	}

	if got := bank.authCalls.Load(); got != 1 {
		t.Errorf("bank called %d times across 6 requests, want 1", got)
	}
	assertSingleCharge(t, pool, merchantID)
}

// Regression: a retry must replay even though the first attempt consumed the
// single-use vault token.
//
// This failed once. CreateCharge fetched vault metadata before claiming the
// idempotency key, so a retry hit "token already consumed" — a precondition the
// FIRST attempt had legitimately changed — and returned an error instead of the
// original response. A merchant retrying a lost response would have concluded
// the charge failed when it had actually succeeded.
//
// The rule this pins: the idempotency claim is the outermost operation, and a
// retry re-executes no precondition that the first attempt could have altered.
func TestRetryReplaysAfterSingleUseTokenConsumed(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	bank := newFakeBank(t, 0)
	vlt := newFakeVault()
	token := vlt.issue("tok_single_use", "4242424242424242")
	svc := newService(t, pool, bank, vlt)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)
	const idemKey = "idem_token_consumed_001"

	first, firstStatus, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), idemKey, hash, body)
	if err != nil {
		t.Fatalf("first charge: %v", err)
	}
	if first.Status != payments.StatusSucceeded {
		t.Fatalf("first charge status = %q, want succeeded", first.Status)
	}

	// The token is now spent. The retry must still replay the original response.
	replay, replayStatus, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), idemKey, hash, body)
	if err != nil {
		t.Fatalf("retry after the token was consumed must replay, got error: %v", err)
	}
	if replay.ID != first.ID {
		t.Errorf("retry returned charge %s, want %s", replay.ID, first.ID)
	}
	if replay.Status != payments.StatusSucceeded || replayStatus != firstStatus {
		t.Errorf("retry returned %q/%d, want %q/%d",
			replay.Status, replayStatus, first.Status, firstStatus)
	}

	// The replay must not have consulted the vault at all.
	if got := vlt.detokenizeCalls.Load(); got != 1 {
		t.Errorf("vault detokenize called %d times, want 1", got)
	}
	if got := bank.authCalls.Load(); got != 1 {
		t.Errorf("bank called %d times, want 1", got)
	}
	assertSingleCharge(t, pool, merchantID)
}

// An unusable token must produce a terminal, replayable failure rather than
// leaving the claimed key stuck in 'processing' until its lock goes stale.
func TestUnusableTokenFailsTerminally(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	svc := newService(t, pool, newFakeBank(t, 0), newFakeVault())

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)

	_, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, "tok_never_issued"), "bad_token_key", hash, body)
	if !errors.Is(err, payments.ErrTokenUnusable) {
		t.Fatalf("expected ErrTokenUnusable, got %v", err)
	}

	// The immediate retry must replay the failure, not return 409 in-flight.
	charge, status, err := svc.CreateCharge(ctx, chargeInput(merchantID, "tok_never_issued"), "bad_token_key", hash, body)
	if err != nil {
		t.Fatalf("retry should replay the stored failure, got: %v", err)
	}
	if charge.Status != payments.StatusFailed {
		t.Errorf("replayed status = %q, want failed", charge.Status)
	}
	if status != 400 {
		t.Errorf("replayed HTTP status = %d, want 400", status)
	}
}

// Reusing a key for a genuinely different request is a client bug, and
// replaying the old response would silently hide it (§4.2).
func TestKeyReuseWithDifferentBodyRejected(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	token := vlt.issue("tok_reuse", "4242424242424242")
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	bodyA := []byte(`{"amount":10000,"currency":"USD"}`)
	hashA, _ := idempotency.HashRequest(bodyA)
	if _, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "reused_key", hashA, bodyA); err != nil {
		t.Fatalf("first charge: %v", err)
	}

	bodyB := []byte(`{"amount":99999,"currency":"USD"}`)
	hashB, _ := idempotency.HashRequest(bodyB)
	in := chargeInput(merchantID, token)
	in.AmountCents = 99_999

	_, _, err := svc.CreateCharge(ctx, in, "reused_key", hashB, bodyB)
	if !errors.Is(err, idempotency.ErrKeyReused) {
		t.Fatalf("expected ErrKeyReused, got %v", err)
	}
}

// A retry whose JSON keys were reordered by the client's HTTP library is the
// same request and must not be rejected as key reuse.
func TestCanonicalHashIgnoresKeyOrder(t *testing.T) {
	a, err := idempotency.HashRequest([]byte(`{"amount":100,"currency":"USD","card":{"number":"4242"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := idempotency.HashRequest([]byte(`{"card":{"number":"4242"},"currency":"USD","amount":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("reordering JSON keys changed the request hash — a legitimate retry would be rejected")
	}

	c, _ := idempotency.HashRequest([]byte(`{"amount":101,"currency":"USD"}`))
	if a == c {
		t.Error("a genuinely different body produced the same hash")
	}
}

// A declined charge is a completed request, not a failed one: the decline is
// the answer, and retrying the key must replay it rather than re-ask the bank.
func TestDeclinedChargeIsIdempotentAndPostsNoLedgerEntries(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	bank := newFakeBank(t, 0)
	vlt := newFakeVault()
	token := vlt.issue("tok_decline", "4242424242424242")
	svc := newService(t, pool, bank, vlt)

	in := chargeInput(merchantID, token)
	in.SimulateOutcome = "decline"
	body := []byte(`{"amount":10000,"currency":"USD","outcome":"decline"}`)
	hash, _ := idempotency.HashRequest(body)

	charge, status, err := svc.CreateCharge(ctx, in, "declined_key", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if charge.Status != payments.StatusFailed {
		t.Errorf("status = %q, want failed", charge.Status)
	}
	if status != 402 {
		t.Errorf("HTTP status = %d, want 402", status)
	}

	// A declined charge moved no money, so it must have written no ledger entries.
	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Errorf("a declined charge wrote %d ledger entries, want 0", entries)
	}

	if _, _, err := svc.CreateCharge(ctx, in, "declined_key", hash, body); err != nil {
		t.Fatalf("retry of declined charge: %v", err)
	}
	if got := bank.authCalls.Load(); got != 1 {
		t.Errorf("bank called %d times, want 1 — the decline should have replayed", got)
	}
}

// A successful charge must leave the platform's books balanced: the fee and
// the merchant's net together account for exactly the amount charged.
func TestSuccessfulChargePostsBalancedLedger(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	token := vlt.issue("tok_ledger", "4242424242424242")
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)

	charge, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "ledger_key", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if charge.LedgerTxnID == nil {
		t.Fatal("successful charge has no ledger transaction")
	}

	rows, err := pool.Query(ctx, `
		SELECT a.account_type, e.direction, e.amount_cents
		FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
		WHERE e.transaction_id = $1
		ORDER BY a.account_type`, *charge.LedgerTxnID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	byAccount := map[string]int64{}
	var net int64
	for rows.Next() {
		var accountType, direction string
		var amount int64
		if err := rows.Scan(&accountType, &direction, &amount); err != nil {
			t.Fatal(err)
		}
		byAccount[accountType] = amount
		if direction == "debit" {
			net += amount
		} else {
			net -= amount
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if net != 0 {
		t.Errorf("transaction is off by %d cents", net)
	}
	if byAccount["customer_receivable"] != 10_000 {
		t.Errorf("receivable = %d, want 10000", byAccount["customer_receivable"])
	}
	// 290 bps of 10000 = 290, leaving 9710 for the merchant.
	if byAccount["platform_fees"] != 290 {
		t.Errorf("platform fee = %d, want 290", byAccount["platform_fees"])
	}
	if byAccount["merchant_balance"] != 9_710 {
		t.Errorf("merchant net = %d, want 9710", byAccount["merchant_balance"])
	}
}

// A charge whose outcome is unknown must be parked for reconciliation, never
// marked failed — the money may well have moved (§24.1).
func TestAmbiguousTimeoutMarksChargeForReconciliation(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	// A bank that authorizes but never answers in time.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		writeJSON(w, map[string]any{"processor_reference": "bank_x", "status": "authorized"})
	}))
	defer slow.Close()

	vlt := newFakeVault()
	token := vlt.issue("tok_ambiguous", "4242424242424242")
	svc := payments.NewService(pool, payments.NewBankClient(slow.URL, 300*time.Millisecond), vlt, nil)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)

	charge, status, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "ambiguous_key", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if charge.Status != payments.StatusRequiresReconciliation {
		t.Errorf("status = %q, want requires_reconciliation", charge.Status)
	}
	if status != 202 {
		t.Errorf("HTTP status = %d, want 202", status)
	}

	// Critically, no ledger entries: we must not book money we cannot confirm.
	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Errorf("an unconfirmed charge wrote %d ledger entries, want 0", entries)
	}
}

// Every terminal charge must leave an outbox row, in the same transaction as
// the ledger write. This is what guarantees the webhook eventually fires even
// if the broker was down at the moment of the charge (§22.1).
func TestChargeWritesOutboxEvent(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	token := vlt.issue("tok_outbox", "4242424242424242")
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)
	charge, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "outbox_key", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	var eventType string
	var published bool
	err = pool.QueryRow(ctx, `
		SELECT event_type, published FROM outbox_events WHERE aggregate_id = $1`,
		charge.ID,
	).Scan(&eventType, &published)
	if err != nil {
		t.Fatalf("no outbox event was written for the charge: %v", err)
	}
	if eventType != "payment.succeeded" {
		t.Errorf("event_type = %q, want payment.succeeded", eventType)
	}
	if published {
		t.Error("outbox event should start unpublished")
	}
}

// Two merchants using the same idempotency key must not collide — keys are
// scoped per merchant, and a shared namespace would let one merchant's key
// replay another's response.
func TestIdempotencyKeysAreScopedPerMerchant(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantA := testsupport.CreateMerchant(t, pool, "alpha")
	merchantB := testsupport.CreateMerchant(t, pool, "beta")

	bank := newFakeBank(t, 0)
	vlt := newFakeVault()
	tokenA := vlt.issue("tok_alpha", "4242424242424242")
	tokenB := vlt.issue("tok_beta", "5555555555554444")
	svc := newService(t, pool, bank, vlt)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)

	chargeA, _, err := svc.CreateCharge(ctx, chargeInput(merchantA, tokenA), "shared_key", hash, body)
	if err != nil {
		t.Fatalf("merchant A: %v", err)
	}
	chargeB, _, err := svc.CreateCharge(ctx, chargeInput(merchantB, tokenB), "shared_key", hash, body)
	if err != nil {
		t.Fatalf("merchant B: %v", err)
	}

	if chargeA.ID == chargeB.ID {
		t.Error("two merchants sharing a key produced the same charge")
	}
	if got := bank.authCalls.Load(); got != 2 {
		t.Errorf("bank called %d times, want 2 — both merchants should be charged", got)
	}
}

func assertSingleCharge(t *testing.T, pool *pgxpool.Pool, merchantID uuid.UUID) {
	t.Helper()

	var total int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM charges WHERE merchant_id = $1`, merchantID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("%d charge rows exist, want exactly 1", total)
	}

	var txns int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(DISTINCT transaction_id) FROM ledger_entries`).Scan(&txns); err != nil {
		t.Fatal(err)
	}
	if txns > 1 {
		t.Errorf("%d distinct ledger transactions exist, want at most 1", txns)
	}
}

// assertLedgerBalanced re-derives the §5 invariant across every transaction,
// which is the same query the reconciliation job runs (§24.2).
func assertLedgerBalanced(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	rows, err := pool.Query(context.Background(), `
		SELECT transaction_id, currency,
		       SUM(CASE WHEN direction = 'debit' THEN amount_cents ELSE -amount_cents END)
		FROM ledger_entries
		GROUP BY transaction_id, currency
		HAVING SUM(CASE WHEN direction = 'debit' THEN amount_cents ELSE -amount_cents END) <> 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var problems []string
	for rows.Next() {
		var txnID uuid.UUID
		var currency string
		var delta int64
		if err := rows.Scan(&txnID, &currency, &delta); err != nil {
			t.Fatal(err)
		}
		problems = append(problems, fmt.Sprintf("%s (%s) off by %d", txnID, currency, delta))
	}
	if len(problems) > 0 {
		t.Errorf("unbalanced ledger transactions: %v", problems)
	}
}
