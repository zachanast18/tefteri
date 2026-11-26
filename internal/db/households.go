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

type HouseholdInvite struct {
	Code      string
	Role      string
	ExpiresAt string
}

// CreateHousehold creates a new household, wraps a fresh DEK, seeds default categories, and adds creatorUserID as owner.
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

	if _, err := s.ensureDEKTx(ctx, tx, hhID); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO household_members (household_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)
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
		ORDER BY CASE WHEN m.role = 'owner' THEN 0 ELSE 1 END, u.id ASC
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

func normalizeRole(role string) string {
	if role == "owner" || role == "admin" {
		return "owner"
	}
	return "member"
}

func (s *Store) AddHouseholdMember(ctx context.Context, householdID, userID int64, role string) error {
	role = normalizeRole(role)
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
	role = normalizeRole(role)

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
	return s.CreateInviteWithRole(ctx, householdID, createdByUserID, code, "member", expiresAt)
}

func (s *Store) CreateInviteWithRole(ctx context.Context, householdID, createdByUserID int64, code, role string, expiresAt time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO household_invites (code, household_id, role, created_by_user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, code, householdID, normalizeRole(role), createdByUserID, expiresAt.UTC().Format(time.RFC3339), now)
	return err
}

func (s *Store) ListActiveInvites(ctx context.Context, householdID int64, now time.Time) ([]HouseholdInvite, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT code, role, expires_at FROM household_invites
		WHERE household_id = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?
		ORDER BY created_at DESC
	`, householdID, now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HouseholdInvite
	for rows.Next() {
		var inv HouseholdInvite
		if err := rows.Scan(&inv.Code, &inv.Role, &inv.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) RevokeInvite(ctx context.Context, householdID int64, code string) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE household_invites SET revoked_at = ? WHERE household_id = ? AND code = ? AND used_at IS NULL AND revoked_at IS NULL
	`, time.Now().UTC().Format(time.RFC3339), householdID, strings.TrimSpace(code))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
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
	var role, expStr string
	var usedAt, revokedAt sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT household_id, role, expires_at, used_at, revoked_at FROM household_invites WHERE code = ?
	`, code).Scan(&hhID, &role, &expStr, &usedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrInvalidInvite
	}
	if err != nil {
		return 0, err
	}
	if usedAt.Valid || revokedAt.Valid {
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
		VALUES (?, ?, ?, ?)
		ON CONFLICT(household_id, user_id) DO NOTHING
	`, hhID, userID, normalizeRole(role), nowStr); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return hhID, nil
}

func (s *Store) RemoveHouseholdMember(ctx context.Context, householdID, targetUserID int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM household_members WHERE household_id = ? AND user_id = ?`, householdID, targetUserID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if role == "owner" {
		var owners int
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM household_members WHERE household_id = ? AND role = 'owner'`, householdID).Scan(&owners)
		if owners <= 1 {
			return ErrLastOwner
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM household_members WHERE household_id = ? AND user_id = ?`, householdID, targetUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_household_id = NULL WHERE user_id = ? AND active_household_id = ?`, targetUserID, householdID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) TransferOwnership(ctx context.Context, householdID, newOwnerID, currentOwnerID int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM household_members WHERE household_id = ? AND user_id = ?`, householdID, newOwnerID).Scan(&exists); err != nil || exists == 0 {
		return ErrNotMember
	}
	if _, err := tx.ExecContext(ctx, `UPDATE household_members SET role = 'owner' WHERE household_id = ? AND user_id = ?`, householdID, newOwnerID); err != nil {
		return err
	}
	if currentOwnerID > 0 && currentOwnerID != newOwnerID {
		if _, err := tx.ExecContext(ctx, `UPDATE household_members SET role = 'member' WHERE household_id = ? AND user_id = ?`, householdID, currentOwnerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteHousehold(ctx context.Context, householdID int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM households WHERE id = ?`, householdID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
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
