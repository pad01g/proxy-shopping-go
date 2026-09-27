// Package amount converts between the decimal strings of the protocol and exact numbers.
package amount

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var decimalRe = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// Parse reads a non-negative decimal string exactly.
func Parse(s string) (*big.Rat, error) {
	if !decimalRe.MatchString(s) {
		return nil, fmt.Errorf("%q is not a decimal amount", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal amount", s)
	}
	return r, nil
}

// ParseInt reads an integer amount in base units.
func ParseInt(s string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.Sign() < 0 || !decimalRe.MatchString(s) || strings.Contains(s, ".") {
		return nil, fmt.Errorf("%q is not an integer amount", s)
	}
	return n, nil
}

// Decimals of the currencies and assets the protocol prices in.
func Decimals(currency string) int {
	switch currency {
	case "JPY", "KRW":
		return 0
	case "BTC":
		return 8
	case "USDC":
		return 6
	}
	return 2
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// Ceil rounds a non-negative rational up to an integer.
func Ceil(r *big.Rat) *big.Int {
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// Floor rounds a non-negative rational down to an integer.
func Floor(r *big.Rat) *big.Int { return new(big.Int).Quo(r.Num(), r.Denom()) }

// CeilTo rounds up to the given number of decimals.
func CeilTo(r *big.Rat, decimals int) *big.Rat {
	scale := new(big.Rat).SetInt(pow10(decimals))
	n := Ceil(new(big.Rat).Mul(r, scale))
	return new(big.Rat).Quo(new(big.Rat).SetInt(n), scale)
}

// Format writes r with exactly the given decimals (r must already be at that precision, otherwise it is rounded
// half up by big.Rat.FloatString).
func Format(r *big.Rat, decimals int) string { return r.FloatString(decimals) }

// FormatCurrency writes a fiat amount with its usual decimals.
func FormatCurrency(r *big.Rat, currency string) string { return Format(r, Decimals(currency)) }

// ToUnits converts a decimal amount of an asset (e.g. "0.50" USDC) to base units, rounding up.
func ToUnits(r *big.Rat, decimals int) *big.Int {
	return Ceil(new(big.Rat).Mul(r, new(big.Rat).SetInt(pow10(decimals))))
}

// BPS returns floor(v × bps / 10000).
func BPS(v *big.Int, bps int64) *big.Int {
	x := new(big.Int).Mul(v, big.NewInt(bps))
	return x.Quo(x, big.NewInt(10000))
}

// Max returns the larger integer.
func Max(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

// LockAmount implements §4.5: ceil(total / rate × 10^decimals) + reserve.
func LockAmount(total, rate *big.Rat, decimals int, reserve int64) (*big.Int, error) {
	if rate.Sign() <= 0 {
		return nil, fmt.Errorf("rate must be positive")
	}
	units := ToUnits(new(big.Rat).Quo(total, rate), decimals)
	return units.Add(units, big.NewInt(reserve)), nil
}
