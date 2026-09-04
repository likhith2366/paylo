// Package payouts moves money out to merchants (§18).
//
// Everything else in this system models money coming in. This is the half that
// closes the loop.
//
// Three things make a payout different from a charge, and each shapes the code:
//
//	IT IS A BATCH, NOT A REQUEST. Nobody is waiting on an HTTP response, so
//	there is no client idempotency key. The (merchant, currency, period_end)
//	unique constraint plays that role — a re-run of the same window conflicts
//	instead of paying twice.
//
//	THE MONEY IS NOT GONE WHEN WE SAY SO. An ACH transfer can fail days later
//	on a bad routing number. So a payout posts to an `in_transit` account
//	first and only reaches `paid_out` when the bank confirms, and a failure
//	returns the funds to the merchant's balance rather than losing them.
//
//	THE BALANCE IS NOT THE PAYABLE AMOUNT. Funds must clear before they can be
//	paid out (T+2), and money reserved against open disputes is not the
//	merchant's to take. Paying out the raw ledger balance would hand over money
//	that is about to be clawed back.
package payouts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/ledger"
	"github.com/likhith2366/paylo/internal/money"
)

const (
	StatusPending                = "pending"
	StatusPaid                   = "paid"
	StatusFailed                 = "failed"
	StatusRequiresReconciliation = "requires_reconciliation"
)

var (
	ErrNoPayoutAccount = errors.New("payouts: merchant has no payout account")
	ErrNothingToPay    = errors.New("payouts: no available balance")
	ErrAlreadyPaid     = errors.New("payouts: a payout already exists for this period")
)

// Transfer is the bank's ACH interface.
type Transfer interface {
	Transfer(ctx context.Context, req TransferRequest) (*TransferResponse, error)
}

type TransferRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	AccountToken   string `json:"account_token"`
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency"`
}

type TransferResponse struct {
	Reference   string `json:"payout_reference"`
	Status      string `json:"status"` // accepted | rejected
	FailureCode string `json:"failure_code,omitempty"`
}

type Service struct {
	pool *pgxpool.Pool
	bank Transfer
	// holdPeriod is how long funds must settle before they can be paid out.
	// T+2 is the industry standard, and it is also a fraud control: a dispute
	// usually surfaces within days, so paying out instantly means paying out
	// money that is about to be clawed back (§18, §19).
	holdPeriod time.Duration
	// minPayoutCents avoids ACH fees exceeding the transfer.
	minPayoutCents int64
}

func NewService(pool *pgxpool.Pool, bank Transfer) *Service {
	return &Service{
		pool:           pool,
		bank:           bank,
		holdPeriod:     48 * time.Hour,
		minPayoutCents: 100,
	}
}

type Payout struct {
	ID          uuid.UUID  `json:"id"`
	Object      string     `json:"object"`
	MerchantID  uuid.UUID  `json:"merchant_id"`
	AmountCents int64      `json:"amount"`
	Currency    string     `json:"currency"`
	Status      string     `json:"status"`
	FailureCode string     `json:"failure_code,omitempty"`
	LedgerTxnID *uuid.UUID `json:"ledger_transaction_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type RunResult struct {
	MerchantsConsidered int
	PayoutsCreated      int
	TotalCents          int64
	Skipped             int
}

// Run creates payouts for every merchant with a payable balance.
func (s *Service) Run(ctx context.Context) (*RunResult, error) {
	// The window closes at the hold boundary. Deriving it once and reusing it
	// for every merchant makes the batch deterministic: a re-run at a
	// different wall-clock time still produces the same period_end and so
	// conflicts with itself rather than creating a second payout.
	periodEnd := time.Now().UTC().Add(-s.holdPeriod).Truncate(time.Hour)
	result := &RunResult{}

	rows, err := s.pool.Query(ctx, `
		SELECT pa.merchant_id, pa.id, pa.currency, pa.account_token
		FROM payout_accounts pa
		WHERE pa.verified_at IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("payouts: load payout accounts: %w", err)
	}

	type account struct {
		merchantID, accountID uuid.UUID
		currency, token       string
	}
	var accounts []account
	for rows.Next() {
		var a account
		if err := rows.Scan(&a.merchantID, &a.accountID, &a.currency, &a.token); err != nil {
			rows.Close()
			return nil, fmt.Errorf("payouts: scan account: %w", err)
		}
		accounts = append(accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payouts: read accounts: %w", err)
	}

	for _, a := range accounts {
		result.MerchantsConsidered++

		payout, err := s.payMerchant(ctx, a.merchantID, a.accountID, a.currency, a.token, periodEnd)
		switch {
		case errors.Is(err, ErrNothingToPay), errors.Is(err, ErrAlreadyPaid):
			result.Skipped++
		case err != nil:
			// One merchant's failure must not abort the batch — the others are
			// owed their money regardless.
			slog.Error("payouts: merchant payout failed",
				"merchant_id", a.merchantID, "error", err)
			result.Skipped++
		default:
			result.PayoutsCreated++
			result.TotalCents += payout.AmountCents
		}
	}
	return result, nil
}

