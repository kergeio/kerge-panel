package setup

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kergeio/kerge-panel/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "kerge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var codeFormat = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){5}-[0-9A-HJKMNP-TV-Z]{2}$`)

func TestStartUninitialized(t *testing.T) {
	db := openDB(t)
	s, code, err := Start(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if s.Initialized() {
		t.Error("initialized with no users")
	}
	if !codeFormat.MatchString(code) {
		t.Errorf("code %q has wrong format", code)
	}
	ok, err := s.CheckCode(t.Context(), code)
	if err != nil || !ok {
		t.Fatalf("CheckCode(own code) = %v, %v", ok, err)
	}

	// A restart replaces the code; the old one stops working.
	s2, code2, err := Start(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if code2 == code {
		t.Error("code not regenerated")
	}
	if ok, _ := s2.CheckCode(t.Context(), code); ok {
		t.Error("old code still valid")
	}
	var n int
	_ = db.Read().QueryRowContext(t.Context(), "SELECT count(*) FROM setup_state").Scan(&n)
	if n != 1 {
		t.Errorf("setup_state rows = %d", n)
	}
}

func TestNormalize(t *testing.T) {
	db := openDB(t)
	s, code, _ := Start(t.Context(), db)
	variants := []string{
		code,
		strings.ToLower(code),
		strings.ReplaceAll(code, "-", ""),
		" " + strings.ReplaceAll(code, "-", " ") + " ",
		strings.NewReplacer("0", "O", "1", "l").Replace(code),
	}
	for _, v := range variants {
		if ok, _ := s.CheckCode(t.Context(), v); !ok {
			t.Errorf("variant %q rejected", v)
		}
	}
	for _, bad := range []string{"", code[:len(code)-1], code + "0", "0000-0000-0000-0000-0000-0000-00"} {
		if ok, _ := s.CheckCode(t.Context(), bad); ok && bad != code {
			t.Errorf("bad code %q accepted", bad)
		}
	}
}

func params(code string) Params {
	return Params{Code: code, Username: "admin", PasswordHash: "$argon2id$x", Language: "en", TimeZone: "Asia/Tokyo"}
}

func count(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	var n int
	if err := db.Read().QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func noSession(context.Context, *sql.Tx, int64) error { return nil }

func TestCompleteWrongCode(t *testing.T) {
	db := openDB(t)
	s, _, _ := Start(t.Context(), db)
	if err := s.Complete(t.Context(), params("0000-0000"), noSession); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("err = %v", err)
	}
	if s.Initialized() || count(t, db, "users") != 0 || count(t, db, "setup_state") != 1 {
		t.Error("state changed after wrong code")
	}
}

func TestComplete(t *testing.T) {
	db := openDB(t)
	s, code, _ := Start(t.Context(), db)
	var sessionUser int64
	err := s.Complete(t.Context(), params(code), func(ctx context.Context, tx *sql.Tx, userID int64) error {
		sessionUser = userID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Initialized() {
		t.Error("not initialized after setup")
	}
	var id int64
	var name, role string
	if err := db.Read().QueryRowContext(t.Context(), "SELECT id, username, role FROM users").Scan(&id, &name, &role); err != nil {
		t.Fatal(err)
	}
	if name != "admin" || role != "admin" || sessionUser != id {
		t.Errorf("user = %d %s %s, session user %d", id, name, role, sessionUser)
	}
	for key, want := range map[string]string{store.SettingLanguage: "en", store.SettingTimeZone: "Asia/Tokyo"} {
		if v, _, _ := db.GetSetting(t.Context(), key); v != want {
			t.Errorf("setting %s = %q, want %q", key, v, want)
		}
	}
	if count(t, db, "setup_state") != 0 {
		t.Error("setup code not invalidated")
	}
	if err := s.Complete(t.Context(), params(code), noSession); !errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("second setup: %v", err)
	}

	// After a restart the panel stays initialized and prints no code.
	s2, code2, err := Start(t.Context(), db)
	if err != nil || !s2.Initialized() || code2 != "" {
		t.Errorf("restart: initialized=%v code=%q err=%v", s2.Initialized(), code2, err)
	}
}

func TestCompleteRollsBackWhenSessionFails(t *testing.T) {
	db := openDB(t)
	s, code, _ := Start(t.Context(), db)
	boom := errors.New("boom")
	if err := s.Complete(t.Context(), params(code), func(context.Context, *sql.Tx, int64) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if s.Initialized() || count(t, db, "users") != 0 || count(t, db, "settings") != 0 || count(t, db, "setup_state") != 1 {
		t.Error("setup was not rolled back")
	}
}
