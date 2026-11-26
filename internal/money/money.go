package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

var (
	ErrInvalidAmount = errors.New("Enter the amount paid.")
	ErrNonPositive   = errors.New("Enter the amount paid.")
)

// ParsePositive parses a user-entered or OCR-extracted monetary string into
// positive integer minor units (cents). It supports European comma decimals
// ("1.234,56", "45,80") as well as dot decimals ("1,234.56", "45.80").
func ParsePositive(raw string) (int64, error) {
	cents, err := Parse(raw)
	if err != nil {
		return 0, err
	}
	if cents <= 0 {
		return 0, ErrNonPositive
	}
	return cents, nil
}

// Parse parses a monetary string into integer minor units (cents).
func Parse(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, ErrInvalidAmount
	}

	// Strip common currency markers and whitespace.
	replacer := strings.NewReplacer(
		"€", "", "$", "", "£", "",
		"EUR", "", "eur", "", "USD", "", "usd", "", "GBP", "", "gbp", "",
		"ΕΥΡΩ", "", "ευρω", "",
		"\u00a0", "", " ", "",
	)
	s = replacer.Replace(s)
	if s == "" {
		return 0, ErrInvalidAmount
	}

	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = strings.TrimPrefix(s, "-")
	} else if strings.HasPrefix(s, "+") {
		s = strings.TrimPrefix(s, "+")
	}

	if s == "" {
		return 0, ErrInvalidAmount
	}

	// Validate characters: only digits, '.', ','
	for _, r := range s {
		if !unicode.IsDigit(r) && r != '.' && r != ',' {
			return 0, ErrInvalidAmount
		}
	}

	lastComma := strings.LastIndexByte(s, ',')
	lastDot := strings.LastIndexByte(s, '.')

	var intPart, fracPart string

	switch {
	case lastComma >= 0 && lastDot >= 0:
		// Both separators present: whichever appears last is the decimal separator.
		if lastComma > lastDot {
			var ok bool
			intPart, ok = validateGroupedInt(s[:lastComma], ".")
			if !ok {
				return 0, ErrInvalidAmount
			}
			fracPart = s[lastComma+1:]
		} else {
			var ok bool
			intPart, ok = validateGroupedInt(s[:lastDot], ",")
			if !ok {
				return 0, ErrInvalidAmount
			}
			fracPart = s[lastDot+1:]
		}
	case lastComma >= 0:
		// Only comma(s).
		if strings.Count(s, ",") > 1 {
			var ok bool
			intPart, ok = validateGroupedInt(s, ",")
			if !ok {
				return 0, ErrInvalidAmount
			}
		} else {
			after := s[lastComma+1:]
			before := s[:lastComma]
			if len(after) == 3 && len(before) >= 1 {
				var ok bool
				intPart, ok = validateGroupedInt(s, ",")
				if !ok {
					return 0, ErrInvalidAmount
				}
			} else {
				intPart = before
				fracPart = after
			}
		}
	case lastDot >= 0:
		// Only dot(s).
		if strings.Count(s, ".") > 1 {
			var ok bool
			intPart, ok = validateGroupedInt(s, ".")
			if !ok {
				return 0, ErrInvalidAmount
			}
		} else {
			after := s[lastDot+1:]
			before := s[:lastDot]
			if len(after) == 3 && len(before) >= 1 {
				var ok bool
				intPart, ok = validateGroupedInt(s, ".")
				if !ok {
					return 0, ErrInvalidAmount
				}
			} else {
				intPart = before
				fracPart = after
			}
		}
	default:
		intPart = s
	}

	if intPart == "" && fracPart == "" {
		return 0, ErrInvalidAmount
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > 2 {
		return 0, ErrInvalidAmount
	}
	for _, r := range fracPart {
		if !unicode.IsDigit(r) {
			return 0, ErrInvalidAmount
		}
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}

	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, ErrInvalidAmount
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, ErrInvalidAmount
	}
	if whole > (math.MaxInt64-frac)/100 {
		return 0, ErrInvalidAmount
	}

	total := whole*100 + frac
	if neg {
		total = -total
	}
	return total, nil
}

// validateGroupedInt checks that raw is either digits only or valid 3-digit
// thousands groups separated by sep (e.g. "1.234.567"), returning the
// ungrouped digit string.
func validateGroupedInt(raw, sep string) (string, bool) {
	if raw == "" {
		return "", true
	}
	if !strings.Contains(raw, sep) {
		for _, r := range raw {
			if !unicode.IsDigit(r) {
				return "", false
			}
		}
		return raw, true
	}
	parts := strings.Split(raw, sep)
	if len(parts[0]) < 1 || len(parts[0]) > 3 {
		return "", false
	}
	for _, r := range parts[0] {
		if !unicode.IsDigit(r) {
			return "", false
		}
	}
	for _, p := range parts[1:] {
		if len(p) != 3 {
			return "", false
		}
		for _, r := range p {
			if !unicode.IsDigit(r) {
				return "", false
			}
		}
	}
	return strings.Join(parts, ""), true
}

// Format formats minor units using Greek/European grouping and comma decimal,
// e.g. 123456 -> "1.234,56 €", -500 -> "-5,00 €".
func Format(cents int64, symbol string) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	whole := cents / 100
	frac := cents % 100

	wholeStr := strconv.FormatInt(whole, 10)
	grouped := groupThousands(wholeStr, '.')
	out := fmt.Sprintf("%s,%02d", grouped, frac)
	if neg {
		out = "-" + out
	}
	if symbol != "" {
		out += " " + symbol
	}
	return out
}

// FormatInput formats minor units for editable text inputs without thousands separators,
// e.g. 123456 -> "1234,56".
func FormatInput(cents int64) string {
	if cents < 0 {
		cents = -cents
	}
	return fmt.Sprintf("%d,%02d", cents/100, cents%100)
}

// FormatCSV formats minor units as standard decimal with dot for CSV portability,
// e.g. 123456 -> "1234.56", -500 -> "-5.00".
func FormatCSV(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	if neg {
		return "-" + s
	}
	return s
}

func groupThousands(digits string, sep byte) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b strings.Builder
	rem := n % 3
	if rem > 0 {
		b.WriteString(digits[:rem])
		if n > rem {
			b.WriteByte(sep)
		}
	}
	for i := rem; i < n; i += 3 {
		if i > rem {
			b.WriteByte(sep)
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}
