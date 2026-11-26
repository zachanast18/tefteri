package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"spending-project/internal/auth"
	"spending-project/internal/config"
	"spending-project/internal/db"
	"spending-project/internal/money"
	"spending-project/internal/ocr"
)

//go:embed templates/*.html static/*
var assetsFS embed.FS

type Server struct {
	cfg       config.Config
	store     *db.Store
	scanner   *ocr.Scanner
	limiter   *auth.RateLimiter
	webauthn  *webauthn.WebAuthn
	templates map[string]*template.Template
	mux       *http.ServeMux
	nowFn     func() time.Time
}

type BasePage struct {
	Title               string
	Nav                 string
	Authenticated       bool
	CurrentUserName     string
	ActiveHouseholdID   int64
	ActiveHouseholdName string
	Households          []db.Household
	ReviewCount         int
	CSRFToken           string
	Flash               string
	Currency            string
}

type contextKey string

const userContextKey contextKey = "current_user"

func NewServer(cfg config.Config, store *db.Store, scanner *ocr.Scanner) (*Server, error) {
	s := &Server{
		cfg:       cfg,
		store:     store,
		scanner:   scanner,
		limiter:   auth.NewRateLimiter(),
		templates: make(map[string]*template.Template),
		mux:       http.NewServeMux(),
		nowFn:     time.Now,
	}
	if wa, err := s.initWebAuthn(); err == nil {
		s.webauthn = wa
	} else {
		log.Printf("webauthn disabled: %v", err)
	}

	funcs := template.FuncMap{
		"formatMoney": func(cents int64) string {
			return money.Format(cents, cfg.CurrencySymbol)
		},
		"subZero": func(cents int64) int64 {
			return -cents
		},
		"formatMoneyPtr": func(cents *int64) string {
			if cents == nil {
				return ""
			}
			return money.Format(*cents, cfg.CurrencySymbol)
		},
		"formatInputMoney": func(cents int64) string {
			return money.FormatInput(cents)
		},
		"formatDate": func(ymd string) string {
			t, err := time.Parse("2006-01-02", ymd)
			if err != nil {
				return ymd
			}
			// Day-first locale format for Europe/Athens & Greek receipts.
			return t.Format("02/01/2006")
		},
		"paymentLabel": func(m string) string {
			if m == "cash" {
				return "Cash"
			}
			if m == "card" {
				return "Card"
			}
			return m
		},
	}

	pages := []string{
		"login.html",
		"mfa.html",
		"recovery.html",
		"security.html",
		"overview.html",
		"scan.html",
		"expense_form.html",
		"expense_detail.html",
		"ledger.html",
		"subscriptions.html",
		"users.html",
		"inbox.html",
	}
	for _, p := range pages {
		tmpl, err := template.New("layout").Funcs(funcs).ParseFS(assetsFS, "templates/layout.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", p, err)
		}
		s.templates[p] = tmpl
	}

	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.mux)
}

