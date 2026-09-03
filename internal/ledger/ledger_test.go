package ledger_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/ledger"
	"github.com/likhith2366/paylo/internal/money"
	"github.com/likhith2366/paylo/internal/testsupport"
)

func TestPostBalancedTransaction(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	var receivable, balance uuid.UUID
	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if receivable, err = ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, "USD"); err != nil {
			return err
		}
		if balance, err = ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, "USD"); err != nil {
			return err
		}
		return ledger.Post(ctx, tx, ledger.Transaction{
			ID: uuid.New(),
			Legs: []ledger.Leg{
				ledger.Debit(receivable, money.New(10_000, "USD"), ledger.EntryCharge),
				ledger.Credit(balance, money.New(10_000, "USD"), ledger.EntryCharge),
			},
		})
	})
	if err != nil {
		t.Fatalf("post balanced transaction: %v", err)
	}

	// The cached balance and the derived balance must agree. Divergence between
	// them is the exact condition reconciliation exists to catch (§24.3).
	cached, err := ledger.Balance(ctx, pool, balance)
	if err != nil {
		t.Fatalf("read cached balance: %v", err)
	}
	if cached.Cents != -10_000 {
		t.Errorf("cached merchant balance = %d, want -10000 (credit)", cached.Cents)
	}

	derived, err := ledger.DerivedBalance(ctx, pool, balance)
	if err != nil {
		t.Fatalf("derive balance: %v", err)
	}
	if derived.Cents != cached.Cents {
		t.Errorf("cached (%d) and derived (%d) balances diverge", cached.Cents, derived.Cents)
	}
}

// The deferred constraint trigger is the last line of defence against money
// being created or destroyed. If it ever stops firing, every other correctness
// claim in the system rests on application code alone.
func TestUnbalancedTransactionRejectedByDatabase(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		receivable, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, "USD")
		if err != nil {
			return err
		}
		balance, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, "USD")
		if err != nil {
			return err
		}

		// Bypass ledger.Post's own validation to prove the DB catches this
		// independently — the point is that a bug in Go cannot corrupt the ledger.
		txnID := uuid.New()
		for _, leg := range []struct {
			acct   uuid.UUID
			dir    string
			amount int64
		}{
			{receivable, "debit", 10_000},
			{balance, "credit", 9_000}, // 1000 cents unaccounted for
		} {
			if _, err := tx.Exec(ctx, `
				INSERT INTO ledger_entries
					(transaction_id, account_id, direction, amount_cents, currency, entry_type)
				VALUES ($1,$2,$3,$4,'USD','charge')`,
				txnID, leg.acct, leg.dir, leg.amount,
			); err != nil {
				return err
			}
		}
		return nil
	})

	if err == nil {
		t.Fatal("expected the deferred constraint to reject an unbalanced transaction")
	}
	if !strings.Contains(err.Error(), "unbalanced ledger transaction") {
		t.Errorf("expected an imbalance error, got: %v", err)
	}
}

// A single transaction may not mix currencies (§20): 100 USD debited against
// 100 EUR credited is not balanced in any meaningful sense, even though the
// raw numbers cancel.
func TestCurrenciesMustBalanceIndependently(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		usdAcct, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, "USD")
		if err != nil {
			return err
		}
		eurAcct, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, "EUR")
		if err != nil {
			return err
		}
		return ledger.Post(ctx, tx, ledger.Transaction{
			ID: uuid.New(),
			Legs: []ledger.Leg{
				ledger.Debit(usdAcct, money.New(10_000, "USD"), ledger.EntryCharge),
				ledger.Credit(eurAcct, money.New(10_000, "EUR"), ledger.EntryCharge),
			},
		})
	})

	if err == nil {
		t.Fatal("expected a cross-currency transaction to be rejected")
	}
}

