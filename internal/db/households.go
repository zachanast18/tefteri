package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidInvite = errors.New("invite code is invalid, expired, or already used")
	ErrNotMember     = errors.New("user is not a member of that household")
)

type Household struct {
	ID        int64
	Name      string
	Role      string
	CreatedAt string
}

type HouseholdMember struct {
	UserID      int64
	Username    string
	DisplayName string
	Role        string
	CreatedAt   string
}

func (m HouseholdMember) Label() string {
	if strings.TrimSpace(m.DisplayName) != "" {
		return m.DisplayName
	}
	return m.Username
}

var defaultCategories = []struct {
	name string
	sort int
}{
	{"Groceries", 10},
	{"Dining", 20},
	{"Transport", 30},
	{"Fuel", 40},
	{"Utilities", 50},
	{"Health", 60},
	{"Shopping", 70},
	{"Entertainment", 80},
	{"Other", 90},
	{"Uncategorized", 100},
}

// CreateHousehold creates a new household, seeds default categories, and adds creatorUserID as admin.
func (s *Store) CreateHousehold(ctx context.Context, name string, creatorUserID int64) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("household name is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `INSERT INTO households (name, created_at) VALUES (?, ?)`, name, now)
	if err != nil {
		return 0, err
	}
	hhID, _ := res.LastInsertId()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO household_members (household_id, user_id, role, created_at) VALUES (?, ?, 'admin', ?)
	`, hhID, creatorUserID, now); err != nil {
		return 0, err
	}

	for _, c := range defaultCategories {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO categories (household_id, name, sort_order, is_default) VALUES (?, ?, ?, 1)
		`, hhID, c.name, c.sort); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return hhID, nil
}

func (s *Store) ListUserHouseholds(ctx context.Context, userID int64) ([]Household, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT h.id, h.name, m.role, h.created_at
		FROM household_members m
		JOIN households h ON h.id = m.household_id
		WHERE m.user_id = ?
		ORDER BY h.id ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Household
	for rows.Next() {
		var h Household
		if err := rows.Scan(&h.ID, &h.Name, &h.Role, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) ListHouseholdMembers(ctx context.Context, householdID int64) ([]HouseholdMember, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT u.id, u.username, u.display_name, m.role, m.created_at
		FROM household_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.household_id = ?
		ORDER BY CASE WHEN m.role = 'admin' THEN 0 ELSE 1 END, u.id ASC
	`, householdID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HouseholdMember
	for rows.Next() {
		var m HouseholdMember
		if err := rows.Scan(&m.UserID, &m.Username, &m.DisplayName, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) AddHouseholdMember(ctx context.Context, householdID, userID int64, role string) error {
	if role != "admin" {
		role = "member"
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO household_members (household_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(household_id, user_id) DO NOTHING
	`, householdID, userID, role, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) CreateAndAddHouseholdMember(ctx context.Context, householdID int64, username, displayName, passwordHash, role string) (int64, error) {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if username == "" {
		return 0, errors.New("username is required")
	}
	if displayName == "" {
		displayName = username
	}
	if role != "admin" {
		role = "member"
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO users (username, display_name, password_hash, created_at)
		VALUES (?, ?, ?, ?)
	`, username, displayName, passwordHash, now)
	if err != nil {
		return 0, err
	}
	uid, _ := res.LastInsertId()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO household_members (household_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)
	`, householdID, uid, role, now); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uid, nil
}

func (s *Store) CreateInvite(ctx context.Context, householdID, createdByUserID int64, code string, expiresAt time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO household_invites (code, household_id, created_by_user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, code, householdID, createdByUserID, expiresAt.UTC().Format(time.RFC3339), now)
	return err
}

func (s *Store) AcceptInvite(ctx context.Context, code string, userID int64, now time.Time) (int64, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return 0, ErrInvalidInvite
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var hhID int64
	var expStr string
	var usedAt sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT household_id, expires_at, used_at FROM household_invites WHERE code = ?
	`, code).Scan(&hhID, &expStr, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrInvalidInvite
	}
	if err != nil {
		return 0, err
	}
	if usedAt.Valid {
		return 0, ErrInvalidInvite
	}
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil || now.UTC().After(exp) {
		return 0, ErrInvalidInvite
	}

	nowStr := now.UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `UPDATE household_invites SET used_at = ? WHERE code = ?`, nowStr, code); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO household_members (household_id, user_id, role, created_at)
		VALUES (?, ?, 'member', ?)
		ON CONFLICT(household_id, user_id) DO NOTHING
	`, hhID, userID, nowStr); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return hhID, nil
}

func (s *Store) SetSessionHousehold(ctx context.Context, tokenHash string, userID, householdID int64) error {
	var exists int
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM household_members WHERE household_id = ? AND user_id = ?
	`, householdID, userID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotMember
	}
	_, err := s.DB.ExecContext(ctx, `
		UPDATE sessions SET active_household_id = ? WHERE token_hash = ? AND user_id = ?
	`, householdID, tokenHash, userID)
	return err
}
