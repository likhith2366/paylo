// Package ledger implements append-only double-entry bookkeeping (§5).
//
// Two rules hold everywhere in this package:
//
//  1. Nothing is ever mutated. Corrections are new, balanced entries that
//     reference the original transaction — reversals, not edits.
//  2. Every transaction balances: debits equal credits, per currency.
//
// Rule 2 is enforced three times over — in Go before the write, by a deferred
// Postgres constraint trigger at COMMIT, and again by the reconciliation job.
// Belt and braces is proportionate when the failure mode is unaccounted money.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/likhith2366/paylo/internal/money"
)

// Account types. Merchant-scoped accounts carry a merchant_id; platform
// accounts are singletons per currency.
const (
	AccountMerchantBalance    = "merchant_balance"
	AccountCustomerReceivable = "customer_receivable"
	AccountPlatformFees       = "platform_fees"
	AccountInTransit          = "in_transit"
	AccountPaidOut            = "paid_out"
	AccountMerchantDebt       = "merchant_debt"
	AccountReserve            = "reserve"
)

// Entry types, recording what caused a pair of entries to exist.
const (
	EntryCharge          = "charge"
	EntryRefund          = "refund"
	EntryDispute         = "dispute"
	EntryDisputeReversal = "dispute_reversal"
	EntryPayout          = "payout"
	EntryFee             = "fee"
)

const (
	DirectionDebit  = "debit"
	DirectionCredit = "credit"
)

var (
	ErrUnbalanced      = errors.New("ledger: transaction does not balance")
	ErrNoLegs          = errors.New("ledger: transaction has no entries")
	ErrAccountNotFound = errors.New("ledger: account not found")
)

// Leg is one side of a double-entry transaction.
type Leg struct {
	AccountID uuid.UUID
	Direction string
	Amount    money.Amount
	EntryType string
	Metadata  map[string]any
}

func Debit(accountID uuid.UUID, amount money.Amount, entryType string) Leg {
	return Leg{AccountID: accountID, Direction: DirectionDebit, Amount: amount, EntryType: entryType}
}

func Credit(accountID uuid.UUID, amount money.Amount, entryType string) Leg {
	return Leg{AccountID: accountID, Direction: DirectionCredit, Amount: amount, EntryType: entryType}
}

// Transaction is a set of legs written atomically.
type Transaction struct {
	ID             uuid.UUID
	Legs           []Leg
	IdempotencyKey string
}

// Validate checks the balance invariant before touching the database.
//
// The DB trigger would catch an imbalance anyway, but failing here gives a
// clear Go-level error at the call site instead of an opaque constraint
// violation surfacing at COMMIT, potentially several function calls away.
func (t Transaction) Validate() error {
	if len(t.Legs) == 0 {
		return ErrNoLegs
	}

	// Per-currency, because a balanced pair may never mix currencies (§20).
	delta := make(map[string]int64)
	for i, leg := range t.Legs {
		if err := leg.Amount.Validate(); err != nil {
			return fmt.Errorf("leg %d: %w", i, err)
		}
		switch leg.Direction {
		case DirectionDebit:
			delta[leg.Amount.Currency] += leg.Amount.Cents
		case DirectionCredit:
			delta[leg.Amount.Currency] -= leg.Amount.Cents
		default:
			return fmt.Errorf("leg %d: invalid direction %q", i, leg.Direction)
		}
	}

	for currency, d := range delta {
		if d != 0 {
			return fmt.Errorf("%w: %s is off by %d minor units", ErrUnbalanced, currency, d)
		}
	}
	return nil
}

// Post writes a balanced transaction and updates cached balances.
//
// It takes a pgx.Tx rather than a pool because the caller must be able to
// commit the ledger write, the idempotency record, and the outbox row in ONE
// transaction (§4.2 step 5, §22.1). Owning its own transaction here would
// reintroduce exactly the inconsistency the outbox pattern exists to prevent.
func Post(ctx context.Context, tx pgx.Tx, t Transaction) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}

	for _, leg := range t.Legs {
		metadata := leg.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries
				(transaction_id, account_id, direction, amount_cents,
				 currency, entry_type, idempotency_key, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			t.ID, leg.AccountID, leg.Direction, leg.Amount.Cents,
			leg.Amount.Currency, leg.EntryType, nullString(t.IdempotencyKey), metadata,
		)
		if err != nil {
			return fmt.Errorf("ledger: insert entry for account %s: %w", leg.AccountID, err)
		}

		if err := applyBalance(ctx, tx, leg); err != nil {
			return err
		}
	}
	return nil
}

