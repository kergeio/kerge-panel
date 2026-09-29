// Package auth implements users, password hashing, sessions, CSRF tokens and
// login rate limiting for the panel.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kergeio/kerge-panel/internal/store"
)

// Role is a user's authorization level.
type Role string

// RoleAdmin is the only role in v1; every protected route requires it.
const RoleAdmin Role = "admin"

// SessionTTL is the fixed lifetime of a session from login.
const SessionTTL = 30 * 24 * time.Hour

// SessionCookie is the session cookie name. The __Host- prefix requires
// Secure, Path=/ and no Domain attribute.
const SessionCookie = "__Host-kerge_session"

const maxUserAgentLen = 256

var (
	// ErrInvalidCredentials means the username or password is wrong.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrWrongPassword means the current password given for a password
	// change is wrong.
	ErrWrongPassword = errors.New("auth: wrong current password")
)

// RateLimitedError is returned while a client or username is backing off.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("auth: rate limited, retry after %s", e.RetryAfter)
}

// Identity is the authenticated user of a request.
type Identity struct {
	UserID   int64
	Username string
	Role     Role
	// SessionHash identifies the session (the hash of its id, never the id
	// itself).
	SessionHash string
}

// Service provides authentication backed by the store.
type Service struct {
	db      *store.DB
	limiter *Limiter
	now     func() time.Time
}

// NewService returns an auth service.
func NewService(db *store.DB) *Service {
	return &Service{db: db, limiter: NewLimiter(), now: time.Now}
}

// hashToken returns the hex SHA-256 of a session id. Session ids are 32
// random bytes, so a fast hash is sufficient.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Authenticate resolves the session cookie of r to an identity.
func (s *Service) Authenticate(r *http.Request) (Identity, bool) {
	ck, err := r.Cookie(SessionCookie)
	if err != nil || ck.Value == "" {
		return Identity{}, false
	}
	h := hashToken(ck.Value)
	var id Identity
	err = s.db.Read().QueryRowContext(r.Context(), `
		SELECT u.id, u.username, u.role
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = ? AND s.expires_at > ?`,
		h, s.now().Unix()).Scan(&id.UserID, &id.Username, &id.Role)
	if err != nil {
		return Identity{}, false
	}
	id.SessionHash = h
	return id, true
}

// Login verifies credentials and creates a session. It returns the session
// id to put in the cookie.
func (s *Service) Login(ctx context.Context, username, password string, ip netip.Addr, userAgent string) (string, error) {
	ipKey, userKey := "ip:"+ip.String(), "user:"+username
	if d := max(s.limiter.Blocked(ipKey), s.limiter.Blocked(userKey)); d > 0 {
		return "", &RateLimitedError{RetryAfter: d}
	}

	var userID int64
	var hash string
	err := s.db.Read().QueryRowContext(ctx,
		"SELECT id, password_hash FROM users WHERE username = ?", username).Scan(&userID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		_, _, _ = VerifyPassword(password, dummyHash())
		s.limiter.Fail(ipKey)
		s.limiter.Fail(userKey)
		return "", ErrInvalidCredentials
	}
	if err != nil {
		return "", err
	}
	ok, rehash, err := VerifyPassword(password, hash)
	if err != nil {
		return "", err
	}
	if !ok {
		s.limiter.Fail(ipKey)
		s.limiter.Fail(userKey)
		return "", ErrInvalidCredentials
	}
	s.limiter.Reset(ipKey)
	s.limiter.Reset(userKey)

	var newHash string
	if rehash {
		if newHash, err = HashPassword(password); err != nil {
			return "", err
		}
	}
	var token string
	err = s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.now().Unix()
		if newHash != "" {
			if _, err := tx.ExecContext(ctx,
				"UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?", newHash, now, userID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE users SET last_login_at = ? WHERE id = ?", now, userID); err != nil {
			return err
		}
		var err error
		token, err = s.CreateSession(ctx, tx, userID, ip, userAgent)
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// CreateSession inserts a new session for userID inside tx and returns its
// id. Expired sessions are purged at the same time.
func (s *Service) CreateSession(ctx context.Context, tx *sql.Tx, userID int64, ip netip.Addr, userAgent string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", now.Unix()); err != nil {
		return "", err
	}
	ipText := ""
	if ip.IsValid() {
		ipText = ip.String()
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id_hash, user_id, created_at, expires_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(token), userID, now.Unix(), now.Add(SessionTTL).Unix(), ipText, truncate(userAgent, maxUserAgentLen))
	if err != nil {
		return "", err
	}
	return token, nil
}

// Logout deletes the session.
func (s *Service) Logout(ctx context.Context, sessionHash string) error {
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE id_hash = ?", sessionHash)
		return err
	})
}

// ChangePassword verifies the current password, stores the new one and
// deletes every other session of the user.
func (s *Service) ChangePassword(ctx context.Context, id Identity, current, next string) error {
	userKey := "user:" + id.Username
	if d := s.limiter.Blocked(userKey); d > 0 {
		return &RateLimitedError{RetryAfter: d}
	}
	var hash string
	if err := s.db.Read().QueryRowContext(ctx,
		"SELECT password_hash FROM users WHERE id = ?", id.UserID).Scan(&hash); err != nil {
		return err
	}
	ok, _, err := VerifyPassword(current, hash)
	if err != nil {
		return err
	}
	if !ok {
		s.limiter.Fail(userKey)
		return ErrWrongPassword
	}
	s.limiter.Reset(userKey)
	newHash, err := HashPassword(next)
	if err != nil {
		return err
	}
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?", newHash, s.now().Unix(), id.UserID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?", id.UserID, id.SessionHash)
		return err
	})
}

// SetSessionCookie writes the session cookie.
func SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie removes the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// ResetAdmin deletes all users and, through the foreign key, their
// sessions, plus any pending setup code. Hosts and metrics are kept.
func ResetAdmin(ctx context.Context, db *store.DB) error {
	return db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM users"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM setup_state")
		return err
	})
}

// truncate shortens s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.ToValidUTF8(s, "")
}
