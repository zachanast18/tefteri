package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"spending-project/internal/categorize"
	"spending-project/internal/crypto"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	ErrNotFound          = errors.New("record not found")
	ErrDuplicateSubmit   = errors.New("this form was already submitted")
	ErrAlreadyConfirmed  = errors.New("this renewal has already been confirmed or skipped")
)

type Store struct {
	DB   *sql.DB
	Keys *crypto.KeyRing
}

// Open opens the SQLite database with an ephemeral in-memory key ring (for tests/dev).
func Open(path string) (*Store, error) {
	k, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	return OpenWithKeys(path, &crypto.KeyRing{
		CurrentVersion: 1,
		Keys:           map[int][]byte{1: k},
	})
}

// OpenWithKeys opens the SQLite database, configures WAL and foreign keys, runs migrations,
// and ensures existing plaintext records are encrypted under the KeyRing.
func OpenWithKeys(path string, kr *crypto.KeyRing) (*Store, error) {
	if kr == nil {
		return nil, crypto.ErrMissingMasterKey
	}
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	} else {
		dsn = "file::memory:?cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite handles concurrent readers well in WAL mode, and serializing writers in-process
	// avoids SQLITE_BUSY under load.
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{DB: sqlDB, Keys: kr}
	if err := s.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := s.MigratePlaintextToEncrypted(context.Background()); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("encrypt existing records: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.DB.Close()
}

// Backup writes a consistent live snapshot using SQLite VACUUM INTO.
func (s *Store) Backup(ctx context.Context, destPath string) error {
	abs, err := filepath.Abs(destPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return err
	}
	// Remove destination if empty/stale? VACUUM INTO fails if target exists.
	_, err = s.DB.ExecContext(ctx, "VACUUM INTO ?", abs)
	return err
}

func (s *Store) migrate() error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}

	type migration struct {
		version int
		name    string
		sql     string
	}
	var list []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(e.Name(), "_", 2)
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("invalid migration filename %s: %w", e.Name(), err)
		}
		b, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		list = append(list, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })

	if _, err := s.DB.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		_, _ = s.DB.Exec(`PRAGMA foreign_keys = ON`)
	}()

	for _, m := range list {
		var exists int
		err := s.DB.QueryRow(`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.version).Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			continue
		}

		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

type Category struct {
	ID          int64
	HouseholdID int64
	Name        string
	SortOrder   int
}

func (s *Store) ListCategories(ctx context.Context, householdID int64) ([]Category, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, household_id, name, sort_order FROM categories WHERE household_id = ? ORDER BY sort_order ASC, name ASC`, householdID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.HouseholdID, &c.Name, &c.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CategoryByName(ctx context.Context, householdID int64, name string) (Category, error) {
	var c Category
	err := s.DB.QueryRowContext(ctx, `SELECT id, household_id, name, sort_order FROM categories WHERE household_id = ? AND name = ?`, householdID, name).Scan(&c.ID, &c.HouseholdID, &c.Name, &c.SortOrder)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *Store) CreateCategory(ctx context.Context, householdID int64, name string) (Category, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Category{}, errors.New("category name is required")
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO categories (household_id, name, sort_order, is_default) VALUES (?, ?, 85, 0)`, householdID, name)
	if err != nil {
		return Category{}, err
	}
	id, _ := res.LastInsertId()
	return Category{ID: id, HouseholdID: householdID, Name: name, SortOrder: 85}, nil
}

func (s *Store) RememberMerchantCategory(ctx context.Context, householdID int64, merchant string, categoryID int64) error {
	norm := categorize.NormalizeMerchant(merchant)
	if norm == "" {
		return nil
	}
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return err
	}
	hm := crypto.BlindIndex(dek, "merchant", norm)
	mEnc, err := crypto.Encrypt(dek, crypto.AAD(householdID, "merchant_category_rules", categoryID, "merchant"), []byte(norm))
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO merchant_category_rules (household_id, normalized_merchant, category_id, merchant_enc, enc_version, updated_at)
		VALUES (?, ?, ?, ?, 1, ?)
		ON CONFLICT(household_id, normalized_merchant) DO UPDATE SET
			category_id = excluded.category_id,
			merchant_enc = excluded.merchant_enc,
			enc_version = 1,
			updated_at = excluded.updated_at
	`, householdID, hm, categoryID, mEnc, time.Now().UTC().Format(time.RFC3339))
	return err
}

// SuggestCategory checks remembered normalized merchant mappings first,
// then falls back to explainable keyword rules, and finally "Uncategorized".
func (s *Store) SuggestCategory(ctx context.Context, householdID int64, merchant string, ocrText string) (Category, string, error) {
	norm := categorize.NormalizeMerchant(merchant)
	if norm != "" {
		if dek, err := s.EnsureHouseholdDEK(ctx, householdID); err == nil {
			hm := crypto.BlindIndex(dek, "merchant", norm)
			var c Category
			err := s.DB.QueryRowContext(ctx, `
				SELECT c.id, c.household_id, c.name, c.sort_order
				FROM merchant_category_rules r
				JOIN categories c ON c.id = r.category_id
				WHERE r.household_id = ? AND r.normalized_merchant = ?
			`, householdID, hm).Scan(&c.ID, &c.HouseholdID, &c.Name, &c.SortOrder)
			if err == nil {
				return c, "remembered", nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return Category{}, "", err
			}
		}
	}

	suggestedName := categorize.SuggestFromKeywords(merchant, ocrText)
	c, err := s.CategoryByName(ctx, householdID, suggestedName)
	if err == nil {
		if suggestedName == "Uncategorized" {
			return c, "none", nil
		}
		return c, "keyword", nil
	}
	c, err = s.CategoryByName(ctx, householdID, "Uncategorized")
	return c, "none", err
}
