package payments

import "testing"

func TestDetectNetwork(t *testing.T) {
	cases := []struct {
		name   string
		number string
		want   Network
	}{
		{"visa", "4242424242424242", NetworkVisa},
		{"visa partial, one digit", "4", NetworkVisa},
		{"mastercard 5x range", "5555555555554444", NetworkMastercard},
		{"mastercard 2-series", "2223003122003222", NetworkMastercard},
		{"mastercard 2-series lower bound", "2221000000000009", NetworkMastercard},
		{"mastercard 2-series upper bound", "2720990000000008", NetworkMastercard},
		{"amex 34", "343434343434343", NetworkAmex},
		{"amex 37", "371449635398431", NetworkAmex},
		{"discover 6011", "6011111111111117", NetworkDiscover},
		{"discover 644-649", "6445644564456445", NetworkDiscover},
		{"rupay 508", "5081234567890123", NetworkRuPay},
		{"rupay 606", "6061234567890123", NetworkRuPay},
		{"unknown", "9999999999999999", NetworkUnknown},
		{"empty", "", NetworkUnknown},
		{"spaces are ignored", "4242 4242 4242 4242", NetworkVisa},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectNetwork(tc.number); got != tc.want {
				t.Errorf("DetectNetwork(%q) = %q, want %q", tc.number, got, tc.want)
			}
		})
	}
}

// RuPay and Discover overlap on the 65 prefix, so ordering in the rules table
// is load-bearing rather than incidental. This pins the behaviour so a future
// reordering cannot silently change it.
func TestDetectNetworkRuPayDiscoverOverlap(t *testing.T) {
	if got := DetectNetwork("6521123456789012"); got != NetworkRuPay {
		t.Errorf("652x should resolve to RuPay before Discover, got %q", got)
	}
	if got := DetectNetwork("6550123456789012"); got != NetworkDiscover {
		t.Errorf("655x should resolve to Discover, got %q", got)
	}
}

func TestLuhn(t *testing.T) {
	valid := []string{
		"4242424242424242", "5555555555554444", "371449635398431",
		"6011111111111117", "4000056655665556",
	}
	for _, n := range valid {
		if !Luhn(n) {
			t.Errorf("Luhn(%q) = false, want true", n)
		}
	}

	invalid := []string{
		"4242424242424243", // last digit wrong
		"4242424242424",    // too short
		"",
		"1234567890123456",
	}
	for _, n := range invalid {
		if Luhn(n) {
			t.Errorf("Luhn(%q) = true, want false", n)
		}
	}
}

// A transposition is the classic typo Luhn exists to catch.
func TestLuhnCatchesTransposition(t *testing.T) {
	if !Luhn("4000056655665556") {
		t.Fatal("control number should be valid")
	}
	if Luhn("4000056656565556") {
		t.Error("transposed digits should fail the checksum")
	}
}

func TestValidateNumber(t *testing.T) {
	if _, err := ValidateNumber("4242424242424242"); err != nil {
		t.Errorf("valid Visa rejected: %v", err)
	}
	if _, err := ValidateNumber("4242424242424243"); err != ErrInvalidCardNumber {
		t.Errorf("bad checksum: got %v, want ErrInvalidCardNumber", err)
	}
}

// Fingerprints must be stable across formatting and unique across cards —
// velocity rules and blocklists are only meaningful if both hold (§14.5).
func TestFingerprint(t *testing.T) {
	const salt = "test-salt"

	a := Fingerprint("4242424242424242", salt)
	b := Fingerprint("4242 4242 4242 4242", salt)
	if a != b {
		t.Error("formatting should not change the fingerprint")
	}

	if c := Fingerprint("4000056655665556", salt); a == c {
		t.Error("different cards must not share a fingerprint")
	}

	// A different salt must produce a different value, or rotating the salt
	// would not actually invalidate a leaked fingerprint table.
	if d := Fingerprint("4242424242424242", "other-salt"); a == d {
		t.Error("fingerprint must depend on the salt")
	}

	// The PAN must not be recoverable from, or visible in, the fingerprint.
	if len(a) < 20 {
		t.Errorf("fingerprint %q is suspiciously short", a)
	}
}

func TestLast4AndBIN(t *testing.T) {
	if got := Last4("4242 4242 4242 1234"); got != "1234" {
		t.Errorf("Last4 = %q, want 1234", got)
	}
	if got := BIN("424242 4242424242"); got != "424242" {
		t.Errorf("BIN = %q, want 424242", got)
	}
	if got := Last4("12"); got != "" {
		t.Errorf("Last4 of a too-short number = %q, want empty", got)
	}
}
