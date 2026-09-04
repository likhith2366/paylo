// Package dashboard serves the merchant-facing read API (§2.2).
//
// Read-only by design. Every mutation goes through the payments API, which
// carries the idempotency and ledger machinery; a second write path into the
// same tables would be a second place to get exactly-once wrong.
//
// Two things shape the queries here:
//
//	CURSOR PAGINATION, NOT OFFSET. OFFSET re-scans everything it skips, so
//	page 500 costs 500 pages of work — and rows shift under the reader as new
//	charges arrive, so an offset can show the same row twice or skip one. A
//	cursor on (created_at, id) is stable and costs the same on every page.
//
//	MERCHANT SCOPE IN EVERY QUERY. Not in a wrapper, not in middleware alone —
//	in the WHERE clause of each statement, so a missing filter is a compile-time
//	absence rather than a silent cross-merchant leak.
package dashboard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultLimit = 25
	maxLimit     = 100
)

// ErrBadCursor is a client error, not a server one. Returning 500 for a
// malformed cursor sends whoever is debugging it looking for a server fault
// that does not exist.
var ErrBadCursor = errors.New("dashboard: malformed cursor")

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Page is the list envelope. Mirrors Stripe's shape, which merchants already
// know how to consume.
type Page[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	HasMore bool   `json:"has_more"`
	// NextCursor is opaque on purpose — encoding it means the pagination
	// scheme can change without breaking integrations (§22.2).
	NextCursor string `json:"next_cursor,omitempty"`
}

type ListParams struct {
	Limit  int
	Cursor string
	Status string
}

// cursor encodes the position after the last row returned.
type cursor struct {
	createdAt time.Time
	id        uuid.UUID
}

func encodeCursor(c cursor) string {
	raw := fmt.Sprintf("%d|%s", c.createdAt.UnixNano(), c.id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, ErrBadCursor
	}
	nanos, idStr, found := strings.Cut(string(raw), "|")
	if !found {
		return cursor{}, ErrBadCursor
	}
	var n int64
	if _, err := fmt.Sscanf(nanos, "%d", &n); err != nil {
		return cursor{}, ErrBadCursor
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return cursor{}, ErrBadCursor
	}
	return cursor{createdAt: time.Unix(0, n).UTC(), id: id}, nil
}

func (p ListParams) limit() int {
	switch {
	case p.Limit <= 0:
		return defaultLimit
	case p.Limit > maxLimit:
		return maxLimit
	default:
		return p.Limit
	}
}

// --- charges ---------------------------------------------------------------

