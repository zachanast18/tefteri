package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"spending-project/internal/auth"
	"spending-project/internal/db"
)

type waUser struct {
	user  db.User
	creds []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte {
	return []byte{
		byte(u.user.ID >> 56), byte(u.user.ID >> 48), byte(u.user.ID >> 40), byte(u.user.ID >> 32),
		byte(u.user.ID >> 24), byte(u.user.ID >> 16), byte(u.user.ID >> 8), byte(u.user.ID),
	}
}
func (u waUser) WebAuthnName() string        { return u.user.Username }
func (u waUser) WebAuthnDisplayName() string { return u.user.Label() }
func (u waUser) WebAuthnCredentials() []webauthn.Credential {
	return u.creds
}

func (s *Server) initWebAuthn() (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPDisplayName: "Teftéri",
		RPID:          s.cfg.WebAuthnRPID,
		RPOrigins:     []string{s.cfg.WebAuthnRPOrigin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
		},
	})
}

func (s *Server) hasRecentStrongAuth(u db.User) bool {
	if u.LastAuthAt.IsZero() {
		return false
	}
	return s.nowFn().UTC().Sub(u.LastAuthAt) <= 15*time.Minute
}

func (s *Server) handleMFAGet(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "mfa.html", http.StatusOK, struct {
		BasePage
		Error string
	}{
		BasePage: s.newBasePage(w, r, "Two-factor verification", "", false),
	})
}

func (s *Server) handleMFAPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
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
	if !s.limiter.Allow("mfa:"+s.clientIP(r), 10, 5*time.Minute, s.nowFn()) {
		s.render(w, r, "mfa.html", http.StatusTooManyRequests, struct {
			BasePage
			Error string
		}{BasePage: s.newBasePage(w, r, "Two-factor verification", "", false), Error: "Too many attempts. Wait a few minutes."})
		return
	}

	seed, err := s.store.Keys.DecryptTOTPSeed(u.ID, u.TOTPKekVersion, u.TOTPSeedEnc)
	if err != nil || !auth.VerifyTOTP(seed, r.FormValue("code"), s.nowFn()) {
		_ = s.store.LogAudit(r.Context(), "mfa_failed", &u.ID, nil, "")
		s.render(w, r, "mfa.html", http.StatusUnauthorized, struct {
			BasePage
			Error string
		}{BasePage: s.newBasePage(w, r, "Two-factor verification", "", false), Error: "Invalid verification code."})
		return
	}

	// Rotate session identifier on MFA completion.
	c, _ := r.Cookie(auth.SessionCookieName)
	newRaw, err := auth.RandomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.nowFn()
	if err := s.store.RotateSession(r.Context(), auth.HashToken(c.Value), auth.HashToken(newRaw), u.ID, true, now.Add(s.cfg.SessionTTL), now); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, r, newRaw, s.cfg.SessionTTL, s.cfg.CookieSecure)
	_ = s.store.LogAudit(r.Context(), "login_totp_success", &u.ID, nil, "")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleRecoveryGet(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "recovery.html", http.StatusOK, struct {
		BasePage
		Username string
		Error    string
	}{
		BasePage: s.newBasePage(w, r, "Account recovery", "", false),
	})
}

