package web

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"spending-project/internal/auth"
	"spending-project/internal/db"
	"spending-project/internal/imageproc"
	"spending-project/internal/money"
	"spending-project/internal/ocr"
	"spending-project/internal/recurrence"
)

type draftSplitPayload struct {
	Mode   string            `json:"mode"`
	Users  []int64           `json:"users,omitempty"`
	Custom map[string]string `json:"custom,omitempty"`
}

type scanPageData struct {
	BasePage
	FormHouseholdID   int64
	FormHouseholdName string
	MaxUploadMB       int64
	OCRLanguages      string
	Error             string
}

type expenseFormData struct {
	BasePage
	Heading          string
	ActionURL        string
	CancelURL        string
	SubmitLabel      string
	IsEdit            bool
	FormHouseholdID   int64
	FormHouseholdName string
	SubmissionToken   string
	DraftID          string
	ReceiptImageID   int64
	Amount           string
	Merchant         string
	PurchaseDate     string
	PaymentMethod    string
	CategoryID       int64
	PaidByUserID     int64
	SplitMode        string
	SplitSelected    map[int64]bool
	CustomSplits     map[int64]string
	Kind             string
	Note             string
	RememberMerchant bool
	ConfirmDuplicate bool
	CheckAmount      bool
	CheckMerchant    bool
	CheckDate        bool
	OCRNotice        string
	SuggestReason    string
	FormError        string
	Errors           map[string]string
	Categories       []db.Category
	Members          []db.HouseholdMember
	Duplicates       []db.DuplicateMatch
}

func (s *Server) resolveSubmittedHousehold(r *http.Request, u db.User) (int64, string, bool) {
	raw := strings.TrimSpace(r.FormValue("household_id"))
	if raw == "" {
		if u.ActiveHouseholdID <= 0 {
			return 0, "", false
		}
		return u.ActiveHouseholdID, u.ActiveHouseholdName, true
	}
	formHH, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || formHH <= 0 {
		return 0, "", false
	}
	hhs, err := s.store.ListUserHouseholds(r.Context(), u.ID)
	if err != nil {
		return 0, "", false
	}
	for _, h := range hhs {
		if h.ID == formHH {
			return h.ID, h.Name, true
		}
	}
	return 0, "", false
}

func (s *Server) handleScanGet(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	s.render(w, r, "scan.html", http.StatusOK, scanPageData{
		BasePage:          s.newBasePage(w, r, "Scan receipt", "overview", true),
		FormHouseholdID:   u.ActiveHouseholdID,
		FormHouseholdName: u.ActiveHouseholdName,
		MaxUploadMB:       s.cfg.MaxUploadBytes / (1024 * 1024),
		OCRLanguages:      s.cfg.OCRLanguages,
	})
}

