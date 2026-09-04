package risk_test

import (
	"testing"
	"time"

	"github.com/likhith2366/paylo/internal/risk"
)

func f64(v float64) *float64 { return &v }

// humanTyping is what a cardholder entering the card in their hand looks like:
// uneven rhythm, few corrections, no paste, a few seconds total.
func humanTyping() risk.Behavior {
	return risk.Behavior{
		Present:               true,
		Pasted:                false,
		KeystrokeIntervalCV:   f64(0.62), // uneven — digits cluster in fours
		KeystrokeIntervalMean: f64(180),
		NumberKeystrokes:      16,
		CorrectionRate:        f64(0.0),
		CVCHesitationMs:       f64(900),
		TotalDurationMs:       f64(6400),
	}
}

func withBehavior(b risk.Behavior) risk.Transaction {
	t := baseline()
	t.Behavior = b
	return t
}

func TestNormalTypingTripsNothing(t *testing.T) {
	got := risk.NewEngine(nil).Evaluate(withBehavior(humanTyping()))

	if got.Level != risk.LevelLow {
		t.Errorf("level = %q, want low (fired: %v)", got.Level, got.RulesFired)
	}
	if len(got.RulesFired) != 0 {
		t.Errorf("ordinary typing tripped %v", got.RulesFired)
	}
}

// The most important negative case in this file.
//
// A charge from a non-browser API caller, or an older widget, carries no
// behavioral data at all. Absence must never read as suspicious — otherwise
// every server-to-server integration would be flagged.
func TestAbsentBehaviorDataIsNotSuspicious(t *testing.T) {
	got := risk.NewEngine(nil).Evaluate(withBehavior(risk.Behavior{Present: false}))

	if got.Level != risk.LevelLow {
		t.Errorf("level = %q, want low — missing data must not be treated as a signal", got.Level)
	}
	for _, name := range got.RulesFired {
		switch name {
		case "automation_typing_rhythm", "card_number_pasted", "high_correction_rate",
			"cvc_hesitation", "impossible_entry_speed", "automated_card_entry":
			t.Errorf("behavioral rule %q fired with no behavioral data", name)
		}
	}
}

// Metronomic typing is automation. The CV is scale-free, so this catches a
// script without catching a fast human.
func TestMachineRhythmIsChallenged(t *testing.T) {
	b := humanTyping()
	b.KeystrokeIntervalCV = f64(0.03) // near-constant intervals

	got := risk.NewEngine(nil).Evaluate(withBehavior(b))

	if !fired(got, "automation_typing_rhythm") {
		t.Errorf("automation_typing_rhythm did not fire: %v", got.RulesFired)
	}
	if got.Level != risk.LevelMedium {
		t.Errorf("level = %q, want medium", got.Level)
	}
}

// The calibration decision worth defending: pasting alone must NOT block.
// Password managers paste, and plenty of people keep a card in a notes app.
func TestPasteAloneDoesNotBlock(t *testing.T) {
	b := humanTyping()
	b.Pasted = true
	b.PastedFields = []string{"number"}
	// A human pasting then filling the rest normally.
	b.NumberKeystrokes = 0
	b.KeystrokeIntervalCV = nil
	b.TotalDurationMs = f64(4200)

	got := risk.NewEngine(nil).Evaluate(withBehavior(b))

	if !fired(got, "card_number_pasted") {
		t.Errorf("card_number_pasted did not fire: %v", got.RulesFired)
	}
	if got.Level == risk.LevelHigh {
		t.Error("pasting a card number alone blocked the payment; password managers do this")
	}
	// Weight 15 is under the medium threshold of 30 — a signal, not a verdict.
	if got.Level != risk.LevelLow {
		t.Errorf("level = %q, want low for paste alone", got.Level)
	}
}

