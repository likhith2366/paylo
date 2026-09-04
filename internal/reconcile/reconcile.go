// Package reconcile resolves ambiguous charges and re-derives the ledger
// invariants (§24.3).
//
// Two responsibilities, deliberately in one job because they answer the same
// question — does our record of the money match reality?
//
//  1. Resolve anything parked as requires_reconciliation, by asking the
//     processor what actually happened. This is the other half of the
//     ambiguous-timeout contract: the charge path refuses to guess, so
//     something has to come along later and find out.
//
//  2. Independently recompute the ledger invariants. The write path enforces
//     them, and a deferred Postgres constraint enforces them again, but a
//     third check that shares no code with either is what catches the case
//     where both were wrong in the same way.
//
// THE JOB IS READ-ONLY WITH RESPECT TO DISCREPANCIES. It resolves ambiguity
// where the processor gives a definitive answer, and it RECORDS everything
// else for a human. Auto-correcting a financial mismatch is how a real
// accounting bug gets buried instead of found (§24.3).
//
// It is also idempotent and safe to re-run from scratch after a crash — it
// only ever reads, flags, and applies outcomes the processor has confirmed.
package reconcile

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
	"github.com/likhith2366/paylo/internal/payments"
)

// Lookup is the processor's transaction log. Narrowed to an interface so this
// package does not depend on the whole bank client, and so tests can drive
// each outcome.
type Lookup interface {
	Lookup(ctx context.Context, reference string) (*payments.BankChargeResponse, error)
}

type Service struct {
	pool *pgxpool.Pool
	bank Lookup
	// minAge keeps the job away from charges still legitimately in flight.
	// Without it a charge that timed out two seconds ago would be "resolved"
	// while its original request is still running.
	minAge time.Duration
	// feeBps must match the charge path, or a resolved charge posts different
	// ledger amounts than a normal one would have.
	feeBps int64
}

func NewService(pool *pgxpool.Pool, bank Lookup) *Service {
	return &Service{pool: pool, bank: bank, minAge: 5 * time.Minute, feeBps: 290}
}

type Result struct {
	RunID           uuid.UUID
	ChargesChecked  int
	ChargesResolved int
	Discrepancies   int
	UnbalancedTxns  int
	BalanceDrifts   int
}

