package payments_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/likhith2366/paylo/internal/idempotency"
	"github.com/likhith2366/paylo/internal/payments"
	"github.com/likhith2366/paylo/internal/risk"
	"github.com/likhith2366/paylo/internal/testsupport"
)

// stubAssessor returns a fixed verdict, so tests drive each branch of the risk
// integration without depending on rule thresholds or a live model.
type stubAssessor struct {
	level           risk.Level
	rules           []string
	assessCalls     atomic.Int64
	recordedCalls   atomic.Int64
	recordedDecline atomic.Bool
}

func (s *stubAssessor) Assess(context.Context, risk.Transaction) risk.Decision {
	s.assessCalls.Add(1)
	rules := s.rules
	if rules == nil {
		rules = []string{}
	}
	return risk.Decision{Level: s.level, Score: 42, RulesFired: rules, Reason: "stub"}
}

func (s *stubAssessor) RecordOutcome(_ context.Context, _ risk.Transaction, declined bool) {
	s.recordedCalls.Add(1)
	s.recordedDecline.Store(declined)
}

func newRiskService(t *testing.T, pool *pgxpool.Pool, bank *fakeBank, v *fakeVault, a payments.RiskAssessor) *payments.Service {
	t.Helper()
	return payments.NewService(pool, payments.NewBankClient(bank.server.URL, 10*time.Second), v, a)
}

// A high-risk verdict must decline before the charge ever reaches the
// processor. Reaching the bank at all would mean a real authorization on a
// charge we had already decided to refuse.
func TestHighRiskDeclinesBeforeReachingTheBank(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	bank := newFakeBank(t, 0)
	vlt := newFakeVault()
	token := vlt.issue("tok_high_risk", "4242424242424242")
	assessor := &stubAssessor{level: risk.LevelHigh, rules: []string{"card_testing_declines"}}
	svc := newRiskService(t, pool, bank, vlt, assessor)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)

	charge, status, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "risk_high", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	if charge.Status != payments.StatusFailed {
		t.Errorf("status = %q, want failed", charge.Status)
	}
	if charge.FailureCode != "risk_declined" {
		t.Errorf("failure_code = %q, want risk_declined", charge.FailureCode)
	}
	if status != 402 {
		t.Errorf("HTTP status = %d, want 402", status)
	}

	// The whole point: the processor was never contacted.
	if got := bank.authCalls.Load(); got != 0 {
		t.Errorf("bank was called %d times on a risk-declined charge, want 0", got)
	}
	// Nor was the card ever decrypted.
	if got := vlt.detokenizeCalls.Load(); got != 0 {
		t.Errorf("vault detokenized %d times on a risk-declined charge, want 0", got)
	}
	// And no money moved.
	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Errorf("a risk-declined charge wrote %d ledger entries, want 0", entries)
	}

	// The block must feed velocity — a fraudster's blocked attempts are what
	// catch the next one.
	if assessor.recordedCalls.Load() == 0 {
		t.Error("a risk decline was not recorded against velocity counters")
	}
	if !assessor.recordedDecline.Load() {
		t.Error("the risk decline was recorded as a success")
	}
}

// The assessment must be stored on the charge so the decision can be explained
// months later during a dispute or an audit (§14.5).
func TestRiskAssessmentIsStoredOnTheCharge(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	vlt := newFakeVault()
	token := vlt.issue("tok_audit", "4242424242424242")
	assessor := &stubAssessor{level: risk.LevelHigh, rules: []string{"card_testing_declines", "geo_mismatch"}}
	svc := newRiskService(t, pool, newFakeBank(t, 0), vlt, assessor)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)
	charge, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "risk_audit", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	var level string
	var rules []byte
	var score *float64
	err = pool.QueryRow(ctx,
		`SELECT risk_level, risk_rules_fired, risk_score FROM charges WHERE id = $1`,
		charge.ID,
	).Scan(&level, &rules, &score)
	if err != nil {
		t.Fatalf("read stored assessment: %v", err)
	}

	if level != "high" {
		t.Errorf("stored risk_level = %q, want high", level)
	}
	if score == nil {
		t.Error("risk_score was not stored")
	}
	// Both rules must be listed, not just the one that decided.
	got := string(rules)
	if !strings.Contains(got, "card_testing_declines") || !strings.Contains(got, "geo_mismatch") {
		t.Errorf("stored rules = %s, want both rules recorded", got)
	}
}

// Low risk proceeds untouched.
func TestLowRiskProceedsNormally(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	bank := newFakeBank(t, 0)
	vlt := newFakeVault()
	token := vlt.issue("tok_low_risk", "4242424242424242")
	assessor := &stubAssessor{level: risk.LevelLow}
	svc := newRiskService(t, pool, bank, vlt, assessor)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)
	charge, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "risk_low", hash, body)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	if charge.Status != payments.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", charge.Status)
	}
	if got := bank.authCalls.Load(); got != 1 {
		t.Errorf("bank called %d times, want 1", got)
	}
}

// A retry must replay the stored decline rather than re-scoring.
//
// Re-scoring would make the same request non-idempotent: velocity counters move
// between attempts, so a second assessment could legitimately reach a different
// verdict and the retry would behave differently from the original.
func TestRiskDeclineIsIdempotent(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	vlt := newFakeVault()
	token := vlt.issue("tok_risk_retry", "4242424242424242")
	assessor := &stubAssessor{level: risk.LevelHigh, rules: []string{"card_testing_declines"}}
	svc := newRiskService(t, pool, newFakeBank(t, 0), vlt, assessor)

	body := []byte(`{"amount":10000,"currency":"USD"}`)
	hash, _ := idempotency.HashRequest(body)

	first, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "risk_retry", hash, body)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}

	replay, _, err := svc.CreateCharge(ctx, chargeInput(merchantID, token), "risk_retry", hash, body)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if replay.ID != first.ID {
		t.Errorf("retry produced a different charge: %s vs %s", replay.ID, first.ID)
	}
	if replay.FailureCode != "risk_declined" {
		t.Errorf("replayed failure_code = %q, want risk_declined", replay.FailureCode)
	}
	if got := assessor.assessCalls.Load(); got != 1 {
		t.Errorf("risk was assessed %d times across 2 requests, want 1 — "+
			"re-scoring a retry makes it non-idempotent", got)
	}
}
