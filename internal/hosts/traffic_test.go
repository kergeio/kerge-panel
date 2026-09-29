package hosts

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-panel/internal/traffic"
	"github.com/kergeio/kerge-protocol/ifacefilter"
)

// A host that never saved traffic settings reads with the defaults; saved
// settings read back as saved.
func TestTrafficSettings(t *testing.T) {
	s, _ := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Traffic != traffic.Default() {
		t.Fatalf("defaults: %+v", rec.Traffic)
	}

	want := traffic.Settings{Mode: traffic.In, ResetDay: 28, LimitBytes: 5 << 30}
	if err := s.SetTraffic(ctx, id, want); err != nil {
		t.Fatal(err)
	}
	if rec, _ = s.Get(ctx, id); rec.Traffic != want {
		t.Fatalf("saved: %+v", rec.Traffic)
	}
	want.LimitBytes = 0
	if err := s.SetTraffic(ctx, id, want); err != nil {
		t.Fatal(err)
	}
	if rec, _ = s.Get(ctx, id); rec.Traffic != want {
		t.Fatalf("without a limit: %+v", rec.Traffic)
	}

	for _, bad := range []traffic.Settings{
		{Mode: "sideways", ResetDay: 1},
		{Mode: traffic.Both, ResetDay: 29},
		{Mode: traffic.Both, ResetDay: 1, LimitBytes: -1},
	} {
		if err := s.SetTraffic(ctx, id, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := s.SetTraffic(ctx, id+1, traffic.Default()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown host: %v", err)
	}
}

func TestDailyTraffic(t *testing.T) {
	s, db := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	put := func(day string, rx, tx int64) {
		t.Helper()
		if err := db.Write(ctx, func(ctx context.Context, tx2 *sql.Tx) error {
			_, err := tx2.ExecContext(ctx,
				"INSERT INTO traffic_daily (host_id, day, rx_bytes, tx_bytes) VALUES (?, ?, ?, ?)", id, day, rx, tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("2026-08-31", 1, 2)
	put("2026-09-01", 3, 4)
	put("2026-09-23", 5, 6)

	days, err := s.DailyTraffic(ctx, day(t, "2026-09-01"))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %+v", days)
	}
	if rx, tx := traffic.Sum(days, id, day(t, "2026-09-01")); rx != 8 || tx != 10 {
		t.Fatalf("sum = %d %d", rx, tx)
	}

	put("2026-13-01", 1, 1)
	if _, err := s.DailyTraffic(ctx, day(t, "2026-09-01")); err == nil {
		t.Error("a malformed day was accepted")
	}
}

// Deleting a host deletes its traffic with it.
func TestDeleteRemovesTraffic(t *testing.T) {
	s, db := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO traffic_daily (host_id, day, rx_bytes, tx_bytes) VALUES (?, '2026-09-23', 1, 1)", id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO traffic_state
			(host_id, iface, last_rx_bytes, last_tx_bytes, last_mono_ms, last_ts) VALUES (?, 'eth0', 1, 1, 1, 1)`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"traffic_daily", "traffic_state"} {
		var n int
		if err := db.Read().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s holds %d rows after the delete", table, n)
		}
	}
}

// A host's own exclusion list replaces the panel's, an empty one excludes
// nothing, and nil puts the host back on the panel's rules; what ingest
// applies follows.
func TestSetIfaceExclude(t *testing.T) {
	s, db := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	applied := func() string {
		t.Helper()
		patterns, err := s.IfaceExclude(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(patterns, ",")
	}
	if rec, _ := s.Get(ctx, id); rec.IfaceExclude != nil {
		t.Fatalf("new host has its own list: %q", *rec.IfaceExclude)
	}
	if got := applied(); got != strings.Join(ifacefilter.DefaultExclude, ",") {
		t.Fatalf("new host applies %q", got)
	}
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return store.SetSetting(ctx, tx, store.SettingNetIfaceExclude, "lo,eth9")
	}); err != nil {
		t.Fatal(err)
	}
	if got := applied(); got != "lo,eth9" {
		t.Fatalf("following the panel: %q", got)
	}

	own := " lo, docker* "
	if err := s.SetIfaceExclude(ctx, id, &own); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.Get(ctx, id); rec.IfaceExclude == nil || *rec.IfaceExclude != "lo, docker*" {
		t.Fatalf("stored: %v", rec.IfaceExclude)
	}
	if got := applied(); got != "lo,docker*" {
		t.Fatalf("own list: %q", got)
	}

	empty := ""
	if err := s.SetIfaceExclude(ctx, id, &empty); err != nil {
		t.Fatal(err)
	}
	if got := applied(); got != "" {
		t.Fatalf("empty own list: %q", got)
	}

	if err := s.SetIfaceExclude(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.Get(ctx, id); rec.IfaceExclude != nil {
		t.Fatalf("back to the panel's rules, still stored: %q", *rec.IfaceExclude)
	}
	if got := applied(); got != "lo,eth9" {
		t.Fatalf("back to the panel's rules: %q", got)
	}

	bad := "eth0,,eth1"
	if err := s.SetIfaceExclude(ctx, id, &bad); err == nil {
		t.Error("an invalid list was accepted")
	}
	if err := s.SetIfaceExclude(ctx, id+1, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown host: %v", err)
	}
}
