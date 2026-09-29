package rollup

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/store"
)

const minute = 60

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "kerge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// clock is a hand-wound clock the service reads through Options.Now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) set(unix int64)      { c.t = time.Unix(unix, 0) }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newService(t *testing.T, db *store.DB, c *clock) *Service {
	t.Helper()
	s, err := New(Options{
		DB:     db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    c.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func addHost(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	var id int64
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO hosts (name, created_at) VALUES (?, ?)", name, 0)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// sample is one raw row; the fields not under test are left at zero.
type sample struct {
	ts     int64
	cpu    float64
	mem    int64
	rxRate float64
	uptime int64
}

func addSamples(t *testing.T, db *store.DB, hostID int64, samples ...sample) {
	t.Helper()
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		for _, s := range samples {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO metrics_raw (host_id, ts, cpu, mem_used, mem_total,
					swap_used, swap_total, load1, load5, load15,
					disk_used, disk_total, disk_free, rx_rate, tx_rate, uptime)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				hostID, s.ts, s.cpu, s.mem, s.mem*2, 0, 0, 1.0, 1.0, 1.0,
				100, 200, 100, s.rxRate, s.rxRate/2, s.uptime)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// bucket is a summarized row, read back from metrics_1m or metrics_1h.
type bucket struct {
	ts         int64
	cpu        float64
	cpuMax     float64
	memUsed    int64
	memUsedMax int64
	rxRate     float64
	rxRateMax  float64
	uptime     sql.NullInt64
}

func buckets(t *testing.T, db *store.DB, table string, hostID int64) []bucket {
	t.Helper()
	rows, err := db.Read().QueryContext(t.Context(),
		"SELECT ts, cpu, cpu_max, mem_used, mem_used_max, rx_rate, rx_rate_max, uptime FROM "+table+
			" WHERE host_id = ? ORDER BY ts", hostID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.ts, &b.cpu, &b.cpuMax, &b.memUsed, &b.memUsedMax,
			&b.rxRate, &b.rxRateMax, &b.uptime); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func count(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	var n int
	if err := db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setSetting(t *testing.T, db *store.DB, key, value string) {
	t.Helper()
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return store.SetSetting(ctx, tx, key, value)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func getSetting(t *testing.T, db *store.DB, key string) (string, bool) {
	t.Helper()
	v, ok, err := db.GetSetting(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return v, ok
}

func runOnce(t *testing.T, s *Service) {
	t.Helper()
	if err := s.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMinuteBucketAveragesAndPeaks(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	c.set(10 * minute)
	host := addHost(t, db, "web-1")
	// Three samples in the first minute, one in the second.
	addSamples(t, db, host,
		sample{ts: 0, cpu: 10, mem: 100, rxRate: 1000, uptime: 60},
		sample{ts: 20, cpu: 50, mem: 200, rxRate: 3000, uptime: 80},
		sample{ts: 40, cpu: 30, mem: 300, rxRate: 2000, uptime: 100},
		sample{ts: 90, cpu: 70, mem: 400, rxRate: 4000, uptime: 150},
	)
	runOnce(t, newService(t, db, c))

	got := buckets(t, db, "metrics_1m", host)
	if len(got) != 2 {
		t.Fatalf("metrics_1m has %d rows, want 2: %+v", len(got), got)
	}
	first := got[0]
	switch {
	case first.ts != 0:
		t.Errorf("first bucket starts at %d, want 0", first.ts)
	case first.cpu != 30: // (10+50+30)/3
		t.Errorf("cpu = %v, want the average 30", first.cpu)
	case first.cpuMax != 50:
		t.Errorf("cpu_max = %v, want 50", first.cpuMax)
	case first.memUsed != 200:
		t.Errorf("mem_used = %d, want the average 200", first.memUsed)
	case first.memUsedMax != 300:
		// Memory is a chart of its own, so the window can be read as
		// maxima too.
		t.Errorf("mem_used_max = %d, want the busiest sample 300", first.memUsedMax)
	case first.rxRate != 2000:
		t.Errorf("rx_rate = %v, want the average 2000", first.rxRate)
	case first.rxRateMax != 3000:
		t.Errorf("rx_rate_max = %v, want 3000", first.rxRateMax)
	case !first.uptime.Valid || first.uptime.Int64 != 100:
		// Uptime is a reading, not a rate: the last one in the bucket
		// wins, and averaging it would be meaningless.
		t.Errorf("uptime = %v, want the last value 100", first.uptime)
	}
	if second := got[1]; second.ts != 60 || second.cpu != 70 || second.uptime.Int64 != 150 {
		t.Errorf("second bucket = %+v, want ts 60, cpu 70, uptime 150", second)
	}
}

func TestUptimeIgnoresMissingValues(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	c.set(10 * minute)
	host := addHost(t, db, "web-1")
	addSamples(t, db, host, sample{ts: 0, uptime: 42})
	// A later sample in the same bucket that carries no uptime at all.
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO metrics_raw (host_id, ts, cpu) VALUES (?, ?, ?)", host, 30, 5.0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, newService(t, db, c))

	got := buckets(t, db, "metrics_1m", host)
	if len(got) != 1 || !got[0].uptime.Valid || got[0].uptime.Int64 != 42 {
		t.Fatalf("metrics_1m = %+v, want one row whose uptime is 42", got)
	}
}

func TestIncompleteBucketIsLeftAlone(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	c.set(90) // half way through the second minute
	host := addHost(t, db, "web-1")
	addSamples(t, db, host,
		sample{ts: 10, cpu: 10, uptime: 10},
		sample{ts: 80, cpu: 90, uptime: 80}, // the minute still running
	)
	s := newService(t, db, c)
	runOnce(t, s)

	got := buckets(t, db, "metrics_1m", host)
	if len(got) != 1 || got[0].ts != 0 {
		t.Fatalf("metrics_1m = %+v, want only the finished minute", got)
	}
	// Once that minute is over it is summarized, and the sample taken
	// meanwhile is included.
	addSamples(t, db, host, sample{ts: 100, cpu: 70, uptime: 100})
	c.set(3 * minute)
	runOnce(t, s)
	got = buckets(t, db, "metrics_1m", host)
	if len(got) != 2 || got[1].ts != 60 || got[1].cpu != 80 { // (90+70)/2
		t.Fatalf("metrics_1m = %+v, want the second minute averaged over both samples", got)
	}
}

func TestHourBucketSummarizesMinutes(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	c.set(2 * 3600)
	host := addHost(t, db, "web-1")
	// Two samples a minute for the first hour and a bit, so that a
	// minute's average and its maximum differ.
	var samples []sample
	for i := range int64(70) {
		samples = append(samples,
			sample{ts: i * minute, cpu: float64(i), mem: 100, rxRate: float64(i) * 10, uptime: i * minute},
			sample{ts: i*minute + 30, cpu: float64(i) + 5, mem: 300, rxRate: float64(i)*10 + 100, uptime: i*minute + 30},
		)
	}
	addSamples(t, db, host, samples...)
	s := newService(t, db, c)
	runOnce(t, s)
	// The second hour waits: the minute stage has stopped at the last
	// sample, and until it is known to have passed the hour boundary an
	// hour could still be filling up.
	if n := count(t, db, "metrics_1h"); n != 1 {
		t.Fatalf("metrics_1h has %d rows after one pass, want the first hour only", n)
	}
	runOnce(t, s)

	got := buckets(t, db, "metrics_1h", host)
	if len(got) != 2 {
		t.Fatalf("metrics_1h has %d rows, want 2: %+v", len(got), got)
	}
	first := got[0]
	switch {
	case first.ts != 0:
		t.Errorf("first hour starts at %d, want 0", first.ts)
	case first.cpu != 32: // the mean of the minute averages, 2.5 .. 61.5
		t.Errorf("cpu = %v, want 32", first.cpu)
	case first.cpuMax != 64:
		// The maximum comes from the minute maxima, not their averages,
		// so a spike inside a minute survives into the hour.
		t.Errorf("cpu_max = %v, want the busiest sample 64", first.cpuMax)
	case first.rxRate != 345:
		t.Errorf("rx_rate = %v, want 345", first.rxRate)
	case first.rxRateMax != 690:
		t.Errorf("rx_rate_max = %v, want 690", first.rxRateMax)
	case first.memUsed != 200 || first.memUsedMax != 300:
		// Memory's maximum comes from the minute maxima, so a spike
		// inside a minute survives into the hour just as cpu's does.
		t.Errorf("mem_used = %d / max = %d, want the average 200 and the peak 300",
			first.memUsed, first.memUsedMax)
	case first.uptime.Int64 != 59*minute+30:
		t.Errorf("uptime = %v, want the last sample's value %d", first.uptime, 59*minute+30)
	}
}

// TestHourWaitsForTheMinuteStage guards the rule that an hour is only
// written once every minute in it has been summarized.
func TestHourWaitsForTheMinuteStage(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	host := addHost(t, db, "web-1")
	// Two hours of samples, but the minute stage has only reached the
	// middle of the first hour.
	var samples []sample
	for i := range int64(120) {
		samples = append(samples, sample{ts: i * minute, cpu: 1, uptime: i})
	}
	addSamples(t, db, host, samples...)
	c.set(3 * 3600)
	s := newService(t, db, c)
	s.minute.maxBuckets = 30 // half an hour per pass
	runOnce(t, s)
	if n := count(t, db, "metrics_1h"); n != 0 {
		t.Fatalf("metrics_1h has %d rows after half an hour was summarized, want 0", n)
	}
	// Two more passes finish the first hour, which may then be summarized.
	runOnce(t, s)
	runOnce(t, s)
	if got := buckets(t, db, "metrics_1h", host); len(got) != 1 || got[0].ts != 0 {
		t.Fatalf("metrics_1h = %+v, want the first hour only", got)
	}
}

func TestCatchUpAfterTheGapOfAStoppedPanel(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	host := addHost(t, db, "web-1")
	// Ten minutes of samples, then nothing for two days: the panel was
	// stopped and has just started again.
	var samples []sample
	for i := range int64(10) {
		samples = append(samples, sample{ts: i * minute, cpu: float64(i), uptime: i})
	}
	addSamples(t, db, host, samples...)
	c.set(2 * 86400)
	s := newService(t, db, c)
	runOnce(t, s)

	if got := buckets(t, db, "metrics_1m", host); len(got) != 10 {
		t.Fatalf("metrics_1m has %d rows, want the 10 minutes recorded before the stop", len(got))
	}
	// One pass covers the whole backlog: the watermark stops at the end
	// of the recorded data instead of walking the empty gap bucket by
	// bucket.
	if v, ok := getSetting(t, db, store.SettingRollupMinuteThrough); !ok || v != "600" {
		t.Errorf("minute watermark = %q, %v; want 600", v, ok)
	}
}

// TestLongCatchUpAdvancesInPasses checks the cap on one pass: a backlog
// larger than maxBuckets is summarized over several runs rather than in
// one long transaction.
func TestLongCatchUpAdvancesInPasses(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	host := addHost(t, db, "web-1")
	var samples []sample
	for i := range int64(10) {
		samples = append(samples, sample{ts: i * minute, cpu: 1, uptime: i})
	}
	addSamples(t, db, host, samples...)
	c.set(20 * minute)
	s := newService(t, db, c)
	s.minute.maxBuckets = 4

	runOnce(t, s)
	if n := count(t, db, "metrics_1m"); n != 4 {
		t.Fatalf("metrics_1m has %d rows after one pass, want 4", n)
	}
	runOnce(t, s)
	runOnce(t, s)
	if n := count(t, db, "metrics_1m"); n != 10 {
		t.Fatalf("metrics_1m has %d rows after three passes, want all 10", n)
	}
}

func TestRetentionDeletesOldRows(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	host := addHost(t, db, "web-1")
	now := int64(100 * 86400)
	c.set(now)
	// One raw sample inside the retention window and one outside it, plus
	// rows in the summarized tables on either side of their cut-offs.
	addSamples(t, db, host,
		sample{ts: now - 30*3600, cpu: 1, uptime: 1}, // older than 24 hours
		sample{ts: now - 600, cpu: 2, uptime: 2},
	)
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		for _, row := range []struct {
			table string
			ts    int64
		}{
			{"metrics_1m", now - 8*86400}, // older than 7 days
			{"metrics_1m", now - 86400},
			{"metrics_1h", now - 100*86400}, // older than 90 days
			{"metrics_1h", now - 86400},
		} {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO "+row.table+" (host_id, ts, cpu) VALUES (?, ?, 1.0)", host, row.ts); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pretend both stages have already covered everything, so retention
	// alone decides what goes.
	setSetting(t, db, store.SettingRollupMinuteThrough, "8640000")
	setSetting(t, db, store.SettingRollupHourThrough, "8640000")
	runOnce(t, newService(t, db, c))

	if n := count(t, db, "metrics_raw"); n != 1 {
		t.Errorf("metrics_raw has %d rows, want only the recent sample", n)
	}
	for _, table := range []string{"metrics_1m", "metrics_1h"} {
		if n := count(t, db, table); n != 1 {
			t.Errorf("%s has %d rows, want only the recent one", table, n)
		}
	}
}

// TestPruningWaitsForSummarizing is the guard that keeps a slow stage from
// losing data: raw rows past the retention window survive while the minute
// stage has not covered them yet.
func TestPruningWaitsForSummarizing(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	host := addHost(t, db, "web-1")
	// An hour of samples, every one of them older than the 24 hour raw
	// retention, and a stage that only covers two minutes per pass.
	var samples []sample
	for i := range int64(60) {
		samples = append(samples, sample{ts: i * minute, cpu: 1, uptime: i})
	}
	addSamples(t, db, host, samples...)
	c.set(2 * 86400)
	s := newService(t, db, c)
	s.minute.maxBuckets = 2
	runOnce(t, s)

	if n := count(t, db, "metrics_1m"); n != 2 {
		t.Fatalf("metrics_1m has %d rows after one pass, want 2", n)
	}
	if n := count(t, db, "metrics_raw"); n != 58 {
		t.Fatalf("metrics_raw has %d rows, want the 58 minutes not summarized yet", n)
	}
	// Once everything has been summarized the raw rows may go.
	for range 30 {
		runOnce(t, s)
	}
	if n := count(t, db, "metrics_1m"); n != 60 {
		t.Fatalf("metrics_1m has %d rows, want 60", n)
	}
	if n := count(t, db, "metrics_raw"); n != 0 {
		t.Errorf("metrics_raw has %d rows, want none left", n)
	}
}

// TestPruningDeletesMoreRowsThanOneBatch covers a backlog larger than one
// pruning write: every expired row goes within the pass, no write deletes
// more than a batch, and the rows still inside the window stay.
func TestPruningDeletesMoreRowsThanOneBatch(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	hosts := []int64{addHost(t, db, "web-1"), addHost(t, db, "web-2")}
	now := int64(100 * 86400)
	c.set(now)
	// Per host and table: five expired rows and two recent ones, so ten
	// expired rows per table against a batch of three.
	cutoffs := map[string]int64{
		"metrics_raw": now - 24*3600,
		"metrics_1m":  now - 7*86400,
		"metrics_1h":  now - 90*86400,
	}
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		for table, cutoff := range cutoffs {
			for _, host := range hosts {
				for i := range int64(7) {
					ts := cutoff - 5*minute + i*minute
					if _, err := tx.ExecContext(ctx,
						"INSERT INTO "+table+" (host_id, ts, cpu) VALUES (?, ?, 1.0)", host, ts); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both stages have covered everything, so retention alone decides.
	setSetting(t, db, store.SettingRollupMinuteThrough, "8640000")
	setSetting(t, db, store.SettingRollupHourThrough, "8640000")

	s := newService(t, db, c)
	s.pruneBatch = 3
	writes := map[string][]int64{}
	s.pruneHook = func(table string, deleted int64) {
		writes[table] = append(writes[table], deleted)
	}
	runOnce(t, s)

	for table := range cutoffs {
		if n := count(t, db, table); n != 4 {
			t.Errorf("%s has %d rows, want the 4 recent ones", table, n)
		}
		var total int64
		for _, d := range writes[table] {
			if d > 3 {
				t.Errorf("%s: one write deleted %d rows, want at most 3", table, d)
			}
			total += d
		}
		if total != 10 || len(writes[table]) != 4 {
			t.Errorf("%s: writes deleted %v, want 10 rows in 4 writes", table, writes[table])
		}
	}
}

func TestEmptyDatabaseIsNoWork(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	c.set(5 * 3600)
	runOnce(t, newService(t, db, c))

	if v, ok := getSetting(t, db, store.SettingRollupMinuteThrough); !ok || v != "18000" {
		t.Errorf("minute watermark = %q, %v; want 18000", v, ok)
	}
	if n := count(t, db, "metrics_1m"); n != 0 {
		t.Errorf("metrics_1m has %d rows, want none", n)
	}
}

func TestDamagedWatermarkStartsFromTheOldestRow(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	host := addHost(t, db, "web-1")
	addSamples(t, db, host, sample{ts: 0, cpu: 5, uptime: 1})
	c.set(10 * minute)
	setSetting(t, db, store.SettingRollupMinuteThrough, "not a number")
	runOnce(t, newService(t, db, c))

	if n := count(t, db, "metrics_1m"); n != 1 {
		t.Errorf("metrics_1m has %d rows, want the one minute of samples", n)
	}
}

func TestSummarizingIsIdempotent(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	host := addHost(t, db, "web-1")
	addSamples(t, db, host,
		sample{ts: 0, cpu: 10, uptime: 1},
		sample{ts: 30, cpu: 20, uptime: 2},
	)
	c.set(10 * minute)
	s := newService(t, db, c)
	runOnce(t, s)
	// Rewind the watermark: the same minute is summarized a second time.
	setSetting(t, db, store.SettingRollupMinuteThrough, "0")
	runOnce(t, s)

	got := buckets(t, db, "metrics_1m", host)
	if len(got) != 1 || got[0].cpu != 15 {
		t.Fatalf("metrics_1m = %+v, want one row whose cpu is 15", got)
	}
}

func TestOptimizeRunsOnSchedule(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	c.set(86400)
	s, err := New(Options{
		DB:            db,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           c.now,
		OptimizeEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := s.lastOptimize
	runOnce(t, s)
	if !s.lastOptimize.Equal(before) {
		t.Error("optimize ran on the first pass, want it held back until the interval has passed")
	}
	c.add(time.Hour)
	runOnce(t, s)
	if s.lastOptimize.Equal(before) {
		t.Error("optimize did not run once the interval had passed")
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	c.set(60)
	s := newService(t, db, c)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestSeveralHostsAreSummarizedApart(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	c.set(10 * minute)
	a := addHost(t, db, "web-1")
	b := addHost(t, db, "web-2")
	addSamples(t, db, a, sample{ts: 0, cpu: 10, uptime: 1}, sample{ts: 30, cpu: 20, uptime: 2})
	addSamples(t, db, b, sample{ts: 0, cpu: 80, uptime: 3}, sample{ts: 30, cpu: 100, uptime: 4})
	runOnce(t, newService(t, db, c))

	first := buckets(t, db, "metrics_1m", a)
	second := buckets(t, db, "metrics_1m", b)
	if len(first) != 1 || first[0].cpu != 15 {
		t.Errorf("web-1 = %+v, want cpu 15", first)
	}
	if len(second) != 1 || second[0].cpu != 90 {
		t.Errorf("web-2 = %+v, want cpu 90", second)
	}
}

func TestRetentionSettings(t *testing.T) {
	db := openDB(t)
	got, err := LoadRetention(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got != Default {
		t.Errorf("LoadRetention on a fresh database = %+v, want %+v", got, Default)
	}

	want := Retention{RawHours: 48, MinuteDays: 14, HourDays: 365}
	if !want.Valid() {
		t.Fatalf("%+v is not one of the offered combinations", want)
	}
	err = db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return StoreRetention(ctx, tx, want)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err = LoadRetention(t.Context(), db); err != nil || got != want {
		t.Fatalf("LoadRetention = %+v, %v; want %+v", got, err, want)
	}

	// A value that is not offered falls back to the default for that
	// table without disturbing the others.
	setSetting(t, db, store.SettingRetentionRawHours, "9999")
	setSetting(t, db, store.SettingRetentionMinuteDays, "not a number")
	got, err = LoadRetention(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got.RawHours != Default.RawHours || got.MinuteDays != Default.MinuteDays || got.HourDays != 365 {
		t.Errorf("LoadRetention = %+v, want the defaults for the damaged values and 365 hour days", got)
	}

	for _, bad := range []Retention{
		{RawHours: 5, MinuteDays: 7, HourDays: 90},
		{RawHours: 24, MinuteDays: 8, HourDays: 90},
		{RawHours: 24, MinuteDays: 7, HourDays: 91},
		{},
	} {
		if bad.Valid() {
			t.Errorf("%+v counts as valid", bad)
		}
	}
}

func TestRetentionIsRereadEveryPass(t *testing.T) {
	db := openDB(t)
	c := &clock{}
	host := addHost(t, db, "web-1")
	now := int64(100 * 86400)
	c.set(now)
	addSamples(t, db, host, sample{ts: now - 10*3600, cpu: 1, uptime: 1})
	setSetting(t, db, store.SettingRollupMinuteThrough, "8640000")
	s := newService(t, db, c)
	runOnce(t, s)
	if n := count(t, db, "metrics_raw"); n != 1 {
		t.Fatalf("metrics_raw has %d rows, want the sample kept by the default 24 hours", n)
	}

	// Shortening the retention takes effect on the next pass; no restart.
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return StoreRetention(ctx, tx, Retention{RawHours: 6, MinuteDays: 7, HourDays: 90})
	})
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, s)
	if n := count(t, db, "metrics_raw"); n != 0 {
		t.Errorf("metrics_raw has %d rows, want the sample dropped by the 6 hour retention", n)
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
		t.Error("New without a database returned no error")
	}
	if _, err := New(Options{DB: openDB(t)}); err == nil {
		t.Error("New without a logger returned no error")
	}
}
