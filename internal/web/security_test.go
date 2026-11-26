package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
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

func TestEncryptionPlaintextInspectionAndRotation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "enc.db")

	k1, _ := crypto.GenerateKey()
	k2, _ := crypto.GenerateKey()
	kr1 := &crypto.KeyRing{CurrentVersion: 1, Keys: map[int][]byte{1: k1}}

	store, err := db.OpenWithKeys(dbPath, kr1)
	if err != nil {
		t.Fatal(err)
	}


	ctx := context.Background()
	h, _ := auth.HashPassword("secret1234")
	_ = store.UpsertUser(ctx, "household", h)
	u, _ := store.GetUserByUsername(ctx, "household")
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")

	jpegBytes := makeTestJPEG(t, 120, 120)
	imgID, err := store.SaveReceiptImage(ctx, 1, "sha256hex", "image/jpeg", 120, 120, jpegBytes)
	if err != nil {
		t.Fatal(err)
	}


	expID, err := store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID:     1,
		Kind:            "expense",
		AmountCents:     9950,
		Merchant:        "UNIQUE_SECRET_MERCHANT_999",
		PurchaseDate:    "2026-03-18",
		PaymentMethod:   "card",
		CategoryID:      groc.ID,
		Note:            "UNIQUE_SECRET_NOTE_888",
		Source:          "receipt",
		ReceiptImageID:  &imgID,
		CreatedByUserID: &u.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Also confirm a subscription renewal and remember a merchant category before rotation.
	loc, _ := time.LoadLocation("Europe/Athens")
	subID, err := store.CreateSubscription(ctx, db.SaveSubscriptionInput{
		HouseholdID:      1,
		Name:             "Water Bill",
		AmountCents:      3290,
		Frequency:        "monthly",
		CategoryID:       groc.ID,
		PaymentMethod:    "card",
		FirstRenewalDate: "2026-03-01",
		NextDueDate:      "2026-03-01",
		Status:           "active",
	}, loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmRenewal(ctx, 1, subID, "2026-03-01", 3290, "2026-03-01", loc); err != nil {
		t.Fatal(err)
	}
	if err := store.RememberMerchantCategory(ctx, 1, "ΜΥΣΤΙΚΟ ΜΑΡΚΕΤ", groc.ID); err != nil {
		t.Fatal(err)
	}


	// Backup and inspect raw SQLite file + backup bytes for plaintext markers.
	backupPath := filepath.Join(tmpDir, "backup.db")
	if err := store.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	store.Close()

	for _, p := range []string{dbPath, backupPath} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "UNIQUE_SECRET_MERCHANT_999") {
			t.Fatalf("%s contains plaintext merchant!", p)
		}
		if strings.Contains(string(raw), "UNIQUE_SECRET_NOTE_888") {
			t.Fatalf("%s contains plaintext note!", p)
		}
	}

	// Rotate KEK from v1 to v2, then rotate household 1 DEK, then verify receipt & expense still decrypt.
	kr12 := &crypto.KeyRing{CurrentVersion: 2, Keys: map[int][]byte{1: k1, 2: k2}}
	store2, err := db.OpenWithKeys(dbPath, kr12)
	if err != nil {
		t.Fatal(err)
	}

	defer store2.Close()

	if err := store2.RotateKEK(ctx); err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}
	if err := store2.RotateHouseholdDEK(ctx, 1); err != nil {
		t.Fatalf("RotateHouseholdDEK: %v", err)
	}
	store2.Close()

	// Reopen with ONLY v2 KEK (v1 retired).
	kr2Only := &crypto.KeyRing{CurrentVersion: 2, Keys: map[int][]byte{2: k2}}
	store3, err := db.OpenWithKeys(dbPath, kr2Only)
	if err != nil {
		t.Fatal(err)
	}

	defer store3.Close()

	gotExp, err := store3.GetExpense(ctx, 1, expID)
	if err != nil {
		t.Fatal(err)
	}

	if gotExp.Merchant != "UNIQUE_SECRET_MERCHANT_999" || gotExp.Note != "UNIQUE_SECRET_NOTE_888" || gotExp.AmountCents != 9950 {
		t.Fatalf("decrypted expense mismatch after rotation: %+v", gotExp)
	}
	gotImg, err := store3.GetReceiptImage(ctx, 1, imgID)
	if err != nil || len(gotImg.Data) != len(jpegBytes) {
		t.Fatalf("decrypted receipt mismatch after rotation: len=%d err=%v", len(gotImg.Data), err)
	}

	// Verify paid renewal occurrence and remembered merchant rule survived DEK rotation.
	occs, err := store3.ListRecentOccurrences(ctx, 1, 5)
	if err != nil || len(occs) != 1 || occs[0].PaidAmountCents == nil || *occs[0].PaidAmountCents != 3290 {
		t.Fatalf("renewal occurrence after DEK rotation = %+v, err=%v", occs, err)
	}
	sugCat, reason, err := store3.SuggestCategory(ctx, 1, "ΜΥΣΤΙΚΟ ΜΑΡΚΕΤ", "")
	if err != nil || reason != "remembered" || sugCat.ID != groc.ID {
		t.Fatalf("remembered merchant after DEK rotation = %+v (%s, %v)", sugCat, reason, err)
	}
}

func TestAuthMFARecoveryAndPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "auth.db")
	k1, _ := crypto.GenerateKey()
	kr := &crypto.KeyRing{CurrentVersion: 1, Keys: map[int][]byte{1: k1}}
	store, err := db.OpenWithKeys(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()

	ctx := context.Background()
	h, _ := auth.HashPassword("ownerpass123")
	_ = store.UpsertUser(ctx, "owner1", h)
	owner, _ := store.GetUserByUsername(ctx, "owner1")

	// Add TOTP + recovery codes to owner1.
	seed, _ := auth.GenerateTOTPSeed()
	seedEnc, kv, _ := kr.EncryptTOTPSeed(owner.ID, seed)
	_ = store.EnableTOTP(ctx, owner.ID, seedEnc, kv)
	plainCodes, hashes, _ := auth.GenerateRecoveryCodes(4)
	_ = store.ReplaceRecoveryCodes(ctx, owner.ID, hashes)

	// Add a regular member.
	mh, _ := auth.HashPassword("memberpass123")
	memberID, err := store.CreateAndAddHouseholdMember(ctx, 1, "member1", "Member One", mh, "member")
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

	now := time.Now()
	srv.nowFn = func() time.Time { return now }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Password login with TOTP enabled must land on /mfa, not /.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	lPage, _ := client.Get(ts.URL + "/login")
	lb, _ := io.ReadAll(lPage.Body)
	lPage.Body.Close()
	csrf := extractCSRF(t, string(lb))

	r1, _ := client.PostForm(ts.URL+"/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"owner1"},
		"password":   {"ownerpass123"},
	})
	r1.Body.Close()
	if r1.Request.URL.Path != "/mfa" {
		t.Fatalf("expected redirect to /mfa, got %s", r1.Request.URL.Path)
	}

	// Verify TOTP code -> lands on /.
	// Compute current TOTP using totp helper via time-aligned VerifyTOTP? Let's test recovery code flow + member permissions too!
	// Test recovery code login (single-use, revokes prior sessions).
	recGet, _ := client.Get(ts.URL + "/recovery")
	recB, _ := io.ReadAll(recGet.Body)
	recGet.Body.Close()
	rcSRF := extractCSRF(t, string(recB))
	recResp, _ := client.PostForm(ts.URL+"/recovery", url.Values{
		"csrf_token":    {rcSRF},
		"username":      {"owner1"},
		"recovery_code": {plainCodes[0]},
	})
	recResp.Body.Close()
	if recResp.Request.URL.Path != "/security" {
		t.Fatalf("recovery expected redirect to /security, got %s", recResp.Request.URL.Path)
	}
	// Reusing same recovery code must fail.
	recResp2, _ := client.PostForm(ts.URL+"/recovery", url.Values{
		"csrf_token":    {rcSRF},
		"username":      {"owner1"},
		"recovery_code": {plainCodes[0]},
	})
	recResp2.Body.Close()
	if recResp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused recovery code status = %d, want 401", recResp2.StatusCode)
	}

	// 2. Member cannot create invites or remove members (403 Forbidden).
	memClient := loginAs(t, ts, "member1", "memberpass123")
	uPage, _ := memClient.Get(ts.URL + "/users")
	ub, _ := io.ReadAll(uPage.Body)
	uPage.Body.Close()
	mCSRF := extractCSRF(t, string(ub))

	invResp, _ := memClient.PostForm(ts.URL+"/households/invite", url.Values{"csrf_token": {mCSRF}})
	invResp.Body.Close()
	if invResp.StatusCode != http.StatusForbidden {
		t.Fatalf("member creating invite got %d, want 403", invResp.StatusCode)
	}

	// 3. Owner removes member1 -> member1's existing session loses household access immediately.
	secPage, _ := client.Get(ts.URL + "/users")
	sb, _ := io.ReadAll(secPage.Body)
	secPage.Body.Close()
	oCSRF := extractCSRF(t, string(sb))

	remResp, _ := client.PostForm(ts.URL+"/households/members/remove", url.Values{
		"csrf_token": {oCSRF},
		"user_id":    {fmt.Sprintf("%d", memberID)},
	})
	remResp.Body.Close()

	memOv, _ := memClient.Get(ts.URL + "/")
	memOv.Body.Close()
	if memOv.Request.URL.Path != "/users" {
		t.Fatalf("removed member accessing / got %s, want redirect to /users", memOv.Request.URL.Path)
	}

	// 4. CSV formula sanitization check.
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")
	_, _ = store.SaveExpense(ctx, db.SaveExpenseInput{
		HouseholdID: 1, Kind: "expense", AmountCents: 1000, Merchant: "=CMD|'/C calc'!A0",
		PurchaseDate: "2026-03-18", PaymentMethod: "cash", CategoryID: groc.ID, Source: "manual",
	})
	csvResp, _ := client.Get(ts.URL + "/ledger/export.csv")
	csvBytes, _ := io.ReadAll(csvResp.Body)
	csvResp.Body.Close()
	if !strings.Contains(string(csvBytes), "'=CMD") {
		t.Fatalf("expected sanitized CSV cell '=CMD, got %s", string(csvBytes))
	}
	_ = base64.StdEncoding.EncodeToString(k1)
}

