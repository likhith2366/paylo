package payments_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/likhith2366/paylo/internal/idempotency"
	"github.com/likhith2366/paylo/internal/payments"
	"github.com/likhith2366/paylo/internal/testsupport"
)

// Regression for the most serious bug found in architecture review.
//
// A refund whose processor call times out is left `requires_reconciliation` —
// the money MAY have moved. The reservation view originally counted only
// 'succeeded' and 'pending' toward the committed total, so that refund released
// its capacity and a second full refund under a different key passed the
// remaining-amount check. A 10000 charge could be refunded 20000.
//
// The ledger stayed balanced the whole time (only the second refund posted
// legs), so no balance assertion could have caught it. It is the same rule the
// charge path already follows: ambiguous is not failed.
func TestAmbiguousRefundKeepsItsReservation(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	vlt := newFakeVault()
	fastBank := newFakeBank(t, 0)
	svc := newService(t, pool, fastBank, vlt)
	charge := seedCharge(t, svc, vlt, merchantID, "tok_ambiguous_refund")

	// A processor that never answers in time for the refund call.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"status":"succeeded"}`))
	}))
	defer slow.Close()

	slowSvc := payments.NewService(pool,
		payments.NewBankClient(slow.URL, 200*time.Millisecond), vlt, nil)

	body := []byte(`{"amount":10000}`)
	hash, _ := idempotency.HashRequest(body)
	first, status, err := slowSvc.CreateRefund(ctx,
		refundInput(merchantID, charge.ID, 10_000), "ambiguous_key", hash)
	if err != nil {
		t.Fatalf("first refund: %v", err)
	}
	if first.Status != payments.RefundStatusRequiresReconciliation {
		t.Fatalf("status = %q, want requires_reconciliation", first.Status)
	}
	if status != 202 {
		t.Errorf("HTTP status = %d, want 202", status)
	}

	// The whole charge is now spoken for, even though we cannot confirm it.
	_, remaining, err := slowSvc.RefundedTotal(ctx, charge.ID)
	if err != nil {
		t.Fatalf("refunded total: %v", err)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0 — an unconfirmed refund must hold its reservation", remaining)
	}

	// A genuinely different request, so idempotency does not replay it. It must
	// be rejected on the remaining-amount check.
	body2 := []byte(`{"amount":10000,"attempt":2}`)
	hash2, _ := idempotency.HashRequest(body2)
	_, _, err = svc.CreateRefund(ctx,
		refundInput(merchantID, charge.ID, 10_000), "second_key", hash2)

	if !errors.Is(err, payments.ErrRefundExceedsCharge) {
		t.Fatalf("a second full refund was allowed against an unresolved one: %v", err)
	}

	// And prove it in the data: nothing beyond the charge amount is committed.
	var committed, chargeAmount int64
	if err := pool.QueryRow(ctx,
		`SELECT committed_cents, amount_cents FROM charge_refund_totals WHERE charge_id = $1`,
		charge.ID,
	).Scan(&committed, &chargeAmount); err != nil {
		t.Fatal(err)
	}
	if committed > chargeAmount {
		t.Errorf("committed %d against a %d charge — over-refunded", committed, chargeAmount)
	}
}

