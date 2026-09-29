// Package web implements the panel's HTTP layer: routing, the
// authentication gate, CSRF checks, security headers, template rendering
// and static assets.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/kergeio/kerge-panel/internal/auth"
	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/i18n"
	"github.com/kergeio/kerge-panel/internal/metrics"
	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/setup"
	"github.com/kergeio/kerge-panel/internal/status"
)

// AgentConnections is what host management needs from the agent endpoint:
// closing the connection of a credential that no longer exists, and
// dropping what the panel kept in memory for a deleted host.
type AgentConnections interface {
	Revoke(agentID string)
	Forget(hostID int64)
}

// contentSecurityPolicy forbids inline scripts and any external source.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self'; font-src 'self'; connect-src 'self'; object-src 'none'; " +
	"base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// maxFormBytes limits request bodies of state-changing requests.
const maxFormBytes = 64 << 10

// Page template names, each parsed together with layout.html and
// partials.html.
var pages = []string{"home.html", "login.html", "setup.html", "settings.html",
	"host_form.html", "host_install.html", "host_confirm.html",
	"host_detail.html",
	"not_found.html", "error.html"}

// Options configures a Server.
type Options struct {
	// Files contains templates/ and static/ (see package github.com/kergeio/kerge-panel/web).
	Files    fs.FS
	Catalog  *i18n.Catalog
	Auth     *auth.Service
	CSRF     *auth.CSRF
	Setup    *setup.Service
	Hosts    *hosts.Service
	Metrics  *metrics.Service
	Settings *settings.Service
	Status   *status.Registry
	ClientIP *ClientIPResolver
	Logger   *slog.Logger
	// Agents acts on live agent connections when a host is deleted or
	// its access is reset. Nil means no connections are tracked.
	Agents AgentConnections
	// AgentEndpoint is the WebSocket address the install command points
	// an agent at, derived from the panel's public URL. Empty falls back
	// to the address each request arrived on, which is meant for local
	// development.
	AgentEndpoint string
	// Language returns the current interface language. Nil means
	// i18n.Default.
	Language func() string
	// DateFormat returns how dates are written, one of
	// i18n.DateFormats. Nil or anything else means the default.
	DateFormat func() string
	// TimeZone returns the panel's time zone, in which dates are shown.
	// Nil means UTC.
	TimeZone func() *time.Location
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// Health reports whether the panel can serve requests. Nil means
	// always healthy.
	Health func(context.Context) error
	// AgentWS serves the agent WebSocket endpoint. It authenticates with
	// the Authorization header rather than a session, so the gate treats
	// it as public, but it is closed while the panel is uninitialized.
	// Nil leaves the route unregistered.
	AgentWS http.Handler
}

type routeKind int

const (
	pageRoute routeKind = iota
	apiRoute
)

// route describes how the gate treats a ServeMux pattern. The zero value is
// a protected page.
type route struct {
	kind routeKind
	// public routes need no session.
	public bool
	// beforeSetup routes stay reachable while the panel is uninitialized.
	beforeSetup bool
	// setupOnly routes exist only while the panel is uninitialized.
	setupOnly bool
}

// Server is the panel's root HTTP handler.
type Server struct {
	opts         Options
	mux          *http.ServeMux
	routes       map[string]route                         // by ServeMux pattern
	tmpl         map[string]map[string]*template.Template // lang -> page -> template
	assets       map[string]string                        // static file name -> content hash
	setupLimiter *auth.Limiter
	handler      http.Handler
}

