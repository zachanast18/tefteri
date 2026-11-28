package ocr

import (
	"testing"
	"time"
)

func TestParseGreekReceipt(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	sample := `ΑΠΟΔΕΙΞΗ ΛΙΑΝΙΚΗΣ
ΣΚΛΑΒΕΝΙΤΗΣ Α.Ε.
ΑΦΜ 094123456
ΗΜΕΡΟΜΗΝΙΑ: 18/03/2026 14:22
ΓΑΛΑ ΦΡΕΣΚΟ         1,85
ΨΩΜΙ ΧΩΡΙΑΤΙΚΟ      1,40
ΜΕΡΙΚΟ ΣΥΝΟΛΟ      35,80
ΦΠΑ 13%             4,20
ΤΕΛΙΚΟ ΣΥΝΟΛΟ      45,80
ΜΕΤΡΗΤΑ            50,00
ΡΕΣΤΑ               4,20`

	got := ParseReceipt(sample, loc)
	if got.Merchant != "ΣΚΛΑΒΕΝΙΤΗΣ Α.Ε." || got.CheckMerchant {
		t.Fatalf("merchant = %q (check=%v)", got.Merchant, got.CheckMerchant)
	}
	if got.Date != "2026-03-18" || got.CheckDate {
		t.Fatalf("date = %q (check=%v)", got.Date, got.CheckDate)
	}
	if got.AmountCents != 4580 || got.CheckAmount {
		t.Fatalf("amount = %d (check=%v)", got.AmountCents, got.CheckAmount)
	}
	if got.PaymentHint != "cash" {
		t.Fatalf("paymentHint = %q, want cash", got.PaymentHint)
	}
}

func TestParseEnglishReceipt(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	sample := `NORTHSIDE PHARMACY
Date: 05/04/2026
SUBTOTAL     1,100.00
VAT 24%        134.56
TOTAL DUE    1.234,56
VISA **** 4821`

	got := ParseReceipt(sample, loc)
	if got.Merchant != "NORTHSIDE PHARMACY" {
		t.Fatalf("merchant = %q", got.Merchant)
	}
	if got.Date != "2026-04-05" {
		t.Fatalf("date = %q", got.Date)
	}
	if got.AmountCents != 123456 || got.CheckAmount {
		t.Fatalf("amount = %d (check=%v)", got.AmountCents, got.CheckAmount)
	}
	if got.PaymentHint != "card" {
		t.Fatalf("paymentHint = %q, want card", got.PaymentHint)
	}
}

func TestParseAmbiguousReceipt(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	sample := `123456789
ITEM A 12,30
ITEM B 15,00`

	got := ParseReceipt(sample, loc)
	if !got.CheckMerchant {
		t.Fatalf("expected CheckMerchant=true")
	}
	if !got.CheckDate || got.Date != "" {
		t.Fatalf("expected CheckDate=true and empty date, got %q", got.Date)
	}
	if !got.CheckAmount {
		t.Fatalf("expected CheckAmount=true when no total label")
	}
}

func TestParseGreekSupermarketReceipt(t *testing.T) {
	sample := `Α-Β ΒΑΣΙΛΟΠΟΥΛΟΣ Μ.Α.Ε.
ΥΠΟΚ: ΑΚΡΟΠΟΛΗΣ
ΑΠΟΔΕΙΞΗ ΛΙΑΝΙΚΩΝ ΣΥΝΑΛΛΑΓΩΝ
ΠΡΟΪΟΝ 2,80 13,00%
ΣΥΝΟΛΟ
5,60 ΕΥΡΩ
ΣΥΝΟΛΟ ΕΙΔΩΝ *3*
ΡΕΣΤΑ 0,00
ΠΙΣΤ.ΚΑΡΤΕΣ EDPS 5,60
ΑΝΑΛΥΣΗ ΦΠΑ
13,00% 4,86 0,63
ΗΜΕΡ: 28/11/2026 ΩΡΑ: 11:28`
	got := ParseReceipt(sample, time.UTC)
	if got.Merchant != "Α-Β ΒΑΣΙΛΟΠΟΥΛΟΣ Μ.Α.Ε." || got.Date != "2026-11-28" ||
		got.AmountCents != 560 || got.CheckAmount || got.PaymentHint != "card" {
		t.Fatalf("unexpected receipt: %+v", got)
	}
}

func TestParseDiscountedReceipt(t *testing.T) {
	sample := `LUKUMADES STORES
Ωρα 28-11-2026 11:22
ΣΥΝΟΛΟ 6,40 €
ΕΚΠΤΩΣΗ 1,00 €
ΠΛΗΡΩΤΕΟ 5, 40 €
ΜΕΤΡΗΤΑ Ε 10,00 €
ΡΕΣΤΑ 4,60 €`
	got := ParseReceipt(sample, time.UTC)
	if got.AmountCents != 540 || got.CheckAmount || got.PaymentHint != "cash" {
		t.Fatalf("unexpected receipt: %+v", got)
	}
}
