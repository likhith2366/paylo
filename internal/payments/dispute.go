package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/ledger"
	"github.com/likhith2366/paylo/internal/money"
)

// Disputes / chargebacks (§15).
//
// A dispute is the cardholder's bank forcibly reversing a charge. Three things
// happen at once, and all three matter:
//
//  1. The funds move back immediately — before any evidence is reviewed. The
//     merchant is out the money while the dispute is open; that is how card
//     networks actually work, and modelling it any other way would overstate
//     the merchant's available balance.
//  2. A dispute fee is charged, which the merchant pays whether they win or
//     lose the underlying dispute.
//  3. The eventual outcome becomes a labelled training example for the fraud
//     model (§14.3) — disputes are literally where fraud labels come from.

const (
	DisputeStatusNeedsResponse = "needs_response"
	DisputeStatusUnderReview   = "under_review"
	DisputeStatusWon           = "won"
	DisputeStatusLost          = "lost"
)

// disputeFeeCents is charged on every dispute regardless of outcome — the
// network charges the acquirer, who passes it on. Refunded only if won.
const disputeFeeCents = 1500

var (
	ErrDisputeNotFound  = errors.New("payments: dispute not found")
	ErrDisputeNotOpen   = errors.New("payments: dispute is already resolved")
	ErrDisputeDuplicate = errors.New("payments: a dispute already exists for this reference")
)

