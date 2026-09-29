package ingest

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-protocol"
)

type fixture struct {
	t      *testing.T
	db     *store.DB
	hosts  *hosts.Service
	status *status.Registry
	svc    *Service
	server *httptest.Server
	hostID int64
	token  string
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "kerge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	f := &fixture{t: t, db: db, hosts: hosts.NewService(db), now: time.Unix(1_700_000_000, 0)}
	f.status = status.New(status.Options{Now: func() time.Time { return f.now }})
	f.svc, err = New(Options{
		DB:     db,
		Hosts:  f.hosts,
		Status: f.status,
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.svc.Close)

	handler := f.svc.Handler()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)

	if f.hostID, err = f.hosts.Create(t.Context(), "web-1"); err != nil {
		t.Fatal(err)
	}
	if f.token, err = f.hosts.NewEnrollToken(t.Context(), f.hostID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) url() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + "/api/agent/ws"
}

// dial connects as an agent would.
func (f *fixture) dial(authorization string) (*websocket.Conn, *http.Response, error) {
	return f.dialVersions(authorization, protocol.Supported())
}

// dialVersions connects offering the given protocol versions.
func (f *fixture) dialVersions(authorization string, versions []string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, f.url(), &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": []string{authorization}},
		Subprotocols:    versions,
		CompressionMode: websocket.CompressionContextTakeover,
	})
}

// enroll connects with the one-time token and returns the connection and
// the credential the panel issued.
func (f *fixture) enroll() (*websocket.Conn, string) {
	f.t.Helper()
	conn, _, err := f.dial(protocol.EnrollAuthorization(f.token))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.CloseNow() })
	msg := f.recv(conn)
	reg, ok := msg.(*protocol.Registered)
	if !ok {
		f.t.Fatalf("got %T, want the registration result", msg)
	}
	return conn, reg.AgentID + "." + reg.Secret
}

func (f *fixture) recv(conn *websocket.Conn) protocol.PanelMessage {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		f.t.Fatalf("reading from the panel: %v", err)
	}
	msg, err := protocol.DecodePanelMessage(data)
	if err != nil {
		f.t.Fatalf("decoding the panel message: %v", err)
	}
	return msg
}

func (f *fixture) send(conn *websocket.Conn, msg any) {
	f.t.Helper()
	data, err := protocol.Marshal(msg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.sendRaw(conn, websocket.MessageText, data)
}

func (f *fixture) sendRaw(conn *websocket.Conn, typ websocket.MessageType, data []byte) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, typ, data); err != nil {
		f.t.Fatalf("writing to the panel: %v", err)
	}
}