// applyBalance updates the cached running balance for a leg's account.
//
// The sign convention: asset-style accounts increase on debit. The single
// atomic UPDATE avoids the check-then-act race described in §22.1 — there is
// no SELECT to go stale between reading and writing, so two concurrent legs
// against the same account serialize on the row lock rather than clobbering
// each other.
func applyBalance(ctx context.Context, tx pgx.Tx, leg Leg) error {
	signed := leg.Amount.Cents
	if leg.Direction == DirectionCredit {
		signed = -signed
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO ledger_balances (account_id, balance_cents, currency, version, updated_at)
		VALUES ($1, $2, $3, 1, now())
		ON CONFLICT (account_id) DO UPDATE
		SET balance_cents = ledger_balances.balance_cents + EXCLUDED.balance_cents,
		    version       = ledger_balances.version + 1,
		    updated_at    = now()`,
		leg.AccountID, signed, leg.Amount.Currency,
	)
	if err != nil {
		return fmt.Errorf("ledger: update balance for account %s: %w", leg.AccountID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ledger: balance update affected no rows for account %s", leg.AccountID)
	}
	return nil
}

// Balance returns the cached balance for an account.
func Balance(ctx context.Context, q Querier, accountID uuid.UUID) (money.Amount, error) {
	var cents int64
	var currency string
	err := q.QueryRow(ctx,
		`SELECT balance_cents, currency FROM ledger_balances WHERE account_id = $1`,
		accountID,
	).Scan(&cents, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return money.Amount{}, ErrAccountNotFound
	}
	if err != nil {
		return money.Amount{}, fmt.Errorf("ledger: read balance: %w", err)
	}
	return money.Amount{Cents: cents, Currency: currency}, nil
}

// DerivedBalance recomputes an account's balance from ledger_entries, ignoring
// the cache. This is the authoritative figure; Balance is merely fast. The
// reconciliation job compares the two and alerts on any divergence (§24.3).
func DerivedBalance(ctx context.Context, q Querier, accountID uuid.UUID) (money.Amount, error) {
	var cents int64
	var currency string
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'debit' THEN amount_cents
		                         ELSE -amount_cents END), 0),
		       COALESCE(MIN(currency), '')
		FROM ledger_entries WHERE account_id = $1`,
		accountID,
	).Scan(&cents, &currency)
	if err != nil {
		return money.Amount{}, fmt.Errorf("ledger: derive balance: %w", err)
	}
	return money.Amount{Cents: cents, Currency: currency}, nil
}

// EnsureAccount looks up an account, creating it if absent, and returns its ID.
// merchantID may be uuid.Nil for platform-level accounts.
func EnsureAccount(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, accountType, currency string) (uuid.UUID, error) {
	var id uuid.UUID
	var merchantArg any
	query := `SELECT id FROM accounts WHERE account_type = $1 AND currency = $2 AND merchant_id `

	if merchantID == uuid.Nil {
		merchantArg = nil
		query += `IS NULL`
	} else {
		merchantArg = merchantID
		query += `= $3`
	}

	var err error
	if merchantID == uuid.Nil {
		err = tx.QueryRow(ctx, query, accountType, currency).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, query, accountType, currency, merchantArg).Scan(&id)
	}
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("ledger: lookup account: %w", err)
	}

	// Race-safe create: a concurrent caller may insert the same account between
	// our SELECT and INSERT, so fall back to re-reading on conflict.
	err = tx.QueryRow(ctx, `
		INSERT INTO accounts (merchant_id, account_type, currency)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		merchantArg, accountType, currency,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if merchantID == uuid.Nil {
			err = tx.QueryRow(ctx, query, accountType, currency).Scan(&id)
		} else {
			err = tx.QueryRow(ctx, query, accountType, currency, merchantArg).Scan(&id)
		}
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("ledger: create account: %w", err)
	}
	return id, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