// New builds the server, parsing all templates up front.
func New(opts Options) (*Server, error) {
	if opts.Files == nil || opts.Catalog == nil || opts.Auth == nil || opts.CSRF == nil ||
		opts.Setup == nil || opts.Hosts == nil || opts.Metrics == nil ||
		opts.Settings == nil || opts.Status == nil ||
		opts.ClientIP == nil || opts.Logger == nil {
		return nil, errors.New("web: incomplete options")
	}
	static, err := fs.Sub(opts.Files, "static")
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts:         opts,
		mux:          http.NewServeMux(),
		routes:       make(map[string]route),
		setupLimiter: auth.NewLimiter(),
	}
	if s.assets, err = hashAssets(static); err != nil {
		return nil, err
	}
	if s.tmpl, err = s.parseTemplates(); err != nil {
		return nil, err
	}

	always := route{public: true, beforeSetup: true}
	s.handle("GET /static/", always, http.StripPrefix("/static", staticHandler(static)))
	s.handle("GET /healthz", always, http.HandlerFunc(s.healthz))
	s.handle("GET /favicon.ico", always, faviconHandler(static))
	s.handle("GET /setup", route{setupOnly: true}, http.HandlerFunc(s.setupPage))
	s.handle("POST /setup", route{setupOnly: true}, http.HandlerFunc(s.setupSubmit))
	s.handle("GET /login", route{public: true}, http.HandlerFunc(s.loginPage))
	s.handle("POST /login", route{public: true}, http.HandlerFunc(s.loginSubmit))
	s.handle("POST /logout", route{}, http.HandlerFunc(s.logout))
	s.handle("GET /settings", route{}, http.HandlerFunc(s.settingsPage))
	s.handle("POST /settings/password", route{}, http.HandlerFunc(s.changePassword))
	s.handle("POST /settings/display", route{}, http.HandlerFunc(s.saveDisplay))
	s.handle("POST /settings/data", route{}, http.HandlerFunc(s.saveData))
	s.handle("GET /{$}", route{}, http.HandlerFunc(s.homePage))
	s.handle("GET /api/live", route{kind: apiRoute}, http.HandlerFunc(s.liveWS))
	s.handle("GET /hosts/new", route{}, http.HandlerFunc(s.hostNewPage))
	s.handle("POST /hosts/new", route{}, http.HandlerFunc(s.hostCreate))
	s.handle("GET /hosts/{id}", route{}, http.HandlerFunc(s.hostDetailPage))
	s.handle("GET /api/hosts/{id}/metrics", route{kind: apiRoute}, http.HandlerFunc(s.hostMetrics))
	s.handle("GET /hosts/{id}/edit", route{}, http.HandlerFunc(s.hostEditPage))
	s.handle("POST /hosts/{id}/edit", route{}, http.HandlerFunc(s.hostUpdate))
	s.handle("GET /hosts/{id}/delete", route{}, http.HandlerFunc(s.hostDeletePage))
	s.handle("POST /hosts/{id}/delete", route{}, http.HandlerFunc(s.hostDelete))
	s.handle("GET /hosts/{id}/reset", route{}, http.HandlerFunc(s.hostResetPage))
	s.handle("POST /hosts/{id}/reset", route{}, http.HandlerFunc(s.hostReset))
	s.handle("POST /api/hosts/{id}/reminder/ack", route{kind: apiRoute}, http.HandlerFunc(s.reminderAck))
	s.handle("POST /api/hosts/{id}/reminder/snooze", route{kind: apiRoute}, http.HandlerFunc(s.reminderSnooze))
	s.handle("POST /api/settings/reveal", route{kind: apiRoute}, http.HandlerFunc(s.saveReveal))
	s.handle("POST /api/settings/view", route{kind: apiRoute}, http.HandlerFunc(s.saveView))
	if opts.AgentWS != nil {
		s.handle("GET /api/agent/ws", route{kind: apiRoute, public: true}, opts.AgentWS)
	}
	s.handle("/api/", route{kind: apiRoute}, http.HandlerFunc(s.apiNotFound))
	s.handle("/", route{}, http.HandlerFunc(s.pageNotFound))

	var h http.Handler = s.gate(s.mux)
	h = http.NewCrossOriginProtection().Handler(h)
	h = s.withClientIP(h)
	s.handler = s.securityHeaders(h)
	return s, nil
}

