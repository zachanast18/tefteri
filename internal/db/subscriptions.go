package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"spending-project/internal/categorize"
	"spending-project/internal/crypto"
	"spending-project/internal/recurrence"
)

type Subscription struct {
	ID               int64
	HouseholdID      int64
	Name             string
	AmountCents      int64
	Frequency        string // "monthly" or "yearly"
	CategoryID       int64
	CategoryName     string
	PaymentMethod    string // "cash" or "card"
	FirstRenewalDate string
	NextDueDate      string
	AnchorDay        int
	AnchorMonth      int
	Status           string // "active" or "paused"
	IsOverdue        bool
	IsDueToday       bool
	CreatedAt        string
	UpdatedAt        string
}

type SubscriptionOccurrence struct {
	ID              int64
	SubscriptionID  int64
	SubName         string
	ScheduledDate   string
	Status          string
	PaidAmountCents *int64
	PaidDate        *string
	ExpenseID       *int64
	ConfirmedByName string
}

type SaveSubscriptionInput struct {
	HouseholdID      int64
	Name             string
	AmountCents      int64
	Frequency        string
	CategoryID       int64
	PaymentMethod    string
	FirstRenewalDate string
	NextDueDate      string
	Status           string
}

func (s *Store) CreateSubscription(ctx context.Context, in SaveSubscriptionInput, loc *time.Location) (int64, error) {
	first, err := recurrence.ParseDate(in.FirstRenewalDate, loc)
	if err != nil {
		return 0, err
	}
	nextDue := in.NextDueDate
	if nextDue == "" {
		nextDue = in.FirstRenewalDate
	}
	if _, err := recurrence.ParseDate(nextDue, loc); err != nil {
		return 0, err
	}
	status := in.Status
	if status != "paused" {
		status = "active"
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if in.HouseholdID <= 0 {
		in.HouseholdID = 1
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	dek, err := s.ensureDEKTx(ctx, tx, in.HouseholdID)
	if err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (
			household_id, name, amount_cents, frequency, category_id, payment_method,
			first_renewal_date, next_due_date, anchor_day, anchor_month,
			status, enc_version, created_at, updated_at
		) VALUES (?, '', 1, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
	`, in.HouseholdID, in.Frequency, in.CategoryID, in.PaymentMethod,
		in.FirstRenewalDate, nextDue, first.Day(), int(first.Month()), status, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()

	nEnc, _ := crypto.Encrypt(dek, crypto.AAD(in.HouseholdID, "subscriptions", id, "name"), []byte(strings.TrimSpace(in.Name)))
	aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(in.HouseholdID, "subscriptions", id, "amount"), in.AmountCents)
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET name_enc = ?, amount_enc = ? WHERE id = ?`, nEnc, aEnc, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) UpdateSubscription(ctx context.Context, householdID, id int64, in SaveSubscriptionInput, loc *time.Location) error {
	anchorRef, err := recurrence.ParseDate(in.NextDueDate, loc)
	if err != nil {
		return err
	}
	status := in.Status
	if status != "paused" {
		status = "active"
	}
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return err
	}
	nEnc, _ := crypto.Encrypt(dek, crypto.AAD(householdID, "subscriptions", id, "name"), []byte(strings.TrimSpace(in.Name)))
	aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(householdID, "subscriptions", id, "amount"), in.AmountCents)

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.ExecContext(ctx, `
		UPDATE subscriptions
		SET name = '', amount_cents = 1, name_enc = ?, amount_enc = ?, enc_version = 1,
		    frequency = ?, category_id = ?,
		    payment_method = ?, next_due_date = ?, anchor_day = ?, anchor_month = ?,
		    status = ?, updated_at = ?
		WHERE id = ? AND household_id = ?
	`, nEnc, aEnc, in.Frequency, in.CategoryID,
		in.PaymentMethod, in.NextDueDate, anchorRef.Day(), int(anchorRef.Month()),
		status, now, id, householdID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetSubscriptionStatus(ctx context.Context, householdID, id int64, status string) error {
	if status != "active" && status != "paused" {
		return errors.New("invalid subscription status")
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE subscriptions SET status = ?, updated_at = ? WHERE id = ? AND household_id = ?
	`, status, time.Now().UTC().Format(time.RFC3339), id, householdID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteSubscription(ctx context.Context, householdID, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ? AND household_id = ?`, id, householdID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetSubscription(ctx context.Context, householdID, id int64, today string) (Subscription, error) {
	var sub Subscription
	var nEnc, aEnc []byte
	var ev int
	err := s.DB.QueryRowContext(ctx, `
		SELECT s.id, s.household_id, s.name, s.amount_cents, s.frequency, s.category_id, c.name,
		       s.payment_method, s.first_renewal_date, s.next_due_date, s.anchor_day, s.anchor_month,
		       s.status, s.name_enc, s.amount_enc, s.enc_version, s.created_at, s.updated_at
		FROM subscriptions s
		JOIN categories c ON c.id = s.category_id
		WHERE s.id = ? AND s.household_id = ?
	`, id, householdID).Scan(
		&sub.ID, &sub.HouseholdID, &sub.Name, &sub.AmountCents, &sub.Frequency, &sub.CategoryID, &sub.CategoryName,
		&sub.PaymentMethod, &sub.FirstRenewalDate, &sub.NextDueDate, &sub.AnchorDay, &sub.AnchorMonth,
		&sub.Status, &nEnc, &aEnc, &ev, &sub.CreatedAt, &sub.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return sub, ErrNotFound
	}
	if err != nil {
		return sub, err
	}
	if ev == 1 {
		dek, err := s.EnsureHouseholdDEK(ctx, householdID)
		if err != nil {
			return sub, err
		}
		if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "subscriptions", sub.ID, "name"), nEnc); err == nil {
			sub.Name = string(pt)
		}
		if amt, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "subscriptions", sub.ID, "amount"), aEnc); err == nil {
			sub.AmountCents = amt
		}
	}
	if sub.Status == "active" {
		sub.IsOverdue = sub.NextDueDate < today
		sub.IsDueToday = sub.NextDueDate == today
	}
	return sub, nil
}

// ListSubscriptions returns active subscriptions ordered with due/overdue first,
// then upcoming by next_due_date, followed by paused subscriptions.
func (s *Store) ListSubscriptions(ctx context.Context, householdID int64, today string) ([]Subscription, error) {
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT s.id, s.household_id, s.name, s.amount_cents, s.frequency, s.category_id, c.name,
		       s.payment_method, s.first_renewal_date, s.next_due_date, s.anchor_day, s.anchor_month,
		       s.status, s.name_enc, s.amount_enc, s.enc_version, s.created_at, s.updated_at
		FROM subscriptions s
		JOIN categories c ON c.id = s.category_id
		WHERE s.household_id = ?
		ORDER BY
			CASE WHEN s.status = 'active' THEN 0 ELSE 1 END ASC,
			CASE WHEN s.status = 'active' AND s.next_due_date <= ? THEN 0 ELSE 1 END ASC,
			s.next_due_date ASC,
			s.id ASC
	`, householdID, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Subscription
	for rows.Next() {
		var sub Subscription
		var nEnc, aEnc []byte
		var ev int
		if err := rows.Scan(
			&sub.ID, &sub.HouseholdID, &sub.Name, &sub.AmountCents, &sub.Frequency, &sub.CategoryID, &sub.CategoryName,
			&sub.PaymentMethod, &sub.FirstRenewalDate, &sub.NextDueDate, &sub.AnchorDay, &sub.AnchorMonth,
			&sub.Status, &nEnc, &aEnc, &ev, &sub.CreatedAt, &sub.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if ev == 1 {
			if dek == nil {
				dek, err = s.EnsureHouseholdDEK(ctx, householdID)
				if err != nil {
					return nil, err
				}
			}
			if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "subscriptions", sub.ID, "name"), nEnc); err == nil {
				sub.Name = string(pt)
			}
			if amt, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "subscriptions", sub.ID, "amount"), aEnc); err == nil {
				sub.AmountCents = amt
			}
		}
		if sub.Status == "active" {
			sub.IsOverdue = sub.NextDueDate < today
			sub.IsDueToday = sub.NextDueDate == today
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// EstimatedMonthlySubscriptionCost calculates active monthly + yearly/12 in minor units.
func (s *Store) EstimatedMonthlySubscriptionCost(ctx context.Context, householdID int64) (int64, error) {
	subs, err := s.ListSubscriptions(ctx, householdID, "")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, sub := range subs {
		if sub.Status != "active" {
			continue
		}
		if sub.Frequency == "monthly" {
			total += sub.AmountCents
		} else if sub.Frequency == "yearly" {
			total += (sub.AmountCents + 6) / 12
		}
	}
	return total, nil
}

func (s *Store) ConfirmRenewal(ctx context.Context, householdID, subscriptionID int64, scheduledDate string, paidAmountCents int64, paidDate string, loc *time.Location) (int64, error) {
	return s.ConfirmRenewalBy(ctx, householdID, subscriptionID, scheduledDate, paidAmountCents, paidDate, loc, nil)
}

func (s *Store) ConfirmRenewalBy(ctx context.Context, householdID, subscriptionID int64, scheduledDate string, paidAmountCents int64, paidDate string, loc *time.Location, confirmedByUserID *int64) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var sub Subscription
	var nEnc, aEnc []byte
	var ev int
	err = tx.QueryRowContext(ctx, `
		SELECT id, household_id, name, amount_cents, frequency, category_id, payment_method,
		       next_due_date, anchor_day, anchor_month, status, name_enc, amount_enc, enc_version
		FROM subscriptions WHERE id = ? AND household_id = ?
	`, subscriptionID, householdID).Scan(
		&sub.ID, &sub.HouseholdID, &sub.Name, &sub.AmountCents, &sub.Frequency, &sub.CategoryID,
		&sub.PaymentMethod, &sub.NextDueDate, &sub.AnchorDay, &sub.AnchorMonth, &sub.Status,
		&nEnc, &aEnc, &ev,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	dek, err := s.ensureDEKTx(ctx, tx, householdID)
	if err != nil {
		return 0, err
	}
	if ev == 1 {
		if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "subscriptions", sub.ID, "name"), nEnc); err == nil {
			sub.Name = string(pt)
		}
		if amt, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "subscriptions", sub.ID, "amount"), aEnc); err == nil {
			sub.AmountCents = amt
		}
	}

	if scheduledDate == "" {
		scheduledDate = sub.NextDueDate
	}
	if paidAmountCents <= 0 {
		paidAmountCents = sub.AmountCents
	}
	if paidDate == "" {
		paidDate = scheduledDate
	}

	// Check if already confirmed or skipped for this (subscription_id, scheduled_date).
	var existing int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM subscription_occurrences WHERE subscription_id = ? AND scheduled_date = ?
	`, subscriptionID, scheduledDate).Scan(&existing)
	if err != nil {
		return 0, err
	}
	if existing > 0 {
		return 0, ErrAlreadyConfirmed
	}

	now := time.Now().UTC().Format(time.RFC3339)
	norm := categorize.NormalizeMerchant(sub.Name)
	hm := crypto.BlindIndex(dek, "merchant", norm)

	expRes, err := tx.ExecContext(ctx, `
		INSERT INTO expenses (
			household_id, kind, amount_cents, merchant, normalized_merchant, purchase_date,
			payment_method, category_id, note, source, created_by_user_id, paid_by_user_id, enc_version, created_at, updated_at
		) VALUES (?, 'expense', 1, '', ?, ?, ?, ?, '', 'subscription', ?, ?, 1, ?, ?)
	`, sub.HouseholdID, hm, paidDate, sub.PaymentMethod, sub.CategoryID,
		confirmedByUserID, confirmedByUserID, now, now)
	if err != nil {
		return 0, err
	}
	expenseID, _ := expRes.LastInsertId()

	mEnc, _ := crypto.Encrypt(dek, crypto.AAD(sub.HouseholdID, "expenses", expenseID, "merchant"), []byte(sub.Name))
	nEncExp, _ := crypto.Encrypt(dek, crypto.AAD(sub.HouseholdID, "expenses", expenseID, "note"), []byte("Subscription renewal ("+scheduledDate+")"))
	aEncExp, _ := crypto.EncryptInt64(dek, crypto.AAD(sub.HouseholdID, "expenses", expenseID, "amount"), paidAmountCents)
	if _, err := tx.ExecContext(ctx, `UPDATE expenses SET merchant_enc = ?, note_enc = ?, amount_enc = ? WHERE id = ?`, mEnc, nEncExp, aEncExp, expenseID); err != nil {
		return 0, err
	}

	paidEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(sub.HouseholdID, "subscription_occurrences", fmt.Sprintf("%d:%s", subscriptionID, scheduledDate), "paid_amount"), paidAmountCents)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_occurrences (
			subscription_id, scheduled_date, status, paid_amount_cents, paid_amount_enc, paid_date, expense_id, confirmed_by_user_id, enc_version, created_at
		) VALUES (?, ?, 'paid', 0, ?, ?, ?, ?, 1, ?)
	`, subscriptionID, scheduledDate, paidEnc, paidDate, expenseID, confirmedByUserID, now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrAlreadyConfirmed
		}
		return 0, err
	}

	if scheduledDate == sub.NextDueDate {
		nextDue, err := recurrence.NextDate(sub.NextDueDate, sub.Frequency, sub.AnchorDay, sub.AnchorMonth, loc)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions SET next_due_date = ?, updated_at = ? WHERE id = ?
		`, nextDue, now, subscriptionID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return expenseID, nil
}

