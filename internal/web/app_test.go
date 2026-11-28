package web

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"

	"spending-project/internal/auth"
	"spending-project/internal/config"
	"spending-project/internal/db"
	"spending-project/internal/ocr"
)

func newTestApp(t *testing.T, tesseractBin string) (*Server, *db.Store, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	hash, err := auth.HashPassword("secret1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertUser(context.Background(), "household", hash); err != nil {
		t.Fatal(err)
	}

	loc, _ := time.LoadLocation("Europe/Athens")
	cfg := config.Config{
		ListenAddr:      ":0",
		DBPath:          dbPath,
		Currency:        "EUR",
		CurrencySymbol:  "€",
		Timezone:        "Europe/Athens",
		Location:        loc,
		OCRLanguages:    "ell+eng",
		OCRTimeout:      5 * time.Second,
		MaxUploadBytes:         12 * 1024 * 1024,
		MaxImageDim:            6000,
		MaxStoredDim:           1600,
		MaxHouseholdQuotaBytes: 250 * 1024 * 1024,
		SessionTTL:             24 * time.Hour,
		SessionIdleTTL:         24 * time.Hour,
		CookieSecure:           "false",
		WebAuthnRPID:           "localhost",
		WebAuthnRPOrigin:       "http://localhost",
		RegistrationMode:       "invite_only",
		TesseractBin:           tesseractBin,
		TesseractThread:        1,
	}

	scanner := ocr.NewScanner(tesseractBin, cfg.OCRLanguages, cfg.OCRTimeout, 1)
	srv, err := NewServer(cfg, store, scanner)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.nowFn = func() time.Time {
		return time.Date(2026, 3, 18, 12, 0, 0, 0, loc)
	}
	return srv, store, tmpDir
}

func makeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 245, G: 245, B: 240, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func extractCSRF(t *testing.T, body string) string {
	t.Helper()
	re := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatalf("csrf_token not found in body")
	}
	return m[1]
}

