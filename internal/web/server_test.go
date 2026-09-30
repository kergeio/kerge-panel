package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"text/template/parse"

	"github.com/kergeio/kerge-panel/internal/auth"
	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/i18n"
	"github.com/kergeio/kerge-panel/internal/metrics"
	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/setup"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/store"
	webfiles "github.com/kergeio/kerge-panel/web"
)

const secret = "TOP-SECRET-SENTINEL"

// testFiles is the real web tree plus files outside static/ that must never
// be served.
func testFiles(t *testing.T) fs.FS {
	t.Helper()
	m := fstest.MapFS{
		"secret.txt":           {Data: []byte(secret)},
		"kerge.db":             {Data: []byte(secret)},
		"templates/secret.txt": {Data: []byte(secret)},
	}
	err := fs.WalkDir(webfiles.Files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(webfiles.Files, name)
		m[name] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func realCatalog(t *testing.T) *i18n.Catalog {
	t.Helper()
	locales, err := fs.Sub(webfiles.Files, "locales")
	if err != nil {
		t.Fatal(err)
	}
	c, err := i18n.Load(locales, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// env is a panel wired to a fresh database.
type env struct {
	t       *testing.T
	srv     *Server
	db      *store.DB
	auth    *auth.Service
	csrf    *auth.CSRF
	setup   *setup.Service
	hosts   *hosts.Service
	metrics *metrics.Service
	status  *status.Registry
	// agents records what host management asked of the agent endpoint.
	agents   *fakeAgents
	settings *settings.Service
	code     string // setup code while uninitialized
}

// fakeAgents stands in for the agent endpoint's connection registry.
type fakeAgents struct {
	revoked   []string
	forgotten []int64
}

func (f *fakeAgents) Revoke(agentID string) { f.revoked = append(f.revoked, agentID) }
func (f *fakeAgents) Forget(hostID int64)   { f.forgotten = append(f.forgotten, hostID) }

func newEnv(t *testing.T, cat *i18n.Catalog) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "kerge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	csrf, err := auth.LoadCSRF(dir)
	if err != nil {
		t.Fatal(err)
	}
	setupSvc, code, err := setup.Start(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if cat == nil {
		cat = realCatalog(t)
	}
	e := &env{
		t: t, db: db, auth: auth.NewService(db), csrf: csrf, setup: setupSvc,
		hosts: hosts.NewService(db), metrics: metrics.NewService(db),
		settings: settings.NewService(db),
		status:   status.New(status.Options{}),
		agents:   &fakeAgents{}, code: code,
	}
	logger := slog.New(slog.DiscardHandler)
	e.srv, err = New(Options{
		Files:    testFiles(t),
		Catalog:  cat,
		Auth:     e.auth,
		CSRF:     csrf,
		Setup:    setupSvc,
		Hosts:    e.hosts,
		Metrics:  e.metrics,
		Settings: e.settings,
		Status:   e.status,
		Agents:   e.agents,
		ClientIP: NewClientIPResolver(nil, logger),
		Logger:   logger,
		Health:   func(ctx context.Context) error { return db.Read().PingContext(ctx) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const adminPassword = "correct horse battery"

// initialize completes setup directly and returns a session id for admin.
func (e *env) initialize() string {
	e.t.Helper()
	hash, err := auth.HashPassword(adminPassword)
	if err != nil {
		e.t.Fatal(err)
	}
	var tok string
	err = e.setup.Complete(e.t.Context(), setup.Params{
		Code: e.code, Username: "admin", PasswordHash: hash, Language: "en", TimeZone: "UTC",
	}, func(ctx context.Context, tx *sql.Tx, uid int64) error {
		var err error
		tok, err = e.auth.CreateSession(ctx, tx, uid, testIP(), "test")
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func testIP() netip.Addr { return netip.MustParseAddr("192.0.2.1") }

// do sends a request with an optional session and returns the recorder.
func (e *env) do(method, target, session string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

// sessionCSRF returns the CSRF token for a session id.
func (e *env) sessionCSRF(session string) string {
	id, ok := e.auth.Authenticate(sessionRequest(session))
	if !ok {
		e.t.Fatal("session not valid")
	}
	return e.csrf.SessionToken(id.SessionHash)
}

func sessionRequest(session string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
	return r
}

// browser is a TLS test server and a client with a cookie jar, so Secure
// cookies behave as in a real browser.
type browser struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
}

func (e *env) browser() *browser {
	srv := httptest.NewTLSServer(e.srv)
	e.t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	c := srv.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &browser{t: e.t, server: srv, client: c}
}

func (b *browser) get(path string) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.client.Get(b.server.URL + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.client.PostForm(b.server.URL+path, form)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

var csrfMeta = regexp.MustCompile(`<meta name="csrf-token" content="([^"]*)">`)

func csrfFrom(t *testing.T, page string) string {
	t.Helper()
	m := csrfMeta.FindStringSubmatch(page)
	if m == nil || m[1] == "" {
		t.Fatalf("no CSRF token in page:\n%s", page)
	}
	return html.UnescapeString(m[1])
}

// Uninitialized state.

func TestUninitializedGate(t *testing.T) {
	e := newEnv(t, nil)
	cases := []struct {
		method, target string
		status         int
		location       string
	}{
		{"GET", "/", http.StatusFound, "/setup"},
		{"GET", "/login", http.StatusFound, "/setup"},
		{"GET", "/settings", http.StatusFound, "/setup"},
		{"GET", "/no-such-page", http.StatusFound, "/setup"},
		{"POST", "/login", http.StatusFound, "/setup"},
		{"GET", "/api/hosts", http.StatusServiceUnavailable, ""},
		{"POST", "/api/hosts/1/reminder/ack", http.StatusServiceUnavailable, ""},
		{"POST", "/api/hosts/1/reminder/snooze", http.StatusServiceUnavailable, ""},
		{"POST", "/api/settings/reveal", http.StatusServiceUnavailable, ""},
		{"POST", "/api/settings/view", http.StatusServiceUnavailable, ""},
		{"GET", "/setup", http.StatusOK, ""},
		{"GET", "/healthz", http.StatusOK, ""},
		{"GET", "/favicon.ico", http.StatusOK, ""},
		{"GET", "/static/app.css", http.StatusOK, ""},
	}
	for _, c := range cases {
		rec := e.do(c.method, c.target, "", nil)
		if rec.Code != c.status || rec.Header().Get("Location") != c.location {
			t.Errorf("%s %s: %d %q, want %d %q", c.method, c.target, rec.Code, rec.Header().Get("Location"), c.status, c.location)
		}
		if c.status == http.StatusServiceUnavailable && !strings.Contains(rec.Body.String(), "not been set up") {
			t.Errorf("%s %s: body %q", c.method, c.target, rec.Body.String())
		}
	}
}

func setupValues(csrf, code string) url.Values {
	return url.Values{
		"csrf_token": {csrf}, "code": {code}, "username": {"admin"},
		"password": {adminPassword}, "password_confirm": {adminPassword},
		"language": {"en"}, "timezone": {"Europe/Berlin"},
	}
}

func TestSetupFlow(t *testing.T) {
	e := newEnv(t, nil)
	b := e.browser()

	resp, page := b.get("/setup")
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `name="code"`) {
		t.Fatalf("GET /setup: %d", resp.StatusCode)
	}
	token := csrfFrom(t, page)

	// Missing or foreign CSRF token.
	if resp, _ := b.post("/setup", setupValues("", e.code)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", resp.StatusCode)
	}
	if resp, _ := b.post("/setup", setupValues(e.csrf.SessionToken("x"), e.code)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong CSRF token: %d", resp.StatusCode)
	}
	// Wrong setup code.
	resp, page = b.post("/setup", setupValues(token, "AAAA-BBBB"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, "setup code is incorrect") {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	if e.setup.Initialized() {
		t.Fatal("initialized with a wrong code")
	}

	// Correct code: user created, signed in, redirected home.
	resp, _ = b.post("/setup", setupValues(token, strings.ToLower(e.code)))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("setup: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, page = b.get("/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Sign out") {
		t.Fatalf("home after setup: %d", resp.StatusCode)
	}
	if tz, _, _ := e.db.GetSetting(t.Context(), store.SettingTimeZone); tz != "Europe/Berlin" {
		t.Errorf("timezone = %q", tz)
	}

	// /setup is gone for good, with or without a session.
	if resp, _ := b.get("/setup"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /setup after init: %d", resp.StatusCode)
	}
	if resp, _ := b.post("/setup", setupValues(token, e.code)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /setup after init: %d", resp.StatusCode)
	}
	if rec := e.do("GET", "/setup", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("anonymous GET /setup after init: %d", rec.Code)
	}
}

func TestSetupValidation(t *testing.T) {
	e := newEnv(t, nil)
	b := e.browser()
	_, page := b.get("/setup")
	token := csrfFrom(t, page)

	cases := map[string]struct {
		field, value, message string
	}{
		"short username":    {"username", "ab", "username must be"},
		"bad username":      {"username", "a b c", "username must be"},
		"short password":    {"password", "short", "12 to 256"},
		"mismatch":          {"password_confirm", "something else entirely", "do not match"},
		"unknown language":  {"language", "xx", "Choose a language"},
		"unknown time zone": {"timezone", "Mars/Olympus", "Choose a time zone"},
		"legacy time zone":  {"timezone", "US/Eastern", "Choose a time zone"},
	}
	for name, c := range cases {
		form := setupValues(token, e.code)
		form.Set(c.field, c.value)
		if c.field == "password" {
			form.Set("password_confirm", c.value)
		}
		resp, page := b.post("/setup", form)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(html.UnescapeString(page), c.message) {
			t.Errorf("%s: status %d, message %q missing", name, resp.StatusCode, c.message)
		}
	}
	if e.setup.Initialized() {
		t.Error("initialized despite invalid input")
	}
}

func TestSetupCodeRateLimit(t *testing.T) {
	e := newEnv(t, nil)
	b := e.browser()
	_, page := b.get("/setup")
	token := csrfFrom(t, page)
	for range 6 {
		b.post("/setup", setupValues(token, "WRONG"))
	}
	resp, _ := b.post("/setup", setupValues(token, e.code))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("after 6 wrong codes: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// Initialized state.

func TestGateInitialized(t *testing.T) {
	e := newEnv(t, nil)
	sess := e.initialize()
	cases := []struct {
		method, target, session string
		status                  int
		location                string
	}{
		{"GET", "/", "", http.StatusFound, "/login"},
		{"GET", "/settings", "", http.StatusFound, "/login"},
		{"GET", "/no-such-page", "", http.StatusFound, "/login"},
		{"POST", "/logout", "", http.StatusFound, "/login"},
		{"GET", "/api/hosts", "", http.StatusUnauthorized, ""},
		{"POST", "/api/hosts/1/reminder/ack", "", http.StatusUnauthorized, ""},
		{"POST", "/api/hosts/1/reminder/snooze", "", http.StatusUnauthorized, ""},
		{"POST", "/api/settings/reveal", "", http.StatusUnauthorized, ""},
		{"POST", "/api/settings/view", "", http.StatusUnauthorized, ""},
		{"GET", "/", "bogus-session", http.StatusFound, "/login"},
		{"GET", "/login", "", http.StatusOK, ""},
		{"GET", "/healthz", "", http.StatusOK, ""},
		{"GET", "/favicon.ico", "", http.StatusOK, ""},
		{"GET", "/", sess, http.StatusOK, ""},
		{"GET", "/settings", sess, http.StatusOK, ""},
		{"GET", "/login", sess, http.StatusFound, "/"},
		{"GET", "/no-such-page", sess, http.StatusNotFound, ""},
		{"GET", "/api/no-such-endpoint", sess, http.StatusNotFound, ""},
	}
	for _, c := range cases {
		rec := e.do(c.method, c.target, c.session, nil)
		if rec.Code != c.status || rec.Header().Get("Location") != c.location {
			t.Errorf("%s %s (session %v): %d %q, want %d %q", c.method, c.target, c.session != "",
				rec.Code, rec.Header().Get("Location"), c.status, c.location)
		}
	}
}

func TestGateRequiresAdminRole(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	var tok string
	err := e.db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO users (username, password_hash, role, created_at, updated_at) VALUES ('viewer', 'x', 'viewer', 0, 0)")
		if err != nil {
			return err
		}
		uid, _ := res.LastInsertId()
		tok, err = e.auth.CreateSession(ctx, tx, uid, testIP(), "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.do("GET", "/", tok, nil); rec.Code != http.StatusFound {
		t.Errorf("viewer on /: %d", rec.Code)
	}
	if rec := e.do("GET", "/api/x", tok, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("viewer on /api/x: %d", rec.Code)
	}
}

func TestCSRFProtection(t *testing.T) {
	e := newEnv(t, nil)
	sess := e.initialize()
	good := e.sessionCSRF(sess)

	if rec := e.do("POST", "/logout", sess, url.Values{}); rec.Code != http.StatusForbidden {
		t.Errorf("no token: %d", rec.Code)
	}
	if rec := e.do("POST", "/logout", sess, url.Values{"csrf_token": {e.csrf.SessionToken("other")}}); rec.Code != http.StatusForbidden {
		t.Errorf("token of another session: %d", rec.Code)
	}
	if rec := e.do("POST", "/api/anything", sess, url.Values{}); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Errorf("API without token: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	// Header token (used by scripts) is accepted.
	req := httptest.NewRequest("POST", "/api/anything", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sess})
	req.Header.Set("X-CSRF-Token", good)
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("API with header token: %d, want 404 from the catch-all", rec.Code)
	}

	// Cross-site requests are rejected even with a valid token.
	req = httptest.NewRequest("POST", "/logout", strings.NewReader(url.Values{"csrf_token": {good}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sess})
	rec = httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: %d", rec.Code)
	}

	if rec := e.do("POST", "/logout", sess, url.Values{"csrf_token": {good}}); rec.Code != http.StatusSeeOther {
		t.Errorf("valid logout: %d", rec.Code)
	}
	if _, ok := e.auth.Authenticate(sessionRequest(sess)); ok {
		t.Error("session valid after logout")
	}
}

func TestLoginFlow(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := e.browser()

	_, page := b.get("/login")
	token := csrfFrom(t, page)
	form := func(pw string) url.Values {
		return url.Values{"csrf_token": {token}, "username": {"admin"}, "password": {pw}}
	}
	if resp, _ := b.post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("login without CSRF token: %d", resp.StatusCode)
	}
	resp, page := b.post("/login", form("wrong password!!"))
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "Invalid username or password") {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}
	resp, _ = b.post("/login", form(adminPassword))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookie {
			session = c
		}
	}
	if session == nil || !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v", session)
	}
	resp, page = b.get("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("home: %d", resp.StatusCode)
	}

	// Sign out through the form on the page.
	resp, _ = b.post("/logout", url.Values{"csrf_token": {csrfFrom(t, page)}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := b.get("/"); resp.StatusCode != http.StatusFound {
		t.Errorf("home after logout: %d", resp.StatusCode)
	}
}

func TestLoginRateLimited(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := e.browser()
	_, page := b.get("/login")
	token := csrfFrom(t, page)
	for range 6 {
		b.post("/login", url.Values{"csrf_token": {token}, "username": {"admin"}, "password": {"wrong password!!"}})
	}
	resp, page := b.post("/login", url.Values{"csrf_token": {token}, "username": {"admin"}, "password": {adminPassword}})
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" ||
		!strings.Contains(page, "Too many failed attempts") {
		t.Errorf("rate limited login: %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestChangePasswordPage(t *testing.T) {
	e := newEnv(t, nil)
	other := e.initialize()
	b := e.browser()
	_, page := b.get("/login")
	b.post("/login", url.Values{"csrf_token": {csrfFrom(t, page)}, "username": {"admin"}, "password": {adminPassword}})
	_, page = b.get("/settings")
	token := csrfFrom(t, page)

	form := func(cur, next, confirm string) url.Values {
		return url.Values{"csrf_token": {token}, "current_password": {cur}, "new_password": {next}, "new_password_confirm": {confirm}}
	}
	for name, c := range map[string]struct {
		form    url.Values
		status  int
		message string
	}{
		"short":         {form(adminPassword, "short", "short"), http.StatusUnprocessableEntity, "12 to 256"},
		"mismatch":      {form(adminPassword, "new password 123", "new password 456"), http.StatusUnprocessableEntity, "do not match"},
		"wrong current": {form("wrong password!!", "new password 123", "new password 123"), http.StatusUnprocessableEntity, "current password is incorrect"},
	} {
		resp, page := b.post("/settings/password", c.form)
		if resp.StatusCode != c.status || !strings.Contains(page, c.message) {
			t.Errorf("%s: %d, message %q missing", name, resp.StatusCode, c.message)
		}
	}
	resp, page := b.post("/settings/password", form(adminPassword, "new password 123", "new password 123"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Password changed") {
		t.Fatalf("change password: %d", resp.StatusCode)
	}
	if resp, _ := b.get("/settings"); resp.StatusCode != http.StatusOK {
		t.Errorf("current session signed out: %d", resp.StatusCode)
	}
	if rec := e.do("GET", "/", other, nil); rec.Code != http.StatusFound {
		t.Errorf("other session still valid: %d", rec.Code)
	}
}

// TestNoPlaintextSecretsInDatabase runs setup and a login, then checks that
// no table holds the password, session ids or the setup code in clear text.
func TestNoPlaintextSecretsInDatabase(t *testing.T) {
	e := newEnv(t, nil)
	b := e.browser()
	_, page := b.get("/setup")
	b.post("/setup", setupValues(csrfFrom(t, page), e.code))
	b2 := e.browser()
	_, page = b2.get("/login")
	resp, _ := b2.post("/login", url.Values{"csrf_token": {csrfFrom(t, page)}, "username": {"admin"}, "password": {adminPassword}})

	secrets := []string{adminPassword, setup.Normalize(e.code), e.code}
	for _, c := range resp.Cookies() {
		secrets = append(secrets, c.Value)
	}
	u, _ := url.Parse(b.server.URL)
	for _, c := range b.client.Jar.Cookies(u) {
		secrets = append(secrets, c.Value)
	}
	if len(secrets) < 4 {
		t.Fatalf("expected session cookies, got %d secrets", len(secrets))
	}

	rows, err := e.db.Read().QueryContext(t.Context(), "SELECT name FROM sqlite_schema WHERE type = 'table'")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, table := range tables {
		r, err := e.db.Read().QueryContext(t.Context(), "SELECT * FROM "+table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = r.Scan(ptrs...)
			for _, v := range vals {
				s := toString(v)
				for _, sec := range secrets {
					if sec != "" && strings.Contains(s, sec) {
						t.Errorf("table %s contains a plaintext secret", table)
					}
				}
			}
		}
		r.Close()
	}
}

func toString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

func TestHealthz(t *testing.T) {
	e := newEnv(t, nil)
	if rec := e.do("GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Errorf("healthy: %d", rec.Code)
	}
	e.db.Close()
	if rec := e.do("GET", "/healthz", "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("closed database: %d", rec.Code)
	}
}

// Headers, static files and templates.

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, nil)
	sess := e.initialize()
	for _, target := range []string{"/", "/login", "/healthz", "/static/app.css", "/api/x", "/nope", "/setup"} {
		for _, session := range []string{"", sess} {
			h := e.do("GET", target, session, nil).Header()
			csp := h.Get("Content-Security-Policy")
			if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") ||
				!strings.Contains(csp, "frame-ancestors 'none'") {
				t.Errorf("GET %s: CSP %q", target, csp)
			}
			if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" ||
				h.Get("Referrer-Policy") != "no-referrer" {
				t.Errorf("GET %s: missing security headers: %v", target, h)
			}
		}
	}
}

func TestPathTraversal(t *testing.T) {
	e := newEnv(t, nil)
	sess := e.initialize()
	srv := httptest.NewServer(e.srv)
	defer srv.Close()

	targets := []string{
		"/static/../secret.txt",
		"/static/../kerge.db",
		"/static/%2e%2e/secret.txt",
		"/static/%2E%2E/secret.txt",
		"/static/%2e%2e%2fsecret.txt",
		"/static/..%2fsecret.txt",
		"/static/..%2Fsecret.txt",
		"/static/%252e%252e/secret.txt",
		"/static/%252e%252e%252fsecret.txt",
		"/static/..%252fsecret.txt",
		"/static..",
		"/static../secret.txt",
		"/static..%2fsecret.txt",
		"/static//../secret.txt",
		"/static/./../secret.txt",
		"/static/app.css/../../secret.txt",
		"/static/..\\secret.txt",
		"/static/%5c..%5csecret.txt",
		"/static/../templates/secret.txt",
		"/static/%2e%2e/templates/layout.html",
		"/%2e%2e/secret.txt",
		"/secret.txt",
		"/kerge.db",
		"/templates/secret.txt",
		"/static/",
		"/static",
		"/static/.",
	}
	for _, session := range []string{sess, ""} {
		client := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return http.ErrUseLastResponse
				}
				if session != "" {
					req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
				}
				return nil
			},
		}
		for _, target := range targets {
			req, err := http.NewRequest("GET", srv.URL+target, nil)
			if err != nil {
				t.Fatalf("%s: %v", target, err)
			}
			if session != "" {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", target, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if strings.Contains(string(body), secret) || strings.Contains(string(body), "{{define") {
				t.Errorf("%s (signed in %v): leaked file content", target, session != "")
			}
			// A path with "." or ".." segments or doubled slashes gets 404
			// straight away, not a redirect to its cleaned form.
			if u, err := url.Parse(target); err == nil && !cleanPath(u.Path) {
				first := resp
				for first.Request.Response != nil {
					first = first.Request.Response
				}
				if first.StatusCode != http.StatusNotFound {
					t.Errorf("%s (signed in %v): first answer %d, want 404", target, session != "", first.StatusCode)
				}
			}
			final := resp.Request.URL.Path
			switch {
			case session == "" && final == "/login" && resp.StatusCode == http.StatusOK:
				// Anonymous requests end at the login page.
			case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusForbidden,
				resp.StatusCode == http.StatusBadRequest:
			default:
				t.Errorf("%s (signed in %v): final %s status %d, want 404/403", target, session != "", final, resp.StatusCode)
			}
		}
	}
}

func TestCleanPath(t *testing.T) {
	for _, p := range []string{"/", "/static/", "/static/app.css", "/hosts/1", "/api/hosts/1/metrics"} {
		if !cleanPath(p) {
			t.Errorf("cleanPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/static/../secret.txt", "/static/./app.css", "/static//app.css", "/static/.", "/..", "//", "/hosts/1/.."} {
		if cleanPath(p) {
			t.Errorf("cleanPath(%q) = true, want false", p)
		}
	}
}

func TestStaticAssets(t *testing.T) {
	e := newEnv(t, nil)
	for _, name := range []string{"app.css", "js/setup.js"} {
		u, err := e.srv.assetURL(name)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^/static/` + regexp.QuoteMeta(name) + `\?v=[0-9a-f]{16}$`).MatchString(u) {
			t.Errorf("assetURL = %q", u)
		}
		rec := e.do("GET", u, "", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("GET %s: %d %q", u, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	if _, err := e.srv.assetURL("missing.css"); err == nil {
		t.Error("assetURL of missing file succeeded")
	}
	rec := e.do("GET", "/static/app.css", "", nil)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("unversioned: %q %q", rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"))
	}
}

// Browsers ask for /favicon.ico on their own, before anyone signs in, and
// every page links both icons.
func TestFavicon(t *testing.T) {
	e := newEnv(t, nil)
	want, err := fs.ReadFile(webfiles.Files, "static/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do("GET", "/favicon.ico", "", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("GET /favicon.ico: %d, %d bytes, want the embedded %d bytes", rec.Code, rec.Body.Len(), len(want))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Errorf("Content-Type %q", ct)
	}

	svg, err := e.srv.assetURL("favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.do("GET", svg, "", nil); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/svg+xml") {
		t.Errorf("GET %s: %d %q", svg, rec.Code, rec.Header().Get("Content-Type"))
	}
	page := e.do("GET", "/setup", "", nil).Body.String()
	for _, link := range []string{`<link rel="icon" href="/favicon.ico"`, `<link rel="icon" href="` + svg + `" type="image/svg+xml">`} {
		if !strings.Contains(page, link) {
			t.Errorf("page lacks %s", link)
		}
	}
}

func TestClientTextIsEscapedJSON(t *testing.T) {
	en, err := fs.ReadFile(webfiles.Files, "locales/en.json")
	if err != nil {
		t.Fatal(err)
	}
	var msgs map[string]string
	if err := json.Unmarshal(en, &msgs); err != nil {
		t.Fatal(err)
	}
	msgs["js.evil"] = "</script><img src=x onerror=alert(1)>"
	data, _ := json.Marshal(msgs)
	cat, err := i18n.Load(fstest.MapFS{"en.json": {Data: data}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, cat)
	e.initialize()
	body := e.do("GET", "/login", "", nil).Body.String()
	if strings.Contains(body, "</script><img") {
		t.Fatal("client text was not escaped")
	}
	m := regexp.MustCompile(`(?s)<script type="application/json" id="i18n">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("i18n data block not found")
	}
	var text map[string]string
	if err := json.Unmarshal([]byte(m[1]), &text); err != nil {
		t.Fatalf("data block is not JSON: %v\n%s", err, m[1])
	}
	if text["js.evil"] != "</script><img src=x onerror=alert(1)>" {
		t.Errorf("client text = %v", text)
	}
}

// TestTranslationKeysExist checks that every translation key referenced by a
// template ("t" calls) or by Go code in this package (msgKey conversions)
// exists in en.json, and that both are only used with string literals so
// the check is complete.
func TestTranslationKeysExist(t *testing.T) {
	known := make(map[string]bool)
	for _, k := range realCatalog(t).Keys() {
		known[k] = true
	}
	used := 0
	check := func(where, key string) {
		used++
		if !known[key] {
			t.Errorf("%s: key %q is not in en.json", where, key)
		}
	}

	funcs := make(map[string]any, len(FuncNames))
	for _, n := range FuncNames {
		funcs[n] = true
	}
	// parse.Parse only knows the functions it is given, so the builtins a
	// template may use have to be listed as well.
	builtins := make(map[string]any)
	for _, n := range []string{"and", "call", "html", "index", "slice", "js", "len", "not",
		"or", "print", "printf", "println", "urlquery", "eq", "ge", "gt", "le", "lt", "ne"} {
		builtins[n] = true
	}
	names, err := fs.Glob(webfiles.Files, "templates/*.html")
	if err != nil || len(names) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, name := range names {
		src, err := fs.ReadFile(webfiles.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		trees, err := parse.Parse(name, string(src), "{{", "}}", funcs, builtins)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, tree := range trees {
			walk(tree.Root, func(cmd *parse.CommandNode) {
				id, ok := cmd.Args[0].(*parse.IdentifierNode)
				if !ok || id.Ident != "t" {
					return
				}
				if len(cmd.Args) != 2 {
					t.Errorf("%s: %s: t takes exactly one argument", name, cmd)
					return
				}
				lit, ok := cmd.Args[1].(*parse.StringNode)
				if !ok {
					t.Errorf("%s: %s: t argument must be a string literal", name, cmd)
					return
				}
				check(name, lit.Text)
			})
		}
	}

	fset := token.NewFileSet()
	goFiles, _ := filepath.Glob("*.go")
	for _, path := range goFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "msgKey" {
				return true
			}
			pos := fset.Position(call.Pos()).String()
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: msgKey must be converted from a string literal", pos)
				return true
			}
			key, _ := strconv.Unquote(lit.Value)
			check(pos, key)
			return true
		})
	}
	if used == 0 {
		t.Error("no translation keys found")
	}
}

// walk calls fn for every command node under n.
func walk(n parse.Node, fn func(*parse.CommandNode)) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			walk(c, fn)
		}
	case *parse.ActionNode:
		walk(n.Pipe, fn)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			walk(c, fn)
		}
	case *parse.CommandNode:
		fn(n)
		for _, a := range n.Args {
			walk(a, fn)
		}
	case *parse.IfNode:
		walk(n.Pipe, fn)
		walk(n.List, fn)
		walk(n.ElseList, fn)
	case *parse.RangeNode:
		walk(n.Pipe, fn)
		walk(n.List, fn)
		walk(n.ElseList, fn)
	case *parse.WithNode:
		walk(n.Pipe, fn)
		walk(n.List, fn)
		walk(n.ElseList, fn)
	case *parse.TemplateNode:
		walk(n.Pipe, fn)
	case *parse.ChainNode:
		walk(n.Node, fn)
	}
}

// TestNoInlineScripts checks that templates only contain script elements
// that load a file or carry a non-executable JSON data block, and no inline
// event handlers.
func TestNoInlineScripts(t *testing.T) {
	tag := regexp.MustCompile(`(?i)<script\b[^>]*>`)
	names, _ := fs.Glob(webfiles.Files, "templates/*.html")
	for _, name := range names {
		src, _ := fs.ReadFile(webfiles.Files, name)
		for _, m := range tag.FindAllString(string(src), -1) {
			if !strings.Contains(m, `type="application/json"`) && !strings.Contains(m, "src=") {
				t.Errorf("%s: inline script %s", name, m)
			}
		}
		if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).Match(src) {
			t.Errorf("%s: inline event handler attribute", name)
		}
	}
}
