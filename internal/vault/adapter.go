package vault

import (
	"context"
	"errors"
	"fmt"

	"github.com/likhith2366/paylo/internal/payments"
)

// PaymentsAdapter presents this vault client as a payments.VaultClient.
//
// It exists to keep the dependency pointing one way: the payments package
// declares the narrow interface it needs and knows nothing about the vault,
// while this package — which already imports payments for card validation —
// supplies the implementation. Without it the two packages would import each
// other.
//
// It also collapses the vault's specific token errors into a single
// payments.ErrTokenUnusable, so the API cannot distinguish "no such token"
// from "already spent" for a caller.
type PaymentsAdapter struct {
	client *Client
}

func NewPaymentsAdapter(client *Client) *PaymentsAdapter {
	return &PaymentsAdapter{client: client}
}

var _ payments.VaultClient = (*PaymentsAdapter)(nil)

func (a *PaymentsAdapter) Metadata(ctx context.Context, token string) (*payments.CardMetadata, error) {
	t, err := a.client.Metadata(ctx, token)
	if err != nil {
		return nil, translate(err)
	}
	return &payments.CardMetadata{
		Brand:       t.Brand,
		Last4:       t.Last4,
		BIN:         t.BIN,
		Fingerprint: t.Fingerprint,
		ExpMonth:    t.ExpMonth,
		ExpYear:     t.ExpYear,
	}, nil
}

func (a *PaymentsAdapter) Detokenize(ctx context.Context, token, caller, reason string) (string, error) {
	pan, err := a.client.Detokenize(ctx, token, caller, reason)
	if err != nil {
		return "", translate(err)
	}
	return pan, nil
}

func translate(err error) error {
	switch {
	case errors.Is(err, ErrTokenNotFound),
		errors.Is(err, ErrTokenExpired),
		errors.Is(err, ErrTokenConsumed):
		// Wrapped so the underlying cause stays available in logs, while the
		// error the API branches on is the indistinguishable one.
		return fmt.Errorf("%w: %v", payments.ErrTokenUnusable, err)
	default:
		return err
	}
}
