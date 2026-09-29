package ingest

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kergeio/kerge-panel/internal/web"
	"github.com/kergeio/kerge-protocol"
)

// session is one agent connection. Only the connection's own goroutine
// writes to it: another goroutine that wants the connection closed records
// a reason and cancels, and the owner sends the error on its way out.
type session struct {
	conn         *websocket.Conn
	cancel       context.CancelFunc
	writeTimeout time.Duration

	mu      sync.Mutex
	code    string
	message string
}

// write sends one message under the write timeout.
func (s *session) write(ctx context.Context, msg any) error {
	data, err := protocol.Marshal(msg)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, s.writeTimeout)
	defer cancel()
	return s.conn.Write(writeCtx, websocket.MessageText, data)
}

// fail tells the agent why the connection is ending and closes it. It
// writes the error itself rather than leaving it to the owning goroutine,
// because cancelling a read closes the connection underneath it. Writes
// are serialized by the library, so calling this from another goroutine is
// safe.
func (s *session) fail(code, message string) {
	s.mu.Lock()
	first := s.code == ""
	if first {
		s.code, s.message = code, message
	}
	s.mu.Unlock()
	if first {
		ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
		//nolint:errcheck // the agent may already be gone; closing follows either way.
		_ = s.write(ctx, &protocol.ErrorMessage{Code: code, Message: message})
		cancel()
	}
	// CloseNow rather than a closing handshake: the peer may be a zombie
	// connection, and nothing here may wait on it.
	s.conn.CloseNow()
	s.cancel()
}

// finish closes a connection that ended without a reason to report.
func (s *session) finish() {
	s.mu.Lock()
	reported := s.code != ""
	s.mu.Unlock()
	if reported {
		return
	}
	s.conn.Close(websocket.StatusNormalClosure, "")
}

// clientIP returns the source address the panel trusts for this request.
func clientIP(r *http.Request) netip.Addr {
	return web.ClientIPFrom(r.Context())
}
