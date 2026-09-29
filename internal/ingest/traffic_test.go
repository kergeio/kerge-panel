package ingest

import (
	"context"
	"database/sql"
	"log/slog"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/kergeio/kerge-protocol"
)

type dayTotal struct{ rx, tx int64 }

// daily returns the host's traffic per day.
func (f *fixture) daily() map[string]dayTotal {
	f.t.Helper()
	rows, err := f.db.Read().QueryContext(f.t.Context(),
		"SELECT day, rx_bytes, tx_bytes FROM traffic_daily WHERE host_id = ?", f.hostID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]dayTotal{}
	for rows.Next() {
		var day string
		var d dayTotal
		if err := rows.Scan(&day, &d.rx, &d.tx); err != nil {
			f.t.Fatal(err)
		}
		out[day] = d
	}
	return out
}

// saved returns the counters the panel keeps for one interface.
func (f *fixture) saved(name string) (protocol.NetCounters, bool) {
	f.t.Helper()
	var rx, tx int64
	err := f.db.Read().QueryRowContext(f.t.Context(),
		"SELECT last_rx_bytes, last_tx_bytes FROM traffic_state WHERE host_id = ? AND iface = ?",
		f.hostID, name).Scan(&rx, &tx)
	if err == sql.ErrNoRows {
		return protocol.NetCounters{}, false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return protocol.NetCounters{RX: uint64(rx), TX: uint64(tx)}, true
}

// The first report only records the counters; later ones add what the
// counted interfaces carried to the day, a reset counter adds its new
// value, and nothing is ever negative.
func TestTrafficAccumulates(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	day := f.now.UTC().Format("2006-01-02")

	f.send(conn, sample(1000, iface(1_000_000, 2_000_000)))
	f.waitRows(1)
	if got := f.daily(); len(got) != 0 {
		t.Fatalf("the first report counted traffic: %v", got)
	}
	if c, ok := f.saved("eth0"); !ok || c.RX != 1_000_000 || c.TX != 2_000_000 {
		t.Fatalf("baseline = %+v %v", c, ok)
	}

	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(6000, iface(1_000_500, 2_000_700)))
	f.waitRows(2)
	if got := f.daily()[day]; got != (dayTotal{500, 700}) {
		t.Fatalf("after one step: %+v", got)
	}

	// The host rebooted: its counters start again from zero.
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(11000, iface(300, 400)))
	f.waitRows(3)
	if got := f.daily()[day]; got != (dayTotal{800, 1100}) {
		t.Fatalf("after a reboot: %+v", got)
	}

	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(16000, iface(1300, 1400)))
	f.waitRows(4)
	if got := f.daily()[day]; got != (dayTotal{1800, 2100}) {
		t.Fatalf("after the reboot: %+v", got)
	}
}

// The counters live in the database, so traffic carried while the panel
// was down is counted when the agent reports again, and a restarted panel
// continues rather than starting over.
func TestTrafficSurvivesAPanelRestart(t *testing.T) {
	f := newFixture(t)
	conn, credential := f.enroll()
	day := f.now.UTC().Format("2006-01-02")
	f.send(conn, sample(1000, iface(1000, 1000)))
	f.waitRows(1)
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(6000, iface(2000, 3000)))
	f.waitRows(2)
	conn.CloseNow()

	// A new panel process on the same database.
	f.svc.Close()
	restarted, err := New(Options{
		DB: f.db, Hosts: f.hosts, Logger: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	f.svc = restarted
	handler := restarted.Handler()
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	})

	next, _, err := f.dial(protocol.AgentAuthorization(credential))
	if err != nil {
		t.Fatal(err)
	}
	defer next.CloseNow()
	f.now = f.now.Add(10 * time.Minute)
	f.send(next, sample(600_000, iface(9000, 4000)))
	f.waitRows(3)
	if got := f.daily()[day]; got != (dayTotal{1000 + 7000, 2000 + 1000}) {
		t.Fatalf("after the restart: %+v", got)
	}
}

