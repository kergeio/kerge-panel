package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

const (
	csrfKeyFile = "csrf.key"
	csrfKeyLen  = 32

	// preSessionCookie carries a random value that anonymous forms (login,
	// setup) are bound to (signed double-submit).
	preSessionCookie = "__Host-kerge_csrf"
)

// CSRF issues and checks anti-CSRF tokens. Tokens are HMACs under a key
// stored in the data directory, bound either to a session or, for
// anonymous forms, to a random pre-session cookie.
type CSRF struct {
	key []byte
}

// LoadCSRF reads the key from dataDir, creating it on first start.
func LoadCSRF(dataDir string) (*CSRF, error) {
	path := filepath.Join(dataDir, csrfKeyFile)
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key = make([]byte, csrfKeyLen)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("auth: create %s: %w", csrfKeyFile, err)
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, fmt.Errorf("auth: write %s: %w", csrfKeyFile, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("auth: write %s: %w", csrfKeyFile, err)
		}
		return &CSRF{key: key}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read %s: %w", csrfKeyFile, err)
	}
	if len(key) != csrfKeyLen {
		return nil, fmt.Errorf("auth: %s has length %d, want %d", csrfKeyFile, len(key), csrfKeyLen)
	}
	return &CSRF{key: key}, nil
}

func (c *CSRF) mac(domain, value string) string {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(domain))
	m.Write([]byte{0})
	m.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (c *CSRF) check(domain, value, token string) bool {
	if value == "" || token == "" {
		return false
	}
	return hmac.Equal([]byte(c.mac(domain, value)), []byte(token))
}

// SessionToken returns the CSRF token for the session with the given hash.
func (c *CSRF) SessionToken(sessionHash string) string {
	return c.mac("session", sessionHash)
}

// CheckSession reports whether token is valid for the session.
func (c *CSRF) CheckSession(sessionHash, token string) bool {
	return c.check("session", sessionHash, token)
}

// PreSessionToken returns a token for an anonymous form, setting the
// pre-session cookie if the request does not carry one.
func (c *CSRF) PreSessionToken(w http.ResponseWriter, r *http.Request) (string, error) {
	if ck, err := r.Cookie(preSessionCookie); err == nil && len(ck.Value) == 43 {
		return c.mac("pre", ck.Value), nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	v := base64.RawURLEncoding.EncodeToString(raw) // 43 characters
	http.SetCookie(w, &http.Cookie{
		Name:     preSessionCookie,
		Value:    v,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	return c.mac("pre", v), nil
}

// CheckPreSession reports whether token matches the request's pre-session
// cookie.
func (c *CSRF) CheckPreSession(r *http.Request, token string) bool {
	ck, err := r.Cookie(preSessionCookie)
	if err != nil {
		return false
	}
	return c.check("pre", ck.Value, token)
}