// payMerchant computes the payable amount and moves it.
func (s *Service) payMerchant(ctx context.Context, merchantID, accountID uuid.UUID,
	currency, accountToken string, periodEnd time.Time) (*Payout, error) {

	payoutID := uuid.New()
	ledgerTxnID := uuid.New()
	var amount money.Amount

	// --- tx1: reserve the funds and record intent ----------------------------
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		available, err := s.availableCents(ctx, tx, merchantID, currency, periodEnd)
		if err != nil {
			return err
		}
		if available < s.minPayoutCents {
			return ErrNothingToPay
		}
		amount = money.New(available, currency)

		// The unique constraint is the idempotency mechanism. A crashed batch
		// re-run over the same window conflicts here rather than paying twice.
		tag, err := tx.Exec(ctx, `
			INSERT INTO payouts
				(id, merchant_id, payout_account_id, amount_cents, currency,
				 status, period_end, ledger_transaction_id)
			VALUES ($1,$2,$3,$4,$5,'pending',$6,$7)
			ON CONFLICT (merchant_id, currency, period_end) DO NOTHING`,
			payoutID, merchantID, accountID, available, currency, periodEnd, ledgerTxnID,
		)
		if err != nil {
			return fmt.Errorf("payouts: insert payout: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyPaid
		}

		// The funds leave the merchant's balance now, before the transfer is
		// attempted. Otherwise a second run moments later would see the same
		// balance as still available and pay it out again.
		return s.postPayoutLedger(ctx, tx, ledgerTxnID, merchantID, amount)
	})
	if err != nil {
		return nil, err
	}

	// --- ACH, outside any transaction ----------------------------------------
	resp, bankErr := s.bank.Transfer(ctx, TransferRequest{
		IdempotencyKey: payoutID.String(),
		AccountToken:   accountToken,
		AmountCents:    amount.Cents,
		Currency:       amount.Currency,
	})

	if bankErr != nil {
		// Ambiguous: the transfer may or may not be underway. The funds stay
		// in in_transit and reconciliation resolves it — the same rule as
		// charges. Returning them now could pay the merchant twice.
		slog.Warn("payouts: transfer outcome ambiguous, deferring to reconciliation",
			"payout_id", payoutID, "error", bankErr)
		if err := s.setStatus(ctx, payoutID, StatusRequiresReconciliation, ""); err != nil {
			return nil, err
		}
		return &Payout{ID: payoutID, Object: "payout", MerchantID: merchantID,
			AmountCents: amount.Cents, Currency: amount.Currency,
			Status: StatusRequiresReconciliation, LedgerTxnID: &ledgerTxnID}, nil
	}

	if resp.Status == "rejected" {
		// Definitively refused, so the money never left. Return it.
		if err := s.reverse(ctx, payoutID, merchantID, amount, resp.FailureCode); err != nil {
			return nil, err
		}
		return &Payout{ID: payoutID, Object: "payout", MerchantID: merchantID,
			AmountCents: amount.Cents, Currency: amount.Currency,
			Status: StatusFailed, FailureCode: resp.FailureCode}, nil
	}

	if err := s.setStatus(ctx, payoutID, StatusPending, resp.Reference); err != nil {
		return nil, err
	}
	return &Payout{ID: payoutID, Object: "payout", MerchantID: merchantID,
		AmountCents: amount.Cents, Currency: amount.Currency,
		Status: StatusPending, LedgerTxnID: &ledgerTxnID, CreatedAt: time.Now().UTC()}, nil
}

