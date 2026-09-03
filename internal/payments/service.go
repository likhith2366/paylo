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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/db"
	"github.com/likhith2366/paylo/internal/idempotency"
	"github.com/likhith2366/paylo/internal/ledger"
	"github.com/likhith2366/paylo/internal/money"
)

// Charge statuses.
const (
	StatusPending                = "pending"
	StatusRequiresAction         = "requires_action"
	StatusSucceeded              = "succeeded"
	StatusFailed                 = "failed"
	StatusRequiresReconciliation = "requires_reconciliation"
)

type Service struct {
	pool     *pgxpool.Pool
	bank     *BankClient
	hashSalt string
	// feeBps is the platform fee in basis points (100 bps = 1%).
	feeBps int64
}

func NewService(pool *pgxpool.Pool, bank *BankClient, hashSalt string) *Service {
	return &Service{pool: pool, bank: bank, hashSalt: hashSalt, feeBps: 290}
}

type ChargeInput struct {
	MerchantID   uuid.UUID
	AmountCents  int64
	Currency     string
	CardNumber   string
	CardExpMonth int
	CardExpYear  int
	CardCVC      string
	Description  string
	Metadata     map[string]any

	// Fraud signals captured at the edge (§14.5).
	DeviceFingerprint string
	IPAddress         string

	SimulateOutcome string // test-mode only, forwarded to the bank simulator
}

type Charge struct {
	ID               uuid.UUID      `json:"id"`
	Object           string         `json:"object"`
	AmountCents      int64          `json:"amount"`
	Currency         string         `json:"currency"`
	Status           string         `json:"status"`
	FailureCode      string         `json:"failure_code,omitempty"`
	FailureMessage   string         `json:"failure_message,omitempty"`
	CardLast4        string         `json:"card_last4,omitempty"`
	CardBrand        string         `json:"card_brand,omitempty"`
	LedgerTxnID      *uuid.UUID     `json:"ledger_transaction_id,omitempty"`
	ProcessorRef     string         `json:"processor_reference,omitempty"`
	Metadata         map[string]any `json:"metadata"`
	CreatedAt        time.Time      `json:"created_at"`
}

var (
	ErrInvalidAmount   = errors.New("payments: amount must be positive")
	ErrMerchantUnknown = errors.New("payments: merchant not found")
)

