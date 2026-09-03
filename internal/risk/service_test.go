package risk_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/likhith2366/paylo/internal/risk"
)

// The property the whole risk design rests on (§14.3, §24.1):
//
//	The model going down must degrade detection, never block payments.
//
// Failing open to rules is correct. Failing closed — refusing every payment
// because a scoring service is unhealthy — turns a model outage into a total
// outage, which is far more expensive than the fraud it would have caught.
func TestModelOutageFallsOpenToRules(t *testing.T) {
	// A server that is simply not there.
	svc := risk.NewService(
		risk.NewEngine(nil),
		nil, // no Redis either
		risk.NewClient("http://127.0.0.1:1", 20*time.Millisecond),
	)

	got := svc.Assess(context.Background(), risk.Transaction{
		AmountCents: 5_000,
		Currency:    "USD",
		Timestamp:   time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC),
	})

	if got.Level != risk.LevelLow {
		t.Errorf("level = %q; an unreachable model must not block a clean charge", got.Level)
	}
	if !got.ModelSkipped {
		t.Error("ModelSkipped must be set so the degradation is visible in the audit trail")
	}
	if got.ModelScore != nil {
		t.Error("a score was reported despite the model being unreachable")
	}
}

// Rules must still catch fraud while the model is down — that is what makes
// failing open acceptable rather than reckless.
func TestRulesStillBlockWhileTheModelIsDown(t *testing.T) {
	svc := risk.NewService(
		risk.NewEngine(nil),
		nil,
		risk.NewClient("http://127.0.0.1:1", 20*time.Millisecond),
	)

	got := svc.Assess(context.Background(), risk.Transaction{
		AmountCents: 5_000,
		Currency:    "USD",
		Timestamp:   time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC),
		Velocity:    risk.Velocity{CardDeclinesLastHour: 20}, // card testing
	})

	if got.Level != risk.LevelHigh {
		t.Errorf("level = %q, want high — rules must work without the model", got.Level)
	}
	if !got.ModelSkipped {
		t.Error("ModelSkipped should be set")
	}
}

// A slow model is treated as a down one. Waiting would spend the charge path's
// entire latency budget on a service that is already failing.
func TestSlowModelIsTreatedAsUnavailable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"score":0.99,"risk_level":"high"}`))
	}))
	defer slow.Close()

	svc := risk.NewService(risk.NewEngine(nil), nil, risk.NewClient(slow.URL, 30*time.Millisecond))

	started := time.Now()
	got := svc.Assess(context.Background(), risk.Transaction{
		AmountCents: 5_000,
		Currency:    "USD",
		Timestamp:   time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC),
	})
	elapsed := time.Since(started)

	if !got.ModelSkipped {
		t.Error("a model exceeding its timeout must be treated as unavailable")
	}
	// The charge must not have waited on it.
	if elapsed > 500*time.Millisecond {
		t.Errorf("assessment took %v; the timeout was not enforced", elapsed)
	}
	if got.Level != risk.LevelLow {
		t.Errorf("level = %q, want low", got.Level)
	}
}

// After repeated failures the breaker opens and stops calling entirely, so a
// sustained outage stops costing every charge its full timeout (§11).
func TestCircuitBreakerStopsCallingAFailingModel(t *testing.T) {
	var calls atomic.Int64
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	client := risk.NewClient(failing.URL, 100*time.Millisecond)
	svc := risk.NewService(risk.NewEngine(nil), nil, client)

	txn := risk.Transaction{
		AmountCents: 5_000,
		Currency:    "USD",
		Timestamp:   time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC),
	}

	for i := 0; i < 20; i++ {
		if got := svc.Assess(context.Background(), txn); got.Level != risk.LevelLow {
			t.Fatalf("attempt %d: level = %q; a failing model must not block", i, got.Level)
		}
	}

	open, failures := client.State()
	if !open {
		t.Errorf("breaker did not open after %d failures", failures)
	}
	// The threshold is 5, so it should stop well short of 20 calls.
	if got := calls.Load(); got > 8 {
		t.Errorf("model was called %d times across 20 charges; the breaker is not shedding load", got)
	}
	t.Logf("20 charges → %d model calls before the breaker opened", calls.Load())
}

// A healthy model can raise the risk level.
func TestModelCanEscalateRiskLevel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"score":0.98,"risk_level":"high","model_version":"test-v1"}`))
	}))
	defer server.Close()

	svc := risk.NewService(risk.NewEngine(nil), nil, risk.NewClient(server.URL, time.Second))

	got := svc.Assess(context.Background(), risk.Transaction{
		AmountCents: 5_000,
		Currency:    "USD",
		Timestamp:   time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC),
	})

	if got.Level != risk.LevelHigh {
		t.Errorf("level = %q, want high — the model should be able to escalate", got.Level)
	}
	if got.ModelScore == nil || *got.ModelScore != 0.98 {
		t.Errorf("model score = %v, want 0.98", got.ModelScore)
	}
	if got.ModelVersion != "test-v1" {
		t.Errorf("model version = %q, want test-v1", got.ModelVersion)
	}
}

// The model must not be able to wave through what a rule blocked. Rules encode
// deliberate policy; a model trained on historical data should not override it.
func TestModelCannotOverrideARuleBlock(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		_, _ = w.Write([]byte(`{"score":0.001,"risk_level":"low"}`))
	}))
	defer server.Close()

	svc := risk.NewService(risk.NewEngine(nil), nil, risk.NewClient(server.URL, time.Second))

	got := svc.Assess(context.Background(), risk.Transaction{
		AmountCents: 5_000,
		Currency:    "USD",
		Timestamp:   time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC),
		Velocity:    risk.Velocity{CardDeclinesLastHour: 20},
	})

	if got.Level != risk.LevelHigh {
		t.Errorf("level = %q; a confident model must not clear a rule block", got.Level)
	}
	// It should not even have asked — a decided charge needs no score.
	if called.Load() {
		t.Error("the model was called for a charge the rules had already blocked")
	}
}