// Run performs one reconciliation pass.
func (s *Service) Run(ctx context.Context) (*Result, error) {
	var runID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO reconciliation_runs DEFAULT VALUES RETURNING id`).Scan(&runID); err != nil {
		return nil, fmt.Errorf("reconcile: start run: %w", err)
	}
	result := &Result{RunID: runID}

	if err := s.resolveCharges(ctx, runID, result); err != nil {
		s.finishRun(ctx, runID, result, err)
		return result, err
	}
	if err := s.checkLedger(ctx, runID, result); err != nil {
		s.finishRun(ctx, runID, result, err)
		return result, err
	}

	s.finishRun(ctx, runID, result, nil)

	if result.Discrepancies > 0 {
		// P1, not a dashboard line. A discrepancy means money is unaccounted
		// for, which is a correctness problem rather than an uptime one (§12).
		slog.Error("reconcile: DISCREPANCIES FOUND — money may be unaccounted for",
			"run_id", runID, "count", result.Discrepancies,
			"unbalanced", result.UnbalancedTxns, "balance_drift", result.BalanceDrifts)
	}
	return result, nil
}

// resolveCharges asks the processor about every charge we could not confirm.
func (s *Service) resolveCharges(ctx context.Context, runID uuid.UUID, result *Result) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id, merchant_id, amount_cents, currency, processor_reference, idempotency_key
		FROM charges
		WHERE status = 'requires_reconciliation'
		  AND created_at < now() - $1::interval
		ORDER BY created_at
		LIMIT 500`,
		s.minAge.String(),
	)
	if err != nil {
		return fmt.Errorf("reconcile: load unresolved charges: %w", err)
	}

	type pending struct {
		id, merchantID uuid.UUID
		amountCents    int64
		currency       string
		processorRef   *string
		idemKey        *string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.merchantID, &p.amountCents, &p.currency,
			&p.processorRef, &p.idemKey); err != nil {
			rows.Close()
			return fmt.Errorf("reconcile: scan charge: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reconcile: read charges: %w", err)
	}

	for _, p := range batch {
		result.ChargesChecked++

		// The reference the charge path derived when calling the processor.
		// Present even when the response never arrived, which is the whole
		// point — it is the handle for asking after the fact.
		reference := ""
		if p.processorRef != nil {
			reference = *p.processorRef
		} else if p.idemKey != nil {
			reference = fmt.Sprintf("%s:%s", p.merchantID, *p.idemKey)
		}
		if reference == "" {
			s.record(ctx, runID, result, "orphaned_charge", "charge", p.id.String(),
				map[string]any{"reason": "no processor reference to query"})
			continue
		}

		resp, err := s.bank.Lookup(ctx, reference)
		if err != nil {
			// The processor is unreachable now. Leave the charge parked and
			// try again next run rather than recording a false discrepancy.
			slog.Warn("reconcile: processor lookup failed, will retry",
				"charge_id", p.id, "error", err)
			continue
		}

		if resp == nil {
			// Definitively absent from their log: the charge never landed.
			if err := s.settle(ctx, p.id, payments.StatusFailed, "", nil,
				p.merchantID, money.New(p.amountCents, p.currency)); err != nil {
				if errors.Is(err, errAlreadySettled) {
					continue
				}
				return err
			}
			result.ChargesResolved++
			slog.Info("reconcile: charge never reached the processor, marked failed",
				"charge_id", p.id)
			continue
		}

		switch resp.Status {
		case "authorized":
			// The money DID move. Post the ledger entries the charge path
			// could not, using the same accounts and fee split it would have.
			txnID := uuid.New()
			if err := s.settle(ctx, p.id, payments.StatusSucceeded, resp.ProcessorReference,
				&txnID, p.merchantID, money.New(p.amountCents, p.currency)); err != nil {
				// A concurrent run or a late response already settled it. Benign
				// — and the guard is what stopped a second set of ledger entries.
				if errors.Is(err, errAlreadySettled) {
					continue
				}
				return err
			}
			result.ChargesResolved++
			slog.Info("reconcile: ambiguous charge confirmed authorized, ledger posted",
				"charge_id", p.id, "ledger_transaction_id", txnID)

		case "declined":
			if err := s.settle(ctx, p.id, payments.StatusFailed, resp.ProcessorReference,
				nil, p.merchantID, money.New(p.amountCents, p.currency)); err != nil {
				if errors.Is(err, errAlreadySettled) {
					continue
				}
				return err
			}
			result.ChargesResolved++

		case "pending":
			// Still settling on their side. Leave it parked.

		default:
			s.record(ctx, runID, result, "processor_mismatch", "charge", p.id.String(),
				map[string]any{"processor_status": resp.Status, "reference": reference})
		}
	}
	return nil
}

