package vault

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/payments"
)

var (
	ErrTokenNotFound = errors.New("vault: token not found")
	ErrTokenExpired  = errors.New("vault: token has expired")
	ErrTokenConsumed = errors.New("vault: single-use token has already been used")
	ErrInvalidCard   = errors.New("vault: card number is not valid")
	ErrCardExpired   = errors.New("vault: card has expired")
)

type Service struct {
	pool     *pgxpool.Pool
	keys     KeyManager
	hashSalt string
	// ttl is how long a checkout token stays valid — long enough to finish an
	// order, short enough that a leaked token from a browser session is stale
	// before it can be used.
	ttl time.Duration
}

func NewService(pool *pgxpool.Pool, keys KeyManager, hashSalt string, ttl time.Duration) *Service {
	if ttl == 0 {
		ttl = 15 * time.Minute
	}
	return &Service{pool: pool, keys: keys, hashSalt: hashSalt, ttl: ttl}
}

// TokenizeInput carries a raw PAN. Instances must never be logged, and the CVC
// is deliberately absent: PCI DSS prohibits storing it, so it is passed
// straight through to the processor at charge time and never reaches the vault.
type TokenizeInput struct {
	Number     string
	ExpMonth   int
	ExpYear    int
	MerchantID *uuid.UUID
	// SingleUse tokens are consumed by the first charge that presents them.
	// Saved cards ("keep this on file") set this false.
	SingleUse bool
}

// Token is what leaves the vault. Every field here is safe to return to the
// browser, store in the Payments API, and display in a dashboard.
type Token struct {
	Token       string    `json:"token"`
	Brand       string    `json:"brand"`
	Last4       string    `json:"last4"`
	BIN         string    `json:"bin"`
	Fingerprint string    `json:"fingerprint"`
	ExpMonth    int       `json:"exp_month"`
	ExpYear     int       `json:"exp_year"`
	ExpiresAt   time.Time `json:"expires_at"`
	SingleUse   bool      `json:"single_use"`
}

// Tokenize encrypts a card number and returns an opaque handle to it.
//
// This is the only entry point in the entire system that accepts a PAN. It is
// called directly by the hosted checkout iframe, so the number travels from the
// browser to here without passing through the merchant's server or our own
// Payments API (§2.4).
func (s *Service) Tokenize(ctx context.Context, in TokenizeInput) (*Token, error) {
	// Validate before encrypting: a typo'd number should fail here, cheaply,
	// rather than at authorization time after a round trip to the processor.
	brand, err := payments.ValidateNumber(in.Number)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCard, err)
	}
	if err := validateExpiry(in.ExpMonth, in.ExpYear); err != nil {
		return nil, err
	}

	dataKey, wrappedKey, keyID, err := s.keys.GenerateDataKey()
	if err != nil {
		return nil, err
	}
	// Zero the plaintext key once we're done with it. Go's GC makes this
	// best-effort rather than a guarantee, but leaving a key sitting in a
	// reachable buffer for the rest of the process's life is strictly worse.
	defer zero(dataKey)

	ciphertext, err := encryptAESGCM(dataKey, []byte(onlyDigits(in.Number)))
	if err != nil {
		return nil, fmt.Errorf("vault: encrypt pan: %w", err)
	}

	token, err := generateToken()
	if err != nil {
		return nil, err
	}

	// Derived from the PAN under a secret salt, so the same card presented
	// twice gets the same fingerprint — which is what makes velocity rules and
	// blocklists work across tokens (§14.5).
	fingerprint := payments.Fingerprint(in.Number, s.hashSalt)
	expiresAt := time.Now().UTC().Add(s.ttl)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO vault_tokens
				(token, pan_ciphertext, encrypted_data_key, key_id,
				 card_brand, card_last4, card_bin, card_fingerprint,
				 exp_month, exp_year, merchant_id, single_use, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now() + $13::interval)`,
			token, ciphertext, wrappedKey, keyID,
			string(brand), payments.Last4(in.Number), payments.BIN(in.Number), fingerprint,
			in.ExpMonth, in.ExpYear, in.MerchantID, in.SingleUse, s.ttl.String(),
		)
		if err != nil {
			return fmt.Errorf("vault: store token: %w", err)
		}
		return logAccess(ctx, tx, token, "tokenize", "checkout", "", true)
	})
	if err != nil {
		return nil, err
	}

	// Note what is logged: the token and the last four, never the PAN.
	slog.Info("vault: card tokenized",
		"token", token, "brand", brand, "last4", payments.Last4(in.Number))

	return &Token{
		Token:       token,
		Brand:       string(brand),
		Last4:       payments.Last4(in.Number),
		BIN:         payments.BIN(in.Number),
		Fingerprint: fingerprint,
		ExpMonth:    in.ExpMonth,
		ExpYear:     in.ExpYear,
		ExpiresAt:   expiresAt,
		SingleUse:   in.SingleUse,
	}, nil
}

// Metadata returns everything about a token except the card number.
//
// This is what the Payments API calls. It deliberately cannot return a PAN, so
// no bug in the Payments API can cause one to be exposed.
func (s *Service) Metadata(ctx context.Context, token string) (*Token, error) {
	var t Token
	var consumedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT token, card_brand, card_last4, card_bin, card_fingerprint,
		       exp_month, exp_year, expires_at, single_use, consumed_at
		FROM vault_tokens WHERE token = $1`,
		token,
	).Scan(&t.Token, &t.Brand, &t.Last4, &t.BIN, &t.Fingerprint,
		&t.ExpMonth, &t.ExpYear, &t.ExpiresAt, &t.SingleUse, &consumedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("vault: read token metadata: %w", err)
	}
	if consumedAt != nil {
		return nil, ErrTokenConsumed
	}
	return &t, nil
}

