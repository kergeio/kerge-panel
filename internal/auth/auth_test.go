package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/kergeio/kerge-panel/internal/store"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash = %q", h)
	}
	if ok, rehash, err := VerifyPassword("correct horse battery", h); !ok || rehash || err != nil {
		t.Errorf("verify correct: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	if ok, _, err := VerifyPassword("correct horse batterY", h); ok || err != nil {
		t.Errorf("verify wrong: ok=%v err=%v", ok, err)
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Error("two hashes of the same password are equal; salt not random")
	}
}

// legacyHash encodes a hash with weaker parameters, as an older build
// might have stored.
func legacyHash(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, 1, 8*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", 8*1024, 1, 1,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func TestVerifyFlagsOutdatedParams(t *testing.T) {
	ok, rehash, err := VerifyPassword("pw", legacyHash("pw"))
	if !ok || !rehash || err != nil {
		t.Errorf("legacy hash: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, h := range []string{
		"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AAAA$AAAA", "$argon2id$v=18$m=1,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=0,t=1,p=1$AAAA$AAAA", "$argon2id$v=19$m=1,t=1,p=1$!!$AAAA",
		"$argon2id$v=19$m=1,t=1,p=1$AAAA$", "$argon2id$v=19$m=1,t=1,p=1$AAAA$AAAA$extra",
	} {
		if _, _, err := VerifyPassword("x", h); err == nil {
			t.Errorf("VerifyPassword accepted %q", h)
		}
	}
}

func TestValidation(t *testing.T) {
	for name, want := range map[string]bool{
		"abc": true, "admin.ops_1-x": true, "ab": false, strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false, "has space": false, "café": false, "a/b": false, "": false,
	} {
		if ValidUsername(name) != want {
			t.Errorf("ValidUsername(%q) = %v", name, !want)
		}
	}
	for pw, want := range map[string]bool{
		strings.Repeat("x", 11): false, strings.Repeat("x", 12): true,
		strings.Repeat("é", 12):  true, // counted in characters, not bytes
		strings.Repeat("x", 256): true, strings.Repeat("x", 257): false, "\xff" + strings.Repeat("x", 12): false,
	} {
		if ValidPassword(pw) != want {
			t.Errorf("ValidPassword(len %d) = %v", len(pw), !want)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	for i := range 5 {
		l.Fail("k")
		if d := l.Blocked("k"); d != 0 {
			t.Fatalf("blocked after %d failures", i+1)
		}
	}
	l.Fail("k")
	if d := l.Blocked("k"); d != time.Second {
		t.Errorf("after 6 failures: %v, want 1s", d)
	}
	l.Fail("k")
	if d := l.Blocked("k"); d != 2*time.Second {
		t.Errorf("after 7 failures: %v, want 2s", d)
	}
	for range 40 {
		l.Fail("k")
	}
	if d := l.Blocked("k"); d != 15*time.Minute {
		t.Errorf("cap: %v, want 15m", d)
	}
	if d := l.Blocked("other"); d != 0 {
		t.Errorf("other key blocked: %v", d)
	}
	now = now.Add(15 * time.Minute)
	if d := l.Blocked("k"); d != 0 {
		t.Errorf("still blocked after backoff: %v", d)
	}
	l.Reset("k")
	l.Fail("k")
	if d := l.Blocked("k"); d != 0 {
		t.Errorf("blocked after reset: %v", d)
	}

	// Idle entries are garbage collected.
	now = now.Add(25 * time.Hour)
	l.Fail("fresh")
	if _, ok := l.entries["k"]; ok {
		t.Error("idle entry not collected")
	}
}

func TestCSRFKeyFile(t *testing.T) {
	dir := t.TempDir()
	c1, err := LoadCSRF(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, csrfKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", info.Mode().Perm())
	}
	c2, err := LoadCSRF(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c1.SessionToken("s") != c2.SessionToken("s") {
		t.Error("key changed on reload")
	}
	if err := os.WriteFile(filepath.Join(dir, csrfKeyFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCSRF(dir); err == nil {
		t.Error("short key accepted")
	}
}

func TestCSRFTokens(t *testing.T) {
	c, err := LoadCSRF(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tok := c.SessionToken("hash-a")
	if !c.CheckSession("hash-a", tok) || c.CheckSession("hash-b", tok) || c.CheckSession("hash-a", "") ||
		c.CheckSession("", c.SessionToken("")) {
		t.Error("session token checks wrong")
	}

	// Pre-session: the token is bound to the cookie it was issued with.
	rec := httptest.NewRecorder()
	pre, err := c.PreSessionToken(rec, httptest.NewRequest("GET", "/login", nil))
	if err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-kerge_csrf" || !cookies[0].Secure ||
		!cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
		t.Fatalf("pre-session cookie = %+v", cookies)
	}
	post := httptest.NewRequest("POST", "/login", nil)
	post.AddCookie(cookies[0])
	if !c.CheckPreSession(post, pre) {
		t.Error("valid pre-session token rejected")
	}
	if c.CheckPreSession(post, tok) || c.CheckPreSession(httptest.NewRequest("POST", "/", nil), pre) {
		t.Error("pre-session token accepted without matching cookie")
	}
	// An existing cookie is reused rather than replaced.
	rec2 := httptest.NewRecorder()
	get := httptest.NewRequest("GET", "/login", nil)
	get.AddCookie(cookies[0])
	pre2, _ := c.PreSessionToken(rec2, get)
	if pre2 != pre || len(rec2.Result().Cookies()) != 0 {
		t.Error("pre-session cookie was replaced")
	}
}

// service test fixtures

type fixture struct {
	db  *store.DB
	svc *Service
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "kerge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{db: db, now: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	f.svc = NewService(db)
	f.svc.now = func() time.Time { return f.now }
	f.svc.limiter.now = f.svc.now
	return f
}

func (f *fixture) addUser(t *testing.T, name, hash, role string) int64 {
	t.Helper()
	var id int64
	err := f.db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO users (username, password_hash, role, created_at, updated_at) VALUES (?, ?, ?, 0, 0)",
			name, hash, role)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func authReq(token string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if token != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	}
	return r
}

var testIP = netip.MustParseAddr("198.51.100.7")

const testPassword = "a long enough password"

func TestLoginAndAuthenticate(t *testing.T) {
	f := newFixture(t)
	hash, _ := HashPassword(testPassword)
	uid := f.addUser(t, "admin", hash, "admin")
	ctx := t.Context()

	if _, err := f.svc.Login(ctx, "admin", "wrong password!!", testIP, "ua"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := f.svc.Login(ctx, "nobody", testPassword, testIP, "ua"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	token, err := f.svc.Login(ctx, "admin", testPassword, testIP, strings.Repeat("u", 1000))
	if err != nil {
		t.Fatal(err)
	}
	id, ok := f.svc.Authenticate(authReq(token))
	if !ok || id.UserID != uid || id.Username != "admin" || id.Role != RoleAdmin || id.SessionHash != hashToken(token) {
		t.Fatalf("Authenticate = %+v %v", id, ok)
	}
	for _, bad := range []string{"", "not-a-session", token + "x"} {
		if _, ok := f.svc.Authenticate(authReq(bad)); ok {
			t.Errorf("Authenticate(%q) succeeded", bad)
		}
	}

	var ip, ua string
	var created, expires int64
	var lastLogin sql.NullInt64
	if err := f.db.Read().QueryRowContext(ctx,
		"SELECT ip, user_agent, created_at, expires_at FROM sessions").Scan(&ip, &ua, &created, &expires); err != nil {
		t.Fatal(err)
	}
	if ip != "198.51.100.7" || len(ua) != maxUserAgentLen || expires-created != int64(SessionTTL/time.Second) {
		t.Errorf("session row: ip=%q ua len=%d ttl=%d", ip, len(ua), expires-created)
	}
	_ = f.db.Read().QueryRowContext(ctx, "SELECT last_login_at FROM users").Scan(&lastLogin)
	if !lastLogin.Valid || lastLogin.Int64 != f.now.Unix() {
		t.Errorf("last_login_at = %v", lastLogin)
	}

	// The session expires 30 days after login, whether or not it is used.
	f.now = f.now.Add(SessionTTL - time.Second)
	if _, ok := f.svc.Authenticate(authReq(token)); !ok {
		t.Error("session expired early")
	}
	f.now = f.now.Add(time.Second)
	if _, ok := f.svc.Authenticate(authReq(token)); ok {
		t.Error("session valid after 30 days")
	}
}

func TestLoginRehashesOutdatedHash(t *testing.T) {
	f := newFixture(t)
	f.addUser(t, "admin", legacyHash(testPassword), "admin")
	if _, err := f.svc.Login(t.Context(), "admin", testPassword, testIP, ""); err != nil {
		t.Fatal(err)
	}
	var h string
	_ = f.db.Read().QueryRowContext(t.Context(), "SELECT password_hash FROM users").Scan(&h)
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash not upgraded: %q", h)
	}
	if ok, rehash, _ := VerifyPassword(testPassword, h); !ok || rehash {
		t.Error("upgraded hash does not verify")
	}
}

func TestLoginRateLimit(t *testing.T) {
	f := newFixture(t)
	hash, _ := HashPassword(testPassword)
	f.addUser(t, "admin", hash, "admin")
	ctx := t.Context()

	for i := range 6 {
		if _, err := f.svc.Login(ctx, "admin", "wrong password!!", testIP, ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	// Even the right password is refused while backing off.
	var rl *RateLimitedError
	if _, err := f.svc.Login(ctx, "admin", testPassword, testIP, ""); !errors.As(err, &rl) || rl.RetryAfter != time.Second {
		t.Fatalf("7th attempt: %v", err)
	}
	// The username is limited from another IP too.
	if _, err := f.svc.Login(ctx, "admin", testPassword, netip.MustParseAddr("203.0.113.9"), ""); !errors.As(err, &rl) {
		t.Fatalf("other IP, same user: %v", err)
	}
	// The IP is limited for other usernames too.
	if _, err := f.svc.Login(ctx, "someone", testPassword, testIP, ""); !errors.As(err, &rl) {
		t.Fatalf("same IP, other user: %v", err)
	}
	f.now = f.now.Add(time.Second)
	if _, err := f.svc.Login(ctx, "admin", testPassword, testIP, ""); err != nil {
		t.Fatalf("after backoff: %v", err)
	}
}

func TestChangePasswordSignsOutOtherSessions(t *testing.T) {
	f := newFixture(t)
	hash, _ := HashPassword(testPassword)
	f.addUser(t, "admin", hash, "admin")
	ctx := t.Context()
	a, _ := f.svc.Login(ctx, "admin", testPassword, testIP, "")
	b, _ := f.svc.Login(ctx, "admin", testPassword, testIP, "")
	idA, _ := f.svc.Authenticate(authReq(a))

	if err := f.svc.ChangePassword(ctx, idA, "wrong password!!", "new password 123"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong current password: %v", err)
	}
	if err := f.svc.ChangePassword(ctx, idA, testPassword, "new password 123"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.svc.Authenticate(authReq(a)); !ok {
		t.Error("current session was signed out")
	}
	if _, ok := f.svc.Authenticate(authReq(b)); ok {
		t.Error("other session still valid")
	}
	if _, err := f.svc.Login(ctx, "admin", testPassword, testIP, ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("old password still works: %v", err)
	}
	if _, err := f.svc.Login(ctx, "admin", "new password 123", testIP, ""); err != nil {
		t.Errorf("new password: %v", err)
	}
}

func TestLogout(t *testing.T) {
	f := newFixture(t)
	hash, _ := HashPassword(testPassword)
	f.addUser(t, "admin", hash, "admin")
	tok, _ := f.svc.Login(t.Context(), "admin", testPassword, testIP, "")
	id, _ := f.svc.Authenticate(authReq(tok))
	if err := f.svc.Logout(t.Context(), id.SessionHash); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.svc.Authenticate(authReq(tok)); ok {
		t.Error("session valid after logout")
	}
}

func TestResetAdminKeepsHosts(t *testing.T) {
	f := newFixture(t)
	hash, _ := HashPassword(testPassword)
	f.addUser(t, "admin", hash, "admin")
	tok, _ := f.svc.Login(t.Context(), "admin", testPassword, testIP, "")
	err := f.db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO hosts (name, created_at) VALUES ('web-1', 0)"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO setup_state (id, code_hash, created_at) VALUES (1, 'x', 0)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ResetAdmin(t.Context(), f.db); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"users": 0, "sessions": 0, "setup_state": 0, "hosts": 1} {
		var n int
		_ = f.db.Read().QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n)
		if n != want {
			t.Errorf("%s rows = %d, want %d", table, n, want)
		}
	}
	if _, ok := f.svc.Authenticate(authReq(tok)); ok {
		t.Error("session valid after reset")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "tok")
	c := rec.Result().Cookies()[0]
	if c.Name != "__Host-kerge_session" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode ||
		c.Path != "/" || c.Domain != "" || c.MaxAge != 30*24*3600 {
		t.Errorf("cookie = %+v", c)
	}
	rec = httptest.NewRecorder()
	ClearSessionCookie(rec)
	if c := rec.Result().Cookies()[0]; c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("clear cookie = %+v", c)
	}
}
