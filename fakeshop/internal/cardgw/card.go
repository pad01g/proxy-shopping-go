package cardgw

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// validateCard checks the form fields and returns the normalized number.
func validateCard(c Card, now time.Time) (string, error) {
	number := strings.NewReplacer(" ", "", "-", "").Replace(c.Number)
	if len(number) < 12 || len(number) > 19 || !allDigits(number) {
		return "", errors.New("card number is invalid")
	}
	if strings.TrimSpace(c.Name) == "" {
		return "", errors.New("cardholder name is required")
	}
	cvc := strings.TrimSpace(c.CVC)
	if (len(cvc) != 3 && len(cvc) != 4) || !allDigits(cvc) {
		return "", errors.New("security code is invalid")
	}
	if err := checkExpiry(c.Exp, now); err != nil {
		return "", err
	}
	return number, nil
}

// checkExpiry accepts "MM/YY" or "MM/YYYY"; the card is valid through the
// end of that month.
func checkExpiry(exp string, now time.Time) error {
	mm, yy, ok := strings.Cut(strings.TrimSpace(exp), "/")
	month, err1 := strconv.Atoi(strings.TrimSpace(mm))
	year, err2 := strconv.Atoi(strings.TrimSpace(yy))
	if !ok || err1 != nil || err2 != nil || month < 1 || month > 12 {
		return errors.New("expiry must be MM/YY")
	}
	if year < 100 {
		year += 2000
	}
	endOfMonth := time.Date(year, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC)
	if !now.Before(endOfMonth) {
		return errors.New("card has expired")
	}
	return nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
