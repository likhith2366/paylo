package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/likhith2366/paylo/internal/db"
)

// backoffSchedule is the retry ladder (§7): 1s, 5s, 30s, 2m, 10m, 1h, then
// hourly to roughly 24 hours.
//
// The budget is generous on purpose. A merchant's server being down for twenty
// minutes is NORMAL, not exceptional — they have deploys and outages like
// anyone else — so giving up quickly would drop legitimate events for an
// ordinary operational blip (§24.4).
var backoffSchedule = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	1 * time.Hour,
	2 * time.Hour,
	4 * time.Hour,
	8 * time.Hour,
	12 * time.Hour,
}

// MaxAttempts before a delivery is marked dead and alerted on.
var MaxAttempts = len(backoffSchedule) + 1

// Worker delivers queued webhooks.
type Worker struct {
	pool  *pgxpool.Pool
	http  *http.Client
	batch int
	poll  time.Duration
	// concurrency caps simultaneous in-flight deliveries. Unbounded delivery
	// would let one merchant's slow endpoint consume every worker goroutine
	// and stall delivery for everyone else — the bulkhead in §11.
	concurrency int

	// AllowPrivateTargets permits delivery to loopback and private addresses.
	//
	// DEVELOPMENT AND TESTS ONLY. In production this must stay false: a
	// merchant registering http://169.254.169.254/ would otherwise have this
	// worker fetch cloud credentials from inside the network perimeter and
	// POST them onward.
	//
	// It exists because the SSRF guard would otherwise be untestable — every
	// httptest server binds to 127.0.0.1, so an unconditional check makes the
	// success path impossible to exercise. A protection that cannot be tested
	// tends to get weakened later by someone who needs the tests to pass; an
	// explicit, loudly-documented switch is safer than that.
	AllowPrivateTargets bool
}

func NewWorker(pool *pgxpool.Pool, batch, concurrency int, poll time.Duration) *Worker {
	if batch <= 0 {
		batch = 50
	}
	if concurrency <= 0 {
		concurrency = 10
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	return &Worker{
		pool:        pool,
		batch:       batch,
		poll:        poll,
		concurrency: concurrency,
		http: &http.Client{
			// Merchants get a bounded window to respond. A slow endpoint is
			// treated as a failed attempt and retried, rather than being
			// allowed to hold a worker indefinitely.
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// Following redirects would let a merchant point us at an
				// internal address after the URL was validated — a classic
				// SSRF pivot.
				return http.ErrUseLastResponse
			},
		},
	}
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("webhook: delivery worker stopping")
			return
		case <-ticker.C:
			if _, err := w.DeliverBatch(ctx); err != nil {
				slog.Error("webhook: delivery batch failed", "error", err)
			}
		}
	}
}

type pending struct {
	id         uuid.UUID
	endpointID uuid.UUID
	eventID    uuid.UUID
	eventType  string
	payload    []byte
	attempts   int
	url        string
	secret     string
	prevSecret *string
}

// DeliverBatch claims and delivers one batch. Returns how many were attempted.
func (w *Worker) DeliverBatch(ctx context.Context) (int, error) {
	batch, err := w.claim(ctx)
	if err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	sem := make(chan struct{}, w.concurrency)
	done := make(chan struct{}, len(batch))

	for _, d := range batch {
		sem <- struct{}{}
		go func(d pending) {
			defer func() { <-sem; done <- struct{}{} }()
			w.deliver(ctx, d)
		}(d)
	}
	for range batch {
		<-done
	}
	return len(batch), nil
}

