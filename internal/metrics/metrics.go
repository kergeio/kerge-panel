// Package metrics answers the history queries behind the charts: one
// window of one host's readings, taken from whichever table covers the
// requested span.
//
// Readings are sent to the browser as columns rather than as rows: a
// chart wants one array per series, and a day of raw samples is tens of
// thousands of points, so the encoding leaves out everything it can.
package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/kergeio/kerge-panel/internal/store"
)

// Range is one of the spans the detail page offers together with the
// table that covers it: one-minute averages up to a day, one-hour averages
// beyond that.
//
// Charts never read raw samples. A summarized point is one point per
// pixel or thereabouts, which is all a chart can draw, and it lets every
// span be read as an average or as a maximum; the raw samples are what
// the dashboard shows live and what the summaries are computed from. A
// span whose data has been pruned to a shorter retention simply starts
// later; the window itself does not change.
type Range struct {
	Key   string
	Span  time.Duration
	Table string
	// Resolution names what a row of Table holds, for the page to say
	// how wide a point is.
	Resolution string
}

// Ranges are the spans the detail page offers, in the order they are
// shown. The point counts are what a chart draws: 60, 1440, 168, 720 and
// 2160.
var Ranges = []Range{
	{Key: "1h", Span: time.Hour, Table: "metrics_1m", Resolution: "1m"},
	{Key: "24h", Span: 24 * time.Hour, Table: "metrics_1m", Resolution: "1m"},
	{Key: "7d", Span: 7 * 24 * time.Hour, Table: "metrics_1h", Resolution: "1h"},
	{Key: "30d", Span: 30 * 24 * time.Hour, Table: "metrics_1h", Resolution: "1h"},
	{Key: "90d", Span: 90 * 24 * time.Hour, Table: "metrics_1h", Resolution: "1h"},
}

// DefaultRange is what the detail page opens with.
const DefaultRange = "24h"

// Lookup returns the range with this key.
func Lookup(key string) (Range, bool) {
	i := slices.IndexFunc(Ranges, func(r Range) bool { return r.Key == key })
	if i < 0 {
		return Range{}, false
	}
	return Ranges[i], true
}

// MaxPoints bounds how many points one query returns. The widest span
// draws 2160 points, so the cap is far out of the way; it is here so
// that a damaged or hand-edited table cannot make the panel build an
// answer of any size. Past it the newest points are kept and the answer
// says it was cut.
const MaxPoints = 30000

// Column is one series of readings. A reading that was not reported is
// NaN, which is written as null and which the chart draws as a gap.
type Column []float64

// MarshalJSON writes the column as a JSON array. Values are already
// rounded to the precision the charts show, so the shortest
// representation of each one is exact.
func (c Column) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 0, len(c)*8+2)
	buf = append(buf, '[')
	for i, v := range c {
		if i > 0 {
			buf = append(buf, ',')
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			buf = append(buf, "null"...)
			continue
		}
		buf = strconv.AppendFloat(buf, v, 'f', -1, 64)
	}
	return append(buf, ']'), nil
}

// Series is a window of one host's history, ready to be encoded for the
// charts. Every column has the same length as T.
//
// Each charted metric has an average column and a maximum column: the
// operator reads the window as one or the other. The load columns are
// absent for a platform that does not report load averages.
type Series struct {
	// From and To bound the window as Unix seconds.
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// Truncated reports that the window held more rows than MaxPoints
	// and only the newest ones are here.
	Truncated bool `json:"truncated,omitempty"`

	T           []int64 `json:"t"`
	CPU         Column  `json:"cpu"`
	CPUMax      Column  `json:"cpu_max,omitempty"`
	MemUsed     Column  `json:"mem_used"`
	MemUsedMax  Column  `json:"mem_used_max,omitempty"`
	Load1       Column  `json:"load1,omitempty"`
	Load1Max    Column  `json:"load1_max,omitempty"`
	DiskUsed    Column  `json:"disk_used"`
	DiskUsedMax Column  `json:"disk_used_max,omitempty"`
	RxRate      Column  `json:"rx_rate"`
	RxRateMax   Column  `json:"rx_rate_max,omitempty"`
	TxRate      Column  `json:"tx_rate"`
	TxRateMax   Column  `json:"tx_rate_max,omitempty"`

	// The capacities the memory and disk charts are drawn against: their
	// axes reach the total rather than the highest reading, so the height
	// of a curve is the share in use.
	MemTotal  Column `json:"mem_total"`
	DiskTotal Column `json:"disk_total"`
}

// Decimals kept per kind of reading. Percentages and byte counts are
// shown the way the rest of the panel shows them; load averages keep one
// digit more, because a chart of a lightly loaded host would otherwise be
// a staircase.
const (
	percentDecimals = 1
	loadDecimals    = 2
	byteDecimals    = 0
	rateDecimals    = 0
)

// selectSQL reads the window newest first, so that the cap keeps the
// newest points. Only the table name is substituted, from the
// compile-time constant of a Range; the host and the window are bound.
const selectSQL = `
SELECT ts, cpu, cpu_max, mem_used, mem_used_max, mem_total,
       load1, load1_max, disk_used, disk_used_max, disk_total,
       rx_rate, rx_rate_max, tx_rate, tx_rate_max
FROM %s
WHERE host_id = ? AND ts >= ? AND ts <= ?
ORDER BY ts DESC
LIMIT ?`

// query returns the statement for a range.
func (r Range) query() string {
	return fmt.Sprintf(selectSQL, r.Table)
}

// Service reads the history.
type Service struct{ db *store.DB }

