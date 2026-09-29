// Package rollup keeps the metrics tables bounded. Raw samples are
// summarized into one-minute buckets, those into one-hour buckets, and
// whatever has outlived its retention is deleted.
//
// Summarizing is driven by a watermark per stage rather than by the clock,
// so a panel that was stopped for a while catches up when it starts again
// instead of leaving a hole in the history.
package rollup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/kergeio/kerge-panel/internal/store"
)

// Timing.
const (
	// DefaultInterval is how often Run summarizes and prunes. A raw
	// bucket is one minute, so nothing is gained by running more often.
	DefaultInterval = time.Minute
	// DefaultOptimizeEvery is how often PRAGMA optimize runs.
	DefaultOptimizeEvery = time.Hour
	// DefaultPruneBatch is the most rows one pruning write deletes. Each
	// write is one savepoint, and the pages it changes are journaled in
	// memory (temp_store=MEMORY), so the batch bounds that memory however
	// many rows have expired.
	DefaultPruneBatch = 1000
)

// Options configure a Service.
type Options struct {
	DB     *store.DB
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Interval and OptimizeEvery default to the constants above.
	Interval      time.Duration
	OptimizeEvery time.Duration
}

// Service summarizes and prunes the metrics tables. All of its writes go
// through the store's writer, so they are serialized with the samples
// arriving from the agents.
type Service struct {
	opts Options
	// minute and hour are the two summarizing stages. They are copies of
	// the defaults so that a test can narrow one of them.
	minute, hour stage
	// pruneBatch is DefaultPruneBatch, held here so that a test can
	// narrow it.
	pruneBatch int64
	// pruneHook, if set, is called with the rows each pruning write
	// deleted.
	pruneHook func(table string, deleted int64)
	// lastOptimize is only touched by the goroutine calling RunOnce.
	lastOptimize time.Time
}

// New returns a service. It does nothing until Run or RunOnce is called.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Logger == nil {
		return nil, errors.New("rollup: incomplete options")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.OptimizeEvery <= 0 {
		opts.OptimizeEvery = DefaultOptimizeEvery
	}
	return &Service{
		opts:         opts,
		minute:       minuteStage,
		hour:         hourStage,
		pruneBatch:   DefaultPruneBatch,
		lastOptimize: opts.Now(),
	}, nil
}

