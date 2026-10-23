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
	"github.com/kergeio/kerge-panel/internal/reminder"
	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-panel/internal/traffic"
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
	loc      *time.Location
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
		now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC), loc: time.UTC}
	clock := func() time.Time { return r.now }
	r.status = status.New(status.Options{Now: clock})
	r.watcher = New(Options{
		DB: db, Hosts: r.hosts, Status: r.status, Settings: r.settings, Sender: notify.NewSender(),
		Translator: func() *i18n.Translator { return cat.Translator("en") },
		TimeZone:   func() *time.Location { return r.loc },
		Logger:     slog.New(slog.DiscardHandler),
		Now:        clock,
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

// onlyDateEvents switches the offline event off, for tests that move the
// clock by hours or days, which takes every host offline.
func (r *rig) onlyDateEvents() {
	r.t.Helper()
	cfg, err := r.settings.Notifications(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	cfg.Offline = false
	r.configure(cfg)
}

func date(y int, m time.Month, d int) reminder.Date { return reminder.Date{Year: y, Month: m, Day: d} }

func TestRenewalOnTheReminderDay(t *testing.T) {
	r := newRig(t)
	r.onlyDateEvents()
	id := r.addHost("web-1", "203.0.113.10")
	if err := r.hosts.SetReminder(t.Context(), id, date(2026, 10, 23), 1); err != nil {
		t.Fatal(err)
	}
	r.status.Connected(id, 5*time.Second, nil)

	r.now = time.Date(2026, 10, 23, 8, 59, 0, 0, time.UTC)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("before 09:00: sent %q", got)
	}
	r.now = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	want := []string{"[Kerge] web-1 (203.0.113.10) renewal due today"}
	if got := r.check(); !equal(got, want) {
		t.Fatalf("at 09:00: sent %q, want %q", got, want)
	}
	r.advance(time.Hour)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("later that day: sent %q again", got)
	}
	// The next day the reminder is still due, but it is not its day.
	r.now = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("the day after: sent %q", got)
	}
	// The next reminder date is reported again.
	r.now = time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	if got := r.check(); !equal(got, want) {
		t.Fatalf("next month: sent %q, want %q", got, want)
	}
}

func TestRenewalAlreadyConfirmed(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	if err := r.hosts.SetReminder(t.Context(), id, date(2026, 10, 23), 1); err != nil {
		t.Fatal(err)
	}
	if err := r.hosts.AckReminder(t.Context(), id, date(2026, 10, 23)); err != nil {
		t.Fatal(err)
	}
	if got := r.check(); len(got) != 0 {
		t.Fatalf("confirmed before 09:00: sent %q", got)
	}
}

func TestRenewalFollowsThePanelTimeZone(t *testing.T) {
	r := newRig(t)
	r.onlyDateEvents()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	r.loc = loc
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	if err := r.hosts.SetReminder(t.Context(), id, date(2026, 10, 24), 12); err != nil {
		t.Fatal(err)
	}
	// 00:30 UTC on the 24th is 08:30 in Shanghai: not yet.
	r.now = time.Date(2026, 10, 24, 0, 30, 0, 0, time.UTC)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("08:30 local: sent %q", got)
	}
	// 01:00 UTC is 09:00 local.
	r.now = time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	if got := r.check(); len(got) != 1 {
		t.Fatalf("09:00 local: sent %q, want one message", got)
	}
}

// addTraffic stores the traffic of one day.
func (r *rig) addTraffic(id int64, day reminder.Date, rx, tx int64) {
	r.t.Helper()
	err := r.db.Write(r.t.Context(), func(ctx context.Context, tx2 *sql.Tx) error {
		_, err := tx2.ExecContext(ctx,
			`INSERT INTO traffic_daily (host_id, day, rx_bytes, tx_bytes) VALUES (?, ?, ?, ?)
			 ON CONFLICT (host_id, day) DO UPDATE SET rx_bytes = rx_bytes + excluded.rx_bytes, tx_bytes = tx_bytes + excluded.tx_bytes`,
			id, day.String(), rx, tx)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

const mib = 1 << 20

func TestTrafficLimitOncePerCycle(t *testing.T) {
	r := newRig(t)
	r.onlyDateEvents()
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	if err := r.hosts.SetTraffic(t.Context(), id, traffic.Settings{Mode: traffic.Both, ResetDay: 1, LimitBytes: 1024 * mib}); err != nil {
		t.Fatal(err)
	}
	// Last cycle's traffic does not count.
	r.addTraffic(id, date(2026, 9, 30), 5000*mib, 0)
	r.addTraffic(id, date(2026, 10, 5), 600*mib, 300*mib)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("900 MiB of 1 GiB: sent %q", got)
	}
	r.addTraffic(id, date(2026, 10, 20), 124*mib, 0)
	want := []string{"[Kerge] web-1 (203.0.113.10) traffic reached the limit of 1.0 GiB"}
	if got := r.check(); !equal(got, want) {
		t.Fatalf("at the limit: sent %q, want %q", got, want)
	}
	r.addTraffic(id, date(2026, 10, 23), 500*mib, 0)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("over the limit: sent %q again", got)
	}
	// A new cycle starts from nothing, and is reported again once full.
	r.now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("new cycle: sent %q", got)
	}
	r.addTraffic(id, date(2026, 11, 1), 2048*mib, 0)
	if got := r.check(); !equal(got, want) {
		t.Fatalf("new cycle at the limit: sent %q, want %q", got, want)
	}
}

