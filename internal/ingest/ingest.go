// Package ingest is the panel's agent endpoint: it authenticates agent
// connections, validates what they report, computes network rates and
// writes samples to the store.
//
// Everything an agent sends is untrusted input. The panel never sends a
// command: the only messages out are the registration result and an error
// that ends the connection.
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-protocol"
	"github.com/kergeio/kerge-protocol/metering"
)

// Defaults for the connection limits.
const (
	DefaultPingInterval = 30 * time.Second
	DefaultPingTimeout  = 10 * time.Second
	DefaultWriteTimeout = 5 * time.Second
	// A well-behaved agent reports every 3 to 60 seconds, so these leave a
	// wide margin and only catch abuse.
	DefaultMessageRate  = 2
	DefaultMessageBurst = 20
)

// Options configure a Service.
type Options struct {
	DB    *store.DB
	Hosts *hosts.Service
	// Status is the live registry the pages read. Nil keeps no live
	// state, which is what the storage tests want.
	Status *status.Registry
	Logger *slog.Logger
	// Now is the clock; zero means time.Now.
	Now func() time.Time
	// TimeZone returns the panel's time zone, which decides the day that
	// traffic is counted on. Nil means UTC.
	TimeZone func() *time.Location

	PingInterval time.Duration
	PingTimeout  time.Duration
	WriteTimeout time.Duration
	// MessageRate is the sustained messages per second per connection and
	// MessageBurst the bucket size.
	MessageRate  float64
	MessageBurst float64
}

// Service handles agent connections.
type Service struct {
	opts   Options
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// conns maps an agent id to its current session, so that a new
	// connection can close the previous one.
	conns map[string]*session

	// rates holds the previous counters per host, which is what network
	// rates are computed from. It is deliberately in memory: a rate is a
	// derivative of two consecutive messages, unlike the cumulative
	// traffic, which is stored.
	rates *metering.RateTracker[int64]
}

// New returns a Service. Close ends every open connection.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Hosts == nil || opts.Logger == nil {
		return nil, errors.New("ingest: incomplete options")
	}
	if opts.TimeZone == nil {
		opts.TimeZone = func() *time.Location { return time.UTC }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	setDefault(&opts.PingInterval, DefaultPingInterval)
	setDefault(&opts.PingTimeout, DefaultPingTimeout)
	setDefault(&opts.WriteTimeout, DefaultWriteTimeout)
	if opts.MessageRate <= 0 {
		opts.MessageRate = DefaultMessageRate
	}
	if opts.MessageBurst <= 0 {
		opts.MessageBurst = DefaultMessageBurst
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		opts:   opts,
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[string]*session),
		rates:  metering.NewRateTracker[int64](),
	}, nil
}

func setDefault(d *time.Duration, v time.Duration) {
	if *d <= 0 {
		*d = v
	}
}

// Close ends every open agent connection.
func (s *Service) Close() { s.cancel() }

// Handler serves the agent WebSocket endpoint. The route is public in the
// sense that it carries no session cookie; it authenticates itself with the
// Authorization header.
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	// The protocol version is settled before the upgrade: an agent that
	// offers none the panel speaks is refused outright.
	version, ok := protocol.SelectVersion(r.Header.Values(protocol.SubprotocolHeader))
	if !ok {
		http.Error(w, "unsupported protocol version", http.StatusBadRequest)
		return
	}
	auth, err := s.opts.Hosts.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, hosts.ErrUnauthorized) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.opts.Logger.Error("authenticating an agent", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{version},
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(protocol.MaxPanelRead)
	s.run(conn, auth, clientIP(r))
}