func (s *Server) handleScanPost(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+64*1024)
	if err := r.ParseMultipartForm(s.cfg.MaxUploadBytes); err != nil {
		s.render(w, r, "scan.html", http.StatusBadRequest, scanPageData{
			BasePage:          s.newBasePage(w, r, "Scan receipt", "overview", true),
			FormHouseholdID:   u.ActiveHouseholdID,
			FormHouseholdName: u.ActiveHouseholdName,
			MaxUploadMB:       s.cfg.MaxUploadBytes / (1024 * 1024),
			OCRLanguages:      s.cfg.OCRLanguages,
			Error:             "Upload too large or invalid form data.",
		})
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	hhID, hhName, ok := s.resolveSubmittedHousehold(r, u)
	if !ok {
		http.Error(w, "you are no longer a member of that household", http.StatusForbidden)
		return
	}

	file, _, err := r.FormFile("receipt")
	if err != nil {
		s.render(w, r, "scan.html", http.StatusBadRequest, scanPageData{
			BasePage:          s.newBasePage(w, r, "Scan receipt", "overview", true),
			FormHouseholdID:   hhID,
			FormHouseholdName: hhName,
			MaxUploadMB:       s.cfg.MaxUploadBytes / (1024 * 1024),
			OCRLanguages:      s.cfg.OCRLanguages,
			Error:             "Please choose or take a receipt photo first.",
		})
		return
	}
	defer file.Close()

	proc, err := imageproc.Process(file, s.cfg.MaxUploadBytes, s.cfg.MaxImageDim, s.cfg.MaxStoredDim)
	if err != nil {
		s.render(w, r, "scan.html", http.StatusBadRequest, scanPageData{
			BasePage:          s.newBasePage(w, r, "Scan receipt", "overview", true),
			FormHouseholdID:   hhID,
			FormHouseholdName: hhName,
			MaxUploadMB:       s.cfg.MaxUploadBytes / (1024 * 1024),
			OCRLanguages:      s.cfg.OCRLanguages,
			Error:             err.Error(),
		})
		return
	}

	if !s.limiter.Allow(fmt.Sprintf("upload:%d", hhID), 20, 10*time.Minute, s.nowFn()) {
		s.render(w, r, "scan.html", http.StatusTooManyRequests, scanPageData{
			BasePage:          s.newBasePage(w, r, "Scan receipt", "overview", true),
			FormHouseholdID:   hhID,
			FormHouseholdName: hhName,
			MaxUploadMB:       s.cfg.MaxUploadBytes / (1024 * 1024),
			OCRLanguages:      s.cfg.OCRLanguages,
			Error:             "Too many uploads in a short time. Wait a few minutes or enter manually.",
		})
		return
	}

	imgID, err := s.store.SaveReceiptImageWithQuota(r.Context(), hhID, s.cfg.MaxHouseholdQuotaBytes, proc.SHA256, proc.MimeType, proc.Width, proc.Height, proc.Data)
	if errors.Is(err, db.ErrQuotaExceeded) {
		s.render(w, r, "scan.html", http.StatusRequestEntityTooLarge, scanPageData{
			BasePage:          s.newBasePage(w, r, "Scan receipt", "overview", true),
			FormHouseholdID:   hhID,
			FormHouseholdName: hhName,
			MaxUploadMB:       s.cfg.MaxUploadBytes / (1024 * 1024),
			OCRLanguages:      s.cfg.OCRLanguages,
			Error:             "Household receipt storage quota reached. Enter expense manually or delete old receipts.",
		})
		return
	}
	if err != nil {
		http.Error(w, "failed to store receipt image", http.StatusInternalServerError)
		return
	}

	draftID, err := auth.RandomToken(16)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	draft := db.ReceiptDraft{
		ID:             draftID,
		HouseholdID:    hhID,
		ReceiptImageID: imgID,
		PurchaseDate:   recurrence.Today(s.nowFn(), s.cfg.Location),
		OCRStatus:      "ok",
	}

	rawText, scanErr := s.scanner.Scan(r.Context(), proc.Data)
	if scanErr != nil {
		switch {
		case errors.Is(scanErr, ocr.ErrBusy):
			draft.OCRStatus = "busy"
			draft.OCRError = "Scanner is busy with another receipt. Your photo is attached — enter details manually or try scanning again."
		case errors.Is(scanErr, ocr.ErrTimeout):
			draft.OCRStatus = "timeout"
			draft.OCRError = "Receipt scan timed out. Your photo is attached — please enter the details manually."
		default:
			draft.OCRStatus = "failed"
			draft.OCRError = "Couldn't read the receipt. Enter the details below."
		}
		draft.CheckAmount = true
		draft.CheckMerchant = true
		draft.CheckDate = true
		if cat, err := s.store.CategoryByName(r.Context(), hhID, "Uncategorized"); err == nil {
			draft.CategoryID = cat.ID
		}
	} else {
		parsed := ocr.ParseReceipt(rawText, s.cfg.Location)
		draft.Merchant = parsed.Merchant
		draft.AmountCents = parsed.AmountCents
		if parsed.Date != "" {
			draft.PurchaseDate = parsed.Date
		}
		draft.PaymentMethod = parsed.PaymentHint
		draft.CheckMerchant = parsed.CheckMerchant
		draft.CheckDate = parsed.CheckDate
		draft.CheckAmount = parsed.CheckAmount

		cat, _, err := s.store.SuggestCategory(r.Context(), hhID, parsed.Merchant, rawText)
		if err == nil {
			draft.CategoryID = cat.ID
		}
	}

	if err := s.store.CreateDraft(r.Context(), draft); err != nil {
		http.Error(w, "failed to create draft", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/expenses/new?draft="+url.QueryEscape(draftID), http.StatusSeeOther)
}

func (s *Server) handleExpenseNewGet(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	hhID := u.ActiveHouseholdID
	cats, err := s.store.ListCategories(r.Context(), hhID)
	if err != nil {
		http.Error(w, "failed to load categories", http.StatusInternalServerError)
		return
	}
	members, _ := s.store.ListHouseholdMembers(r.Context(), hhID)
	subTok, _ := auth.RandomToken(16)
	today := recurrence.Today(s.nowFn(), s.cfg.Location)

	data := expenseFormData{
		BasePage:          s.newBasePage(w, r, "Add expense", "ledger", true),
		Heading:           "Add expense",
		ActionURL:         "/expenses",
		CancelURL:         "/",
		SubmitLabel:       "Save expense",
		FormHouseholdID:   hhID,
		FormHouseholdName: u.ActiveHouseholdName,
		SubmissionToken:   subTok,
		PurchaseDate:    today,
		PaidByUserID:    u.ID,
		SplitMode:       "none",
		SplitSelected:   map[int64]bool{},
		CustomSplits:    map[int64]string{},
		Kind:            "expense",
		Categories:      cats,
		Members:         members,
		Errors:          map[string]string{},
	}

	if uncat, err := s.store.CategoryByName(r.Context(), hhID, "Uncategorized"); err == nil {
		data.CategoryID = uncat.ID
	} else if len(cats) > 0 {
		data.CategoryID = cats[0].ID
	}

	if draftID := strings.TrimSpace(r.URL.Query().Get("draft")); draftID != "" {
		draft, err := s.store.GetDraft(r.Context(), hhID, draftID)
		if err == nil {
			data.Heading = "Review receipt"
			data.Title = "Review receipt"
			data.DraftID = draft.ID
			data.ReceiptImageID = draft.ReceiptImageID
			data.Merchant = draft.Merchant
			if draft.PurchaseDate != "" {
				data.PurchaseDate = draft.PurchaseDate
			}
			if draft.AmountRaw != "" {
				data.Amount = draft.AmountRaw
			} else if draft.AmountCents > 0 {
				data.Amount = money.FormatInput(draft.AmountCents)
			}
			data.PaymentMethod = draft.PaymentMethod
			if draft.CategoryID > 0 {
				data.CategoryID = draft.CategoryID
			}
			if draft.Kind == "refund" {
				data.Kind = "refund"
			}
			if draft.PaidByUserID != nil && *draft.PaidByUserID > 0 {
				data.PaidByUserID = *draft.PaidByUserID
			}
			data.Note = draft.Note
			if draft.SplitData != "" {
				var sp draftSplitPayload
				if err := json.Unmarshal([]byte(draft.SplitData), &sp); err == nil {
					if sp.Mode != "" {
						data.SplitMode = sp.Mode
					}
					for _, uid := range sp.Users {
						data.SplitSelected[uid] = true
					}
					for k, v := range sp.Custom {
						if uid, err := strconv.ParseInt(k, 10, 64); err == nil {
							data.CustomSplits[uid] = v
						}
					}
				}
			}
			data.CheckMerchant = draft.CheckMerchant
			data.CheckDate = draft.CheckDate
			data.CheckAmount = draft.CheckAmount
			data.OCRNotice = draft.OCRError

			if draft.OCRStatus == "ok" && draft.Merchant != "" {
				if _, reason, err := s.store.SuggestCategory(r.Context(), hhID, draft.Merchant, ""); err == nil {
					if reason == "remembered" {
						data.SuggestReason = "Suggested from your remembered choice for this merchant."
					} else if reason == "keyword" {
						data.SuggestReason = "Suggested from merchant keywords."
					}
				}
			}

			var recPtr *int64
			if draft.ReceiptImageID > 0 {
				v := draft.ReceiptImageID
				recPtr = &v
			}
			if dups, err := s.store.FindDuplicates(r.Context(), hhID, recPtr, draft.Merchant, draft.PurchaseDate, draft.AmountCents, 0); err == nil {
				data.Duplicates = dups
			}
		}
	}

	s.render(w, r, "expense_form.html", http.StatusOK, data)
}

func (s *Server) handleExpenseCreatePost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+128*1024)
	if err := r.ParseMultipartForm(s.cfg.MaxUploadBytes); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	u, ok := s.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	hhID, hhName, memberOK := s.resolveSubmittedHousehold(r, u)
	if !memberOK {
		http.Error(w, "you are no longer a member of that household", http.StatusForbidden)
		return
	}
	cats, _ := s.store.ListCategories(r.Context(), hhID)
	members, _ := s.store.ListHouseholdMembers(r.Context(), hhID)
	draftID := strings.TrimSpace(r.FormValue("draft_id"))
	recIDVal, _ := strconv.ParseInt(r.FormValue("receipt_image_id"), 10, 64)

	var attachmentErr string
	// Handle optional attachment uploaded directly on manual form with atomic quota enforcement.
	if recIDVal == 0 {
		if file, _, err := r.FormFile("attachment"); err == nil {
			defer file.Close()
			proc, procErr := imageproc.Process(file, s.cfg.MaxUploadBytes, s.cfg.MaxImageDim, s.cfg.MaxStoredDim)
			if procErr != nil {
				attachmentErr = procErr.Error()
			} else {
				id, saveErr := s.store.SaveReceiptImageWithQuota(r.Context(), hhID, s.cfg.MaxHouseholdQuotaBytes, proc.SHA256, proc.MimeType, proc.Width, proc.Height, proc.Data)
				if saveErr != nil {
					attachmentErr = saveErr.Error()
				} else {
					recIDVal = id
				}
			}
		} else if !errors.Is(err, http.ErrMissingFile) {
			attachmentErr = "Could not read attached photo."
		}
	}

	paidByID, _ := strconv.ParseInt(r.FormValue("paid_by_user_id"), 10, 64)
	if paidByID <= 0 {
		paidByID = u.ID
	}
	catID, _ := strconv.ParseInt(r.FormValue("category_id"), 10, 64)

	splitSelected := map[int64]bool{}
	var splitUserIDs []int64
	for _, idStr := range r.Form["split_users"] {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id > 0 {
			splitSelected[id] = true
			splitUserIDs = append(splitUserIDs, id)
		}
	}
	splitMode := strings.TrimSpace(r.FormValue("split_mode"))
	if splitMode == "" {
		splitMode = "none"
	}
	rawCustomSplits := map[int64]string{}
	rawCustomJSON := map[string]string{}
	for _, m := range members {
		val := strings.TrimSpace(r.FormValue(fmt.Sprintf("split_amount_%d", m.UserID)))
		if val != "" {
			rawCustomSplits[m.UserID] = val
			rawCustomJSON[strconv.FormatInt(m.UserID, 10)] = val
		}
	}

	// Save to Receipt Review Inbox without creating an expense.
	if r.FormValue("save_action") == "draft" && draftID != "" {
		rawAmount := strings.TrimSpace(r.FormValue("amount"))
		parsedCents, _ := money.ParsePositive(rawAmount)
		reason := strings.TrimSpace(r.FormValue("review_reason"))
		if reason == "" {
			if parsedCents <= 0 {
				reason = "Uncertain total"
			} else if catID <= 0 {
				reason = "Missing category"
			} else {
				reason = "Needs review"
			}
		}
		kind := strings.TrimSpace(r.FormValue("kind"))
		if kind != "refund" {
			kind = "expense"
		}
		splitBytes, _ := json.Marshal(draftSplitPayload{
			Mode:   splitMode,
			Users:  splitUserIDs,
			Custom: rawCustomJSON,
		})
		paidByPtr := &paidByID
		if err := s.store.UpdateDraftInbox(r.Context(), hhID, db.ReceiptDraft{
			ID:            draftID,
			Merchant:      strings.TrimSpace(r.FormValue("merchant")),
			PurchaseDate:  strings.TrimSpace(r.FormValue("purchase_date")),
			AmountCents:   parsedCents,
			AmountRaw:     rawAmount,
			PaymentMethod: strings.TrimSpace(r.FormValue("payment_method")),
			CategoryID:    catID,
			Kind:          kind,
			PaidByUserID:  paidByPtr,
			Note:          strings.TrimSpace(r.FormValue("note")),
			SplitData:     string(splitBytes),
			ReviewReason:  reason,
		}); err != nil {
			subTok, _ := auth.RandomToken(16)
			data := expenseFormData{
				BasePage:          s.newBasePage(w, r, "Review receipt", "ledger", true),
				Heading:           "Review receipt",
				ActionURL:         "/expenses",
				CancelURL:         "/",
				SubmitLabel:       "Save expense",
				FormHouseholdID:   hhID,
				FormHouseholdName: hhName,
				DraftID:           draftID,
				ReceiptImageID:    recIDVal,
				SubmissionToken:   subTok,
				Kind:              kind,
				Amount:            rawAmount,
				Merchant:          strings.TrimSpace(r.FormValue("merchant")),
				PurchaseDate:      strings.TrimSpace(r.FormValue("purchase_date")),
				PaymentMethod:     strings.TrimSpace(r.FormValue("payment_method")),
				CategoryID:        catID,
				PaidByUserID:      paidByID,
				SplitMode:         splitMode,
				SplitSelected:     splitSelected,
				CustomSplits:      rawCustomSplits,
				Note:              strings.TrimSpace(r.FormValue("note")),
				RememberMerchant:  r.FormValue("remember_merchant") == "1",
				Categories:        cats,
				Members:           members,
				Errors:            map[string]string{},
				FormError:         "Could not save draft to inbox. Please try again.",
			}
			s.render(w, r, "expense_form.html", http.StatusInternalServerError, data)
			return
		}
		s.setFlash(w, r, "Saved to Needs review inbox.")
		http.Redirect(w, r, "/inbox", http.StatusSeeOther)
		return
	}

	data := expenseFormData{
		BasePage:          s.newBasePage(w, r, "Add expense", "ledger", true),
		Heading:           "Add expense",
		ActionURL:         "/expenses",
		CancelURL:         "/",
		SubmitLabel:       "Save expense",
		FormHouseholdID:   hhID,
		FormHouseholdName: hhName,
		SubmissionToken:   r.FormValue("submission_token"),
		DraftID:          draftID,
		ReceiptImageID:   recIDVal,
		Amount:           strings.TrimSpace(r.FormValue("amount")),
		Merchant:         strings.TrimSpace(r.FormValue("merchant")),
		PurchaseDate:     strings.TrimSpace(r.FormValue("purchase_date")),
		PaymentMethod:    strings.TrimSpace(r.FormValue("payment_method")),
		PaidByUserID:     paidByID,
		SplitMode:        r.FormValue("split_mode"),
		SplitSelected:    splitSelected,
		Kind:             strings.TrimSpace(r.FormValue("kind")),
		Note:             strings.TrimSpace(r.FormValue("note")),
		RememberMerchant: r.FormValue("remember_merchant") == "1",
		ConfirmDuplicate: r.FormValue("confirm_duplicate") == "1",
		Categories:       cats,
		Members:          members,
		Errors:           map[string]string{},
	}
	if data.SplitMode == "" {
		data.SplitMode = "none"
	}
	if recIDVal > 0 {
		data.Heading = "Review receipt"
		data.Title = "Review receipt"
	}
	if data.Kind != "refund" {
		data.Kind = "expense"
	}
	data.CategoryID = catID

	cents, err := money.ParsePositive(data.Amount)
	if err != nil {
		data.Errors["amount"] = err.Error()
	}
	if data.Merchant == "" {
		data.Errors["merchant"] = "Enter a merchant name."
	}
	if _, err := recurrence.ParseDate(data.PurchaseDate, s.cfg.Location); err != nil {
		data.Errors["purchase_date"] = "Enter a valid date."
	}
	if data.PaymentMethod != "cash" && data.PaymentMethod != "card" {
		data.Errors["payment_method"] = "Choose Cash or Card."
	}
	if catID <= 0 {
		data.Errors["category_id"] = "Choose a category."
	}
	if attachmentErr != "" {
		data.Errors["attachment"] = attachmentErr
	}

	var splits []db.ExpenseSplit
	var customMap map[int64]string
	if cents > 0 {
		var splitErr string
		splits, data.SplitMode, customMap, splitErr = parseSplitsFromForm(r, cents, members, s.cfg.CurrencySymbol)
		data.CustomSplits = customMap
		if splitErr != "" {
			data.Errors["splits"] = splitErr
		}
	}

	if len(data.Errors) > 0 {
		s.render(w, r, "expense_form.html", http.StatusUnprocessableEntity, data)
		return
	}

	var recPtr *int64
	if recIDVal > 0 {
		v := recIDVal
		recPtr = &v
	}

	dups, err := s.store.FindDuplicates(r.Context(), hhID, recPtr, data.Merchant, data.PurchaseDate, cents, 0)
	if err == nil && len(dups) > 0 && !data.ConfirmDuplicate {
		data.Duplicates = dups
		data.FormError = "This looks like an existing expense. Review the match below or confirm to save anyway."
		s.render(w, r, "expense_form.html", http.StatusConflict, data)
		return
	}

	source := "manual"
	if recIDVal > 0 {
		source = "receipt"
	}

	var createdByPtr *int64
	if ok && u.ID > 0 {
		uid := u.ID
		createdByPtr = &uid
	}

	var paidByPtr *int64
	if paidByID > 0 {
		p := paidByID
		paidByPtr = &p
	}

	_, err = s.store.SaveExpense(r.Context(), db.SaveExpenseInput{
		HouseholdID:      hhID,
		Kind:             data.Kind,
		AmountCents:      cents,
		Merchant:         data.Merchant,
		PurchaseDate:     data.PurchaseDate,
		PaymentMethod:    data.PaymentMethod,
		CategoryID:       catID,
		Note:             data.Note,
		Source:           source,
		ReceiptImageID:   recPtr,
		CreatedByUserID:  createdByPtr,
		PaidByUserID:     paidByPtr,
		Splits:           splits,
		SubmissionToken:  data.SubmissionToken,
		RememberMerchant: data.RememberMerchant,
		DraftID:          draftID,
	})
	if errors.Is(err, db.ErrDuplicateSubmit) {
		s.setFlash(w, r, "Expense already saved.")
		http.Redirect(w, r, "/ledger", http.StatusSeeOther)
		return
	}
	if err != nil {
		data.FormError = "Could not save expense. Please try again."
		s.render(w, r, "expense_form.html", http.StatusInternalServerError, data)
		return
	}

	s.setFlash(w, r, fmt.Sprintf("Saved %s (%s).", data.Merchant, money.Format(cents, s.cfg.CurrencySymbol)))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleExpenseDetail(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	exp, err := s.store.GetExpense(r.Context(), u.ActiveHouseholdID, id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	splits, _ := s.store.GetExpenseSplits(r.Context(), exp.ID)
	retURL := strings.TrimSpace(r.URL.Query().Get("return"))
	if !strings.HasPrefix(retURL, "/ledger") || strings.HasPrefix(retURL, "//") {
		retURL = "/ledger"
	}

	s.render(w, r, "expense_detail.html", http.StatusOK, struct {
		BasePage
		Expense   db.Expense
		Splits    []db.ExpenseSplit
		ReturnURL string
	}{
		BasePage:  s.newBasePage(w, r, exp.Merchant, "ledger", true),
		Expense:   exp,
		Splits:    splits,
		ReturnURL: retURL,
	})
}

func (s *Server) handleExpenseEditGet(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	hhID := u.ActiveHouseholdID
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	exp, err := s.store.GetExpense(r.Context(), hhID, id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cats, _ := s.store.ListCategories(r.Context(), hhID)
	members, _ := s.store.ListHouseholdMembers(r.Context(), hhID)
	existingSplits, _ := s.store.GetExpenseSplits(r.Context(), exp.ID)
	var recID int64
	if exp.ReceiptImageID != nil {
		recID = *exp.ReceiptImageID
	}
	paidByID := u.ID
	if exp.PaidByUserID != nil {
		paidByID = *exp.PaidByUserID
	}
	splitMode := "none"
	splitSel := map[int64]bool{}
	customSplits := map[int64]string{}
	if len(existingSplits) > 0 {
		splitMode = "custom"
		for _, sp := range existingSplits {
			splitSel[sp.UserID] = true
			customSplits[sp.UserID] = money.FormatInput(sp.ShareCents)
		}
	}

	s.render(w, r, "expense_form.html", http.StatusOK, expenseFormData{
		BasePage:       s.newBasePage(w, r, "Edit expense", "ledger", true),
		Heading:        "Edit expense",
		ActionURL:      fmt.Sprintf("/expenses/%d/edit", exp.ID),
		CancelURL:      fmt.Sprintf("/expenses/%d", exp.ID),
		SubmitLabel:    "Save changes",
		IsEdit:         true,
		ReceiptImageID: recID,
		Amount:         money.FormatInput(exp.AmountCents),
		Merchant:       exp.Merchant,
		PurchaseDate:   exp.PurchaseDate,
		PaymentMethod:  exp.PaymentMethod,
		CategoryID:     exp.CategoryID,
		PaidByUserID:   paidByID,
		SplitMode:      splitMode,
		SplitSelected:  splitSel,
		CustomSplits:   customSplits,
		Kind:           exp.Kind,
		Note:           exp.Note,
		Categories:     cats,
		Members:        members,
		Errors:         map[string]string{},
	})
}

func (s *Server) handleExpenseEditPost(w http.ResponseWriter, r *http.Request) {
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
	hhID := u.ActiveHouseholdID
	cats, _ := s.store.ListCategories(r.Context(), hhID)
	members, _ := s.store.ListHouseholdMembers(r.Context(), hhID)
	recIDVal, _ := strconv.ParseInt(r.FormValue("receipt_image_id"), 10, 64)
	catID, _ := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	paidByID, _ := strconv.ParseInt(r.FormValue("paid_by_user_id"), 10, 64)
	if paidByID <= 0 {
		paidByID = u.ID
	}
	splitSelected := map[int64]bool{}
	for _, idStr := range r.Form["split_users"] {
		if uid, err := strconv.ParseInt(idStr, 10, 64); err == nil && uid > 0 {
			splitSelected[uid] = true
		}
	}

	data := expenseFormData{
		BasePage:         s.newBasePage(w, r, "Edit expense", "ledger", true),
		Heading:          "Edit expense",
		ActionURL:        fmt.Sprintf("/expenses/%d/edit", id),
		CancelURL:        fmt.Sprintf("/expenses/%d", id),
		SubmitLabel:      "Save changes",
		IsEdit:           true,
		ReceiptImageID:   recIDVal,
		Amount:           strings.TrimSpace(r.FormValue("amount")),
		Merchant:         strings.TrimSpace(r.FormValue("merchant")),
		PurchaseDate:     strings.TrimSpace(r.FormValue("purchase_date")),
		PaymentMethod:    strings.TrimSpace(r.FormValue("payment_method")),
		CategoryID:       catID,
		PaidByUserID:     paidByID,
		SplitMode:        r.FormValue("split_mode"),
		SplitSelected:    splitSelected,
		Kind:             strings.TrimSpace(r.FormValue("kind")),
		Note:             strings.TrimSpace(r.FormValue("note")),
		RememberMerchant: r.FormValue("remember_merchant") == "1",
		Categories:       cats,
		Members:          members,
		Errors:           map[string]string{},
	}
	if data.Kind != "refund" {
		data.Kind = "expense"
	}

	cents, err := money.ParsePositive(data.Amount)
	if err != nil {
		data.Errors["amount"] = err.Error()
	}
	if data.Merchant == "" {
		data.Errors["merchant"] = "Enter a merchant name."
	}
	if _, err := recurrence.ParseDate(data.PurchaseDate, s.cfg.Location); err != nil {
		data.Errors["purchase_date"] = "Enter a valid date."
	}
	if data.PaymentMethod != "cash" && data.PaymentMethod != "card" {
		data.Errors["payment_method"] = "Choose Cash or Card."
	}
	if catID <= 0 {
		data.Errors["category_id"] = "Choose a category."
	}

	var splits []db.ExpenseSplit
	var customMap map[int64]string
	if cents > 0 {
		var splitErr string
		splits, data.SplitMode, customMap, splitErr = parseSplitsFromForm(r, cents, members, s.cfg.CurrencySymbol)
		data.CustomSplits = customMap
		if splitErr != "" {
			data.Errors["splits"] = splitErr
		}
	}

	if len(data.Errors) > 0 {
		s.render(w, r, "expense_form.html", http.StatusUnprocessableEntity, data)
		return
	}

	var paidByPtr *int64
	if paidByID > 0 {
		p := paidByID
		paidByPtr = &p
	}

	err = s.store.UpdateExpense(r.Context(), hhID, id, db.SaveExpenseInput{
		Kind:             data.Kind,
		AmountCents:      cents,
		Merchant:         data.Merchant,
		PurchaseDate:     data.PurchaseDate,
		PaymentMethod:    data.PaymentMethod,
		CategoryID:       catID,
		PaidByUserID:     paidByPtr,
		Splits:           splits,
		ReplaceSplits:    true,
		Note:             data.Note,
		RememberMerchant: data.RememberMerchant,
	})
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		data.FormError = "Could not update expense."
		s.render(w, r, "expense_form.html", http.StatusInternalServerError, data)
		return
	}

	s.setFlash(w, r, "Expense updated.")
	http.Redirect(w, r, fmt.Sprintf("/expenses/%d", id), http.StatusSeeOther)
}

func (s *Server) handleExpenseDeletePost(w http.ResponseWriter, r *http.Request) {
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
	if err := s.store.DeleteExpense(r.Context(), u.ActiveHouseholdID, id); errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "failed to delete expense", http.StatusInternalServerError)
		return
	}

	s.setFlash(w, r, "Expense deleted.")
	http.Redirect(w, r, "/ledger", http.StatusSeeOther)
}

