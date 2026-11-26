package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"spending-project/internal/auth"
	"spending-project/internal/config"
	"spending-project/internal/crypto"
	"spending-project/internal/db"
	"spending-project/internal/ocr"
)

func reviewLoginAs(t *testing.T, ts *httptest.Server, username, password string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractCSRF(t, string(b))
	resp2, err := client.PostForm(ts.URL+"/login", url.Values{
		"csrf_token": {csrf},
		"username":   {username},
		"password":   {password},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("login(%s) status = %d", username, resp2.StatusCode)
	}
	return client
}

func fetchPageCSRF(t *testing.T, client *http.Client, pageURL string) string {
	t.Helper()
	resp, err := client.Get(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return extractCSRF(t, string(b))
}

func postMultipart(client *http.Client, targetURL string, fields map[string]string) (*http.Response, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, targetURL, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return client.Do(req)
}

func setupReviewTestServer(t *testing.T, now time.Time) (*Server, *db.Store, *httptest.Server, int64, int64) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "review.db")
	k1, _ := crypto.GenerateKey()
	kr := &crypto.KeyRing{CurrentVersion: 1, Keys: map[int][]byte{1: k1}}
	store, err := db.OpenWithKeys(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	h1, _ := auth.HashPassword("alexpass123")
	if err := store.UpsertUser(ctx, "alex", h1); err != nil {
		t.Fatal(err)
	}
	alex, err := store.GetUserByUsername(ctx, "alex")
	if err != nil {
		t.Fatal(err)
	}

	h2, _ := auth.HashPassword("mariapass123")
	mariaID, err := store.CreateAndAddHouseholdMember(ctx, 1, "maria", "Maria", h2, "member")
	if err != nil {
		t.Fatal(err)
	}

	loc, _ := time.LoadLocation("Europe/Athens")
	cfg := config.Config{
		ListenAddr:             ":0",
		DBPath:                 dbPath,
		Currency:               "EUR",
		CurrencySymbol:         "€",
		Timezone:               "Europe/Athens",
		Location:               loc,
		OCRLanguages:           "ell+eng",
		OCRTimeout:             5 * time.Second,
		MaxUploadBytes:         12 * 1024 * 1024,
		MaxImageDim:            6000,
		MaxStoredDim:           1600,
		MaxHouseholdQuotaBytes: 50 * 1024 * 1024,
		SessionTTL:             24 * time.Hour,
		SessionIdleTTL:         24 * time.Hour,
		CookieSecure:           "false",
		WebAuthnRPID:           "localhost",
		WebAuthnRPOrigin:       "http://localhost",
		RegistrationMode:       "invite_only",
	}
	srv, err := NewServer(cfg, store, ocr.NewScanner("tesseract", "ell+eng", 5*time.Second, 1))
	if err != nil {
		t.Fatal(err)
	}
	srv.nowFn = func() time.Time { return now }
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, store, ts, alex.ID, mariaID
}

func TestSubscriptionEditPreservesAnchorDay(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	_, store, _, _, _ := setupReviewTestServer(t, time.Date(2026, 2, 15, 10, 0, 0, 0, loc))
	ctx := context.Background()
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")

	// 1. Monthly Jan 31 -> Feb 28 -> price edit -> March 31.
	subID, err := store.CreateSubscription(ctx, db.SaveSubscriptionInput{
		HouseholdID:      1,
		Name:             "Monthly Gym",
		AmountCents:      3000,
		Frequency:        "monthly",
		CategoryID:       groc.ID,
		PaymentMethod:    "card",
		FirstRenewalDate: "2026-01-31",
		NextDueDate:      "2026-01-31",
		Status:           "active",
	}, loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmRenewal(ctx, 1, subID, "2026-01-31", 3000, "2026-01-31", loc); err != nil {
		t.Fatal(err)
	}
	sub, err := store.GetSubscription(ctx, 1, subID, "")
	if err != nil || sub.NextDueDate != "2026-02-28" {
		t.Fatalf("after Jan 31 renewal got NextDueDate=%q err=%v", sub.NextDueDate, err)
	}

	// Edit price only in February; keep frequency & next_due_date unchanged.
	if err := store.UpdateSubscription(ctx, 1, subID, db.SaveSubscriptionInput{
		HouseholdID:   1,
		Name:          "Monthly Gym",
		AmountCents:   3500,
		Frequency:     "monthly",
		CategoryID:    groc.ID,
		PaymentMethod: "card",
		NextDueDate:   "2026-02-28",
		Status:        "active",
	}, loc); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmRenewal(ctx, 1, subID, "2026-02-28", 3500, "2026-02-28", loc); err != nil {
		t.Fatal(err)
	}
	sub, _ = store.GetSubscription(ctx, 1, subID, "")
	if sub.NextDueDate != "2026-03-31" {
		t.Fatalf("after price edit in Feb expected 2026-03-31, got %q", sub.NextDueDate)
	}

	// 2. Yearly Feb 29 -> 2025-02-28 -> price edit -> 2028-02-29.
	leapID, err := store.CreateSubscription(ctx, db.SaveSubscriptionInput{
		HouseholdID:      1,
		Name:             "Leap Domain",
		AmountCents:      1200,
		Frequency:        "yearly",
		CategoryID:       groc.ID,
		PaymentMethod:    "card",
		FirstRenewalDate: "2024-02-29",
		NextDueDate:      "2024-02-29",
		Status:           "active",
	}, loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmRenewal(ctx, 1, leapID, "2024-02-29", 1200, "2024-02-29", loc); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSubscription(ctx, 1, leapID, db.SaveSubscriptionInput{
		HouseholdID:   1,
		Name:          "Leap Domain",
		AmountCents:   1500,
		Frequency:     "yearly",
		CategoryID:    groc.ID,
		PaymentMethod: "card",
		NextDueDate:   "2025-02-28",
		Status:        "active",
	}, loc); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"2025-02-28", "2026-02-28", "2027-02-28"} {
		if _, err := store.ConfirmRenewal(ctx, 1, leapID, d, 1500, d, loc); err != nil {
			t.Fatal(err)
		}
	}
	leapSub, _ := store.GetSubscription(ctx, 1, leapID, "")
	if leapSub.NextDueDate != "2028-02-29" {
		t.Fatalf("expected leap year return to 2028-02-29, got %q", leapSub.NextDueDate)
	}
}

func TestDraftInboxRoundTripAndStorageError(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	_, store, ts, alexID, mariaID := setupReviewTestServer(t, time.Date(2026, 3, 18, 12, 0, 0, 0, loc))
	ctx := context.Background()
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")

	client := reviewLoginAs(t, ts, "alex", "alexpass123")
	csrf := fetchPageCSRF(t, client, ts.URL+"/expenses/new")

	imgID, err := store.SaveReceiptImage(ctx, 1, "draftsha256", "image/jpeg", 100, 100, []byte("jpeg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDraft(ctx, db.ReceiptDraft{
		ID:             "draft-rt-1",
		HouseholdID:    1,
		ReceiptImageID: imgID,
		Merchant:       "ΑΒ ΒΑΣΙΛΟΠΟΥΛΟΣ",
		OCRStatus:      "ok",
	}); err != nil {
		t.Fatal(err)
	}

	// Save partially completed refund with another payer, note, and custom split.
	resp, err := postMultipart(client, ts.URL+"/expenses", map[string]string{
		"csrf_token":                               csrf,
		"household_id":                             "1",
		"draft_id":                                 "draft-rt-1",
		"save_action":                              "draft",
		"kind":                                     "refund",
		"paid_by_user_id":                          strconv.FormatInt(mariaID, 10),
		"merchant":                                 "ΑΒ ΒΑΣΙΛΟΠΟΥΛΟΣ",
		"purchase_date":                            "2026-03-18",
		"amount":                                   "12,3,4",
		"payment_method":                           "card",
		"category_id":                              strconv.FormatInt(groc.ID, 10),
		"note":                                     "Damaged milk bottle",
		"split_mode":                               "custom",
		fmt.Sprintf("split_amount_%d", alexID):     "5,00",
		fmt.Sprintf("split_amount_%d", mariaID):    "7,00",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/inbox" {
		t.Fatalf("draft save landed on %s (status %d), want /inbox (200)", resp.Request.URL.Path, resp.StatusCode)
	}

	// Reopen draft and verify all fields round-trip.
	getResp, err := client.Get(ts.URL + "/expenses/new?draft=draft-rt-1")
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	html := string(bodyBytes)
	for _, want := range []string{
		`value="refund" checked`,
		fmt.Sprintf(`value="%d" selected`, mariaID),
		`Damaged milk bottle`,
		`value="12,3,4"`,
		`value="custom" checked`,
		`value="5,00"`,
		`value="7,00"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("reopened draft missing %q", want)
		}
	}

	// Force storage error (missing draft ID) and verify error is shown & values preserved.
	errResp, err := postMultipart(client, ts.URL+"/expenses", map[string]string{
		"csrf_token":      csrf,
		"household_id":    "1",
		"draft_id":        "missing-draft-id",
		"save_action":     "draft",
		"kind":            "refund",
		"paid_by_user_id": strconv.FormatInt(mariaID, 10),
		"merchant":        "KEEP_ME_MERCHANT",
		"purchase_date":   "2026-03-18",
		"amount":          "19,50",
		"payment_method":  "cash",
		"category_id":     strconv.FormatInt(groc.ID, 10),
		"note":            "KEEP_ME_NOTE",
	})
	if err != nil {
		t.Fatal(err)
	}
	errBodyBytes, _ := io.ReadAll(errResp.Body)
	errResp.Body.Close()
	errHTML := string(errBodyBytes)
	if errResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 on failed draft write, got %d", errResp.StatusCode)
	}
	if strings.Contains(errHTML, "Saved to Needs review inbox") {
		t.Fatalf("must not claim saved on failed write")
	}
	if !strings.Contains(errHTML, "Could not save draft to inbox") || !strings.Contains(errHTML, "KEEP_ME_MERCHANT") || !strings.Contains(errHTML, "KEEP_ME_NOTE") {
		t.Fatalf("failed draft write did not preserve recoverable form state")
	}
}

func TestLedgerPaidByFilterUIAndCSV(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	_, store, ts, alexID, mariaID := setupReviewTestServer(t, time.Date(2026, 3, 18, 12, 0, 0, 0, loc))
	ctx := context.Background()
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")

	// Maria scans/creates expense, but Alex paid.
	if _, err := store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID:     1,
		Kind:            "expense",
		AmountCents:     2450,
		Merchant:        "ALEX_PAID_MARIA_SCANNED",
		PurchaseDate:    "2026-03-18",
		PaymentMethod:   "card",
		CategoryID:      groc.ID,
		Source:          "manual",
		CreatedByUserID: &mariaID,
		PaidByUserID:    &alexID,
	}); err != nil {
		t.Fatal(err)
	}

	client := reviewLoginAs(t, ts, "alex", "alexpass123")

	for _, path := range []string{
		fmt.Sprintf("/ledger?user=%d", alexID),
		fmt.Sprintf("/ledger/export.csv?user=%d", alexID),
	} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), "ALEX_PAID_MARIA_SCANNED") {
			t.Fatalf("%s omitted expense paid by Alex", path)
		}
	}

	for _, path := range []string{
		fmt.Sprintf("/ledger?user=%d", mariaID),
		fmt.Sprintf("/ledger/export.csv?user=%d", mariaID),
	} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), "ALEX_PAID_MARIA_SCANNED") {
			t.Fatalf("%s unexpectedly included expense paid by Alex when filtering by Maria", path)
		}
	}
}

func TestOverviewBudgetsAndRenewalReminders(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	// March 28, 2026: day before Europe/Athens spring DST transition (March 29, 23h day).
	_, store, ts, alexID, _ := setupReviewTestServer(t, time.Date(2026, 3, 28, 10, 0, 0, 0, loc))
	ctx := context.Background()
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")

	if err := store.SetCategoryBudget(ctx, 1, groc.ID, 20000); err != nil {
		t.Fatal(err)
	}
	for _, dt := range []string{"2026-01-15", "2026-02-15"} {
		if _, err := store.SaveExpense(ctx, db.SaveExpenseInput{
			HouseholdID:     1,
			Kind:            "expense",
			AmountCents:     15000,
			Merchant:        "Supermarket",
			PurchaseDate:    dt,
			PaymentMethod:   "card",
			CategoryID:      groc.ID,
			Source:          "manual",
			CreatedByUserID: &alexID,
			PaidByUserID:    &alexID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Create 6 active subscriptions due within 7 days, including one on DST spring-forward day (2026-03-29).
	for i := 0; i < 6; i++ {
		due := time.Date(2026, 3, 29+i, 0, 0, 0, 0, loc).Format("2006-01-02")
		if _, err := store.CreateSubscription(ctx, db.SaveSubscriptionInput{
			HouseholdID:      1,
			Name:             fmt.Sprintf("Sub %d", i+1),
			AmountCents:      1000,
			Frequency:        "monthly",
			CategoryID:       groc.ID,
			PaymentMethod:    "card",
			FirstRenewalDate: due,
			NextDueDate:      due,
			Status:           "active",
		}, loc); err != nil {
			t.Fatal(err)
		}
	}

	client := reviewLoginAs(t, ts, "alex", "alexpass123")

	// 1. Two-month custom range must not show monthly "Over by".
	r2m, err := client.Get(ts.URL + "/?from=2026-01-01&to=2026-02-28")
	if err != nil {
		t.Fatal(err)
	}
	b2m, _ := io.ReadAll(r2m.Body)
	r2m.Body.Close()
	if strings.Contains(string(b2m), "Over by") {
		t.Fatalf("two-month custom range showed monthly budget overage")
	}

	// 2. Single full month with zero spend still shows budgeted Groceries + 6 renewals + DST "Due in 1d".
	rMar, err := client.Get(ts.URL + "/?month=2026-03")
	if err != nil {
		t.Fatal(err)
	}
	bMar, _ := io.ReadAll(rMar.Body)
	rMar.Body.Close()
	marHTML := string(bMar)
	if !strings.Contains(marHTML, "200,00 € left of 200,00 €") {
		t.Fatalf("expected zero-spend budgeted category in March overview")
	}
	if !strings.Contains(marHTML, "6 subscription renewal(s)") {
		t.Fatalf("expected 6 subscription renewal(s) count in banner")
	}
	if !strings.Contains(marHTML, "Due in 1d") || strings.Contains(marHTML, "Due today") {
		t.Fatalf("expected DST civil date 'Due in 1d' (not 'Due today') on 2026-03-28 -> 2026-03-29")
	}
}

func TestLedgerPaginationAndReturnPreservation(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	_, store, ts, alexID, _ := setupReviewTestServer(t, time.Date(2026, 3, 18, 12, 0, 0, 0, loc))
	ctx := context.Background()
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")

	for i := 1; i <= 120; i++ {
		if _, err := store.SaveExpense(ctx, db.SaveExpenseInput{
			HouseholdID:     1,
			Kind:            "expense",
			AmountCents:     1000,
			Merchant:        fmt.Sprintf("Vendor %03d", i),
			PurchaseDate:    "2026-03-18",
			PaymentMethod:   "card",
			CategoryID:      groc.ID,
			Source:          "manual",
			CreatedByUserID: &alexID,
			PaidByUserID:    &alexID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	client := reviewLoginAs(t, ts, "alex", "alexpass123")
	resp, err := client.Get(ts.URL + "/ledger?payment=card&page=2")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(b)
	if !strings.Contains(html, "of <strong class=\"num\">120</strong> entries") {
		t.Fatalf("expected 120 total matching entries on page 2")
	}
	if !strings.Contains(html, "1.200,00 €") {
		t.Fatalf("expected full filtered net total 1.200,00 € on page 2")
	}
	if !strings.Contains(html, "Page 2 of 3") {
		t.Fatalf("expected Page 2 of 3 indicator")
	}
	wantReturn := url.QueryEscape("/ledger?payment=card&page=2")
	if !strings.Contains(html, "return="+wantReturn) {
		t.Fatalf("expected preserved return query %q in detail links", wantReturn)
	}
}

func BenchmarkLedger10k(b *testing.B) {
	loc, _ := time.LoadLocation("Europe/Athens")
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "bench.db")
	k1, _ := crypto.GenerateKey()
	kr := &crypto.KeyRing{CurrentVersion: 1, Keys: map[int][]byte{1: k1}}
	store, err := db.OpenWithKeys(dbPath, kr)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	h, _ := auth.HashPassword("benchpass123")
	_ = store.UpsertUser(ctx, "bench", h)
	u, _ := store.GetUserByUsername(ctx, "bench")
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")

	for i := 0; i < 10000; i++ {
		if _, err := store.SaveExpense(ctx, db.SaveExpenseInput{
			HouseholdID:     1,
			Kind:            "expense",
			AmountCents:     1050,
			Merchant:        fmt.Sprintf("Merchant %05d", i),
			PurchaseDate:    "2026-03-18",
			PaymentMethod:   "card",
			CategoryID:      groc.ID,
			Source:          "manual",
			CreatedByUserID: &u.ID,
			PaidByUserID:    &u.ID,
		}); err != nil {
			b.Fatal(err)
		}
	}
	_ = loc

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		items, count, net, err := store.ListExpensesPage(ctx, db.ExpenseFilter{HouseholdID: 1}, 1, 50)
		if err != nil || len(items) != 50 || count != 10000 || net != 10500000 {
			b.Fatalf("unexpected page result: len=%d count=%d net=%d err=%v", len(items), count, net, err)
		}
	}
}
