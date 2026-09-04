// Package idempotency implements exactly-once request handling (§4).
//
// The whole design rests on one observation: Postgres's UNIQUE constraint is
// already a correct, atomic, cross-process lock. INSERT ... ON CONFLICT DO
// NOTHING either succeeds (this process owns the request) or does not (someone
// else got there first). There is no window between checking and claiming, so
// there is nothing to race.
//
// This is why there is no Redlock here. A distributed lock built on Redis is
// hard to make correct and is not something to bet money on; the database that
// already has to be in the transaction for the ledger write can do the job for
// free, and with stronger guarantees. Redis is used only as a read-through
// cache for replaying completed responses (§4.2).
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	StatusProcessing     = "processing"
	StatusRequiresAction = "requires_action"
	StatusCompleted      = "completed"
	StatusFailed         = "failed"
)

// StaleLockAfter is how long a 'processing' row may sit untouched before another
// worker may assume the original owner died and steal it (§4.2 step 4).
//
// The value is a tradeoff. Too short and a slow-but-alive request gets its work
// duplicated; too long and a crashed pod blocks that key until it expires. 30s
// comfortably exceeds the request timeout budget, so any row older than this
// really does imply a dead owner rather than a slow one.
const StaleLockAfter = 30 * time.Second

var (
	// ErrInFlight means another worker holds a live lock on this key.
	// Callers surface this as 409 Conflict.
	ErrInFlight = errors.New("idempotency: request already in flight")

	// ErrKeyReused means the same key arrived with a different request body.
	// This is a client bug — surfaced as 422, matching Stripe's behaviour (§4.2).
	ErrKeyReused = errors.New("idempotency: key reused with a different request body")
)

// Record is the stored state of one idempotent request.
type Record struct {
	ID             uuid.UUID
	Key            string
	MerchantID     uuid.UUID
	RequestHash    string
	Status         string
	ResponseBody   []byte
	ResponseStatus int
	CreatedAt      time.Time
}

// Outcome tells the caller what to do next.
type Outcome int

const (
	// OutcomeProceed means this caller owns the request and should do the work.
	OutcomeProceed Outcome = iota
	// OutcomeReplay means the request already completed; return the stored
	// response verbatim without re-executing anything.
	OutcomeReplay
)

// HashRequest produces the request fingerprint used to detect key reuse.
//
// The body is normalized through a canonical JSON encoding first, so that two
// semantically identical requests that differ only in key order or whitespace
// hash identically. Without this, a client whose HTTP library reorders JSON
// fields between retries would get a spurious 422 on a legitimate retry.
func HashRequest(body []byte) (string, error) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not JSON — hash the raw bytes rather than failing. Some endpoints
		// take form encoding or an empty body.
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:]), nil
	}
	canonical, err := canonicalJSON(parsed)
	if err != nil {
		return "", fmt.Errorf("idempotency: canonicalize request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON re-encodes a decoded value with object keys sorted, recursively.
func canonicalJSON(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		buf := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			vb, err := canonicalJSON(t[k])
			if err != nil {
				return nil, err
			}
			buf = append(buf, kb...)
			buf = append(buf, ':')
			buf = append(buf, vb...)
		}
		return append(buf, '}'), nil

	case []any:
		buf := []byte{'['}
		for i, item := range t {
			if i > 0 {
				buf = append(buf, ',')
			}
			ib, err := canonicalJSON(item)
			if err != nil {
				return nil, err
			}
			buf = append(buf, ib...)
		}
		return append(buf, ']'), nil

	default:
		return json.Marshal(t)
	}
}

