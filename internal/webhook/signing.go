// Package webhook delivers events to merchant endpoints (§7).
//
// The delivery contract is at-least-once, and that is a deliberate choice, not
// a limitation we failed to overcome. Exactly-once delivery to a third party we
// do not control is not achievable: if their server accepts a request and then
// dies before we read the response, we cannot distinguish that from a request
// they never received, and one of "retry" or "give up" is wrong. So we retry,
// and we give merchants an event_id to deduplicate on. Stripe publishes the
// same contract for the same reason.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the header merchants verify, modelled on Stripe-Signature.
const SignatureHeader = "PayFlow-Signature"

// Sign produces the signature header value for a payload.
//
// The timestamp is signed ALONGSIDE the body, not just alongside the request.
// Signing the body alone would let an attacker who captured one valid webhook
// replay it forever — the signature would still verify. Including the
// timestamp in the signed material means a replay can be detected by checking
// the timestamp's age, and the attacker cannot alter it without breaking the
// signature.
func Sign(payload []byte, secret string, timestamp time.Time) string {
	ts := timestamp.Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// Verify checks a signature header against a payload.
//
// Provided so the test suite's fake merchant server can verify exactly as a
// real merchant would — a signing scheme nobody has verified from the other
// side is a scheme that has not been tested.
func Verify(payload []byte, header, secret string, tolerance time.Duration) error {
	var timestamp int64
	var signatures []string

	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("webhook: malformed timestamp in signature")
			}
			timestamp = n
		case "v1":
			// Multiple v1 values appear during secret rotation, when both the
			// old and new secrets are signed with.
			signatures = append(signatures, value)
		}
	}

	if timestamp == 0 {
		return fmt.Errorf("webhook: signature has no timestamp")
	}
	if len(signatures) == 0 {
		return fmt.Errorf("webhook: signature has no v1 value")
	}

	// Reject anything too old to be a live delivery. Without this the
	// signature alone proves authenticity but not freshness, and a captured
	// webhook could be replayed indefinitely.
	age := time.Since(time.Unix(timestamp, 0))
	if age > tolerance || age < -tolerance {
		return fmt.Errorf("webhook: timestamp is outside the %s tolerance", tolerance)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(payload)
	expected := mac.Sum(nil)

	for _, candidate := range signatures {
		got, err := hex.DecodeString(candidate)
		if err != nil {
			continue
		}
		// Constant-time: a byte-by-byte comparison leaks how much of the
		// signature was correct, which is enough to forge one given enough
		// attempts.
		if hmac.Equal(got, expected) {
			return nil
		}
	}
	return fmt.Errorf("webhook: no signature matched")
}
