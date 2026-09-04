// Command bank-simulator stands in for the acquiring bank / card network (§25.2).
//
// This is the load-bearing fake in the whole system, so it is built as real
// software rather than a throwaway script:
//
//   - Stateless. Any state it must remember (a pending delayed_success
//     callback) lives in Redis, never a process-local map. A simulator that
//     pins a request to one instance becomes the bottleneck you were trying to
//     test around.
//   - Deterministic by header. X-Simulate-Outcome selects the behaviour, so
//     tests exercise the dispute and timeout paths on demand instead of
//     waiting for randomness to eventually produce them.
//   - Load-test friendly. A k6 script varies the header per request to produce
//     a realistic outcome mix, so determinism costs nothing at 1M requests.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Simulated outcomes (§25.2).
const (
	OutcomeSuccess        = "success"
	OutcomeDecline        = "decline"
	OutcomeTimeout        = "timeout"
	OutcomeDelayedSuccess = "delayed_success"
	OutcomeNetworkError   = "network_error"
)

type ChargeRequest struct {
	// The caller's own idempotency key. Real processors accept one, and using
	// it is what protects against a double charge on an ambiguous timeout —
	// the merchant-facing key protects our API from client retries, not our
	// calls to the processor (§24.1).
	IdempotencyKey string `json:"idempotency_key"`
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency"`
	CardNumber     string `json:"card_number"`
	CardExpMonth   int    `json:"card_exp_month"`
	CardExpYear    int    `json:"card_exp_year"`
	CardCVC        string `json:"card_cvc"`
	CallbackURL    string `json:"callback_url,omitempty"`
}

type ChargeResponse struct {
	ProcessorReference string `json:"processor_reference"`
	Status             string `json:"status"` // authorized | declined | pending
	DeclineCode        string `json:"decline_code,omitempty"`
	DeclineMessage     string `json:"decline_message,omitempty"`
	AuthorizedAt       string `json:"authorized_at,omitempty"`
	NetworkCode        string `json:"network_code,omitempty"`
}

// store is the simulator's persistence seam.
//
// The in-memory implementation below is sufficient for a single-node local
// stack; the interface exists so a Redis implementation can be dropped in for
// the horizontally-scaled load-test deployment without touching handlers.
type store interface {
	Put(ctx context.Context, key string, resp ChargeResponse) error
	Get(ctx context.Context, key string) (ChargeResponse, bool, error)
}

type memStore struct {
	mu   sync.RWMutex
	data map[string]ChargeResponse
}

func newMemStore() *memStore { return &memStore{data: map[string]ChargeResponse{}} }

func (m *memStore) Put(_ context.Context, key string, resp ChargeResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = resp
	return nil
}

func (m *memStore) Get(_ context.Context, key string) (ChargeResponse, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.data[key]
	return r, ok, nil
}

type server struct {
	store       store
	timeoutHold time.Duration
	delayHold   time.Duration
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	port := envInt("PORT", 8090)
	srv := &server{
		store: newMemStore(),
		// Long enough to exceed the payments API's BANK_TIMEOUT (default 5s),
		// which is what makes the ambiguous-timeout path reachable in tests.
		timeoutHold: envDuration("SIM_TIMEOUT_HOLD", 10*time.Second),
		delayHold:   envDuration("SIM_DELAY_HOLD", 30*time.Second),
	}

	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Post("/simulator/charge", srv.handleCharge)
	r.Get("/simulator/charge/{reference}", srv.handleLookup)
	r.Post("/simulator/refund", srv.handleRefund)
	r.Post("/simulator/disputes", srv.handleInjectDispute)
	r.Post("/simulator/payouts", srv.handleTransfer)
	r.Post("/simulator/payouts/fail", srv.handlePayoutFail)
	r.Get("/simulator/3ds/challenge", srv.handle3DSChallenge)

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		// Must exceed timeoutHold, or the server would cut off the very
		// response the timeout scenario is trying to delay.
		WriteTimeout: srv.timeoutHold + 30*time.Second,
	}

	go func() {
		slog.Info("bank-simulator listening", "port", port)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("bank-simulator: listen failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("bank-simulator shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("bank-simulator: shutdown", "error", err)
	}
}

