package hosts

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kergeio/kerge-panel/internal/reminder"
	"github.com/kergeio/kerge-panel/internal/traffic"
	"github.com/kergeio/kerge-protocol/ifacefilter"
)

// SetTraffic saves the traffic accounting of a host. The traffic already
// counted is kept per day, so a new reset day applies to it at once.
func (s *Service) SetTraffic(ctx context.Context, id int64, settings traffic.Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	var limit any
	if settings.LimitBytes > 0 {
		limit = settings.LimitBytes
	}
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE hosts SET traffic_mode = ?, traffic_reset_day = ?, traffic_limit_bytes = ?
			WHERE id = ?`, string(settings.Mode), settings.ResetDay, limit, id)
		if err != nil {
			return err
		}
		return requireOneRow(res)
	})
}

// SetIfaceExclude saves the host's own interface exclusion list, which
// replaces the panel's, or with nil makes the host follow the panel's rules
// again. The list is checked the way the settings page checks the panel's;
// an empty one excludes nothing.
func (s *Service) SetIfaceExclude(ctx context.Context, id int64, list *string) error {
	var value any
	if list != nil {
		trimmed := strings.TrimSpace(*list)
		if _, err := ifacefilter.ValidatePatterns(trimmed); err != nil {
			return err
		}
		value = trimmed
	}
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE hosts SET net_iface_exclude = ? WHERE id = ?", value, id)
		if err != nil {
			return err
		}
		return requireOneRow(res)
	})
}

// DailyTraffic returns the traffic of every host per day, from since on.
func (s *Service) DailyTraffic(ctx context.Context, since reminder.Date) ([]traffic.Day, error) {
	rows, err := s.db.Read().QueryContext(ctx,
		"SELECT host_id, day, rx_bytes, tx_bytes FROM traffic_daily WHERE day >= ?", since.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []traffic.Day
	for rows.Next() {
		var (
			d   traffic.Day
			day string
		)
		if err := rows.Scan(&d.HostID, &day, &d.RX, &d.TX); err != nil {
			return nil, err
		}
		if d.Day, err = reminder.Parse(day); err != nil {
			return nil, fmt.Errorf("hosts: traffic of host %d: %w", d.HostID, err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
