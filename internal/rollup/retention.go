package rollup

import (
	"context"
	"database/sql"
	"slices"
	"strconv"

	"github.com/kergeio/kerge-panel/internal/store"
)

// Retention is how long each metrics table is kept.
type Retention struct {
	// RawHours keeps metrics_raw, MinuteDays metrics_1m and HourDays
	// metrics_1h.
	RawHours   int
	MinuteDays int
	HourDays   int
}

// The values the settings page offers. Anything else is refused, so a
// stored value can never let a table grow without bound.
var (
	RawHoursChoices   = []int{6, 12, 24, 48, 72}
	MinuteDaysChoices = []int{3, 7, 14, 30}
	HourDaysChoices   = []int{30, 90, 180, 365}
)

// Default is what applies until the operator chooses otherwise.
var Default = Retention{RawHours: 24, MinuteDays: 7, HourDays: 90}

// Valid reports whether every field is one of the offered values.
func (r Retention) Valid() bool {
	return slices.Contains(RawHoursChoices, r.RawHours) &&
		slices.Contains(MinuteDaysChoices, r.MinuteDays) &&
		slices.Contains(HourDaysChoices, r.HourDays)
}

// LoadRetention reads the stored retention. A value that is missing or no
// longer offered falls back to the default for that table, so the panel
// keeps pruning even if the settings row is damaged.
func LoadRetention(ctx context.Context, db *store.DB) (Retention, error) {
	r := Default
	for _, f := range []struct {
		key     string
		field   *int
		choices []int
	}{
		{store.SettingRetentionRawHours, &r.RawHours, RawHoursChoices},
		{store.SettingRetentionMinuteDays, &r.MinuteDays, MinuteDaysChoices},
		{store.SettingRetentionHourDays, &r.HourDays, HourDaysChoices},
	} {
		value, ok, err := db.GetSetting(ctx, f.key)
		if err != nil {
			return Default, err
		}
		if !ok {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil || !slices.Contains(f.choices, n) {
			continue
		}
		*f.field = n
	}
	return r, nil
}

// StoreRetention writes r inside tx. The caller has already checked it
// with Valid.
func StoreRetention(ctx context.Context, tx *sql.Tx, r Retention) error {
	for _, f := range []struct {
		key   string
		value int
	}{
		{store.SettingRetentionRawHours, r.RawHours},
		{store.SettingRetentionMinuteDays, r.MinuteDays},
		{store.SettingRetentionHourDays, r.HourDays},
	} {
		if err := store.SetSetting(ctx, tx, f.key, strconv.Itoa(f.value)); err != nil {
			return err
		}
	}
	return nil
}
