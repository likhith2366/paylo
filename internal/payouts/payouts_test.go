package payouts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/payouts"
	"github.com/likhith2366/paylo/internal/testsupport"
)

type fakeBank struct {
	status      string
	failureCode string
	err         error
	calls       int
}

func (f *fakeBank) Transfer(context.Context, payouts.TransferRequest) (*payouts.TransferResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	status := f.status
	if status == "" {
		status = "accepted"
	}
	return &payouts.TransferResponse{
		Reference: "ach_" + uuid.NewString(), Status: status, FailureCode: f.failureCode,
	}, nil
}

// setup gives a merchant a verified payout account and a settled balance.
// ageHours backdates the ledger entries so they clear the T+2 hold.
func setup(t *testing.T, pool *pgxpool.Pool, balanceCents int64, ageHours int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	if _, err := pool.Exec(ctx, `
		INSERT INTO payout_accounts
			(merchant_id, account_last4, routing_last4, account_token, currency, verified_at)
		VALUES ($1,'6789','4321','ba_tok_1','USD', now())`, merchantID); err != nil {
		t.Fatalf("seed payout account: %v", err)
	}

	if balanceCents == 0 {
		return merchantID
	}

	// A settled charge: receivable debited, merchant credited.
	var receivable, balance uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (merchant_id, account_type, currency)
		VALUES ($1,'customer_receivable','USD') RETURNING id`, merchantID).Scan(&receivable); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (merchant_id, account_type, currency)
		VALUES ($1,'merchant_balance','USD') RETURNING id`, merchantID).Scan(&balance); err != nil {
		t.Fatal(err)
	}

	// Both legs in ONE statement. The balance constraint is deferred to COMMIT,
	// so inserting them separately means each commits alone and is correctly
	// rejected as unbalanced — the trigger doing exactly its job.
	txnID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries
			(transaction_id, account_id, direction, amount_cents, currency, entry_type, created_at)
		VALUES ($1,$2,'debit',$4,'USD','charge', now() - make_interval(hours => $5)),
		       ($1,$3,'credit',$4,'USD','charge', now() - make_interval(hours => $5))`,
		txnID, receivable, balance, balanceCents, ageHours,
	); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_balances (account_id, balance_cents, currency)
		VALUES ($1,$2,'USD'), ($3,$4,'USD')`,
		receivable, balanceCents, balance, -balanceCents); err != nil {
		t.Fatal(err)
	}
	return merchantID
}

func payoutRow(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, int64, string) {
	t.Helper()
	var id uuid.UUID
	var amount int64
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT id, amount_cents, status FROM payouts LIMIT 1`).Scan(&id, &amount, &status); err != nil {
		t.Fatalf("no payout row: %v", err)
	}
	return id, amount, status
}

func accountBalance(t *testing.T, pool *pgxpool.Pool, merchantID uuid.UUID, accountType string) int64 {
	t.Helper()
	var cents int64
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(CASE WHEN e.direction='debit' THEN e.amount_cents
		                         ELSE -e.amount_cents END), 0)
		FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
		WHERE a.merchant_id = $1 AND a.account_type = $2`,
		merchantID, accountType).Scan(&cents)
	if err != nil {
		t.Fatal(err)
	}
	return cents
}

func TestSettledBalanceIsPaidOut(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := setup(t, pool, 50_000, 72) // 3 days old, clears the hold

	bank := &fakeBank{}
	result, err := payouts.NewService(pool, bank).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.PayoutsCreated != 1 {
		t.Fatalf("created %d payouts, want 1", result.PayoutsCreated)
	}

	_, amount, status := payoutRow(t, pool)
	if amount != 50_000 {
		t.Errorf("payout amount = %d, want 50000", amount)
	}
	if status != payouts.StatusPending {
		t.Errorf("status = %q, want pending", status)
	}

	// The funds left the merchant's balance and sit in in_transit — not
	// paid_out, because the bank has not confirmed yet.
	if got := accountBalance(t, pool, merchantID, "merchant_balance"); got != 0 {
		t.Errorf("merchant balance = %d, want 0 after payout", got)
	}
	if got := accountBalance(t, pool, merchantID, "in_transit"); got != -50_000 {
		t.Errorf("in_transit = %d, want -50000 (credit)", got)
	}
	if got := accountBalance(t, pool, merchantID, "paid_out"); got != 0 {
		t.Errorf("paid_out = %d, want 0 until the bank confirms", got)
	}
}

// Funds inside the hold period are not the merchant's to take yet (§18).
func TestUnsettledFundsAreNotPaidOut(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	setup(t, pool, 50_000, 1) // an hour old, still inside T+2

	result, err := payouts.NewService(pool, &fakeBank{}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.PayoutsCreated != 0 {
		t.Errorf("paid out %d times on funds inside the hold period, want 0", result.PayoutsCreated)
	}
}

// The reason payouts are not just "pay the balance": money reserved against an
// open dispute is about to be clawed back (§19).
func TestOpenDisputesAreReservedFromThePayout(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := setup(t, pool, 50_000, 72)

	var chargeID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO charges (merchant_id, amount_cents, currency, status)
		VALUES ($1, 20000, 'USD', 'succeeded') RETURNING id`, merchantID).Scan(&chargeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO disputes
			(charge_id, merchant_id, amount_cents, currency, reason, status,
			 evidence_due_by, processor_reference)
		VALUES ($1,$2,20000,'USD','fraudulent','needs_response', now() + INTERVAL '14 days', 'dp_1')`,
		chargeID, merchantID); err != nil {
		t.Fatal(err)
	}

	if _, err := payouts.NewService(pool, &fakeBank{}).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	_, amount, _ := payoutRow(t, pool)
	if amount != 30_000 {
		t.Errorf("payout = %d, want 30000 (50000 less the 20000 disputed)", amount)
	}
}