// settle applies a resolved outcome: ledger entries when the money moved, the
// charge status, and an outbox event — all in one transaction, exactly as the
// charge path does it.
func (s *Service) settle(ctx context.Context, chargeID uuid.UUID, status, processorRef string,
	ledgerTxnID *uuid.UUID, merchantID uuid.UUID, amount money.Amount) error {

	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if ledgerTxnID != nil {
			if err := s.postChargeLedger(ctx, tx, *ledgerTxnID, merchantID, amount); err != nil {
				return err
			}
		}

		// Guarded on the current status. If a concurrent run or a late
		// response already settled this charge, the update affects no rows and
		// we must not post a second set of ledger entries.
		tag, err := tx.Exec(ctx, `
			UPDATE charges
			SET status = $2, processor_reference = COALESCE(NULLIF($3,''), processor_reference),
			    ledger_transaction_id = COALESCE($4, ledger_transaction_id), updated_at = now()
			WHERE id = $1 AND status = 'requires_reconciliation'`,
			chargeID, status, processorRef, ledgerTxnID,
		)
		if err != nil {
			return fmt.Errorf("reconcile: settle charge: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return errAlreadySettled
		}

		payload, err := json.Marshal(map[string]any{
			"id": chargeID, "status": status, "amount": amount.Cents,
			"currency": amount.Currency, "resolved_by": "reconciliation",
		})
		if err != nil {
			return fmt.Errorf("reconcile: marshal event: %w", err)
		}
		eventType := "payment.succeeded"
		if status == payments.StatusFailed {
			eventType = "payment.failed"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (aggregate_id, event_type, payload)
			VALUES ($1, $2, $3)`, chargeID, eventType, payload,
		); err != nil {
			return fmt.Errorf("reconcile: insert outbox event: %w", err)
		}
		return nil
	})
}

var errAlreadySettled = errors.New("reconcile: charge was already settled")

// postChargeLedger mirrors the charge path's split exactly. A resolved charge
// must produce the same entries a normal one would have.
func (s *Service) postChargeLedger(ctx context.Context, tx pgx.Tx, txnID, merchantID uuid.UUID, amount money.Amount) error {
	fee := amount.Cents * s.feeBps / 10_000
	net := amount.Cents - fee

	receivable, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, amount.Currency)
	if err != nil {
		return err
	}
	balance, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, amount.Currency)
	if err != nil {
		return err
	}
	platformFees, err := ledger.EnsureAccount(ctx, tx, uuid.Nil, ledger.AccountPlatformFees, amount.Currency)
	if err != nil {
		return err
	}

	legs := []ledger.Leg{
		ledger.Debit(receivable, money.New(amount.Cents, amount.Currency), ledger.EntryCharge),
		ledger.Credit(balance, money.New(net, amount.Currency), ledger.EntryCharge),
	}
	if fee > 0 {
		legs = append(legs, ledger.Credit(platformFees, money.New(fee, amount.Currency), ledger.EntryFee))
	}
	return ledger.Post(ctx, tx, ledger.Transaction{ID: txnID, Legs: legs})
}

// checkLedger re-derives the invariants from the entries themselves.
//
// Deliberately shares no code with the write path or the database trigger. A
// check that reuses the logic it is verifying only proves that logic is
// self-consistent, not that it is right.
func (s *Service) checkLedger(ctx context.Context, runID uuid.UUID, result *Result) error {
	// Debits must equal credits, per transaction, per currency (§5).
	rows, err := s.pool.Query(ctx, `
		SELECT transaction_id, currency,
		       SUM(CASE WHEN direction = 'debit' THEN amount_cents ELSE -amount_cents END)
		FROM ledger_entries
		GROUP BY transaction_id, currency
		HAVING SUM(CASE WHEN direction = 'debit' THEN amount_cents ELSE -amount_cents END) <> 0`)
	if err != nil {
		return fmt.Errorf("reconcile: balance check: %w", err)
	}
	for rows.Next() {
		var txnID uuid.UUID
		var currency string
		var delta int64
		if err := rows.Scan(&txnID, &currency, &delta); err != nil {
			rows.Close()
			return fmt.Errorf("reconcile: scan imbalance: %w", err)
		}
		result.UnbalancedTxns++
		s.record(ctx, runID, result, "unbalanced_transaction", "ledger_transaction",
			txnID.String(), map[string]any{"currency": currency, "delta_cents": delta})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reconcile: read imbalances: %w", err)
	}

	// The cached balance is the one mutable financial value in the system, so
	// it is the one that can drift. Compare it against the entries.
	driftRows, err := s.pool.Query(ctx, `
		SELECT b.account_id, b.balance_cents, COALESCE(e.derived, 0)
		FROM ledger_balances b
		LEFT JOIN (
			SELECT account_id,
			       SUM(CASE WHEN direction = 'debit' THEN amount_cents ELSE -amount_cents END) AS derived
			FROM ledger_entries GROUP BY account_id
		) e ON e.account_id = b.account_id
		WHERE b.balance_cents <> COALESCE(e.derived, 0)`)
	if err != nil {
		return fmt.Errorf("reconcile: drift check: %w", err)
	}
	defer driftRows.Close()

	for driftRows.Next() {
		var accountID uuid.UUID
		var cached, derived int64
		if err := driftRows.Scan(&accountID, &cached, &derived); err != nil {
			return fmt.Errorf("reconcile: scan drift: %w", err)
		}
		result.BalanceDrifts++
		s.record(ctx, runID, result, "balance_drift", "account", accountID.String(),
			map[string]any{"cached": cached, "derived": derived, "delta": cached - derived})
	}
	return driftRows.Err()
}

// record writes a discrepancy for a human. It never fixes anything.
func (s *Service) record(ctx context.Context, runID uuid.UUID, result *Result,
	kind, subjectType, subjectID string, detail map[string]any) {

	result.Discrepancies++
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO reconciliation_discrepancies
			(run_id, kind, subject_type, subject_id, detail)
		VALUES ($1,$2,$3,$4,$5)`,
		runID, kind, subjectType, subjectID, detail,
	); err != nil {
		slog.Error("reconcile: failed to record discrepancy",
			"kind", kind, "subject", subjectID, "error", err)
	}
}

func (s *Service) finishRun(ctx context.Context, runID uuid.UUID, r *Result, runErr error) {
	var errText any
	if runErr != nil {
		errText = runErr.Error()
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE reconciliation_runs
		SET finished_at = now(), charges_checked = $2, charges_resolved = $3,
		    discrepancies = $4, error = $5
		WHERE id = $1`,
		runID, r.ChargesChecked, r.ChargesResolved, r.Discrepancies, errText,
	); err != nil {
		slog.Error("reconcile: failed to close run", "run_id", runID, "error", err)
	}
}