// Run summarizes and prunes every Interval until ctx ends. It starts with
// one pass so that a panel which was down catches up right away.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.opts.Interval)
	defer t.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.opts.Logger.Error("summarizing the metrics", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce summarizes every complete bucket that is still pending, deletes
// what has outlived its retention and, now and then, lets SQLite refresh
// its query statistics.
func (s *Service) RunOnce(ctx context.Context) error {
	retention, err := LoadRetention(ctx, s.opts.DB)
	if err != nil {
		return err
	}
	now := s.opts.Now()

	// The hour stage may only summarize hours the minute stage has
	// finished with, or a partly filled hour would be written as if it
	// were complete.
	minuteThrough, err := s.summarize(ctx, s.minute, now, 0)
	if err != nil {
		return err
	}
	hourThrough, err := s.summarize(ctx, s.hour, now, minuteThrough)
	if err != nil {
		return err
	}

	// A row is only deleted once the coarser table holds it, so a stage
	// that is still catching up holds its source table's pruning back.
	for _, t := range []struct {
		table string
		keep  time.Duration
		// summarized is how far the stage reading this table has got.
		// It is nil for metrics_1h, which nothing summarizes.
		summarized *int64
	}{
		{"metrics_raw", time.Duration(retention.RawHours) * time.Hour, &minuteThrough},
		{"metrics_1m", time.Duration(retention.MinuteDays) * 24 * time.Hour, &hourThrough},
		{"metrics_1h", time.Duration(retention.HourDays) * 24 * time.Hour, nil},
	} {
		cutoff := now.Add(-t.keep).Unix()
		if t.summarized != nil && *t.summarized < cutoff {
			cutoff = *t.summarized
		}
		if err := s.prune(ctx, t.table, cutoff); err != nil {
			return err
		}
	}

	if now.Sub(s.lastOptimize) >= s.opts.OptimizeEvery {
		if err := s.optimize(ctx); err != nil {
			return err
		}
		s.lastOptimize = now
	}
	return nil
}

// stage is one summarizing step: complete buckets of src are written to
// dst. Every field is a compile-time constant.
type stage struct {
	src, dst string
	// bucket is the width of a destination row, in seconds.
	bucket int64
	// watermark is the settings key holding the end of what has been
	// summarized, as a Unix second.
	watermark string
	// maxBuckets bounds how much one pass writes, so catching up after a
	// long stop cannot hold the writer for minutes at a time.
	maxBuckets int64
	// peaks names the source column each maximum is taken from: the
	// reading itself in the first stage, the maximum already computed in
	// the second. Every metric a chart draws has one, so that a window can
	// be read as averages or as maxima.
	cpuPeak, memPeak, loadPeak, diskPeak, rxPeak, txPeak string
}

var (
	minuteStage = stage{
		src: "metrics_raw", dst: "metrics_1m", bucket: 60,
		watermark: store.SettingRollupMinuteThrough, maxBuckets: 1500,
		cpuPeak: "cpu", memPeak: "mem_used", loadPeak: "load1",
		diskPeak: "disk_used", rxPeak: "rx_rate", txPeak: "tx_rate",
	}
	hourStage = stage{
		src: "metrics_1m", dst: "metrics_1h", bucket: 3600,
		watermark: store.SettingRollupHourThrough, maxBuckets: 750,
		cpuPeak: "cpu_max", memPeak: "mem_used_max", loadPeak: "load1_max",
		diskPeak: "disk_used_max", rxPeak: "rx_rate_max", txPeak: "tx_rate_max",
	}
)

// summarizeSQL averages every column over a bucket and carries the
// maxima, plus the last uptime in the bucket, which is a reading rather
// than a rate and so must not be averaged.
//
// The identifiers come from a stage, which holds compile-time constants;
// the bucket range is bound.
const summarizeSQL = `
WITH bucketed AS (
    SELECT host_id, ts, ts / %[3]d * %[3]d AS bucket,
           cpu, %[4]s AS cpu_peak, mem_used, %[5]s AS mem_peak,
           mem_total, swap_used, swap_total,
           load1, %[6]s AS load_peak, load5, load15,
           disk_used, %[7]s AS disk_peak, disk_total, disk_free,
           rx_rate, %[8]s AS rx_peak, tx_rate, %[9]s AS tx_peak, uptime
    FROM %[1]s
    WHERE ts >= ? AND ts < ?
),
last_uptime AS (
    SELECT host_id, bucket, uptime FROM (
        SELECT host_id, bucket, uptime,
               ROW_NUMBER() OVER (PARTITION BY host_id, bucket ORDER BY ts DESC) AS rn
        FROM bucketed WHERE uptime IS NOT NULL
    ) WHERE rn = 1
)
INSERT OR REPLACE INTO %[2]s (
    host_id, ts, cpu, cpu_max, mem_used, mem_used_max, mem_total,
    swap_used, swap_total,
    load1, load1_max, load5, load15,
    disk_used, disk_used_max, disk_total, disk_free,
    rx_rate, rx_rate_max, tx_rate, tx_rate_max, uptime)
SELECT b.host_id, b.bucket,
       AVG(b.cpu), MAX(b.cpu_peak),
       CAST(AVG(b.mem_used) AS INTEGER), CAST(MAX(b.mem_peak) AS INTEGER),
       CAST(AVG(b.mem_total) AS INTEGER),
       CAST(AVG(b.swap_used) AS INTEGER), CAST(AVG(b.swap_total) AS INTEGER),
       AVG(b.load1), MAX(b.load_peak), AVG(b.load5), AVG(b.load15),
       CAST(AVG(b.disk_used) AS INTEGER), CAST(MAX(b.disk_peak) AS INTEGER),
       CAST(AVG(b.disk_total) AS INTEGER),
       CAST(AVG(b.disk_free) AS INTEGER),
       AVG(b.rx_rate), MAX(b.rx_peak), AVG(b.tx_rate), MAX(b.tx_peak),
       u.uptime
FROM bucketed b
LEFT JOIN last_uptime u ON u.host_id = b.host_id AND u.bucket = b.bucket
GROUP BY b.host_id, b.bucket`

func (st stage) insertSQL() string {
	return fmt.Sprintf(summarizeSQL, st.src, st.dst, st.bucket,
		st.cpuPeak, st.memPeak, st.loadPeak, st.diskPeak, st.rxPeak, st.txPeak)
}

// summarize writes every complete bucket between the stage's watermark and
// now, and returns the watermark it leaves behind: the end of the range
// that has been summarized. limit, when not zero, caps that range -- the
// hour stage may not run past what the minute stage has produced.
func (s *Service) summarize(ctx context.Context, st stage, now time.Time, limit int64) (int64, error) {
	to := floorTo(now.Unix(), st.bucket)
	if limit > 0 {
		if capped := floorTo(limit, st.bucket); capped < to {
			to = capped
		}
	}
	from, marked, err := s.watermark(ctx, st.watermark)
	if err != nil {
		return 0, err
	}
	oldest, newest, any, err := s.span(ctx, st.src)
	if err != nil {
		return 0, err
	}
	switch {
	case !any:
		// Nothing to summarize: the watermark moves to the present so
		// the empty range is not walked again.
		from = to
	case !marked || oldest > from:
		// A first run, or a gap left by a panel that was stopped for
		// longer than the source table's retention.
		from = floorTo(oldest, st.bucket)
	}
	// Nothing past the newest row can be summarized, so a stop of any
	// length costs one pass rather than one pass per empty bucket.
	if any {
		if end := floorTo(newest, st.bucket) + st.bucket; end < to {
			to = end
		}
	}
	if to <= from {
		// Nothing complete is pending. When the source holds nothing
		// above the watermark either, the stage has caught up with the
		// present, and saying so lets the next stage and the pruning
		// move past a host that stopped reporting.
		caught := from
		if !any || floorTo(newest, st.bucket) < from {
			caught = floorTo(now.Unix(), st.bucket)
		}
		if !marked || caught != from {
			return caught, s.setWatermark(ctx, st.watermark, caught)
		}
		return caught, nil
	}
	if span := st.maxBuckets * st.bucket; to-from > span {
		to = from + span
	}

	insert := st.insertSQL()
	err = s.opts.DB.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, insert, from, to); err != nil {
			return fmt.Errorf("rollup: summarizing %s into %s: %w", st.src, st.dst, err)
		}
		return store.SetSetting(ctx, tx, st.watermark, strconv.FormatInt(to, 10))
	})
	if err != nil {
		return 0, err
	}
	return to, nil
}