type Charge struct {
	ID            uuid.UUID `json:"id"`
	AmountCents   int64     `json:"amount"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	FailureCode   string    `json:"failure_code,omitempty"`
	CardLast4     string    `json:"card_last4,omitempty"`
	CardBrand     string    `json:"card_brand,omitempty"`
	RiskLevel     string    `json:"risk_level,omitempty"`
	RiskScore     *float64  `json:"risk_score,omitempty"`
	RefundedCents int64     `json:"amount_refunded"`
	Disputed      bool      `json:"disputed"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Service) ListCharges(ctx context.Context, merchantID uuid.UUID, p ListParams) (*Page[Charge], error) {
	cur, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	// limit+1 so the extra row reveals whether another page exists, without a
	// second COUNT query over the whole table.
	limit := p.limit()
	args := []any{merchantID, limit + 1}
	where := "c.merchant_id = $1"

	if p.Status != "" {
		args = append(args, p.Status)
		where += fmt.Sprintf(" AND c.status = $%d", len(args))
	}
	if p.Cursor != "" {
		args = append(args, cur.createdAt, cur.id)
		// Row-value comparison, so ties on created_at break deterministically
		// by id — otherwise two charges in the same microsecond could be
		// returned twice or skipped.
		where += fmt.Sprintf(" AND (c.created_at, c.id) < ($%d, $%d)", len(args)-1, len(args))
	}

	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.amount_cents, c.currency, c.status, c.failure_code,
		       c.card_last4, c.card_brand, c.risk_level, c.risk_score,
		       COALESCE(t.refunded_cents, 0),
		       EXISTS (SELECT 1 FROM disputes d WHERE d.charge_id = c.id),
		       c.created_at
		FROM charges c
		LEFT JOIN charge_refund_totals t ON t.charge_id = c.id
		WHERE `+where+`
		ORDER BY c.created_at DESC, c.id DESC
		LIMIT $2`, args...)
	if err != nil {
		return nil, fmt.Errorf("dashboard: list charges: %w", err)
	}
	defer rows.Close()

	page := &Page[Charge]{Object: "list", Data: []Charge{}}
	for rows.Next() {
		var c Charge
		var failureCode, last4, brand, riskLevel *string
		if err := rows.Scan(&c.ID, &c.AmountCents, &c.Currency, &c.Status, &failureCode,
			&last4, &brand, &riskLevel, &c.RiskScore, &c.RefundedCents,
			&c.Disputed, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("dashboard: scan charge: %w", err)
		}
		c.FailureCode = deref(failureCode)
		c.CardLast4 = deref(last4)
		c.CardBrand = deref(brand)
		c.RiskLevel = deref(riskLevel)
		page.Data = append(page.Data, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dashboard: read charges: %w", err)
	}

	if len(page.Data) > limit {
		page.Data = page.Data[:limit]
		page.HasMore = true
		last := page.Data[len(page.Data)-1]
		page.NextCursor = encodeCursor(cursor{createdAt: last.CreatedAt, id: last.ID})
	}
	return page, nil
}

// --- balance ---------------------------------------------------------------

type Balance struct {
	Currency string `json:"currency"`
	// Available is what could be paid out now: settled, less what is reserved
	// against open disputes. NOT the raw ledger balance (§18, §19).
	AvailableCents int64 `json:"available"`
	// Pending is money that has not cleared the T+2 hold yet.
	PendingCents int64 `json:"pending"`
	// Reserved is held against disputes that are still open.
	ReservedCents int64 `json:"reserved"`
	// InTransit has left the balance for a payout the bank has not confirmed.
	InTransitCents int64 `json:"in_transit"`
}

// Balances reports what a merchant actually has, which is deliberately not one
// number. "Your balance" is ambiguous in a payments system — settled funds,
// funds still clearing, and funds reserved against disputes are three
// different things, and a merchant who sees only the total will plan against
// money they cannot take.
func (s *Service) Balances(ctx context.Context, merchantID uuid.UUID) ([]Balance, error) {
	const holdPeriod = "48 hours"

	rows, err := s.pool.Query(ctx, `
		WITH settled AS (
			SELECT a.currency,
			       -SUM(CASE WHEN e.direction='debit' THEN e.amount_cents
			                 ELSE -e.amount_cents END) AS cents
			FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
			WHERE a.merchant_id = $1 AND a.account_type = 'merchant_balance'
			  AND e.created_at <= now() - $2::interval
			GROUP BY a.currency
		),
		total AS (
			SELECT a.currency,
			       -SUM(CASE WHEN e.direction='debit' THEN e.amount_cents
			                 ELSE -e.amount_cents END) AS cents
			FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
			WHERE a.merchant_id = $1 AND a.account_type = 'merchant_balance'
			GROUP BY a.currency
		),
		transit AS (
			SELECT a.currency,
			       -SUM(CASE WHEN e.direction='debit' THEN e.amount_cents
			                 ELSE -e.amount_cents END) AS cents
			FROM ledger_entries e JOIN accounts a ON a.id = e.account_id
			WHERE a.merchant_id = $1 AND a.account_type = 'in_transit'
			GROUP BY a.currency
		),
		reserved AS (
			SELECT currency, SUM(amount_cents) AS cents
			FROM disputes
			WHERE merchant_id = $1 AND status IN ('needs_response','under_review')
			GROUP BY currency
		)
		SELECT t.currency,
		       COALESCE(s.cents,0), COALESCE(t.cents,0),
		       COALESCE(r.cents,0), COALESCE(x.cents,0)
		FROM total t
		LEFT JOIN settled s ON s.currency = t.currency
		LEFT JOIN reserved r ON r.currency = t.currency
		LEFT JOIN transit x ON x.currency = t.currency`,
		merchantID, holdPeriod)
	if err != nil {
		return nil, fmt.Errorf("dashboard: balances: %w", err)
	}
	defer rows.Close()

	out := []Balance{}
	for rows.Next() {
		var b Balance
		var settled, total, reserved, transit int64
		if err := rows.Scan(&b.Currency, &settled, &total, &reserved, &transit); err != nil {
			return nil, fmt.Errorf("dashboard: scan balance: %w", err)
		}
		b.ReservedCents = reserved
		b.InTransitCents = transit
		b.PendingCents = total - settled
		b.AvailableCents = settled - reserved
		if b.AvailableCents < 0 {
			// A merchant can owe more than they hold (§19). Surfacing that as
			// a negative available balance rather than clamping to zero is the
			// honest presentation — they need to know they are in debt.
			b.AvailableCents = settled - reserved
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- summary ---------------------------------------------------------------

type Summary struct {
	WindowDays       int     `json:"window_days"`
	GrossVolumeCents int64   `json:"gross_volume"`
	NetVolumeCents   int64   `json:"net_volume"`
	Currency         string  `json:"currency"`
	SucceededCount   int     `json:"succeeded_count"`
	FailedCount      int     `json:"failed_count"`
	SuccessRate      float64 `json:"success_rate"`
	RefundedCents    int64   `json:"refunded"`
	DisputedCount    int     `json:"disputed_count"`
	// UnresolvedCount is charges parked awaiting reconciliation. A number that
	// climbs means the processor is unhealthy or the scheduler has stopped.
	UnresolvedCount int `json:"unresolved_count"`
}

func (s *Service) Summary(ctx context.Context, merchantID uuid.UUID, days int) (*Summary, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	sum := &Summary{WindowDays: days, Currency: "USD"}

	err := s.pool.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(amount_cents) FILTER (WHERE status='succeeded'), 0),
		  COUNT(*) FILTER (WHERE status='succeeded'),
		  COUNT(*) FILTER (WHERE status='failed'),
		  COUNT(*) FILTER (WHERE status='requires_reconciliation')
		FROM charges
		WHERE merchant_id = $1 AND created_at > now() - make_interval(days => $2)`,
		merchantID, days,
	).Scan(&sum.GrossVolumeCents, &sum.SucceededCount, &sum.FailedCount, &sum.UnresolvedCount)
	if err != nil {
		return nil, fmt.Errorf("dashboard: summary: %w", err)
	}

	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(r.amount_cents), 0), COUNT(DISTINCT d.id)
		FROM charges c
		LEFT JOIN refunds r ON r.charge_id = c.id AND r.status = 'succeeded'
		LEFT JOIN disputes d ON d.charge_id = c.id
		WHERE c.merchant_id = $1 AND c.created_at > now() - make_interval(days => $2)`,
		merchantID, days,
	).Scan(&sum.RefundedCents, &sum.DisputedCount)
	if err != nil {
		return nil, fmt.Errorf("dashboard: summary refunds: %w", err)
	}

	sum.NetVolumeCents = sum.GrossVolumeCents - sum.RefundedCents
	if total := sum.SucceededCount + sum.FailedCount; total > 0 {
		sum.SuccessRate = float64(sum.SucceededCount) / float64(total)
	}
	return sum, nil
}

// VolumePoint is one day of the volume chart.
type VolumePoint struct {
	Date        string `json:"date"`
	VolumeCents int64  `json:"volume"`
	Count       int    `json:"count"`
}

func (s *Service) Volume(ctx context.Context, merchantID uuid.UUID, days int) ([]VolumePoint, error) {
	if days <= 0 || days > 365 {
		days = 30
	}

	// generate_series so days with no charges appear as zero rather than being
	// absent — a chart with gaps reads as missing data, not as a quiet day.
	rows, err := s.pool.Query(ctx, `
		SELECT to_char(d.day, 'YYYY-MM-DD'),
		       COALESCE(SUM(c.amount_cents) FILTER (WHERE c.status='succeeded'), 0),
		       COUNT(c.id) FILTER (WHERE c.status='succeeded')
		FROM generate_series(
		       date_trunc('day', now() - make_interval(days => $2)),
		       date_trunc('day', now()), '1 day') AS d(day)
		LEFT JOIN charges c
		  ON c.merchant_id = $1 AND date_trunc('day', c.created_at) = d.day
		GROUP BY d.day ORDER BY d.day`,
		merchantID, days)
	if err != nil {
		return nil, fmt.Errorf("dashboard: volume: %w", err)
	}
	defer rows.Close()

	out := []VolumePoint{}
	for rows.Next() {
		var p VolumePoint
		if err := rows.Scan(&p.Date, &p.VolumeCents, &p.Count); err != nil {
			return nil, fmt.Errorf("dashboard: scan volume: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- disputes, payouts, webhook deliveries ---------------------------------

type Dispute struct {
	ID            uuid.UUID `json:"id"`
	ChargeID      uuid.UUID `json:"charge_id"`
	AmountCents   int64     `json:"amount"`
	Currency      string    `json:"currency"`
	Reason        string    `json:"reason"`
	Status        string    `json:"status"`
	EvidenceDueBy time.Time `json:"evidence_due_by"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Service) ListDisputes(ctx context.Context, merchantID uuid.UUID, p ListParams) (*Page[Dispute], error) {
	limit := p.limit()
	rows, err := s.pool.Query(ctx, `
		SELECT id, charge_id, amount_cents, currency, reason, status,
		       evidence_due_by, created_at
		FROM disputes WHERE merchant_id = $1
		ORDER BY created_at DESC LIMIT $2`, merchantID, limit)
	if err != nil {
		return nil, fmt.Errorf("dashboard: list disputes: %w", err)
	}
	defer rows.Close()

	page := &Page[Dispute]{Object: "list", Data: []Dispute{}}
	for rows.Next() {
		var d Dispute
		if err := rows.Scan(&d.ID, &d.ChargeID, &d.AmountCents, &d.Currency,
			&d.Reason, &d.Status, &d.EvidenceDueBy, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("dashboard: scan dispute: %w", err)
		}
		page.Data = append(page.Data, d)
	}
	return page, rows.Err()
}