func (s *Server) handleRecoveryPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if !s.limiter.Allow("recovery:"+s.clientIP(r), 5, 15*time.Minute, s.nowFn()) {
		s.render(w, r, "recovery.html", http.StatusTooManyRequests, struct {
			BasePage
			Username string
			Error    string
		}{BasePage: s.newBasePage(w, r, "Account recovery", "", false), Error: "Too many recovery attempts. Wait 15 minutes."})
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	code := r.FormValue("recovery_code")
	user, err := s.store.GetUserByUsername(r.Context(), username)
	if err != nil {
		s.render(w, r, "recovery.html", http.StatusUnauthorized, struct {
			BasePage
			Username string
			Error    string
		}{BasePage: s.newBasePage(w, r, "Account recovery", "", false), Username: username, Error: "Invalid username or recovery code."})
		return
	}

	ok, err := s.store.UseRecoveryCode(r.Context(), user.ID, auth.NormalizeRecoveryCode(code), s.nowFn())
	if err != nil || !ok {
		_ = s.store.LogAudit(r.Context(), "recovery_failed", &user.ID, nil, "")
		s.render(w, r, "recovery.html", http.StatusUnauthorized, struct {
			BasePage
			Username string
			Error    string
		}{BasePage: s.newBasePage(w, r, "Account recovery", "", false), Username: username, Error: "Invalid username or recovery code."})
		return
	}

	// Revoke existing sessions on recovery sign-in and issue a fresh session.
	_ = s.store.DeleteAllUserSessions(r.Context(), user.ID)
	rawToken, err := auth.RandomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.nowFn()
	if err := s.store.CreateSessionFull(r.Context(), auth.HashToken(rawToken), user.ID, true, now.Add(s.cfg.SessionTTL), now); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, r, rawToken, s.cfg.SessionTTL, s.cfg.CookieSecure)
	_ = s.store.LogAudit(r.Context(), "recovery_success", &user.ID, nil, "")
	s.setFlash(w, r, "Signed in with recovery code. Review your passkeys and TOTP settings now.")
	http.Redirect(w, r, "/security", http.StatusSeeOther)
}

type securityPageData struct {
	BasePage
	User              db.User
	Passkeys          []passkeyView
	UnusedRecovery    int
	NewTOTPSeed       string
	NewTOTPURI        string
	NewRecoveryCodes  []string
	RecentAuthOK      bool
	Error             string
}

type passkeyView struct {
	IDB64     string
	Label     string
	CreatedAt string
}

func (s *Server) handleSecurityGet(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	_, infos, _ := s.store.ListWebAuthnCredentials(r.Context(), u.ID)
	views := make([]passkeyView, 0, len(infos))
	for _, info := range infos {
		views = append(views, passkeyView{
			IDB64:     base64.RawURLEncoding.EncodeToString(info.ID),
			Label:     info.Label,
			CreatedAt: info.CreatedAt,
		})
	}
	unused, _ := s.store.CountUnusedRecoveryCodes(r.Context(), u.ID)

	var newSeed, newURI string
	if !u.TOTPEnabled {
		if seed, err := auth.GenerateTOTPSeed(); err == nil {
			newSeed = seed
			newURI = auth.TOTPAuthURI(u.Username, seed)
		}
	}

	s.render(w, r, "security.html", http.StatusOK, securityPageData{
		BasePage:       s.newBasePage(w, r, "Security", "security", true),
		User:           u,
		Passkeys:       views,
		UnusedRecovery: unused,
		NewTOTPSeed:    newSeed,
		NewTOTPURI:     newURI,
		RecentAuthOK:   s.hasRecentStrongAuth(u),
	})
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	if !s.hasRecentStrongAuth(u) {
		s.setFlash(w, r, "Please sign in again before changing MFA settings.")
		http.Redirect(w, r, "/security", http.StatusSeeOther)
		return
	}
	seed := strings.TrimSpace(r.FormValue("seed"))
	code := strings.TrimSpace(r.FormValue("code"))
	if !auth.VerifyTOTP(seed, code, s.nowFn()) {
		s.setFlash(w, r, "Invalid authenticator code. TOTP not enabled.")
		http.Redirect(w, r, "/security", http.StatusSeeOther)
		return
	}
	enc, ver, err := s.store.Keys.EncryptTOTPSeed(u.ID, seed)
	if err != nil {
		http.Error(w, "encryption error", http.StatusInternalServerError)
		return
	}
	if err := s.store.EnableTOTP(r.Context(), u.ID, enc, ver); err != nil {
		http.Error(w, "failed to enable TOTP", http.StatusInternalServerError)
		return
	}
	_ = s.store.LogAudit(r.Context(), "totp_enabled", &u.ID, nil, "")
	s.setFlash(w, r, "TOTP two-factor authentication enabled.")
	http.Redirect(w, r, "/security", http.StatusSeeOther)
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	if !s.hasRecentStrongAuth(u) {
		s.setFlash(w, r, "Please sign in again before changing MFA settings.")
		http.Redirect(w, r, "/security", http.StatusSeeOther)
		return
	}
	if err := s.store.DisableTOTP(r.Context(), u.ID); err != nil {
		http.Error(w, "failed to disable TOTP", http.StatusInternalServerError)
		return
	}
	_ = s.store.LogAudit(r.Context(), "totp_disabled", &u.ID, nil, "")
	s.setFlash(w, r, "TOTP disabled.")
	http.Redirect(w, r, "/security", http.StatusSeeOther)
}

