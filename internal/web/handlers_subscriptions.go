package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"spending-project/internal/auth"
	"spending-project/internal/db"
	"spending-project/internal/money"
	"spending-project/internal/recurrence"
)

type subscriptionsPageData struct {
	BasePage
	Today               string
	Subscriptions       []db.Subscription
	Occurrences         []db.SubscriptionOccurrence
	Categories          []db.Category
	EstMonthlyFormatted string
	EditSub             *db.Subscription
	Error               string
}

func (s *Server) handleSubscriptionsGet(w http.ResponseWriter, r *http.Request) {
	s.renderSubscriptions(w, r, http.StatusOK, "")
}

func (s *Server) renderSubscriptions(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	u, _ := s.currentUser(r)
	hhID := u.ActiveHouseholdID
	today := recurrence.Today(s.nowFn(), s.cfg.Location)
	subs, err := s.store.ListSubscriptions(r.Context(), hhID, today)
	if err != nil {
		http.Error(w, "failed to load subscriptions", http.StatusInternalServerError)
		return
	}
	cats, _ := s.store.ListCategories(r.Context(), hhID)
	occs, _ := s.store.ListRecentOccurrences(r.Context(), hhID, 15)
	estMonthly, _ := s.store.EstimatedMonthlySubscriptionCost(r.Context(), hhID)

	var editSub *db.Subscription
	if editIDStr := r.URL.Query().Get("edit"); editIDStr != "" {
		if editID, err := strconv.ParseInt(editIDStr, 10, 64); err == nil {
			if sub, err := s.store.GetSubscription(r.Context(), hhID, editID, today); err == nil {
				editSub = &sub
			}
		}
	}

	s.render(w, r, "subscriptions.html", status, subscriptionsPageData{
		BasePage:            s.newBasePage(w, r, "Subscriptions", "subscriptions", true),
		Today:               today,
		Subscriptions:       subs,
		Occurrences:         occs,
		Categories:          cats,
		EstMonthlyFormatted: money.Format(estMonthly, s.cfg.CurrencySymbol),
		EditSub:             editSub,
		Error:               errMsg,
	})
}

func (s *Server) handleSubscriptionCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	cents, err := money.ParsePositive(r.FormValue("amount"))
	freq := r.FormValue("frequency")
	firstDate := strings.TrimSpace(r.FormValue("first_renewal_date"))
	catID, _ := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	pay := r.FormValue("payment_method")
	status := r.FormValue("status")

	if name == "" || err != nil || (freq != "monthly" && freq != "yearly") || (pay != "cash" && pay != "card") || catID <= 0 {
		s.renderSubscriptions(w, r, http.StatusUnprocessableEntity, "Please fill all subscription fields with a valid amount and date.")
		return
	}
	if _, err := recurrence.ParseDate(firstDate, s.cfg.Location); err != nil {
		s.renderSubscriptions(w, r, http.StatusUnprocessableEntity, "Enter a valid first renewal date.")
		return
	}

	u, _ := s.currentUser(r)
	if _, err := s.store.CreateSubscription(r.Context(), db.SaveSubscriptionInput{
		HouseholdID:      u.ActiveHouseholdID,
		Name:             name,
		AmountCents:      cents,
		Frequency:        freq,
		CategoryID:       catID,
		PaymentMethod:    pay,
		FirstRenewalDate: firstDate,
		NextDueDate:      firstDate,
		Status:           status,
	}, s.cfg.Location); err != nil {
		s.renderSubscriptions(w, r, http.StatusInternalServerError, "Could not save subscription.")
		return
	}

	s.setFlash(w, r, "Subscription added.")
	http.Redirect(w, r, "/subscriptions", http.StatusSeeOther)
}

