package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"spending-project/internal/categorize"
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

func (s *Store) SaveReceiptImage(ctx context.Context, householdID int64, sha256Hex, mimeType string, width, height int, data []byte) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO receipt_images (household_id, sha256, mime_type, width, height, data, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, householdID, sha256Hex, mimeType, width, height, data, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetReceiptImage(ctx context.Context, householdID, id int64) (ReceiptImage, error) {
	var img ReceiptImage
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, household_id, sha256, mime_type, width, height, data, created_at
		FROM receipt_images WHERE id = ? AND household_id = ?
	`, id, householdID).Scan(&img.ID, &img.HouseholdID, &img.SHA256, &img.MimeType, &img.Width, &img.Height, &img.Data, &img.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return img, ErrNotFound
	}
	return img, err
}

func (s *Store) CreateDraft(ctx context.Context, d ReceiptDraft) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO receipt_drafts (
			id, household_id, receipt_image_id, merchant, purchase_date, amount_cents,
			payment_method, category_id, check_merchant, check_date, check_amount,
			ocr_status, ocr_error, needs_review, review_reason, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, d.ID, d.HouseholdID, d.ReceiptImageID, d.Merchant, d.PurchaseDate, d.AmountCents,
		d.PaymentMethod, nullableInt64(d.CategoryID), boolToInt(d.CheckMerchant), boolToInt(d.CheckDate), boolToInt(d.CheckAmount),
		d.OCRStatus, d.OCRError, boolToInt(d.NeedsReview), d.ReviewReason, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetDraft(ctx context.Context, householdID int64, id string) (ReceiptDraft, error) {
	var d ReceiptDraft
	var catID sql.NullInt64
	var cm, cd, ca, nr int
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, household_id, receipt_image_id, merchant, purchase_date, amount_cents,
		       payment_method, category_id, check_merchant, check_date, check_amount,
		       ocr_status, ocr_error, needs_review, review_reason
		FROM receipt_drafts WHERE id = ? AND household_id = ?
	`, id, householdID).Scan(
		&d.ID, &d.HouseholdID, &d.ReceiptImageID, &d.Merchant, &d.PurchaseDate, &d.AmountCents,
		&d.PaymentMethod, &catID, &cm, &cd, &ca, &d.OCRStatus, &d.OCRError, &nr, &d.ReviewReason,
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
	return d, nil
}

func (s *Store) DeleteDraft(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM receipt_drafts WHERE id = ?`, id)
	return err
}

// FindDuplicates checks for exact-image SHA-256 matches and likely merchant+date+amount matches within a household.
func (s *Store) FindDuplicates(ctx context.Context, householdID int64, receiptImageID *int64, merchant, purchaseDate string, amountCents int64, excludeExpenseID int64) ([]DuplicateMatch, error) {
	var matches []DuplicateMatch
	seen := map[int64]bool{}

	if receiptImageID != nil && *receiptImageID > 0 {
		rows, err := s.DB.QueryContext(ctx, `
			SELECT e.id, e.kind, e.merchant, e.purchase_date, e.amount_cents, e.payment_method
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
			if err := rows.Scan(&m.ExpenseID, &m.Kind, &m.Merchant, &m.PurchaseDate, &m.AmountCents, &m.PaymentMethod); err != nil {
				rows.Close()
				return nil, err
			}
			m.MatchReason = "exact_image"
			seen[m.ExpenseID] = true
			matches = append(matches, m)
		}
		rows.Close()
	}

	norm := categorize.NormalizeMerchant(merchant)
	if norm != "" && purchaseDate != "" && amountCents > 0 {
		rows, err := s.DB.QueryContext(ctx, `
			SELECT id, kind, merchant, purchase_date, amount_cents, payment_method
			FROM expenses
			WHERE household_id = ? AND normalized_merchant = ? AND purchase_date = ? AND amount_cents = ? AND id != ?
			ORDER BY id DESC LIMIT 5
		`, householdID, norm, purchaseDate, amountCents, excludeExpenseID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var m DuplicateMatch
			if err := rows.Scan(&m.ExpenseID, &m.Kind, &m.Merchant, &m.PurchaseDate, &m.AmountCents, &m.PaymentMethod); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[m.ExpenseID] {
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
	paidBy := in.PaidByUserID
	if paidBy == nil {
		paidBy = in.CreatedByUserID
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO expenses (
			household_id, kind, amount_cents, merchant, normalized_merchant, purchase_date,
			payment_method, category_id, note, source, receipt_image_id,
			created_by_user_id, paid_by_user_id, submission_token, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, in.HouseholdID, in.Kind, in.AmountCents, strings.TrimSpace(in.Merchant), norm, in.PurchaseDate,
		in.PaymentMethod, in.CategoryID, strings.TrimSpace(in.Note), in.Source,
		in.ReceiptImageID, in.CreatedByUserID, paidBy, tok, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && in.SubmissionToken != "" {
			return 0, ErrDuplicateSubmit
		}
		return 0, err
	}
	id, _ := res.LastInsertId()

	for _, sp := range in.Splits {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO expense_splits (expense_id, user_id, share_cents) VALUES (?, ?, ?)
		`, id, sp.UserID, sp.ShareCents); err != nil {
			return 0, err
		}
	}

	if in.RememberMerchant && norm != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO merchant_category_rules (household_id, normalized_merchant, category_id, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(household_id, normalized_merchant) DO UPDATE SET category_id = excluded.category_id, updated_at = excluded.updated_at
		`, in.HouseholdID, norm, in.CategoryID, now); err != nil {
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

	now := time.Now().UTC().Format(time.RFC3339)
	norm := categorize.NormalizeMerchant(in.Merchant)

	res, err := tx.ExecContext(ctx, `
		UPDATE expenses
		SET kind = ?, amount_cents = ?, merchant = ?, normalized_merchant = ?,
		    purchase_date = ?, payment_method = ?, category_id = ?, note = ?,
		    paid_by_user_id = COALESCE(?, paid_by_user_id),
		    receipt_image_id = COALESCE(?, receipt_image_id), updated_at = ?
		WHERE id = ? AND household_id = ?
	`, in.Kind, in.AmountCents, strings.TrimSpace(in.Merchant), norm,
		in.PurchaseDate, in.PaymentMethod, in.CategoryID, strings.TrimSpace(in.Note),
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
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO expense_splits (expense_id, user_id, share_cents) VALUES (?, ?, ?)
			`, id, sp.UserID, sp.ShareCents); err != nil {
				return err
			}
		}
	}

	if in.RememberMerchant && norm != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO merchant_category_rules (household_id, normalized_merchant, category_id, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(household_id, normalized_merchant) DO UPDATE SET category_id = excluded.category_id, updated_at = excluded.updated_at
		`, householdID, norm, in.CategoryID, now); err != nil {
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
	err := s.DB.QueryRowContext(ctx, `
		SELECT e.id, e.household_id, e.kind, e.amount_cents, e.merchant, e.normalized_merchant,
		       e.purchase_date, e.payment_method, e.category_id, c.name,
		       e.note, e.source, e.receipt_image_id, e.created_by_user_id,
		       COALESCE(NULLIF(u.display_name, ''), u.username, ''),
		       e.paid_by_user_id,
		       COALESCE(NULLIF(up.display_name, ''), up.username, ''),
		       e.created_at, e.updated_at
		FROM expenses e
		JOIN categories c ON c.id = e.category_id
		LEFT JOIN users u ON u.id = e.created_by_user_id
		LEFT JOIN users up ON up.id = e.paid_by_user_id
		WHERE e.id = ? AND e.household_id = ?
	`, id, householdID).Scan(
		&e.ID, &e.HouseholdID, &e.Kind, &e.AmountCents, &e.Merchant, &e.NormalizedMerchant,
		&e.PurchaseDate, &e.PaymentMethod, &e.CategoryID, &e.CategoryName,
		&e.Note, &e.Source, &recID, &userID, &e.CreatedByName, &paidByID, &e.PaidByName, &e.CreatedAt, &e.UpdatedAt,
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
	if strings.TrimSpace(f.Merchant) != "" {
		q := "%" + strings.ToLower(strings.TrimSpace(f.Merchant)) + "%"
		where = append(where, "(LOWER(e.merchant) LIKE ? OR e.normalized_merchant LIKE ?)")
		args = append(args, q, q)
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
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Expense
	for rows.Next() {
		var e Expense
		var recID sql.NullInt64
		var userID sql.NullInt64
		var paidByID sql.NullInt64
		if err := rows.Scan(
			&e.ID, &e.HouseholdID, &e.Kind, &e.AmountCents, &e.Merchant, &e.NormalizedMerchant,
			&e.PurchaseDate, &e.PaymentMethod, &e.CategoryID, &e.CategoryName,
			&e.Note, &e.Source, &recID, &userID, &e.CreatedByName, &paidByID, &e.PaidByName, &e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			return nil, err
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
		out = append(out, e)
	}
	return out, rows.Err()
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

	err := s.DB.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN kind = 'refund' THEN -amount_cents ELSE amount_cents END), 0),
			COALESCE(SUM(CASE WHEN payment_method = 'cash' THEN (CASE WHEN kind = 'refund' THEN -amount_cents ELSE amount_cents END) ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN payment_method = 'card' THEN (CASE WHEN kind = 'refund' THEN -amount_cents ELSE amount_cents END) ELSE 0 END), 0),
			COUNT(1)
		FROM expenses
		WHERE household_id = ? AND purchase_date >= ? AND purchase_date <= ?
	`, householdID, fromDate, toDate).Scan(&sum.TotalCents, &sum.CashCents, &sum.CardCents, &sum.ExpenseCount)
	if err != nil {
		return sum, err
	}

	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.id, c.name,
		       COALESCE(SUM(CASE WHEN e.kind = 'refund' THEN -e.amount_cents ELSE e.amount_cents END), 0) AS net_cents
		FROM expenses e
		JOIN categories c ON c.id = e.category_id
		WHERE e.household_id = ? AND e.purchase_date >= ? AND e.purchase_date <= ?
		GROUP BY c.id, c.name
		HAVING net_cents != 0
		ORDER BY net_cents DESC, c.name ASC
	`, householdID, fromDate, toDate)
	if err != nil {
		return sum, err
	}
	var maxCat int64
	for rows.Next() {
		var ct CategoryTotal
		if err := rows.Scan(&ct.CategoryID, &ct.CategoryName, &ct.TotalCents); err != nil {
			rows.Close()
			return sum, err
		}
		if ct.TotalCents > maxCat {
			maxCat = ct.TotalCents
		}
		sum.ByCategory = append(sum.ByCategory, ct)
	}
	rows.Close()

	for i := range sum.ByCategory {
		if maxCat > 0 && sum.ByCategory[i].TotalCents > 0 {
			sum.ByCategory[i].SharePercent = float64(sum.ByCategory[i].TotalCents) * 100.0 / float64(maxCat)
		}
	}

	recent, err := s.ListExpenses(ctx, ExpenseFilter{HouseholdID: householdID, FromDate: fromDate, ToDate: toDate, Limit: 8})
	if err != nil {
		return sum, err
	}
	sum.RecentExpenses = recent

	// Build 6-month trend ending at trendEndMonth.
	endT, err := time.ParseInLocation("2006-01", trendEndMonth, loc)
	if err != nil {
		endT = time.Now().In(loc)
	}
	startT := time.Date(endT.Year(), endT.Month()-5, 1, 0, 0, 0, 0, loc)
	endRange := time.Date(endT.Year(), endT.Month()+1, 0, 0, 0, 0, 0, loc)

	tRows, err := s.DB.QueryContext(ctx, `
		SELECT substr(purchase_date, 1, 7) AS ym,
		       COALESCE(SUM(CASE WHEN kind = 'refund' THEN -amount_cents ELSE amount_cents END), 0)
		FROM expenses
		WHERE household_id = ? AND purchase_date >= ? AND purchase_date <= ?
		GROUP BY ym
	`, householdID, startT.Format("2006-01-02"), endRange.Format("2006-01-02"))
	if err != nil {
		return sum, err
	}
	byMonth := map[string]int64{}
	for tRows.Next() {
		var ym string
		var tot int64
		if err := tRows.Scan(&ym, &tot); err != nil {
			tRows.Close()
			return sum, err
		}
		byMonth[ym] = tot
	}
	tRows.Close()

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