// handle registers h under pattern. Routes are protected unless marked
// public, so a route cannot become public by omission.
func (s *Server) handle(pattern string, r route, h http.Handler) {
	s.routes[pattern] = r
	s.mux.Handle(pattern, h)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Context values.

type ctxKey int

const (
	identityKey ctxKey = iota
	clientIPKey
)

// IdentityFrom returns the authenticated identity stored by the gate.
func IdentityFrom(ctx context.Context) (auth.Identity, bool) {
	id, ok := ctx.Value(identityKey).(auth.Identity)
	return id, ok
}

// ClientIPFrom returns the client address resolved for the request.
func ClientIPFrom(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(clientIPKey).(netip.Addr)
	return a
}

// Middleware.

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.opts.ClientIP.ClientIP(r)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey, ip)))
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// gate applies, in order: the uninitialized state (only setup, static
// files and the health check are reachable), the default-deny session
// check, and the CSRF token check for state-changing requests. The route
// is looked up the way the mux will dispatch, so unknown paths are
// protected as well.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safeMethod(r.Method) {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		_, pattern := s.mux.Handler(r)
		rt := s.routes[pattern]

		if !s.opts.Setup.Initialized() {
			switch {
			case rt.beforeSetup || rt.setupOnly:
				next.ServeHTTP(w, r)
			case rt.kind == apiRoute:
				s.apiError(w, http.StatusServiceUnavailable, msgKey("error.not_initialized"))
			default:
				http.Redirect(w, r, "/setup", http.StatusFound)
			}
			return
		}
		if rt.setupOnly {
			s.pageNotFound(w, r)
			return
		}
		if rt.public {
			next.ServeHTTP(w, r)
			return
		}

		id, ok := s.opts.Auth.Authenticate(r)
		if !ok || id.Role != auth.RoleAdmin {
			if rt.kind == apiRoute {
				s.apiError(w, http.StatusUnauthorized, msgKey("error.unauthorized"))
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), identityKey, id))
		if !safeMethod(r.Method) {
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("csrf_token")
			}
			if !s.opts.CSRF.CheckSession(id.SessionHash, token) {
				s.forbidden(w, r, rt.kind)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// forbidden reports a failed CSRF check.
func (s *Server) forbidden(w http.ResponseWriter, r *http.Request, kind routeKind) {
	if kind == apiRoute {
		s.apiError(w, http.StatusForbidden, msgKey("error.csrf"))
		return
	}
	s.render(w, r, http.StatusForbidden, "error.html", errorPage{Message: msgKey("error.csrf")})
}

// apiError writes a JSON error body with a translated message.
func (s *Server) apiError(w http.ResponseWriter, status int, key msgKey) {
	body, err := json.Marshal(struct {
		Error string `json:"error"`
	}{s.translator().T(string(key))})
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Rendering.

// pageData is the root value passed to every template.
type pageData struct {
	// ClientText holds the "js." translation keys for front-end scripts.
	ClientText map[string]string
	// CSRF is the token for forms and scripts on this page.
	CSRF string
	// User is the signed-in user, or nil.
	User *auth.Identity
	// Reminders are the due renewal reminders the dialog asks about on
	// every signed-in page.
	Reminders []dueReminder
	// RevealIPs is the state of the navigation bar's switch that shows
	// or masks host addresses.
	RevealIPs bool
	Data      any
}

// msgKey is a translation key selected in Go code and rendered with the
// "msg" template function. Create values only by converting a string
// literal, msgKey("some.key"): a test checks every such key exists.
type msgKey string

// errorPage is the data of error.html.
type errorPage struct{ Message msgKey }

func (s *Server) language() string {
	if s.opts.Language == nil {
		return i18n.Default
	}
	return s.opts.Language()
}

func (s *Server) translator() *i18n.Translator {
	return s.opts.Catalog.Translator(s.language()).WithDateFormat(s.dateFormat())
}

// dateFormat is the layout dates are written in.
func (s *Server) dateFormat() string {
	if s.opts.DateFormat == nil {
		return i18n.DefaultDateFormat
	}
	if f := s.opts.DateFormat(); i18n.ValidDateFormat(f) {
		return f
	}
	return i18n.DefaultDateFormat
}

// location is the time zone dates are shown in.
func (s *Server) location() *time.Location {
	if s.opts.TimeZone == nil {
		return time.UTC
	}
	if loc := s.opts.TimeZone(); loc != nil {
		return loc
	}
	return time.UTC
}

// render executes a page into a buffer first so a template error never
// produces a partial response. Signed-in pages get the session's CSRF
// token; anonymous pages get a pre-session token.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	tr := s.translator()
	pd := pageData{ClientText: tr.WithPrefix("js."), Data: data}
	if id, ok := IdentityFrom(r.Context()); ok {
		pd.User = &id
		pd.CSRF = s.opts.CSRF.SessionToken(id.SessionHash)
		pd.Reminders = s.dueReminders(r.Context())
		pd.RevealIPs = s.revealIPs(r.Context())
	} else {
		tok, err := s.opts.CSRF.PreSessionToken(w, r)
		if err != nil {
			s.opts.Logger.Error("issue csrf token", "err", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		pd.CSRF = tok
	}

	var buf bytes.Buffer
	err := errors.New("unknown page")
	if tpl := s.tmpl[tr.Lang()][page]; tpl != nil {
		err = tpl.ExecuteTemplate(&buf, "layout", pd)
	}
	if err != nil {
		s.opts.Logger.Error("render page", "page", page, "err", err)
		if page != "error.html" {
			s.render(w, r, http.StatusInternalServerError, "error.html", errorPage{Message: msgKey("error.internal")})
			return
		}
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
