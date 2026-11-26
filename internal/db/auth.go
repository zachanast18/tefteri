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
	ActiveHouseholdID   int64
	ActiveHouseholdName string
	ActiveRole          string
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
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, username, display_name, password_hash, created_at FROM users WHERE username = ?
	`, strings.TrimSpace(username)).Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, expiresAt time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var hhID sql.NullInt64
	if hhs, err := s.ListUserHouseholds(ctx, userID); err == nil && len(hhs) > 0 {
		hhID = sql.NullInt64{Int64: hhs[0].ID, Valid: true}
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, active_household_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, tokenHash, userID, hhID, expiresAt.UTC().Format(time.RFC3339), now)
	return err
}

func (s *Store) GetSessionUser(ctx context.Context, tokenHash string, now time.Time) (User, error) {
	var u User
	var expStr string
	var activeHH sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.display_name, u.password_hash, u.created_at, s.active_household_id, s.expires_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?
	`, tokenHash).Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.CreatedAt, &activeHH, &expStr)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil || now.UTC().After(exp) {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
		return User{}, ErrNotFound
	}

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

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}
