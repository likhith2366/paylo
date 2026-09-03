package testsupport

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateMerchant inserts a merchant and returns its ID.
func CreateMerchant(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO merchants (name, email) VALUES ($1, $2) RETURNING id`,
		name, name+"@example.test",
	).Scan(&id)
	if err != nil {
		t.Fatalf("testsupport: create merchant: %v", err)
	}
	return id
}

// CreateAPIKey inserts an API key for a merchant and returns the raw key,
// which is the only moment it exists in plaintext (§8).
func CreateAPIKey(t *testing.T, pool *pgxpool.Pool, merchantID uuid.UUID, keyHash string) {
	t.Helper()

	_, err := pool.Exec(context.Background(), `
		INSERT INTO api_keys (merchant_id, key_hash, key_prefix, mode, scope)
		VALUES ($1, $2, $3, 'test', 'write')`,
		merchantID, keyHash, "sk_test_fixture",
	)
	if err != nil {
		t.Fatalf("testsupport: create api key: %v", err)
	}
}

// CountCharges returns how many charges exist for a merchant in a given status.
func CountCharges(t *testing.T, pool *pgxpool.Pool, merchantID uuid.UUID, status string) int {
	t.Helper()

	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM charges WHERE merchant_id = $1 AND status = $2`,
		merchantID, status,
	).Scan(&n)
	if err != nil {
		t.Fatalf("testsupport: count charges: %v", err)
	}
	return n
}