// availableCents is the payable amount, which is NOT the ledger balance.
//
// Three deductions, each for a different reason:
//   - only charges that have cleared the hold period count (T+2)
//   - refunds and disputes already reduced the balance, and are reflected
//   - money reserved against OPEN disputes is not the merchant's to take —
//     it is about to be clawed back (§19)
func (s *Service) availableCents(ctx context.Context, tx pgx.Tx,
	merchantID uuid.UUID, currency string, periodEnd time.Time) (int64, error) {

	var settled int64
	// The merchant_balance account is credit-normal, so a negative cached
	// balance means the merchant is owed money. Negate it to get the amount.
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(-SUM(CASE WHEN e.direction = 'debit' THEN e.amount_cents
		                          ELSE -e.amount_cents END), 0)
		FROM ledger_entries e
		JOIN accounts a ON a.id = e.account_id
		WHERE a.merchant_id = $1
		  AND a.account_type = 'merchant_balance'
		  AND a.currency = $2
		  AND e.created_at <= $3`,
		merchantID, currency, periodEnd,
	).Scan(&settled)
	if err != nil {
		return 0, fmt.Errorf("payouts: compute settled balance: %w", err)
	}

	// Reserve the full amount of every dispute still open. Losing one after
	// the money has been paid out leaves the merchant's balance negative and
	// the platform chasing it (§19).
	var reserved int64
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_cents), 0)
		FROM disputes
		WHERE merchant_id = $1 AND currency = $2
		  AND status IN ('needs_response', 'under_review')`,
		merchantID, currency,
	).Scan(&reserved)
	if err != nil {
		return 0, fmt.Errorf("payouts: compute dispute reserve: %w", err)
	}

	available := settled - reserved
	if available < 0 {
		// The merchant owes more than they hold. Not a payout — §19's
		// negative-balance case, which needs collection rather than transfer.
		return 0, nil
	}
	return available, nil
}

// postPayoutLedger moves the funds from the merchant's balance into in_transit.
//
// Not straight to paid_out: the money is genuinely in neither place until the
// bank confirms, and an ACH can fail days later. in_transit is what makes that
// failure recoverable rather than a hole in the books.
func (s *Service) postPayoutLedger(ctx context.Context, tx pgx.Tx, txnID, merchantID uuid.UUID, amount money.Amount) error {
	balance, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, amount.Currency)
	if err != nil {
		return err
	}
	inTransit, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountInTransit, amount.Currency)
	if err != nil {
		return err
	}
	return ledger.Post(ctx, tx, ledger.Transaction{
		ID: txnID,
		Legs: []ledger.Leg{
			ledger.Debit(balance, amount, ledger.EntryPayout),
			ledger.Credit(inTransit, amount, ledger.EntryPayout),
		},
	})
}

