package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/i18n"
	"github.com/kergeio/kerge-panel/internal/notify"
	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/store"
	webfiles "github.com/kergeio/kerge-panel/web"
)

// hook is a webhook that records the text of every message.
type hook struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []string
	code int
}

func newHook(t *testing.T) *hook {
	h := &hook{code: http.StatusOK}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Text string }
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &v)
		h.mu.Lock()
		h.got = append(h.got, v.Text)
		code := h.code
		h.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// take returns the messages received since the last call.
func (h *hook) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.got
	h.got = nil
	return out
}

type rig struct {
	t        *testing.T
	db       *store.DB
	hosts    *hosts.Service
	settings *settings.Service
	status   *status.Registry
	watcher  *Watcher
	hook     *hook
	now      time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "kerge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	locales, err := fs.Sub(webfiles.Files, "locales")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := i18n.Load(locales, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, db: db, hosts: hosts.NewService(db), settings: settings.NewService(db), hook: newHook(t),
		now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return r.now }
	r.status = status.New(status.Options{Now: clock})
	r.watcher = New(Options{
		DB: db, Hosts: r.hosts, Status: r.status, Settings: r.settings, Sender: notify.NewSender(),
		Text:   cat.Translator("en").T,
		Logger: slog.New(slog.DiscardHandler),
		Now:    clock,
	})
	cfg := notify.Default()
	cfg.Enabled = true
	cfg.URL = r.hook.srv.URL
	cfg.Body = `{"text":"{{message}}"}`
	r.configure(cfg)
	return r
}

func (r *rig) configure(cfg notify.Config) {
	r.t.Helper()
	if err := r.settings.SaveNotifications(r.t.Context(), cfg); err != nil {
		r.t.Fatal(err)
	}
}

// addHost creates an enrolled host that last connected from ip.
func (r *rig) addHost(name, ip string) int64 {
	r.t.Helper()
	ctx := r.t.Context()
	id, err := r.hosts.Create(ctx, name)
	if err != nil {
		r.t.Fatal(err)
	}
	err = r.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE hosts SET agent_id = ?, secret_hash = 'x' WHERE id = ?", name+"-agent", id)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	if ip != "" {
		if err := r.hosts.Connected(ctx, id, netip.MustParseAddr(ip)); err != nil {
			r.t.Fatal(err)
		}
	}
	return id
}

func (r *rig) check() []string {
	r.t.Helper()
	if err := r.watcher.Check(r.t.Context()); err != nil {
		r.t.Fatal(err)
	}
	return r.hook.take()
}

func (r *rig) advance(d time.Duration) { r.now = r.now.Add(d) }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOfflineAndBackOnline(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	disconnect := r.status.Connected(id, 5*time.Second, nil)

	if got := r.check(); len(got) != 0 {
		t.Fatalf("online host: sent %q", got)
	}
	disconnect()
	want := []string{"[Kerge] web-1 (203.0.113.10) is offline"}
	if got := r.check(); !equal(got, want) {
		t.Fatalf("after disconnect: sent %q, want %q", got, want)
	}
	if got := r.check(); len(got) != 0 {
		t.Fatalf("still offline: sent %q again", got)
	}

	r.status.Connected(id, 5*time.Second, nil)
	want = []string{"[Kerge] web-1 (203.0.113.10) is back online"}
	if got := r.check(); !equal(got, want) {
		t.Fatalf("after reconnect: sent %q, want %q", got, want)
	}
	if got := r.check(); len(got) != 0 {
		t.Fatalf("still online: sent %q again", got)
	}
}

func TestStaleIsNotOffline(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	r.advance(90 * time.Second) // stale, still connected
	if s := r.status.Get(id).State; s != status.Stale {
		t.Fatalf("state %s, want stale", s)
	}
	if got := r.check(); len(got) != 0 {
		t.Fatalf("stale host: sent %q", got)
	}
	r.advance(2 * time.Minute) // no data for 3.5 minutes: offline
	if got := r.check(); len(got) != 1 {
		t.Fatalf("silent host: sent %q, want one message", got)
	}
}

func TestStartGraceAndRestart(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "")
	// Right after the panel starts the host is connecting: nothing yet.
	if got := r.check(); len(got) != 0 {
		t.Fatalf("connecting host: sent %q", got)
	}
	// It never comes back: offline once the grace period is over. The
	// panel has no address for it, so none is shown.
	r.advance(status.StartGrace)
	want := []string{"[Kerge] web-1 is offline"}
	if got := r.check(); !equal(got, want) {
		t.Fatalf("after the grace period: sent %q, want %q", got, want)
	}

	// The panel restarts while the host is still down: a new registry,
	// the same database. Nothing is sent again.
	r.status = status.New(status.Options{Now: func() time.Time { return r.now }})
	r.watcher.opts.Status = r.status
	r.advance(status.StartGrace)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("after a restart: sent %q again", got)
	}
	// It reconnects to the new panel: back online.
	r.status.Connected(id, 5*time.Second, nil)
	if got := r.check(); len(got) != 1 {
		t.Fatalf("after reconnecting: sent %q, want one message", got)
	}
}

func TestPendingHostsAreIgnored(t *testing.T) {
	r := newRig(t)
	if _, err := r.hosts.Create(t.Context(), "new"); err != nil {
		t.Fatal(err)
	}
	r.advance(status.StartGrace)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("pending host: sent %q", got)
	}
}

func TestSwitchedOffEventsAreTrackedButNotSent(t *testing.T) {
	for _, change := range []func(*notify.Config){
		func(c *notify.Config) { c.Enabled = false },
		func(c *notify.Config) { c.Offline = false },
	} {
		r := newRig(t)
		cfg := notify.Default()
		cfg.Enabled = true
		cfg.URL = r.hook.srv.URL
		cfg.Body = `{"text":"{{message}}"}`
		change(&cfg)
		r.configure(cfg)

		id := r.addHost("web-1", "203.0.113.10")
		r.status.Connected(id, 5*time.Second, nil)()
		if got := r.check(); len(got) != 0 {
			t.Fatalf("switched off: sent %q", got)
		}
		// Switched on while the host is down: the old outage is not
		// reported, but its end is.
		cfg.Enabled, cfg.Offline = true, true
		r.configure(cfg)
		if got := r.check(); len(got) != 0 {
			t.Fatalf("switched on during an outage: sent %q", got)
		}
		r.status.Connected(id, 5*time.Second, nil)
		if got := r.check(); len(got) != 1 {
			t.Fatalf("back online: sent %q, want one message", got)
		}
	}
}

func TestFailedDeliveryIsNotRepeated(t *testing.T) {
	r := newRig(t)
	r.hook.code = http.StatusInternalServerError
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)()
	if got := r.check(); len(got) != 1 {
		t.Fatalf("sent %q, want one attempt", got)
	}
	if got := r.check(); len(got) != 0 {
		t.Fatalf("a refused message was sent again: %q", got)
	}
}

func TestDeletedHostLeavesNoAlert(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)()
	r.check()
	if _, err := r.hosts.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := r.db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM alerts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d alert rows left after deleting the host", n)
	}
}
