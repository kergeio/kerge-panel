package metrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "kerge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func addHost(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var id int64
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO hosts (name, created_at) VALUES (?, ?)", "host", 0)
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

// row is one reading. A nil field is stored as NULL.
type row struct {
	ts                    int64
	cpu, cpuMax           *float64
	memUsed, memUsedMax   *float64
	memTotal              *float64
	swapUsed, swapTotal   *float64
	load1, load1Max       *float64
	load5, load15         *float64
	diskUsed, diskUsedMax *float64
	diskTotal             *float64
	rxRate, rxMax         *float64
	txRate, txMax         *float64
}

func f(v float64) *float64 { return &v }

// insert writes rows into one of the summarized tables, which are the only
// ones a chart reads.
func insert(t *testing.T, db *store.DB, table string, hostID int64, rows ...row) {
	t.Helper()
	stmt := `INSERT INTO ` + table + ` (host_id, ts, cpu, cpu_max,
			mem_used, mem_used_max, mem_total, swap_used, swap_total,
			load1, load1_max, load5, load15,
			disk_used, disk_used_max, disk_total, disk_free,
			rx_rate, rx_rate_max, tx_rate, tx_rate_max, uptime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		// One prepared statement: some tests write tens of thousands of
		// rows.
		prepared, err := tx.PrepareContext(ctx, stmt)
		if err != nil {
			return err
		}
		defer prepared.Close()
		for _, r := range rows {
			args := []any{hostID, r.ts, value(r.cpu), value(r.cpuMax),
				whole(r.memUsed), whole(r.memUsedMax), whole(r.memTotal),
				whole(r.swapUsed), whole(r.swapTotal),
				value(r.load1), value(r.load1Max), value(r.load5), value(r.load15),
				whole(r.diskUsed), whole(r.diskUsedMax), whole(r.diskTotal), nil,
				value(r.rxRate), value(r.rxMax), value(r.txRate), value(r.txMax),
				nil} // uptime, which no chart shows
			if _, err := prepared.ExecContext(ctx, args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func value(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// whole binds a byte count, whose columns are INTEGER in the STRICT
// schema.
func whole(p *float64) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

func rangeOf(t *testing.T, key string) Range {
	t.Helper()
	r, ok := Lookup(key)
	if !ok {
		t.Fatalf("no range %q", key)
	}
	return r
}

var now = time.Unix(1_700_000_000, 0)

// TestRangesSelectTheTable checks which table a span reads: a day or
// less reads the one-minute averages, anything longer the one-hour
// averages, and no span reads the raw samples. Each table holds a
// different value, so the answer says which one was read.
func TestRangesSelectTheTable(t *testing.T) {
	db := openDB(t)
	host := addHost(t, db)
	ts := now.Add(-30 * time.Minute).Unix()
	insert(t, db, "metrics_1m", host, row{ts: ts, cpu: f(2), cpuMax: f(20)})
	insert(t, db, "metrics_1h", host, row{ts: ts, cpu: f(3), cpuMax: f(30)})

	svc := NewService(db)
	for _, tc := range []struct {
		key   string
		cpu   float64
		peaks bool
	}{
		{"1h", 2, true},
		{"24h", 2, true},
		{"7d", 3, true},
		{"30d", 3, true},
		{"90d", 3, true},
	} {
		s, err := svc.Query(t.Context(), host, rangeOf(t, tc.key), now)
		if err != nil {
			t.Fatalf("%s: %v", tc.key, err)
		}
		if len(s.CPU) != 1 || s.CPU[0] != tc.cpu {
			t.Errorf("%s: cpu = %v, want [%v]", tc.key, s.CPU, tc.cpu)
		}
		for name, col := range map[string]Column{
			"cpu_max": s.CPUMax, "mem_used_max": s.MemUsedMax,
			"disk_used_max": s.DiskUsedMax, "rx_rate_max": s.RxRateMax,
			"tx_rate_max": s.TxRateMax,
		} {
			if got := col != nil; got != tc.peaks {
				t.Errorf("%s: %s present = %v, want %v", tc.key, name, got, tc.peaks)
			}
		}
	}
}

// TestWindowBounds checks that only readings inside the span come back,
// and in chronological order.
func TestWindowBounds(t *testing.T) {
	db := openDB(t)
	host := addHost(t, db)
	insert(t, db, "metrics_1m", host,
		row{ts: now.Add(-25 * time.Hour).Unix(), cpu: f(1)}, // before the window
		row{ts: now.Add(-2 * time.Hour).Unix(), cpu: f(2)},
		row{ts: now.Add(-time.Minute).Unix(), cpu: f(3)},
		row{ts: now.Add(time.Hour).Unix(), cpu: f(4)}, // after it
	)
	s, err := NewService(db).Query(t.Context(), host, rangeOf(t, "24h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.T) != 2 || s.T[0] >= s.T[1] {
		t.Fatalf("timestamps = %v, want two in ascending order", s.T)
	}
	if s.CPU[0] != 2 || s.CPU[1] != 3 {
		t.Errorf("cpu = %v, want [2 3]", s.CPU)
	}
	if s.From != now.Add(-24*time.Hour).Unix() || s.To != now.Unix() {
		t.Errorf("window = %d..%d, want %d..%d", s.From, s.To,
			now.Add(-24*time.Hour).Unix(), now.Unix())
	}
}

// TestOtherHostsAreNotRead checks the host filter.
func TestOtherHostsAreNotRead(t *testing.T) {
	db := openDB(t)
	mine, theirs := addHost(t, db), addHost(t, db)
	ts := now.Add(-time.Minute).Unix()
	insert(t, db, "metrics_1m", mine, row{ts: ts, cpu: f(1)})
	insert(t, db, "metrics_1m", theirs, row{ts: ts, cpu: f(2)})

	s, err := NewService(db).Query(t.Context(), mine, rangeOf(t, "1h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.CPU) != 1 || s.CPU[0] != 1 {
		t.Errorf("cpu = %v, want [1]", s.CPU)
	}
}

// TestLoadIsDroppedWhenUnsupported covers the rule that a platform which
// does not report load averages gets no load chart.
func TestLoadIsDroppedWhenUnsupported(t *testing.T) {
	db := openDB(t)
	host := addHost(t, db)
	insert(t, db, "metrics_1m", host,
		row{ts: now.Add(-2 * time.Minute).Unix(), cpu: f(1)},
		row{ts: now.Add(-time.Minute).Unix(), cpu: f(2)},
	)
	svc := NewService(db)
	s, err := svc.Query(t.Context(), host, rangeOf(t, "1h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Load1 != nil || s.Load1Max != nil {
		t.Errorf("load columns = %v / %v, want both absent", s.Load1, s.Load1Max)
	}

	// One reported value is enough to keep the chart, and the readings
	// that were not reported stay gaps.
	insert(t, db, "metrics_1m", host, row{ts: now.Add(-30 * time.Second).Unix(), load1: f(0.25)})
	s, err = svc.Query(t.Context(), host, rangeOf(t, "1h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Load1) != 3 {
		t.Fatalf("load1 = %v, want three points", s.Load1)
	}
	if !math.IsNaN(s.Load1[0]) || !math.IsNaN(s.Load1[1]) || s.Load1[2] != 0.25 {
		t.Errorf("load1 = %v, want two gaps then 0.25", s.Load1)
	}
}

// TestReadingsAreRounded checks the precision the columns carry: one
// decimal for a percentage, two for a load average, whole bytes.
func TestReadingsAreRounded(t *testing.T) {
	db := openDB(t)
	host := addHost(t, db)
	insert(t, db, "metrics_1m", host, row{
		ts:      now.Add(-time.Minute).Unix(),
		cpu:     f(12.3456),
		load1:   f(0.123456),
		memUsed: f(1025),
		rxRate:  f(1234.56),
	})
	s, err := NewService(db).Query(t.Context(), host, rangeOf(t, "1h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.CPU[0] != 12.3 || s.Load1[0] != 0.12 || s.MemUsed[0] != 1025 || s.RxRate[0] != 1235 {
		t.Errorf("readings = %v %v %v %v, want 12.3 0.12 1025 1235",
			s.CPU[0], s.Load1[0], s.MemUsed[0], s.RxRate[0])
	}
}

// TestTooManyPointsKeepsTheNewest checks the cap: a window with more rows
// than the panel sends comes back cut, newest points kept, and says so.
func TestTooManyPointsKeepsTheNewest(t *testing.T) {
	db := openDB(t)
	host := addHost(t, db)
	start := now.Add(-24 * time.Hour).Unix()
	rows := make([]row, 0, MaxPoints+10)
	for i := range int64(MaxPoints + 10) {
		rows = append(rows, row{ts: start + i, cpu: f(float64(i % 100))})
	}
	insert(t, db, "metrics_1m", host, rows...)

	s, err := NewService(db).Query(t.Context(), host, rangeOf(t, "24h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Truncated {
		t.Error("truncated = false, want true")
	}
	if len(s.T) != MaxPoints || len(s.CPU) != MaxPoints {
		t.Fatalf("points = %d/%d, want %d", len(s.T), len(s.CPU), MaxPoints)
	}
	if last := rows[len(rows)-1]; s.T[len(s.T)-1] != last.ts {
		t.Errorf("newest point = %d, want %d", s.T[len(s.T)-1], last.ts)
	}
}

// TestEmptyWindow checks that a host with no readings yields no columns
// rather than an error.
func TestEmptyWindow(t *testing.T) {
	db := openDB(t)
	host := addHost(t, db)
	s, err := NewService(db).Query(t.Context(), host, rangeOf(t, "90d"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.T) != 0 || s.Truncated {
		t.Errorf("series = %+v, want empty", s)
	}
	// The script reads one shape either way, so an empty window sends
	// empty arrays rather than null.
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"t":[]`) || !strings.Contains(string(body), `"cpu":[]`) {
		t.Errorf("empty window = %s, want empty arrays", body)
	}
}

