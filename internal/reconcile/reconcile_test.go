package reconcile_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/payments"
	"github.com/likhith2366/paylo/internal/reconcile"
	"github.com/likhith2366/paylo/internal/testsupport"
)

// fakeProcessor drives each branch of the resolution logic.
type fakeProcessor struct {
	resp *payments.BankChargeResponse
	err  error
}

func (f *fakeProcessor) Lookup(context.Context, string) (*payments.BankChargeResponse, error) {
	return f.resp, f.err
}

// seedAmbiguous creates a charge parked in requires_reconciliation, aged past
// the job's minimum so it is eligible.
func seedAmbiguous(t *testing.T, pool *pgxpool.Pool, merchantID uuid.UUID) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO charges
			(merchant_id, amount_cents, currency, status, processor_reference,
			 idempotency_key, created_at)
		VALUES ($1, 10000, 'USD', 'requires_reconciliation', 'bank_ref_1',
		        'idem_' || gen_random_uuid()::text, now() - INTERVAL '1 hour')
		RETURNING id`,
		merchantID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed ambiguous charge: %v", err)
	}
	return id
}

func status(t *testing.T, pool *pgxpool.Pool, chargeID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM charges WHERE id = $1`, chargeID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func ledgerCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The other half of the ambiguous-timeout contract. The charge path refuses to
// guess whether the money moved; this is what finds out, and posts the ledger
// entries it could not.
func TestAmbiguousChargeConfirmedAuthorizedPostsTheLedger(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	chargeID := seedAmbiguous(t, pool, merchantID)

	svc := reconcile.NewService(pool, &fakeProcessor{
		resp: &payments.BankChargeResponse{
			ProcessorReference: "bank_ref_1", Status: "authorized",
		},
	})

	result, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ChargesResolved != 1 {
		t.Errorf("resolved %d charges, want 1", result.ChargesResolved)
	}
	if got := status(t, pool, chargeID); got != payments.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", got)
	}

	// The money moved, so the ledger must now say so — with the same three
	// legs the charge path would have written.
	var receivable, balance, fees int64
	err = pool.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(amount_cents) FILTER (WHERE a.account_type='customer_receivable'),0),
		  COALESCE(SUM(amount_cents) FILTER (WHERE a.account_type='merchant_balance'),0),
		  COALESCE(SUM(amount_cents) FILTER (WHERE a.account_type='platform_fees'),0)
		FROM ledger_entries e JOIN accounts a ON a.id = e.account_id`,
	).Scan(&receivable, &balance, &fees)
	if err != nil {
		t.Fatal(err)
	}
	if receivable != 10_000 || balance != 9_710 || fees != 290 {
		t.Errorf("ledger legs = receivable %d, balance %d, fees %d; want 10000/9710/290",
			receivable, balance, fees)
	}
}

// Absent from the processor's log means the charge never landed. Safe to fail,
// and no money may be booked.
func TestChargeAbsentFromProcessorIsMarkedFailed(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	chargeID := seedAmbiguous(t, pool, merchantID)

	svc := reconcile.NewService(pool, &fakeProcessor{resp: nil})

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := status(t, pool, chargeID); got != payments.StatusFailed {
		t.Errorf("status = %q, want failed", got)
	}
	if n := ledgerCount(t, pool); n != 0 {
		t.Errorf("a charge that never landed wrote %d ledger entries, want 0", n)
	}
}

// A processor that is merely unreachable must not be read as "never happened".
// The charge stays parked for the next run.
func TestUnreachableProcessorLeavesTheChargeParked(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	chargeID := seedAmbiguous(t, pool, merchantID)

	svc := reconcile.NewService(pool, &fakeProcessor{err: errors.New("connection refused")})

	result, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ChargesResolved != 0 {
		t.Errorf("resolved %d charges while the processor was down, want 0", result.ChargesResolved)
	}
	if got := status(t, pool, chargeID); got != payments.StatusRequiresReconciliation {
		t.Errorf("status = %q, want it left parked", got)
	}
	if result.Discrepancies != 0 {
		t.Error("an unreachable processor was recorded as a discrepancy")
	}
}

// The job must be safe to re-run from scratch — §24.3 calls that out as a
// deliberate design property. A second pass must not double-post.
func TestRunningTwiceDoesNotDoublePost(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	seedAmbiguous(t, pool, merchantID)

	svc := reconcile.NewService(pool, &fakeProcessor{
		resp: &payments.BankChargeResponse{ProcessorReference: "bank_ref_1", Status: "authorized"},
	})

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	after := ledgerCount(t, pool)

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := ledgerCount(t, pool); got != after {
		t.Errorf("second run wrote %d additional ledger entries, want 0", got-after)
	}
}

// The independent invariant check. It shares no code with the write path or
// the database trigger, which is what lets it catch a case where both were
// wrong in the same way.
func TestBalanceDriftIsDetectedAndRecorded(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	seedAmbiguous(t, pool, merchantID)

	svc := reconcile.NewService(pool, &fakeProcessor{
		resp: &payments.BankChargeResponse{ProcessorReference: "bank_ref_1", Status: "authorized"},
	})
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Corrupt the cached balance, simulating drift the write path failed to
	// prevent. The entries themselves are append-only and untouched.
	if _, err := pool.Exec(ctx,
		`UPDATE ledger_balances SET balance_cents = balance_cents + 500`); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.BalanceDrifts == 0 {
		t.Fatal("drift between the cached and derived balance was not detected")
	}

	var kind string
	var resolvedAt *string
	if err := pool.QueryRow(ctx, `
		SELECT kind, resolved_at::text FROM reconciliation_discrepancies
		WHERE kind = 'balance_drift' LIMIT 1`).Scan(&kind, &resolvedAt); err != nil {
		t.Fatalf("no discrepancy recorded: %v", err)
	}
	// Recorded for a human, never auto-corrected (§24.3).
	if resolvedAt != nil {
		t.Error("the job resolved a discrepancy itself; that must be a human action")
	}

	var stillDrifted int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_balances b
		JOIN (SELECT account_id, SUM(CASE WHEN direction='debit' THEN amount_cents
		                                  ELSE -amount_cents END) d
		      FROM ledger_entries GROUP BY 1) e USING (account_id)
		WHERE b.balance_cents <> e.d`).Scan(&stillDrifted); err != nil {
		t.Fatal(err)
	}
	if stillDrifted == 0 {
		t.Error("the job silently corrected the balance instead of flagging it")
	}
}

