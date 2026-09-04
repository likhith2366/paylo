// Command seed creates a test merchant with an API key and a payout account.
//
// The raw API key is printed exactly once, here — only its hash is stored (§8),
// so there is no way to recover it afterwards.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/likhith2366/paylo/internal/config"
	"github.com/likhith2366/paylo/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, 4)
	if err != nil {
		return err
	}
	defer pool.Close()

	name := "Demo Store"
	email := fmt.Sprintf("demo+%s@example.test", uuid.NewString()[:8])

	var merchantID uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO merchants (name, email, category)
		VALUES ($1, $2, 'shopping_net') RETURNING id`,
		name, email,
	).Scan(&merchantID)
	if err != nil {
		return fmt.Errorf("create merchant: %w", err)
	}

	raw, err := generateKey()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(raw))

	_, err = pool.Exec(ctx, `
		INSERT INTO api_keys (merchant_id, key_hash, key_prefix, mode, scope)
		VALUES ($1, $2, $3, 'test', 'write')`,
		merchantID, hex.EncodeToString(sum[:]), raw[:16],
	)
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}

	// Verified so the payout batch will pick it up. A real account requires
	// micro-deposit verification, which is stubbed per §0.1.
	_, err = pool.Exec(ctx, `
		INSERT INTO payout_accounts
			(merchant_id, account_last4, routing_last4, account_token, currency, verified_at)
		VALUES ($1, '6789', '4321', $2, 'USD', now())`,
		merchantID, "ba_tok_"+uuid.NewString()[:12],
	)
	if err != nil {
		return fmt.Errorf("create payout account: %w", err)
	}

	fmt.Printf("\nmerchant   %s\n", merchantID)
	fmt.Printf("email      %s\n", email)
	fmt.Printf("api key    %s\n", raw)
	fmt.Printf("\nShown once — only the hash is stored. Try:\n\n")
	fmt.Printf("  TOKEN=$(curl -s -X POST localhost:8081/vault/tokenize \\\n")
	fmt.Printf("    -H 'Content-Type: application/json' \\\n")
	fmt.Printf("    -d '{\"number\":\"4242424242424242\",\"exp_month\":12,\"exp_year\":2030}' \\\n")
	fmt.Printf("    | python -c 'import sys,json;print(json.load(sys.stdin)[\"token\"])')\n\n")
	fmt.Printf("  curl -X POST localhost:8080/v1/charges \\\n")
	fmt.Printf("    -H 'Authorization: Bearer %s' \\\n", raw)
	fmt.Printf("    -H \"Idempotency-Key: $(uuidgen)\" \\\n")
	fmt.Printf("    -H 'Content-Type: application/json' \\\n")
	fmt.Printf("    -d \"{\\\"amount\\\":4500,\\\"currency\\\":\\\"USD\\\",\\\"payment_token\\\":\\\"$TOKEN\\\"}\"\n\n")
	return nil
}

// generateKey mirrors Stripe's convention (§8), which merchants already
// recognise: the prefix says at a glance whether a leaked key is dangerous.
func generateKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return "sk_test_" + hex.EncodeToString(buf), nil
}
