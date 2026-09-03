package payments_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/idempotency"
	"github.com/likhith2366/paylo/internal/payments"
	"github.com/likhith2366/paylo/internal/testsupport"
)

// seedCharge creates a succeeded charge to refund or dispute against.
func seedCharge(t *testing.T, svc *payments.Service, vlt *fakeVault, merchantID uuid.UUID, tokenName string) *payments.Charge {
	t.Helper()

	token := vlt.issue(tokenName, "4242424242424242")
	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)

	charge, _, err := svc.CreateCharge(context.Background(),
		chargeInput(merchantID, token), "charge_"+tokenName, hash, body)
	if err != nil {
		t.Fatalf("seed charge: %v", err)
	}
	if charge.Status != payments.StatusSucceeded {
		t.Fatalf("seed charge status = %q, want succeeded", charge.Status)
	}
	return charge
}

func refundInput(merchantID, chargeID uuid.UUID, amount int64) payments.RefundInput {
	return payments.RefundInput{
		MerchantID: merchantID, ChargeID: chargeID,
		AmountCents: amount, Reason: "requested_by_customer",
	}
}

func TestFullRefundReversesTheMerchantBalance(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_full_refund")

	body := []byte(`{"charge":"x","amount":10000}`)
	hash, _ := idempotency.HashRequest(body)
	refund, status, err := svc.CreateRefund(ctx, refundInput(merchantID, charge.ID, 10_000), "refund_key", hash)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if refund.Status != payments.RefundStatusSucceeded || status != 200 {
		t.Fatalf("refund status = %q / HTTP %d", refund.Status, status)
	}

	// The merchant received 9710 net and gives back the full 10000: the
	// platform keeps its 290 fee, so the merchant is 290 down overall. That is
	// how real processors behave, and the ledger should show it.
	balance := merchantBalance(t, pool, merchantID, "USD")
	if balance != 290 {
		t.Errorf("merchant balance = %d, want 290 (debit — the retained fee)", balance)
	}
	assertLedgerBalanced(t, pool)
}

func TestPartialRefundsAccumulate(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_partial")

	for i, amount := range []int64{3000, 2000} {
		body := []byte(fmt.Sprintf(`{"amount":%d}`, amount))
		hash, _ := idempotency.HashRequest(body)
		if _, _, err := svc.CreateRefund(ctx,
			refundInput(merchantID, charge.ID, amount),
			fmt.Sprintf("partial_%d", i), hash); err != nil {
			t.Fatalf("partial refund %d: %v", i, err)
		}
	}

	refunded, remaining, err := svc.RefundedTotal(ctx, charge.ID)
	if err != nil {
		t.Fatalf("refunded total: %v", err)
	}
	if refunded != 5000 {
		t.Errorf("refunded = %d, want 5000", refunded)
	}
	if remaining != 5000 {
		t.Errorf("remaining = %d, want 5000", remaining)
	}
	assertLedgerBalanced(t, pool)
}

func TestRefundCannotExceedChargeAmount(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_over")

	body := []byte(`{"amount":15000}`)
	hash, _ := idempotency.HashRequest(body)
	_, _, err := svc.CreateRefund(ctx, refundInput(merchantID, charge.ID, 15_000), "over_key", hash)
	if !errors.Is(err, payments.ErrRefundExceedsCharge) {
		t.Fatalf("expected ErrRefundExceedsCharge, got %v", err)
	}

	// And nothing was written.
	refunded, _, _ := svc.RefundedTotal(ctx, charge.ID)
	if refunded != 0 {
		t.Errorf("a rejected refund still moved %d cents", refunded)
	}
}

// The race that makes refunds hard (§17, §22.1).
//
// Ten concurrent partial refunds of 2000 against a 10000 charge: at most five
// may succeed. A naive "SELECT total, then INSERT" would let all ten through,
// because every one of them reads a total taken before any of the others
// committed. This asserts the row lock actually closes that window.
func TestConcurrentPartialRefundsCannotOverdrawTheCharge(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_race")

	const attempts = 10
	const each = 2000 // 5 × 2000 = the full 10000

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf(`{"amount":%d,"n":%d}`, each, i))
			hash, _ := idempotency.HashRequest(body)
			<-start
			_, _, errs[i] = svc.CreateRefund(ctx,
				refundInput(merchantID, charge.ID, each),
				fmt.Sprintf("race_key_%d", i), hash)
		}(i)
	}
	close(start)
	wg.Wait()

	var succeeded int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, payments.ErrRefundExceedsCharge):
			// Expected for the losers.
		default:
			t.Errorf("attempt %d failed unexpectedly: %v", i, err)
		}
	}

	if succeeded != 5 {
		t.Errorf("%d refunds succeeded, want exactly 5 — the charge was overdrawn", succeeded)
	}

	refunded, remaining, err := svc.RefundedTotal(ctx, charge.ID)
	if err != nil {
		t.Fatalf("refunded total: %v", err)
	}
	if refunded != 10_000 {
		t.Errorf("refunded %d cents against a 10000 charge", refunded)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0", remaining)
	}
	assertLedgerBalanced(t, pool)

	t.Logf("%d concurrent refunds of %d against a 10000 charge → %d succeeded, %d cents refunded",
		attempts, each, succeeded, refunded)
}

// A retried refund must not refund twice (§17).
func TestRefundIsIdempotent(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_idem_refund")

	body := []byte(`{"amount":4000}`)
	hash, _ := idempotency.HashRequest(body)

	first, _, err := svc.CreateRefund(ctx, refundInput(merchantID, charge.ID, 4000), "same_refund_key", hash)
	if err != nil {
		t.Fatalf("first refund: %v", err)
	}
	for i := 0; i < 3; i++ {
		replay, _, err := svc.CreateRefund(ctx, refundInput(merchantID, charge.ID, 4000), "same_refund_key", hash)
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if replay.ID != first.ID {
			t.Errorf("retry %d created a new refund %s", i, replay.ID)
		}
	}

	refunded, _, _ := svc.RefundedTotal(ctx, charge.ID)
	if refunded != 4000 {
		t.Errorf("refunded = %d after 4 identical requests, want 4000", refunded)
	}
}

func TestCannotRefundAFailedCharge(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	token := vlt.issue("tok_declined", "4242424242424242")
	in := chargeInput(merchantID, token)
	in.SimulateOutcome = "decline"
	body := []byte(`{"amount":10000,"outcome":"decline"}`)
	hash, _ := idempotency.HashRequest(body)

	charge, _, err := svc.CreateCharge(ctx, in, "declined_charge", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	rbody := []byte(`{"amount":1000}`)
	rhash, _ := idempotency.HashRequest(rbody)
	_, _, err = svc.CreateRefund(ctx, refundInput(merchantID, charge.ID, 1000), "refund_declined", rhash)
	if !errors.Is(err, payments.ErrChargeNotRefundable) {
		t.Errorf("expected ErrChargeNotRefundable, got %v", err)
	}
}

// merchantBalance returns the cached balance in the sign convention used
// throughout: positive means debit-heavy, negative means the merchant is owed.
func merchantBalance(t *testing.T, pool *pgxpool.Pool, merchantID uuid.UUID, currency string) int64 {
	t.Helper()

	var cents int64
	err := pool.QueryRow(context.Background(), `
		SELECT b.balance_cents
		FROM ledger_balances b JOIN accounts a ON a.id = b.account_id
		WHERE a.merchant_id = $1 AND a.account_type = 'merchant_balance'
		  AND a.currency = $2`,
		merchantID, currency,
	).Scan(&cents)
	if err != nil {
		t.Fatalf("read merchant balance: %v", err)
	}
	return cents
}
