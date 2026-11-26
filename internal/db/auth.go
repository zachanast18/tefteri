package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type User struct {
	ID                  int64
	Username            string
	DisplayName         string
	PasswordHash        string
	CreatedAt           string
	IsInstanceAdmin     bool
	TOTPEnabled         bool
	TOTPSeedEnc         []byte
	TOTPKekVersion      int
	ActiveHouseholdID   int64
	ActiveHouseholdName string
	ActiveRole          string
	MFAVerified         bool
	LastAuthAt          time.Time
}

func (u User) Label() string {
	if strings.TrimSpace(u.DisplayName) != "" {
		return u.DisplayName
	}
	return u.Username
}

func (s *Store) HasUser(ctx context.Context) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&n)
	return n > 0, err
}

func (s *Store) UpsertUser(ctx context.Context, username, passwordHash string) error {
	return s.UpsertUserWithDisplay(ctx, username, username, passwordHash)
}

func (s *Store) UpsertUserWithDisplay(ctx context.Context, username, displayName, passwordHash string) error {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = username
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO users (username, display_name, password_hash, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(username) DO UPDATE SET
			display_name = excluded.display_name,
			password_hash = excluded.password_hash
	`, username, displayName, passwordHash, now)
	if err != nil {
		return err
	}
	u, err := s.GetUserByUsername(ctx, username)
	if err != nil {
		return err
	}
	hhs, err := s.ListUserHouseholds(ctx, u.ID)
	if err == nil && len(hhs) == 0 {
		// Attach to household 1 if it exists, otherwise create default household.
		var h1 int
		_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM households WHERE id = 1`).Scan(&h1)
		if h1 > 0 {
			_ = s.AddHouseholdMember(ctx, 1, u.ID, "admin")
		} else {
			_, _ = s.CreateHousehold(ctx, "Our Household", u.ID)
		}
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, username, displayName, passwordHash string) (int64, error) {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if username == "" {
		return 0, errors.New("username is required")
	}
	if displayName == "" {
		displayName = username
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO users (username, display_name, password_hash, created_at)
		VALUES (?, ?, ?, ?)
	`, username, displayName, passwordHash, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, username, display_name, created_at
		FROM users
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	var adminInt, totpInt int
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, username, display_name, password_hash, created_at, is_instance_admin, totp_enabled, totp_seed_enc, totp_kek_version
		FROM users WHERE username = ?
	`, strings.TrimSpace(username)).Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt, &adminInt, &totpInt, &u.TOTPSeedEnc, &u.TOTPKekVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.IsInstanceAdmin = adminInt == 1
	u.TOTPEnabled = totpInt == 1
	return u, err
}

func (s *Store) GetUserByID(ctx context.Context, id int64) (User, error) {
	var u User
	var adminInt, totpInt int
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, username, display_name, password_hash, created_at, is_instance_admin, totp_enabled, totp_seed_enc, totp_kek_version
		FROM users WHERE id = ?
	`, id).Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt, &adminInt, &totpInt, &u.TOTPSeedEnc, &u.TOTPKekVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.IsInstanceAdmin = adminInt == 1
	u.TOTPEnabled = totpInt == 1
	return u, err
}

func (s *Store) UpdateUserPassword(ctx context.Context, userID int64, passwordHash string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
	return err
}

func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, expiresAt time.Time) error {
	return s.CreateSessionFull(ctx, tokenHash, userID, true, expiresAt, time.Now())
}

func (s *Store) CreateSessionFull(ctx context.Context, tokenHash string, userID int64, mfaVerified bool, expiresAt time.Time, now time.Time) error {
	nowStr := now.UTC().Format(time.RFC3339)
	var hhID sql.NullInt64
	if hhs, err := s.ListUserHouseholds(ctx, userID); err == nil && len(hhs) > 0 {
		hhID = sql.NullInt64{Int64: hhs[0].ID, Valid: true}
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, active_household_id, expires_at, created_at, last_seen_at, mfa_verified, last_auth_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, tokenHash, userID, hhID, expiresAt.UTC().Format(time.RFC3339), nowStr, nowStr, boolToInt(mfaVerified), nowStr)
	return err
}

func (s *Store) GetSessionUser(ctx context.Context, tokenHash string, now time.Time) (User, error) {
	return s.GetSessionUserWithIdle(ctx, tokenHash, now, 7*24*time.Hour)
}

func (s *Store) GetSessionUserWithIdle(ctx context.Context, tokenHash string, now time.Time, idleTTL time.Duration) (User, error) {
	var u User
	var expStr, lastSeenStr, lastAuthStr string
	var activeHH sql.NullInt64
	var adminInt, totpInt, mfaInt int
	err := s.DB.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.display_name, u.password_hash, u.created_at,
		       u.is_instance_admin, u.totp_enabled, u.totp_seed_enc, u.totp_kek_version,
		       s.active_household_id, s.expires_at, s.last_seen_at, s.mfa_verified, s.last_auth_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?
	`, tokenHash).Scan(
		&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt,
		&adminInt, &totpInt, &u.TOTPSeedEnc, &u.TOTPKekVersion,
		&activeHH, &expStr, &lastSeenStr, &mfaInt, &lastAuthStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	u.IsInstanceAdmin = adminInt == 1
	u.TOTPEnabled = totpInt == 1
	u.MFAVerified = mfaInt == 1

	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil || now.UTC().After(exp) {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
		return User{}, ErrNotFound
	}
	if lastSeenStr != "" && idleTTL > 0 {
		if ls, err := time.Parse(time.RFC3339, lastSeenStr); err == nil && now.UTC().After(ls.Add(idleTTL)) {
			_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
			return User{}, ErrNotFound
		}
	}
	if lastAuthStr != "" {
		if la, err := time.Parse(time.RFC3339, lastAuthStr); err == nil {
			u.LastAuthAt = la
		}
	}

	_, _ = s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, now.UTC().Format(time.RFC3339), tokenHash)

	hhs, err := s.ListUserHouseholds(ctx, u.ID)
	if err == nil && len(hhs) > 0 {
		chosen := hhs[0]
		if activeHH.Valid {
			for _, h := range hhs {
				if h.ID == activeHH.Int64 {
					chosen = h
					break
				}
			}
		}
		u.ActiveHouseholdID = chosen.ID
		u.ActiveHouseholdName = chosen.Name
		u.ActiveRole = chosen.Role
	}
	return u, nil
}

func (s *Store) RotateSession(ctx context.Context, oldTokenHash, newTokenHash string, userID int64, mfaVerified bool, expiresAt, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var activeHH sql.NullInt64
	_ = tx.QueryRowContext(ctx, `SELECT active_household_id FROM sessions WHERE token_hash = ?`, oldTokenHash).Scan(&activeHH)
	_, _ = tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, oldTokenHash)

	nowStr := now.UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, active_household_id, expires_at, created_at, last_seen_at, mfa_verified, last_auth_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, newTokenHash, userID, activeHH, expiresAt.UTC().Format(time.RFC3339), nowStr, nowStr, boolToInt(mfaVerified), nowStr); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteAllUserSessions(ctx context.Context, userID int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}
