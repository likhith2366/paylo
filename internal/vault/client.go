package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client is how the Payments API talks to the vault.
//
// It exposes Metadata and Detokenize as separate calls on purpose. The charge
// flow needs card metadata for risk scoring and display on every request, but
// needs the actual PAN only at the moment of submitting to the processor — so
// the sensitive call happens once, late, and is separately audited.
type Client struct {
	baseURL        string
	internalSecret string
	http           *http.Client
}

func NewClient(baseURL, internalSecret string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	return &Client{
		baseURL:        baseURL,
		internalSecret: internalSecret,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Metadata fetches the non-sensitive attributes of a token.
func (c *Client) Metadata(ctx context.Context, token string) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/vault/tokens/"+token, nil)
	if err != nil {
		return nil, fmt.Errorf("vault client: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault client: metadata request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrTokenNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vault client: metadata returned %d", resp.StatusCode)
	}

	var t Token
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("vault client: decode metadata: %w", err)
	}
	return &t, nil
}

// Detokenize retrieves a raw PAN for immediate submission to the processor.
//
// The returned value must be passed straight to the bank client and never
// stored, logged, or included in an error. caller and reason are recorded in
// the vault's audit log.
func (c *Client) Detokenize(ctx context.Context, token, caller, reason string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"token": token, "caller": caller, "reason": reason,
	})
	if err != nil {
		return "", fmt.Errorf("vault client: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/vault/detokenize", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("vault client: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Internal-Secret", c.internalSecret)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault client: detokenize request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", ErrTokenNotFound
	case http.StatusGone:
		return "", ErrTokenExpired
	case http.StatusConflict:
		return "", ErrTokenConsumed
	default:
		return "", fmt.Errorf("vault client: detokenize returned %d", resp.StatusCode)
	}

	var out struct {
		Number string `json:"number"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("vault client: decode response: %w", err)
	}
	return out.Number, nil
}