// Traffic is counted on the day in the panel's time zone.
func TestTrafficDayFollowsThePanelTimeZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	f := newFixture(t)
	f.svc.opts.TimeZone = func() *time.Location { return tokyo }
	// 20:00 UTC is already the next day in Tokyo.
	f.now = time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	conn, _ := f.enroll()
	f.send(conn, sample(1000, iface(0, 0)))
	f.waitRows(1)
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(6000, iface(10, 20)))
	f.waitRows(2)
	got := f.daily()
	if len(got) != 1 || got["2026-09-23"] != (dayTotal{10, 20}) {
		t.Fatalf("days = %v, want the traffic on 2026-09-23", got)
	}
}

// Excluded interfaces add nothing but keep their counters, so counting
// them later starts from where they are instead of from zero; an interface
// that disappears and returns is measured against its saved counters.
func TestTrafficPerInterface(t *testing.T) {
	f := newFixture(t)
	setExclude := func(v any) {
		t.Helper()
		if err := f.db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE hosts SET net_iface_exclude = ? WHERE id = ?", v, f.hostID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setExclude("docker*")
	conn, _ := f.enroll()
	day := f.now.UTC().Format("2006-01-02")
	two := func(eth, docker uint64) map[string]protocol.NetCounters {
		return map[string]protocol.NetCounters{"eth0": {RX: eth, TX: eth}, "docker0": {RX: docker, TX: docker}}
	}
	step := func(mono int64, net map[string]protocol.NetCounters, rows int) {
		t.Helper()
		f.now = f.now.Add(5 * time.Second)
		f.send(conn, sample(mono, net))
		f.waitRows(rows)
	}

	step(1000, two(0, 0), 1)
	step(6000, two(100, 50_000), 2)
	if got := f.daily()[day]; got != (dayTotal{100, 100}) {
		t.Fatalf("with docker0 excluded: %+v", got)
	}
	if c, ok := f.saved("docker0"); !ok || c.RX != 50_000 {
		t.Fatalf("docker0 counters were not kept: %+v %v", c, ok)
	}

	// Counting docker0 from now on does not bring its past traffic along.
	setExclude("")
	step(11000, two(200, 50_010), 3)
	if got := f.daily()[day]; got != (dayTotal{210, 210}) {
		t.Fatalf("after docker0 was included: %+v", got)
	}

	// eth0 disappears for a while and comes back.
	step(16000, map[string]protocol.NetCounters{"docker0": {RX: 50_020, TX: 50_020}}, 4)
	if _, ok := f.saved("eth0"); !ok {
		t.Fatal("the counters of a vanished interface were dropped")
	}
	step(21000, two(260, 50_020), 5)
	if got := f.daily()[day]; got != (dayTotal{280, 280}) {
		t.Fatalf("after eth0 returned: %+v", got)
	}
}

// Counters above the largest INTEGER survive the round trip through the
// database, and day totals stop at the largest INTEGER.
func TestTrafficExtremeCounters(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	day := f.now.UTC().Format("2006-01-02")
	f.send(conn, sample(1000, iface(math.MaxUint64-100, 0)))
	f.waitRows(1)
	if c, _ := f.saved("eth0"); c.RX != math.MaxUint64-100 {
		t.Fatalf("saved rx = %d", c.RX)
	}
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(6000, iface(math.MaxUint64, math.MaxUint64)))
	f.waitRows(2)
	if got := f.daily()[day]; got != (dayTotal{100, math.MaxInt64}) {
		t.Fatalf("day = %+v", got)
	}
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(11000, iface(math.MaxUint64, math.MaxUint64)))
	f.waitRows(3)
	f.now = f.now.Add(5 * time.Second)
	f.send(conn, sample(16000, iface(5, 5)))
	f.waitRows(4)
	if got := f.daily()[day]; got != (dayTotal{105, math.MaxInt64}) {
		t.Fatalf("day after more traffic = %+v", got)
	}
}

// A sample without network counters leaves the traffic alone.
func TestTrafficWithoutCounters(t *testing.T) {
	f := newFixture(t)
	conn, _ := f.enroll()
	f.send(conn, sample(1000, nil))
	f.waitRows(1)
	if got := f.daily(); len(got) != 0 {
		t.Fatalf("days = %v", got)
	}
	var n int
	if err := f.db.Read().QueryRowContext(t.Context(), "SELECT count(*) FROM traffic_state").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("traffic_state holds %d rows", n)
	}
}
