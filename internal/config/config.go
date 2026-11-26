package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime configuration for Teftéri.
type Config struct {
	ListenAddr             string
	DBPath                 string
	Currency               string
	CurrencySymbol         string
	Timezone               string
	Location               *time.Location
	OCRLanguages           string
	OCRTimeout             time.Duration
	MaxUploadBytes         int64
	MaxImageDim            int
	MaxStoredDim           int
	MaxHouseholdQuotaBytes int64
	SessionTTL             time.Duration
	SessionIdleTTL         time.Duration
	CookieSecure           string // "auto", "true", "false"
	TrustedProxy           string
	TesseractBin           string
	TesseractThread        int
	MasterKeyFile          string
	MasterKey              string
	WebAuthnRPID           string
	WebAuthnRPOrigin       string
	RegistrationMode       string // "invite_only" or "closed"
}

// Load reads configuration from environment variables with household defaults.
func Load() (Config, error) {
	tzName := getEnv("APP_TIMEZONE", "Europe/Athens")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return Config{}, err
	}

	currency := strings.ToUpper(getEnv("APP_CURRENCY", "EUR"))
	symbol := getEnv("APP_CURRENCY_SYMBOL", defaultSymbol(currency))

	timeoutSec := getEnvInt("OCR_TIMEOUT_SECONDS", 25)
	maxUploadMB := getEnvInt("MAX_UPLOAD_MB", 12)
	maxImageDim := getEnvInt("MAX_IMAGE_DIM", 6000)
	maxStoredDim := getEnvInt("MAX_STORED_DIM", 1600)
	maxQuotaMB := getEnvInt("MAX_HOUSEHOLD_RECEIPTS_MB", 250)
	sessionHours := getEnvInt("SESSION_TTL_HOURS", 24*30)
	idleHours := getEnvInt("SESSION_IDLE_HOURS", 24*7)

	regMode := strings.ToLower(getEnv("REGISTRATION_MODE", "invite_only"))
	if regMode != "closed" {
		regMode = "invite_only"
	}

	return Config{
		ListenAddr:             getEnv("LISTEN_ADDR", ":8080"),
		DBPath:                 getEnv("DB_PATH", "/data/ledger.db"),
		Currency:               currency,
		CurrencySymbol:         symbol,
		Timezone:               tzName,
		Location:               loc,
		OCRLanguages:           getEnv("OCR_LANGUAGES", "ell+eng"),
		OCRTimeout:             time.Duration(timeoutSec) * time.Second,
		MaxUploadBytes:         int64(maxUploadMB) * 1024 * 1024,
		MaxImageDim:            maxImageDim,
		MaxStoredDim:           maxStoredDim,
		MaxHouseholdQuotaBytes: int64(maxQuotaMB) * 1024 * 1024,
		SessionTTL:             time.Duration(sessionHours) * time.Hour,
		SessionIdleTTL:         time.Duration(idleHours) * time.Hour,
		CookieSecure:           strings.ToLower(getEnv("COOKIE_SECURE", "auto")),
		TrustedProxy:           strings.TrimSpace(os.Getenv("TRUSTED_PROXY")),
		TesseractBin:           getEnv("TESSERACT_BIN", "tesseract"),
		TesseractThread:        1,
		MasterKeyFile:          strings.TrimSpace(os.Getenv("MASTER_KEY_FILE")),
		MasterKey:              strings.TrimSpace(os.Getenv("MASTER_KEY")),
		WebAuthnRPID:           getEnv("WEBAUTHN_RP_ID", "localhost"),
		WebAuthnRPOrigin:       getEnv("WEBAUTHN_RP_ORIGIN", "http://localhost:8080"),
		RegistrationMode:       regMode,
	}, nil
}

func defaultSymbol(currency string) string {
	switch currency {
	case "EUR":
		return "€"
	case "USD":
		return "$"
	case "GBP":
		return "£"
	default:
		return currency
	}
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