// claim takes ownership of due deliveries.
//
// SKIP LOCKED lets many worker replicas run without contending: each claims a
// disjoint set. next_attempt_at is pushed forward immediately so a crashed
// worker's rows become eligible again on their own rather than being stuck.
func (w *Worker) claim(ctx context.Context) ([]pending, error) {
	var batch []pending

	err := db.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT d.id, d.endpoint_id, d.event_id, d.event_type, d.payload, d.attempts,
			       e.url, e.secret, e.previous_secret
			FROM webhook_deliveries d
			JOIN webhook_endpoints e ON e.id = d.endpoint_id
			WHERE d.status = 'pending'
			  AND d.next_attempt_at <= now()
			  AND e.enabled
			ORDER BY d.next_attempt_at
			LIMIT $1
			FOR UPDATE OF d SKIP LOCKED`,
			w.batch,
		)
		if err != nil {
			return fmt.Errorf("webhook: claim deliveries: %w", err)
		}

		for rows.Next() {
			var d pending
			if err := rows.Scan(&d.id, &d.endpointID, &d.eventID, &d.eventType,
				&d.payload, &d.attempts, &d.url, &d.secret, &d.prevSecret); err != nil {
				rows.Close()
				return fmt.Errorf("webhook: scan delivery: %w", err)
			}
			batch = append(batch, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("webhook: read deliveries: %w", err)
		}

		// Lease the claimed rows: push them out of the due window so another
		// worker cannot pick them up while this one is mid-flight. If this
		// worker dies, they become due again once the lease lapses.
		for _, d := range batch {
			if _, err := tx.Exec(ctx, `
				UPDATE webhook_deliveries
				SET next_attempt_at = now() + INTERVAL '2 minutes'
				WHERE id = $1`, d.id,
			); err != nil {
				return fmt.Errorf("webhook: lease delivery: %w", err)
			}
		}
		return nil
	})

	return batch, err
}

func (w *Worker) deliver(ctx context.Context, d pending) {
	attempt := d.attempts + 1
	started := time.Now()

	statusCode, err := w.post(ctx, d)
	duration := time.Since(started)

	// 2xx means delivered. Everything else — including 4xx — is retried,
	// because a 404 or 500 from a merchant is far more often a broken deploy
	// than a permanent rejection, and dropping a payment.succeeded event on a
	// transient 404 is worse than delivering it late.
	delivered := err == nil && statusCode >= 200 && statusCode < 300

	recordCtx := context.WithoutCancel(ctx)
	if delivered {
		if err := w.markDelivered(recordCtx, d, attempt, statusCode, duration); err != nil {
			slog.Error("webhook: failed to record delivery", "delivery_id", d.id, "error", err)
		}
		return
	}

	if err := w.markFailed(recordCtx, d, attempt, statusCode, err, duration); err != nil {
		slog.Error("webhook: failed to record failure", "delivery_id", d.id, "error", err)
	}
}

func (w *Worker) post(ctx context.Context, d pending) (int, error) {
	if err := validateURL(d.url, w.AllowPrivateTargets); err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(d.payload))
	if err != nil {
		return 0, fmt.Errorf("webhook: build request: %w", err)
	}

	now := time.Now()
	signature := Sign(d.payload, d.secret, now)

	// During rotation both secrets sign, so a merchant who has updated their
	// config and one who has not both verify successfully (§22.2). Rotating
	// without this window breaks every in-flight delivery.
	if d.prevSecret != nil && *d.prevSecret != "" {
		old := Sign(d.payload, *d.prevSecret, now)
		if _, v1, found := cutSignature(old); found {
			signature += ",v1=" + v1
		}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "PayFlow-Webhooks/1.0")
	req.Header.Set(SignatureHeader, signature)
	req.Header.Set("PayFlow-Event-Id", d.eventID.String())
	req.Header.Set("PayFlow-Event-Type", d.eventType)

	resp, err := w.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain but discard: the body serves no purpose, while draining a bounded
	// amount lets the connection be reused. Capped so a merchant returning a
	// huge response cannot make us read it all.
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	return resp.StatusCode, nil
}

func (w *Worker) markDelivered(ctx context.Context, d pending, attempt, statusCode int, duration time.Duration) error {
	return db.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries
			SET status = 'delivered', attempts = $2, last_status_code = $3,
			    last_attempt_at = now(), delivered_at = now(), last_error = NULL
			WHERE id = $1`,
			d.id, attempt, statusCode,
		); err != nil {
			return fmt.Errorf("webhook: mark delivered: %w", err)
		}
		return recordAttempt(ctx, tx, d.id, attempt, &statusCode, "", duration)
	})
}

