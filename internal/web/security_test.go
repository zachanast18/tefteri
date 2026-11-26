package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
