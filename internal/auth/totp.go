package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// GenerateTOTPSeed creates a random 160-bit Base32-encoded TOTP secret.
func GenerateTOTPSeed() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// TOTPAuthURI returns a standard otpauth:// URI for authenticator apps.
func TOTPAuthURI(username, seed string) string {
	label := url.PathEscape("Teftéri:" + username)
	return fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=%s&digits=6&period=30",
		label, url.QueryEscape(seed), url.QueryEscape("Teftéri"))
}

// VerifyTOTP checks a 6-digit TOTP code against seed with +/- 1 step skew.
func VerifyTOTP(seed, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(seed)))
	if err != nil {
		return false
	}
	step := now.UTC().Unix() / 30
	for delta := int64(-1); delta <= 1; delta++ {
		want := totpAt(key, uint64(step+delta))
		if subtle.ConstantTimeCompare([]byte(code), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

func totpAt(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset])&0x7f)<<24 |
		(uint32(sum[offset+1])&0xff)<<16 |
		(uint32(sum[offset+2])&0xff)<<8 |
		(uint32(sum[offset+3]) & 0xff)
	return fmt.Sprintf("%06d", bin%1_000_000)
}