type Dispute struct {
	ID              uuid.UUID  `json:"id"`
	Object          string     `json:"object"`
	ChargeID        uuid.UUID  `json:"charge_id"`
	AmountCents     int64      `json:"amount"`
	Currency        string     `json:"currency"`
	Reason          string     `json:"reason"`
	Status          string     `json:"status"`
	EvidenceDueBy   time.Time  `json:"evidence_due_by"`
	LedgerTxnID     *uuid.UUID `json:"ledger_transaction_id,omitempty"`
	ResolutionTxnID *uuid.UUID `json:"resolution_transaction_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type OpenDisputeInput struct {
	ChargeID           uuid.UUID
	Reason             string
	AmountCents        int64
	ProcessorReference string
	EvidenceDueBy      time.Time
}

// OpenDispute records an incoming chargeback and reverses the funds.
//
// This is driven by the processor, not by a merchant API call, so there is no
// idempotency key from a client. The processor's reference plays that role:
// it is UNIQUE, so a redelivered chargeback notification cannot reverse the
// funds twice.
func (s *Service) OpenDispute(ctx context.Context, in OpenDisputeInput) (*Dispute, error) {
	disputeID := uuid.New()
	ledgerTxnID := uuid.New()
	var out Dispute

	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var merchantID uuid.UUID
		var chargeAmount int64
		var currency, chargeStatus string
		err := tx.QueryRow(ctx, `
			SELECT merchant_id, amount_cents, currency, status
			FROM charges WHERE id = $1
			FOR UPDATE`,
			in.ChargeID,
		).Scan(&merchantID, &chargeAmount, &currency, &chargeStatus)

		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChargeNotFound
		}
		if err != nil {
			return fmt.Errorf("payments: lock charge for dispute: %w", err)
		}

		amountCents := in.AmountCents
		if amountCents == 0 {
			amountCents = chargeAmount
		}
		amount := money.New(amountCents, currency)

		dueBy := in.EvidenceDueBy
		if dueBy.IsZero() {
			dueBy = time.Now().UTC().Add(14 * 24 * time.Hour)
		}

		// The unique processor_reference is what makes a redelivered
		// notification safe. ON CONFLICT DO NOTHING plus a zero-row check
		// turns a duplicate into a clean error rather than a second reversal.
		tag, err := tx.Exec(ctx, `
			INSERT INTO disputes
				(id, charge_id, merchant_id, amount_cents, currency, reason,
				 status, evidence_due_by, ledger_transaction_id, processor_reference)
			VALUES ($1,$2,$3,$4,$5,$6,'needs_response',$7,$8,$9)
			ON CONFLICT (processor_reference) DO NOTHING`,
			disputeID, in.ChargeID, merchantID, amountCents, currency,
			in.Reason, dueBy, ledgerTxnID, nullIfEmpty(in.ProcessorReference),
		)
		if err != nil {
			return fmt.Errorf("payments: insert dispute: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrDisputeDuplicate
		}

		if err := s.postDisputeLedger(ctx, tx, ledgerTxnID, merchantID, amount); err != nil {
			return err
		}

		payload, err := json.Marshal(map[string]any{
			"id": disputeID, "charge_id": in.ChargeID, "amount": amountCents,
			"currency": currency, "reason": in.Reason,
			"status": DisputeStatusNeedsResponse, "evidence_due_by": dueBy,
		})
		if err != nil {
			return fmt.Errorf("payments: marshal dispute event: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (aggregate_id, event_type, payload)
			VALUES ($1, 'dispute.created', $2)`,
			disputeID, payload,
		); err != nil {
			return fmt.Errorf("payments: insert dispute outbox event: %w", err)
		}

		out = Dispute{
			ID: disputeID, Object: "dispute", ChargeID: in.ChargeID,
			AmountCents: amountCents, Currency: currency, Reason: in.Reason,
			Status: DisputeStatusNeedsResponse, EvidenceDueBy: dueBy,
			LedgerTxnID: &ledgerTxnID, CreatedAt: time.Now().UTC(),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// postDisputeLedger reverses the disputed funds and charges the dispute fee.
//
// Two balanced pairs in one transaction: the reversal itself, and the fee. The
// merchant's balance can go negative as a result, which is expected and
// explicitly allowed (§19) — a merchant with no balance still owes the money.
func (s *Service) postDisputeLedger(ctx context.Context, tx pgx.Tx, txnID, merchantID uuid.UUID, amount money.Amount) error {
	merchantBalance, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, amount.Currency)
	if err != nil {
		return err
	}
	receivable, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, amount.Currency)
	if err != nil {
		return err
	}
	platformFees, err := ledger.EnsureAccount(ctx, tx, uuid.Nil, ledger.AccountPlatformFees, amount.Currency)
	if err != nil {
		return err
	}

	fee := money.New(disputeFeeCents, amount.Currency)
	return ledger.Post(ctx, tx, ledger.Transaction{
		ID: txnID,
		Legs: []ledger.Leg{
			// The funds go back to the cardholder.
			ledger.Debit(merchantBalance, amount, ledger.EntryDispute),
			ledger.Credit(receivable, amount, ledger.EntryDispute),
			// And the merchant pays for the privilege.
			ledger.Debit(merchantBalance, fee, ledger.EntryFee),
			ledger.Credit(platformFees, fee, ledger.EntryFee),
		},
	})
}

