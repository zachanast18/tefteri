package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"spending-project/internal/categorize"
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
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO subscriptions (
			household_id, name, amount_cents, frequency, category_id, payment_method,
			first_renewal_date, next_due_date, anchor_day, anchor_month,
			status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, in.HouseholdID, strings.TrimSpace(in.Name), in.AmountCents, in.Frequency, in.CategoryID, in.PaymentMethod,
		in.FirstRenewalDate, nextDue, first.Day(), int(first.Month()), status, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
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
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.ExecContext(ctx, `
		UPDATE subscriptions
		SET name = ?, amount_cents = ?, frequency = ?, category_id = ?,
		    payment_method = ?, next_due_date = ?, anchor_day = ?, anchor_month = ?,
		    status = ?, updated_at = ?
		WHERE id = ? AND household_id = ?
	`, strings.TrimSpace(in.Name), in.AmountCents, in.Frequency, in.CategoryID,
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
	err := s.DB.QueryRowContext(ctx, `
		SELECT s.id, s.household_id, s.name, s.amount_cents, s.frequency, s.category_id, c.name,
		       s.payment_method, s.first_renewal_date, s.next_due_date, s.anchor_day, s.anchor_month,
		       s.status, s.created_at, s.updated_at
		FROM subscriptions s
		JOIN categories c ON c.id = s.category_id
		WHERE s.id = ? AND s.household_id = ?
	`, id, householdID).Scan(
		&sub.ID, &sub.HouseholdID, &sub.Name, &sub.AmountCents, &sub.Frequency, &sub.CategoryID, &sub.CategoryName,
		&sub.PaymentMethod, &sub.FirstRenewalDate, &sub.NextDueDate, &sub.AnchorDay, &sub.AnchorMonth,
		&sub.Status, &sub.CreatedAt, &sub.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return sub, ErrNotFound
	}
	if err != nil {
		return sub, err
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
	rows, err := s.DB.QueryContext(ctx, `
		SELECT s.id, s.household_id, s.name, s.amount_cents, s.frequency, s.category_id, c.name,
		       s.payment_method, s.first_renewal_date, s.next_due_date, s.anchor_day, s.anchor_month,
		       s.status, s.created_at, s.updated_at
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
		if err := rows.Scan(
			&sub.ID, &sub.HouseholdID, &sub.Name, &sub.AmountCents, &sub.Frequency, &sub.CategoryID, &sub.CategoryName,
			&sub.PaymentMethod, &sub.FirstRenewalDate, &sub.NextDueDate, &sub.AnchorDay, &sub.AnchorMonth,
			&sub.Status, &sub.CreatedAt, &sub.UpdatedAt,
		); err != nil {
			return nil, err
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
	var total int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
			CASE WHEN frequency = 'monthly' THEN amount_cents
			     WHEN frequency = 'yearly' THEN (amount_cents + 6) / 12
			     ELSE 0 END
		), 0)
		FROM subscriptions
		WHERE household_id = ? AND status = 'active'
	`, householdID).Scan(&total)
	return total, err
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
	err = tx.QueryRowContext(ctx, `
		SELECT id, household_id, name, amount_cents, frequency, category_id, payment_method,
		       next_due_date, anchor_day, anchor_month, status
		FROM subscriptions WHERE id = ? AND household_id = ?
	`, subscriptionID, householdID).Scan(
		&sub.ID, &sub.HouseholdID, &sub.Name, &sub.AmountCents, &sub.Frequency, &sub.CategoryID,
		&sub.PaymentMethod, &sub.NextDueDate, &sub.AnchorDay, &sub.AnchorMonth, &sub.Status,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
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

	expRes, err := tx.ExecContext(ctx, `
		INSERT INTO expenses (
			household_id, kind, amount_cents, merchant, normalized_merchant, purchase_date,
			payment_method, category_id, note, source, created_by_user_id, paid_by_user_id, created_at, updated_at
		) VALUES (?, 'expense', ?, ?, ?, ?, ?, ?, ?, 'subscription', ?, ?, ?, ?)
	`, sub.HouseholdID, paidAmountCents, sub.Name, norm, paidDate, sub.PaymentMethod, sub.CategoryID,
		"Subscription renewal ("+scheduledDate+")", confirmedByUserID, confirmedByUserID, now, now)
	if err != nil {
		return 0, err
	}
	expenseID, _ := expRes.LastInsertId()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_occurrences (
			subscription_id, scheduled_date, status, paid_amount_cents, paid_date, expense_id, confirmed_by_user_id, created_at
		) VALUES (?, ?, 'paid', ?, ?, ?, ?, ?)
	`, subscriptionID, scheduledDate, paidAmountCents, paidDate, expenseID, confirmedByUserID, now); err != nil {
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
	rows, err := s.DB.QueryContext(ctx, `
		SELECT o.id, o.subscription_id, s.name, o.scheduled_date, o.status,
		       o.paid_amount_cents, o.paid_date, o.expense_id,
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
		var paidCents sql.NullInt64
		var paidDate sql.NullString
		var expID sql.NullInt64
		if err := rows.Scan(&o.ID, &o.SubscriptionID, &o.SubName, &o.ScheduledDate, &o.Status, &paidCents, &paidDate, &expID, &o.ConfirmedByName); err != nil {
			return nil, err
		}
		if paidCents.Valid {
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
