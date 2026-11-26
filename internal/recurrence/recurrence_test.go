package recurrence

import (
	"testing"
	"time"
)

func TestMonthlyMonthEndAnchor(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		t.Fatal(err)
	}

	// Anchor day 31: Jan 31 -> Feb 28 (non-leap 2025) -> Mar 31 -> Apr 30 -> May 31
	d1, err := NextDate("2025-01-31", FreqMonthly, 31, 1, loc)
	if err != nil || d1 != "2025-02-28" {
		t.Fatalf("got %q, %v; want 2025-02-28", d1, err)
	}
	d2, err := NextDate(d1, FreqMonthly, 31, 1, loc)
	if err != nil || d2 != "2025-03-31" {
		t.Fatalf("got %q, %v; want 2025-03-31", d2, err)
	}
	d3, err := NextDate(d2, FreqMonthly, 31, 1, loc)
	if err != nil || d3 != "2025-04-30" {
		t.Fatalf("got %q, %v; want 2025-04-30", d3, err)
	}
	d4, err := NextDate(d3, FreqMonthly, 31, 1, loc)
	if err != nil || d4 != "2025-05-31" {
		t.Fatalf("got %q, %v; want 2025-05-31", d4, err)
	}

	// Leap year 2024: Jan 31 -> Feb 29 -> Mar 31
	leap, err := NextDate("2024-01-31", FreqMonthly, 31, 1, loc)
	if err != nil || leap != "2024-02-29" {
		t.Fatalf("got %q, %v; want 2024-02-29", leap, err)
	}
}

func TestYearlyLeapDayAnchor(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		t.Fatal(err)
	}

	cur := "2024-02-29"
	want := []string{"2025-02-28", "2026-02-28", "2027-02-28", "2028-02-29"}
	for _, w := range want {
		next, err := NextDate(cur, FreqYearly, 29, 2, loc)
		if err != nil {
			t.Fatal(err)
		}
		if next != w {
			t.Fatalf("from %s got %s, want %s", cur, next, w)
		}
		cur = next
	}
}