// TestColumnJSON checks the encoding the charts read: a reading that was
// not reported is null, and the numbers are written without exponents.
func TestColumnJSON(t *testing.T) {
	c := Column{12.3, math.NaN(), 1073741824, 0}
	got, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[12.3,null,1073741824,0]`; string(got) != want {
		t.Errorf("json = %s, want %s", got, want)
	}
}

// TestSeriesJSONOmitsAbsentColumns checks that a table without peaks and
// a host without load averages send no empty arrays.
func TestSeriesJSONOmitsAbsentColumns(t *testing.T) {
	body, err := json.Marshal(Series{T: []int64{1}, CPU: Column{1}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cpu_max", "load1", "load1_max", "mem_used_max",
		"disk_used_max", "rx_rate_max", "tx_rate_max", "swap_used", "swap_total"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("%s is in the answer, want it left out", key)
		}
	}
	for _, key := range []string{"t", "cpu", "mem_used", "rx_rate"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("%s is missing from the answer", key)
		}
	}
}

// TestSummarizedWindowCarriesEveryStatistic reads one summarized row
// whose every column differs, so a column read from the wrong place cannot
// pass unnoticed.
func TestSummarizedWindowCarriesEveryStatistic(t *testing.T) {
	db := openDB(t)
	host := addHost(t, db)
	insert(t, db, "metrics_1m", host, row{
		ts:      now.Add(-time.Hour).Unix(),
		cpu:     f(11),
		cpuMax:  f(12),
		memUsed: f(21), memUsedMax: f(22), memTotal: f(23),
		load1: f(0.31), load1Max: f(0.32),
		diskUsed: f(41), diskUsedMax: f(42), diskTotal: f(43),
		rxRate: f(51), rxMax: f(52),
		txRate: f(61), txMax: f(62),
	})
	s, err := NewService(db).Query(t.Context(), host, rangeOf(t, "24h"), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  Column
		want float64
	}{
		{"cpu", s.CPU, 11}, {"cpu_max", s.CPUMax, 12},
		{"mem_used", s.MemUsed, 21}, {"mem_used_max", s.MemUsedMax, 22},
		{"mem_total", s.MemTotal, 23},
		{"load1", s.Load1, 0.31}, {"load1_max", s.Load1Max, 0.32},
		{"disk_used", s.DiskUsed, 41}, {"disk_used_max", s.DiskUsedMax, 42},
		{"disk_total", s.DiskTotal, 43},
		{"rx_rate", s.RxRate, 51}, {"rx_rate_max", s.RxRateMax, 52},
		{"tx_rate", s.TxRate, 61}, {"tx_rate_max", s.TxRateMax, 62},
	} {
		if len(c.got) != 1 || c.got[0] != c.want {
			t.Errorf("%s = %v, want [%v]", c.name, c.got, c.want)
		}
	}
}

// TestLookup checks that only the offered spans are accepted.
func TestLookup(t *testing.T) {
	if _, ok := Lookup("2h"); ok {
		t.Error("2h was accepted")
	}
	if _, ok := Lookup(""); ok {
		t.Error("the empty key was accepted")
	}
	r, ok := Lookup(DefaultRange)
	if !ok || r.Span != 24*time.Hour || r.Table != "metrics_1m" {
		t.Errorf("default range = %+v, %v", r, ok)
	}
}