func TestTrafficCountsTheModeOnly(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	if err := r.hosts.SetTraffic(t.Context(), id, traffic.Settings{Mode: traffic.Out, ResetDay: 1, LimitBytes: 1024 * mib}); err != nil {
		t.Fatal(err)
	}
	r.addTraffic(id, date(2026, 10, 10), 4096*mib, 1000*mib)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("outbound under the limit: sent %q", got)
	}
	r.addTraffic(id, date(2026, 10, 11), 0, 24*mib)
	if got := r.check(); len(got) != 1 {
		t.Fatalf("outbound at the limit: sent %q, want one message", got)
	}
}

func TestNoLimitNoTrafficMessage(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	r.addTraffic(id, date(2026, 10, 10), 1<<50, 1<<50)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("no limit: sent %q", got)
	}
}

// report sends a sample with the given disk, memory and CPU usage in
// percent, taken now.
func (r *rig) report(id int64, disk, mem, cpu float64) {
	total := uint64(1000)
	du, mu := uint64(disk*10), uint64(mem*10)
	r.status.Report(id, status.Sample{At: r.now, DiskUsed: &du, DiskTotal: &total, MemUsed: &mu, MemTotal: &total, CPU: &cpu})
}

// minute moves the clock on by a minute, reports and checks.
func (r *rig) minute(id int64, disk, mem, cpu float64) []string {
	r.t.Helper()
	r.advance(time.Minute)
	r.report(id, disk, mem, cpu)
	return r.check()
}

func TestDiskOverThreshold(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	r.report(id, 92, 10, 5)
	if got := r.check(); len(got) != 0 {
		t.Fatalf("first sample over: sent %q", got)
	}
	for i := 1; i < 5; i++ {
		if got := r.minute(id, 92, 10, 5); len(got) != 0 {
			t.Fatalf("over for %d min: sent %q", i, got)
		}
	}
	want := []string{"[Kerge] web-1 (203.0.113.10) disk usage 92.0% (≥ 90% for 5 min)"}
	if got := r.minute(id, 92, 10, 5); !equal(got, want) {
		t.Fatalf("over for 5 min: sent %q, want %q", got, want)
	}
	if got := r.minute(id, 95, 10, 5); len(got) != 0 {
		t.Fatalf("still over: sent %q again", got)
	}
	// Back under: reported once it has stayed under for 5 minutes too.
	for i := 0; i < 5; i++ {
		if got := r.minute(id, 85, 10, 5); len(got) != 0 {
			t.Fatalf("under for %d min: sent %q", i, got)
		}
	}
	want = []string{"[Kerge] web-1 (203.0.113.10) disk usage back to 85.0%"}
	if got := r.minute(id, 85, 10, 5); !equal(got, want) {
		t.Fatalf("under for 5 min: sent %q, want %q", got, want)
	}
	if got := r.minute(id, 85, 10, 5); len(got) != 0 {
		t.Fatalf("still under: sent %q again", got)
	}
}

func TestADipStartsTheMinutesOver(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	r.report(id, 95, 10, 5)
	r.check()
	for i := 0; i < 3; i++ {
		r.minute(id, 95, 10, 5)
	}
	r.minute(id, 50, 10, 5) // one sample under
	// The minutes count from the first sample over again.
	for i := 0; i < 5; i++ {
		if got := r.minute(id, 95, 10, 5); len(got) != 0 {
			t.Fatalf("over again for %d min: sent %q", i, got)
		}
	}
	if got := r.minute(id, 95, 10, 5); len(got) != 1 {
		t.Fatalf("over again for 5 min: sent %q, want one message", got)
	}
}

