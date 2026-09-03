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
	"github.com/likhith2366/paylo/internal/risk"
)

// Charge statuses.
const (
	StatusPending                = "pending"
	StatusRequiresAction         = "requires_action"
	StatusSucceeded              = "succeeded"
	StatusFailed                 = "failed"
	StatusRequiresReconciliation = "requires_reconciliation"
)

// VaultClient is the payments service's view of the vault (§2.4).
//
// Narrowed to an interface here rather than taking *vault.Client directly, so
// this package has no dependency on the vault package and tests can supply a
// stand-in. It is also a standing reminder of the boundary: these two methods
// are the only way card data enters the charge flow.
type VaultClient interface {
	// Metadata returns brand, last4, BIN and fingerprint — never a PAN.
	Metadata(ctx context.Context, token string) (*CardMetadata, error)
	// Detokenize returns a raw PAN for immediate submission to the processor.
	// The result must never be stored, logged, or returned in an error.
	Detokenize(ctx context.Context, token, caller, reason string) (string, error)
}

// CardMetadata is everything the payments service is permitted to know about a
// card. Note the absence of a number field — the type makes exposure
// structurally impossible, not merely discouraged.
type CardMetadata struct {
	Brand       string `json:"brand"`
	Last4       string `json:"last4"`
	BIN         string `json:"bin"`
	Fingerprint string `json:"fingerprint"`
	ExpMonth    int    `json:"exp_month"`
	ExpYear     int    `json:"exp_year"`
}

// RiskAssessor scores a charge before it reaches the processor (§14.1).
//
// An interface so this package does not depend on the risk package, and so
// tests can drive specific verdicts. A nil assessor means risk checking is
// disabled entirely — valid for tests, never for production.
type RiskAssessor interface {
	Assess(ctx context.Context, txn risk.Transaction) risk.Decision
	RecordOutcome(ctx context.Context, txn risk.Transaction, declined bool)
}

type Service struct {
	pool  *pgxpool.Pool
	bank  *BankClient
	vault VaultClient
	risk  RiskAssessor
	// feeBps is the platform fee in basis points (100 bps = 1%).
	feeBps int64
}

func NewService(pool *pgxpool.Pool, bank *BankClient, vaultClient VaultClient, assessor RiskAssessor) *Service {
	return &Service{pool: pool, bank: bank, vault: vaultClient, risk: assessor, feeBps: 290}
}

type ChargeInput struct {
	MerchantID  uuid.UUID
	AmountCents int64
	Currency    string
	// PaymentToken references a card held in the vault. The payments service
	// never accepts a raw card number — that is the whole point of §2.4.
	PaymentToken string
	Description  string
	Metadata     map[string]any

	// Fraud signals captured at the edge (§14.5).
	DeviceFingerprint string
	IPAddress         string

	SimulateOutcome string // test-mode only, forwarded to the bank simulator
}

type Charge struct {
	ID             uuid.UUID      `json:"id"`
	Object         string         `json:"object"`
	AmountCents    int64          `json:"amount"`
	Currency       string         `json:"currency"`
	Status         string         `json:"status"`
	FailureCode    string         `json:"failure_code,omitempty"`
	FailureMessage string         `json:"failure_message,omitempty"`
	CardLast4      string         `json:"card_last4,omitempty"`
	CardBrand      string         `json:"card_brand,omitempty"`
	LedgerTxnID    *uuid.UUID     `json:"ledger_transaction_id,omitempty"`
	ProcessorRef   string         `json:"processor_reference,omitempty"`
	Metadata       map[string]any `json:"metadata"`
	CreatedAt      time.Time      `json:"created_at"`
}

