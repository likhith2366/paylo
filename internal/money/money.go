// Package money represents monetary amounts as integer minor units.
//
// Floating point is never used for money anywhere in this system: 0.1 + 0.2 is
// not 0.3 in binary floating point, and a payments ledger that drifts by a cent
// is a correctness bug, not a rounding detail.
package money

import (
	"fmt"
	"strings"
)

// Amount is a value in a currency's minor unit (cents for USD, paise for INR).
type Amount struct {
	Cents    int64
	Currency string // ISO 4217, uppercase
}

// exponents holds currencies whose minor unit is not 1/100. Most currencies are
// two-decimal; these are the common exceptions. JPY and KRW have no minor unit
// at all, so "cents" and "units" are the same thing for them.
var exponents = map[string]int{
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3,
}

// Exponent returns the number of decimal places for a currency, defaulting to 2.
func Exponent(currency string) int {
	if e, ok := exponents[strings.ToUpper(currency)]; ok {
		return e
	}
	return 2
}

func New(cents int64, currency string) Amount {
	return Amount{Cents: cents, Currency: strings.ToUpper(currency)}
}

// Validate rejects amounts that must never reach the ledger.
func (a Amount) Validate() error {
	if len(a.Currency) != 3 {
		return fmt.Errorf("money: currency %q is not a 3-letter ISO 4217 code", a.Currency)
	}
	if a.Currency != strings.ToUpper(a.Currency) {
		return fmt.Errorf("money: currency %q must be uppercase", a.Currency)
	}
	if a.Cents <= 0 {
		return fmt.Errorf("money: amount must be positive, got %d", a.Cents)
	}
	return nil
}

// Add returns a+b, erroring on currency mismatch. Cross-currency arithmetic is
// always a bug: converting requires an explicit rate that must be recorded on
// the ledger entry (§20), so it can never happen implicitly here.
func (a Amount) Add(b Amount) (Amount, error) {
	if a.Currency != b.Currency {
		return Amount{}, fmt.Errorf("money: cannot add %s to %s", b.Currency, a.Currency)
	}
	return Amount{Cents: a.Cents + b.Cents, Currency: a.Currency}, nil
}

func (a Amount) Sub(b Amount) (Amount, error) {
	if a.Currency != b.Currency {
		return Amount{}, fmt.Errorf("money: cannot subtract %s from %s", b.Currency, a.Currency)
	}
	return Amount{Cents: a.Cents - b.Cents, Currency: a.Currency}, nil
}

// String renders the amount for logs and receipts, e.g. "12.34 USD", "500 JPY".
func (a Amount) String() string {
	exp := Exponent(a.Currency)
	if exp == 0 {
		return fmt.Sprintf("%d %s", a.Cents, a.Currency)
	}
	div := int64(1)
	for i := 0; i < exp; i++ {
		div *= 10
	}
	neg := ""
	c := a.Cents
	if c < 0 {
		neg, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%0*d %s", neg, c/div, exp, c%div, a.Currency)
}