// Begin claims an idempotency key, or reports that it is already claimed.
//
// Must be called inside the same transaction that will later perform the
// business write, so that claiming the key and doing the work commit together.
func Begin(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key, endpoint, requestHash string) (Outcome, *Record, error) {
	// The claim. ON CONFLICT DO NOTHING means at most one caller gets a row
	// back; everyone else falls through to the inspection path below.
	var rec Record
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency_keys
			(idempotency_key, merchant_id, request_hash, endpoint, status, locked_at)
		VALUES ($1, $2, $3, $4, 'processing', now())
		ON CONFLICT (merchant_id, idempotency_key) DO NOTHING
		RETURNING id, idempotency_key, merchant_id, request_hash, status, created_at`,
		key, merchantID, requestHash, endpoint,
	).Scan(&rec.ID, &rec.Key, &rec.MerchantID, &rec.RequestHash, &rec.Status, &rec.CreatedAt)

	if err == nil {
		return OutcomeProceed, &rec, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, fmt.Errorf("idempotency: claim key: %w", err)
	}

	// A row already exists. FOR UPDATE serializes concurrent arrivals on the
	// same key so that exactly one of them can steal a stale lock.
	//
	// locked_at is compared against Postgres's now(), never the pod's clock:
	// wall clocks drift across a fleet and this decision controls whether work
	// gets duplicated (§22.1).
	var (
		existing     Record
		responseBody []byte
		respStatus   *int
		lockIsStale  bool
	)
	err = tx.QueryRow(ctx, `
		SELECT id, idempotency_key, merchant_id, request_hash, status,
		       response_body, response_status,
		       (locked_at IS NULL OR locked_at < now() - $2::interval) AS lock_is_stale,
		       created_at
		FROM idempotency_keys
		WHERE merchant_id = $1 AND idempotency_key = $3
		FOR UPDATE`,
		merchantID, StaleLockAfter.String(), key,
	).Scan(&existing.ID, &existing.Key, &existing.MerchantID, &existing.RequestHash,
		&existing.Status, &responseBody, &respStatus, &lockIsStale, &existing.CreatedAt)
	if err != nil {
		return 0, nil, fmt.Errorf("idempotency: load existing key: %w", err)
	}

	// Checked before status: reusing a key for a *different* request is a
	// client bug regardless of what state the original request reached, and
	// replaying an unrelated response would be worse than erroring.
	if existing.RequestHash != requestHash {
		return 0, nil, ErrKeyReused
	}

	existing.ResponseBody = responseBody
	if respStatus != nil {
		existing.ResponseStatus = *respStatus
	}

	switch existing.Status {
	case StatusCompleted, StatusFailed:
		// Terminal. Replay verbatim — do not re-execute.
		return OutcomeReplay, &existing, nil

	case StatusRequiresAction:
		// A 3DS challenge is outstanding (§16). Replay the requires_action
		// response so the client is re-pointed at the challenge URL; the charge
		// resumes under this same key once the challenge completes.
		return OutcomeReplay, &existing, nil

	case StatusProcessing:
		if !lockIsStale {
			return 0, nil, ErrInFlight
		}
		// The previous owner died. Take the lock and redo the work.
		if _, err := tx.Exec(ctx,
			`UPDATE idempotency_keys SET locked_at = now() WHERE id = $1`,
			existing.ID,
		); err != nil {
			return 0, nil, fmt.Errorf("idempotency: steal stale lock: %w", err)
		}
		return OutcomeProceed, &existing, nil

	default:
		return 0, nil, fmt.Errorf("idempotency: unknown status %q on key %s", existing.Status, key)
	}
}

// Release abandons a claim without recording an outcome.
//
// For the case where a request failed for a reason that says nothing about the
// request itself — a dependency was briefly unavailable — so there is no
// meaningful response to replay. Deleting the row lets an immediate retry
// proceed as a fresh attempt.
//
// The alternative, writing a terminal failure, would be wrong: it burns a
// transient outage into a permanent answer, and the client's retry would
// replay that failure forever under the same key. Leaving the row in
// 'processing' would be wrong too — the client would get 409 until the lock
// went stale, for a request nobody is actually working on.
//
// Only ever called on a row this caller just claimed, so there is no risk of
// releasing someone else's in-flight work.
func Release(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE id = $1 AND status = 'processing'`, id)
	if err != nil {
		return fmt.Errorf("idempotency: release key %s: %w", id, err)
	}
	return nil
}

// Complete records the final response for a key.
//
// Must run in the SAME transaction as the business write it describes (§4.2
// step 5). Committing them separately allows a "completed" idempotency record
// with no ledger entry behind it — a request that can never be retried and
// never actually happened.
func Complete(ctx context.Context, tx pgx.Tx, id uuid.UUID, status string, httpStatus int, responseBody []byte) error {
	_, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = $2, response_status = $3, response_body = $4, locked_at = NULL
		WHERE id = $1`,
		id, status, httpStatus, responseBody,
	)
	if err != nil {
		return fmt.Errorf("idempotency: complete key %s: %w", id, err)
	}
	return nil
}
