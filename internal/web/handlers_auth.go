package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"spending-project/internal/auth"
	"spending-project/internal/db"
	"spending-project/internal/recurrence"
)

type loginPageData struct {
	BasePage
	HasUser  bool
	Username string
	Error    string
}

func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
		if _, err := s.store.GetSessionUser(r.Context(), auth.HashToken(c.Value), s.nowFn()); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}

	hasUser, err := s.store.HasUser(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "login.html", http.StatusOK, loginPageData{
		BasePage: s.newBasePage(w, r, "Sign in", "", false),
		HasUser:  hasUser,
	})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	hasUser, _ := s.store.HasUser(r.Context())
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	user, err := s.store.GetUserByUsername(r.Context(), username)
	if err != nil || !auth.VerifyPassword(user.PasswordHash, password) {
		s.render(w, r, "login.html", http.StatusUnauthorized, loginPageData{
			BasePage: s.newBasePage(w, r, "Sign in", "", false),
			HasUser:  hasUser,
			Username: username,
			Error:    "Invalid username or password.",
		})
		return
	}

	rawToken, err := auth.RandomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	exp := s.nowFn().Add(s.cfg.SessionTTL)
	if err := s.store.CreateSession(r.Context(), auth.HashToken(rawToken), user.ID, exp); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	auth.SetSessionCookie(w, r, rawToken, s.cfg.SessionTTL, s.cfg.CookieSecure)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
		_ = s.store.DeleteSession(r.Context(), auth.HashToken(c.Value))
	}
	auth.ClearSessionCookie(w, r, s.cfg.CookieSecure)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type usersPageData struct {
	BasePage
	Members     []db.HouseholdMember
	Balances    []db.MemberBalance
	Settlements []db.Settlement
	Today       string
	InviteCode  string
	Username    string
	DisplayName string
	Error       string
}

func (s *Server) handleUsersGet(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	members, err := s.store.ListHouseholdMembers(r.Context(), u.ActiveHouseholdID)
	if err != nil {
		http.Error(w, "failed to load members", http.StatusInternalServerError)
		return
	}
	balances, _ := s.store.ComputeHouseholdBalances(r.Context(), u.ActiveHouseholdID)
	settlements, _ := s.store.ListSettlements(r.Context(), u.ActiveHouseholdID, 10)
	s.render(w, r, "users.html", http.StatusOK, usersPageData{
		BasePage:    s.newBasePage(w, r, "Household", "users", true),
		Members:     members,
		Balances:    balances,
		Settlements: settlements,
		Today:       recurrence.Today(s.nowFn(), s.cfg.Location),
		InviteCode:  strings.TrimSpace(r.URL.Query().Get("invite")),
	})
}

func (s *Server) handleUsersPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	u, _ := s.currentUser(r)
	username := strings.TrimSpace(r.FormValue("username"))
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	role := r.FormValue("role")
	password := r.FormValue("password")
	confirm := r.FormValue("confirm_password")

	renderErr := func(status int, msg string) {
		members, _ := s.store.ListHouseholdMembers(r.Context(), u.ActiveHouseholdID)
		s.render(w, r, "users.html", status, usersPageData{
			BasePage:    s.newBasePage(w, r, "Household", "users", true),
			Members:     members,
			Username:    username,
			DisplayName: displayName,
			Error:       msg,
		})
	}

	if username == "" {
		renderErr(http.StatusUnprocessableEntity, "Username is required.")
		return
	}
	if len(password) < 8 {
		renderErr(http.StatusUnprocessableEntity, "Password must be at least 8 characters.")
		return
	}
	if password != confirm {
		renderErr(http.StatusUnprocessableEntity, "Passwords do not match.")
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		renderErr(http.StatusInternalServerError, "Could not hash password.")
		return
	}

	if _, err := s.store.CreateAndAddHouseholdMember(r.Context(), u.ActiveHouseholdID, username, displayName, hash, role); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			renderErr(http.StatusConflict, "That username already exists. Share an invite code instead if they already have an account.")
			return
		}
		renderErr(http.StatusInternalServerError, "Could not create user.")
		return
	}

	s.setFlash(w, r, "Household member added.")
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) handleHouseholdCreate(w http.ResponseWriter, r *http.Request) {
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
	if name == "" {
		s.setFlash(w, r, "Household name is required.")
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	hhID, err := s.store.CreateHousehold(r.Context(), name, u.ID)
	if err != nil {
		http.Error(w, "could not create household", http.StatusInternalServerError)
		return
	}
	if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
		_ = s.store.SetSessionHousehold(r.Context(), auth.HashToken(c.Value), u.ID, hhID)
	}
	s.setFlash(w, r, "Household created and selected.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleHouseholdSwitch(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	hhID, _ := strconv.ParseInt(r.FormValue("household_id"), 10, 64)
	if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
		if err := s.store.SetSessionHousehold(r.Context(), auth.HashToken(c.Value), u.ID, hhID); err == nil {
			s.setFlash(w, r, "Switched household.")
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleHouseholdInvite(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	code, err := auth.RandomToken(9)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.CreateInvite(r.Context(), u.ActiveHouseholdID, u.ID, code, s.nowFn().Add(7*24*time.Hour)); err != nil {
		http.Error(w, "could not create invite", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/users?invite="+code, http.StatusSeeOther)
}

func (s *Server) handleHouseholdJoin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	code := strings.TrimSpace(r.FormValue("code"))
	hhID, err := s.store.AcceptInvite(r.Context(), code, u.ID, s.nowFn())
	if err != nil {
		s.setFlash(w, r, "Invite code is invalid, expired, or already used.")
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
		_ = s.store.SetSessionHousehold(r.Context(), auth.HashToken(c.Value), u.ID, hhID)
	}
	s.setFlash(w, r, "Joined household.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
