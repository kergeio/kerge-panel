// Package setup implements the first-run state: the one-time setup code and
// the transaction that creates the admin user.
package setup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kergeio/kerge-panel/internal/auth"
	"github.com/kergeio/kerge-panel/internal/store"
)

// crockford is the Crockford Base32 alphabet (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// codeLen is the number of symbols in a setup code: 26 x 5 bits = 130 bits.
const codeLen = 26

// ErrWrongCode means the setup code does not match.
var ErrWrongCode = errors.New("setup: wrong setup code")

// ErrAlreadyInitialized means a user already exists.
var ErrAlreadyInitialized = errors.New("setup: already initialized")

// Service tracks whether the panel has been initialized.
type Service struct {
	db          *store.DB
	initialized atomic.Bool
	now         func() time.Time
}

// Start determines the initialization state. If no user exists it creates
// a fresh setup code, replacing any previous one, and returns it for
// display; otherwise it returns "".
func Start(ctx context.Context, db *store.DB) (*Service, string, error) {
	s := &Service{db: db, now: time.Now}
	var n int
	if err := db.Read().QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
		return nil, "", err
	}
	if n > 0 {
		s.initialized.Store(true)
		return s, "", nil
	}
	code, err := newCode()
	if err != nil {
		return nil, "", err
	}
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO setup_state (id, code_hash, created_at) VALUES (1, ?, ?)
			ON CONFLICT (id) DO UPDATE SET code_hash = excluded.code_hash, created_at = excluded.created_at`,
			hashCode(code), s.now().Unix())
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return s, format(code), nil
}

// Initialized reports whether an admin user exists.
func (s *Service) Initialized() bool { return s.initialized.Load() }

// newCode returns codeLen random Crockford symbols. 256 is a multiple of 32,
// so taking each byte modulo 32 is unbiased.
func newCode() (string, error) {
	raw := make([]byte, codeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	b := make([]byte, codeLen)
	for i, v := range raw {
		b[i] = crockford[v%32]
	}
	return string(b), nil
}

// format groups a code in blocks of four: "ABCD-EFGH-...".
func format(code string) string {
	var b strings.Builder
	for i := 0; i < len(code); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(code[i:min(i+4, len(code))])
	}
	return b.String()
}

// Normalize converts user input to the canonical code: upper case, without
// separators or spaces, with the Crockford substitutions O->0 and I/L->1.
func Normalize(input string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(input) {
		switch c {
		case '-', ' ', '\t':
			continue
		case 'O':
			c = '0'
		case 'I', 'L':
			c = '1'
		}
		b.WriteRune(c)
	}
	return b.String()
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// CheckCode reports whether input matches the current setup code.
func (s *Service) CheckCode(ctx context.Context, input string) (bool, error) {
	var stored string
	err := s.db.Read().QueryRowContext(ctx, "SELECT code_hash FROM setup_state WHERE id = 1").Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(hashCode(Normalize(input))), []byte(stored)) == 1, nil
}

// Params are the validated values submitted to the setup wizard.
type Params struct {
	Code         string
	Username     string
	PasswordHash string
	Language     string
	TimeZone     string
}

// Complete creates the admin user, stores the language and time zone, and
// invalidates the setup code in one transaction. newSession runs in the same
// transaction so that setup and the automatic login are atomic.
func (s *Service) Complete(ctx context.Context, p Params, newSession func(ctx context.Context, tx *sql.Tx, userID int64) error) error {
	err := s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var users int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
			return err
		}
		if users > 0 {
			return ErrAlreadyInitialized
		}
		var stored string
		err := tx.QueryRowContext(ctx, "SELECT code_hash FROM setup_state WHERE id = 1").Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrWrongCode
		}
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(hashCode(Normalize(p.Code))), []byte(stored)) != 1 {
			return ErrWrongCode
		}
		now := s.now().Unix()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users (username, password_hash, role, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`,
			p.Username, p.PasswordHash, string(auth.RoleAdmin), now, now)
		if err != nil {
			return err
		}
		userID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if err := store.SetSetting(ctx, tx, store.SettingLanguage, p.Language); err != nil {
			return err
		}
		if err := store.SetSetting(ctx, tx, store.SettingTimeZone, p.TimeZone); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM setup_state"); err != nil {
			return err
		}
		return newSession(ctx, tx, userID)
	})
	if err == nil || errors.Is(err, ErrAlreadyInitialized) {
		s.initialized.Store(true)
	}
	return err
}
