package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"spending-project/internal/categorize"
	"spending-project/internal/crypto"
)

type ReceiptImage struct {
	ID          int64
	HouseholdID int64
	SHA256      string
	MimeType    string
	Width       int
	Height      int
	Data        []byte
	CreatedAt   string
}

type ReceiptDraft struct {
	ID             string
	HouseholdID    int64
	ReceiptImageID int64
	Merchant       string
	PurchaseDate   string
	AmountCents    int64
	PaymentMethod  string
	CategoryID     int64
	CheckMerchant  bool
	CheckDate      bool
	CheckAmount    bool
	OCRStatus      string // "ok", "busy", "timeout", "failed"
	OCRError       string
	NeedsReview    bool
	ReviewReason   string
}

type Expense struct {
	ID                 int64
	HouseholdID        int64
	Kind               string // "expense" or "refund"
	AmountCents        int64
	Merchant           string
	NormalizedMerchant string
	PurchaseDate       string // YYYY-MM-DD
	PaymentMethod      string // "cash" or "card"
	CategoryID         int64
	CategoryName       string
	Note               string
	Source             string // "manual", "receipt", "subscription"
	ReceiptImageID     *int64
	CreatedByUserID    *int64
	CreatedByName      string
	PaidByUserID       *int64
	PaidByName         string
	CreatedAt          string
	UpdatedAt          string
}

type ExpenseFilter struct {
	HouseholdID   int64
	FromDate      string
	ToDate        string
	Merchant      string
	CategoryID    int64
	PaymentMethod string
	UserID        int64
	Limit         int
}

type DuplicateMatch struct {
	ExpenseID     int64
	Kind          string
	Merchant      string
	PurchaseDate  string
	AmountCents   int64
	PaymentMethod string
	MatchReason   string // "exact_image" or "likely_match"
}

type CategoryTotal struct {
	CategoryID   int64
	CategoryName string
	TotalCents   int64
	SharePercent float64
}

type MonthlyPoint struct {
	Month      string // YYYY-MM
	TotalCents int64
}

type DashboardSummary struct {
	TotalCents      int64
	CashCents       int64
	CardCents       int64
	ExpenseCount    int
	ByCategory      []CategoryTotal
	RecentExpenses  []Expense
	MonthlyTrend    []MonthlyPoint
}

func (s *Store) GetHouseholdReceiptBytes(ctx context.Context, householdID int64) (int64, error) {
	var total int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(length(data) + COALESCE(length(data_enc), 0)), 0)
		FROM receipt_images WHERE household_id = ?
	`, householdID).Scan(&total)
	return total, err
}

func (s *Store) SaveReceiptImage(ctx context.Context, householdID int64, sha256Hex, mimeType string, width, height int, data []byte) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	dek, err := s.ensureDEKTx(ctx, tx, householdID)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO receipt_images (household_id, sha256, mime_type, width, height, data, enc_version, created_at)
		VALUES (?, ?, ?, ?, ?, x'', 1, ?)
	`, householdID, sha256Hex, mimeType, width, height, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()

	ct, err := crypto.Encrypt(dek, crypto.AAD(householdID, "receipt_images", id, "data"), data)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE receipt_images SET data_enc = ? WHERE id = ?`, ct, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) GetReceiptImage(ctx context.Context, householdID, id int64) (ReceiptImage, error) {
	var img ReceiptImage
	var dataEnc []byte
	var encVer int
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, household_id, sha256, mime_type, width, height, data, data_enc, enc_version, created_at
		FROM receipt_images WHERE id = ? AND household_id = ?
	`, id, householdID).Scan(&img.ID, &img.HouseholdID, &img.SHA256, &img.MimeType, &img.Width, &img.Height, &img.Data, &dataEnc, &encVer, &img.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return img, ErrNotFound
	}
	if err != nil {
		return img, err
	}
	if encVer == 1 {
		dek, err := s.EnsureHouseholdDEK(ctx, householdID)
		if err != nil {
			return img, err
		}
		pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "receipt_images", img.ID, "data"), dataEnc)
		if err != nil {
			return img, err
		}
		img.Data = pt
	}
	return img, nil
}