func (s *Server) handleRecoveryRegenerate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	if !s.hasRecentStrongAuth(u) {
		s.setFlash(w, r, "Please sign in again before regenerating recovery codes.")
		http.Redirect(w, r, "/security", http.StatusSeeOther)
		return
	}
	plain, hashes, err := auth.GenerateRecoveryCodes(8)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.ReplaceRecoveryCodes(r.Context(), u.ID, hashes); err != nil {
		http.Error(w, "failed to save recovery codes", http.StatusInternalServerError)
		return
	}
	_ = s.store.LogAudit(r.Context(), "recovery_codes_regenerated", &u.ID, nil, "")

	_, infos, _ := s.store.ListWebAuthnCredentials(r.Context(), u.ID)
	views := make([]passkeyView, 0, len(infos))
	for _, info := range infos {
		views = append(views, passkeyView{
			IDB64:     base64.RawURLEncoding.EncodeToString(info.ID),
			Label:     info.Label,
			CreatedAt: info.CreatedAt,
		})
	}
	s.render(w, r, "security.html", http.StatusOK, securityPageData{
		BasePage:         s.newBasePage(w, r, "Security", "security", true),
		User:             u,
		Passkeys:         views,
		UnusedRecovery:   len(plain),
		NewRecoveryCodes: plain,
		RecentAuthOK:     true,
	})
}

func (s *Server) handleSessionsRevokeAll(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	_ = s.store.DeleteAllUserSessions(r.Context(), u.ID)
	auth.ClearSessionCookie(w, r, s.cfg.CookieSecure)
	_ = s.store.LogAudit(r.Context(), "sessions_revoked_all", &u.ID, nil, "")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !auth.ValidateCSRF(r, r.FormValue("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	if !s.hasRecentStrongAuth(u) {
		s.setFlash(w, r, "Please sign in again before removing a passkey.")
		http.Redirect(w, r, "/security", http.StatusSeeOther)
		return
	}
	credID, err := base64.RawURLEncoding.DecodeString(r.FormValue("cred_id"))
	if err != nil {
		http.Error(w, "bad credential id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteWebAuthnCredential(r.Context(), u.ID, credID); err != nil {
		s.setFlash(w, r, err.Error())
		http.Redirect(w, r, "/security", http.StatusSeeOther)
		return
	}
	_ = s.store.LogAudit(r.Context(), "passkey_deleted", &u.ID, nil, "")
	s.setFlash(w, r, "Passkey removed.")
	http.Redirect(w, r, "/security", http.StatusSeeOther)
}

// WebAuthn registration (authenticated)
func (s *Server) handleWebAuthnRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !auth.ValidateCSRF(r, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if s.webauthn == nil {
		http.Error(w, "webauthn unavailable", http.StatusServiceUnavailable)
		return
	}
	u, _ := s.currentUser(r)
	if !s.hasRecentStrongAuth(u) {
		http.Error(w, "recent authentication required", http.StatusForbidden)
		return
	}
	creds, _, _ := s.store.ListWebAuthnCredentials(r.Context(), u.ID)
	opts, sessionData, err := s.webauthn.BeginRegistration(waUser{user: u, creds: creds})
	if err != nil {
		http.Error(w, "failed to begin registration", http.StatusInternalServerError)
		return
	}
	chID, _ := auth.RandomToken(24)
	_ = s.store.SaveWebAuthnChallenge(r.Context(), chID, &u.ID, "register", *sessionData, s.nowFn().Add(5*time.Minute))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"challenge_id": chID, "options": opts})
}