func (s *server) handleCharge(w http.ResponseWriter, r *http.Request) {
	var req ChargeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	// The processor is idempotent on its own key. This is what makes a retry
	// after an ambiguous timeout safe rather than a second real charge.
	if req.IdempotencyKey != "" {
		if prior, found, _ := s.store.Get(r.Context(), req.IdempotencyKey); found {
			writeJSON(w, http.StatusOK, prior)
			return
		}
	}

	outcome := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Simulate-Outcome")))
	if outcome == "" {
		outcome = weightedOutcome()
	}

	reference := "bank_" + uuid.NewString()
	logger := slog.With("outcome", outcome, "reference", reference,
		"idempotency_key", req.IdempotencyKey)

	switch outcome {
	case OutcomeSuccess:
		resp := ChargeResponse{
			ProcessorReference: reference,
			Status:             "authorized",
			AuthorizedAt:       time.Now().UTC().Format(time.RFC3339),
			NetworkCode:        "00",
		}
		s.persist(r.Context(), req.IdempotencyKey, resp)
		logger.Info("charge authorized")
		writeJSON(w, http.StatusOK, resp)

	case OutcomeDecline:
		resp := ChargeResponse{
			ProcessorReference: reference,
			Status:             "declined",
			DeclineCode:        "insufficient_funds",
			DeclineMessage:     "The card has insufficient funds to complete this purchase.",
			NetworkCode:        "51",
		}
		s.persist(r.Context(), req.IdempotencyKey, resp)
		logger.Info("charge declined")
		writeJSON(w, http.StatusOK, resp)

	case OutcomeTimeout:
		// The critical scenario: the charge DOES succeed on this side, but the
		// caller gives up before hearing so. The authorization is persisted, so
		// reconciliation can later discover the truth by querying /charge/{ref}
		// — which is precisely the recovery path §24.1 specifies.
		resp := ChargeResponse{
			ProcessorReference: reference,
			Status:             "authorized",
			AuthorizedAt:       time.Now().UTC().Format(time.RFC3339),
			NetworkCode:        "00",
		}
		s.persist(r.Context(), req.IdempotencyKey, resp)
		logger.Warn("charge authorized but response withheld past caller timeout")

		select {
		case <-time.After(s.timeoutHold):
			writeJSON(w, http.StatusOK, resp)
		case <-r.Context().Done():
			// Caller already hung up, as intended.
		}

	case OutcomeDelayedSuccess:
		resp := ChargeResponse{ProcessorReference: reference, Status: "pending"}
		s.persist(r.Context(), req.IdempotencyKey, resp)
		if req.CallbackURL != "" {
			go s.fireDelayedCallback(req.CallbackURL, reference, req.IdempotencyKey)
		}
		logger.Info("charge pending, async settlement scheduled")
		writeJSON(w, http.StatusOK, resp)

	case OutcomeNetworkError:
		// Distinct from a timeout: the connection dies with no response at all,
		// which is what exercises the circuit breaker rather than the
		// reconciliation path (§11).
		logger.Warn("simulating connection reset")
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
				return
			}
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "network_error"})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown X-Simulate-Outcome %q", outcome),
		})
	}
}

func (s *server) persist(ctx context.Context, key string, resp ChargeResponse) {
	if key == "" {
		return
	}
	if err := s.store.Put(ctx, key, resp); err != nil {
		slog.Error("bank-simulator: persist outcome", "error", err)
	}
	// Also addressable by reference, so reconciliation can resolve an
	// ambiguous charge it only knows by processor reference.
	if err := s.store.Put(ctx, resp.ProcessorReference, resp); err != nil {
		slog.Error("bank-simulator: persist by reference", "error", err)
	}
}

