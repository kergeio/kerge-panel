package web

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/kergeio/kerge-panel/internal/auth"
	"github.com/kergeio/kerge-panel/internal/i18n"
	"github.com/kergeio/kerge-panel/internal/setup"
)

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.opts.Health != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.opts.Health(ctx); err != nil {
			s.opts.Logger.Error("health check failed", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable\n"))
			return
		}
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) pageNotFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "not_found.html", nil)
}

func (s *Server) apiNotFound(w http.ResponseWriter, r *http.Request) {
	s.apiError(w, http.StatusNotFound, msgKey("error.not_found"))
}

// checkPreSession verifies the CSRF token of an anonymous form. On failure
// it writes a 403 page and returns false.
func (s *Server) checkPreSession(w http.ResponseWriter, r *http.Request) bool {
	if s.opts.CSRF.CheckPreSession(r, r.PostFormValue("csrf_token")) {
		return true
	}
	s.render(w, r, http.StatusForbidden, "error.html", errorPage{Message: msgKey("error.csrf")})
	return false
}

func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
}

// Setup.

type option struct {
	Value, Label string
	Selected     bool
}

type setupForm struct {
	Error     msgKey
	Username  string
	Languages []option
	TimeZones []option
}

func (s *Server) newSetupForm(username, lang, tz string) setupForm {
	f := setupForm{Username: username}
	if lang == "" {
		lang = i18n.Default
	}
	if tz == "" {
		tz = "UTC"
	}
	for _, l := range s.opts.Catalog.Languages() {
		f.Languages = append(f.Languages, option{
			Value: l, Label: s.opts.Catalog.Translator(l).T(string(msgKey("language.name"))), Selected: l == lang,
		})
	}
	for _, z := range i18n.TimeZones {
		f.TimeZones = append(f.TimeZones, option{Value: z, Label: z, Selected: z == tz})
	}
	return f
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "setup.html", s.newSetupForm("", "", ""))
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkPreSession(w, r) {
		return
	}
	ipKey := "ip:" + ClientIPFrom(r.Context()).String()
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	lang := r.PostFormValue("language")
	tz := r.PostFormValue("timezone")
	fail := func(status int, key msgKey) {
		f := s.newSetupForm(username, lang, tz)
		f.Error = key
		s.render(w, r, status, "setup.html", f)
	}

	if d := s.setupLimiter.Blocked(ipKey); d > 0 {
		setRetryAfter(w, d)
		fail(http.StatusTooManyRequests, msgKey("error.rate_limited"))
		return
	}
	code := r.PostFormValue("code")
	ok, err := s.opts.Setup.CheckCode(r.Context(), code)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !ok {
		s.setupLimiter.Fail(ipKey)
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.code"))
		return
	}
	switch {
	case !auth.ValidUsername(username):
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.username"))
		return
	case !auth.ValidPassword(password):
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.password_length"))
		return
	case password != r.PostFormValue("password_confirm"):
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.password_mismatch"))
		return
	case !s.opts.Catalog.Has(lang):
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.language"))
		return
	case !validTimeZone(tz):
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.timezone"))
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var token string
	err = s.opts.Setup.Complete(r.Context(), setup.Params{
		Code: code, Username: username, PasswordHash: hash, Language: lang, TimeZone: tz,
	}, func(ctx context.Context, tx *sql.Tx, userID int64) error {
		var err error
		token, err = s.opts.Auth.CreateSession(ctx, tx, userID, ClientIPFrom(ctx), r.UserAgent())
		return err
	})
	switch {
	case errors.Is(err, setup.ErrWrongCode):
		s.setupLimiter.Fail(ipKey)
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.code"))
		return
	case errors.Is(err, setup.ErrAlreadyInitialized):
		s.pageNotFound(w, r)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.setupLimiter.Reset(ipKey)
	s.opts.Logger.Info("initial setup completed", "username", username)
	auth.SetSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func validTimeZone(tz string) bool {
	if !slices.Contains(i18n.TimeZones, tz) {
		return false
	}
	_, err := time.LoadLocation(tz)
	return err == nil
}

// Login and logout.

type loginForm struct {
	Error    msgKey
	Username string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.opts.Auth.Authenticate(r); ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.render(w, r, http.StatusOK, "login.html", loginForm{})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkPreSession(w, r) {
		return
	}
	username := r.PostFormValue("username")
	token, err := s.opts.Auth.Login(r.Context(), username, r.PostFormValue("password"),
		ClientIPFrom(r.Context()), r.UserAgent())
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		setRetryAfter(w, limited.RetryAfter)
		s.render(w, r, http.StatusTooManyRequests, "login.html",
			loginForm{Error: msgKey("error.rate_limited"), Username: username})
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.render(w, r, http.StatusUnauthorized, "login.html",
			loginForm{Error: msgKey("login.error.invalid"), Username: username})
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	auth.SetSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	if err := s.opts.Auth.Logout(r.Context(), id.SessionHash); err != nil {
		s.internalError(w, r, err)
		return
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.opts.Logger.Error("request failed", "path", r.URL.Path, "err", err)
	s.render(w, r, http.StatusInternalServerError, "error.html", errorPage{Message: msgKey("error.internal")})
}