func (w *Worker) markFailed(ctx context.Context, d pending, attempt int, statusCode int, cause error, duration time.Duration) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	return db.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		if attempt >= MaxAttempts {
			// Retry budget exhausted. 'dead' is the DLQ: the row stays for the
			// merchant's delivery log and for alerting, and is never retried
			// automatically again.
			if _, err := tx.Exec(ctx, `
				UPDATE webhook_deliveries
				SET status = 'dead', attempts = $2, last_status_code = $3,
				    last_error = $4, last_attempt_at = now()
				WHERE id = $1`,
				d.id, attempt, nullInt(statusCode), nullStr(message),
			); err != nil {
				return fmt.Errorf("webhook: mark dead: %w", err)
			}
			// Loud on purpose. A dead webhook means a merchant will never learn
			// their customer paid, which is a correctness problem for them, not
			// merely an availability blip (§12).
			slog.Error("webhook: delivery exhausted its retry budget",
				"delivery_id", d.id, "endpoint_id", d.endpointID,
				"event_type", d.eventType, "attempts", attempt,
				"last_status", statusCode, "last_error", message)
			return recordAttempt(ctx, tx, d.id, attempt, &statusCode, message, duration)
		}

		delay := backoffFor(attempt)
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries
			SET attempts = $2, last_status_code = $3, last_error = $4,
			    last_attempt_at = now(), next_attempt_at = now() + $5::interval
			WHERE id = $1`,
			d.id, attempt, nullInt(statusCode), nullStr(message), delay.String(),
		); err != nil {
			return fmt.Errorf("webhook: schedule retry: %w", err)
		}
		return recordAttempt(ctx, tx, d.id, attempt, &statusCode, message, duration)
	})
}

func recordAttempt(ctx context.Context, tx pgx.Tx, deliveryID uuid.UUID, attempt int, statusCode *int, errMsg string, duration time.Duration) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO webhook_delivery_attempts
			(delivery_id, attempt, status_code, error, duration_ms)
		VALUES ($1, $2, $3, $4, $5)`,
		deliveryID, attempt, nullIntPtr(statusCode), nullStr(errMsg),
		int(duration.Milliseconds()),
	)
	if err != nil {
		return fmt.Errorf("webhook: record attempt: %w", err)
	}
	return nil
}

// backoffFor returns the delay before the next attempt, with jitter.
//
// Jitter matters more than it looks: without it, every delivery queued during a
// merchant's outage retries at the same instant when the schedule elapses,
// producing a thundering herd against a server that has only just recovered —
// and often knocking it over again (§11).
func backoffFor(attempt int) time.Duration {
	idx := attempt - 1
	if idx >= len(backoffSchedule) {
		idx = len(backoffSchedule) - 1
	}
	base := backoffSchedule[idx]
	jitter := time.Duration(rand.Int63n(int64(base / 4)))
	return base + jitter
}

// validateURL blocks obviously unsafe endpoints.
//
// SSRF protection: a merchant could otherwise register a URL pointing at
// internal infrastructure or cloud metadata and have our worker fetch it for
// them, from inside the network perimeter. This is a basic check; production
// would additionally resolve the host and reject private ranges at dial time,
// since DNS can be rebound after validation.
func validateURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook: invalid endpoint url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("webhook: endpoint scheme %q is not allowed", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return errors.New("webhook: endpoint has no host")
	}
	if allowPrivate {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("webhook: endpoint address %s is not routable", host)
		}
	}
	// "localhost" resolves to loopback but is not an IP literal, so it must be
	// rejected by name as well.
	if host == "localhost" {
		return fmt.Errorf("webhook: endpoint address %s is not routable", host)
	}
	return nil
}

func cutSignature(header string) (ts, v1 string, found bool) {
	for _, part := range splitComma(header) {
		if len(part) > 3 && part[:3] == "v1=" {
			return "", part[3:], true
		}
	}
	return "", "", false
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullIntPtr(n *int) any {
	if n == nil || *n == 0 {
		return nil
	}
	return *n
}