func (s *Server) handleWebAuthnRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !auth.ValidateCSRF(r, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, _ := s.currentUser(r)
	chID := r.URL.Query().Get("challenge_id")
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if label == "" {
		label = "Passkey"
	}
	sd, uidPtr, err := s.store.ConsumeWebAuthnChallenge(r.Context(), chID, "register", s.nowFn())
	if err != nil || uidPtr == nil || *uidPtr != u.ID {
		http.Error(w, "invalid or expired challenge", http.StatusBadRequest)
		return
	}
	creds, _, _ := s.store.ListWebAuthnCredentials(r.Context(), u.ID)
	cred, err := s.webauthn.FinishRegistration(waUser{user: u, creds: creds}, sd, r)
	if err != nil {
		http.Error(w, "passkey verification failed", http.StatusBadRequest)
		return
	}
	if err := s.store.SaveWebAuthnCredential(r.Context(), u.ID, cred.ID, label, *cred); err != nil {
		http.Error(w, "failed to save passkey", http.StatusInternalServerError)
		return
	}
	_ = s.store.LogAudit(r.Context(), "passkey_registered", &u.ID, nil, label)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// WebAuthn login (unauthenticated)
func (s *Server) handleWebAuthnLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !auth.ValidateCSRF(r, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if !s.limiter.Allow("login:"+s.clientIP(r), 15, 5*time.Minute, s.nowFn()) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	if s.webauthn == nil {
		http.Error(w, "webauthn unavailable", http.StatusServiceUnavailable)
		return
	}
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	var opts *protocol.CredentialAssertion
	var sd *webauthn.SessionData
	var uidPtr *int64
	var err error

	if username != "" {
		u, uErr := s.store.GetUserByUsername(r.Context(), username)
		if uErr != nil {
			http.Error(w, "unknown user", http.StatusBadRequest)
			return
		}
		creds, _, _ := s.store.ListWebAuthnCredentials(r.Context(), u.ID)
		if len(creds) == 0 {
			http.Error(w, "no passkeys registered", http.StatusBadRequest)
			return
		}
		uidPtr = &u.ID
		opts, sd, err = s.webauthn.BeginLogin(waUser{user: u, creds: creds})
	} else {
		opts, sd, err = s.webauthn.BeginDiscoverableLogin()
	}
	if err != nil {
		http.Error(w, "failed to begin passkey login", http.StatusInternalServerError)
		return
	}
	chID, _ := auth.RandomToken(24)
	_ = s.store.SaveWebAuthnChallenge(r.Context(), chID, uidPtr, "login", *sd, s.nowFn().Add(5*time.Minute))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"challenge_id": chID, "options": opts})
}

func (s *Server) handleWebAuthnLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !auth.ValidateCSRF(r, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	chID := r.URL.Query().Get("challenge_id")
	sd, _, err := s.store.ConsumeWebAuthnChallenge(r.Context(), chID, "login", s.nowFn())
	if err != nil {
		http.Error(w, "invalid or expired challenge", http.StatusBadRequest)
		return
	}

	var matchedUser db.User
	cred, err := s.webauthn.FinishDiscoverableLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		if len(userHandle) != 8 {
			return nil, db.ErrNotFound
		}
		uid := int64(userHandle[0])<<56 | int64(userHandle[1])<<48 | int64(userHandle[2])<<40 | int64(userHandle[3])<<32 |
			int64(userHandle[4])<<24 | int64(userHandle[5])<<16 | int64(userHandle[6])<<8 | int64(userHandle[7])
		u, err := s.store.GetUserByID(r.Context(), uid)
		if err != nil {
			return nil, err
		}
		matchedUser = u
		creds, _, _ := s.store.ListWebAuthnCredentials(r.Context(), u.ID)
		return waUser{user: u, creds: creds}, nil
	}, sd, r)
	if err != nil {
		http.Error(w, "passkey login failed", http.StatusUnauthorized)
		return
	}
	_ = s.store.UpdateWebAuthnCredential(r.Context(), cred.ID, *cred)

	rawToken, err := auth.RandomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.nowFn()
	if err := s.store.CreateSessionFull(r.Context(), auth.HashToken(rawToken), matchedUser.ID, true, now.Add(s.cfg.SessionTTL), now); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, r, rawToken, s.cfg.SessionTTL, s.cfg.CookieSecure)
	_ = s.store.LogAudit(r.Context(), "login_passkey_success", &matchedUser.ID, nil, "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": "/"})
}
