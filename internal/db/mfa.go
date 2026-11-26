package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

var (
	ErrLastAuthMethod  = errors.New("cannot remove the last usable sign-in method")
	ErrLastOwner       = errors.New("transfer ownership before removing or demoting the last owner")
	ErrChallengeInvalid = errors.New("authentication challenge is invalid or expired")
)

type PasskeyInfo struct {
	ID        []byte
	Label     string
	CreatedAt string
}

func (s *Store) EnableTOTP(ctx context.Context, userID int64, seedEnc []byte, kekVer int) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE users SET totp_seed_enc = ?, totp_kek_version = ?, totp_enabled = 1 WHERE id = ?
	`, seedEnc, kekVer, userID)
	return err
}

func (s *Store) DisableTOTP(ctx context.Context, userID int64) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE users SET totp_seed_enc = NULL, totp_kek_version = 0, totp_enabled = 0 WHERE id = ?
	`, userID)
	return err
}

func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO recovery_codes (user_id, code_hash, created_at) VALUES (?, ?, ?)
		`, userID, h, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UseRecoveryCode(ctx context.Context, userID int64, codeHash string, now time.Time) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM recovery_codes WHERE user_id = ? AND code_hash = ? AND used_at IS NULL LIMIT 1
	`, userID, codeHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE recovery_codes SET used_at = ? WHERE id = ?
	`, now.UTC().Format(time.RFC3339), id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) CountUnusedRecoveryCodes(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL
	`, userID).Scan(&n)
	return n, err
}

func (s *Store) SaveWebAuthnCredential(ctx context.Context, userID int64, credID []byte, label string, cred webauthn.Credential) error {
	b, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO webauthn_credentials (id, user_id, label, credential_json, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, credID, userID, label, b, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) UpdateWebAuthnCredential(ctx context.Context, credID []byte, cred webauthn.Credential) error {
	b, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE webauthn_credentials SET credential_json = ? WHERE id = ?`, b, credID)
	return err
}

func (s *Store) ListWebAuthnCredentials(ctx context.Context, userID int64) ([]webauthn.Credential, []PasskeyInfo, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, label, credential_json, created_at FROM webauthn_credentials WHERE user_id = ? ORDER BY created_at ASC
	`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var creds []webauthn.Credential
	var infos []PasskeyInfo
	for rows.Next() {
		var id, raw []byte
		var label, created string
		if err := rows.Scan(&id, &label, &raw, &created); err != nil {
			return nil, nil, err
		}
		var c webauthn.Credential
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		creds = append(creds, c)
		infos = append(infos, PasskeyInfo{ID: id, Label: label, CreatedAt: created})
	}
	return creds, infos, rows.Err()
}

func (s *Store) DeleteWebAuthnCredential(ctx context.Context, userID int64, credID []byte) error {
	creds, _, err := s.ListWebAuthnCredentials(ctx, userID)
	if err != nil {
		return err
	}
	var hasPass bool
	var pwdHash string
	_ = s.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&pwdHash)
	hasPass = pwdHash != ""

	if len(creds) <= 1 && !hasPass {
		return ErrLastAuthMethod
	}
	_, err = s.DB.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE id = ? AND user_id = ?`, credID, userID)
	return err
}

func (s *Store) SaveWebAuthnChallenge(ctx context.Context, id string, userID *int64, purpose string, data webauthn.SessionData, expiresAt time.Time) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO webauthn_challenges (id, user_id, purpose, session_data, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, userID, purpose, b, expiresAt.UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) ConsumeWebAuthnChallenge(ctx context.Context, id string, purpose string, now time.Time) (webauthn.SessionData, *int64, error) {
	var sd webauthn.SessionData
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return sd, nil, err
	}
	defer tx.Rollback()

	var raw []byte
	var expStr string
	var uid sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT user_id, session_data, expires_at
		FROM webauthn_challenges
		WHERE id = ? AND purpose = ?
	`, id, purpose).Scan(&uid, &raw, &expStr)
	if errors.Is(err, sql.ErrNoRows) {
		return sd, nil, ErrChallengeInvalid
	}
	if err != nil {
		return sd, nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webauthn_challenges WHERE id = ?`, id); err != nil {
		return sd, nil, err
	}
	if err := tx.Commit(); err != nil {
		return sd, nil, err
	}

	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil || now.UTC().After(exp) {
		return sd, nil, ErrChallengeInvalid
	}
	if err := json.Unmarshal(raw, &sd); err != nil {
		return sd, nil, ErrChallengeInvalid
	}
	var uidPtr *int64
	if uid.Valid {
		v := uid.Int64
		uidPtr = &v
	}
	return sd, uidPtr, nil
}
