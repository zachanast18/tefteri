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

	if !s.limiter.Allow("login:"+s.clientIP(r), 15, 5*time.Minute, s.nowFn()) {
		hasUser, _ := s.store.HasUser(r.Context())
		s.render(w, r, "login.html", http.StatusTooManyRequests, loginPageData{
			BasePage: s.newBasePage(w, r, "Sign in", "", false),
			HasUser:  hasUser,
			Error:    "Too many sign-in attempts. Wait a few minutes.",
		})
		return
	}

	hasUser, _ := s.store.HasUser(r.Context())
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	user, err := s.store.GetUserByUsername(r.Context(), username)
	matched, needsRehash := false, false
	if err == nil {
		matched, needsRehash = auth.VerifyPassword(user.PasswordHash, password)
	}
	if err != nil || !matched {
		var uidPtr *int64
		if err == nil {
			uidPtr = &user.ID
		}
		_ = s.store.LogAudit(r.Context(), "login_failed", uidPtr, nil, "")
		s.render(w, r, "login.html", http.StatusUnauthorized, loginPageData{
			BasePage: s.newBasePage(w, r, "Sign in", "", false),
			HasUser:  hasUser,
			Username: username,
			Error:    "Invalid username or password.",
		})
		return
	}

	if needsRehash {
		if newHash, err := auth.HashPassword(password); err == nil {
			_ = s.store.UpdateUserPassword(r.Context(), user.ID, newHash)
		}
	}

	rawToken, err := auth.RandomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.nowFn()
	mfaOK := !user.TOTPEnabled
	if err := s.store.CreateSessionFull(r.Context(), auth.HashToken(rawToken), user.ID, mfaOK, now.Add(s.cfg.SessionTTL), now); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	auth.SetSessionCookie(w, r, rawToken, s.cfg.SessionTTL, s.cfg.CookieSecure)
	if !mfaOK {
		http.Redirect(w, r, "/mfa", http.StatusSeeOther)
		return
	}
	_ = s.store.LogAudit(r.Context(), "login_password_success", &user.ID, nil, "")
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
	Members       []db.HouseholdMember
	Balances      []db.MemberBalance
	Settlements   []db.Settlement
	ActiveInvites []db.HouseholdInvite
	IsOwner       bool
	Today         string
	InviteCode    string
	Username      string
	DisplayName   string
	Error         string
}

