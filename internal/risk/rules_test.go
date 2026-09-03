package risk_test

import (
	"testing"
	"time"

	"github.com/likhith2366/paylo/internal/risk"
)

// baseline is an unremarkable transaction that should trip nothing. Each test
// perturbs one thing, so a failure points at one rule rather than a
// combination.
func baseline() risk.Transaction {
	return risk.Transaction{
		MerchantID:      "m_1",
		AmountCents:     5_000,
		Currency:        "USD",
		CardFingerprint: "card_abc",
		CardBIN:         "424242",
		Email:           "customer@example.com",
		IPCountry:       "US",
		BillingCountry:  "US",
		CardCountry:     "US",
		DeviceID:        "dev_1",
		// Midday, to stay clear of the odd-hour rule.
		Timestamp:              time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC),
		MerchantAvgAmountCents: 4_000,
		Velocity: risk.Velocity{
			CardChargesLastHour: 1,
			CardChargesLastDay:  2,
			IPChargesLastHour:   1,
			DeviceDistinctCards: 1,
		},
	}
}

func TestOrdinaryTransactionIsLowRisk(t *testing.T) {
	got := risk.NewEngine(nil).Evaluate(baseline())

	if got.Level != risk.LevelLow {
		t.Errorf("level = %q, want low (rules fired: %v)", got.Level, got.RulesFired)
	}
	if len(got.RulesFired) != 0 {
		t.Errorf("an ordinary transaction tripped %v", got.RulesFired)
	}
}

// Card testing — a run of declines then another attempt — is the clearest
// fraud pattern in the set, and the only one that blocks on its own.
func TestCardTestingIsBlocked(t *testing.T) {
	txn := baseline()
	txn.Velocity.CardDeclinesLastHour = 8

	got := risk.NewEngine(nil).Evaluate(txn)

	if got.Level != risk.LevelHigh {
		t.Errorf("level = %q, want high", got.Level)
	}
	if !fired(got, "card_testing_declines") {
		t.Errorf("card_testing_declines did not fire: %v", got.RulesFired)
	}
	if got.Reason == "" {
		t.Error("a blocking decision must carry a reason for the audit trail")
	}
}

// One device cycling many cards is a fraud ring signal (§14.4).
func TestDeviceCyclingManyCardsIsBlocked(t *testing.T) {
	txn := baseline()
	txn.Velocity.DeviceDistinctCards = 15

	got := risk.NewEngine(nil).Evaluate(txn)

	if got.Level != risk.LevelHigh {
		t.Errorf("level = %q, want high", got.Level)
	}
	if !fired(got, "device_card_cycling") {
		t.Errorf("device_card_cycling did not fire: %v", got.RulesFired)
	}
}

// Velocity challenges rather than blocks: a burst of charges is suspicious but
// routinely legitimate, so the customer gets a 3DS step-up instead of a wall.
func TestHourlyVelocityChallengesRatherThanBlocks(t *testing.T) {
	txn := baseline()
	txn.Velocity.CardChargesLastHour = 12

	got := risk.NewEngine(nil).Evaluate(txn)

	if got.Level != risk.LevelMedium {
		t.Errorf("level = %q, want medium — a busy customer should be challenged, not declined", got.Level)
	}
}

// A single country mismatch is common and innocent: travel, VPNs, gifts.
// Firing on one would flag every tourist.
func TestSingleGeoMismatchDoesNotFire(t *testing.T) {
	txn := baseline()
	txn.IPCountry = "CA" // travelling

	got := risk.NewEngine(nil).Evaluate(txn)

	if fired(got, "geo_mismatch") {
		t.Error("geo_mismatch fired on a single country difference")
	}
	if got.Level != risk.LevelLow {
		t.Errorf("level = %q, want low", got.Level)
	}
}

func TestTwoGeoMismatchesFire(t *testing.T) {
	txn := baseline()
	txn.IPCountry = "RU"
	txn.CardCountry = "BR"
	txn.BillingCountry = "US"

	got := risk.NewEngine(nil).Evaluate(txn)

	if !fired(got, "geo_mismatch") {
		t.Errorf("geo_mismatch did not fire when nothing agreed: %v", got.RulesFired)
	}
}