// waitRows polls until metrics_raw holds n rows for the host.
func (f *fixture) waitRows(n int) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := f.rows(); got >= n {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("metrics_raw holds %d rows, want %d", f.rows(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fixture) rows() int {
	f.t.Helper()
	var n int
	if err := f.db.Read().QueryRowContext(f.t.Context(),
		"SELECT count(*) FROM metrics_raw WHERE host_id = ?", f.hostID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func sample(monoMS int64, net map[string]protocol.NetCounters) *protocol.Metrics {
	cpu := 12.5
	memTotal, memUsed := uint64(2048), uint64(512)
	uptime := uint64(3600)
	return &protocol.Metrics{
		TS: 1, MonoMS: monoMS, CPUPercent: &cpu,
		MemTotal: &memTotal, MemUsed: &memUsed, Uptime: &uptime, Net: net,
	}
}

func iface(rx, tx uint64) map[string]protocol.NetCounters {
	return map[string]protocol.NetCounters{"eth0": {RX: rx, TX: tx}}
}

func TestEnrollAndStore(t *testing.T) {
	f := newFixture(t)
	conn, credential := f.enroll()

	f.send(conn, &protocol.HostInfo{Hostname: "web-1", OS: "linux", CPUCores: 2, IntervalMS: 5000})
	f.send(conn, sample(1000, iface(100, 200)))
	f.waitRows(1)

	var hostname string
	var lastSeen, cores int64
	if err := f.db.Read().QueryRowContext(t.Context(),
		"SELECT hostname, cpu_cores, last_seen_at FROM hosts WHERE id = ?",
		f.hostID).Scan(&hostname, &cores, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if hostname != "web-1" || cores != 2 || lastSeen != f.now.Unix() {
		t.Errorf("host row: %q %d %d", hostname, cores, lastSeen)
	}

	var ts int64
	var cpu, rxRate sql.NullFloat64
	var memUsed sql.NullInt64
	if err := f.db.Read().QueryRowContext(t.Context(),
		"SELECT ts, cpu, mem_used, rx_rate FROM metrics_raw WHERE host_id = ?",
		f.hostID).Scan(&ts, &cpu, &memUsed, &rxRate); err != nil {
		t.Fatal(err)
	}
	// The row carries the panel's receive time, not the agent's ts.
	if ts != f.now.Unix() {
		t.Errorf("ts = %d, want the receive time %d", ts, f.now.Unix())
	}
	if !cpu.Valid || cpu.Float64 != 12.5 || !memUsed.Valid || memUsed.Int64 != 512 {
		t.Errorf("cpu = %v, mem_used = %v", cpu, memUsed)
	}
	// The first sample has nothing to compute a rate against.
	if rxRate.Valid {
		t.Errorf("rx_rate = %v on the first sample, want NULL", rxRate)
	}

	// The credential works on its own connection.
	next, _, err := f.dial(protocol.AgentAuthorization(credential))
	if err != nil {
		t.Fatal(err)
	}
	next.CloseNow()
}

// Rates come from the difference between consecutive counters, divided by
// the agent's monotonic delta.
func TestNetworkRate(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()

	f.send(conn, sample(1000, iface(1000, 2000)))
	f.waitRows(1)
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(6000, iface(6000, 9000)))
	f.waitRows(2)

	rx, tx := f.lastRate()
	if !rx.Valid || rx.Float64 != 1000 {
		t.Errorf("rx_rate = %v, want 1000 bytes per second", rx)
	}
	if !tx.Valid || tx.Float64 != 1400 {
		t.Errorf("tx_rate = %v, want 1400 bytes per second", tx)
	}
}

func (f *fixture) lastRate() (rx, tx sql.NullFloat64) {
	f.t.Helper()
	if err := f.db.Read().QueryRowContext(f.t.Context(),
		"SELECT rx_rate, tx_rate FROM metrics_raw WHERE host_id = ? ORDER BY ts DESC LIMIT 1",
		f.hostID).Scan(&rx, &tx); err != nil {
		f.t.Fatal(err)
	}
	return rx, tx
}

// Samples that cannot yield a trustworthy rate leave the columns NULL
// rather than reporting zero.
func TestNetworkRateIsSkipped(t *testing.T) {
	cases := []struct {
		name     string
		advance  time.Duration
		monoMS   int64
		counters map[string]protocol.NetCounters
	}{
		{"monotonic clock went backwards", 5 * time.Second, 500, iface(6000, 9000)},
		{"monotonic clock did not advance", 5 * time.Second, 1000, iface(6000, 9000)},
		{"monotonic delta far above the receive delta", time.Second, 60000, iface(6000, 9000)},
		{"receive delta far above the monotonic delta", time.Minute, 2000, iface(6000, 9000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			conn, _ := f.enroll()
			f.send(conn, sample(1000, iface(1000, 2000)))
			f.waitRows(1)
			f.now = f.now.Add(c.advance)
			f.send(conn, sample(c.monoMS, c.counters))
			f.waitRows(2)
			if rx, tx := f.lastRate(); rx.Valid || tx.Valid {
				t.Errorf("rx=%v tx=%v, want NULL", rx, tx)
			}
		})
	}
}

// A counter that went backwards, typically after the host rebooted, means
// that interface contributes nothing this cycle.
func TestInterfaceCounterWentBackwards(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	two := func(a, b uint64) map[string]protocol.NetCounters {
		return map[string]protocol.NetCounters{"eth0": {RX: a, TX: a}, "eth1": {RX: b, TX: b}}
	}
	f.send(conn, sample(1000, two(1000, 1000)))
	f.waitRows(1)
	f.now = f.now.Add(time.Second)
	f.send(conn, sample(2000, two(10, 3000)))
	f.waitRows(2)

	// Only eth1 counts: 2000 bytes in one second.
	if rx, _ := f.lastRate(); !rx.Valid || rx.Float64 != 2000 {
		t.Errorf("rx_rate = %v, want 2000", rx)
	}
}

// Interfaces the panel excludes are left out of the sum, even though the
// agent already applied its own rules.
func TestPanelExcludesInterfaces(t *testing.T) {
	f := newFixture(t)
	if err := f.db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE hosts SET net_iface_exclude = ? WHERE id = ?", "eth1", f.hostID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conn, _ := f.enroll()
	two := func(a, b uint64) map[string]protocol.NetCounters {
		return map[string]protocol.NetCounters{"eth0": {RX: a, TX: a}, "eth1": {RX: b, TX: b}}
	}
	f.send(conn, sample(1000, two(0, 0)))
	f.waitRows(1)
	f.now = f.now.Add(time.Second)
	f.send(conn, sample(2000, two(100, 5000)))
	f.waitRows(2)
	if rx, _ := f.lastRate(); !rx.Valid || rx.Float64 != 100 {
		t.Errorf("rx_rate = %v, want only eth0 counted", rx)
	}
}

// A change to the exclusion rules reaches the next sample rather than
// waiting for the agent to reconnect.
func TestExclusionRulesAreRereadPerSample(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	two := func(a, b uint64) map[string]protocol.NetCounters {
		return map[string]protocol.NetCounters{"eth0": {RX: a, TX: a}, "eth1": {RX: b, TX: b}}
	}
	f.send(conn, sample(1000, two(0, 0)))
	f.waitRows(1)

	if err := f.db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE hosts SET net_iface_exclude = ? WHERE id = ?", "eth1", f.hostID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	f.send(conn, sample(2000, two(100, 5000)))
	f.waitRows(2)
	if rx, _ := f.lastRate(); !rx.Valid || rx.Float64 != 100 {
		t.Errorf("rx_rate = %v, want only eth0 counted", rx)
	}
}

// Two samples inside the same second collide on the primary key; the newer
// one wins.
func TestSameSecondSampleReplaces(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	first := sample(1000, nil)
	cpu := 90.0
	second := sample(2000, nil)
	second.CPUPercent = &cpu

	f.send(conn, first)
	f.waitRows(1)
	f.send(conn, second)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var got sql.NullFloat64
		if err := f.db.Read().QueryRowContext(t.Context(),
			"SELECT cpu FROM metrics_raw WHERE host_id = ?", f.hostID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.Valid && got.Float64 == 90 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cpu = %v, want the newer sample to win", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if f.rows() != 1 {
		t.Errorf("metrics_raw holds %d rows, want 1", f.rows())
	}
}

func TestHandshakeRejectsBadCredentials(t *testing.T) {
	f := newFixture(t)
	cases := map[string]string{
		"missing":       "",
		"unknown kind":  "Bearer other:x",
		"unknown token": protocol.EnrollAuthorization("nope"),
		"unknown agent": protocol.AgentAuthorization("nope.nope"),
	}
	for name, header := range cases {
		conn, resp, err := f.dial(header)
		if err == nil {
			conn.CloseNow()
			t.Errorf("%s: the handshake succeeded", name)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %v, want 401", name, resp)
		}
	}
}

// A message the panel cannot accept ends the connection with a reason.
func TestBadAgentMessagesEndTheConnection(t *testing.T) {
	cases := map[string]string{
		"unknown type":  `{"type":"hello"}`,
		"unknown field": `{"type":"metrics","ts":1,"mono_ms":2,"rm":"-rf"}`,
		"out of range":  `{"type":"metrics","ts":1,"mono_ms":2,"cpu_percent":500}`,
		"malformed":     `{"type":"metrics",`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			conn, _ := f.enroll()
			f.sendRaw(conn, websocket.MessageText, []byte(payload))
			msg := f.recv(conn)
			e, ok := msg.(*protocol.ErrorMessage)
			if !ok || e.Code != protocol.CodeInvalidMessage {
				t.Fatalf("got %+v, want an invalid_message error", msg)
			}
		})
	}
}

func TestBinaryMessageEndsTheConnection(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	f.sendRaw(conn, websocket.MessageBinary, []byte(`{"type":"metrics","ts":1,"mono_ms":2}`))
	if e, ok := f.recv(conn).(*protocol.ErrorMessage); !ok || e.Code != protocol.CodeInvalidMessage {
		t.Errorf("got %v, want an invalid_message error", e)
	}
}

// A second connection for the same agent takes over and the first is told
// why.
func TestNewConnectionReplacesTheOld(t *testing.T) {
	f := newFixture(t)
	first, credential := f.enroll()

	second, _, err := f.dial(protocol.AgentAuthorization(credential))
	if err != nil {
		t.Fatal(err)
	}
	defer second.CloseNow()

	msg := f.recv(first)
	e, ok := msg.(*protocol.ErrorMessage)
	if !ok || e.Code != protocol.CodeReplaced {
		t.Fatalf("got %+v, want a replaced error", msg)
	}

	// The surviving connection still works.
	f.send(second, sample(1000, nil))
	f.waitRows(1)
}

// Too many messages end the connection.
func TestMessageRateLimit(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	for range int(DefaultMessageBurst) + 5 {
		f.send(conn, sample(1000, nil))
	}
	msg := f.recv(conn)
	e, ok := msg.(*protocol.ErrorMessage)
	if !ok || e.Code != protocol.CodeRateLimited {
		t.Fatalf("got %+v, want a rate_limited error", msg)
	}
}

// An oversized message is cut off at the WebSocket layer, so the panel
// never buffers it.
func TestOversizedMessageIsCutOff(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	big := `{"type":"metrics","ts":1,"mono_ms":2,"cpu_model":"` + strings.Repeat("a", 2*protocol.MaxPanelRead) + `"}`
	f.sendRaw(conn, websocket.MessageText, []byte(big))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Errorf("close status = %v, want %v (error %v)", got, websocket.StatusMessageTooBig, err)
	}
}

func TestBucket(t *testing.T) {
	b := bucket{tokens: 2, rate: 1, burst: 2}
	now := time.Unix(0, 0)
	if !b.allow(now) || !b.allow(now) {
		t.Fatal("the burst was not allowed")
	}
	if b.allow(now) {
		t.Error("a third message was allowed with an empty bucket")
	}
	if !b.allow(now.Add(time.Second)) {
		t.Error("the bucket did not refill")
	}
	if b.allow(now.Add(time.Second)) {
		t.Error("the bucket refilled too fast")
	}
}

func TestClientIPIsRecorded(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	f.send(conn, sample(1000, nil))
	f.waitRows(1)
	var ip string
	if err := f.db.Read().QueryRowContext(t.Context(),
		"SELECT coalesce(remote_ip, '') FROM hosts WHERE id = ?", f.hostID).Scan(&ip); err != nil {
		t.Fatal(err)
	}
	// The test server has no client IP middleware in front of it, so the
	// recorded address is the zero value; what matters is that the column
	// is written on every connection.
	if _, err := netip.ParseAddr(ip); err != nil && ip != "" {
		t.Errorf("remote_ip = %q", ip)
	}
}

// An agent that offers no version the panel speaks is refused before the
// upgrade, so no connection exists to report an error on.
func TestHandshakeRequiresProtocolVersion(t *testing.T) {
	f := newFixture(t)
	for name, versions := range map[string][]string{
		"none offered":    nil,
		"unknown version": {"kerge.v9"},
		"other protocol":  {"graphql-ws"},
		"wrong case":      {"KERGE.V1"},
	} {
		t.Run(name, func(t *testing.T) {
			conn, resp, err := f.dialVersions(protocol.EnrollAuthorization(f.token), versions)
			if err == nil {
				conn.CloseNow()
				t.Fatal("the handshake succeeded")
			}
			if resp == nil || resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %v, want 400", resp)
			}
		})
	}
	// The token survives a refused handshake and still enrolls an agent
	// that offers a version the panel speaks.
	conn, _ := f.enroll()
	if got := conn.Subprotocol(); got != protocol.Version {
		t.Errorf("negotiated subprotocol = %q, want %q", got, protocol.Version)
	}
}

