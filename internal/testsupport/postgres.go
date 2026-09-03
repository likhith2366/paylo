// Package testsupport spins up real dependencies for integration tests.
//
// Tests here run against an actual Postgres via testcontainers rather than a
// mock, deliberately (§22.3). Idempotency and ledger correctness depend on
// unique constraints, row locks, deferred constraint triggers, and the exact
// semantics of ON CONFLICT — none of which a mock reproduces. A mocked test
// suite here would pass while the real system double-charged people.
package testsupport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// NewPostgres starts a throwaway Postgres, applies all migrations, and returns
// a connected pool. The container is terminated when the test finishes.
func NewPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("paylo_test"),
		tcpostgres.WithUsername("paylo"),
		tcpostgres.WithPassword("paylo"),
		testcontainers.WithWaitStrategy(
			// Postgres briefly accepts connections during init before
			// restarting, so waiting for the readiness line twice is what
			// avoids connecting to a server that is about to go away.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("testsupport: start postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("testsupport: terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("testsupport: connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testsupport: connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := applyMigrations(ctx, pool); err != nil {
		t.Fatalf("testsupport: apply migrations: %v", err)
	}
	return pool
}

// applyMigrations runs every *.up.sql in migrations/ in filename order.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := migrationsDir()
	if err != nil {
		return err
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found in %s", dir)
	}

	for _, file := range files {
		sql, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(file), err)
		}
	}
	return nil
}

// migrationsDir locates migrations/ relative to this source file, so tests
// work regardless of which package directory `go test` runs from.
func migrationsDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot determine caller path")
	}
	// internal/testsupport/postgres.go -> repo root
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	dir := filepath.Join(root, "migrations")
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("stat migrations dir %s: %w", dir, err)
	}
	return dir, nil
}