func (s *Store) CreateDraft(ctx context.Context, d ReceiptDraft) error {
	dek, err := s.EnsureHouseholdDEK(ctx, d.HouseholdID)
	if err != nil {
		return err
	}
	mEnc, err := crypto.Encrypt(dek, crypto.AAD(d.HouseholdID, "receipt_drafts", d.ID, "merchant"), []byte(d.Merchant))
	if err != nil {
		return err
	}
	aEnc, err := crypto.EncryptInt64(dek, crypto.AAD(d.HouseholdID, "receipt_drafts", d.ID, "amount"), d.AmountCents)
	if err != nil {
		return err
	}
	eEnc, err := crypto.Encrypt(dek, crypto.AAD(d.HouseholdID, "receipt_drafts", d.ID, "ocr_error"), []byte(d.OCRError))
	if err != nil {
		return err
	}

	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO receipt_drafts (
			id, household_id, receipt_image_id, merchant, purchase_date, amount_cents,
			payment_method, category_id, check_merchant, check_date, check_amount,
			ocr_status, ocr_error, needs_review, review_reason,
			merchant_enc, amount_enc, ocr_error_enc, enc_version, created_at
		) VALUES (?, ?, ?, '', ?, 0, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, 1, ?)
	`, d.ID, d.HouseholdID, d.ReceiptImageID, d.PurchaseDate,
		d.PaymentMethod, nullableInt64(d.CategoryID), boolToInt(d.CheckMerchant), boolToInt(d.CheckDate), boolToInt(d.CheckAmount),
		d.OCRStatus, boolToInt(d.NeedsReview), d.ReviewReason,
		mEnc, aEnc, eEnc, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetDraft(ctx context.Context, householdID int64, id string) (ReceiptDraft, error) {
	var d ReceiptDraft
	var catID sql.NullInt64
	var cm, cd, ca, nr, encVer int
	var mEnc, aEnc, eEnc []byte
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, household_id, receipt_image_id, merchant, purchase_date, amount_cents,
		       payment_method, category_id, check_merchant, check_date, check_amount,
		       ocr_status, ocr_error, needs_review, review_reason,
		       merchant_enc, amount_enc, ocr_error_enc, enc_version
		FROM receipt_drafts WHERE id = ? AND household_id = ?
	`, id, householdID).Scan(
		&d.ID, &d.HouseholdID, &d.ReceiptImageID, &d.Merchant, &d.PurchaseDate, &d.AmountCents,
		&d.PaymentMethod, &catID, &cm, &cd, &ca, &d.OCRStatus, &d.OCRError, &nr, &d.ReviewReason,
		&mEnc, &aEnc, &eEnc, &encVer,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if catID.Valid {
		d.CategoryID = catID.Int64
	}
	d.CheckMerchant = cm == 1
	d.CheckDate = cd == 1
	d.CheckAmount = ca == 1
	d.NeedsReview = nr == 1
	if encVer == 1 {
		dek, err := s.EnsureHouseholdDEK(ctx, householdID)
		if err != nil {
			return d, err
		}
		if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "receipt_drafts", d.ID, "merchant"), mEnc); err == nil {
			d.Merchant = string(pt)
		}
		if amt, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "receipt_drafts", d.ID, "amount"), aEnc); err == nil {
			d.AmountCents = amt
		}
		if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "receipt_drafts", d.ID, "ocr_error"), eEnc); err == nil {
			d.OCRError = string(pt)
		}
	}
	return d, nil
}

