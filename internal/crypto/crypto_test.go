package crypto

import (
	"encoding/base64"
	"testing"
)

func TestKeyRingWrapAndAADTamper(t *testing.T) {
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	env := "v1:" + base64.StdEncoding.EncodeToString(k1) + ",v2:" + base64.StdEncoding.EncodeToString(k2)

	kr, err := LoadKeyRing("", env)
	if err != nil {
		t.Fatal(err)
	}
	if kr.CurrentVersion != 2 {
		t.Fatalf("CurrentVersion = %d, want 2", kr.CurrentVersion)
	}

	dek, _ := GenerateKey()
	wrapped, ver, err := kr.WrapKey(dek, 1)
	if err != nil || ver != 2 {
		t.Fatalf("WrapKey failed: ver=%d err=%v", ver, err)
	}
	unwrapped, err := kr.UnwrapKey(wrapped, ver, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Unwrapping with wrong household ID must fail AAD verification.
	if _, err := kr.UnwrapKey(wrapped, ver, 2); err == nil {
		t.Fatalf("expected error unwrapping DEK for wrong household")
	}

	// Encrypt field for expense 10 in household 1.
	aad := AAD(1, "expenses", 10, "merchant")
	ct, err := Encrypt(unwrapped, aad, []byte("ΣΚΛΑΒΕΝΙΤΗΣ"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt(unwrapped, aad, ct)
	if err != nil || string(pt) != "ΣΚΛΑΒΕΝΙΤΗΣ" {
		t.Fatalf("Decrypt = %q, %v", pt, err)
	}

	// Moving ciphertext to expense 11 or another field or household must fail AAD verification.
	if _, err := Decrypt(unwrapped, AAD(1, "expenses", 11, "merchant"), ct); err == nil {
		t.Fatalf("expected AAD failure on different record ID")
	}
	if _, err := Decrypt(unwrapped, AAD(1, "expenses", 10, "note"), ct); err == nil {
		t.Fatalf("expected AAD failure on different field")
	}
	if _, err := Decrypt(unwrapped, AAD(2, "expenses", 10, "merchant"), ct); err == nil {
		t.Fatalf("expected AAD failure on different household")
	}
}

func TestTOTPSeedEncryption(t *testing.T) {
	k1, _ := GenerateKey()
	kr, err := LoadKeyRing("", base64.StdEncoding.EncodeToString(k1))
	if err != nil {
		t.Fatal(err)
	}
	ct, ver, err := kr.EncryptTOTPSeed(7, "JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	got, err := kr.DecryptTOTPSeed(7, ver, ct)
	if err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("DecryptTOTPSeed = %q, %v", got, err)
	}
	if _, err := kr.DecryptTOTPSeed(8, ver, ct); err == nil {
		t.Fatalf("expected error decrypting TOTP seed for wrong user")
	}
}