// prune deletes rows older than cutoff. A cutoff of zero or less deletes
// nothing. The rows go in batches of pruneBatch, one write each, until a
// batch comes back short; the samples arriving from the agents are written
// in between.
func (s *Service) prune(ctx context.Context, table string, cutoff int64) error {
	if cutoff <= 0 {
		return nil
	}
	// table is one of the three compile-time constants above. The row
	// value subquery stands in for DELETE ... LIMIT, which SQLite only
	// offers when built with SQLITE_ENABLE_UPDATE_DELETE_LIMIT.
	stmt := "DELETE FROM " + table + " WHERE (host_id, ts) IN (" +
		"SELECT host_id, ts FROM " + table + " WHERE ts < ? LIMIT ?)"
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var deleted int64
		err := s.opts.DB.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, stmt, cutoff, s.pruneBatch)
			if err != nil {
				return fmt.Errorf("rollup: pruning %s: %w", table, err)
			}
			deleted, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return err
		}
		if s.pruneHook != nil {
			s.pruneHook(table, deleted)
		}
		if deleted < s.pruneBatch {
			return nil
		}
	}
}

// optimize lets SQLite refresh the statistics its query planner uses. It
// runs on the writer connection, which is the one that has been changing
// the tables.
func (s *Service) optimize(ctx context.Context) error {
	return s.opts.DB.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "PRAGMA optimize"); err != nil {
			return fmt.Errorf("rollup: optimize: %w", err)
		}
		return nil
	})
}

// watermark reads a stage's watermark. An unreadable value counts as
// unset, which makes the stage start again from the oldest row it has.
func (s *Service) watermark(ctx context.Context, key string) (int64, bool, error) {
	value, ok, err := s.opts.DB.GetSetting(ctx, key)
	if err != nil || !ok {
		return 0, false, err
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, false, nil
	}
	return n, true, nil
}

func (s *Service) setWatermark(ctx context.Context, key string, value int64) error {
	return s.opts.DB.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return store.SetSetting(ctx, tx, key, strconv.FormatInt(value, 10))
	})
}

// span returns the timestamps of the oldest and the newest row in table,
// and whether it holds any row at all.
func (s *Service) span(ctx context.Context, table string) (oldest, newest int64, any bool, err error) {
	// table is one of the three compile-time constants above.
	var lo, hi sql.NullInt64
	err = s.opts.DB.Read().QueryRowContext(ctx, "SELECT MIN(ts), MAX(ts) FROM "+table).Scan(&lo, &hi)
	if err != nil {
		return 0, 0, false, fmt.Errorf("rollup: reading the range of %s: %w", table, err)
	}
	return lo.Int64, hi.Int64, lo.Valid, nil
}

// floorTo rounds ts down to a multiple of bucket. Timestamps are Unix
// seconds of the panel's own clock, so they are never negative.
func floorTo(ts, bucket int64) int64 {
	if ts < 0 {
		return 0
	}
	return ts / bucket * bucket
}
