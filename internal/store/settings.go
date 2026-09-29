package store

import (
	"context"
	"database/sql"
	"errors"
)

// Setting keys.
const (
	SettingLanguage = "language"
	SettingTimeZone = "timezone"
	// SettingNetIfaceExclude holds the comma-separated default interface
	// exclusion patterns. Unset means ifacefilter.DefaultExclude.
	SettingNetIfaceExclude = "net_iface_exclude"
	// SettingRevealIPs is "1" while host addresses are shown across the
	// panel and "0" while they are masked. Unset means masked.
	SettingRevealIPs = "reveal_ips"
	// SettingDashboardView is how the dashboard lists the hosts: "cards" or
	// "list". Unset means cards.
	SettingDashboardView = "dashboard_view"
	// SettingDateFormat is how dates are written, as one of
	// i18n.DateFormats. Unset means the first.
	SettingDateFormat = "date_format"
	// How long each metrics table is kept, as a whole number of hours or
	// days. Unset or unreadable means the default.
	SettingRetentionRawHours   = "retention_raw_hours"
	SettingRetentionMinuteDays = "retention_1m_days"
	SettingRetentionHourDays   = "retention_1h_days"
	// Where summarizing has got to, as a Unix second: every bucket that
	// ends at or before this value has been written to the coarser table.
	// These two are the panel's own bookkeeping, not operator settings.
	SettingRollupMinuteThrough = "rollup_1m_through"
	SettingRollupHourThrough   = "rollup_1h_through"
)

// GetSetting returns the value of key and whether it is set.
func (d *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := d.read.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting inserts or replaces key inside tx.
func SetSetting(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