// Detokenize returns the raw PAN.
//
// This is the most sensitive operation in the system. It is restricted at the
// network layer to the charge-submission path alone, every call is audit
// logged, and single-use tokens are consumed atomically so a leaked token
// cannot be replayed.
//
// The caller must use the returned value immediately and never store, log, or
// return it.
func (s *Service) Detokenize(ctx context.Context, token, caller, reason string) (string, error) {
	var (
		ciphertext []byte
		wrappedKey []byte
		keyID      string
		singleUse  bool
		consumedAt *time.Time
		expired    bool
	)

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// FOR UPDATE serializes concurrent presentations of the same token, so
		// two simultaneous charges cannot both consume a single-use token.
		// Expiry is evaluated against Postgres now(), not a pod clock (§22.1).
		err := tx.QueryRow(ctx, `
			SELECT pan_ciphertext, encrypted_data_key, key_id, single_use,
			       consumed_at, (expires_at <= now()) AS expired
			FROM vault_tokens WHERE token = $1
			FOR UPDATE`,
			token,
		).Scan(&ciphertext, &wrappedKey, &keyID, &singleUse, &consumedAt, &expired)

		if errors.Is(err, pgx.ErrNoRows) {
			_ = logAccess(ctx, tx, token, "detokenize", caller, "token not found", false)
			return ErrTokenNotFound
		}
		if err != nil {
			return fmt.Errorf("vault: load token: %w", err)
		}
		if expired {
			_ = logAccess(ctx, tx, token, "detokenize", caller, "token expired", false)
			return ErrTokenExpired
		}
		if consumedAt != nil {
			_ = logAccess(ctx, tx, token, "detokenize", caller, "already consumed", false)
			return ErrTokenConsumed
		}

		// Marked consumed in the same transaction as the read, so the check and
		// the claim cannot be separated by a concurrent caller.
		if singleUse {
			if _, err := tx.Exec(ctx,
				`UPDATE vault_tokens SET consumed_at = now() WHERE token = $1`, token,
			); err != nil {
				return fmt.Errorf("vault: consume token: %w", err)
			}
		}
		return logAccess(ctx, tx, token, "detokenize", caller, reason, true)
	})
	if err != nil {
		return "", err
	}

	dataKey, err := s.keys.UnwrapDataKey(wrappedKey, keyID)
	if err != nil {
		return "", err
	}
	defer zero(dataKey)

	pan, err := decryptAESGCM(dataKey, ciphertext)
	if err != nil {
		return "", fmt.Errorf("vault: decrypt pan: %w", err)
	}

	slog.Info("vault: card detokenized", "token", token, "caller", caller, "reason", reason)
	return string(pan), nil
}

// PurgeExpired deletes tokens past their TTL. Run on a schedule (§9): a vault
// that accumulates card data it no longer needs is carrying risk for nothing.
func (s *Service) PurgeExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM vault_tokens WHERE expires_at <= now() AND NOT single_use OR
		 (single_use AND (consumed_at IS NOT NULL OR expires_at <= now()))`)
	if err != nil {
		return 0, fmt.Errorf("vault: purge expired tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

func logAccess(ctx context.Context, tx pgx.Tx, token, operation, caller, reason string, ok bool) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO vault_access_log (token, operation, caller, reason, succeeded)
		VALUES ($1, $2, $3, $4, $5)`,
		token, operation, caller, nullIfEmpty(reason), ok,
	)
	if err != nil {
		return fmt.Errorf("vault: write access log: %w", err)
	}
	return nil
}

// generateToken produces a random, opaque handle.
//
// Random rather than derived from the card: a token computed from the PAN would
// be a hash an attacker could attack offline. This one carries no information
// about the card at all, which is what makes it safe to put in a URL, a log
// line, or a merchant's database.
func generateToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("vault: generate token: %w", err)
	}
	return "tok_" + hex.EncodeToString(buf), nil
}

func validateExpiry(month, year int) error {
	if month < 1 || month > 12 {
		return fmt.Errorf("%w: expiry month %d is out of range", ErrInvalidCard, month)
	}
	// Cards expire at the end of their stated month.
	expiry := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).
		AddDate(0, 1, 0)
	if expiry.Before(time.Now().UTC()) {
		return ErrCardExpired
	}
	return nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func onlyDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
