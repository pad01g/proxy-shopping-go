package amount

import (
	"math/big"
	"testing"
)

func rat(s string) *big.Rat {
	r, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return r
}

func TestLockAmount(t *testing.T) {
	// spec example: 12000 + 800 + 640 JPY at 15000000 JPY/BTC = 0.000896 BTC = 89600 sats, plus 1000 reserve
	n, err := LockAmount(rat("13440"), rat("15000000"), 8, 1000)
	if err != nil || n.String() != "90600" {
		t.Fatalf("%v %v", n, err)
	}
	// rounding up: 1 JPY at 15000000 → 6.67 sats → 7
	n, _ = LockAmount(rat("1"), rat("15000000"), 8, 0)
	if n.String() != "7" {
		t.Fatal(n)
	}
	// USDC: 45.00 USD at 1 → 45000000 units
	n, _ = LockAmount(rat("45.00"), rat("1"), 6, 0)
	if n.String() != "45000000" {
		t.Fatal(n)
	}
	if _, err := LockAmount(rat("1"), new(big.Rat), 8, 0); err == nil {
		t.Fatal("zero rate")
	}
}

func TestParseFormat(t *testing.T) {
	for _, bad := range []string{"", "-1", "1e5", "01", "1.", ".5", "1/2"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if _, err := ParseInt("1.5"); err == nil {
		t.Error("ParseInt accepted a fraction")
	}
	if got := FormatCurrency(CeilTo(rat("12.341"), 2), "USD"); got != "12.35" {
		t.Error(got)
	}
	if got := FormatCurrency(CeilTo(rat("640.2"), 0), "JPY"); got != "641" {
		t.Error(got)
	}
	if got := ToUnits(rat("0.50"), 6); got.String() != "500000" {
		t.Error(got)
	}
	if got := BPS(big.NewInt(90600), 50); got.String() != "453" {
		t.Error(got)
	}
	if Floor(rat("2.9")).String() != "2" || Ceil(rat("2.1")).String() != "3" || Ceil(rat("2")).String() != "2" {
		t.Error("floor/ceil")
	}
}