func TestUsageNeedsAnOnlineHost(t *testing.T) {
	r := newRig(t)
	r.onlyDateEvents() // the offline message is not what this is about
	id := r.addHost("web-1", "203.0.113.10")
	disconnect := r.status.Connected(id, 5*time.Second, nil)
	r.report(id, 95, 10, 5)
	r.check()
	for i := 0; i < 3; i++ {
		r.minute(id, 95, 10, 5)
	}
	// Offline for a while: the last sample does not keep counting.
	disconnect()
	for i := 0; i < 10; i++ {
		r.advance(time.Minute)
		if got := r.check(); len(got) != 0 {
			t.Fatalf("offline: sent %q", got)
		}
	}
	r.status.Connected(id, 5*time.Second, nil)
	for i := 0; i < 5; i++ {
		if got := r.minute(id, 95, 10, 5); len(got) != 0 {
			t.Fatalf("back and over for %d min: sent %q", i, got)
		}
	}
	if got := r.minute(id, 95, 10, 5); len(got) != 1 {
		t.Fatalf("back and over for 5 min: sent %q, want one message", got)
	}
}

func TestMemoryAndCPU(t *testing.T) {
	r := newRig(t)
	cfg, err := r.settings.Notifications(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Disk.On = false
	cfg.Memory = notify.Threshold{On: true, Percent: 80, Minutes: 2}
	cfg.CPU = notify.Threshold{On: true, Percent: 75, Minutes: 3}
	r.configure(cfg)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	r.report(id, 99, 85, 80)
	r.check()
	r.minute(id, 99, 85, 80)
	want := []string{"[Kerge] web-1 (203.0.113.10) memory usage 85.0% (≥ 80% for 2 min)"}
	if got := r.minute(id, 99, 85, 80); !equal(got, want) {
		t.Fatalf("memory: sent %q, want %q", got, want)
	}
	want = []string{"[Kerge] web-1 (203.0.113.10) CPU usage 80.0% (≥ 75% for 3 min)"}
	if got := r.minute(id, 99, 85, 80); !equal(got, want) {
		t.Fatalf("cpu: sent %q, want %q", got, want)
	}
	r.minute(id, 99, 10, 10) // under from here
	r.minute(id, 99, 10, 10)
	want = []string{"[Kerge] web-1 (203.0.113.10) memory usage back to 10.0%"}
	if got := r.minute(id, 99, 10, 10); !equal(got, want) {
		t.Fatalf("memory back: sent %q, want %q", got, want)
	}
	want = []string{"[Kerge] web-1 (203.0.113.10) CPU usage back to 10.0%"}
	if got := r.minute(id, 99, 10, 10); !equal(got, want) {
		t.Fatalf("cpu back: sent %q, want %q", got, want)
	}
}

func TestUsageSwitchedOffIsTrackedButNotSent(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	// CPU is off by default.
	r.report(id, 10, 10, 99)
	r.check()
	for i := 0; i < 15; i++ {
		if got := r.minute(id, 10, 10, 99); len(got) != 0 {
			t.Fatalf("cpu switched off: sent %q", got)
		}
	}
	// Switched on during the episode: its start is not reported, its end
	// is.
	cfg, err := r.settings.Notifications(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cfg.CPU.On = true
	r.configure(cfg)
	if got := r.minute(id, 10, 10, 99); len(got) != 0 {
		t.Fatalf("switched on while over: sent %q", got)
	}
	// The default duration is 10 minutes, counted from the first sample
	// under.
	for i := 0; i < 10; i++ {
		if got := r.minute(id, 10, 10, 20); len(got) != 0 {
			t.Fatalf("under for %d min: sent %q", i, got)
		}
	}
	want := []string{"[Kerge] web-1 (203.0.113.10) CPU usage back to 20.0%"}
	if got := r.minute(id, 10, 10, 20); !equal(got, want) {
		t.Fatalf("under for 10 min: sent %q, want %q", got, want)
	}
	if n := countAlerts(t, r, "cpu"); n != 0 {
		t.Fatalf("cpu row still there: %d", n)
	}
}

func TestUsageAcrossARestart(t *testing.T) {
	r := newRig(t)
	id := r.addHost("web-1", "203.0.113.10")
	r.status.Connected(id, 5*time.Second, nil)
	r.report(id, 95, 10, 5)
	r.check()
	for i := 0; i < 5; i++ {
		r.minute(id, 95, 10, 5)
	}
	if n := countAlerts(t, r, "disk"); n != 1 {
		t.Fatalf("disk rows = %d, want 1", n)
	}
	// A new watcher, as after a restart: still over, nothing is sent again.
	r.watcher = New(r.watcher.opts)
	for i := 0; i < 10; i++ {
		if got := r.minute(id, 95, 10, 5); len(got) != 0 {
			t.Fatalf("after a restart: sent %q again", got)
		}
	}
}

func countAlerts(t *testing.T, r *rig, kind string) int {
	t.Helper()
	var n int
	if err := r.db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM alerts WHERE kind = ?", kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