// Charges still legitimately in flight must be left alone — otherwise the job
// would "resolve" a charge whose original request is still running.
func TestRecentChargesAreNotTouched(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	var chargeID uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO charges (merchant_id, amount_cents, currency, status, processor_reference)
		VALUES ($1, 10000, 'USD', 'requires_reconciliation', 'bank_fresh')
		RETURNING id`, merchantID).Scan(&chargeID)
	if err != nil {
		t.Fatal(err)
	}

	svc := reconcile.NewService(pool, &fakeProcessor{
		resp: &payments.BankChargeResponse{Status: "authorized"},
	})
	result, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if result.ChargesChecked != 0 {
		t.Errorf("checked %d charges, want 0 — a charge seconds old may still be in flight",
			result.ChargesChecked)
	}
	if got := status(t, pool, chargeID); got != payments.StatusRequiresReconciliation {
		t.Errorf("status = %q, want untouched", got)
	}
}

// A resolved charge must emit an outbox event, or the merchant never learns
// the outcome (§7).
func TestResolutionEmitsAnOutboxEvent(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	chargeID := seedAmbiguous(t, pool, merchantID)

	svc := reconcile.NewService(pool, &fakeProcessor{
		resp: &payments.BankChargeResponse{ProcessorReference: "bank_ref_1", Status: "authorized"},
	})
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	var eventType string
	if err := pool.QueryRow(ctx,
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1`, chargeID,
	).Scan(&eventType); err != nil {
		t.Fatalf("no outbox event for the resolved charge: %v", err)
	}
	if eventType != "payment.succeeded" {
		t.Errorf("event_type = %q, want payment.succeeded", eventType)
	}
}