// Append-only is enforced by the database, not merely by convention.
func TestLedgerEntriesAreImmutable(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	var entryID int64
	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		receivable, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, "USD")
		if err != nil {
			return err
		}
		balance, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, "USD")
		if err != nil {
			return err
		}
		if err := ledger.Post(ctx, tx, ledger.Transaction{
			ID: uuid.New(),
			Legs: []ledger.Leg{
				ledger.Debit(receivable, money.New(5_000, "USD"), ledger.EntryCharge),
				ledger.Credit(balance, money.New(5_000, "USD"), ledger.EntryCharge),
			},
		}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT id FROM ledger_entries LIMIT 1`).Scan(&entryID)
	})
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE ledger_entries SET amount_cents = 1 WHERE id = $1`, entryID); err == nil {
		t.Error("expected UPDATE on ledger_entries to be rejected")
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM ledger_entries WHERE id = $1`, entryID); err == nil {
		t.Error("expected DELETE on ledger_entries to be rejected")
	}
}

// Concurrent writers to one account must not lose updates. The cached balance
// is the only mutable financial value in the system, so this is where a
// lost-update bug would actually cost money (§24.2).
func TestConcurrentBalanceUpdatesDoNotLoseWrites(t *testing.T) {
	pool := testsupport.NewPostgres(t)
	ctx := context.Background()
	merchantID := testsupport.CreateMerchant(t, pool, "acme")

	var receivable, balance uuid.UUID
	if err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		receivable, err = ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, "USD")
		if err != nil {
			return err
		}
		balance, err = ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, "USD")
		return err
	}); err != nil {
		t.Fatalf("create accounts: %v", err)
	}

	const workers = 50
	const amount = 100

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines at once to maximise contention
			errs <- db.InTx(ctx, pool, func(tx pgx.Tx) error {
				return ledger.Post(ctx, tx, ledger.Transaction{
					ID: uuid.New(),
					Legs: []ledger.Leg{
						ledger.Debit(receivable, money.New(amount, "USD"), ledger.EntryCharge),
						ledger.Credit(balance, money.New(amount, "USD"), ledger.EntryCharge),
					},
				})
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent post failed: %v", err)
		}
	}

	cached, err := ledger.Balance(ctx, pool, balance)
	if err != nil {
		t.Fatalf("read cached balance: %v", err)
	}
	derived, err := ledger.DerivedBalance(ctx, pool, balance)
	if err != nil {
		t.Fatalf("derive balance: %v", err)
	}

	want := int64(-workers * amount)
	if cached.Cents != want {
		t.Errorf("cached balance = %d, want %d — an update was lost", cached.Cents, want)
	}
	if derived.Cents != want {
		t.Errorf("derived balance = %d, want %d", derived.Cents, want)
	}
}

func TestValidateRejectsBadTransactions(t *testing.T) {
	acct := uuid.New()

	cases := []struct {
		name string
		txn  ledger.Transaction
	}{
		{"no legs", ledger.Transaction{ID: uuid.New()}},
		{"unbalanced", ledger.Transaction{ID: uuid.New(), Legs: []ledger.Leg{
			ledger.Debit(acct, money.New(100, "USD"), ledger.EntryCharge),
			ledger.Credit(acct, money.New(99, "USD"), ledger.EntryCharge),
		}}},
		{"zero amount", ledger.Transaction{ID: uuid.New(), Legs: []ledger.Leg{
			ledger.Debit(acct, money.New(0, "USD"), ledger.EntryCharge),
			ledger.Credit(acct, money.New(0, "USD"), ledger.EntryCharge),
		}}},
		{"negative amount", ledger.Transaction{ID: uuid.New(), Legs: []ledger.Leg{
			ledger.Debit(acct, money.New(-100, "USD"), ledger.EntryCharge),
			ledger.Credit(acct, money.New(-100, "USD"), ledger.EntryCharge),
		}}},
		{"bad direction", ledger.Transaction{ID: uuid.New(), Legs: []ledger.Leg{
			{AccountID: acct, Direction: "sideways", Amount: money.New(100, "USD")},
		}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.txn.Validate(); err == nil {
				t.Error("expected Validate to reject this transaction")
			}
		})
	}
}