func (s *Store) DeleteDraft(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM receipt_drafts WHERE id = ?`, id)
	return err
}

// FindDuplicates checks for exact-image SHA-256 matches and likely merchant+date+amount matches within a household.
func (s *Store) FindDuplicates(ctx context.Context, householdID int64, receiptImageID *int64, merchant, purchaseDate string, amountCents int64, excludeExpenseID int64) ([]DuplicateMatch, error) {
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return nil, err
	}
	var matches []DuplicateMatch
	seen := map[int64]bool{}

	if receiptImageID != nil && *receiptImageID > 0 {
		rows, err := s.DB.QueryContext(ctx, `
			SELECT e.id, e.kind, e.merchant, e.purchase_date, e.amount_cents, e.payment_method,
			       e.merchant_enc, e.amount_enc, e.enc_version
			FROM expenses e
			JOIN receipt_images r1 ON r1.id = e.receipt_image_id
			JOIN receipt_images r2 ON r2.id = ?
			WHERE e.household_id = ? AND r1.sha256 = r2.sha256 AND e.id != ?
			ORDER BY e.id DESC LIMIT 5
		`, *receiptImageID, householdID, excludeExpenseID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var m DuplicateMatch
			var mEnc, aEnc []byte
			var ev int
			if err := rows.Scan(&m.ExpenseID, &m.Kind, &m.Merchant, &m.PurchaseDate, &m.AmountCents, &m.PaymentMethod, &mEnc, &aEnc, &ev); err != nil {
				rows.Close()
				return nil, err
			}
			if ev == 1 {
				if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "expenses", m.ExpenseID, "merchant"), mEnc); err == nil {
					m.Merchant = string(pt)
				}
				if amt, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "expenses", m.ExpenseID, "amount"), aEnc); err == nil {
					m.AmountCents = amt
				}
			}
			m.MatchReason = "exact_image"
			seen[m.ExpenseID] = true
			matches = append(matches, m)
		}
		rows.Close()
	}

	norm := categorize.NormalizeMerchant(merchant)
	if norm != "" && purchaseDate != "" && amountCents > 0 {
		hm := crypto.BlindIndex(dek, "merchant", norm)
		rows, err := s.DB.QueryContext(ctx, `
			SELECT id, kind, merchant, purchase_date, amount_cents, payment_method,
			       merchant_enc, amount_enc, enc_version
			FROM expenses
			WHERE household_id = ? AND normalized_merchant = ? AND purchase_date = ? AND id != ?
			ORDER BY id DESC LIMIT 10
		`, householdID, hm, purchaseDate, excludeExpenseID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var m DuplicateMatch
			var mEnc, aEnc []byte
			var ev int
			if err := rows.Scan(&m.ExpenseID, &m.Kind, &m.Merchant, &m.PurchaseDate, &m.AmountCents, &m.PaymentMethod, &mEnc, &aEnc, &ev); err != nil {
				rows.Close()
				return nil, err
			}
			if ev == 1 {
				if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "expenses", m.ExpenseID, "merchant"), mEnc); err == nil {
					m.Merchant = string(pt)
				}
				if amt, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "expenses", m.ExpenseID, "amount"), aEnc); err == nil {
					m.AmountCents = amt
				}
			}
			if m.AmountCents == amountCents && !seen[m.ExpenseID] {
				m.MatchReason = "likely_match"
				matches = append(matches, m)
			}
		}
		rows.Close()
	}

	return matches, nil
}

type SaveExpenseInput struct {
	HouseholdID      int64
	Kind             string
	AmountCents      int64
	Merchant         string
	PurchaseDate     string
	PaymentMethod    string
	CategoryID       int64
	Note             string
	Source           string
	ReceiptImageID   *int64
	CreatedByUserID  *int64
	PaidByUserID     *int64
	Splits           []ExpenseSplit
	ReplaceSplits    bool
	SubmissionToken  string
	RememberMerchant bool
	DraftID          string
}

func (s *Store) SaveExpense(ctx context.Context, in SaveExpenseInput) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if in.SubmissionToken != "" {
		var existing int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM expenses WHERE submission_token = ?`, in.SubmissionToken).Scan(&existing)
		if err == nil {
			return existing, ErrDuplicateSubmit
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	norm := categorize.NormalizeMerchant(in.Merchant)
	var tok sql.NullString
	if in.SubmissionToken != "" {
		tok = sql.NullString{String: in.SubmissionToken, Valid: true}
	}

	if in.HouseholdID <= 0 {
		in.HouseholdID = 1
	}
	dek, err := s.ensureDEKTx(ctx, tx, in.HouseholdID)
	if err != nil {
		return 0, err
	}
	paidBy := in.PaidByUserID
	if paidBy == nil {
		paidBy = in.CreatedByUserID
	}
	hm := crypto.BlindIndex(dek, "merchant", norm)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO expenses (
			household_id, kind, amount_cents, merchant, normalized_merchant, purchase_date,
			payment_method, category_id, note, source, receipt_image_id,
			created_by_user_id, paid_by_user_id, submission_token, enc_version, created_at, updated_at
		) VALUES (?, ?, 1, '', ?, ?, ?, ?, '', ?, ?, ?, ?, ?, 1, ?, ?)
	`, in.HouseholdID, in.Kind, hm, in.PurchaseDate,
		in.PaymentMethod, in.CategoryID, in.Source,
		in.ReceiptImageID, in.CreatedByUserID, paidBy, tok, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && in.SubmissionToken != "" {
			return 0, ErrDuplicateSubmit
		}
		return 0, err
	}
	id, _ := res.LastInsertId()

	mEnc, _ := crypto.Encrypt(dek, crypto.AAD(in.HouseholdID, "expenses", id, "merchant"), []byte(strings.TrimSpace(in.Merchant)))
	nEnc, _ := crypto.Encrypt(dek, crypto.AAD(in.HouseholdID, "expenses", id, "note"), []byte(strings.TrimSpace(in.Note)))
	aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(in.HouseholdID, "expenses", id, "amount"), in.AmountCents)
	if _, err := tx.ExecContext(ctx, `UPDATE expenses SET merchant_enc = ?, note_enc = ?, amount_enc = ? WHERE id = ?`, mEnc, nEnc, aEnc, id); err != nil {
		return 0, err
	}

	for _, sp := range in.Splits {
		sEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(in.HouseholdID, "expense_splits", fmt.Sprintf("%d:%d", id, sp.UserID), "share"), sp.ShareCents)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO expense_splits (expense_id, user_id, share_cents, share_enc, enc_version) VALUES (?, ?, 0, ?, 1)
		`, id, sp.UserID, sEnc); err != nil {
			return 0, err
		}
	}

	if in.RememberMerchant && norm != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO merchant_category_rules (household_id, normalized_merchant, category_id, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(household_id, normalized_merchant) DO UPDATE SET category_id = excluded.category_id, updated_at = excluded.updated_at
		`, in.HouseholdID, hm, in.CategoryID, now); err != nil {
			return 0, err
		}
	}

	if in.DraftID != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM receipt_drafts WHERE id = ?`, in.DraftID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) UpdateExpense(ctx context.Context, householdID, id int64, in SaveExpenseInput) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	dek, err := s.ensureDEKTx(ctx, tx, householdID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	norm := categorize.NormalizeMerchant(in.Merchant)
	hm := crypto.BlindIndex(dek, "merchant", norm)
	mEnc, _ := crypto.Encrypt(dek, crypto.AAD(householdID, "expenses", id, "merchant"), []byte(strings.TrimSpace(in.Merchant)))
	nEnc, _ := crypto.Encrypt(dek, crypto.AAD(householdID, "expenses", id, "note"), []byte(strings.TrimSpace(in.Note)))
	aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(householdID, "expenses", id, "amount"), in.AmountCents)

	res, err := tx.ExecContext(ctx, `
		UPDATE expenses
		SET kind = ?, amount_cents = 1, merchant = '', normalized_merchant = ?,
		    purchase_date = ?, payment_method = ?, category_id = ?, note = '',
		    merchant_enc = ?, note_enc = ?, amount_enc = ?, enc_version = 1,
		    paid_by_user_id = COALESCE(?, paid_by_user_id),
		    receipt_image_id = COALESCE(?, receipt_image_id), updated_at = ?
		WHERE id = ? AND household_id = ?
	`, in.Kind, hm,
		in.PurchaseDate, in.PaymentMethod, in.CategoryID,
		mEnc, nEnc, aEnc,
		in.PaidByUserID, in.ReceiptImageID, now, id, householdID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}

	if in.ReplaceSplits {
		if _, err := tx.ExecContext(ctx, `DELETE FROM expense_splits WHERE expense_id = ?`, id); err != nil {
			return err
		}
		for _, sp := range in.Splits {
			sEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(householdID, "expense_splits", fmt.Sprintf("%d:%d", id, sp.UserID), "share"), sp.ShareCents)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO expense_splits (expense_id, user_id, share_cents, share_enc, enc_version) VALUES (?, ?, 0, ?, 1)
			`, id, sp.UserID, sEnc); err != nil {
				return err
			}
		}
	}

	if in.RememberMerchant && norm != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO merchant_category_rules (household_id, normalized_merchant, category_id, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(household_id, normalized_merchant) DO UPDATE SET category_id = excluded.category_id, updated_at = excluded.updated_at
		`, householdID, hm, in.CategoryID, now); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) DeleteExpense(ctx context.Context, householdID, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM expenses WHERE id = ? AND household_id = ?`, id, householdID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetExpense(ctx context.Context, householdID, id int64) (Expense, error) {
	var e Expense
	var recID sql.NullInt64
	var userID sql.NullInt64
	var paidByID sql.NullInt64
	var mEnc, nEnc, aEnc []byte
	var encVer int
	err := s.DB.QueryRowContext(ctx, `
		SELECT e.id, e.household_id, e.kind, e.amount_cents, e.merchant, e.normalized_merchant,
		       e.purchase_date, e.payment_method, e.category_id, c.name,
		       e.note, e.source, e.receipt_image_id, e.created_by_user_id,
		       COALESCE(NULLIF(u.display_name, ''), u.username, ''),
		       e.paid_by_user_id,
		       COALESCE(NULLIF(up.display_name, ''), up.username, ''),
		       e.merchant_enc, e.note_enc, e.amount_enc, e.enc_version,
		       e.created_at, e.updated_at
		FROM expenses e
		JOIN categories c ON c.id = e.category_id
		LEFT JOIN users u ON u.id = e.created_by_user_id
		LEFT JOIN users up ON up.id = e.paid_by_user_id
		WHERE e.id = ? AND e.household_id = ?
	`, id, householdID).Scan(
		&e.ID, &e.HouseholdID, &e.Kind, &e.AmountCents, &e.Merchant, &e.NormalizedMerchant,
		&e.PurchaseDate, &e.PaymentMethod, &e.CategoryID, &e.CategoryName,
		&e.Note, &e.Source, &recID, &userID, &e.CreatedByName, &paidByID, &e.PaidByName,
		&mEnc, &nEnc, &aEnc, &encVer, &e.CreatedAt, &e.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	if recID.Valid {
		v := recID.Int64
		e.ReceiptImageID = &v
	}
	if userID.Valid {
		v := userID.Int64
		e.CreatedByUserID = &v
	}
	if paidByID.Valid {
		v := paidByID.Int64
		e.PaidByUserID = &v
	}
	if encVer == 1 {
		dek, err := s.EnsureHouseholdDEK(ctx, householdID)
		if err != nil {
			return e, err
		}
		if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "expenses", e.ID, "merchant"), mEnc); err == nil {
			e.Merchant = string(pt)
		}
		if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "expenses", e.ID, "note"), nEnc); err == nil {
			e.Note = string(pt)
		}
		if amt, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "expenses", e.ID, "amount"), aEnc); err == nil {
			e.AmountCents = amt
		}
	}
	return e, nil
}

func (s *Store) ListExpenses(ctx context.Context, f ExpenseFilter) ([]Expense, error) {
	var where []string
	var args []any

	if f.HouseholdID > 0 {
		where = append(where, "e.household_id = ?")
		args = append(args, f.HouseholdID)
	}
	if f.FromDate != "" {
		where = append(where, "e.purchase_date >= ?")
		args = append(args, f.FromDate)
	}
	if f.ToDate != "" {
		where = append(where, "e.purchase_date <= ?")
		args = append(args, f.ToDate)
	}
	if f.CategoryID > 0 {
		where = append(where, "e.category_id = ?")
		args = append(args, f.CategoryID)
	}
	if f.PaymentMethod == "cash" || f.PaymentMethod == "card" {
		where = append(where, "e.payment_method = ?")
		args = append(args, f.PaymentMethod)
	}
	if f.UserID > 0 {
		where = append(where, "e.created_by_user_id = ?")
		args = append(args, f.UserID)
	}

	query := `
		SELECT e.id, e.household_id, e.kind, e.amount_cents, e.merchant, e.normalized_merchant,
		       e.purchase_date, e.payment_method, e.category_id, c.name,
		       e.note, e.source, e.receipt_image_id, e.created_by_user_id,
		       COALESCE(NULLIF(u.display_name, ''), u.username, ''),
		       e.paid_by_user_id,
		       COALESCE(NULLIF(up.display_name, ''), up.username, ''),
		       e.merchant_enc, e.note_enc, e.amount_enc, e.enc_version,
		       e.created_at, e.updated_at
		FROM expenses e
		JOIN categories c ON c.id = e.category_id
		LEFT JOIN users u ON u.id = e.created_by_user_id
		LEFT JOIN users up ON up.id = e.paid_by_user_id
	`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY e.purchase_date DESC, e.id DESC"
	if f.Limit > 0 && strings.TrimSpace(f.Merchant) == "" {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	var dek []byte
	if f.HouseholdID > 0 {
		var err error
		dek, err = s.EnsureHouseholdDEK(ctx, f.HouseholdID)
		if err != nil {
			return nil, err
		}
	}

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	type rawExpense struct {
		e          Expense
		mEnc, nEnc []byte
		aEnc       []byte
		encVer     int
	}
	var rawList []rawExpense
	for rows.Next() {
		var r rawExpense
		var recID, userID, paidByID sql.NullInt64
		if err := rows.Scan(
			&r.e.ID, &r.e.HouseholdID, &r.e.Kind, &r.e.AmountCents, &r.e.Merchant, &r.e.NormalizedMerchant,
			&r.e.PurchaseDate, &r.e.PaymentMethod, &r.e.CategoryID, &r.e.CategoryName,
			&r.e.Note, &r.e.Source, &recID, &userID, &r.e.CreatedByName, &paidByID, &r.e.PaidByName,
			&r.mEnc, &r.nEnc, &r.aEnc, &r.encVer, &r.e.CreatedAt, &r.e.UpdatedAt,
		); err != nil {
			rows.Close()
			return nil, err
		}
		if recID.Valid {
			v := recID.Int64
			r.e.ReceiptImageID = &v
		}
		if userID.Valid {
			v := userID.Int64
			r.e.CreatedByUserID = &v
		}
		if paidByID.Valid {
			v := paidByID.Int64
			r.e.PaidByUserID = &v
		}
		rawList = append(rawList, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	merchantFilter := strings.ToLower(strings.TrimSpace(f.Merchant))
	var out []Expense
	for _, r := range rawList {
		e := r.e
		if r.encVer == 1 {
			if dek == nil || f.HouseholdID == 0 {
				dek, err = s.EnsureHouseholdDEK(ctx, e.HouseholdID)
				if err != nil {
					return nil, err
				}
			}
			if pt, err := crypto.Decrypt(dek, crypto.AAD(e.HouseholdID, "expenses", e.ID, "merchant"), r.mEnc); err == nil {
				e.Merchant = string(pt)
			}
			if pt, err := crypto.Decrypt(dek, crypto.AAD(e.HouseholdID, "expenses", e.ID, "note"), r.nEnc); err == nil {
				e.Note = string(pt)
			}
			if amt, err := crypto.DecryptInt64(dek, crypto.AAD(e.HouseholdID, "expenses", e.ID, "amount"), r.aEnc); err == nil {
				e.AmountCents = amt
			}
		}
		if merchantFilter != "" && !strings.Contains(strings.ToLower(e.Merchant), merchantFilter) {
			continue
		}
		out = append(out, e)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

func (s *Store) CountAllExpenses(ctx context.Context, householdID int64) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM expenses WHERE household_id = ?`, householdID).Scan(&n)
	return n, err
}