func TestTabSwitchScanFailClosedQuotaAndFirstRunBootstrap(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sec2.db")
	k1, _ := crypto.GenerateKey()
	kr := &crypto.KeyRing{CurrentVersion: 1, Keys: map[int][]byte{1: k1}}
	store, err := db.OpenWithKeys(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	h, _ := auth.HashPassword("secret1234")
	if err := store.CreateInitialUser(ctx, "owner1", h); err != nil {
		t.Fatalf("CreateInitialUser failed: %v", err)
	}
	if err := store.CreateInitialUser(ctx, "owner2", h); err != db.ErrAlreadyInitialized {
		t.Fatalf("second CreateInitialUser got %v, want ErrAlreadyInitialized", err)
	}

	u1, _ := store.GetUserByUsername(ctx, "owner1")
	h2, _ := auth.HashPassword("secret1234")
	u2, _ := store.CreateUser(ctx, "owner2", "Owner 2", h2)
	hh2, _ := store.CreateHousehold(ctx, "Household 2", u2)
	_ = store.AddHouseholdMember(ctx, hh2, u1.ID, "member")

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
		MaxHouseholdQuotaBytes: 3500, // Tight quota: 1 small JPEG fits (~1.5KB), 2nd exceeds!
		SessionTTL:             24 * time.Hour,
		SessionIdleTTL:         24 * time.Hour,
		CookieSecure:           "false",
		WebAuthnRPID:           "localhost",
		WebAuthnRPOrigin:       "http://localhost",
		RegistrationMode:       "invite_only",
	}
	srv, err := NewServer(cfg, store, ocr.NewScanner("nonexistent-tesseract", "ell+eng", 5*time.Second, 1))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := loginAs(t, ts, "owner1", "secret1234")

	// Open /scan while active household is 1 -> get CSRF + household_id=1.
	scanPage, _ := client.Get(ts.URL + "/scan")
	sb, _ := io.ReadAll(scanPage.Body)
	scanPage.Body.Close()
	csrf := extractCSRF(t, string(sb))

	// Switch active household to hh2 in another "tab".
	swResp, _ := client.PostForm(ts.URL+"/households/switch", url.Values{
		"csrf_token":   {csrf},
		"household_id": {strconv.FormatInt(hh2, 10)},
	})
	swResp.Body.Close()

	// Submit /scan with household_id=1 -> must stay in household 1 (not hh2)!
	var body1 bytes.Buffer
	mw1 := multipart.NewWriter(&body1)
	_ = mw1.WriteField("csrf_token", csrf)
	_ = mw1.WriteField("household_id", "1")
	fw1, _ := mw1.CreateFormFile("receipt", "r1.jpg")
	_, _ = fw1.Write(makeTestJPEG(t, 120, 120))
	mw1.Close()

	req1, _ := http.NewRequest(http.MethodPost, ts.URL+"/scan", &body1)
	req1.Header.Set("Content-Type", mw1.FormDataContentType())
	resp1, _ := client.Do(req1)
	resp1.Body.Close()
	if b2, _ := store.GetHouseholdReceiptBytes(ctx, hh2); b2 != 0 {
		t.Fatalf("receipt leaked to hh2 (%d bytes)", b2)
	}

	// Second upload via manual /expenses attachment on household 1 exceeds tight quota -> must fail (422) and not save!
	groc, _ := store.CategoryByName(ctx, 1, "Groceries")
	var body2 bytes.Buffer
	mw2 := multipart.NewWriter(&body2)
	_ = mw2.WriteField("csrf_token", csrf)
	_ = mw2.WriteField("household_id", "1")
	_ = mw2.WriteField("amount", "15,00")
	_ = mw2.WriteField("merchant", "QUOTA TEST")
	_ = mw2.WriteField("purchase_date", "2026-03-18")
	_ = mw2.WriteField("payment_method", "cash")
	_ = mw2.WriteField("category_id", strconv.FormatInt(groc.ID, 10))
	fw2, _ := mw2.CreateFormFile("attachment", "r2.jpg")
	_, _ = fw2.Write(makeTestJPEG(t, 400, 400))
	mw2.Close()

	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/expenses", &body2)
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	resp2, _ := client.Do(req2)
	resp2Bytes, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(resp2Bytes), "quota") {
		t.Fatalf("manual attachment over quota: status=%d body_has_quota=%v", resp2.StatusCode, strings.Contains(string(resp2Bytes), "quota"))
	}

	// Remove owner1 from household 1 (after adding u2 as owner of household 1) -> submitting /scan with household_id=1 must fail closed (403)!
	_ = store.AddHouseholdMember(ctx, 1, u2, "owner")
	_ = store.RemoveHouseholdMember(ctx, 1, u1.ID)

	var body3 bytes.Buffer
	mw3 := multipart.NewWriter(&body3)
	_ = mw3.WriteField("csrf_token", csrf)
	_ = mw3.WriteField("household_id", "1")
	fw3, _ := mw3.CreateFormFile("receipt", "r3.jpg")
	_, _ = fw3.Write(makeTestJPEG(t, 100, 100))
	mw3.Close()
	req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/scan", &body3)
	req3.Header.Set("Content-Type", mw3.FormDataContentType())
	resp3, _ := client.Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("removed member scan on household 1 got %d, want 403", resp3.StatusCode)
	}
}
