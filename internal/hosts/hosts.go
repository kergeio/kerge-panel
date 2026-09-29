// Package hosts owns the host records, the one-time enrollment tokens and
// the long-lived agent credentials.
//
// Tokens and secrets are stored only as hashes and compared in constant
// time. Neither is ever logged.
package hosts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-protocol"
)

// TokenTTL is how long a one-time enrollment token stays valid.
const TokenTTL = 24 * time.Hour

// Credential sizes in bytes. They are encoded with base64url without
// padding, which keeps them free of the dot that separates an agent id from
// its secret and safe to paste into a shell command.
const (
	agentIDBytes = 16
	secretBytes  = 32
	tokenBytes   = 32
)

// ErrUnauthorized covers every authentication failure: an unknown, used or
// expired token, an unknown agent id and a wrong secret all look the same
// from outside.
var ErrUnauthorized = errors.New("hosts: unauthorized")

// Service reads and writes host records.
type Service struct {
	db  *store.DB
	now func() time.Time
}

// NewService returns a host service.
func NewService(db *store.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// Auth is the outcome of authenticating an agent connection.
type Auth struct {
	HostID  int64
	AgentID string
	// Registered is set when this connection enrolled and the panel still
	// has to hand the credential back.
	Registered *protocol.Registered
}

// Create adds a host and returns its id.
func (s *Service) Create(ctx context.Context, name string) (int64, error) {
	name, err := CleanName(name)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO hosts (name, created_at) VALUES (?, ?)", name, s.now().Unix())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// NewEnrollToken issues a one-time token for a host and returns it. The
// token is only ever returned here; the database keeps its hash.
func (s *Service) NewEnrollToken(ctx context.Context, hostID int64) (string, error) {
	token, err := randomValue(tokenBytes)
	if err != nil {
		return "", err
	}
	err = s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM hosts WHERE id = ?", hostID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("hosts: no host with id %d", hostID)
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO enroll_tokens (token_hash, host_id, expires_at) VALUES (?, ?, ?)",
			hashValue(token), hostID, s.now().Add(TokenTTL).Unix())
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Authenticate resolves an Authorization header to a host, enrolling the
// agent when it presents a one-time token.
func (s *Service) Authenticate(ctx context.Context, header string) (Auth, error) {
	kind, value, err := protocol.ParseAuthorization(header)
	if err != nil {
		return Auth{}, ErrUnauthorized
	}
	switch kind {
	case protocol.KindEnroll:
		return s.enroll(ctx, value)
	case protocol.KindAgent:
		return s.byCredential(ctx, value)
	default:
		return Auth{}, ErrUnauthorized
	}
}

// enroll consumes a one-time token and issues the long-lived credential.
// Everything happens in one transaction so that a token cannot be used
// twice by two connections racing each other.
func (s *Service) enroll(ctx context.Context, token string) (Auth, error) {
	agentID, err := randomValue(agentIDBytes)
	if err != nil {
		return Auth{}, err
	}
	secret, err := randomValue(secretBytes)
	if err != nil {
		return Auth{}, err
	}
	var hostID int64
	err = s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.now().Unix()
		var usedAt sql.NullInt64
		var expiresAt int64
		err := tx.QueryRowContext(ctx,
			"SELECT host_id, expires_at, used_at FROM enroll_tokens WHERE token_hash = ?",
			hashValue(token)).Scan(&hostID, &expiresAt, &usedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if usedAt.Valid || expiresAt <= now {
			return ErrUnauthorized
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE enroll_tokens SET used_at = ? WHERE token_hash = ?", now, hashValue(token)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE hosts SET agent_id = ?, secret_hash = ? WHERE id = ?",
			agentID, hashValue(secret), hostID)
		return err
	})
	if err != nil {
		return Auth{}, err
	}
	return Auth{
		HostID:     hostID,
		AgentID:    agentID,
		Registered: &protocol.Registered{Type: protocol.TypeRegistered, AgentID: agentID, Secret: secret},
	}, nil
}

// byCredential checks a long-lived credential.
func (s *Service) byCredential(ctx context.Context, credential string) (Auth, error) {
	agentID, secret, ok := strings.Cut(credential, ".")
	if !ok || agentID == "" || secret == "" {
		return Auth{}, ErrUnauthorized
	}
	var hostID int64
	var stored string
	err := s.db.Read().QueryRowContext(ctx,
		"SELECT id, secret_hash FROM hosts WHERE agent_id = ? AND secret_hash IS NOT NULL",
		agentID).Scan(&hostID, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return Auth{}, ErrUnauthorized
	}
	if err != nil {
		return Auth{}, err
	}
	if subtle.ConstantTimeCompare([]byte(hashValue(secret)), []byte(stored)) != 1 {
		return Auth{}, ErrUnauthorized
	}
	return Auth{HostID: hostID, AgentID: agentID}, nil
}

// randomValue returns n random bytes in base64url without padding.
func randomValue(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashValue returns the hex SHA-256 of a token or secret. Both are long
// random values, so a fast hash is sufficient.
func hashValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}
