package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/likhith2366/paylo/internal/payouts"
)

// BankClient talks to the acquiring-bank simulator.
//
// The distinction this client draws between an ambiguous timeout and a clean
// failure is the whole point of it. A connection that was refused means the
// request never arrived, so retrying is safe. A request that was sent but never
// answered means the charge may well have succeeded on their side — and a blind
// retry there is how you double-charge a real customer (§24.1).
type BankClient struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

func NewBankClient(baseURL string, timeout time.Duration) *BankClient {
	return &BankClient{
		baseURL: baseURL,
		timeout: timeout,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ErrAmbiguous means the request left this system but no answer came back.
// The charge must be marked requires_reconciliation, never blindly retried.
var ErrAmbiguous = errors.New("payments: bank outcome is ambiguous")

// ErrUnreachable means the request provably never landed — safe to retry.
var ErrUnreachable = errors.New("payments: bank unreachable")

type BankChargeRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency"`
	CardNumber     string `json:"card_number"`
	CardExpMonth   int    `json:"card_exp_month"`
	CardExpYear    int    `json:"card_exp_year"`
	CardCVC        string `json:"card_cvc"`
	CallbackURL    string `json:"callback_url,omitempty"`
}

type BankChargeResponse struct {
	ProcessorReference string `json:"processor_reference"`
	Status             string `json:"status"`
	DeclineCode        string `json:"decline_code,omitempty"`
	DeclineMessage     string `json:"decline_message,omitempty"`
	AuthorizedAt       string `json:"authorized_at,omitempty"`
	NetworkCode        string `json:"network_code,omitempty"`
}

// Charge authorizes a payment.
//
// simulateOutcome, when non-empty, is forwarded as X-Simulate-Outcome so tests
// can drive a specific branch deterministically (§25.2).
func (c *BankClient) Charge(ctx context.Context, req BankChargeRequest, simulateOutcome string) (*BankChargeResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("payments: marshal bank request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/simulator/charge", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("payments: build bank request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if simulateOutcome != "" {
		httpReq.Header.Set("X-Simulate-Outcome", simulateOutcome)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		// The request reached them and they failed while handling it. Whether
		// the charge landed is unknowable from here, so treat it as ambiguous.
		return nil, fmt.Errorf("%w: bank returned %d", ErrAmbiguous, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("payments: bank returned %d", resp.StatusCode)
	}

	var out BankChargeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// A malformed body after a 200 is also ambiguous: they think they
		// answered, we cannot tell what they said.
		return nil, fmt.Errorf("%w: decode bank response: %v", ErrAmbiguous, err)
	}
	return &out, nil
}

type BankRefundRequest struct {
	IdempotencyKey     string `json:"idempotency_key"`
	ProcessorReference string `json:"processor_reference"`
	AmountCents        int64  `json:"amount_cents"`
}

type BankRefundResponse struct {
	RefundReference string `json:"refund_reference"`
	Status          string `json:"status"` // succeeded | failed
	FailureCode     string `json:"failure_code,omitempty"`
	AmountCents     int64  `json:"amount_cents"`
}

// Refund reverses a settled charge at the processor.
//
// Ambiguity matters as much here as it does for charges: a refund whose
// outcome is unknown must not be retried blindly, or the customer gets their
// money back twice and the merchant absorbs it.
func (c *BankClient) Refund(ctx context.Context, req BankRefundRequest, simulateOutcome string) (*BankRefundResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("payments: marshal refund request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/simulator/refund", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("payments: build refund request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if simulateOutcome != "" {
		httpReq.Header.Set("X-Simulate-Outcome", simulateOutcome)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: bank returned %d", ErrAmbiguous, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("payments: refund returned %d", resp.StatusCode)
	}

	var out BankRefundResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: decode refund response: %v", ErrAmbiguous, err)
	}
	return &out, nil
}

// Transfer initiates an ACH payout to a merchant's bank account (§18).
//
// Acceptance is not settlement: the bank can accept a transfer and reject it
// days later on a bad routing number, which is why the payout stays in
// in_transit until confirmed.
func (c *BankClient) Transfer(ctx context.Context, req payouts.TransferRequest) (*payouts.TransferResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("payments: marshal transfer: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/simulator/payouts", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("payments: build transfer request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: bank returned %d", ErrAmbiguous, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("payments: transfer returned %d", resp.StatusCode)
	}

	var out payouts.TransferResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: decode transfer response: %v", ErrAmbiguous, err)
	}
	return &out, nil
}

// Lookup queries the processor's transaction log by reference. This is how
// reconciliation resolves a charge left in requires_reconciliation (§24.3) —
// query-before-retry, never guess.
func (c *BankClient) Lookup(ctx context.Context, reference string) (*BankChargeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/simulator/charge/"+reference, nil)
	if err != nil {
		return nil, fmt.Errorf("payments: build lookup request: %w", err)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // The charge genuinely never landed.
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("payments: lookup returned %d", resp.StatusCode)
	}

	var out BankChargeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("payments: decode lookup response: %w", err)
	}
	return &out, nil
}

// classifyTransportError decides whether a failed call is safely retryable.
//
// A refused connection or unresolved host means the request never left, so it
// can be retried. A timeout means it may have been fully processed on the far
// side, so it cannot.
func classifyTransportError(err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", ErrAmbiguous, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrAmbiguous, err)
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// "connect" failed: the TCP handshake never completed, so no request
		// data was transmitted.
		if opErr.Op == "dial" {
			return fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		// A reset mid-flight (the network_error simulation) is ambiguous: the
		// request may have been read and acted on before the peer went away.
		return fmt.Errorf("%w: %v", ErrAmbiguous, err)
	}
	return fmt.Errorf("%w: %v", ErrAmbiguous, err)
}