// Confirm records that the bank settled a payout: in_transit -> paid_out.
func (s *Service) Confirm(ctx context.Context, payoutID uuid.UUID) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var merchantID uuid.UUID
		var amountCents int64
		var currency string
		err := tx.QueryRow(ctx, `
			SELECT merchant_id, amount_cents, currency FROM payouts
			WHERE id = $1 AND status IN ('pending', 'requires_reconciliation')
			FOR UPDATE`,
			payoutID,
		).Scan(&merchantID, &amountCents, &currency)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("payouts: payout %s is not awaiting settlement", payoutID)
		}
		if err != nil {
			return fmt.Errorf("payouts: lock payout: %w", err)
		}

		amount := money.New(amountCents, currency)
		txnID := uuid.New()

		inTransit, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountInTransit, currency)
		if err != nil {
			return err
		}
		paidOut, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountPaidOut, currency)
		if err != nil {
			return err
		}
		if err := ledger.Post(ctx, tx, ledger.Transaction{
			ID: txnID,
			Legs: []ledger.Leg{
				ledger.Debit(inTransit, amount, ledger.EntryPayout),
				ledger.Credit(paidOut, amount, ledger.EntryPayout),
			},
		}); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE payouts SET status = 'paid', paid_at = now(),
			       settlement_transaction_id = $2, updated_at = now()
			WHERE id = $1`, payoutID, txnID,
		); err != nil {
			return fmt.Errorf("payouts: mark paid: %w", err)
		}
		return s.emit(ctx, tx, payoutID, "payout.paid", amount)
	})
}

// Fail returns the funds after the bank rejects a transfer, possibly days
// later (§18). The reversal is a new balanced pair, never an edit.
func (s *Service) Fail(ctx context.Context, payoutID uuid.UUID, failureCode string) error {
	var merchantID uuid.UUID
	var amountCents int64
	var currency string

	err := s.pool.QueryRow(ctx, `
		SELECT merchant_id, amount_cents, currency FROM payouts
		WHERE id = $1 AND status IN ('pending', 'requires_reconciliation')`,
		payoutID,
	).Scan(&merchantID, &amountCents, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("payouts: payout %s is not awaiting settlement", payoutID)
	}
	if err != nil {
		return fmt.Errorf("payouts: load payout: %w", err)
	}

	return s.reverse(ctx, payoutID, merchantID, money.New(amountCents, currency), failureCode)
}

// reverse returns in_transit funds to the merchant's balance.
func (s *Service) reverse(ctx context.Context, payoutID, merchantID uuid.UUID,
	amount money.Amount, failureCode string) error {

	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Guarded on status so a concurrent Fail and Confirm cannot both post.
		tag, err := tx.Exec(ctx, `
			UPDATE payouts SET status = 'failed', failure_code = $2, updated_at = now()
			WHERE id = $1 AND status IN ('pending', 'requires_reconciliation')`,
			payoutID, failureCode,
		)
		if err != nil {
			return fmt.Errorf("payouts: mark failed: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // already resolved by someone else
		}

		inTransit, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountInTransit, amount.Currency)
		if err != nil {
			return err
		}
		balance, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, amount.Currency)
		if err != nil {
			return err
		}
		if err := ledger.Post(ctx, tx, ledger.Transaction{
			ID: uuid.New(),
			Legs: []ledger.Leg{
				ledger.Debit(inTransit, amount, ledger.EntryPayout),
				ledger.Credit(balance, amount, ledger.EntryPayout),
			},
		}); err != nil {
			return err
		}
		return s.emit(ctx, tx, payoutID, "payout.failed", amount)
	})
}

func (s *Service) setStatus(ctx context.Context, payoutID uuid.UUID, status, reference string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE payouts SET status = $2,
		       processor_reference = COALESCE(NULLIF($3,''), processor_reference),
		       updated_at = now()
		WHERE id = $1`, payoutID, status, reference)
	if err != nil {
		return fmt.Errorf("payouts: set status: %w", err)
	}
	return nil
}

func (s *Service) emit(ctx context.Context, tx pgx.Tx, payoutID uuid.UUID, eventType string, amount money.Amount) error {
	payload, err := json.Marshal(map[string]any{
		"id": payoutID, "amount": amount.Cents, "currency": amount.Currency,
	})
	if err != nil {
		return fmt.Errorf("payouts: marshal event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (aggregate_id, event_type, payload)
		VALUES ($1, $2, $3)`, payoutID, eventType, payload,
	); err != nil {
		return fmt.Errorf("payouts: insert outbox event: %w", err)
	}
	return nil
}