// A new merchant has no baseline. Inventing one would flag their first
// legitimate charges, which is the worst possible first impression.
func TestAmountAnomalySkippedWithoutAMerchantBaseline(t *testing.T) {
	txn := baseline()
	txn.MerchantAvgAmountCents = 0
	txn.AmountCents = 400_000

	got := risk.NewEngine(nil).Evaluate(txn)

	if fired(got, "amount_anomaly") {
		t.Error("amount_anomaly fired for a merchant with no history")
	}
}

func TestAmountAnomalyFiresAgainstABaseline(t *testing.T) {
	txn := baseline()
	txn.MerchantAvgAmountCents = 4_000
	txn.AmountCents = 100_000 // 25x the typical charge

	got := risk.NewEngine(nil).Evaluate(txn)

	if !fired(got, "amount_anomaly") {
		t.Errorf("amount_anomaly did not fire: %v", got.RulesFired)
	}
}

func TestDisposableEmailScores(t *testing.T) {
	txn := baseline()
	txn.Email = "burner@mailinator.com"

	got := risk.NewEngine(nil).Evaluate(txn)

	if !fired(got, "disposable_email") {
		t.Errorf("disposable_email did not fire: %v", got.RulesFired)
	}
	// Weight 20 alone is under the medium threshold of 30 — a throwaway email
	// is a signal, not a verdict.
	if got.Level != risk.LevelLow {
		t.Errorf("level = %q; a disposable email alone should not challenge", got.Level)
	}
}

// Weak signals should combine into a real one. This is the whole point of
// scoring rather than a chain of independent if-statements.
func TestWeakSignalsAccumulateIntoMediumRisk(t *testing.T) {
	txn := baseline()
	txn.Email = "burner@guerrillamail.com"                       // 20
	txn.Timestamp = time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC) // 10 — odd hour
	txn.AmountCents = 600_000                                    // 15 — large
	txn.MerchantAvgAmountCents = 500_000                         // but NOT anomalous for this merchant

	got := risk.NewEngine(nil).Evaluate(txn)

	if got.Score < 30 {
		t.Errorf("score = %.0f, want >= 30 from accumulated signals (fired: %v)",
			got.Score, got.RulesFired)
	}
	if got.Level != risk.LevelMedium {
		t.Errorf("level = %q, want medium", got.Level)
	}
}

// Every decision must be explainable months later during a dispute (§14.5).
func TestFiredRulesAreRecordedForAudit(t *testing.T) {
	txn := baseline()
	txn.Email = "x@mailinator.com"
	txn.Velocity.CardChargesLastDay = 50

	got := risk.NewEngine(nil).Evaluate(txn)

	if len(got.RulesFired) < 2 {
		t.Errorf("only %v recorded; every contributing rule must be listed", got.RulesFired)
	}
	// Evaluation must not stop at the first hit, or the audit trail is partial.
	if !fired(got, "disposable_email") || !fired(got, "velocity_card_daily") {
		t.Errorf("evaluation short-circuited: %v", got.RulesFired)
	}
}

func TestThresholdsAreConfigurable(t *testing.T) {
	txn := baseline()
	txn.Velocity.CardChargesLastHour = 7

	// Default max is 5, so this challenges.
	if got := risk.NewEngine(nil).Evaluate(txn); got.Level != risk.LevelMedium {
		t.Errorf("with default config: level = %q, want medium", got.Level)
	}

	// A merchant with genuinely bursty traffic raises the limit.
	relaxed := risk.DefaultConfig()
	relaxed.MaxCardChargesPerHour = 20
	if got := risk.NewEngine(relaxed).Evaluate(txn); got.Level != risk.LevelLow {
		t.Errorf("with a relaxed limit: level = %q, want low", got.Level)
	}
}

// The engine sits in the synchronous charge path with a sub-100ms budget for
// the whole risk step, so the rules themselves must be far cheaper than that.
func TestEvaluationIsFastEnoughForTheChargePath(t *testing.T) {
	engine := risk.NewEngine(nil)
	txn := baseline()

	const iterations = 10_000
	start := time.Now()
	for i := 0; i < iterations; i++ {
		engine.Evaluate(txn)
	}
	perCall := time.Since(start) / iterations

	if perCall > 100*time.Microsecond {
		t.Errorf("evaluation takes %v per call, too slow for the risk budget", perCall)
	}
	t.Logf("rule evaluation: %v per transaction", perCall)
}

func fired(a risk.Assessment, name string) bool {
	for _, n := range a.RulesFired {
		if n == name {
			return true
		}
	}
	return false
}