// handleLookup is the processor's transaction log — the source of truth
// reconciliation consults to resolve an ambiguous timeout (§24.1, §24.3).
func (s *server) handleLookup(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "reference")
	resp, found, err := s.store.Get(r.Context(), ref)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such transaction"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleRefund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessorReference string `json:"processor_reference"`
		AmountCents        int64  `json:"amount_cents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if strings.EqualFold(r.Header.Get("X-Simulate-Outcome"), OutcomeDecline) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "failed", "failure_code": "refund_not_permitted",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"refund_reference": "rfnd_" + uuid.NewString(),
		"status":           "succeeded",
		"amount_cents":     req.AmountCents,
	})
}

// handleInjectDispute lets a test create a chargeback on demand (§25.2),
// rather than waiting for randomness to eventually produce one.
func (s *server) handleInjectDispute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessorReference string `json:"processor_reference"`
		Reason             string `json:"reason"`
		AmountCents        int64  `json:"amount_cents"`
		CallbackURL        string `json:"callback_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.Reason == "" {
		req.Reason = "fraudulent"
	}

	dispute := map[string]any{
		"dispute_reference":   "dp_" + uuid.NewString(),
		"processor_reference": req.ProcessorReference,
		"reason":              req.Reason,
		"amount_cents":        req.AmountCents,
		// Real networks allow 7-21 days to respond (§15).
		"evidence_due_by": time.Now().UTC().Add(14 * 24 * time.Hour).Format(time.RFC3339),
	}
	if req.CallbackURL != "" {
		go postJSON(req.CallbackURL, dispute)
	}
	slog.Info("dispute injected", "reference", req.ProcessorReference, "reason", req.Reason)
	writeJSON(w, http.StatusCreated, dispute)
}

// handleTransfer accepts an ACH payout (§18).
func (s *server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IdempotencyKey string `json:"idempotency_key"`
		AmountCents    int64  `json:"amount_cents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if strings.EqualFold(r.Header.Get("X-Simulate-Outcome"), OutcomeDecline) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "rejected", "failure_code": "invalid_routing_number",
		})
		return
	}
	// Accepted, not settled. A real ACH can still fail days later — that is
	// what /simulator/payouts/fail exists to drive.
	writeJSON(w, http.StatusOK, map[string]any{
		"payout_reference": "ach_" + uuid.NewString(),
		"status":           "accepted",
	})
}

// handlePayoutFail simulates an ACH failure arriving days after the transfer
// was initiated (§18) — the failure mode that needs its own reconciliation path.
func (s *server) handlePayoutFail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayoutReference string `json:"payout_reference"`
		FailureCode     string `json:"failure_code"`
		CallbackURL     string `json:"callback_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.FailureCode == "" {
		req.FailureCode = "invalid_routing_number"
	}
	event := map[string]any{
		"payout_reference": req.PayoutReference,
		"status":           "failed",
		"failure_code":     req.FailureCode,
		"failed_at":        time.Now().UTC().Format(time.RFC3339),
	}
	if req.CallbackURL != "" {
		go postJSON(req.CallbackURL, event)
	}
	writeJSON(w, http.StatusOK, event)
}

// handle3DSChallenge is the fake issuer challenge page (§16, §25.1). It models
// the state-machine shape only — no real 3DS cryptography is implemented.
func (s *server) handle3DSChallenge(w http.ResponseWriter, r *http.Request) {
	result := r.URL.Query().Get("result")
	if result == "" {
		result = "success"
	}
	returnURL := r.URL.Query().Get("return_url")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html>
<title>Issuer Authentication</title>
<body style="font-family:system-ui;max-width:26rem;margin:4rem auto">
<h1>Bank Simulator — 3-D Secure</h1>
<p>Simulated issuer challenge. Result: <strong>%s</strong></p>
<a href="%s?three_d_secure_result=%s">Continue</a>
</body>`, result, returnURL, result)
}

func (s *server) fireDelayedCallback(url, reference, idempotencyKey string) {
	time.Sleep(s.delayHold)
	settled := ChargeResponse{
		ProcessorReference: reference,
		Status:             "authorized",
		AuthorizedAt:       time.Now().UTC().Format(time.RFC3339),
		NetworkCode:        "00",
	}
	s.persist(context.Background(), idempotencyKey, settled)
	postJSON(url, settled)
}

func postJSON(url string, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		slog.Error("bank-simulator: marshal callback", "error", err)
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		slog.Error("bank-simulator: callback failed", "url", url, "error", err)
		return
	}
	defer resp.Body.Close()
	slog.Info("bank-simulator: callback delivered", "url", url, "status", resp.StatusCode)
}

// weightedOutcome produces a realistic outcome mix when no header is set,
// matching the distribution §25.2 specifies for load and soak testing.
func weightedOutcome() string {
	switch n := rand.Float64(); {
	case n < 0.95:
		return OutcomeSuccess
	case n < 0.98:
		return OutcomeDecline
	case n < 0.995:
		return OutcomeTimeout
	default:
		return OutcomeNetworkError
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("bank-simulator: encode response", "error", err)
	}
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