// Paste PLUS machine speed is a script filling a form. No human does both.
func TestPasteWithMachineSpeedBlocks(t *testing.T) {
	b := humanTyping()
	b.Pasted = true
	b.PastedFields = []string{"number", "cvc"}
	b.TotalDurationMs = f64(180) // whole form in under a fifth of a second

	got := risk.NewEngine(nil).Evaluate(withBehavior(b))

	if !fired(got, "automated_card_entry") {
		t.Errorf("automated_card_entry did not fire: %v", got.RulesFired)
	}
	if got.Level != risk.LevelHigh {
		t.Errorf("level = %q, want high", got.Level)
	}
}

// Browser autofill is fast and has no keystrokes. It must not be mistaken for
// a script — this is a very common legitimate path.
func TestBrowserAutofillIsNotFlaggedAsAutomation(t *testing.T) {
	got := risk.NewEngine(nil).Evaluate(withBehavior(risk.Behavior{
		Present:          true,
		Pasted:           false,
		NumberKeystrokes: 0, // autofill types nothing
		TotalDurationMs:  f64(60),
	}))

	if fired(got, "impossible_entry_speed") {
		t.Error("browser autofill was flagged as impossible entry speed")
	}
	if got.Level != risk.LevelLow {
		t.Errorf("level = %q, want low for autofill", got.Level)
	}
}

// Many corrections suggest reading an unfamiliar number off another screen.
func TestHighCorrectionRateScores(t *testing.T) {
	b := humanTyping()
	b.CorrectionRate = f64(0.45)

	got := risk.NewEngine(nil).Evaluate(withBehavior(b))

	if !fired(got, "high_correction_rate") {
		t.Errorf("high_correction_rate did not fire: %v", got.RulesFired)
	}
	// Weight 15 alone stays under the medium threshold.
	if got.Level != risk.LevelLow {
		t.Errorf("level = %q; corrections alone should not challenge", got.Level)
	}
}

func TestCVCHesitationScores(t *testing.T) {
	b := humanTyping()
	b.CVCHesitationMs = f64(15000) // fifteen seconds hunting for it

	got := risk.NewEngine(nil).Evaluate(withBehavior(b))

	if !fired(got, "cvc_hesitation") {
		t.Errorf("cvc_hesitation did not fire: %v", got.RulesFired)
	}
}

// Individually-weak behavioral signals must combine into a real one — the point
// of scoring rather than independent if-statements.
func TestWeakBehavioralSignalsAccumulate(t *testing.T) {
	b := humanTyping()
	b.Pasted = true                // 15
	b.CorrectionRate = f64(0.4)    // 15
	b.CVCHesitationMs = f64(12000) // 10

	got := risk.NewEngine(nil).Evaluate(withBehavior(b))

	if got.Score < 30 {
		t.Errorf("score = %.0f, want >= 30 from combined signals (fired: %v)",
			got.Score, got.RulesFired)
	}
	if got.Level != risk.LevelMedium {
		t.Errorf("level = %q, want medium", got.Level)
	}
}

// Too few keystrokes for the statistic to mean anything. Firing on a 3-digit
// sample would flag people who autofill the number and type only the CVV.
func TestRhythmRulesNeedEnoughKeystrokes(t *testing.T) {
	got := risk.NewEngine(nil).Evaluate(withBehavior(risk.Behavior{
		Present:             true,
		KeystrokeIntervalCV: f64(0.01), // perfectly even...
		NumberKeystrokes:    3,         // ...across three keystrokes
	}))

	if fired(got, "automation_typing_rhythm") {
		t.Error("rhythm rule fired on too small a sample")
	}
}

// Behavioral rules must not slow the charge path.
func TestBehavioralRulesStayWithinBudget(t *testing.T) {
	engine := risk.NewEngine(nil)
	txn := withBehavior(humanTyping())

	const iterations = 10_000
	start := time.Now()
	for i := 0; i < iterations; i++ {
		engine.Evaluate(txn)
	}
	perCall := time.Since(start) / iterations

	if perCall > 100*time.Microsecond {
		t.Errorf("evaluation takes %v per call with behavioral rules", perCall)
	}
	t.Logf("full rule set incl. behavioral: %v per transaction", perCall)
}