func (s *Server) handleSubscriptionEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	cents, err := money.ParsePositive(r.FormValue("amount"))
	freq := r.FormValue("frequency")
	nextDue := strings.TrimSpace(r.FormValue("next_due_date"))
	catID, _ := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	pay := r.FormValue("payment_method")
	status := r.FormValue("status")

	if name == "" || err != nil || (freq != "monthly" && freq != "yearly") || (pay != "cash" && pay != "card") || catID <= 0 {
		s.renderSubscriptions(w, r, http.StatusUnprocessableEntity, "Please fill all subscription fields with valid values.")
		return
	}
	if _, err := recurrence.ParseDate(nextDue, s.cfg.Location); err != nil {
		s.renderSubscriptions(w, r, http.StatusUnprocessableEntity, "Enter a valid next renewal date.")
		return
	}

	u, _ := s.currentUser(r)
	if err := s.store.UpdateSubscription(r.Context(), u.ActiveHouseholdID, id, db.SaveSubscriptionInput{
		Name:          name,
		AmountCents:   cents,
		Frequency:     freq,
		CategoryID:    catID,
		PaymentMethod: pay,
		NextDueDate:   nextDue,
		Status:        status,
	}, s.cfg.Location); err != nil {
		s.renderSubscriptions(w, r, http.StatusInternalServerError, "Could not update subscription.")
		return
	}

	s.setFlash(w, r, "Subscription schedule updated.")
	http.Redirect(w, r, "/subscriptions", http.StatusSeeOther)
}

func (s *Server) handleSubscriptionConfirm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	scheduledDate := strings.TrimSpace(r.FormValue("scheduled_date"))
	paidDate := strings.TrimSpace(r.FormValue("paid_date"))
	paidCents, err := money.ParsePositive(r.FormValue("paid_amount"))
	if err != nil {
		s.renderSubscriptions(w, r, http.StatusUnprocessableEntity, "Enter a valid paid amount.")
		return
	}
	if _, err := recurrence.ParseDate(paidDate, s.cfg.Location); err != nil {
		s.renderSubscriptions(w, r, http.StatusUnprocessableEntity, "Enter a valid paid date.")
		return
	}

	var userPtr *int64
	u, ok := s.currentUser(r)
	if ok && u.ID > 0 {
		uid := u.ID
		userPtr = &uid
	}

	_, err = s.store.ConfirmRenewalBy(r.Context(), u.ActiveHouseholdID, id, scheduledDate, paidCents, paidDate, s.cfg.Location, userPtr)
	if errors.Is(err, db.ErrAlreadyConfirmed) {
		s.setFlash(w, r, "This renewal was already recorded.")
		http.Redirect(w, r, "/subscriptions", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.renderSubscriptions(w, r, http.StatusInternalServerError, "Could not record renewal payment.")
		return
	}

	s.setFlash(w, r, "Renewal payment recorded in ledger.")
	http.Redirect(w, r, "/subscriptions", http.StatusSeeOther)
}

func (s *Server) handleSubscriptionSkip(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	u, _ := s.currentUser(r)
	scheduledDate := strings.TrimSpace(r.FormValue("scheduled_date"))
	err = s.store.SkipRenewal(r.Context(), u.ActiveHouseholdID, id, scheduledDate, s.cfg.Location)
	if errors.Is(err, db.ErrAlreadyConfirmed) {
		s.setFlash(w, r, "This renewal was already handled.")
		http.Redirect(w, r, "/subscriptions", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.renderSubscriptions(w, r, http.StatusInternalServerError, "Could not skip renewal.")
		return
	}

	s.setFlash(w, r, "Renewal skipped.")
	http.Redirect(w, r, "/subscriptions", http.StatusSeeOther)
}

func (s *Server) handleSubscriptionStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	u, _ := s.currentUser(r)
	status := r.FormValue("status")
	if err := s.store.SetSubscriptionStatus(r.Context(), u.ActiveHouseholdID, id, status); err != nil {
		s.renderSubscriptions(w, r, http.StatusBadRequest, "Could not update subscription status.")
		return
	}

	if status == "paused" {
		s.setFlash(w, r, "Subscription paused.")
	} else {
		s.setFlash(w, r, "Subscription resumed.")
	}
	http.Redirect(w, r, "/subscriptions", http.StatusSeeOther)
}