func (s *Server) routes() {
	staticSub, _ := fs.Sub(assetsFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	s.mux.HandleFunc("GET /login", s.handleLoginGet)
	s.mux.HandleFunc("POST /login", s.handleLoginPost)
	s.mux.HandleFunc("GET /recovery", s.handleRecoveryGet)
	s.mux.HandleFunc("POST /recovery", s.handleRecoveryPost)
	s.mux.HandleFunc("POST /webauthn/login/begin", s.handleWebAuthnLoginBegin)
	s.mux.HandleFunc("POST /webauthn/login/finish", s.handleWebAuthnLoginFinish)

	s.mux.HandleFunc("GET /mfa", s.requireSessionOnly(s.handleMFAGet))
	s.mux.HandleFunc("POST /mfa", s.requireSessionOnly(s.handleMFAPost))
	s.mux.HandleFunc("POST /logout", s.requireSessionOnly(s.handleLogout))

	s.mux.HandleFunc("GET /security", s.requireAuthNoHousehold(s.handleSecurityGet))
	s.mux.HandleFunc("POST /security/totp/enable", s.requireAuthNoHousehold(s.handleTOTPEnable))
	s.mux.HandleFunc("POST /security/totp/disable", s.requireAuthNoHousehold(s.handleTOTPDisable))
	s.mux.HandleFunc("POST /security/recovery/regenerate", s.requireAuthNoHousehold(s.handleRecoveryRegenerate))
	s.mux.HandleFunc("POST /security/sessions/revoke-all", s.requireAuthNoHousehold(s.handleSessionsRevokeAll))
	s.mux.HandleFunc("POST /security/passkeys/delete", s.requireAuthNoHousehold(s.handlePasskeyDelete))
	s.mux.HandleFunc("POST /webauthn/register/begin", s.requireAuthNoHousehold(s.handleWebAuthnRegisterBegin))
	s.mux.HandleFunc("POST /webauthn/register/finish", s.requireAuthNoHousehold(s.handleWebAuthnRegisterFinish))

	s.mux.HandleFunc("GET /{$}", s.requireAuth(s.handleOverview))
	s.mux.HandleFunc("GET /scan", s.requireAuth(s.handleScanGet))
	s.mux.HandleFunc("POST /scan", s.requireAuth(s.handleScanPost))

	s.mux.HandleFunc("GET /expenses/new", s.requireAuth(s.handleExpenseNewGet))
	s.mux.HandleFunc("POST /expenses", s.requireAuth(s.handleExpenseCreatePost))
	s.mux.HandleFunc("GET /expenses/{id}", s.requireAuth(s.handleExpenseDetail))
	s.mux.HandleFunc("GET /expenses/{id}/edit", s.requireAuth(s.handleExpenseEditGet))
	s.mux.HandleFunc("POST /expenses/{id}/edit", s.requireAuth(s.handleExpenseEditPost))
	s.mux.HandleFunc("POST /expenses/{id}/delete", s.requireAuth(s.handleExpenseDeletePost))

	s.mux.HandleFunc("GET /receipts/{id}", s.requireAuth(s.handleReceiptImage))

	s.mux.HandleFunc("GET /ledger", s.requireAuth(s.handleLedger))
	s.mux.HandleFunc("GET /ledger/export.csv", s.requireAuth(s.handleLedgerCSV))
	s.mux.HandleFunc("POST /categories", s.requireAuth(s.handleCreateCategory))

	s.mux.HandleFunc("GET /subscriptions", s.requireAuth(s.handleSubscriptionsGet))
	s.mux.HandleFunc("POST /subscriptions", s.requireAuth(s.handleSubscriptionCreate))
	s.mux.HandleFunc("POST /subscriptions/{id}/edit", s.requireAuth(s.handleSubscriptionEdit))
	s.mux.HandleFunc("POST /subscriptions/{id}/confirm", s.requireAuth(s.handleSubscriptionConfirm))
	s.mux.HandleFunc("POST /subscriptions/{id}/skip", s.requireAuth(s.handleSubscriptionSkip))
	s.mux.HandleFunc("POST /subscriptions/{id}/status", s.requireAuth(s.handleSubscriptionStatus))

	s.mux.HandleFunc("GET /inbox", s.requireAuth(s.handleInboxGet))
	s.mux.HandleFunc("POST /inbox/{id}/discard", s.requireAuth(s.handleInboxDiscard))
	s.mux.HandleFunc("POST /budgets", s.requireAuth(s.handleBudgetSave))
	s.mux.HandleFunc("POST /settlements", s.requireAuth(s.handleSettlementCreate))

	s.mux.HandleFunc("GET /users", s.requireAuthNoHousehold(s.handleUsersGet))
	s.mux.HandleFunc("POST /users", s.requireAuth(s.handleUsersPost))
	s.mux.HandleFunc("POST /households", s.requireAuthNoHousehold(s.handleHouseholdCreate))
	s.mux.HandleFunc("POST /households/switch", s.requireAuthNoHousehold(s.handleHouseholdSwitch))
	s.mux.HandleFunc("POST /households/invite", s.requireAuth(s.handleHouseholdInvite))
	s.mux.HandleFunc("POST /households/invites/revoke", s.requireAuth(s.handleHouseholdInviteRevoke))
	s.mux.HandleFunc("POST /households/join", s.requireAuthNoHousehold(s.handleHouseholdJoin))
	s.mux.HandleFunc("POST /households/members/remove", s.requireAuth(s.handleHouseholdMemberRemove))
	s.mux.HandleFunc("POST /households/ownership/transfer", s.requireAuth(s.handleHouseholdOwnershipTransfer))
	s.mux.HandleFunc("POST /households/delete", s.requireAuth(s.handleHouseholdDelete))
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.cfg.TrustedProxy != "" && host == s.cfg.TrustedProxy {
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[0])
		}
	}
	return host
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DB.PingContext(r.Context()); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) requireSessionOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(auth.SessionCookieName)
		if err != nil || strings.TrimSpace(c.Value) == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		user, err := s.store.GetSessionUserWithIdle(r.Context(), auth.HashToken(c.Value), s.nowFn(), s.cfg.SessionIdleTTL)
		if err != nil {
			auth.ClearSessionCookie(w, r, s.cfg.CookieSecure)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		ctx := context.WithValue(r.Context(), userContextKey, user)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) requireAuthNoHousehold(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSessionOnly(func(w http.ResponseWriter, r *http.Request) {
		u, _ := s.currentUser(r)
		if !u.MFAVerified {
			http.Redirect(w, r, "/mfa", http.StatusSeeOther)
			return
		}
		next(w, r)
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuthNoHousehold(func(w http.ResponseWriter, r *http.Request) {
		u, _ := s.currentUser(r)
		if u.ActiveHouseholdID <= 0 {
			s.setFlash(w, r, "You do not belong to an active household. Join with an invite code or create one.")
			http.Redirect(w, r, "/users", http.StatusSeeOther)
			return
		}
		next(w, r)
	})
}

func (s *Server) currentUser(r *http.Request) (db.User, bool) {
	u, ok := r.Context().Value(userContextKey).(db.User)
	return u, ok
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, status int, data any) {
	tmpl, ok := s.templates[name]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) newBasePage(w http.ResponseWriter, r *http.Request, title, nav string, authed bool) BasePage {
	csrf := auth.EnsureCSRFCookie(w, r, s.cfg.CookieSecure)
	flash := s.popFlash(w, r)
	var userName, hhName string
	var hhID int64
	var hhs []db.Household
	var revCount int
	if u, ok := s.currentUser(r); ok {
		userName = u.Label()
		hhID = u.ActiveHouseholdID
		hhName = u.ActiveHouseholdName
		hhs, _ = s.store.ListUserHouseholds(r.Context(), u.ID)
		revCount, _ = s.store.CountReviewDrafts(r.Context(), hhID)
	}
	return BasePage{
		Title:               title,
		Nav:                 nav,
		Authenticated:       authed,
		CurrentUserName:     userName,
		ActiveHouseholdID:   hhID,
		ActiveHouseholdName: hhName,
		Households:          hhs,
		ReviewCount:         revCount,
		CSRFToken:           csrf,
		Flash:               flash,
		Currency:            s.cfg.Currency,
	}
}

func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "household_flash",
		Value:    strings.ReplaceAll(msg, ";", ","),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   auth.IsHTTPS(r, s.cfg.CookieSecure),
		MaxAge:   60,
	})
}

func (s *Server) popFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie("household_flash")
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "household_flash",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   auth.IsHTTPS(r, s.cfg.CookieSecure),
		MaxAge:   -1,
	})
	return c.Value
}