func extractSubToken(t *testing.T, body string) string {
	t.Helper()
	re := regexp.MustCompile(`name="submission_token" value="([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatalf("submission_token not found in body")
	}
	return m[1]
}

func loginClient(t *testing.T, ts *httptest.Server) *http.Client {
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

	form := url.Values{
		"csrf_token": {csrf},
		"username":   {"household"},
		"password":   {"secret1234"},
	}
	resp2, err := client.PostForm(ts.URL+"/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp2.StatusCode)
	}
	return client
}

func TestOCRFailureManualCompletionAndBackupRestore(t *testing.T) {
	// Use a nonexistent tesseract binary to force OCR failure.
	srv, store, tmpDir := newTestApp(t, "nonexistent-tesseract-bin")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Unauthenticated request should redirect to /login.
	noAuthClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	unauthResp, err := noAuthClient.Get(ts.URL + "/ledger")
	if err != nil {
		t.Fatal(err)
	}
	unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect for unauth, got %d", unauthResp.StatusCode)
	}

	client := loginClient(t, ts)

	// Get CSRF from /scan.
	scanGet, err := client.Get(ts.URL + "/scan")
	if err != nil {
		t.Fatal(err)
	}
	scanHTML, _ := io.ReadAll(scanGet.Body)
	scanGet.Body.Close()
	csrf := extractCSRF(t, string(scanHTML))

	// Upload receipt image -> OCR fails -> redirects to draft review with photo attached.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("csrf_token", csrf)
	fw, _ := mw.CreateFormFile("receipt", "receipt.jpg")
	const receiptURL = "https://example.gr/receipts/123"
	qr, err := qrcode.NewQRCodeWriter().Encode(receiptURL, gozxing.BarcodeFormat_QR_CODE, 180, 180, nil)
	if err != nil {
		t.Fatal(err)
	}
	photo := image.NewGray(image.Rect(0, 0, qr.GetWidth(), qr.GetHeight()))
	for y := range qr.GetHeight() {
		for x := range qr.GetWidth() {
			if !qr.Get(x, y) {
				photo.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	if err := jpeg.Encode(fw, photo, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/scan", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	scanResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	draftBodyBytes, _ := io.ReadAll(scanResp.Body)
	scanResp.Body.Close()
	draftBody := string(draftBodyBytes)

	if !strings.Contains(draftBody, "Couldn&#39;t read the receipt. Enter the details below.") && !strings.Contains(draftBody, "Couldn't read the receipt. Enter the details below.") {
		t.Fatalf("expected OCR fallback notice in draft review")
	}
	if !strings.Contains(draftBody, "/receipts/") {
		t.Fatalf("expected attached receipt image in draft review")
	}
	if !strings.Contains(draftBody, `href="`+receiptURL+`"`) {
		t.Fatal("expected QR link in draft review even when OCR fails")
	}

	// Extract draft_id, receipt_image_id, submission_token.
	draftID := regexp.MustCompile(`name="draft_id" value="([^"]+)"`).FindStringSubmatch(draftBody)[1]
	recID := regexp.MustCompile(`name="receipt_image_id" value="([^"]+)"`).FindStringSubmatch(draftBody)[1]
	subTok := extractSubToken(t, draftBody)
	groc, _ := store.CategoryByName(context.Background(), 1, "Groceries")

	// Save expense manually from the failed OCR draft.
	var saveBody bytes.Buffer
	sw := multipart.NewWriter(&saveBody)
	_ = sw.WriteField("csrf_token", csrf)
	_ = sw.WriteField("submission_token", subTok)
	_ = sw.WriteField("draft_id", draftID)
	_ = sw.WriteField("receipt_image_id", recID)
	_ = sw.WriteField("amount", "45,80")
	_ = sw.WriteField("merchant", "ΣΚΛΑΒΕΝΙΤΗΣ Α.Ε.")
	_ = sw.WriteField("purchase_date", "2026-03-18")
	_ = sw.WriteField("payment_method", "cash")
	_ = sw.WriteField("category_id", strconv.FormatInt(groc.ID, 10))
	_ = sw.WriteField("kind", "expense")
	_ = sw.WriteField("remember_merchant", "1")
	sw.Close()

	saveReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/expenses", &saveBody)
	saveReq.Header.Set("Content-Type", sw.FormDataContentType())
	saveResp, err := client.Do(saveReq)
	if err != nil {
		t.Fatal(err)
	}
	saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("save expense status = %d", saveResp.StatusCode)
	}

	// Verify duplicate submission with same submission_token does not create a second expense.
	var dupSubBody bytes.Buffer
	dw := multipart.NewWriter(&dupSubBody)
	_ = dw.WriteField("csrf_token", csrf)
	_ = dw.WriteField("submission_token", subTok)
	_ = dw.WriteField("amount", "45,80")
	_ = dw.WriteField("merchant", "ΣΚΛΑΒΕΝΙΤΗΣ Α.Ε.")
	_ = dw.WriteField("purchase_date", "2026-03-18")
	_ = dw.WriteField("payment_method", "cash")
	_ = dw.WriteField("category_id", strconv.FormatInt(groc.ID, 10))
	dw.Close()
	dupReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/expenses", &dupSubBody)
	dupReq.Header.Set("Content-Type", dw.FormDataContentType())
	dupResp, err := client.Do(dupReq)
	if err != nil {
		t.Fatal(err)
	}
	dupResp.Body.Close()

	exps, err := store.ListExpenses(context.Background(), db.ExpenseFilter{HouseholdID: 1})
	if err != nil || len(exps) != 1 {
		t.Fatalf("expected 1 expense after duplicate submission, got %d (err=%v)", len(exps), err)
	}
	if err := store.RotateHouseholdDEK(context.Background(), 1); err != nil {
		t.Fatalf("rotate receipt key: %v", err)
	}
	detail, err := client.Get(fmt.Sprintf("%s/expenses/%d", ts.URL, exps[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	detailBody, err := io.ReadAll(detail.Body)
	detail.Body.Close()
	if err != nil || detail.StatusCode != http.StatusOK || !bytes.Contains(detailBody, []byte(`href="`+receiptURL+`"`)) {
		t.Fatalf("expected saved QR link after key rotation, got status %d, err %v", detail.StatusCode, err)
	}

	// Verify remembered merchant category suggestion works for "Σκλαβενίτης".
	sugCat, reason, err := store.SuggestCategory(context.Background(), 1, "Σκλαβενίτης", "")
	if err != nil || sugCat.ID != groc.ID || reason != "remembered" {
		t.Fatalf("expected remembered Groceries, got %+v (%s, %v)", sugCat, reason, err)
	}

	// Verify live backup via VACUUM INTO and restore into independent DB.
	backupPath := filepath.Join(tmpDir, "backup.db")
	if err := store.Backup(context.Background(), backupPath); err != nil {
		t.Fatalf("backup failed: %v", err)
	}
	restored, err := db.OpenWithKeys(backupPath, store.Keys)
	if err != nil {
		t.Fatalf("open restored backup: %v", err)
	}
	defer restored.Close()

	restoredExps, err := restored.ListExpenses(context.Background(), db.ExpenseFilter{HouseholdID: 1})
	if err != nil || len(restoredExps) != 1 {
		t.Fatalf("restored expenses = %d, want 1", len(restoredExps))
	}
	if restoredExps[0].ReceiptImageID == nil {
		t.Fatalf("expected receipt image id in restored expense")
	}
	img, err := restored.GetReceiptImage(context.Background(), 1, *restoredExps[0].ReceiptImageID)
	if err != nil || len(img.Data) == 0 {
		t.Fatalf("expected receipt image bytes in restored backup, err=%v", err)
	}
	if link, err := restored.GetReceiptURL(context.Background(), 1, img.ID); err != nil || link != receiptURL {
		t.Fatalf("restored QR link = %q, err = %v", link, err)
	}
}

func TestDashboardTotalsAndRefunds(t *testing.T) {
	_, store, _ := newTestApp(t, "tesseract")
	ctx := context.Background()
	loc, _ := time.LoadLocation("Europe/Athens")

	groc, _ := store.CategoryByName(ctx, 1, "Groceries")
	din, _ := store.CategoryByName(ctx, 1, "Dining")

	_, err := store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID: 1, Kind: "expense", AmountCents: 4580, Merchant: "ΣΚΛΑΒΕΝΙΤΗΣ",
		PurchaseDate: "2026-03-10", PaymentMethod: "cash", CategoryID: groc.ID, Source: "receipt",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID: 1, Kind: "expense", AmountCents: 123456, Merchant: "ΠΛΑΙΣΙΟ",
		PurchaseDate: "2026-03-12", PaymentMethod: "card", CategoryID: din.ID, Source: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID: 1, Kind: "refund", AmountCents: 1000, Merchant: "ΠΛΑΙΣΙΟ",
		PurchaseDate: "2026-03-14", PaymentMethod: "card", CategoryID: din.ID, Source: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}

	sum, err := store.GetDashboardSummary(ctx, 1, "2026-03-01", "2026-03-31", "2026-03", loc)
	if err != nil {
		t.Fatal(err)
	}
	// Total = 4580 + 123456 - 1000 = 127036
	if sum.TotalCents != 127036 {
		t.Fatalf("TotalCents = %d, want 127036", sum.TotalCents)
	}
	if sum.CashCents != 4580 {
		t.Fatalf("CashCents = %d, want 4580", sum.CashCents)
	}
	if sum.CardCents != 122456 {
		t.Fatalf("CardCents = %d, want 122456", sum.CardCents)
	}
}

func TestRepeatedSubscriptionConfirmationAndDeletedExpense(t *testing.T) {
	_, store, _ := newTestApp(t, "tesseract")
	ctx := context.Background()
	loc, _ := time.LoadLocation("Europe/Athens")
	util, _ := store.CategoryByName(ctx, 1, "Utilities")

	subID, err := store.CreateSubscription(ctx, db.SaveSubscriptionInput{
		HouseholdID:      1,
		Name:             "Cosmote Fiber",
		AmountCents:      3290,
		Frequency:        "monthly",
		CategoryID:       util.ID,
		PaymentMethod:    "card",
		FirstRenewalDate: "2026-03-01",
		NextDueDate:      "2026-03-01",
		Status:           "active",
	}, loc)
	if err != nil {
		t.Fatal(err)
	}

	expID, err := store.ConfirmRenewal(ctx, 1, subID, "2026-03-01", 3290, "2026-03-01", loc)
	if err != nil {
		t.Fatalf("first confirm failed: %v", err)
	}

	// Second confirmation for the same scheduled_date must be rejected safely.
	if _, err := store.ConfirmRenewal(ctx, 1, subID, "2026-03-01", 3290, "2026-03-01", loc); err != db.ErrAlreadyConfirmed {
		t.Fatalf("expected ErrAlreadyConfirmed, got %v", err)
	}

	sub, err := store.GetSubscription(ctx, 1, subID, "2026-03-18")
	if err != nil {
		t.Fatal(err)
	}
	if sub.NextDueDate != "2026-04-01" {
		t.Fatalf("NextDueDate = %s, want 2026-04-01", sub.NextDueDate)
	}

	// Delete the linked expense and verify next_due_date does not revert to 2026-03-01
	// and re-confirming 2026-03-01 is still blocked.
	if err := store.DeleteExpense(ctx, 1, expID); err != nil {
		t.Fatal(err)
	}
	subAfter, err := store.GetSubscription(ctx, 1, subID, "2026-03-18")
	if err != nil {
		t.Fatal(err)
	}
	if subAfter.NextDueDate != "2026-04-01" {
		t.Fatalf("NextDueDate after delete = %s, want 2026-04-01", subAfter.NextDueDate)
	}
	if _, err := store.ConfirmRenewal(ctx, 1, subID, "2026-03-01", 3290, "2026-03-01", loc); err != db.ErrAlreadyConfirmed {
		t.Fatalf("expected ErrAlreadyConfirmed after expense delete, got %v", err)
	}
}

func TestMultiUserCreationAttributionAndFilter(t *testing.T) {
	srv, store, _ := newTestApp(t, "tesseract")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client1 := loginClient(t, ts)

	// Create second household member via /users.
	usersGet, err := client1.Get(ts.URL + "/users")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(usersGet.Body)
	usersGet.Body.Close()
	csrf := extractCSRF(t, string(b))

	resp, err := client1.PostForm(ts.URL+"/users", url.Values{
		"csrf_token":       {csrf},
		"username":         {"maria"},
		"display_name":     {"Maria"},
		"password":         {"mariapass123"},
		"confirm_password": {"mariapass123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create user status = %d", resp.StatusCode)
	}

	// Sign in as Maria and add an expense.
	jar2, _ := cookiejar.New(nil)
	client2 := &http.Client{Jar: jar2}
	l2, _ := client2.Get(ts.URL + "/login")
	lb, _ := io.ReadAll(l2.Body)
	l2.Body.Close()
	lcsrf := extractCSRF(t, string(lb))
	lr, _ := client2.PostForm(ts.URL+"/login", url.Values{
		"csrf_token": {lcsrf},
		"username":   {"maria"},
		"password":   {"mariapass123"},
	})
	lr.Body.Close()

	newGet, _ := client2.Get(ts.URL + "/expenses/new")
	nb, _ := io.ReadAll(newGet.Body)
	newGet.Body.Close()
	nc := extractCSRF(t, string(nb))
	st := extractSubToken(t, string(nb))
	groc, _ := store.CategoryByName(context.Background(), 1, "Groceries")

	var expBody bytes.Buffer
	mw := multipart.NewWriter(&expBody)
	_ = mw.WriteField("csrf_token", nc)
	_ = mw.WriteField("submission_token", st)
	_ = mw.WriteField("amount", "22,50")
	_ = mw.WriteField("merchant", "ΑΒ ΒΑΣΙΛΟΠΟΥΛΟΣ")
	_ = mw.WriteField("purchase_date", "2026-03-18")
	_ = mw.WriteField("payment_method", "card")
	_ = mw.WriteField("category_id", strconv.FormatInt(groc.ID, 10))
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/expenses", &expBody)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	er, err := client2.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	er.Body.Close()

	maria, err := store.GetUserByUsername(context.Background(), "maria")
	if err != nil {
		t.Fatal(err)
	}
	exps, err := store.ListExpenses(context.Background(), db.ExpenseFilter{HouseholdID: 1, UserID: maria.ID})
	if err != nil || len(exps) != 1 {
		t.Fatalf("expected 1 expense for Maria, got %d (err=%v)", len(exps), err)
	}
	if exps[0].CreatedByName != "Maria" {
		t.Fatalf("CreatedByName = %q, want Maria", exps[0].CreatedByName)
	}

	// Create a second household and verify ledger isolation + invite join.
	hh2ID, err := store.CreateHousehold(context.Background(), "Vacation House", maria.ID)
	if err != nil {
		t.Fatal(err)
	}
	hh2Exps, err := store.ListExpenses(context.Background(), db.ExpenseFilter{HouseholdID: hh2ID})
	if err != nil || len(hh2Exps) != 0 {
		t.Fatalf("expected 0 expenses in new household, got %d", len(hh2Exps))
	}

	// Invite household1 user to Vacation House.
	if err := store.CreateInvite(context.Background(), hh2ID, maria.ID, "INVITE123", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	u1, _ := store.GetUserByUsername(context.Background(), "household")
	joinedHH, err := store.AcceptInvite(context.Background(), "INVITE123", u1.ID, time.Now())
	if err != nil || joinedHH != hh2ID {
		t.Fatalf("AcceptInvite = %d, %v; want %d", joinedHH, err, hh2ID)
	}
	// Reusing same invite code must fail.
	if _, err := store.AcceptInvite(context.Background(), "INVITE123", u1.ID, time.Now()); err != db.ErrInvalidInvite {
		t.Fatalf("expected ErrInvalidInvite on reuse, got %v", err)
	}
}

func loginAs(t *testing.T, ts *httptest.Server, username, password string) *http.Client {
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

	r2, err := client.PostForm(ts.URL+"/login", url.Values{
		"csrf_token": {csrf},
		"username":   {username},
		"password":   {password},
	})
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("login %s status = %d", username, r2.StatusCode)
	}
	return client
}

func TestHouseholdIsolation(t *testing.T) {
	srv, store, _ := newTestApp(t, "nonexistent-tesseract-bin")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	h, _ := auth.HashPassword("pass12345")
	// User "household" is in Household 1 (created by newTestApp).
	// Create user "onlyB" in Household B, and add "household" (shared) to Household B too.
	u1, _ := store.GetUserByUsername(ctx, "household")
	uB, _ := store.CreateUser(ctx, "onlyB", "Only B", h)
	hhB, err := store.CreateHousehold(ctx, "Household B", uB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddHouseholdMember(ctx, hhB, u1.ID, "member"); err != nil {
		t.Fatal(err)
	}
	// Also create "onlyA" in Household 1 only.
	if _, err := store.CreateAndAddHouseholdMember(ctx, 1, "onlyA", "Only A", h, "member"); err != nil {
		t.Fatal(err)
	}

	// onlyB has a receipt, saved expense + remembered rule, and a second draft in inbox.
	imgIDB, _ := store.SaveReceiptImage(ctx, hhB, "deadbeef", "image/jpeg", 100, 100, makeTestJPEG(t, 100, 100))
	catB, _ := store.CategoryByName(ctx, hhB, "Groceries")
	expBID, err := store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID:      hhB,
		Kind:             "expense",
		AmountCents:      8800,
		Merchant:         "ΜΥΚΟΝΟΣ ΜΑΡΚΕΤ",
		PurchaseDate:     "2026-03-18",
		PaymentMethod:    "card",
		CategoryID:       catB.ID,
		Source:           "receipt",
		ReceiptImageID:   &imgIDB,
		CreatedByUserID:  &uB,
		RememberMerchant: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.CreateDraft(ctx, db.ReceiptDraft{
		ID:             "draft-b-1",
		HouseholdID:    hhB,
		ReceiptImageID: imgIDB,
		Merchant:       "DRAFT B",
		NeedsReview:    true,
		ReviewReason:   "Uncertain total",
	})

	// onlyA (only in Household 1) must NOT be able to open Household B's receipt image, expense, or CSV.
	clientA := loginAs(t, ts, "onlyA", "pass12345")
	recResp, _ := clientA.Get(fmt.Sprintf("%s/receipts/%d", ts.URL, imgIDB))
	recResp.Body.Close()
	if recResp.StatusCode != http.StatusNotFound {
		t.Fatalf("onlyA accessing Household B receipt got %d, want 404", recResp.StatusCode)
	}
	expResp, _ := clientA.Get(fmt.Sprintf("%s/expenses/%d", ts.URL, expBID))
	expResp.Body.Close()
	if expResp.StatusCode != http.StatusNotFound {
		t.Fatalf("onlyA accessing Household B expense got %d, want 404", expResp.StatusCode)
	}
	csvResp, _ := clientA.Get(ts.URL + "/ledger/export.csv")
	csvBytes, _ := io.ReadAll(csvResp.Body)
	csvResp.Body.Close()
	if strings.Contains(string(csvBytes), "ΜΥΚΟΝΟΣ ΜΑΡΚΕΤ") {
		t.Fatalf("onlyA CSV export leaked Household B expense")
	}

	// Shared user starts on Household 1: sees empty ledger/inbox, no remembered rule from B.
	sharedClient := loginAs(t, ts, "household", "secret1234")
	ov1, _ := sharedClient.Get(ts.URL + "/")
	ov1Body, _ := io.ReadAll(ov1.Body)
	ov1.Body.Close()
	if strings.Contains(string(ov1Body), "ΜΥΚΟΝΟΣ ΜΑΡΚΕΤ") {
		t.Fatalf("shared user on Household 1 saw Household B expense")
	}
	csrf := extractCSRF(t, string(ov1Body))

	// Switch shared user to Household B -> sees expense, inbox draft, and receipt image!
	swResp, _ := sharedClient.PostForm(ts.URL+"/households/switch", url.Values{
		"csrf_token":   {csrf},
		"household_id": {strconv.FormatInt(hhB, 10)},
	})
	swBody, _ := io.ReadAll(swResp.Body)
	swResp.Body.Close()
	if !strings.Contains(string(swBody), "ΜΥΚΟΝΟΣ ΜΑΡΚΕΤ") {
		t.Fatalf("shared user after switching to Household B did not see Household B expense")
	}
	inboxResp, _ := sharedClient.Get(ts.URL + "/inbox")
	inboxBody, _ := io.ReadAll(inboxResp.Body)
	inboxResp.Body.Close()
	if !strings.Contains(string(inboxBody), "DRAFT B") {
		t.Fatalf("shared user on Household B did not see Household B review draft")
	}
}

func TestSplitsSettlementsBudgetsAndInbox(t *testing.T) {
	srv, store, _ := newTestApp(t, "nonexistent-tesseract-bin")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	h, _ := auth.HashPassword("pass12345")
	u1, _ := store.GetUserByUsername(ctx, "household")
	u2, err := store.CreateAndAddHouseholdMember(ctx, 1, "partner", "Partner", h, "member")
	if err != nil {
		t.Fatal(err)
	}
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")

	// 1. u1 adds 45,80 EUR expense paid by u2, split equally between u1 and u2 (22,90 each).
	eqSplits := db.ComputeEqualSplits(4580, []int64{u1.ID, u2})
	if _, err := store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID:     1,
		Kind:            "expense",
		AmountCents:     4580,
		Merchant:        "ΣΚΛΑΒΕΝΙΤΗΣ",
		PurchaseDate:    "2026-03-18",
		PaymentMethod:   "card",
		CategoryID:      groc.ID,
		Source:          "manual",
		CreatedByUserID: &u1.ID,
		PaidByUserID:    &u2,
		Splits:          eqSplits,
	}); err != nil {
		t.Fatal(err)
	}

	// 2. u1 adds 30,00 EUR expense paid by u1, custom split 10,00 (u1) / 20,00 (u2).
	if _, err := store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID:     1,
		Kind:            "expense",
		AmountCents:     3000,
		Merchant:        "ΑΒ ΒΑΣΙΛΟΠΟΥΛΟΣ",
		PurchaseDate:    "2026-03-18",
		PaymentMethod:   "cash",
		CategoryID:      groc.ID,
		Source:          "manual",
		CreatedByUserID: &u1.ID,
		PaidByUserID:    &u1.ID,
		Splits: []db.ExpenseSplit{
			{UserID: u1.ID, ShareCents: 1000},
			{UserID: u2, ShareCents: 2000},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Balances before settlement:
	// u1: -2290 + (3000 - 1000) = -290
	// u2: (4580 - 2290) - 2000 = +290
	bals, err := store.ComputeHouseholdBalances(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bals[0].BalanceCents != -290 || bals[1].BalanceCents != 290 {
		t.Fatalf("balances = %+v, want -290 and +290", bals)
	}

	// 3. Record settlement: u1 pays u2 2,90 EUR -> both balances 0, total spending still 75,80 EUR.
	if _, err := store.RecordSettlement(ctx, 1, u1.ID, u2, 290, "2026-03-18", "IRIS", &u1.ID); err != nil {
		t.Fatal(err)
	}
	balsAfter, _ := store.ComputeHouseholdBalances(ctx, 1)
	if balsAfter[0].BalanceCents != 0 || balsAfter[1].BalanceCents != 0 {
		t.Fatalf("balances after settlement = %+v, want 0", balsAfter)
	}

	loc, _ := time.LoadLocation("Europe/Athens")
	sum, _ := store.GetDashboardSummary(ctx, 1, "2026-03-01", "2026-03-31", "2026-03", loc)
	if sum.TotalCents != 7580 {
		t.Fatalf("spending total = %d, want 7580 (settlements must not change spending)", sum.TotalCents)
	}

	// 4. Category budget: 100,00 EUR on Groceries -> 24,20 EUR left.
	if err := store.SetCategoryBudget(ctx, 1, groc.ID, 10000); err != nil {
		t.Fatal(err)
	}
	client := loginAs(t, ts, "household", "secret1234")
	ov, _ := client.Get(ts.URL + "/")
	ovBody, _ := io.ReadAll(ov.Body)
	ov.Body.Close()
	if !strings.Contains(string(ovBody), "24,20 € left of 100,00 €") {
		t.Fatalf("expected budget remaining text on overview")
	}
}