// The batch has no client idempotency key, so the period constraint is what
// stops a crashed re-run paying twice.
func TestRerunningTheBatchDoesNotPayTwice(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	setup(t, pool, 50_000, 72)

	svc := payouts.NewService(pool, &fakeBank{})
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	result, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if result.PayoutsCreated != 0 {
		t.Errorf("second run created %d payouts, want 0", result.PayoutsCreated)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payouts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d payout rows exist, want 1", count)
	}
}

// The bank confirms days later: in_transit -> paid_out.
func TestConfirmSettlesTheFunds(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := setup(t, pool, 50_000, 72)

	svc := payouts.NewService(pool, &fakeBank{})
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	payoutID, _, _ := payoutRow(t, pool)

	if err := svc.Confirm(ctx, payoutID); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	if got := accountBalance(t, pool, merchantID, "in_transit"); got != 0 {
		t.Errorf("in_transit = %d, want 0 after settlement", got)
	}
	if got := accountBalance(t, pool, merchantID, "paid_out"); got != -50_000 {
		t.Errorf("paid_out = %d, want -50000", got)
	}
}

// An ACH can fail days after acceptance on a bad routing number (§18). The
// funds must come back, not vanish.
func TestFailedTransferReturnsTheFunds(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := setup(t, pool, 50_000, 72)

	svc := payouts.NewService(pool, &fakeBank{})
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	payoutID, _, _ := payoutRow(t, pool)

	if err := svc.Fail(ctx, payoutID, "invalid_routing_number"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if got := accountBalance(t, pool, merchantID, "in_transit"); got != 0 {
		t.Errorf("in_transit = %d, want 0 after the reversal", got)
	}
	// Credit-normal, so -50000 means the merchant is owed it again.
	if got := accountBalance(t, pool, merchantID, "merchant_balance"); got != -50_000 {
		t.Errorf("merchant balance = %d, want -50000 restored", got)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM payouts WHERE id = $1`,
		payoutID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != payouts.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
}

// A rejected transfer means the money never left, so it is safe to return
// immediately rather than parking it.
func TestRejectedTransferReturnsFundsImmediately(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := setup(t, pool, 50_000, 72)

	bank := &fakeBank{status: "rejected", failureCode: "account_closed"}
	if _, err := payouts.NewService(pool, bank).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	_, _, status := payoutRow(t, pool)
	if status != payouts.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if got := accountBalance(t, pool, merchantID, "merchant_balance"); got != -50_000 {
		t.Errorf("merchant balance = %d, want the funds returned", got)
	}
}

// An ambiguous transfer must NOT return the funds — the money may be moving,
// and returning it now could pay the merchant twice. Same rule as charges.
func TestAmbiguousTransferParksForReconciliation(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := setup(t, pool, 50_000, 72)

	bank := &fakeBank{err: errors.New("timeout awaiting response")}
	if _, err := payouts.NewService(pool, bank).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	_, _, status := payoutRow(t, pool)
	if status != payouts.StatusRequiresReconciliation {
		t.Errorf("status = %q, want requires_reconciliation", status)
	}
	// Funds stay in in_transit — neither returned nor confirmed.
	if got := accountBalance(t, pool, merchantID, "in_transit"); got != -50_000 {
		t.Errorf("in_transit = %d, want the funds held pending resolution", got)
	}
	if got := accountBalance(t, pool, merchantID, "merchant_balance"); got != 0 {
		t.Errorf("merchant balance = %d; returning funds on an ambiguous transfer risks paying twice", got)
	}
}

// A merchant who owes more than they hold gets nothing, not a negative payout.
func TestNegativeBalanceProducesNoPayout(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := setup(t, pool, 10_000, 72)

	var chargeID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO charges (merchant_id, amount_cents, currency, status)
		VALUES ($1, 50000, 'USD', 'succeeded') RETURNING id`, merchantID).Scan(&chargeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO disputes
			(charge_id, merchant_id, amount_cents, currency, reason, status,
			 evidence_due_by, processor_reference)
		VALUES ($1,$2,50000,'USD','fraudulent','needs_response', now() + INTERVAL '14 days', 'dp_big')`,
		chargeID, merchantID); err != nil {
		t.Fatal(err)
	}

	result, err := payouts.NewService(pool, &fakeBank{}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.PayoutsCreated != 0 {
		t.Errorf("created %d payouts for a merchant in debt, want 0", result.PayoutsCreated)
	}
}

// Unverified accounts must not receive money.
func TestUnverifiedAccountIsSkipped(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := setup(t, pool, 50_000, 72)

	if _, err := pool.Exec(ctx,
		`UPDATE payout_accounts SET verified_at = NULL WHERE merchant_id = $1`,
		merchantID); err != nil {
		t.Fatal(err)
	}

	bank := &fakeBank{}
	result, err := payouts.NewService(pool, bank).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.PayoutsCreated != 0 || bank.calls != 0 {
		t.Errorf("paid out to an unverified account: %d payouts, %d transfers",
			result.PayoutsCreated, bank.calls)
	}
}

func TestPayoutEmitsOutboxEvents(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	setup(t, pool, 50_000, 72)

	svc := payouts.NewService(pool, &fakeBank{})
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	payoutID, _, _ := payoutRow(t, pool)
	if err := svc.Confirm(ctx, payoutID); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	var eventType string
	if err := pool.QueryRow(ctx,
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1`, payoutID,
	).Scan(&eventType); err != nil {
		t.Fatalf("no outbox event: %v", err)
	}
	if eventType != "payout.paid" {
		t.Errorf("event_type = %q, want payout.paid", eventType)
	}
}
