package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/idempotency"
	"github.com/likhith2366/paylo/internal/ledger"
	"github.com/likhith2366/paylo/internal/money"
)

// Refunds (§17).
//
// The whole difficulty here is one race. "Check the charge has room, then
// insert a refund" is check-then-act: two partial refunds arriving together
// both read the same total, both conclude there is room, and together they
// exceed the original charge. Idempotency keys don't help — these are two
// genuinely different requests.
//
// The fix is SELECT ... FOR UPDATE on the charge row, which serializes the
// check and the insert against each other (§22.1). Pending refunds count
// toward the committed total, so a refund in flight reserves its amount.

const (
	RefundStatusPending                = "pending"
	RefundStatusSucceeded              = "succeeded"
	RefundStatusFailed                 = "failed"
	RefundStatusRequiresReconciliation = "requires_reconciliation"
)

var (
	ErrChargeNotFound      = errors.New("payments: charge not found")
	ErrChargeNotRefundable = errors.New("payments: only a succeeded charge can be refunded")
	ErrRefundExceedsCharge = errors.New("payments: refund would exceed the remaining refundable amount")
)

type RefundInput struct {
	MerchantID uuid.UUID
	ChargeID   uuid.UUID
	// AmountCents of 0 means a full refund of whatever remains.
	AmountCents int64
	Reason      string
	Metadata    map[string]any

	SimulateOutcome string
}

type Refund struct {
	ID           uuid.UUID      `json:"id"`
	Object       string         `json:"object"`
	ChargeID     uuid.UUID      `json:"charge_id"`
	AmountCents  int64          `json:"amount"`
	Currency     string         `json:"currency"`
	Status       string         `json:"status"`
	Reason       string         `json:"reason,omitempty"`
	FailureCode  string         `json:"failure_code,omitempty"`
	LedgerTxnID  *uuid.UUID     `json:"ledger_transaction_id,omitempty"`
	ProcessorRef string         `json:"processor_reference,omitempty"`
	Metadata     map[string]any `json:"metadata"`
	CreatedAt    time.Time      `json:"created_at"`
}