// run drives one agent connection until it ends.
func (s *Service) run(conn *websocket.Conn, auth hosts.Auth, ip netip.Addr) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	sess := &session{conn: conn, cancel: cancel, writeTimeout: s.opts.WriteTimeout}
	s.replacePrevious(auth.AgentID, sess)
	defer s.forget(auth.AgentID, sess)
	defer sess.finish()

	log := s.opts.Logger.With("host_id", auth.HostID, "agent_id", auth.AgentID)
	if s.opts.Status != nil {
		// The host counts as connected from here, before the panel says
		// anything on the connection. Cancelling the session is how the
		// evaluator later closes the connection of a host it has written
		// off: the read ends, and the connection with it.
		s.opts.Status.Connected(auth.HostID, 0, cancel)
		defer s.opts.Status.Disconnected(auth.HostID)
	}
	if auth.Registered != nil {
		if err := sess.write(ctx, auth.Registered); err != nil {
			log.Warn("sending the registration result", "error", err)
			return
		}
		log.Info("agent registered")
	}
	if err := s.opts.Hosts.Connected(ctx, auth.HostID, ip); err != nil {
		log.Error("recording the connection", "error", err)
		sess.fail(protocol.CodeServerError, "could not record the connection")
		return
	}
	log.Info("agent connected", "remote_ip", ip)
	defer log.Info("agent disconnected")

	go s.ping(ctx, conn, cancel)

	limit := bucket{tokens: s.opts.MessageBurst, rate: s.opts.MessageRate, burst: s.opts.MessageBurst}
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if !limit.allow(s.opts.Now()) {
			log.Warn("agent exceeded the message rate limit")
			sess.fail(protocol.CodeRateLimited, "too many messages")
			return
		}
		if typ != websocket.MessageText {
			sess.fail(protocol.CodeInvalidMessage, "expected a text message")
			return
		}
		msg, err := protocol.DecodeAgentMessage(data)
		if err != nil {
			log.Warn("rejecting an agent message", "error", err)
			sess.fail(protocol.CodeInvalidMessage, "the message was rejected")
			return
		}
		if err := s.handle(ctx, auth.HostID, msg); err != nil {
			log.Error("storing an agent message", "error", err)
			sess.fail(protocol.CodeServerError, "could not store the message")
			return
		}
	}
}

// handle applies one validated message.
func (s *Service) handle(ctx context.Context, hostID int64, msg protocol.AgentMessage) error {
	switch m := msg.(type) {
	case *protocol.HostInfo:
		if s.opts.Status != nil {
			s.opts.Status.SetInterval(hostID, time.Duration(m.IntervalMS)*time.Millisecond)
		}
		return s.opts.Hosts.SetHostInfo(ctx, hostID, m)
	case *protocol.Metrics:
		// The exclusion rules are read per sample rather than per
		// connection, so a change on the settings page takes effect on
		// the next sample instead of the next reconnect.
		exclude, err := s.opts.Hosts.IfaceExclude(ctx, hostID)
		if err != nil {
			return err
		}
		return s.storeMetrics(ctx, hostID, m, exclude)
	default:
		return nil
	}
}

// ping keeps the connection alive and detects a peer that has gone away.
func (s *Service) ping(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(s.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, done := context.WithTimeout(ctx, s.opts.PingTimeout)
			err := conn.Ping(pingCtx)
			done()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

// replacePrevious closes any earlier connection of the same agent before
// registering this one.
func (s *Service) replacePrevious(agentID string, sess *session) {
	s.mu.Lock()
	previous := s.conns[agentID]
	s.conns[agentID] = sess
	s.mu.Unlock()
	if previous != nil {
		// Asynchronously: the previous peer may be a zombie connection,
		// and the new one must not wait for it.
		go previous.fail(protocol.CodeReplaced, "another connection took over")
	}
}

func (s *Service) forget(agentID string, sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[agentID] == sess {
		delete(s.conns, agentID)
	}
}

// bucket is a token bucket limiting the messages of one connection.
type bucket struct {
	tokens, rate, burst float64
	last                time.Time
}

func (b *bucket) allow(now time.Time) bool {
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Revoke closes the connection of an agent whose credential the operator
// just deleted or reset. Authentication happens at the handshake, so an
// open connection outlives the record it was authenticated against until
// someone closes it.
//
// The agent is told "unauthorized", which is the code that makes an agent
// with a token in its configuration enroll again.
func (s *Service) Revoke(agentID string) {
	if agentID == "" {
		return
	}
	s.mu.Lock()
	sess := s.conns[agentID]
	s.mu.Unlock()
	if sess != nil {
		// Asynchronously: the peer may be a zombie connection and the
		// page must not wait for it.
		go sess.fail(protocol.CodeUnauthorized, "this credential is no longer valid")
	}
}

// Forget drops what the panel kept in memory for a host, which is what
// keeps a deleted host from leaving entries behind.
func (s *Service) Forget(hostID int64) {
	s.rates.Forget(hostID)
	if s.opts.Status != nil {
		s.opts.Status.Forget(hostID)
	}
}