type Payout struct {
	ID          uuid.UUID  `json:"id"`
	AmountCents int64      `json:"amount"`
	Currency    string     `json:"currency"`
	Status      string     `json:"status"`
	FailureCode string     `json:"failure_code,omitempty"`
	PaidAt      *time.Time `json:"paid_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (s *Service) ListPayouts(ctx context.Context, merchantID uuid.UUID, p ListParams) (*Page[Payout], error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, amount_cents, currency, status, failure_code, paid_at, created_at
		FROM payouts WHERE merchant_id = $1
		ORDER BY created_at DESC LIMIT $2`, merchantID, p.limit())
	if err != nil {
		return nil, fmt.Errorf("dashboard: list payouts: %w", err)
	}
	defer rows.Close()

	page := &Page[Payout]{Object: "list", Data: []Payout{}}
	for rows.Next() {
		var out Payout
		var failureCode *string
		if err := rows.Scan(&out.ID, &out.AmountCents, &out.Currency, &out.Status,
			&failureCode, &out.PaidAt, &out.CreatedAt); err != nil {
			return nil, fmt.Errorf("dashboard: scan payout: %w", err)
		}
		out.FailureCode = deref(failureCode)
		page.Data = append(page.Data, out)
	}
	return page, rows.Err()
}

// WebhookDelivery is the merchant-facing delivery log (§7). Merchants need to
// see WHY a delivery failed repeatedly, not just that it did.
type WebhookDelivery struct {
	ID          uuid.UUID  `json:"id"`
	EventID     uuid.UUID  `json:"event_id"`
	EventType   string     `json:"event_type"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	StatusCode  *int       `json:"last_status_code,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	NextAttempt *time.Time `json:"next_attempt_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (s *Service) ListWebhookDeliveries(ctx context.Context, merchantID uuid.UUID, p ListParams) (*Page[WebhookDelivery], error) {
	args := []any{merchantID, p.limit()}
	where := "merchant_id = $1"
	if p.Status != "" {
		args = append(args, p.Status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, event_id, event_type, status, attempts,
		       last_status_code, last_error, next_attempt_at, created_at
		FROM webhook_deliveries WHERE `+where+`
		ORDER BY created_at DESC LIMIT $2`, args...)
	if err != nil {
		return nil, fmt.Errorf("dashboard: list deliveries: %w", err)
	}
	defer rows.Close()

	page := &Page[WebhookDelivery]{Object: "list", Data: []WebhookDelivery{}}
	for rows.Next() {
		var d WebhookDelivery
		var lastError *string
		if err := rows.Scan(&d.ID, &d.EventID, &d.EventType, &d.Status, &d.Attempts,
			&d.StatusCode, &lastError, &d.NextAttempt, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("dashboard: scan delivery: %w", err)
		}
		d.LastError = deref(lastError)
		page.Data = append(page.Data, d)
	}
	return page, rows.Err()
}

// GetCharge returns one charge with its refunds and disputes.
func (s *Service) GetCharge(ctx context.Context, merchantID, chargeID uuid.UUID) (*Charge, error) {
	var c Charge
	var failureCode, last4, brand, riskLevel *string
	err := s.pool.QueryRow(ctx, `
		SELECT c.id, c.amount_cents, c.currency, c.status, c.failure_code,
		       c.card_last4, c.card_brand, c.risk_level, c.risk_score,
		       COALESCE(t.refunded_cents, 0),
		       EXISTS (SELECT 1 FROM disputes d WHERE d.charge_id = c.id),
		       c.created_at
		FROM charges c
		LEFT JOIN charge_refund_totals t ON t.charge_id = c.id
		WHERE c.id = $1 AND c.merchant_id = $2`,
		chargeID, merchantID,
	).Scan(&c.ID, &c.AmountCents, &c.Currency, &c.Status, &failureCode,
		&last4, &brand, &riskLevel, &c.RiskScore, &c.RefundedCents, &c.Disputed, &c.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dashboard: get charge: %w", err)
	}
	c.FailureCode = deref(failureCode)
	c.CardLast4 = deref(last4)
	c.CardBrand = deref(brand)
	c.RiskLevel = deref(riskLevel)
	return &c, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