// GetDashboardSummary computes confirmed net spending, cash/card totals, category breakdown,
// recent expenses for the selected [fromDate, toDate] range, and a 6-month trend ending at trendEndMonth (YYYY-MM).
func (s *Store) GetDashboardSummary(ctx context.Context, householdID int64, fromDate, toDate, trendEndMonth string, loc *time.Location) (DashboardSummary, error) {
	var sum DashboardSummary

	exps, err := s.ListExpenses(ctx, ExpenseFilter{HouseholdID: householdID, FromDate: fromDate, ToDate: toDate})
	if err != nil {
		return sum, err
	}

	catTotals := map[int64]*CategoryTotal{}
	for _, e := range exps {
		net := e.AmountCents
		if e.Kind == "refund" {
			net = -net
		}
		sum.TotalCents += net
		if e.PaymentMethod == "cash" {
			sum.CashCents += net
		} else if e.PaymentMethod == "card" {
			sum.CardCents += net
		}
		sum.ExpenseCount++

		ct, ok := catTotals[e.CategoryID]
		if !ok {
			ct = &CategoryTotal{CategoryID: e.CategoryID, CategoryName: e.CategoryName}
			catTotals[e.CategoryID] = ct
		}
		ct.TotalCents += net
	}

	var maxCat int64
	for _, ct := range catTotals {
		if ct.TotalCents != 0 {
			if ct.TotalCents > maxCat {
				maxCat = ct.TotalCents
			}
			sum.ByCategory = append(sum.ByCategory, *ct)
		}
	}
	sort.Slice(sum.ByCategory, func(i, j int) bool {
		if sum.ByCategory[i].TotalCents != sum.ByCategory[j].TotalCents {
			return sum.ByCategory[i].TotalCents > sum.ByCategory[j].TotalCents
		}
		return sum.ByCategory[i].CategoryName < sum.ByCategory[j].CategoryName
	})
	for i := range sum.ByCategory {
		if maxCat > 0 && sum.ByCategory[i].TotalCents > 0 {
			sum.ByCategory[i].SharePercent = float64(sum.ByCategory[i].TotalCents) * 100.0 / float64(maxCat)
		}
	}

	if len(exps) > 8 {
		sum.RecentExpenses = exps[:8]
	} else {
		sum.RecentExpenses = exps
	}

	// Build 6-month trend ending at trendEndMonth.
	endT, err := time.ParseInLocation("2006-01", trendEndMonth, loc)
	if err != nil {
		endT = time.Now().In(loc)
	}
	startT := time.Date(endT.Year(), endT.Month()-5, 1, 0, 0, 0, 0, loc)
	endRange := time.Date(endT.Year(), endT.Month()+1, 0, 0, 0, 0, 0, loc)

	trendExps, err := s.ListExpenses(ctx, ExpenseFilter{
		HouseholdID: householdID,
		FromDate:    startT.Format("2006-01-02"),
		ToDate:      endRange.Format("2006-01-02"),
	})
	if err != nil {
		return sum, err
	}
	byMonth := map[string]int64{}
	for _, e := range trendExps {
		if len(e.PurchaseDate) >= 7 {
			net := e.AmountCents
			if e.Kind == "refund" {
				net = -net
			}
			byMonth[e.PurchaseDate[:7]] += net
		}
	}

	for i := 0; i < 6; i++ {
		m := time.Date(startT.Year(), startT.Month()+time.Month(i), 1, 0, 0, 0, 0, loc)
		ym := m.Format("2006-01")
		sum.MonthlyTrend = append(sum.MonthlyTrend, MonthlyPoint{
			Month:      ym,
			TotalCents: byMonth[ym],
		})
	}

	return sum, nil
}

func nullableInt64(v int64) sql.NullInt64 {
	if v <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