func (s *Server) handleReceiptImage(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	img, err := s.store.GetReceiptImage(r.Context(), u.ActiveHouseholdID, id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", img.MimeType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img.Data)
}

type dayGroup struct {
	Date  string
	Items []db.Expense
}

func parseExpenseFilter(r *http.Request) db.ExpenseFilter {
	q := r.URL.Query()
	catID, _ := strconv.ParseInt(q.Get("category"), 10, 64)
	userID, _ := strconv.ParseInt(q.Get("user"), 10, 64)
	return db.ExpenseFilter{
		FromDate:      strings.TrimSpace(q.Get("from")),
		ToDate:        strings.TrimSpace(q.Get("to")),
		Merchant:      strings.TrimSpace(q.Get("merchant")),
		CategoryID:    catID,
		PaymentMethod: strings.TrimSpace(q.Get("payment")),
		UserID:        userID,
	}
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	hhID := u.ActiveHouseholdID
	filter := parseExpenseFilter(r)
	filter.HouseholdID = hhID

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const perPage = 50

	expenses, matchCount, netCents, err := s.store.ListExpensesPage(r.Context(), filter, page, perPage)
	if err != nil {
		http.Error(w, "failed to load ledger", http.StatusInternalServerError)
		return
	}
	cats, _ := s.store.ListCategories(r.Context(), hhID)
	members, _ := s.store.ListHouseholdMembers(r.Context(), hhID)
	totalCount, _ := s.store.CountAllExpenses(r.Context(), hhID)

	var groups []dayGroup
	for _, e := range expenses {
		if len(groups) == 0 || groups[len(groups)-1].Date != e.PurchaseDate {
			groups = append(groups, dayGroup{Date: e.PurchaseDate, Items: []db.Expense{e}})
		} else {
			groups[len(groups)-1].Items = append(groups[len(groups)-1].Items, e)
		}
	}

	hasFilters := filter.FromDate != "" || filter.ToDate != "" || filter.Merchant != "" || filter.CategoryID > 0 || filter.PaymentMethod != "" || filter.UserID > 0
	var summaryParts []string
	if filter.FromDate != "" || filter.ToDate != "" {
		switch {
		case filter.FromDate != "" && filter.ToDate != "":
			summaryParts = append(summaryParts, filter.FromDate+" to "+filter.ToDate)
		case filter.FromDate != "":
			summaryParts = append(summaryParts, "From "+filter.FromDate)
		default:
			summaryParts = append(summaryParts, "Until "+filter.ToDate)
		}
	}
	if filter.UserID > 0 {
		for _, m := range members {
			if m.UserID == filter.UserID {
				summaryParts = append(summaryParts, "Paid by "+m.Label())
				break
			}
		}
	}
	if filter.CategoryID > 0 {
		for _, c := range cats {
			if c.ID == filter.CategoryID {
				summaryParts = append(summaryParts, c.Name)
				break
			}
		}
	}
	if filter.PaymentMethod != "" {
		if filter.PaymentMethod == "cash" {
			summaryParts = append(summaryParts, "Cash")
		} else if filter.PaymentMethod == "card" {
			summaryParts = append(summaryParts, "Card")
		}
	}
	if filter.Merchant != "" {
		summaryParts = append(summaryParts, `"`+filter.Merchant+`"`)
	}

	totalPages := (matchCount + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	exportVals := url.Values{}
	for k, vs := range r.URL.Query() {
		if k == "page" {
			continue
		}
		for _, v := range vs {
			if v != "" {
				exportVals.Add(k, v)
			}
		}
	}
	exportQuery := exportVals.Encode()

	var prevPageURL, nextPageURL string
	if page > 1 {
		pv := url.Values{}
		for k, vs := range exportVals {
			pv[k] = vs
		}
		if page-1 > 1 {
			pv.Set("page", strconv.Itoa(page-1))
		}
		if enc := pv.Encode(); enc != "" {
			prevPageURL = "/ledger?" + enc
		} else {
			prevPageURL = "/ledger"
		}
	}
	if page < totalPages {
		nv := url.Values{}
		for k, vs := range exportVals {
			nv[k] = vs
		}
		nv.Set("page", strconv.Itoa(page+1))
		nextPageURL = "/ledger?" + nv.Encode()
	}

	returnURL := "/ledger"
	if r.URL.RawQuery != "" {
		returnURL = "/ledger?" + r.URL.RawQuery
	}

	s.render(w, r, "ledger.html", http.StatusOK, struct {
		BasePage
		Filter                 db.ExpenseFilter
		FilterQuery            string
		ReturnURL              string
		HasActiveFilters       bool
		ActiveFilterSummary    string
		HasAnyExpenses         bool
		TotalMatchCount        int
		Page                   int
		TotalPages             int
		PrevPageURL            string
		NextPageURL            string
		FilteredTotalFormatted string
		Expenses               []db.Expense
		DayGroups              []dayGroup
		Categories             []db.Category
		Users                  []db.HouseholdMember
	}{
		BasePage:               s.newBasePage(w, r, "Ledger", "ledger", true),
		Filter:                 filter,
		FilterQuery:            exportQuery,
		ReturnURL:              returnURL,
		HasActiveFilters:       hasFilters,
		ActiveFilterSummary:    strings.Join(summaryParts, " · "),
		HasAnyExpenses:         totalCount > 0,
		TotalMatchCount:        matchCount,
		Page:                   page,
		TotalPages:             totalPages,
		PrevPageURL:            prevPageURL,
		NextPageURL:            nextPageURL,
		FilteredTotalFormatted: money.Format(netCents, s.cfg.CurrencySymbol),
		Expenses:               expenses,
		DayGroups:              groups,
		Categories:             cats,
		Users:                  members,
	})
}

func (s *Server) handleLedgerCSV(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	if !s.limiter.Allow(fmt.Sprintf("export:%d", u.ID), 10, 5*time.Minute, s.nowFn()) {
		http.Error(w, "too many exports; wait a few minutes", http.StatusTooManyRequests)
		return
	}
	filter := parseExpenseFilter(r)
	filter.HouseholdID = u.ActiveHouseholdID
	expenses, err := s.store.ListExpenses(r.Context(), filter)
	if err != nil {
		http.Error(w, "failed to export ledger", http.StatusInternalServerError)
		return
	}
	_ = s.store.LogAudit(r.Context(), "csv_exported", &u.ID, &u.ActiveHouseholdID, fmt.Sprintf("rows=%d", len(expenses)))

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="tefteri-ledger.csv"`)
	// UTF-8 BOM for spreadsheet compatibility with Greek characters.
	_, _ = w.Write([]byte("\xEF\xBB\xBF"))

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"date", "merchant", "amount", "currency", "category", "payment_method", "source", "paid_by", "added_by", "notes"})
	for _, e := range expenses {
		cents := e.AmountCents
		if e.Kind == "refund" {
			cents = -cents
		}
		_ = cw.Write([]string{
			e.PurchaseDate,
			sanitizeCSVCell(e.Merchant),
			money.FormatCSV(cents),
			s.cfg.Currency,
			sanitizeCSVCell(e.CategoryName),
			e.PaymentMethod,
			e.Source,
			sanitizeCSVCell(e.PaidByName),
			sanitizeCSVCell(e.CreatedByName),
			sanitizeCSVCell(e.Note),
		})
	}
	cw.Flush()
}

func sanitizeCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

func (s *Server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	name := strings.TrimSpace(r.FormValue("name"))
	if name != "" {
		if _, err := s.store.CreateCategory(r.Context(), u.ActiveHouseholdID, name); err == nil {
			s.setFlash(w, r, "Category added.")
		}
	}
	http.Redirect(w, r, "/ledger", http.StatusSeeOther)
}
