package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookieName = "household_session"
	CSRFCookieName    = "household_csrf"
)

func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
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