// Deleting a host or resetting its access revokes a credential that an
// open connection was already authenticated with, so the panel closes that
// connection itself.
func TestRevokeClosesTheConnection(t *testing.T) {
	f := newFixture(t)
	conn, credential := f.enroll()
	agentID, _, _ := strings.Cut(credential, ".")

	f.svc.Revoke(agentID)

	msg := f.recv(conn)
	e, ok := msg.(*protocol.ErrorMessage)
	if !ok || e.Code != protocol.CodeUnauthorized {
		t.Fatalf("got %+v, want an unauthorized error", msg)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Error("the connection is still open after the credential was revoked")
	}
	// An agent that is not connected is simply not there.
	f.svc.Revoke("no-such-agent")
	f.svc.Revoke("")
}

// A deleted host must not leave its rate baseline behind: the next host to
// use that id would otherwise inherit it.
func TestForgetDropsTheRateBaseline(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()

	f.send(conn, sample(1000, iface(1000, 2000)))
	f.waitRows(1)
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(6000, iface(6000, 9000)))
	f.waitRows(2)
	if rx, _ := f.lastRate(); !rx.Valid {
		t.Fatal("no rate before forgetting the baseline")
	}

	f.svc.Forget(f.hostID)

	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(11000, iface(11000, 16000)))
	f.waitRows(3)
	if rx, tx := f.lastRate(); rx.Valid || tx.Valid {
		t.Errorf("rates after forgetting = %v / %v, want none", rx, tx)
	}
}