// A dispute must not reverse a charge that never succeeded: the merchant would
// be debited the amount plus the fee for money they were never credited, and
// both entries would balance so the ledger invariant would not notice.
func TestCannotDisputeANonSucceededCharge(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	token := vlt.issue("tok_declined_dispute", "4242424242424242")
	in := chargeInput(merchantID, token)
	in.SimulateOutcome = "decline"
	body := []byte(`{"amount":10000,"outcome":"decline"}`)
	hash, _ := idempotency.HashRequest(body)

	charge, _, err := svc.CreateCharge(ctx, in, "declined_for_dispute", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	before := countLedgerEntries(t, pool)

	_, err = svc.OpenDispute(ctx, payments.OpenDisputeInput{
		ChargeID: charge.ID, Reason: "fraudulent", ProcessorReference: "dp_on_declined",
	})
	if !errors.Is(err, payments.ErrChargeNotDisputable) {
		t.Fatalf("expected ErrChargeNotDisputable, got %v", err)
	}
	if after := countLedgerEntries(t, pool); after != before {
		t.Errorf("disputing a declined charge wrote %d ledger entries", after-before)
	}
}

// The processor reference IS the dedupe key for processor-driven events. An
// empty one stores as NULL, and Postgres treats every NULL as distinct — so
// ON CONFLICT would never fire and a redelivered notification would reverse
// the funds a second time.
func TestDisputeRequiresAProcessorReference(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)
	charge := seedCharge(t, svc, vlt, merchantID, "tok_no_ref")

	_, err := svc.OpenDispute(ctx, payments.OpenDisputeInput{
		ChargeID: charge.ID, Reason: "fraudulent", ProcessorReference: "",
	})
	if !errors.Is(err, payments.ErrDisputeMissingReference) {
		t.Fatalf("expected ErrDisputeMissingReference, got %v", err)
	}

	// Without the guard, two reference-less notifications would each reverse.
	_, _ = svc.OpenDispute(ctx, payments.OpenDisputeInput{
		ChargeID: charge.ID, Reason: "fraudulent", ProcessorReference: "",
	})

	var disputes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM disputes`).Scan(&disputes); err != nil {
		t.Fatal(err)
	}
	if disputes != 0 {
		t.Errorf("%d disputes were opened without a reference, want 0", disputes)
	}
}

// flakyVault returns a transient (non-ErrTokenUnusable) error, which the fake
// vault could not previously express — and that gap hid a real bug.
type flakyVault struct {
	*fakeVault
	failMetadata bool
}

func (f *flakyVault) Metadata(ctx context.Context, token string) (*payments.CardMetadata, error) {
	if f.failMetadata {
		// What a restarting vault pod actually returns. Critically NOT
		// ErrTokenUnusable — the token is fine, the service is not.
		return nil, fmt.Errorf("vault client: metadata returned 503")
	}
	return f.fakeVault.Metadata(ctx, token)
}

// A transient vault outage must not be burned into a permanent failure.
//
// The old code recorded ANY metadata error as a terminal "invalid_payment_token"
// response. A client retrying with the same key — the correct behaviour — would
// then replay that stored 400 forever, so a valid token became permanently
// unusable under that key because the vault blinked for two seconds.
func TestTransientVaultFailureDoesNotPoisonTheIdempotencyKey(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	base := newFakeVault()
	token := base.issue("tok_flaky", "4242424242424242")
	vlt := &flakyVault{fakeVault: base, failMetadata: true}

	bank := newFakeBank(t, 0)
	svc := payments.NewService(pool,
		payments.NewBankClient(bank.server.URL, 10*time.Second), vlt, nil)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)
	const key = "flaky_vault_key"

	// The vault is down.
	if _, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), key, hash, body); err == nil {
		t.Fatal("expected an error while the vault was unavailable")
	}

	// The claim must not have been left behind as a terminal failure.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE idempotency_key = $1`, key,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		var status string
		_ = pool.QueryRow(ctx,
			`SELECT status FROM idempotency_keys WHERE idempotency_key = $1`, key).Scan(&status)
		t.Fatalf("the claim survived a transient failure as %q; a retry would replay it forever", status)
	}

	// The vault recovers and the SAME key now works.
	vlt.failMetadata = false
	charge, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), key, hash, body)
	if err != nil {
		t.Fatalf("retry after recovery failed: %v", err)
	}
	if charge.Status != payments.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", charge.Status)
	}
}

// A terminal charge must stay terminal. Enforced by a database trigger, so a
// bug in Go cannot move a succeeded charge backwards.
func TestTerminalChargeStatusIsImmutable(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)
	charge := seedCharge(t, svc, vlt, merchantID, "tok_terminal")

	if _, err := pool.Exec(ctx,
		`UPDATE charges SET status = 'failed' WHERE id = $1`, charge.ID); err == nil {
		t.Error("a succeeded charge was moved to failed")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE charges SET status = 'pending' WHERE id = $1`, charge.ID); err == nil {
		t.Error("a succeeded charge was moved backwards to pending")
	}

	// Non-status updates must still work.
	if _, err := pool.Exec(ctx,
		`UPDATE charges SET risk_level = 'low' WHERE id = $1`, charge.ID); err != nil {
		t.Errorf("an unrelated column update was blocked: %v", err)
	}
}

// One charge per idempotency key, enforced by the database rather than by
// convention — the stale-lock steal path mints a fresh charge UUID, so nothing
// in Go prevented two rows for one logical request.
func TestOneChargeRowPerIdempotencyKey(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)
	seedCharge(t, svc, vlt, merchantID, "tok_unique")

	var existingKey string
	if err := pool.QueryRow(ctx,
		`SELECT idempotency_key FROM charges WHERE merchant_id = $1`, merchantID,
	).Scan(&existingKey); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO charges (id, merchant_id, amount_cents, currency, status, idempotency_key)
		VALUES ($1, $2, 10000, 'USD', 'pending', $3)`,
		uuid.New(), merchantID, existingKey,
	)
	if err == nil {
		t.Error("a second charge row was inserted for one idempotency key")
	}
}