func (s *Server) handleUsersGet(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	var members []db.HouseholdMember
	var balances []db.MemberBalance
	var settlements []db.Settlement
	var invites []db.HouseholdInvite
	if u.ActiveHouseholdID > 0 {
		members, _ = s.store.ListHouseholdMembers(r.Context(), u.ActiveHouseholdID)
		balances, _ = s.store.ComputeHouseholdBalances(r.Context(), u.ActiveHouseholdID)
		settlements, _ = s.store.ListSettlements(r.Context(), u.ActiveHouseholdID, 10)
		if u.ActiveRole == "owner" {
			invites, _ = s.store.ListActiveInvites(r.Context(), u.ActiveHouseholdID, s.nowFn())
		}
	}
	s.render(w, r, "users.html", http.StatusOK, usersPageData{
		BasePage:      s.newBasePage(w, r, "Household", "users", true),
		Members:       members,
		Balances:      balances,
		Settlements:   settlements,
		ActiveInvites: invites,
		IsOwner:       u.ActiveRole == "owner",
		Today:         recurrence.Today(s.nowFn(), s.cfg.Location),
		InviteCode:    strings.TrimSpace(r.URL.Query().Get("invite")),
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
	if u.ActiveRole != "owner" {
		http.Error(w, "only household owners can add members", http.StatusForbidden)
		return
	}
	if s.cfg.RegistrationMode == "closed" {
		s.setFlash(w, r, "Member registration is currently closed by the instance administrator.")
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
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
			IsOwner:     true,
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

	uid, err := s.store.CreateAndAddHouseholdMember(r.Context(), u.ActiveHouseholdID, username, displayName, hash, role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			renderErr(http.StatusConflict, "That username already exists. Share an invite code instead if they already have an account.")
			return
		}
		renderErr(http.StatusInternalServerError, "Could not create user.")
		return
	}

	_ = s.store.LogAudit(r.Context(), "member_created", &u.ID, &u.ActiveHouseholdID, "target_user="+strconv.FormatInt(uid, 10))
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
	if u.ActiveRole != "owner" {
		http.Error(w, "only household owners can create invites", http.StatusForbidden)
		return
	}
	code, err := auth.RandomToken(12)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	role := r.FormValue("role")
	if err := s.store.CreateInviteWithRole(r.Context(), u.ActiveHouseholdID, u.ID, code, role, s.nowFn().Add(7*24*time.Hour)); err != nil {
		http.Error(w, "could not create invite", http.StatusInternalServerError)
		return
	}
	_ = s.store.LogAudit(r.Context(), "invite_created", &u.ID, &u.ActiveHouseholdID, "")
	http.Redirect(w, r, "/users?invite="+code, http.StatusSeeOther)
}

func (s *Server) handleHouseholdInviteRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	if u.ActiveRole != "owner" {
		http.Error(w, "only household owners can revoke invites", http.StatusForbidden)
		return
	}
	if err := s.store.RevokeInvite(r.Context(), u.ActiveHouseholdID, r.FormValue("code")); err != nil {
		s.setFlash(w, r, "Invite not found.")
	} else {
		_ = s.store.LogAudit(r.Context(), "invite_revoked", &u.ID, &u.ActiveHouseholdID, "")
		s.setFlash(w, r, "Invite revoked.")
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
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
	if !s.limiter.Allow("invite:"+s.clientIP(r), 10, 15*time.Minute, s.nowFn()) {
		s.setFlash(w, r, "Too many invite attempts. Wait 15 minutes.")
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	if s.cfg.RegistrationMode == "closed" {
		s.setFlash(w, r, "Enrollment is currently closed.")
		http.Redirect(w, r, "/users", http.StatusSeeOther)
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
	_ = s.store.LogAudit(r.Context(), "household_joined", &u.ID, &hhID, "")
	s.setFlash(w, r, "Joined household.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleHouseholdMemberRemove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	if u.ActiveRole != "owner" {
		http.Error(w, "only household owners can remove members", http.StatusForbidden)
		return
	}
	targetID, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err := s.store.RemoveHouseholdMember(r.Context(), u.ActiveHouseholdID, targetID); err != nil {
		s.setFlash(w, r, err.Error())
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	_ = s.store.LogAudit(r.Context(), "member_removed", &u.ID, &u.ActiveHouseholdID, "target_user="+strconv.FormatInt(targetID, 10))
	s.setFlash(w, r, "Member removed.")
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) handleHouseholdOwnershipTransfer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	if u.ActiveRole != "owner" {
		http.Error(w, "only household owners can transfer ownership", http.StatusForbidden)
		return
	}
	newOwnerID, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err := s.store.TransferOwnership(r.Context(), u.ActiveHouseholdID, newOwnerID, u.ID); err != nil {
		s.setFlash(w, r, err.Error())
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	_ = s.store.LogAudit(r.Context(), "ownership_transferred", &u.ID, &u.ActiveHouseholdID, "new_owner="+strconv.FormatInt(newOwnerID, 10))
	s.setFlash(w, r, "Ownership transferred.")
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) handleHouseholdDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	if u.ActiveRole != "owner" {
		http.Error(w, "only household owners can delete a household", http.StatusForbidden)
		return
	}
	hhID := u.ActiveHouseholdID
	if err := s.store.DeleteHousehold(r.Context(), hhID); err != nil {
		s.setFlash(w, r, "Could not delete household.")
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	_ = s.store.LogAudit(r.Context(), "household_deleted", &u.ID, &hhID, "")
	s.setFlash(w, r, "Household deleted.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
