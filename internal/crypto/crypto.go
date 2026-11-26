package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const KeySize = 32 // AES-256

var (
	ErrMissingMasterKey = errors.New("master wrapping key is required (set MASTER_KEY_FILE or MASTER_KEY)")
	ErrInvalidKeyFormat = errors.New("invalid master key format")
	ErrDecryptFailed    = errors.New("decryption or authentication failed")
)

// KeyRing holds versioned 256-bit Key Encryption Keys (KEKs).
// Format in env/file: comma-separated "v1:<base64>,v2:<base64>" or raw 32-byte base64 (treated as v1).
// The highest version is used for wrapping new keys; all versions are kept for unwrapping.
type KeyRing struct {
	CurrentVersion int
	Keys           map[int][]byte
}

func LoadKeyRing(keyFile, keyEnv string) (*KeyRing, error) {
	raw := strings.TrimSpace(keyEnv)
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("read master key file: %w", err)
		}
		raw = strings.TrimSpace(string(b))
	}
	if raw == "" {
		return nil, ErrMissingMasterKey
	}

	kr := &KeyRing{Keys: make(map[int][]byte)}
	parts := strings.Split(raw, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ver := 1
		b64 := p
		if strings.Contains(p, ":") {
			kv := strings.SplitN(p, ":", 2)
			vStr := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(kv[0])), "v")
			v, err := strconv.Atoi(vStr)
			if err != nil || v <= 0 {
				return nil, ErrInvalidKeyFormat
			}
			ver = v
			b64 = strings.TrimSpace(kv[1])
		}
		keyBytes, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(keyBytes) != KeySize {
			return nil, ErrInvalidKeyFormat
		}
		kr.Keys[ver] = keyBytes
		if ver > kr.CurrentVersion {
			kr.CurrentVersion = ver
		}
	}
	if kr.CurrentVersion == 0 {
		return nil, ErrMissingMasterKey
	}
	return kr, nil
}

// GenerateKey returns a cryptographically random 32-byte key.
func GenerateKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, err
	}
	return k, nil
}

// Subkey derives a domain-separated 32-byte subkey using HMAC-SHA256.
func Subkey(master []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("tefteri-subkey-v1:"))
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}

// BlindIndex computes a keyed HMAC-SHA256 hex digest for exact-match lookups
// (e.g. normalized merchant names) without storing plaintext in SQLite.
func BlindIndex(dek []byte, purpose, value string) string {
	key := Subkey(dek, "index:"+purpose)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// WrapKey encrypts a household DEK with the current KEK, binding householdID as AAD.
func (kr *KeyRing) WrapKey(dek []byte, householdID int64) (wrapped []byte, version int, err error) {
	kek, ok := kr.Keys[kr.CurrentVersion]
	if !ok {
		return nil, 0, ErrMissingMasterKey
	}
	aad := fmt.Sprintf("tefteri:dek:hh:%d:v:%d", householdID, kr.CurrentVersion)
	ct, err := Encrypt(kek, []byte(aad), dek)
	if err != nil {
		return nil, 0, err
	}
	return ct, kr.CurrentVersion, nil
}

// UnwrapKey decrypts a household DEK using the specified KEK version.
func (kr *KeyRing) UnwrapKey(wrapped []byte, version int, householdID int64) ([]byte, error) {
	kek, ok := kr.Keys[version]
	if !ok {
		return nil, fmt.Errorf("unknown KEK version %d", version)
	}
	aad := fmt.Sprintf("tefteri:dek:hh:%d:v:%d", householdID, version)
	return Decrypt(kek, []byte(aad), wrapped)
}

// EncryptTOTPSeed encrypts a TOTP secret under a separately scoped subkey of the current KEK.
func (kr *KeyRing) EncryptTOTPSeed(userID int64, seed string) ([]byte, int, error) {
	kek, ok := kr.Keys[kr.CurrentVersion]
	if !ok {
		return nil, 0, ErrMissingMasterKey
	}
	sub := Subkey(kek, "totp-seed")
	aad := fmt.Sprintf("tefteri:totp:user:%d:v:%d", userID, kr.CurrentVersion)
	ct, err := Encrypt(sub, []byte(aad), []byte(seed))
	if err != nil {
		return nil, 0, err
	}
	return ct, kr.CurrentVersion, nil
}

// DecryptTOTPSeed decrypts a TOTP secret using the stored KEK version.
func (kr *KeyRing) DecryptTOTPSeed(userID int64, version int, ct []byte) (string, error) {
	kek, ok := kr.Keys[version]
	if !ok {
		return "", fmt.Errorf("unknown KEK version %d", version)
	}
	sub := Subkey(kek, "totp-seed")
	aad := fmt.Sprintf("tefteri:totp:user:%d:v:%d", userID, version)
	pt, err := Decrypt(sub, []byte(aad), ct)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// Encrypt encrypts plaintext with AES-256-GCM using a random 12-byte nonce and AAD.
func Encrypt(key, aad, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// Decrypt decrypts AES-256-GCM ciphertext and verifies AAD.
func Decrypt(key, aad, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns+gcm.Overhead() {
		return nil, ErrDecryptFailed
	}
	pt, err := gcm.Open(nil, ciphertext[:ns], ciphertext[ns:], aad)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return pt, nil
}

// EncryptInt64 encrypts a 64-bit integer with AES-256-GCM.
func EncryptInt64(key, aad []byte, v int64) ([]byte, error) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	return Encrypt(key, aad, buf[:])
}

// DecryptInt64 decrypts an AES-256-GCM encrypted 64-bit integer.
func DecryptInt64(key, aad, ciphertext []byte) (int64, error) {
	pt, err := Decrypt(key, aad, ciphertext)
	if err != nil || len(pt) != 8 {
		return 0, ErrDecryptFailed
	}
	return int64(binary.BigEndian.Uint64(pt)), nil
}

// AAD builds a canonical authenticated-data byte slice binding household, table, record, and field.
func AAD(householdID int64, table string, recordID any, field string) []byte {
	return []byte(fmt.Sprintf("tefteri:hh:%d:%s:%v:%s", householdID, table, recordID, field))
}
