package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Card network detection and validation (§2.3).
//
// This is fully deterministic, offline, and genuinely production-grade — the
// same logic Stripe Elements ships to browsers. It needs no API key and no
// network call, which is why the design doc calls out that it should be built
// for real rather than stubbed. The same rules are mirrored in the JS checkout
// widget so the card logo appears while the user is still typing.

type Network string

const (
	NetworkVisa       Network = "visa"
	NetworkMastercard Network = "mastercard"
	NetworkAmex       Network = "amex"
	NetworkDiscover   Network = "discover"
	NetworkRuPay      Network = "rupay"
	NetworkUnknown    Network = "unknown"
)

var (
	ErrInvalidCardNumber = errors.New("payments: card number failed Luhn check")
	ErrInvalidLength     = errors.New("payments: card number has an invalid length for its network")
)

// rule describes one network's IIN prefixes and permitted lengths.
type rule struct {
	network Network
	// ranges are inclusive numeric prefix ranges compared at equal digit width,
	// which handles both single prefixes (4) and spans (2221-2720) uniformly.
	ranges  [][2]int
	digits  int // prefix width the ranges are expressed in
	lengths []int
}

// Order matters: RuPay overlaps Discover on 65 and 6521-6529, so the more
// specific RuPay ranges are evaluated first. Real issuers resolve this with
// full BIN tables; for detection purposes the ordering is sufficient.
var rules = []rule{
	{NetworkAmex, [][2]int{{34, 34}, {37, 37}}, 2, []int{15}},
	{NetworkRuPay, [][2]int{{508, 508}, {606, 606}, {652, 653}, {817, 819}}, 3, []int{16}},
	{NetworkVisa, [][2]int{{4, 4}}, 1, []int{13, 16, 19}},
	{NetworkMastercard, [][2]int{{51, 55}}, 2, []int{16}},
	{NetworkMastercard, [][2]int{{2221, 2720}}, 4, []int{16}},
	{NetworkDiscover, [][2]int{{6011, 6011}, {6221, 6229}, {6440, 6499}, {6500, 6599}}, 4, []int{16, 19}},
}

// DetectNetwork identifies the card network from as few digits as are needed.
// It works on partial input, which is what lets the checkout widget light up
// the network logo before the user finishes typing.
func DetectNetwork(number string) Network {
	digits := onlyDigits(number)
	if digits == "" {
		return NetworkUnknown
	}
	for _, r := range rules {
		if len(digits) < r.digits {
			continue
		}
		prefix := parseInt(digits[:r.digits])
		for _, span := range r.ranges {
			if prefix >= span[0] && prefix <= span[1] {
				return r.network
			}
		}
	}
	return NetworkUnknown
}

// Luhn validates the mod-10 checksum. It catches transposed and mistyped
// digits; it says nothing about whether the card exists or has funds.
func Luhn(number string) bool {
	digits := onlyDigits(number)
	if len(digits) < 12 {
		return false
	}
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// ValidateNumber checks the checksum and that the length suits the network.
func ValidateNumber(number string) (Network, error) {
	digits := onlyDigits(number)
	network := DetectNetwork(digits)

	if !Luhn(digits) {
		return network, ErrInvalidCardNumber
	}
	if network != NetworkUnknown && !lengthValid(network, len(digits)) {
		return network, ErrInvalidLength
	}
	return network, nil
}

func lengthValid(network Network, length int) bool {
	for _, r := range rules {
		if r.network != network {
			continue
		}
		for _, l := range r.lengths {
			if l == length {
				return true
			}
		}
	}
	return false
}

// Fingerprint derives a stable, non-reversible identifier for a card.
//
// HMAC rather than a bare hash: the card number space is small enough to
// enumerate (a plain SHA-256 of a 16-digit PAN is brute-forceable in minutes),
// so the secret salt is what actually prevents recovering the PAN from a
// leaked fingerprint table. This value is what velocity rules and blocklists
// key on, and it never lets us reconstruct the card (§14.5).
func Fingerprint(number, salt string) string {
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(onlyDigits(number)))
	return "card_" + hex.EncodeToString(mac.Sum(nil))[:32]
}

// Last4 returns the final four digits, the only part of a PAN safe to store
// and display (§13).
func Last4(number string) string {
	digits := onlyDigits(number)
	if len(digits) < 4 {
		return ""
	}
	return digits[len(digits)-4:]
}

// BIN returns the first six digits, used for issuer risk lists (§14.2).
func BIN(number string) string {
	digits := onlyDigits(number)
	if len(digits) < 6 {
		return ""
	}
	return digits[:6]
}

func onlyDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func parseInt(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