// NewService returns a service backed by db.
func NewService(db *store.DB) *Service { return &Service{db: db} }

// Query reads the window that ends at now and spans r.
func (svc *Service) Query(ctx context.Context, hostID int64, r Range, now time.Time) (Series, error) {
	to := now.Unix()
	from := now.Add(-r.Span).Unix()
	// The columns of an empty window are empty arrays rather than null,
	// so the chart script reads one shape whether or not the window
	// holds readings.
	s := Series{From: from, To: to, T: []int64{}}

	// One row past the cap is read so that a full window can be told
	// from one that was cut.
	rows, err := svc.db.Read().QueryContext(ctx, r.query(), hostID, from, to, MaxPoints+1)
	if err != nil {
		return Series{}, err
	}
	defer rows.Close()

	var anyLoad bool
	for rows.Next() {
		var ts int64
		var cpu, cpuMax, memUsed, memUsedMax, memTotal sql.NullFloat64
		var load1, load1Max, diskUsed, diskUsedMax, diskTotal sql.NullFloat64
		var rxRate, rxMax, txRate, txMax sql.NullFloat64
		if err := rows.Scan(&ts, &cpu, &cpuMax, &memUsed, &memUsedMax, &memTotal,
			&load1, &load1Max, &diskUsed, &diskUsedMax, &diskTotal,
			&rxRate, &rxMax, &txRate, &txMax); err != nil {
			return Series{}, err
		}
		s.T = append(s.T, ts)
		s.CPU = append(s.CPU, reading(cpu, percentDecimals))
		s.MemUsed = append(s.MemUsed, reading(memUsed, byteDecimals))
		s.MemTotal = append(s.MemTotal, reading(memTotal, byteDecimals))
		s.Load1 = append(s.Load1, reading(load1, loadDecimals))
		s.DiskUsed = append(s.DiskUsed, reading(diskUsed, byteDecimals))
		s.DiskTotal = append(s.DiskTotal, reading(diskTotal, byteDecimals))
		s.RxRate = append(s.RxRate, reading(rxRate, rateDecimals))
		s.TxRate = append(s.TxRate, reading(txRate, rateDecimals))
		s.CPUMax = append(s.CPUMax, reading(cpuMax, percentDecimals))
		s.MemUsedMax = append(s.MemUsedMax, reading(memUsedMax, byteDecimals))
		s.Load1Max = append(s.Load1Max, reading(load1Max, loadDecimals))
		s.DiskUsedMax = append(s.DiskUsedMax, reading(diskUsedMax, byteDecimals))
		s.RxRateMax = append(s.RxRateMax, reading(rxMax, rateDecimals))
		s.TxRateMax = append(s.TxRateMax, reading(txMax, rateDecimals))
		anyLoad = anyLoad || load1.Valid || load1Max.Valid
	}
	if err := rows.Err(); err != nil {
		return Series{}, err
	}

	if len(s.T) > MaxPoints {
		s.Truncated = true
		s.trim(MaxPoints)
	}
	// A platform that does not report the load average leaves the three
	// columns empty, and the page then has no load chart.
	if !anyLoad {
		s.Load1, s.Load1Max = nil, nil
	}
	s.reverse()
	return s, nil
}

// trim keeps the first n points, which are the newest ones while the
// series is still in the order the query returned.
func (s *Series) trim(n int) {
	s.T = s.T[:n]
	for _, c := range s.columns() {
		if len(*c) > n {
			*c = (*c)[:n]
		}
	}
}

// reverse puts the series in chronological order, which is what a chart
// reads.
func (s *Series) reverse() {
	slices.Reverse(s.T)
	for _, c := range s.columns() {
		slices.Reverse(*c)
	}
}

// columns lists every column, so that reordering and trimming cannot
// forget one.
func (s *Series) columns() []*Column {
	return []*Column{
		&s.CPU, &s.CPUMax, &s.MemUsed, &s.MemUsedMax, &s.MemTotal,
		&s.Load1, &s.Load1Max, &s.DiskUsed, &s.DiskUsedMax, &s.DiskTotal,
		&s.RxRate, &s.RxRateMax, &s.TxRate, &s.TxRateMax,
	}
}

// reading rounds a stored value to the precision the charts show. A
// column that holds no value becomes a gap.
func reading(v sql.NullFloat64, decimals int) float64 {
	if !v.Valid {
		return math.NaN()
	}
	p := math.Pow(10, float64(decimals))
	return math.Round(v.Float64*p) / p
}

// totalsTables are searched newest first for a host's capacities.
var totalsTables = []string{"metrics_raw", "metrics_1m", "metrics_1h"}

// Totals returns the memory and disk capacity a host last reported, from
// the newest table that holds a reading, for the detail page to show while
// no live sample is at hand. Either is nil when never reported or already
// pruned.
func (svc *Service) Totals(ctx context.Context, hostID int64) (mem, disk *uint64, err error) {
	for _, table := range totalsTables {
		var m, d sql.NullInt64
		err := svc.db.Read().QueryRowContext(ctx, "SELECT mem_total, disk_total FROM "+table+
			" WHERE host_id = ? AND (mem_total IS NOT NULL OR disk_total IS NOT NULL) ORDER BY ts DESC LIMIT 1",
			hostID).Scan(&m, &d)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		return nonNegative(m), nonNegative(d), nil
	}
	return nil, nil, nil
}

func nonNegative(v sql.NullInt64) *uint64 {
	if !v.Valid || v.Int64 < 0 {
		return nil
	}
	u := uint64(v.Int64)
	return &u
}
