package recurrence

import (
	"fmt"
	"time"
)

const (
	FreqMonthly = "monthly"
	FreqYearly  = "yearly"
)

// ParseDate parses a local calendar date YYYY-MM-DD in the given location.
func ParseDate(s string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc), nil
}

// NextDate computes the next scheduled renewal date after currentDate,
// preserving anchorDay (1..31) and anchorMonth (1..12 for yearly) in loc.
// If anchorDay does not exist in the target month (e.g. Jan 31 -> Feb, or Feb 29 -> non-leap year),
// it clamps to the last day of that month and returns to anchorDay in future months when possible.
func NextDate(currentDate string, freq string, anchorDay int, anchorMonth int, loc *time.Location) (string, error) {
	cur, err := ParseDate(currentDate, loc)
	if err != nil {
		return "", err
	}

	var targetYear int
	var targetMonth time.Month

	switch freq {
	case FreqMonthly:
		y, m, _ := cur.Date()
		targetMonth = m + 1
		targetYear = y
		if targetMonth > 12 {
			targetMonth = 1
			targetYear++
		}
	case FreqYearly:
		targetYear = cur.Year() + 1
		if anchorMonth >= 1 && anchorMonth <= 12 {
			targetMonth = time.Month(anchorMonth)
		} else {
			targetMonth = cur.Month()
		}
	default:
		return "", fmt.Errorf("unknown frequency: %s", freq)
	}

	day := anchorDay
	if day < 1 {
		day = cur.Day()
	}
	maxDay := daysInMonth(targetYear, targetMonth)
	if day > maxDay {
		day = maxDay
	}

	next := time.Date(targetYear, targetMonth, day, 0, 0, 0, 0, loc)
	return next.Format("2006-01-02"), nil
}

// Today returns today's YYYY-MM-DD local calendar date in loc.
func Today(now time.Time, loc *time.Location) string {
	return now.In(loc).Format("2006-01-02")
}

func daysInMonth(year int, month time.Month) int {
	// Day 0 of next month is the last day of target month.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