// SkipRenewal marks the scheduled renewal as skipped without creating an expense and advances next_due_date.
func (s *Store) SkipRenewal(ctx context.Context, householdID, subscriptionID int64, scheduledDate string, loc *time.Location) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var sub Subscription
	err = tx.QueryRowContext(ctx, `
		SELECT id, frequency, next_due_date, anchor_day, anchor_month
		FROM subscriptions WHERE id = ? AND household_id = ?
	`, subscriptionID, householdID).Scan(&sub.ID, &sub.Frequency, &sub.NextDueDate, &sub.AnchorDay, &sub.AnchorMonth)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if scheduledDate == "" {
		scheduledDate = sub.NextDueDate
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_occurrences (subscription_id, scheduled_date, status, created_at)
		VALUES (?, ?, 'skipped', ?)
	`, subscriptionID, scheduledDate, now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrAlreadyConfirmed
		}
		return err
	}

	if scheduledDate == sub.NextDueDate {
		nextDue, err := recurrence.NextDate(sub.NextDueDate, sub.Frequency, sub.AnchorDay, sub.AnchorMonth, loc)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions SET next_due_date = ?, updated_at = ? WHERE id = ?
		`, nextDue, now, subscriptionID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) ListRecentOccurrences(ctx context.Context, householdID int64, limit int) ([]SubscriptionOccurrence, error) {
	dek, err := s.EnsureHouseholdDEK(ctx, householdID)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT o.id, o.subscription_id, s.name, s.name_enc, s.enc_version, o.scheduled_date, o.status,
		       o.paid_amount_cents, o.paid_amount_enc, o.enc_version, o.paid_date, o.expense_id,
		       COALESCE(NULLIF(u.display_name, ''), u.username, '')
		FROM subscription_occurrences o
		JOIN subscriptions s ON s.id = o.subscription_id
		LEFT JOIN users u ON u.id = o.confirmed_by_user_id
		WHERE s.household_id = ?
		ORDER BY o.id DESC
		LIMIT ?
	`, householdID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SubscriptionOccurrence
	for rows.Next() {
		var o SubscriptionOccurrence
		var sNameEnc, paidEnc []byte
		var sEv, oEv int
		var paidCents sql.NullInt64
		var paidDate sql.NullString
		var expID sql.NullInt64
		if err := rows.Scan(&o.ID, &o.SubscriptionID, &o.SubName, &sNameEnc, &sEv, &o.ScheduledDate, &o.Status, &paidCents, &paidEnc, &oEv, &paidDate, &expID, &o.ConfirmedByName); err != nil {
			return nil, err
		}
		if sEv == 1 {
			if pt, err := crypto.Decrypt(dek, crypto.AAD(householdID, "subscriptions", o.SubscriptionID, "name"), sNameEnc); err == nil {
				o.SubName = string(pt)
			}
		}
		if oEv == 1 && len(paidEnc) > 0 {
			if amt, err := crypto.DecryptInt64(dek, crypto.AAD(householdID, "subscription_occurrences", fmt.Sprintf("%d:%s", o.SubscriptionID, o.ScheduledDate), "paid_amount"), paidEnc); err == nil {
				o.PaidAmountCents = &amt
			}
		} else if paidCents.Valid {
			v := paidCents.Int64
			o.PaidAmountCents = &v
		}
		if paidDate.Valid {
			v := paidDate.String
			o.PaidDate = &v
		}
		if expID.Valid {
			v := expID.Int64
			o.ExpenseID = &v
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
