package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"spending-project/internal/auth"
	"spending-project/internal/db"
	"spending-project/internal/money"
	"spending-project/internal/recurrence"
)

func (s *Server) handleInboxGet(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	drafts, err := s.store.ListReviewDrafts(r.Context(), u.ActiveHouseholdID)
	if err != nil {
		http.Error(w, "failed to load review inbox", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "inbox.html", http.StatusOK, struct {
		BasePage
		Drafts []db.ReceiptDraft
	}{
		BasePage: s.newBasePage(w, r, "Needs review", "overview", true),
		Drafts:   drafts,
	})
}

func (s *Server) handleInboxDiscard(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	id := r.PathValue("id")
	if err := s.store.DeleteDraftScoped(r.Context(), u.ActiveHouseholdID, id); err != nil {
		http.Error(w, "failed to discard draft", http.StatusInternalServerError)
		return
	}
	s.setFlash(w, r, "Draft discarded.")
	http.Redirect(w, r, "/inbox", http.StatusSeeOther)
}

func (s *Server) handleBudgetSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	catID, _ := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	if catID <= 0 {
		s.setFlash(w, r, "Choose a category.")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	rawLimit := strings.TrimSpace(r.FormValue("monthly_limit"))
	if rawLimit == "" || rawLimit == "0" || rawLimit == "0,00" {
		_ = s.store.SetCategoryBudget(r.Context(), u.ActiveHouseholdID, catID, 0)
		s.setFlash(w, r, "Category budget removed.")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	limitCents, err := money.ParsePositive(rawLimit)
	if err != nil {
		s.setFlash(w, r, "Enter a valid positive monthly budget amount.")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := s.store.SetCategoryBudget(r.Context(), u.ActiveHouseholdID, catID, limitCents); err != nil {
		http.Error(w, "failed to save budget", http.StatusInternalServerError)
		return
	}
	s.setFlash(w, r, "Category budget saved.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleSettlementCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	fromID, _ := strconv.ParseInt(r.FormValue("from_user_id"), 10, 64)
	toID, _ := strconv.ParseInt(r.FormValue("to_user_id"), 10, 64)
	cents, err := money.ParsePositive(r.FormValue("amount"))
	if err != nil {
		s.setFlash(w, r, "Enter a valid settlement amount.")
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	settledDate := strings.TrimSpace(r.FormValue("settled_date"))
	if _, err := recurrence.ParseDate(settledDate, s.cfg.Location); err != nil {
		settledDate = recurrence.Today(s.nowFn(), s.cfg.Location)
	}
	note := strings.TrimSpace(r.FormValue("note"))
	uid := u.ID
	if _, err := s.store.RecordSettlement(r.Context(), u.ActiveHouseholdID, fromID, toID, cents, settledDate, note, &uid); err != nil {
		s.setFlash(w, r, err.Error())
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}

	s.setFlash(w, r, "Settlement recorded. Member balances updated.")
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func parseSplitsFromForm(r *http.Request, amountCents int64, members []db.HouseholdMember, currencySymbol string) ([]db.ExpenseSplit, string, map[int64]string, string) {
	mode := r.FormValue("split_mode")
	customInputs := make(map[int64]string)
	for _, m := range members {
		v := strings.TrimSpace(r.FormValue(fmt.Sprintf("custom_split_%d", m.UserID)))
		if v != "" {
			customInputs[m.UserID] = v
		}
	}

	switch mode {
	case "", "none":
		return nil, "none", customInputs, ""
	case "equal":
		selectedMap := map[int64]bool{}
		for _, idStr := range r.Form["split_users"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id > 0 {
				selectedMap[id] = true
			}
		}
		var selectedIDs []int64
		for _, m := range members {
			if selectedMap[m.UserID] {
				selectedIDs = append(selectedIDs, m.UserID)
			}
		}
		if len(selectedIDs) == 0 {
			return nil, "equal", customInputs, "Select at least one member for an equal split."
		}
		return db.ComputeEqualSplits(amountCents, selectedIDs), "equal", customInputs, ""
	case "custom":
		var splits []db.ExpenseSplit
		var sum int64
		for _, m := range members {
			raw := customInputs[m.UserID]
			if raw == "" {
				continue
			}
			c, err := money.Parse(raw)
			if err != nil || c < 0 {
				return nil, "custom", customInputs, fmt.Sprintf("Invalid split amount for %s.", m.Label())
			}
			if c > 0 {
				splits = append(splits, db.ExpenseSplit{UserID: m.UserID, ShareCents: c})
				sum += c
			}
		}
		if len(splits) == 0 {
			return nil, "custom", customInputs, "Enter split amounts for at least one member."
		}
		if sum != amountCents {
			return nil, "custom", customInputs, fmt.Sprintf("Custom splits (%s) must equal total amount (%s).", money.Format(sum, currencySymbol), money.Format(amountCents, currencySymbol))
		}
		return splits, "custom", customInputs, ""
	default:
		return nil, "none", customInputs, ""
	}
}
