package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookieName = "tefteri_session"
	CSRFCookieName    = "tefteri_csrf"

	argonTime    uint32 = 2
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32
)

// HashPassword hashes a password using Argon2id with OWASP-aligned parameters.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword verifies an Argon2id or legacy bcrypt hash.
// Returns (matched, needsRehash).
func VerifyPassword(encoded, password string) (bool, bool) {
	if strings.HasPrefix(encoded, "$argon2id$") {
		parts := strings.Split(encoded, "$")
		if len(parts) != 6 {
			return false, false
		}
		var ver int
		var mem, tm uint32
		var thr uint8
		if _, err := fmt.Sscanf(parts[2], "v=%d", &ver); err != nil {
			return false, false
		}
		if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &tm, &thr); err != nil {
			return false, false
		}
		salt, err := base64.RawStdEncoding.DecodeString(parts[4])
		if err != nil {
			return false, false
		}
		want, err := base64.RawStdEncoding.DecodeString(parts[5])
		if err != nil {
			return false, false
		}
		got := argon2.IDKey([]byte(password), salt, tm, mem, thr, uint32(len(want)))
		return subtle.ConstantTimeCompare(got, want) == 1, false
	}

	// Legacy bcrypt support for seamless upgrade.
	if bcrypt.CompareHashAndPassword([]byte(encoded), []byte(password)) == nil {
		return true, true
	}
	return false, false
}

func RandomToken(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// GenerateRecoveryCodes returns n single-use recovery codes (e.g. "ABCD-EFGH-JKLM")
// and their SHA-256 hex hashes.
func GenerateRecoveryCodes(n int) (plain []string, hashes []string, err error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	plain = make([]string, 0, n)
	hashes = make([]string, 0, n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 12)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		for j, v := range raw {
			if j > 0 && j%4 == 0 {
				b.WriteByte('-')
			}
			b.WriteByte(alphabet[int(v)%len(alphabet)])
		}
		code := b.String()
		plain = append(plain, code)
		hashes = append(hashes, HashToken(strings.ToUpper(strings.ReplaceAll(code, "-", ""))))
	}
	return plain, hashes, nil
}

func NormalizeRecoveryCode(raw string) string {
	return HashToken(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), "-", "")))
}

func IsHTTPS(r *http.Request, mode string) bool {
	switch mode {
	case "true":
		return true
	case "false":
		return false
	default:
		if r.TLS != nil {
			return true
		}
		proto := strings.ToLower(r.Header.Get("X-Forwarded-Proto"))
		return proto == "https"
	}
}

func SetSessionCookie(w http.ResponseWriter, r *http.Request, rawToken string, ttl time.Duration, secureMode string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsHTTPS(r, secureMode),
		MaxAge:   int(ttl.Seconds()),
	})
}

func ClearSessionCookie(w http.ResponseWriter, r *http.Request, secureMode string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsHTTPS(r, secureMode),
		MaxAge:   -1,
	})
}

// EnsureCSRFCookie returns the current CSRF token from the cookie, or sets a fresh one.
func EnsureCSRFCookie(w http.ResponseWriter, r *http.Request, secureMode string) string {
	if c, err := r.Cookie(CSRFCookieName); err == nil && len(c.Value) >= 16 {
		return c.Value
	}
	tok, err := RandomToken(24)
	if err != nil {
		tok = "fallback-csrf-token"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsHTTPS(r, secureMode),
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
	return tok
}

// ValidateCSRF checks that the submitted form/header token matches the CSRF cookie.
func ValidateCSRF(r *http.Request, submitted string) bool {
	c, err := r.Cookie(CSRFCookieName)
	if err != nil || c.Value == "" || submitted == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(submitted)) == 1
}
