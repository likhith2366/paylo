package risk

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Velocity counters in Redis (§14.3).
//
// These are incremented as charges happen and read back in constant time. The
// alternative — counting rows in Postgres per request — would put an
// aggregate query on the synchronous charge path, which the sub-100ms budget
// cannot absorb and which gets slower exactly when traffic spikes.
//
// The tradeoff accepted here: Redis is not durable, so a flush loses counters
// and velocity rules go quiet until they refill. That is acceptable because
// these are a fraud *signal*, not a financial record — losing them briefly
// weakens detection but cannot lose money. Nothing in the ledger depends on
// them. If Redis is down entirely, Counters returns zeros and the velocity
// rules simply do not fire (see the fail-open reasoning in Service.Assess).

const (
	windowHour = time.Hour
	windowDay  = 24 * time.Hour
	windowWeek = 7 * 24 * time.Hour
)

// Counter reads and writes velocity state.
type Counter struct {
	rdb *redis.Client
}

func NewCounter(rdb *redis.Client) *Counter { return &Counter{rdb: rdb} }

// Keys are namespaced by window so each can carry its own TTL, and Redis
// expires them without a sweeper.
func chargeHourKey(fingerprint string) string  { return "vel:card:1h:" + fingerprint }
func chargeDayKey(fingerprint string) string   { return "vel:card:24h:" + fingerprint }
func declineHourKey(fingerprint string) string { return "vel:card:decl:1h:" + fingerprint }
func ipHourKey(ip string) string               { return "vel:ip:1h:" + ip }
func deviceCardsKey(device string) string      { return "vel:dev:cards:" + device }
func emailDayKey(email string) string          { return "vel:email:24h:" + email }

// Counters reads every velocity signal for one transaction in a single round
// trip.
//
// Pipelined rather than issued serially: six sequential round trips would cost
// six times the network latency, which is most of the risk budget on a remote
// Redis. Errors are swallowed and reported as zero — a scoring signal is never
// worth failing a payment for.
func (c *Counter) Counters(ctx context.Context, fingerprint, ip, device, email string) Velocity {
	if c == nil || c.rdb == nil {
		return Velocity{}
	}

	pipe := c.rdb.Pipeline()
	cardHour := pipe.Get(ctx, chargeHourKey(fingerprint))
	cardDay := pipe.Get(ctx, chargeDayKey(fingerprint))
	declines := pipe.Get(ctx, declineHourKey(fingerprint))
	ipHour := pipe.Get(ctx, ipHourKey(ip))
	devCards := pipe.SCard(ctx, deviceCardsKey(device))
	emailDay := pipe.Get(ctx, emailDayKey(email))

	// redis.Nil is returned per-command for missing keys and surfaces here as a
	// pipeline error. A missing key is the normal case for a first-time card,
	// so it is not treated as a failure — each result is read individually below.
	_, _ = pipe.Exec(ctx)

	return Velocity{
		CardChargesLastHour:  intOrZero(cardHour),
		CardChargesLastDay:   intOrZero(cardDay),
		CardDeclinesLastHour: intOrZero(declines),
		IPChargesLastHour:    intOrZero(ipHour),
		DeviceDistinctCards:  int(scardOrZero(devCards)),
		EmailChargesLastDay:  intOrZero(emailDay),
	}
}

// RecordCharge increments the counters after a charge is attempted.
//
// Called for every attempt, not only successes: a fraudster's declined
// attempts are the signal that matters most, and counting only successes would
// make card testing invisible to the very rule designed to catch it.
func (c *Counter) RecordCharge(ctx context.Context, fingerprint, ip, device, email string, declined bool) error {
	if c == nil || c.rdb == nil {
		return nil
	}

	pipe := c.rdb.Pipeline()

	// INCR then EXPIRE gives a rolling fixed window. A true sliding window
	// (sorted sets of timestamps) is more precise but costs memory per event
	// and more work per read; for thresholds like "more than 5 per hour" the
	// extra precision does not change any decision.
	incrWithTTL(ctx, pipe, chargeHourKey(fingerprint), windowHour)
	incrWithTTL(ctx, pipe, chargeDayKey(fingerprint), windowDay)

	if ip != "" {
		incrWithTTL(ctx, pipe, ipHourKey(ip), windowHour)
	}
	if email != "" {
		incrWithTTL(ctx, pipe, emailDayKey(email), windowDay)
	}
	if declined {
		incrWithTTL(ctx, pipe, declineHourKey(fingerprint), windowHour)
	}

	// A set, so re-using the same card from one device doesn't inflate the
	// count — what matters is how many DISTINCT cards a device has seen.
	if device != "" && fingerprint != "" {
		key := deviceCardsKey(device)
		pipe.SAdd(ctx, key, fingerprint)
		pipe.Expire(ctx, key, windowDay)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("risk: record velocity: %w", err)
	}
	return nil
}

// incrWithTTL increments a counter and sets its expiry only on creation.
//
// EXPIRE unconditionally would slide the window forward on every charge, so a
// steady trickle of traffic would keep an "hourly" counter alive indefinitely
// and it would never reset. NX applies the TTL only when the key is new.
func incrWithTTL(ctx context.Context, pipe redis.Pipeliner, key string, ttl time.Duration) {
	pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, ttl)
}

func intOrZero(cmd *redis.StringCmd) int {
	n, err := cmd.Int()
	if err != nil {
		return 0
	}
	return n
}

func scardOrZero(cmd *redis.IntCmd) int64 {
	n, err := cmd.Result()
	if err != nil {
		return 0
	}
	return n
}
