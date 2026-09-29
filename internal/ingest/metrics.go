package ingest

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/kergeio/kerge-panel/internal/reminder"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-protocol"
	"github.com/kergeio/kerge-protocol/metering"
)

// storeMetrics writes one sample. The row is timestamped with the panel's
// receive time, not the agent's clock.
func (s *Service) storeMetrics(ctx context.Context, hostID int64, m *protocol.Metrics, exclude []string) error {
	received := s.opts.Now()
	rx, tx, haveRate := s.rates.Rate(hostID, m, received, exclude)
	var rxRate, txRate any
	if haveRate {
		rxRate, txRate = rx, tx
	}
	ts := received.Unix()
	err := s.opts.DB.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// A reconnect can deliver two samples within the same second, and
		// the primary key has second resolution: the newer one wins.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO metrics_raw (
				host_id, ts, cpu, mem_used, mem_total, swap_used, swap_total,
				load1, load5, load15, disk_used, disk_total, disk_free,
				rx_rate, tx_rate, uptime)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (host_id, ts) DO UPDATE SET
				cpu = excluded.cpu,
				mem_used = excluded.mem_used, mem_total = excluded.mem_total,
				swap_used = excluded.swap_used, swap_total = excluded.swap_total,
				load1 = excluded.load1, load5 = excluded.load5, load15 = excluded.load15,
				disk_used = excluded.disk_used, disk_total = excluded.disk_total,
				disk_free = excluded.disk_free,
				rx_rate = excluded.rx_rate, tx_rate = excluded.tx_rate,
				uptime = excluded.uptime`,
			hostID, ts, nullable(m.CPUPercent),
			nullable(m.MemUsed), nullable(m.MemTotal),
			nullable(m.SwapUsed), nullable(m.SwapTotal),
			nullable(m.Load1), nullable(m.Load5), nullable(m.Load15),
			nullable(m.DiskUsed), nullable(m.DiskTotal), nullable(m.DiskFree),
			rxRate, txRate, nullable(m.Uptime)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE hosts SET last_seen_at = ? WHERE id = ?", ts, hostID); err != nil {
			return err
		}
		return recordTraffic(ctx, tx, hostID, m, exclude, received, s.opts.TimeZone())
	})
	if err != nil {
		return err
	}
	if s.opts.Status != nil {
		// The live view carries what the cards show, including the rates
		// the panel derived rather than the counters the agent sent.
		sample := status.Sample{
			At: received, CPU: m.CPUPercent,
			MemUsed: m.MemUsed, MemTotal: m.MemTotal,
			SwapUsed: m.SwapUsed, SwapTotal: m.SwapTotal,
			DiskUsed: m.DiskUsed, DiskTotal: m.DiskTotal,
			Uptime: m.Uptime,
		}
		if haveRate {
			sample.RxRate, sample.TxRate = &rx, &tx
		}
		s.opts.Status.Report(hostID, sample)
	}
	return nil
}

// nullable turns an absent optional field into a NULL column.
func nullable[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// recordTraffic adds the traffic of one sample to the host's day and saves
// the counters it was measured from. It runs in the sample's own
// transaction, so the counters and the total always move together, and
// survive restarts of the panel and of the host.
//
// The saved counters are the agent's uint64 values stored bit for bit in a
// signed INTEGER column and read back the same way, which loses nothing.
// The day totals are real byte counts and stop at the largest INTEGER.
func recordTraffic(ctx context.Context, tx *sql.Tx, hostID int64, m *protocol.Metrics,
	exclude []string, received time.Time, zone *time.Location) error {
	if len(m.Net) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT iface, last_rx_bytes, last_tx_bytes FROM traffic_state WHERE host_id = ?", hostID)
	if err != nil {
		return err
	}
	previous := make(map[string]protocol.NetCounters)
	for rows.Next() {
		var (
			name   string
			rx, tx int64
		)
		if err := rows.Scan(&name, &rx, &tx); err != nil {
			rows.Close()
			return err
		}
		previous[name] = protocol.NetCounters{RX: uint64(rx), TX: uint64(tx)}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	rx, txBytes := metering.Traffic(previous, m.Net, exclude)

	// Every reported interface keeps its counters, counted or not, and an
	// interface that stopped being reported keeps its old row.
	for name, c := range m.Net {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO traffic_state (host_id, iface, last_rx_bytes, last_tx_bytes, last_mono_ms, last_ts)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (host_id, iface) DO UPDATE SET
				last_rx_bytes = excluded.last_rx_bytes, last_tx_bytes = excluded.last_tx_bytes,
				last_mono_ms = excluded.last_mono_ms, last_ts = excluded.last_ts`,
			hostID, name, int64(c.RX), int64(c.TX), m.MonoMS, received.Unix()); err != nil {
			return err
		}
	}
	if rx == 0 && txBytes == 0 {
		return nil
	}
	day := reminder.DateOf(received.In(zone)).String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO traffic_daily (host_id, day, rx_bytes, tx_bytes) VALUES (?1, ?2, ?3, ?4)
		ON CONFLICT (host_id, day) DO UPDATE SET
			rx_bytes = CASE WHEN rx_bytes > ?5 - ?3 THEN ?5 ELSE rx_bytes + ?3 END,
			tx_bytes = CASE WHEN tx_bytes > ?5 - ?4 THEN ?5 ELSE tx_bytes + ?4 END`,
		hostID, day, clampBytes(rx), clampBytes(txBytes), int64(math.MaxInt64))
	return err
}

// clampBytes turns a traffic amount into an INTEGER, stopping at the
// largest one.
func clampBytes(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
