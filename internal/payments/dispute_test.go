package payments_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/payments"
	"github.com/likhith2366/paylo/internal/testsupport"
)

// Opening a dispute must move the money immediately, before any evidence is
// reviewed — the merchant is out the funds while it is open (§15).
func TestOpeningADisputeReversesFundsAndChargesTheFee(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_dispute")

	// After the charge the merchant is credited its net 9710.
	if got := merchantBalance(t, pool, merchantID, "USD"); got != -9710 {
		t.Fatalf("pre-dispute balance = %d, want -9710", got)
	}

	dispute, err := svc.OpenDispute(ctx, payments.OpenDisputeInput{
		ChargeID:           charge.ID,
		Reason:             "fraudulent",
		ProcessorReference: "dp_test_001",
	})
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}
	if dispute.Status != payments.DisputeStatusNeedsResponse {
		t.Errorf("status = %q, want needs_response", dispute.Status)
	}
	if dispute.AmountCents != 10_000 {
		t.Errorf("amount = %d, want the full charge 10000", dispute.AmountCents)
	}
	if dispute.EvidenceDueBy.Before(time.Now()) {
		t.Error("evidence deadline is already past")
	}

	// -9710 (credited net) + 10000 (funds clawed back) + 1500 (dispute fee)
	// = +1790 owed by the merchant. The balance going positive here means the
	// merchant is in debt, which is expected and allowed (§19).
	if got := merchantBalance(t, pool, merchantID, "USD"); got != 1790 {
		t.Errorf("post-dispute balance = %d, want 1790 (merchant now owes)", got)
	}
	assertLedgerBalanced(t, pool)
}

// A won dispute returns both the funds and the fee.
func TestWinningADisputeReturnsFundsAndFee(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_dispute_won")
	dispute, err := svc.OpenDispute(ctx, payments.OpenDisputeInput{
		ChargeID: charge.ID, Reason: "product_not_received",
		ProcessorReference: "dp_test_won",
	})
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}

	if err := svc.SubmitEvidence(ctx, merchantID, dispute.ID,
		map[string]any{"tracking_number": "1Z999", "receipt_url": "s3://evidence/1"}); err != nil {
		t.Fatalf("submit evidence: %v", err)
	}

	resolved, err := svc.ResolveDispute(ctx, dispute.ID, true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Status != payments.DisputeStatusWon {
		t.Errorf("status = %q, want won", resolved.Status)
	}
	if resolved.ResolutionTxnID == nil {
		t.Error("a won dispute must post a resolution transaction")
	}

	// Back to exactly where the charge left it.
	if got := merchantBalance(t, pool, merchantID, "USD"); got != -9710 {
		t.Errorf("balance after winning = %d, want -9710 (fully restored)", got)
	}
	assertLedgerBalanced(t, pool)
}

// A lost dispute posts NO new entries — the money already moved when the
// dispute opened. Posting again would debit the merchant twice for one
// chargeback.
func TestLosingADisputePostsNoFurtherEntries(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_dispute_lost")
	dispute, err := svc.OpenDispute(ctx, payments.OpenDisputeInput{
		ChargeID: charge.ID, Reason: "fraudulent", ProcessorReference: "dp_test_lost",
	})
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}

	before := countLedgerEntries(t, pool)
	balanceBefore := merchantBalance(t, pool, merchantID, "USD")

	resolved, err := svc.ResolveDispute(ctx, dispute.ID, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Status != payments.DisputeStatusLost {
		t.Errorf("status = %q, want lost", resolved.Status)
	}
	if resolved.ResolutionTxnID != nil {
		t.Error("a lost dispute must not post a resolution transaction")
	}

	if after := countLedgerEntries(t, pool); after != before {
		t.Errorf("losing a dispute wrote %d new ledger entries, want 0", after-before)
	}
	if got := merchantBalance(t, pool, merchantID, "USD"); got != balanceBefore {
		t.Errorf("balance changed on loss: %d → %d", balanceBefore, got)
	}
}

// A redelivered chargeback notification must not reverse the funds twice. The
// processor reference is the idempotency mechanism here, since there is no
// client-supplied key on a processor-driven event.
func TestDuplicateDisputeNotificationIsRejected(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_dispute_dupe")
	in := payments.OpenDisputeInput{
		ChargeID: charge.ID, Reason: "fraudulent", ProcessorReference: "dp_duplicate",
	}

	if _, err := svc.OpenDispute(ctx, in); err != nil {
		t.Fatalf("first dispute: %v", err)
	}
	balanceAfterFirst := merchantBalance(t, pool, merchantID, "USD")

	if _, err := svc.OpenDispute(ctx, in); !errors.Is(err, payments.ErrDisputeDuplicate) {
		t.Fatalf("expected ErrDisputeDuplicate, got %v", err)
	}

	if got := merchantBalance(t, pool, merchantID, "USD"); got != balanceAfterFirst {
		t.Errorf("a duplicate notification moved money: %d → %d", balanceAfterFirst, got)
	}
	assertLedgerBalanced(t, pool)
}

// Concurrent redeliveries of the same chargeback — the realistic version of the
// duplicate case, since webhook retries arrive in parallel.
func TestConcurrentDuplicateDisputesReverseOnce(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_dispute_race")

	const attempts = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.OpenDispute(ctx, payments.OpenDisputeInput{
				ChargeID: charge.ID, Reason: "fraudulent",
				ProcessorReference: "dp_concurrent",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var succeeded int
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("%d of %d concurrent notifications opened a dispute, want 1", succeeded, attempts)
	}

	// One reversal only: -9710 + 10000 + 1500 = 1790.
	if got := merchantBalance(t, pool, merchantID, "USD"); got != 1790 {
		t.Errorf("balance = %d, want 1790 — the funds were reversed more than once", got)
	}
	assertLedgerBalanced(t, pool)
}

func TestCannotResolveATwiceResolvedDispute(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_dispute_twice")
	dispute, err := svc.OpenDispute(ctx, payments.OpenDisputeInput{
		ChargeID: charge.ID, Reason: "fraudulent", ProcessorReference: "dp_twice",
	})
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}

	if _, err := svc.ResolveDispute(ctx, dispute.ID, true); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// A second "won" would credit the merchant the funds a second time.
	if _, err := svc.ResolveDispute(ctx, dispute.ID, true); !errors.Is(err, payments.ErrDisputeNotOpen) {
		t.Errorf("expected ErrDisputeNotOpen, got %v", err)
	}
	assertLedgerBalanced(t, pool)
}

// The dispute lifecycle must emit outbox events at both ends — the merchant
// needs to know it opened, and the fraud model needs the outcome as a label.
func TestDisputeLifecycleEmitsOutboxEvents(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")
	vlt := newFakeVault()
	svc := newService(t, pool, newFakeBank(t, 0), vlt)

	charge := seedCharge(t, svc, vlt, merchantID, "tok_dispute_events")
	dispute, err := svc.OpenDispute(ctx, payments.OpenDisputeInput{
		ChargeID: charge.ID, Reason: "fraudulent", ProcessorReference: "dp_events",
	})
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}
	if _, err := svc.ResolveDispute(ctx, dispute.ID, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY id`, dispute.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			t.Fatal(err)
		}
		got = append(got, eventType)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := []string{"dispute.created", "dispute.lost"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func countLedgerEntries(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_entries`).Scan(&n); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	return n
}