var (
	ErrInvalidAmount   = errors.New("payments: amount must be positive")
	ErrMerchantUnknown = errors.New("payments: merchant not found")
	ErrMissingToken    = errors.New("payments: a payment token is required")

	// ErrTokenUnusable covers a token that is missing, expired, or already
	// consumed. They are deliberately collapsed into one error so the API
	// cannot be used to distinguish between them — telling a caller that a
	// token exists but was already spent leaks more than it helps.
	ErrTokenUnusable = errors.New("payments: payment token is unusable")
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

	if in.PaymentToken == "" {
		return nil, 0, ErrMissingToken
	}

	chargeID := uuid.New()

	// --- tx1: claim the idempotency key, and nothing else ---------------------
	//
	// This MUST be the first thing that happens, before any validation or
	// lookup that could fail. The reason is subtle and was a real bug here: a
	// successful charge consumes its single-use vault token, so on a retry the
	// token is legitimately gone. If the token were checked first, the retry
	// would fail with "invalid token" instead of replaying the original
	// response — the exact double-charge-adjacent confusion idempotency exists
	// to prevent.
	//
	// The general rule: a retry must never re-execute a precondition that the
	// first attempt may have changed.
	var (
		idemRecordID uuid.UUID
		replay       *idempotency.Record
	)
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		outcome, rec, err := idempotency.Begin(ctx, tx, in.MerchantID, idemKey, "POST /v1/charges", requestHash)
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

	// A completed key replays its stored response verbatim, without re-charging
	// and without touching the vault at all.
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

	// --- card metadata, outside any transaction -------------------------------
	//
	// Brand, last4, BIN, fingerprint — never a PAN. A network call, so it is
	// deliberately not inside a transaction: holding row locks for the duration
	// of someone else's latency exhausts the connection pool under load.
	card, err := s.vault.Metadata(ctx, in.PaymentToken)
	if err != nil {
		// The key is claimed but the token is unusable. Record the failure so
		// the key reaches a terminal state and a retry replays this same error
		// rather than hanging as 'processing' until the lock goes stale.
		if ferr := s.failClaimed(ctx, chargeID, idemRecordID, in, amount,
			"invalid_payment_token", "The payment token is invalid, expired, or has already been used."); ferr != nil {
			return nil, 0, ferr
		}
		return nil, 0, err
	}

	// --- tx2: record intent ---------------------------------------------------
	// A durable record before anything leaves this system, so a crash mid-flight
	// leaves something for reconciliation rather than a charge nobody knows about.
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO charges
				(id, merchant_id, amount_cents, currency, status,
				 card_fingerprint, card_last4, card_brand, card_bin,
				 device_fingerprint, ip_address, idempotency_key, metadata,
				 payment_token)
			VALUES ($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (id) DO NOTHING`,
			chargeID, in.MerchantID, in.AmountCents, amount.Currency,
			card.Fingerprint, card.Last4, card.Brand, card.BIN,
			nullIfEmpty(in.DeviceFingerprint), nullIfEmpty(in.IPAddress),
			idemKey, orEmptyMap(in.Metadata), in.PaymentToken,
		)
		if err != nil {
			return fmt.Errorf("payments: insert charge: %w", err)
		}
		return nil
	}); err != nil {
		return nil, 0, err
	}

	// --- risk assessment (§14.1) ----------------------------------------------
	//
	// Sits after the idempotency claim and before the processor call: a
	// declined-for-risk charge must never reach the bank, and a retry must
	// replay the decline rather than re-score it (a second scoring could
	// legitimately reach a different verdict as velocity counters move, which
	// would make the same request non-idempotent).
	riskTxn := risk.Transaction{
		MerchantID:      in.MerchantID.String(),
		AmountCents:     in.AmountCents,
		Currency:        amount.Currency,
		CardFingerprint: card.Fingerprint,
		CardBIN:         card.BIN,
		CardBrand:       card.Brand,
		IPAddress:       in.IPAddress,
		DeviceID:        in.DeviceFingerprint,
		Timestamp:       time.Now().UTC(),
	}

	var decision risk.Decision
	if s.risk != nil {
		decision = s.risk.Assess(ctx, riskTxn)

		if err := s.recordRisk(ctx, chargeID, decision); err != nil {
			// The assessment is an audit record, not a gate. Failing to store
			// it must not fail the charge.
			slog.Warn("payments: failed to record risk assessment",
				"charge_id", chargeID, "error", err)
		}

		if decision.Level == risk.LevelHigh {
			slog.Info("payments: charge declined by risk engine",
				"charge_id", chargeID, "score", decision.Score,
				"rules", decision.RulesFired, "model_skipped", decision.ModelSkipped)

			// Counts as a decline for velocity: a fraudster's blocked attempts
			// are exactly the signal that catches the next one.
			s.risk.RecordOutcome(ctx, riskTxn, true)

			// A deliberately vague message. Telling the cardholder which rule
			// caught them tells a fraudster what to change; the merchant sees
			// the detail on the charge record and in the dashboard.
			if err := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
				"risk_declined", "This payment was declined by our fraud checks.",
				"", nil, in, card, amount); err != nil {
				return nil, 0, err
			}
			return &Charge{
				ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
				Currency: amount.Currency, Status: StatusFailed,
				FailureCode: "risk_declined", FailureMessage: "This payment was declined by our fraud checks.",
				CardLast4: card.Last4, CardBrand: card.Brand,
				Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
			}, 402, nil
		}
	}

	// A medium verdict currently proceeds with the assessment recorded. The
	// faithful behaviour is a 3DS step-up (§16), which needs the requires_action
	// state machine — the idempotency layer already has the status for it.

	// --- the PAN's only appearance outside the vault --------------------------
	//
	// Fetched here, immediately before submission, and held in a local variable
	// that goes out of scope as soon as the bank call returns. It is never
	// assigned to a struct field that outlives this function, never logged, and
	// never included in an error. A single-use token is consumed by this call,
	// so a replay of the same token cannot charge the card twice.
	pan, err := s.vault.Detokenize(ctx, in.PaymentToken, "payments-api",
		fmt.Sprintf("charge %s", chargeID))
	if err != nil {
		// Vault failures are clean failures: nothing was sent to the processor,
		// so this is unambiguous and safe to report as failed.
		if ferr := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
			"tokenization_error", "The payment token could not be used.", "",
			nil, in, card, amount); ferr != nil {
			return nil, 0, ferr
		}
		return nil, 0, err
	}

	// --- bank call, outside any transaction -----------------------------------
	bankResp, bankErr := s.bank.Charge(ctx, BankChargeRequest{
		// Deriving the downstream key from ours makes the processor's own
		// idempotency cover our retries too (§24.1).
		IdempotencyKey: fmt.Sprintf("%s:%s", in.MerchantID, idemKey),
		AmountCents:    in.AmountCents,
		Currency:       amount.Currency,
		CardNumber:     pan,
		CardExpMonth:   card.ExpMonth,
		CardExpYear:    card.ExpYear,
	}, in.SimulateOutcome)
	pan = "" // drop the reference as early as possible

	// The ambiguous case: park the charge for reconciliation instead of
	// guessing. Deliberately NOT marked failed — the money may well have moved.
	if bankErr != nil && errors.Is(bankErr, ErrAmbiguous) {
		slog.Warn("charge outcome ambiguous, deferring to reconciliation",
			"charge_id", chargeID, "error", bankErr)

		if err := s.finalize(ctx, chargeID, idemRecordID, StatusRequiresReconciliation,
			"", "", "", nil, in, card, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusRequiresReconciliation,
			CardLast4: card.Last4, CardBrand: card.Brand,
			Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 202, nil
	}

	// Provably never landed — safe to fail cleanly and let the client retry.
	if bankErr != nil {
		if err := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
			"processor_unreachable", bankErr.Error(), "", nil, in, card, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusFailed,
			FailureCode: "processor_unreachable", FailureMessage: "The payment processor could not be reached.",
			CardLast4: card.Last4, CardBrand: card.Brand,
			Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 502, nil
	}

	// Feed the outcome back into velocity, whatever it was.
	if s.risk != nil {
		s.risk.RecordOutcome(ctx, riskTxn, bankErr != nil || (bankResp != nil && bankResp.Status == "declined"))
	}

	if bankResp.Status == "declined" {
		if err := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
			bankResp.DeclineCode, bankResp.DeclineMessage, bankResp.ProcessorReference,
			nil, in, card, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusFailed,
			FailureCode: bankResp.DeclineCode, FailureMessage: bankResp.DeclineMessage,
			CardLast4: card.Last4, CardBrand: card.Brand,
			ProcessorRef: bankResp.ProcessorReference,
			Metadata:     orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 402, nil
	}

	// --- tx2: authorized. Post the ledger and finalize atomically -------------
	ledgerTxnID := uuid.New()
	if err := s.finalize(ctx, chargeID, idemRecordID, StatusSucceeded, "", "",
		bankResp.ProcessorReference, &ledgerTxnID, in, card, amount); err != nil {
		return nil, 0, err
	}

	return &Charge{
		ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
		Currency: amount.Currency, Status: StatusSucceeded,
		CardLast4: card.Last4, CardBrand: card.Brand,
		LedgerTxnID: &ledgerTxnID, ProcessorRef: bankResp.ProcessorReference,
		Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
	}, 200, nil
}

// recordRisk stores the assessment on the charge so a decision can be explained
// months later during a dispute or an audit (§14.5).
//
// Written outside the finalize transaction deliberately: this is an audit
// record, and a failure to store it must never roll back or block the charge
// it describes.
func (s *Service) recordRisk(ctx context.Context, chargeID uuid.UUID, d risk.Decision) error {
	rules, err := json.Marshal(d.RulesFired)
	if err != nil {
		return fmt.Errorf("payments: marshal fired rules: %w", err)
	}

	// The rule score is 0-100; risk_score is NUMERIC(5,4), so the model
	// probability is stored when present and the rule score normalized
	// otherwise.
	score := d.Score / 100
	if d.ModelScore != nil {
		score = *d.ModelScore
	}
	if score > 1 {
		score = 1
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE charges
		SET risk_score = $2, risk_level = $3, risk_rules_fired = $4, updated_at = now()
		WHERE id = $1`,
		chargeID, score, string(d.Level), rules,
	)
	if err != nil {
		return fmt.Errorf("payments: record risk assessment: %w", err)
	}
	return nil
}

// failClaimed marks a claimed idempotency key as failed when the request cannot
// proceed past validation — before any charge row exists.
//
// Without this, a request that claims a key and then fails a precondition would
// leave the key stuck in 'processing' until its lock went stale, so the client's
// immediate retry would get 409 instead of the actual error. Writing a terminal
// response makes the failure replayable, which is what the client needs to see.
func (s *Service) failClaimed(
	ctx context.Context,
	chargeID, idemRecordID uuid.UUID,
	in ChargeInput,
	amount money.Amount,
	code, message string,
) error {
	response := Charge{
		ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
		Currency: amount.Currency, Status: StatusFailed,
		FailureCode: code, FailureMessage: message,
		Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("payments: marshal failure response: %w", err)
	}

	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return idempotency.Complete(ctx, tx, idemRecordID, idempotency.StatusFailed, 400, body)
	})
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
	card *CardMetadata,
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
			CardLast4: card.Last4, CardBrand: card.Brand,
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