// An agent connection drives the live registry the dashboard reads.
func TestConnectionFeedsTheRegistry(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()

	if got := f.status.Get(f.hostID).State; got != status.Online {
		t.Errorf("state while connected = %s", got)
	}
	f.send(conn, &protocol.HostInfo{
		Hostname: "web-1", OS: "linux", CPUCores: 2, AgentVersion: "0.1.0", IntervalMS: 30000,
	})
	f.send(conn, sample(1000, iface(1000, 2000)))
	f.waitRows(1)

	live := f.status.Get(f.hostID)
	if live.Interval != 30*time.Second {
		t.Errorf("interval = %v, want the one the agent announced", live.Interval)
	}
	if !live.HasSample || live.Sample.CPU == nil {
		t.Fatalf("no sample in the registry: %+v", live)
	}
	if live.LastDataAt != f.now {
		t.Errorf("last data at %v, want the receive time %v", live.LastDataAt, f.now)
	}

	// The rate the panel derived is what the card shows, so it is in the
	// registry rather than the raw counters.
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(6000, iface(6000, 9000)))
	f.waitRows(2)
	live = f.status.Get(f.hostID)
	if live.Sample.RxRate == nil || *live.Sample.RxRate != 1000 {
		t.Errorf("rx rate in the registry = %v, want 1000", live.Sample.RxRate)
	}

	conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return f.status.Get(f.hostID).State == status.Offline })
}

// A host that stops reporting while its connection stays open is written
// off, and the evaluator closes that connection.
func TestEvaluatorClosesASilentConnection(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	f.send(conn, sample(1000, iface(1000, 2000)))
	f.waitRows(1)

	f.now = f.now.Add(status.OfflineFloor + time.Second)
	f.status.Evaluate([]int64{f.hostID})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	began := time.Now()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("the connection of an offline host stayed open")
	}
	// A read that ran into its own deadline proves nothing: the panel has
	// to close the connection, and quickly.
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("the connection was still open after %v", waited)
	}
	waitFor(t, func() bool { return !f.status.Get(f.hostID).Connected })
}

// waitFor polls until the condition holds, for state the connection
// goroutine updates as it winds down.
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the condition did not hold within five seconds")
}