// CreateRefund reverses all or part of a charge.
//
// Same shape as CreateCharge: claim the key first, reserve the amount under a
// row lock, call the processor outside any transaction, then finalize the
// ledger and idempotency record together.
func (s *Service) CreateRefund(ctx context.Context, in RefundInput, idemKey, requestHash string) (*Refund, int, error) {
	// The claim comes first, before any validation that a prior attempt could
	// have changed — the same rule the charge flow learned the hard way.
	var (
		idemRecordID uuid.UUID
		replay       *idempotency.Record
	)
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		outcome, rec, err := idempotency.Begin(ctx, tx, in.MerchantID, idemKey, "POST /v1/refunds", requestHash)
		if err != nil {
			return err
		}
		if outcome == idempotency.OutcomeReplay {
			replay = rec
			return nil
		}
		idemRecordID = rec.ID
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	if replay != nil {
		var prior Refund
		if len(replay.ResponseBody) > 0 {
			if err := json.Unmarshal(replay.ResponseBody, &prior); err != nil {
				return nil, 0, fmt.Errorf("payments: decode replayed refund: %w", err)
			}
		}
		status := replay.ResponseStatus
		if status == 0 {
			status = 200
		}
		return &prior, status, nil
	}

	// --- reserve the amount under a row lock ----------------------------------
	refundID := uuid.New()
	var (
		amount       money.Amount
		processorRef string
	)

	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// FOR UPDATE is what closes the race. Two concurrent refunds against
		// this charge serialize here; the second one sees the first's pending
		// row in the committed total.
		var chargeStatus, currency string
		var chargeAmount int64
		var chargeProcessorRef *string
		err := tx.QueryRow(ctx, `
			SELECT status, amount_cents, currency, processor_reference
			FROM charges
			WHERE id = $1 AND merchant_id = $2
			FOR UPDATE`,
			in.ChargeID, in.MerchantID,
		).Scan(&chargeStatus, &chargeAmount, &currency, &chargeProcessorRef)

		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChargeNotFound
		}
		if err != nil {
			return fmt.Errorf("payments: lock charge: %w", err)
		}
		if chargeStatus != StatusSucceeded {
			return fmt.Errorf("%w: charge is %s", ErrChargeNotRefundable, chargeStatus)
		}
		if chargeProcessorRef != nil {
			processorRef = *chargeProcessorRef
		}

		// Committed includes pending refunds, so an in-flight refund reserves
		// its amount against a concurrent one.
		var committed int64
		if err := tx.QueryRow(ctx,
			`SELECT committed_cents FROM charge_refund_totals WHERE charge_id = $1`,
			in.ChargeID,
		).Scan(&committed); err != nil {
			return fmt.Errorf("payments: read refund totals: %w", err)
		}

		remaining := chargeAmount - committed
		requested := in.AmountCents
		if requested == 0 {
			requested = remaining // full refund of what's left
		}
		if requested <= 0 || requested > remaining {
			return fmt.Errorf("%w: requested %d, %d remaining",
				ErrRefundExceedsCharge, requested, remaining)
		}

		amount = money.New(requested, currency)

		_, err = tx.Exec(ctx, `
			INSERT INTO refunds
				(id, charge_id, merchant_id, amount_cents, currency, status,
				 reason, idempotency_key, metadata)
			VALUES ($1,$2,$3,$4,$5,'pending',$6,$7,$8)`,
			refundID, in.ChargeID, in.MerchantID, requested, currency,
			nullIfEmpty(in.Reason), idemKey, orEmptyMap(in.Metadata),
		)
		if err != nil {
			return fmt.Errorf("payments: insert refund: %w", err)
		}
		return nil
	})
	if err != nil {
		// Only a genuine rejection of THIS request is terminal. A transient
		// database error says nothing about whether the refund is valid, and
		// burning it into a replayable failure would make a legitimate refund
		// permanently impossible under this key (same bug as the vault path
		// in CreateCharge).
		switch {
		case errors.Is(err, ErrChargeNotFound),
			errors.Is(err, ErrChargeNotRefundable),
			errors.Is(err, ErrRefundExceedsCharge):
			if ferr := s.failRefund(ctx, refundID, idemRecordID, in, err); ferr != nil {
				return nil, 0, ferr
			}
		default:
			if rerr := s.releaseClaim(ctx, idemRecordID); rerr != nil {
				slog.Error("payments: failed to release refund claim",
					"refund_id", refundID, "error", rerr)
			}
		}
		return nil, 0, err
	}

	// --- processor call, outside any transaction ------------------------------
	bankResp, bankErr := s.bank.Refund(ctx, BankRefundRequest{
		IdempotencyKey:     fmt.Sprintf("%s:refund:%s", in.MerchantID, idemKey),
		ProcessorReference: processorRef,
		AmountCents:        amount.Cents,
	}, in.SimulateOutcome)

	if bankErr != nil && errors.Is(bankErr, ErrAmbiguous) {
		slog.Warn("refund outcome ambiguous, deferring to reconciliation",
			"refund_id", refundID, "error", bankErr)
		if err := s.finalizeRefund(ctx, refundID, idemRecordID, RefundStatusRequiresReconciliation,
			"", "", nil, in, amount); err != nil {
			return nil, 0, err
		}
		return s.refundResponse(refundID, in, amount, RefundStatusRequiresReconciliation, "", nil, ""), 202, nil
	}
	if bankErr != nil {
		if err := s.finalizeRefund(ctx, refundID, idemRecordID, RefundStatusFailed,
			"processor_unreachable", "", nil, in, amount); err != nil {
			return nil, 0, err
		}
		return s.refundResponse(refundID, in, amount, RefundStatusFailed, "processor_unreachable", nil, ""), 502, nil
	}
	if bankResp.Status == "failed" {
		if err := s.finalizeRefund(ctx, refundID, idemRecordID, RefundStatusFailed,
			bankResp.FailureCode, "", nil, in, amount); err != nil {
			return nil, 0, err
		}
		return s.refundResponse(refundID, in, amount, RefundStatusFailed, bankResp.FailureCode, nil, ""), 402, nil
	}

	ledgerTxnID := uuid.New()
	if err := s.finalizeRefund(ctx, refundID, idemRecordID, RefundStatusSucceeded,
		"", bankResp.RefundReference, &ledgerTxnID, in, amount); err != nil {
		return nil, 0, err
	}
	return s.refundResponse(refundID, in, amount, RefundStatusSucceeded, "",
		&ledgerTxnID, bankResp.RefundReference), 200, nil
}