// SubmitEvidence attaches the merchant's rebuttal and moves the dispute to
// under_review. Evidence documents themselves live in S3; only the reference
// is stored here (§15).
func (s *Service) SubmitEvidence(ctx context.Context, merchantID, disputeID uuid.UUID, evidence map[string]any) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE disputes
			SET evidence = $3, evidence_submitted_at = now(),
			    status = 'under_review', updated_at = now()
			WHERE id = $1 AND merchant_id = $2 AND status = 'needs_response'`,
			disputeID, merchantID, evidence,
		)
		if err != nil {
			return fmt.Errorf("payments: submit evidence: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Either it doesn't exist, or it's past the point of responding.
			return ErrDisputeNotOpen
		}
		return nil
	})
}

// ResolveDispute applies the network's final decision.
//
// Won: the reversal is itself reversed and the fee returned. Lost: the original
// reversal simply stands — no new ledger entries, because the money already
// moved when the dispute opened. Getting that asymmetry wrong would
// double-debit a merchant who loses.
func (s *Service) ResolveDispute(ctx context.Context, disputeID uuid.UUID, won bool) (*Dispute, error) {
	var out Dispute

	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var merchantID, chargeID uuid.UUID
		var amountCents int64
		var currency, status string
		err := tx.QueryRow(ctx, `
			SELECT merchant_id, charge_id, amount_cents, currency, status
			FROM disputes WHERE id = $1
			FOR UPDATE`,
			disputeID,
		).Scan(&merchantID, &chargeID, &amountCents, &currency, &status)

		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDisputeNotFound
		}
		if err != nil {
			return fmt.Errorf("payments: lock dispute: %w", err)
		}
		if status == DisputeStatusWon || status == DisputeStatusLost {
			return ErrDisputeNotOpen
		}

		amount := money.New(amountCents, currency)
		newStatus := DisputeStatusLost
		var resolutionTxnID *uuid.UUID

		if won {
			newStatus = DisputeStatusWon
			txnID := uuid.New()
			resolutionTxnID = &txnID
			if err := s.postDisputeWonLedger(ctx, tx, txnID, merchantID, amount); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE disputes
			SET status = $2, resolution_transaction_id = $3,
			    resolved_at = now(), updated_at = now()
			WHERE id = $1`,
			disputeID, newStatus, resolutionTxnID,
		); err != nil {
			return fmt.Errorf("payments: resolve dispute: %w", err)
		}

		// The outcome is a labelled training example: a lost 'fraudulent'
		// dispute is a confirmed fraud case for the model (§14.3, §15).
		payload, err := json.Marshal(map[string]any{
			"id": disputeID, "charge_id": chargeID, "status": newStatus,
			"amount": amountCents, "currency": currency,
		})
		if err != nil {
			return fmt.Errorf("payments: marshal dispute resolution: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (aggregate_id, event_type, payload)
			VALUES ($1, $2, $3)`,
			disputeID, "dispute."+newStatus, payload,
		); err != nil {
			return fmt.Errorf("payments: insert resolution outbox event: %w", err)
		}

		out = Dispute{
			ID: disputeID, Object: "dispute", ChargeID: chargeID,
			AmountCents: amountCents, Currency: currency, Status: newStatus,
			ResolutionTxnID: resolutionTxnID,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// postDisputeWonLedger returns the funds and the fee to the merchant.
func (s *Service) postDisputeWonLedger(ctx context.Context, tx pgx.Tx, txnID, merchantID uuid.UUID, amount money.Amount) error {
	merchantBalance, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, amount.Currency)
	if err != nil {
		return err
	}
	receivable, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, amount.Currency)
	if err != nil {
		return err
	}
	platformFees, err := ledger.EnsureAccount(ctx, tx, uuid.Nil, ledger.AccountPlatformFees, amount.Currency)
	if err != nil {
		return err
	}

	fee := money.New(disputeFeeCents, amount.Currency)
	return ledger.Post(ctx, tx, ledger.Transaction{
		ID: txnID,
		Legs: []ledger.Leg{
			ledger.Credit(merchantBalance, amount, ledger.EntryDisputeReversal),
			ledger.Debit(receivable, amount, ledger.EntryDisputeReversal),
			ledger.Credit(merchantBalance, fee, ledger.EntryDisputeReversal),
			ledger.Debit(platformFees, fee, ledger.EntryDisputeReversal),
		},
	})
}

// GetDispute fetches a dispute belonging to a merchant.
func (s *Service) GetDispute(ctx context.Context, merchantID, disputeID uuid.UUID) (*Dispute, error) {
	var d Dispute
	err := s.pool.QueryRow(ctx, `
		SELECT id, charge_id, amount_cents, currency, reason, status,
		       evidence_due_by, ledger_transaction_id, resolution_transaction_id, created_at
		FROM disputes WHERE id = $1 AND merchant_id = $2`,
		disputeID, merchantID,
	).Scan(&d.ID, &d.ChargeID, &d.AmountCents, &d.Currency, &d.Reason, &d.Status,
		&d.EvidenceDueBy, &d.LedgerTxnID, &d.ResolutionTxnID, &d.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDisputeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("payments: get dispute: %w", err)
	}
	d.Object = "dispute"
	return &d, nil
}
