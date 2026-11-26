package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"spending-project/internal/crypto"
)

type ExpenseSplit struct {
	UserID     int64
	UserName   string
	ShareCents int64
}

type MemberBalance struct {
	UserID       int64
	Name         string
	BalanceCents int64 // positive = owed to member; negative = member owes
}

type Settlement struct {
	ID          int64
	HouseholdID int64
	FromUserID  int64
	FromName    string
	ToUserID    int64
	ToName      string
	AmountCents int64
	SettledDate string
	Note        string
}

// ComputeEqualSplits divides amountCents evenly across userIDs, assigning any
// remainder cents (+1) to the first users in order so the sum matches amountCents.
func ComputeEqualSplits(amountCents int64, userIDs []int64) []ExpenseSplit {
	n := int64(len(userIDs))
	if n == 0 || amountCents <= 0 {
		return nil
	}
	base := amountCents / n
	rem := amountCents % n
	out := make([]ExpenseSplit, 0, len(userIDs))
	for i, uid := range userIDs {
		share := base
		if int64(i) < rem {
			share++
		}
		out = append(out, ExpenseSplit{UserID: uid, ShareCents: share})
	}
	return out
}

func (s *Store) GetExpenseSplits(ctx context.Context, expenseID int64) ([]ExpenseSplit, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT e.household_id, sp.user_id, COALESCE(NULLIF(u.display_name, ''), u.username, ''), sp.share_cents, sp.share_enc, sp.enc_version
		FROM expense_splits sp
		JOIN expenses e ON e.id = sp.expense_id
		JOIN users u ON u.id = sp.user_id
		WHERE sp.expense_id = ?
		ORDER BY sp.user_id ASC
	`, expenseID)
	if err != nil {
		return nil, err
	}

	type rawSplit struct {
		hhID int64
		sp   ExpenseSplit
		enc  []byte
		ev   int
	}
	var raw []rawSplit
	for rows.Next() {
		var r rawSplit
		if err := rows.Scan(&r.hhID, &r.sp.UserID, &r.sp.UserName, &r.sp.ShareCents, &r.enc, &r.ev); err != nil {
			rows.Close()
			return nil, err
		}
		raw = append(raw, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var dek []byte
	out := make([]ExpenseSplit, 0, len(raw))
	for _, r := range raw {
		if r.ev == 1 {
			if dek == nil {
				dek, err = s.EnsureHouseholdDEK(ctx, r.hhID)
				if err != nil {
					return nil, err
				}
			}
			if v, err := crypto.DecryptInt64(dek, crypto.AAD(r.hhID, "expense_splits", fmt.Sprintf("%d:%d", expenseID, r.sp.UserID), "share"), r.enc); err == nil {
				r.sp.ShareCents = v
			}
		}
		out = append(out, r.sp)
	}
	return out, nil
}

// ComputeHouseholdBalances calculates each member's running balance from split expenses
// and recorded settlements in the household. Unsplit expenses do not affect member balances.
func (s *Store) ComputeHouseholdBalances(ctx context.Context, householdID int64) ([]MemberBalance, error) {
	members, err := s.ListHouseholdMembers(ctx, householdID)
	if err != nil {
		return nil, err
	}
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return nil, err
	}
	balMap := make(map[int64]int64, len(members))
	for _, m := range members {
		balMap[m.UserID] = 0
	}

	// Load split expenses in Go and decrypt amounts.
	pRows, err := s.DB.QueryContext(ctx, `
		SELECT e.id, e.kind, e.paid_by_user_id, e.amount_cents, e.amount_enc, e.enc_version
		FROM expenses e
		WHERE e.household_id = ?
		  AND e.paid_by_user_id IS NOT NULL
		  AND EXISTS (SELECT 1 FROM expense_splits sp WHERE sp.expense_id = e.id)
	`, householdID)
	if err != nil {
		return nil, err
	}
	expKinds := map[int64]string{}
	for pRows.Next() {
		var eid, uid, amt int64
		var kind string
		var aEnc []byte
		var ev int
		if err := pRows.Scan(&eid, &kind, &uid, &amt, &aEnc, &ev); err != nil {
			pRows.Close()
			return nil, err
		}
		if ev == 1 {
			if v, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "expenses", eid, "amount"), aEnc); err == nil {
				amt = v
			}
		}
		expKinds[eid] = kind
		if kind == "refund" {
			balMap[uid] -= amt
		} else {
			balMap[uid] += amt
		}
	}
	pRows.Close()

	// Debit split participants.
	sRows, err := s.DB.QueryContext(ctx, `
		SELECT sp.expense_id, sp.user_id, sp.share_cents, sp.share_enc, sp.enc_version, e.kind
		FROM expense_splits sp
		JOIN expenses e ON e.id = sp.expense_id
		WHERE e.household_id = ?
	`, householdID)
	if err != nil {
		return nil, err
	}
	for sRows.Next() {
		var eid, uid, share int64
		var sEnc []byte
		var ev int
		var kind string
		if err := sRows.Scan(&eid, &uid, &share, &sEnc, &ev, &kind); err != nil {
			sRows.Close()
			return nil, err
		}
		if ev == 1 {
			if v, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "expense_splits", fmt.Sprintf("%d:%d", eid, uid), "share"), sEnc); err == nil {
				share = v
			}
		}
		if kind == "refund" {
			balMap[uid] += share
		} else {
			balMap[uid] -= share
		}
	}
	sRows.Close()

	// Apply settlements (from_user pays to_user: from_user balance rises, to_user balance drops).
	stRows, err := s.DB.QueryContext(ctx, `
		SELECT id, from_user_id, to_user_id, amount_cents, amount_enc, enc_version
		FROM settlements
		WHERE household_id = ?
	`, householdID)
	if err != nil {
		return nil, err
	}
	for stRows.Next() {
		var sid, fromID, toID, amt int64
		var aEnc []byte
		var ev int
		if err := stRows.Scan(&sid, &fromID, &toID, &amt, &aEnc, &ev); err != nil {
			stRows.Close()
			return nil, err
		}
		if ev == 1 {
			if v, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "settlements", sid, "amount"), aEnc); err == nil {
				amt = v
			}
		}
		balMap[fromID] += amt
		balMap[toID] -= amt
	}
	stRows.Close()

	out := make([]MemberBalance, 0, len(members))
	for _, m := range members {
		out = append(out, MemberBalance{
			UserID:       m.UserID,
			Name:         m.Label(),
			BalanceCents: balMap[m.UserID],
		})
	}
	return out, nil
}

func (s *Store) RecordSettlement(ctx context.Context, householdID, fromUserID, toUserID, amountCents int64, settledDate, note string, createdByUserID *int64) (int64, error) {
	if fromUserID <= 0 || toUserID <= 0 || fromUserID == toUserID {
		return 0, errors.New("choose two different household members")
	}
	if amountCents <= 0 {
		return 0, errors.New("settlement amount must be greater than zero")
	}
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
		INSERT INTO settlements (household_id, from_user_id, to_user_id, amount_cents, settled_date, note, created_by_user_id, enc_version, created_at)
		VALUES (?, ?, ?, 1, ?, '', ?, 1, ?)
	`, householdID, fromUserID, toUserID, settledDate, createdByUserID, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(householdID, "settlements", id, "amount"), amountCents)
	nEnc, _ := crypto.Encrypt(dek, crypto.AAD(householdID, "settlements", id, "note"), []byte(strings.TrimSpace(note)))
	if _, err := tx.ExecContext(ctx, `UPDATE settlements SET amount_enc = ?, note_enc = ? WHERE id = ?`, aEnc, nEnc, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) ListSettlements(ctx context.Context, householdID int64, limit int) ([]Settlement, error) {
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT st.id, st.household_id, st.from_user_id,
		       COALESCE(NULLIF(u1.display_name, ''), u1.username, ''),
		       st.to_user_id,
		       COALESCE(NULLIF(u2.display_name, ''), u2.username, ''),
		       st.amount_cents, st.settled_date, st.note, st.amount_enc, st.note_enc, st.enc_version
		FROM settlements st
		JOIN users u1 ON u1.id = st.from_user_id
		JOIN users u2 ON u2.id = st.to_user_id
		WHERE st.household_id = ?
		ORDER BY st.settled_date DESC, st.id DESC
		LIMIT ?
	`, householdID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Settlement
	for rows.Next() {
		var st Settlement
		var aEnc, nEnc []byte
		var ev int
		if err := rows.Scan(&st.ID, &st.HouseholdID, &st.FromUserID, &st.FromName, &st.ToUserID, &st.ToName, &st.AmountCents, &st.SettledDate, &st.Note, &aEnc, &nEnc, &ev); err != nil {
			return nil, err
		}
		if ev == 1 {
			if v, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "settlements", st.ID, "amount"), aEnc); err == nil {
				st.AmountCents = v
			}
			if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "settlements", st.ID, "note"), nEnc); err == nil {
				st.Note = string(pt)
			}
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) UpdateDraftInbox(ctx context.Context, householdID int64, d ReceiptDraft) error {
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return err
	}
	mEnc, _ := crypto.Encrypt(dek, crypto.AAD(householdID, "receipt_drafts", d.ID, "merchant"), []byte(strings.TrimSpace(d.Merchant)))
	aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(householdID, "receipt_drafts", d.ID, "amount"), d.AmountCents)
	res, err := s.DB.ExecContext(ctx, `
		UPDATE receipt_drafts
		SET merchant = '', purchase_date = ?, amount_cents = 0, payment_method = ?,
		    category_id = ?, needs_review = 1, review_reason = ?,
		    merchant_enc = ?, amount_enc = ?, enc_version = 1
		WHERE id = ? AND household_id = ?
	`, d.PurchaseDate, d.PaymentMethod,
		nullableInt64(d.CategoryID), strings.TrimSpace(d.ReviewReason), mEnc, aEnc, d.ID, householdID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListReviewDrafts(ctx context.Context, householdID int64) ([]ReceiptDraft, error) {
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, household_id, receipt_image_id, merchant, purchase_date, amount_cents,
		       payment_method, COALESCE(category_id, 0), check_merchant, check_date, check_amount,
		       ocr_status, ocr_error, needs_review, review_reason,
		       merchant_enc, amount_enc, ocr_error_enc, enc_version
		FROM receipt_drafts
		WHERE household_id = ? AND needs_review = 1
		ORDER BY created_at DESC
	`, householdID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReceiptDraft
	for rows.Next() {
		var d ReceiptDraft
		var cm, cd, ca, nr, ev int
		var mEnc, aEnc, eEnc []byte
		if err := rows.Scan(
			&d.ID, &d.HouseholdID, &d.ReceiptImageID, &d.Merchant, &d.PurchaseDate, &d.AmountCents,
			&d.PaymentMethod, &d.CategoryID, &cm, &cd, &ca,
			&d.OCRStatus, &d.OCRError, &nr, &d.ReviewReason,
			&mEnc, &aEnc, &eEnc, &ev,
		); err != nil {
			return nil, err
		}
		d.CheckMerchant = cm == 1
		d.CheckDate = cd == 1
		d.CheckAmount = ca == 1
		d.NeedsReview = nr == 1
		if ev == 1 {
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
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) CountReviewDrafts(ctx context.Context, householdID int64) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM receipt_drafts WHERE household_id = ? AND needs_review = 1`, householdID).Scan(&n)
	return n, err
}

func (s *Store) DeleteDraftScoped(ctx context.Context, householdID int64, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM receipt_drafts WHERE id = ? AND household_id = ?`, id, householdID)
	return err
}

func (s *Store) SetCategoryBudget(ctx context.Context, householdID, categoryID, limitCents int64) error {
	if limitCents <= 0 {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM category_budgets WHERE household_id = ? AND category_id = ?`, householdID, categoryID)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO category_budgets (household_id, category_id, monthly_limit_cents)
		VALUES (?, ?, ?)
		ON CONFLICT(household_id, category_id) DO UPDATE SET monthly_limit_cents = excluded.monthly_limit_cents
	`, householdID, categoryID, limitCents)
	return err
}

func (s *Store) ListCategoryBudgets(ctx context.Context, householdID int64) (map[int64]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT category_id, monthly_limit_cents FROM category_budgets WHERE household_id = ?
	`, householdID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]int64)
	for rows.Next() {
		var cid, lim int64
		if err := rows.Scan(&cid, &lim); err != nil {
			return nil, err
		}
		out[cid] = lim
	}
	return out, rows.Err()
}
