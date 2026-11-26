package money

import "testing"

func TestParsePositive(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"45,80", 4580, false},
		{"45.80", 4580, false},
		{"1.234,56", 123456, false},
		{"1,234.56", 123456, false},
		{"1.234,56 €", 123456, false},
		{"EUR 99,5", 9950, false},
		{"100", 10000, false},
		{"1.234", 123400, false},
		{"0,50", 50, false},
		{",50", 50, false},
		{"0", 0, true},
		{"-12,00", 0, true},
		{"abc", 0, true},
		{"12,345", 1234500, false}, // 3 digits after single comma -> thousands
		{"12,3456", 0, true},
		{"12,3,4", 0, true},
		{"1.2.3", 0, true},
		{"1,23,456", 0, true},
		{"1.234,56,78", 0, true},
		{"1,234.56.78", 0, true},
		{"1234,567", 0, true},
		{"1,,2", 0, true},
		{"1.234.567,89", 123456789, false},
		{"1,234,567.89", 123456789, false},
		{"99999999999999999,00", 0, true},
		{"92233720368547758,08", 0, true},
	}

	for _, tc := range tests {
		got, err := ParsePositive(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParsePositive(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("ParsePositive(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormat(t *testing.T) {
	if got := Format(123456, "€"); got != "1.234,56 €" {
		t.Fatalf("got %q, want %q", got, "1.234,56 €")
	}
	if got := Format(-500, "€"); got != "-5,00 €" {
		t.Fatalf("got %q, want %q", got, "-5,00 €")
	}
	if got := FormatInput(123456); got != "1234,56" {
		t.Fatalf("got %q, want %q", got, "1234,56")
	}
}
