package risk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Client for the ML scoring service (§14.3).
//
// The whole design here is about not letting a scoring service take down
// payments. Two independent protections:
//
//   - A hard timeout well inside the risk budget. Slow is treated as down.
//   - A circuit breaker, so a sustained outage stops costing every request its
//     full timeout. Without it, a dead scorer adds the timeout to every charge
//     in the system — the classic cascading failure where one degraded
//     dependency drags down everything upstream of it (§11).
type Client struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
	breaker *breaker
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout == 0 {
		// Deliberately tight. The whole risk step has ~100ms; scoring gets a
		// slice of it, and the rule engine covers what this misses.
		timeout = 50 * time.Millisecond
	}
	return &Client{
		baseURL: baseURL,
		timeout: timeout,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		breaker: newBreaker(5, 30*time.Second),
	}
}

// ModelRequest is the scoring service's input.
type ModelRequest struct {
	AmountCents          int64  `json:"amount_cents"`
	Currency             string `json:"currency"`
	Timestamp            string `json:"timestamp,omitempty"`
	CardBrand            string `json:"card_brand,omitempty"`
	CardType             string `json:"card_type,omitempty"`
	EmailDomain          string `json:"email_domain,omitempty"`
	RecipientEmailDomain string `json:"recipient_email_domain,omitempty"`
	Product              string `json:"product,omitempty"`
	BillingRegion        string `json:"billing_region,omitempty"`
	BillingCountry       string `json:"billing_country,omitempty"`
	DeviceType           string `json:"device_type,omitempty"`
	DeviceInfo           string `json:"device_info,omitempty"`
	CardFingerprint      string `json:"card_fingerprint,omitempty"`

	TxnCount1h          *float64 `json:"txn_count_1h,omitempty"`
	TxnCount24h         *float64 `json:"txn_count_24h,omitempty"`
	TxnCount7d          *float64 `json:"txn_count_7d,omitempty"`
	AmtSum24h           *float64 `json:"amt_sum_24h,omitempty"`
	SecondsSinceLastTxn *float64 `json:"seconds_since_last_txn,omitempty"`
	CardAvgAmount       *float64 `json:"card_avg_amount,omitempty"`
	CardStdAmount       *float64 `json:"card_std_amount,omitempty"`
}

type ModelResponse struct {
	Score        float64 `json:"score"`
	RiskLevel    string  `json:"risk_level"`
	ModelVersion string  `json:"model_version"`
	LatencyMs    float64 `json:"latency_ms"`
	Degraded     bool    `json:"degraded"`
}

// Score requests a model score. A nil response with a nil error means the
// model was unavailable and the caller should proceed on rules alone.
func (c *Client) Score(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	if c == nil || c.baseURL == "" {
		return nil, nil
	}
	if !c.breaker.allow() {
		return nil, nil // open circuit: skip the call entirely
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("risk: marshal model request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/score",
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("risk: build model request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.breaker.failure()
		return nil, fmt.Errorf("risk: model request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.breaker.failure()
		return nil, fmt.Errorf("risk: model returned %d", resp.StatusCode)
	}

	var out ModelResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.breaker.failure()
		return nil, fmt.Errorf("risk: decode model response: %w", err)
	}

	c.breaker.success()
	return &out, nil
}

// breaker is a minimal circuit breaker.
//
// Hand-rolled rather than pulling in a dependency because the behaviour needed
// is exactly this: count consecutive failures, stop calling for a cooldown,
// then let a single probe through. A library would add configuration surface
// for states this service never enters.
type breaker struct {
	mu        sync.Mutex
	failures  int
	threshold int
	cooldown  time.Duration
	openedAt  time.Time
	probing   bool
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown}
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.failures < b.threshold {
		return true
	}
	// Open. After the cooldown, let exactly one request through to test
	// recovery — letting them all through would re-hammer a service that is
	// still coming back up.
	if time.Since(b.openedAt) > b.cooldown && !b.probing {
		b.probing = true
		return true
	}
	return false
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.probing = false
}

func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures++
	b.probing = false
	if b.failures == b.threshold {
		b.openedAt = time.Now()
	} else if b.failures > b.threshold {
		// A failed probe restarts the cooldown.
		b.openedAt = time.Now()
	}
}

// State reports the breaker state, for metrics and tests.
func (c *Client) State() (open bool, failures int) {
	if c == nil || c.breaker == nil {
		return false, 0
	}
	c.breaker.mu.Lock()
	defer c.breaker.mu.Unlock()
	return c.breaker.failures >= c.breaker.threshold, c.breaker.failures
}