// CreateCharge runs the full charge flow.
//
// The sequencing here is the crux of the whole system, and it is deliberate:
//
//	tx1  claim the idempotency key, insert the charge as 'pending', commit
//	     — so a crash mid-flight leaves a durable record to reconcile against
//	     rather than a charge nobody knows happened
//	     ↓
//	     call the bank OUTSIDE any transaction
//	     — a network call inside a DB transaction holds row locks for the
//	       duration of someone else's latency; under load that alone exhausts
//	       the connection pool
//	     ↓
//	tx2  write the ledger entries, the outbox row, the charge status, and the
//	     idempotency response — all in ONE transaction, so money moving and the
//	     event announcing it can never diverge (§4.2 step 5, §22.1)
func (s *Service) CreateCharge(ctx context.Context, in ChargeInput, idemKey, requestHash string, rawBody []byte) (*Charge, int, error) {
	amount := money.New(in.AmountCents, in.Currency)
	if err := amount.Validate(); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrInvalidAmount, err)
	}

	network, err := ValidateNumber(in.CardNumber)
	if err != nil {
		return nil, 0, err
	}

	fingerprint := Fingerprint(in.CardNumber, s.hashSalt)
	chargeID := uuid.New()

	// --- tx1: claim the key and record intent ---------------------------------
	var (
		idemRecordID uuid.UUID
		replay       *idempotency.Record
	)
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		outcome, rec, err := idempotency.Begin(ctx, tx, in.MerchantID, idemKey, "POST /v1/charges", requestHash)
		if err != nil {
			return err
		}
		if outcome == idempotency.OutcomeReplay {
			replay = rec
			return nil
		}
		idemRecordID = rec.ID

		_, err = tx.Exec(ctx, `
			INSERT INTO charges
				(id, merchant_id, amount_cents, currency, status,
				 card_fingerprint, card_last4, card_brand, card_bin,
				 device_fingerprint, ip_address, idempotency_key, metadata)
			VALUES ($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (id) DO NOTHING`,
			chargeID, in.MerchantID, in.AmountCents, amount.Currency,
			fingerprint, Last4(in.CardNumber), string(network), BIN(in.CardNumber),
			nullIfEmpty(in.DeviceFingerprint), nullIfEmpty(in.IPAddress),
			idemKey, orEmptyMap(in.Metadata),
		)
		if err != nil {
			return fmt.Errorf("payments: insert charge: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// A completed key replays its stored response verbatim, without re-charging.
	if replay != nil {
		var prior Charge
		if len(replay.ResponseBody) > 0 {
			if err := json.Unmarshal(replay.ResponseBody, &prior); err != nil {
				return nil, 0, fmt.Errorf("payments: decode replayed response: %w", err)
			}
		}
		status := replay.ResponseStatus
		if status == 0 {
			status = 200
		}
		return &prior, status, nil
	}

	// --- bank call, outside any transaction -----------------------------------
	bankResp, bankErr := s.bank.Charge(ctx, BankChargeRequest{
		// Deriving the downstream key from ours makes the processor's own
		// idempotency cover our retries too (§24.1).
		IdempotencyKey: fmt.Sprintf("%s:%s", in.MerchantID, idemKey),
		AmountCents:    in.AmountCents,
		Currency:       amount.Currency,
		CardNumber:     in.CardNumber,
		CardExpMonth:   in.CardExpMonth,
		CardExpYear:    in.CardExpYear,
		CardCVC:        in.CardCVC,
	}, in.SimulateOutcome)

	// The ambiguous case: park the charge for reconciliation instead of
	// guessing. Deliberately NOT marked failed — the money may well have moved.
	if bankErr != nil && errors.Is(bankErr, ErrAmbiguous) {
		slog.Warn("charge outcome ambiguous, deferring to reconciliation",
			"charge_id", chargeID, "error", bankErr)

		if err := s.finalize(ctx, chargeID, idemRecordID, StatusRequiresReconciliation,
			"", "", "", nil, in, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusRequiresReconciliation,
			CardLast4: Last4(in.CardNumber), CardBrand: string(network),
			Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 202, nil
	}

	// Provably never landed — safe to fail cleanly and let the client retry.
	if bankErr != nil {
		if err := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
			"processor_unreachable", bankErr.Error(), "", nil, in, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusFailed,
			FailureCode: "processor_unreachable", FailureMessage: "The payment processor could not be reached.",
			CardLast4: Last4(in.CardNumber), CardBrand: string(network),
			Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 502, nil
	}

	if bankResp.Status == "declined" {
		if err := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
			bankResp.DeclineCode, bankResp.DeclineMessage, bankResp.ProcessorReference,
			nil, in, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusFailed,
			FailureCode: bankResp.DeclineCode, FailureMessage: bankResp.DeclineMessage,
			CardLast4: Last4(in.CardNumber), CardBrand: string(network),
			ProcessorRef: bankResp.ProcessorReference,
			Metadata:     orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 402, nil
	}

	// --- tx2: authorized. Post the ledger and finalize atomically -------------
	ledgerTxnID := uuid.New()
	if err := s.finalize(ctx, chargeID, idemRecordID, StatusSucceeded, "", "",
		bankResp.ProcessorReference, &ledgerTxnID, in, amount); err != nil {
		return nil, 0, err
	}

	return &Charge{
		ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
		Currency: amount.Currency, Status: StatusSucceeded,
		CardLast4: Last4(in.CardNumber), CardBrand: string(network),
		LedgerTxnID: &ledgerTxnID, ProcessorRef: bankResp.ProcessorReference,
		Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
	}, 200, nil
}

// finalize commits the terminal state of a charge in one transaction: ledger
// entries (when the charge succeeded), charge row, outbox event, and the
// idempotency response. Committing these separately is precisely the
// inconsistency the outbox pattern exists to prevent (§22.1).
func (s *Service) finalize(
	ctx context.Context,
	chargeID, idemRecordID uuid.UUID,
	status, failureCode, failureMessage, processorRef string,
	ledgerTxnID *uuid.UUID,
	in ChargeInput,
	amount money.Amount,
) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if status == StatusSucceeded && ledgerTxnID != nil {
			if err := s.postChargeLedger(ctx, tx, *ledgerTxnID, in, amount); err != nil {
				return err
			}
		}

		_, err := tx.Exec(ctx, `
			UPDATE charges
			SET status = $2, failure_code = $3, failure_message = $4,
			    processor_reference = $5, ledger_transaction_id = $6, updated_at = now()
			WHERE id = $1`,
			chargeID, status, nullIfEmpty(failureCode), nullIfEmpty(failureMessage),
			nullIfEmpty(processorRef), ledgerTxnID,
		)
		if err != nil {
			return fmt.Errorf("payments: update charge: %w", err)
		}

		response := Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: status,
			FailureCode: failureCode, FailureMessage: failureMessage,
			CardLast4: Last4(in.CardNumber), CardBrand: string(DetectNetwork(in.CardNumber)),
			LedgerTxnID: ledgerTxnID, ProcessorRef: processorRef,
			Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}
		body, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("payments: marshal charge response: %w", err)
		}

		eventType := "payment.succeeded"
		httpStatus := 200
		idemStatus := idempotency.StatusCompleted
		switch status {
		case StatusFailed:
			eventType, httpStatus, idemStatus = "payment.failed", 402, idempotency.StatusFailed
		case StatusRequiresReconciliation:
			eventType, httpStatus = "payment.requires_reconciliation", 202
		}

		// Same transaction as the ledger write — this is the outbox guarantee.
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (aggregate_id, event_type, payload)
			VALUES ($1, $2, $3)`,
			chargeID, eventType, body,
		); err != nil {
			return fmt.Errorf("payments: insert outbox event: %w", err)
		}

		return idempotency.Complete(ctx, tx, idemRecordID, idemStatus, httpStatus, body)
	})
}

// postChargeLedger writes the double-entry legs for a successful charge.
//
// Three legs, not two: the customer owes the money (debit receivable), the
// merchant is credited their net, and the platform is credited its fee. All
// three balance to zero, which is what the deferred constraint verifies at
// COMMIT.
func (s *Service) postChargeLedger(ctx context.Context, tx pgx.Tx, txnID uuid.UUID, in ChargeInput, amount money.Amount) error {
	fee := in.AmountCents * s.feeBps / 10_000
	net := in.AmountCents - fee

	receivable, err := ledger.EnsureAccount(ctx, tx, in.MerchantID, ledger.AccountCustomerReceivable, amount.Currency)
	if err != nil {
		return err
	}
	merchantBalance, err := ledger.EnsureAccount(ctx, tx, in.MerchantID, ledger.AccountMerchantBalance, amount.Currency)
	if err != nil {
		return err
	}
	platformFees, err := ledger.EnsureAccount(ctx, tx, uuid.Nil, ledger.AccountPlatformFees, amount.Currency)
	if err != nil {
		return err
	}

	legs := []ledger.Leg{
		ledger.Debit(receivable, money.New(in.AmountCents, amount.Currency), ledger.EntryCharge),
		ledger.Credit(merchantBalance, money.New(net, amount.Currency), ledger.EntryCharge),
	}
	// A zero fee would violate the amount_cents > 0 constraint, so the leg is
	// only written when there is actually a fee to record.
	if fee > 0 {
		legs = append(legs, ledger.Credit(platformFees, money.New(fee, amount.Currency), ledger.EntryFee))
	}

	return ledger.Post(ctx, tx, ledger.Transaction{ID: txnID, Legs: legs})
}

// GetCharge fetches a charge belonging to a merchant.
func (s *Service) GetCharge(ctx context.Context, merchantID, chargeID uuid.UUID) (*Charge, error) {
	var (
		c            Charge
		failCode     *string
		failMsg      *string
		last4, brand *string
		procRef      *string
		ledgerTxn    *uuid.UUID
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, amount_cents, currency, status, failure_code, failure_message,
		       card_last4, card_brand, processor_reference, ledger_transaction_id,
		       metadata, created_at
		FROM charges
		WHERE id = $1 AND merchant_id = $2`,
		chargeID, merchantID,
	).Scan(&c.ID, &c.AmountCents, &c.Currency, &c.Status, &failCode, &failMsg,
		&last4, &brand, &procRef, &ledgerTxn, &c.Metadata, &c.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("payments: get charge: %w", err)
	}

	c.Object = "charge"
	c.FailureCode = deref(failCode)
	c.FailureMessage = deref(failMsg)
	c.CardLast4 = deref(last4)
	c.CardBrand = deref(brand)
	c.ProcessorRef = deref(procRef)
	c.LedgerTxnID = ledgerTxn
	if c.Metadata == nil {
		c.Metadata = map[string]any{}
	}
	return &c, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orEmptyMap(m map[string]any) any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orEmptyMapGo(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