// finalizeRefund posts the reversal and completes the idempotency record in one
// transaction.
func (s *Service) finalizeRefund(
	ctx context.Context,
	refundID, idemRecordID uuid.UUID,
	status, failureCode, processorRef string,
	ledgerTxnID *uuid.UUID,
	in RefundInput,
	amount money.Amount,
) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if status == RefundStatusSucceeded && ledgerTxnID != nil {
			if err := s.postRefundLedger(ctx, tx, *ledgerTxnID, in.MerchantID, amount); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE refunds
			SET status = $2, failure_code = $3, processor_reference = $4,
			    ledger_transaction_id = $5, updated_at = now()
			WHERE id = $1`,
			refundID, status, nullIfEmpty(failureCode), nullIfEmpty(processorRef), ledgerTxnID,
		); err != nil {
			return fmt.Errorf("payments: update refund: %w", err)
		}

		response := s.refundResponse(refundID, in, amount, status, failureCode, ledgerTxnID, processorRef)
		body, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("payments: marshal refund response: %w", err)
		}

		eventType, httpStatus, idemStatus := "refund.succeeded", 200, idempotency.StatusCompleted
		switch status {
		case RefundStatusFailed:
			eventType, httpStatus, idemStatus = "refund.failed", 402, idempotency.StatusFailed
		case RefundStatusRequiresReconciliation:
			eventType, httpStatus = "refund.requires_reconciliation", 202
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (aggregate_id, event_type, payload)
			VALUES ($1, $2, $3)`,
			refundID, eventType, body,
		); err != nil {
			return fmt.Errorf("payments: insert refund outbox event: %w", err)
		}

		return idempotency.Complete(ctx, tx, idemRecordID, idemStatus, httpStatus, body)
	})
}

// postRefundLedger reverses the merchant's side of the original charge.
//
// Note what is NOT reversed: the platform fee. Real processors keep the
// processing fee on a refund, so the merchant bears the full refunded amount.
// Reversing the fee here would silently make refunds free for merchants and
// leave the platform short — an easy mistake with a real cost.
func (s *Service) postRefundLedger(ctx context.Context, tx pgx.Tx, txnID, merchantID uuid.UUID, amount money.Amount) error {
	merchantBalance, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountMerchantBalance, amount.Currency)
	if err != nil {
		return err
	}
	receivable, err := ledger.EnsureAccount(ctx, tx, merchantID, ledger.AccountCustomerReceivable, amount.Currency)
	if err != nil {
		return err
	}

	// The mirror image of the charge: the merchant gives the money back, and
	// the customer's receivable is reduced by the same amount.
	return ledger.Post(ctx, tx, ledger.Transaction{
		ID: txnID,
		Legs: []ledger.Leg{
			ledger.Debit(merchantBalance, amount, ledger.EntryRefund),
			ledger.Credit(receivable, amount, ledger.EntryRefund),
		},
	})
}

func (s *Service) failRefund(ctx context.Context, refundID, idemRecordID uuid.UUID, in RefundInput, cause error) error {
	code := "refund_rejected"
	switch {
	case errors.Is(cause, ErrChargeNotFound):
		code = "charge_not_found"
	case errors.Is(cause, ErrChargeNotRefundable):
		code = "charge_not_refundable"
	case errors.Is(cause, ErrRefundExceedsCharge):
		code = "refund_exceeds_charge"
	}

	body, err := json.Marshal(Refund{
		ID: refundID, Object: "refund", ChargeID: in.ChargeID,
		AmountCents: in.AmountCents, Status: RefundStatusFailed,
		FailureCode: code, Metadata: orEmptyMapGo(in.Metadata),
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("payments: marshal refund failure: %w", err)
	}

	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return idempotency.Complete(ctx, tx, idemRecordID, idempotency.StatusFailed, 400, body)
	})
}

func (s *Service) refundResponse(
	refundID uuid.UUID, in RefundInput, amount money.Amount,
	status, failureCode string, ledgerTxnID *uuid.UUID, processorRef string,
) *Refund {
	return &Refund{
		ID: refundID, Object: "refund", ChargeID: in.ChargeID,
		AmountCents: amount.Cents, Currency: amount.Currency,
		Status: status, Reason: in.Reason, FailureCode: failureCode,
		LedgerTxnID: ledgerTxnID, ProcessorRef: processorRef,
		Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
	}
}

// RefundedTotal reports how much of a charge has been refunded, derived from
// the refunds themselves rather than a stored counter (§17).
func (s *Service) RefundedTotal(ctx context.Context, chargeID uuid.UUID) (refunded, remaining int64, err error) {
	var chargeAmount, committed int64
	err = s.pool.QueryRow(ctx, `
		SELECT amount_cents, refunded_cents, committed_cents
		FROM charge_refund_totals WHERE charge_id = $1`,
		chargeID,
	).Scan(&chargeAmount, &refunded, &committed)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrChargeNotFound
	}
	if err != nil {
		return 0, 0, fmt.Errorf("payments: read refund totals: %w", err)
	}
	return refunded, chargeAmount - committed, nil
}
