package risk

import (
	"context"
	"log/slog"
	"time"
)

// Service combines the rule engine, velocity counters, and the ML model into
// the single risk decision the charge path consults (§14.1).
//
// The composition rule, which is the important part:
//
//	The rule engine ALWAYS runs. The model only ever adds to its verdict.
//
// So a model outage degrades detection quality but never blocks payments, and
// never silently disables risk checking altogether. §24.1 names this exact
// tradeoff as one to be able to defend: fail open to rules, not open to
// nothing, and not closed.
type Service struct {
	engine  *Engine
	counter *Counter
	model   *Client
	// budget caps the whole risk step. Exceeding it means proceeding on
	// whatever has been computed so far rather than delaying the charge.
	budget time.Duration
}

func NewService(engine *Engine, counter *Counter, model *Client) *Service {
	if engine == nil {
		engine = NewEngine(nil)
	}
	return &Service{engine: engine, counter: counter, model: model, budget: 100 * time.Millisecond}
}

// Decision is the risk verdict attached to a charge.
type Decision struct {
	Level      Level    `json:"level"`
	Score      float64  `json:"score"`
	RulesFired []string `json:"rules_fired"`
	Reason     string   `json:"reason,omitempty"`

	// ModelScore is the raw probability, nil when the model was unavailable.
	ModelScore   *float64 `json:"model_score,omitempty"`
	ModelVersion string   `json:"model_version,omitempty"`
	// ModelSkipped records that this decision rests on rules alone — needed
	// when explaining a decline months later (§14.5), and to spot silent
	// degradation in metrics.
	ModelSkipped bool `json:"model_skipped"`

	LatencyMs float64 `json:"latency_ms"`
}

// Assess scores one charge attempt.
//
// Never returns an error. Every failure inside — Redis down, model down, model
// slow — degrades the assessment rather than failing it, because there is no
// failure mode of the risk system that should stop a legitimate payment.
func (s *Service) Assess(ctx context.Context, txn Transaction) Decision {
	started := time.Now()

	ctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()

	// Velocity first: the rules need these counters, and a Redis failure
	// returns zeros so the velocity rules simply stay quiet.
	if s.counter != nil {
		txn.Velocity = s.counter.Counters(ctx,
			txn.CardFingerprint, txn.IPAddress, txn.DeviceID, txn.Email)
	}

	assessment := s.engine.Evaluate(txn)
	decision := Decision{
		Level:      assessment.Level,
		Score:      assessment.Score,
		RulesFired: assessment.RulesFired,
		Reason:     assessment.Reason,
	}
	if decision.RulesFired == nil {
		decision.RulesFired = []string{}
	}

	// A rule that blocks outright is not overridden by the model. Rules encode
	// deliberate policy — "repeated declines means card testing" — and a model
	// trained on historical data should not be able to wave that through. The
	// model is not even called: the charge is already decided.
	if assessment.Level == LevelHigh {
		// ModelSkipped means "no model score contributed to this decision",
		// not "the model was broken". Both readings are useful, but this one
		// is what the audit trail needs when explaining a decline (§14.5).
		// Outage monitoring uses the circuit-breaker state instead, which
		// distinguishes a genuine outage from a rule block.
		decision.ModelSkipped = true
		decision.LatencyMs = msSince(started)
		return decision
	}

	modelResp, err := s.model.Score(ctx, ModelRequest{
		AmountCents:     txn.AmountCents,
		Currency:        txn.Currency,
		Timestamp:       txn.Timestamp.UTC().Format(time.RFC3339),
		CardFingerprint: txn.CardFingerprint,
		TxnCount1h:      floatPtr(txn.Velocity.CardChargesLastHour),
		TxnCount24h:     floatPtr(txn.Velocity.CardChargesLastDay),
	})

	if err != nil || modelResp == nil {
		decision.ModelSkipped = true
		if err != nil {
			// Warn, not error: this is a handled degradation, and paging on it
			// per-request would bury the signal that matters (a sustained
			// outage, which the breaker-state metric surfaces).
			slog.Warn("risk: model unavailable, proceeding on rules alone",
				"error", err, "card_fingerprint", txn.CardFingerprint)
		}
		decision.LatencyMs = msSince(started)
		return decision
	}

	decision.ModelScore = &modelResp.Score
	decision.ModelVersion = modelResp.ModelVersion

	// The model can raise the level but never lower it. Rules are the floor:
	// if a rule found something suspicious, a confident model score does not
	// erase it.
	switch modelResp.RiskLevel {
	case "high":
		decision.Level = LevelHigh
		if decision.Reason == "" {
			decision.Reason = "Model scored this charge as high risk"
		}
	case "medium":
		if decision.Level == LevelLow {
			decision.Level = LevelMedium
			if decision.Reason == "" {
				decision.Reason = "Model scored this charge as elevated risk"
			}
		}
	}

	decision.LatencyMs = msSince(started)
	return decision
}

// RecordOutcome updates velocity counters after a charge attempt. Declined
// attempts count — they are the strongest card-testing signal.
func (s *Service) RecordOutcome(ctx context.Context, txn Transaction, declined bool) {
	if s.counter == nil {
		return
	}
	if err := s.counter.RecordCharge(ctx,
		txn.CardFingerprint, txn.IPAddress, txn.DeviceID, txn.Email, declined); err != nil {
		// Losing a counter update weakens future detection but cannot lose
		// money, so it is logged and swallowed rather than failing the charge.
		slog.Warn("risk: failed to record velocity", "error", err)
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}

func floatPtr(n int) *float64 {
	f := float64(n)
	return &f
}
