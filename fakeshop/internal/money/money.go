// Package money holds amounts as integer minor units and renders them as
// the decimal strings used on the wire (spec: amounts are decimal strings).
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// Money is an amount in the smallest unit of its currency (yen, cents).
type Money struct {
	Minor    int64
	Currency string
}

// JSON is the wire form: {"amount": "25.00", "currency": "USD"}.
type JSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Decimals returns the number of fraction digits a currency is shown with.
func Decimals(currency string) int {
	switch currency {
	case "JPY":
		return 0
	default:
		return 2
	}
}

func New(minor int64, currency string) Money { return Money{Minor: minor, Currency: currency} }

// Parse reads a decimal string such as "25.00" or "3200".
func Parse(amount, currency string) (Money, error) {
	d := Decimals(currency)
	whole, frac, _ := strings.Cut(amount, ".")
	if whole == "" || len(frac) > d || strings.HasPrefix(whole, "-") {
		return Money{}, fmt.Errorf("invalid %s amount %q", currency, amount)
	}
	frac += strings.Repeat("0", d-len(frac))
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("invalid %s amount %q", currency, amount)
	}
	return Money{Minor: n, Currency: currency}, nil
}

// Amount renders the decimal string without the currency.
func (m Money) Amount() string {
	d := Decimals(m.Currency)
	s := strconv.FormatInt(m.Minor, 10)
	if d == 0 {
		return s
	}
	if len(s) <= d {
		s = strings.Repeat("0", d-len(s)+1) + s
	}
	return s[:len(s)-d] + "." + s[len(s)-d:]
}

func (m Money) Add(o Money) Money { return Money{Minor: m.Minor + o.Minor, Currency: m.Currency} }
func (m Money) Mul(n int) Money   { return Money{Minor: m.Minor * int64(n), Currency: m.Currency} }
func (m Money) JSON() JSON        { return JSON{Amount: m.Amount(), Currency: m.Currency} }
func (m Money) String() string    { return m.Amount() + " " + m.Currency }

// Display renders the amount for humans: "¥3,200" or "$25.00".
func (m Money) Display() string {
	whole, frac, _ := strings.Cut(m.Amount(), ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if frac != "" {
		b.WriteString("." + frac)
	}
	switch m.Currency {
	case "JPY":
		return "¥" + b.String()
	case "USD":
		return "$" + b.String()
	default:
		return b.String() + " " + m.Currency
	}
}
