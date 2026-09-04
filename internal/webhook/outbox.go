package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/db"
)

// Poller drains outbox_events into webhook_deliveries (§22.1).
//
// This is the second half of the transactional outbox. The first half — writing
// the outbox row in the same transaction as the ledger entry — is what
// guarantees the event exists. This is what guarantees it eventually leaves.
//
// The poller is safe to run in multiple replicas and safe to crash at any
// point, for one reason: fanning an event out to an endpoint is idempotent.
// The UNIQUE (endpoint_id, event_id) constraint means a re-read outbox row
// cannot create a second delivery, so "did I already publish this?" never has
// to be answered correctly.
type Poller struct {
	pool     *pgxpool.Pool
	batch    int
	interval time.Duration
}

func NewPoller(pool *pgxpool.Pool, batch int, interval time.Duration) *Poller {
	if batch <= 0 {
		batch = 100
	}
	if interval <= 0 {
		interval = 1 * time.Second
	}
	return &Poller{pool: pool, batch: batch, interval: interval}
}

// Run polls until the context is cancelled.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("webhook: outbox poller stopping")
			return
		case <-ticker.C:
			n, err := p.Drain(ctx)
			if err != nil {
				// Log and keep going. A transient DB error must not kill the
				// poller — the events are durable, so the only cost of a failed
				// tick is latency.
				slog.Error("webhook: outbox drain failed", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("webhook: fanned out events", "count", n)
			}
		}
	}
}

// Drain publishes one batch of unpublished outbox events. Returns how many
// were processed.
func (p *Poller) Drain(ctx context.Context) (int, error) {
	var processed int

	err := db.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		// FOR UPDATE SKIP LOCKED is what makes multiple poller replicas work:
		// each claims a different set of rows rather than blocking on the same
		// ones. Without SKIP LOCKED, a second replica would serialize behind
		// the first and add nothing.
		rows, err := tx.Query(ctx, `
			SELECT id, aggregate_id, event_type, payload
			FROM outbox_events
			WHERE NOT published
			ORDER BY id
			LIMIT $1
			FOR UPDATE SKIP LOCKED`,
			p.batch,
		)
		if err != nil {
			return fmt.Errorf("webhook: claim outbox events: %w", err)
		}

		type event struct {
			id          int64
			aggregateID uuid.UUID
			eventType   string
			payload     []byte
		}
		var events []event

		for rows.Next() {
			var e event
			if err := rows.Scan(&e.id, &e.aggregateID, &e.eventType, &e.payload); err != nil {
				rows.Close()
				return fmt.Errorf("webhook: scan outbox event: %w", err)
			}
			events = append(events, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("webhook: read outbox events: %w", err)
		}

		for _, e := range events {
			if err := p.fanOut(ctx, tx, e.aggregateID, e.eventType, e.payload); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE outbox_events
				SET published = true, published_at = now(), attempts = attempts + 1
				WHERE id = $1`, e.id,
			); err != nil {
				return fmt.Errorf("webhook: mark outbox published: %w", err)
			}
			processed++
		}
		return nil
	})

	return processed, err
}

// fanOut creates one delivery per subscribed endpoint.
//
// Runs in the SAME transaction as marking the outbox row published. If it
// committed separately, a crash between the two would either lose the event
// (marked published, no delivery) or duplicate it — the exact inconsistency
// the outbox pattern exists to eliminate.
func (p *Poller) fanOut(ctx context.Context, tx pgx.Tx, aggregateID uuid.UUID, eventType string, payload []byte) error {
	// The merchant is derived from the payload rather than carried on the
	// outbox row, because outbox_events is generic across aggregates. A
	// payload without one is not an error: internal events legitimately have
	// no merchant to notify.
	merchantID, ok := merchantFromPayload(ctx, tx, aggregateID, eventType)
	if !ok {
		return nil
	}

	rows, err := tx.Query(ctx, `
		SELECT id
		FROM webhook_endpoints
		WHERE merchant_id = $1
		  AND enabled
		  AND (cardinality(subscribed_events) = 0 OR $2 = ANY(subscribed_events))`,
		merchantID, eventType,
	)
	if err != nil {
		return fmt.Errorf("webhook: load endpoints: %w", err)
	}
	defer rows.Close()

	var endpointIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("webhook: scan endpoint: %w", err)
		}
		endpointIDs = append(endpointIDs, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("webhook: read endpoints: %w", err)
	}

	// The event_id is derived deterministically from the aggregate and type,
	// so re-processing the same outbox row produces the same id. Combined with
	// the UNIQUE constraint, that is what makes the fan-out idempotent — and
	// it is also the id merchants dedupe on, so a retry looks identical to
	// them too.
	eventID := deterministicEventID(aggregateID, eventType)

	for _, endpointID := range endpointIDs {
		envelope, err := json.Marshal(map[string]any{
			"id":         eventID,
			"type":       eventType,
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"data":       json.RawMessage(payload),
		})
		if err != nil {
			return fmt.Errorf("webhook: build envelope: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries
				(endpoint_id, merchant_id, event_id, event_type, payload)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (endpoint_id, event_id) DO NOTHING`,
			endpointID, merchantID, eventID, eventType, envelope,
		); err != nil {
			return fmt.Errorf("webhook: queue delivery: %w", err)
		}
	}
	return nil
}

// merchantFromPayload resolves which merchant an event belongs to.
//
// Looked up by aggregate id across the tables that can produce webhooks. A
// merchant_id column on outbox_events would be cheaper, but would also let a
// caller write an event attributed to the wrong merchant; deriving it from the
// aggregate makes that impossible.
func merchantFromPayload(ctx context.Context, tx pgx.Tx, aggregateID uuid.UUID, eventType string) (uuid.UUID, bool) {
	var table string
	switch {
	case len(eventType) >= 7 && eventType[:7] == "payment":
		table = "charges"
	case len(eventType) >= 6 && eventType[:6] == "refund":
		table = "refunds"
	case len(eventType) >= 7 && eventType[:7] == "dispute":
		table = "disputes"
	default:
		return uuid.Nil, false
	}

	var merchantID uuid.UUID
	err := tx.QueryRow(ctx,
		fmt.Sprintf(`SELECT merchant_id FROM %s WHERE id = $1`, table),
		aggregateID,
	).Scan(&merchantID)
	if err != nil {
		return uuid.Nil, false
	}
	return merchantID, true
}

// deterministicEventID derives a stable UUID from the aggregate and event type,
// so re-processing an outbox row yields the same id rather than a new one.
func deterministicEventID(aggregateID uuid.UUID, eventType string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, append(aggregateID[:], eventType...))
}
