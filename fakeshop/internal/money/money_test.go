package money

import "testing"

func TestParseAndFormat(t *testing.T) {
	cases := []struct {
		in, cur, amount, display string
		minor                    int64
	}{
		{"3200", "JPY", "3200", "¥3,200", 3200},
		{"25.00", "USD", "25.00", "$25.00", 2500},
		{"9.99", "USD", "9.99", "$9.99", 999},
		{"0.5", "USD", "0.50", "$0.50", 50},
		{"1234567", "JPY", "1234567", "¥1,234,567", 1234567},
	}
	for _, c := range cases {
		m, err := Parse(c.in, c.cur)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if m.Minor != c.minor || m.Amount() != c.amount || m.Display() != c.display {
			t.Errorf("Parse(%q) = %d %q %q", c.in, m.Minor, m.Amount(), m.Display())
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "-1", "1.234", "abc", ".5"} {
		if _, err := Parse(in, "USD"); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
	if _, err := Parse("1.5", "JPY"); err == nil {
		t.Error("JPY has no fraction digits")
	}
}
