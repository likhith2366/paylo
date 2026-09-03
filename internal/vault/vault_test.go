package vault_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/likhith2366/paylo/internal/testsupport"
	"github.com/likhith2366/paylo/internal/vault"
)

const (
	testPAN  = "4242424242424242"
	testSalt = "test-salt"
)

func newVault(t *testing.T, ttl time.Duration) *vault.Service {
	t.Helper()

	pool := testsupport.NewPostgres(t)
	keyB64, err := vault.NewMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	masterKey, err := vault.ParseMasterKey(keyB64)
	if err != nil {
		t.Fatalf("parse master key: %v", err)
	}
	keys, err := vault.NewLocalKeyManager(masterKey)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	return vault.NewService(pool, keys, testSalt, ttl)
}

func TestTokenizeAndDetokenizeRoundTrip(t *testing.T) {
	svc := newVault(t, 15*time.Minute)
	ctx := context.Background()

	token, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: testPAN, ExpMonth: 12, ExpYear: 2030, SingleUse: true,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	// The token itself must reveal nothing about the card.
	if !strings.HasPrefix(token.Token, "tok_") {
		t.Errorf("token %q has an unexpected shape", token.Token)
	}
	if strings.Contains(token.Token, "4242") {
		t.Error("token contains digits from the card number")
	}
	if token.Last4 != "4242" || token.Brand != "visa" || token.BIN != "424242" {
		t.Errorf("metadata wrong: last4=%q brand=%q bin=%q", token.Last4, token.Brand, token.BIN)
	}

	pan, err := svc.Detokenize(ctx, token.Token, "test", "round trip")
	if err != nil {
		t.Fatalf("detokenize: %v", err)
	}
	if pan != testPAN {
		t.Errorf("detokenized %q, want %q", pan, testPAN)
	}
}

// A single-use token must survive exactly one detokenization, so a leaked
// token cannot be replayed into a second charge.
func TestSingleUseTokenCannotBeReused(t *testing.T) {
	svc := newVault(t, 15*time.Minute)
	ctx := context.Background()

	token, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: testPAN, ExpMonth: 12, ExpYear: 2030, SingleUse: true,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	if _, err := svc.Detokenize(ctx, token.Token, "test", "first use"); err != nil {
		t.Fatalf("first detokenize should succeed: %v", err)
	}
	if _, err := svc.Detokenize(ctx, token.Token, "test", "second use"); err != vault.ErrTokenConsumed {
		t.Errorf("second detokenize returned %v, want ErrTokenConsumed", err)
	}
}

// The consume-on-read must be atomic: two charges presenting the same
// single-use token simultaneously must not both get the card.
func TestConcurrentDetokenizeConsumesOnce(t *testing.T) {
	svc := newVault(t, 15*time.Minute)
	ctx := context.Background()

	token, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: testPAN, ExpMonth: 12, ExpYear: 2030, SingleUse: true,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	const attempts = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = svc.Detokenize(ctx, token.Token, "test", "concurrent")
		}(i)
	}
	close(start)
	wg.Wait()

	var succeeded int
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("%d of %d concurrent detokenizations succeeded, want exactly 1",
			succeeded, attempts)
	}
}

// Multi-use tokens back saved cards, so they must survive repeated charges.
func TestMultiUseTokenSurvivesRepeatedUse(t *testing.T) {
	svc := newVault(t, 15*time.Minute)
	ctx := context.Background()

	token, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: testPAN, ExpMonth: 12, ExpYear: 2030, SingleUse: false,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := svc.Detokenize(ctx, token.Token, "test", "subscription renewal"); err != nil {
			t.Fatalf("use %d failed: %v", i, err)
		}
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	// A TTL already in the past, so the token is expired the moment it exists.
	svc := newVault(t, -1*time.Minute)
	ctx := context.Background()

	token, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: testPAN, ExpMonth: 12, ExpYear: 2030, SingleUse: true,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	if _, err := svc.Detokenize(ctx, token.Token, "test", "expired"); err != vault.ErrTokenExpired {
		t.Errorf("detokenize returned %v, want ErrTokenExpired", err)
	}
}

func TestUnknownTokenRejected(t *testing.T) {
	svc := newVault(t, 15*time.Minute)
	if _, err := svc.Detokenize(context.Background(), "tok_nonexistent", "test", "probe"); err != vault.ErrTokenNotFound {
		t.Errorf("detokenize returned %v, want ErrTokenNotFound", err)
	}
}

func TestInvalidCardsRejectedBeforeStorage(t *testing.T) {
	svc := newVault(t, 15*time.Minute)
	ctx := context.Background()

	cases := []struct {
		name  string
		input vault.TokenizeInput
	}{
		{"bad checksum", vault.TokenizeInput{Number: "4242424242424243", ExpMonth: 12, ExpYear: 2030}},
		{"too short", vault.TokenizeInput{Number: "424242", ExpMonth: 12, ExpYear: 2030}},
		{"expired card", vault.TokenizeInput{Number: testPAN, ExpMonth: 1, ExpYear: 2020}},
		{"bad month", vault.TokenizeInput{Number: testPAN, ExpMonth: 13, ExpYear: 2030}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Tokenize(ctx, tc.input); err == nil {
				t.Error("expected tokenize to reject this card")
			}
		})
	}
}

// The same card must produce the same fingerprint across separate tokens —
// that is what lets velocity rules and blocklists work at all (§14.5).
func TestFingerprintIsStableAcrossTokens(t *testing.T) {
	svc := newVault(t, 15*time.Minute)
	ctx := context.Background()

	first, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: testPAN, ExpMonth: 12, ExpYear: 2030, SingleUse: true,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	second, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: testPAN, ExpMonth: 12, ExpYear: 2030, SingleUse: true,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	if first.Token == second.Token {
		t.Error("two tokenizations produced the same token")
	}
	if first.Fingerprint != second.Fingerprint {
		t.Error("the same card produced different fingerprints")
	}

	other, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: "5555555555554444", ExpMonth: 12, ExpYear: 2030, SingleUse: true,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	if other.Fingerprint == first.Fingerprint {
		t.Error("different cards share a fingerprint")
	}
}

// Metadata is what the Payments API calls on every charge. It must never be
// able to return a card number, no matter what the caller does with the result.
func TestMetadataNeverExposesPAN(t *testing.T) {
	svc := newVault(t, 15*time.Minute)
	ctx := context.Background()

	token, err := svc.Tokenize(ctx, vault.TokenizeInput{
		Number: testPAN, ExpMonth: 12, ExpYear: 2030, SingleUse: true,
	})
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	meta, err := svc.Metadata(ctx, token.Token)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}

	// Serialize every field and confirm the PAN appears nowhere in it.
	rendered := meta.Token + meta.Brand + meta.Last4 + meta.BIN + meta.Fingerprint
	if strings.Contains(rendered, testPAN) {
		t.Error("metadata contains the full card number")
	}
	// Reading metadata must not consume a single-use token.
	if _, err := svc.Detokenize(ctx, token.Token, "test", "after metadata"); err != nil {
		t.Errorf("metadata consumed the token: %v", err)
	}
}
