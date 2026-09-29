// Package traffic holds the panel's traffic accounting rules: the billing
// cycle, which direction counts, and the optional limit. How the reported
// counters turn into traffic is defined by the protocol's metering package;
// what arrives is stored per day, and a cycle's usage is the sum of its
// days.
package traffic

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/reminder"
)

// Mode is which direction of traffic counts toward the usage.
type Mode string

const (
	// Out counts transmitted traffic only.
	Out Mode = "out"
	// In counts received traffic only.
	In Mode = "in"
	// Both counts both directions.
	Both Mode = "both"
)

// Modes lists the modes in the order the edit page offers them.
var Modes = []Mode{Both, Out, In}

// Defaults for a host whose traffic settings were never saved.
const (
	DefaultMode     = Both
	DefaultResetDay = 1
)

// MaxResetDay is the last day a cycle may start on, so that every month
// has it.
const MaxResetDay = 28

// bytesPerGiB converts the limit the operator types into bytes.
const bytesPerGiB = 1 << 30

// MinLimitGiB and MaxLimitGiB bound the limit. The minimum is the
// smallest value the edit page writes back with its two decimals; the
// maximum keeps the byte count far inside an INTEGER column.
const (
	MinLimitGiB = 0.01
	MaxLimitGiB = 1 << 20
)

// Settings is the traffic configuration of one host.
type Settings struct {
	Mode     Mode
	ResetDay int
	// LimitBytes is the cycle's allowance, or 0 for none.
	LimitBytes int64
}

// Default returns the settings of a host that has none saved.
func Default() Settings {
	return Settings{Mode: DefaultMode, ResetDay: DefaultResetDay}
}

// ValidMode reports whether m is one of Modes.
func ValidMode(m Mode) bool {
	return m == Out || m == In || m == Both
}

// ValidResetDay reports whether d can start a cycle.
func ValidResetDay(d int) bool {
	return d >= 1 && d <= MaxResetDay
}

// Validate checks every field.
func (s Settings) Validate() error {
	switch {
	case !ValidMode(s.Mode):
		return errors.New("traffic: unknown mode")
	case !ValidResetDay(s.ResetDay):
		return errors.New("traffic: the reset day must be between 1 and 28")
	case s.LimitBytes < 0 || s.LimitBytes > int64(MaxLimitGiB*bytesPerGiB):
		return errors.New("traffic: the limit is out of range")
	}
	return nil
}

// CycleStart returns the first day of the cycle that today belongs to: the
// latest reset day on or before today. today is a date in the panel's
// time zone.
func CycleStart(today reminder.Date, resetDay int) reminder.Date {
	if today.Day >= resetDay {
		return reminder.Date{Year: today.Year, Month: today.Month, Day: resetDay}
	}
	// The reset day is at most the 28th, so the previous month has it.
	prev := time.Date(today.Year, today.Month-1, 1, 0, 0, 0, 0, time.UTC)
	return reminder.Date{Year: prev.Year(), Month: prev.Month(), Day: resetDay}
}

// EarliestCycleStart is the earliest day any cycle containing today can
// start on, whatever its reset day. Reading the days from here on covers
// every host's current cycle.
func EarliestCycleStart(today reminder.Date) reminder.Date {
	earliest := CycleStart(today, 1)
	for d := 2; d <= MaxResetDay; d++ {
		if start := CycleStart(today, d); start.Before(earliest) {
			earliest = start
		}
	}
	return earliest
}

// Used returns the traffic that counts under the mode.
func (s Settings) Used(rx, tx int64) int64 {
	switch s.Mode {
	case Out:
		return tx
	case In:
		return rx
	default:
		return saturatingAdd(rx, tx)
	}
}

// Day is the traffic of one host on one day.
type Day struct {
	HostID int64
	Day    reminder.Date
	RX, TX int64
}

// Sum adds up the days of hostID from start on.
func Sum(days []Day, hostID int64, start reminder.Date) (rx, tx int64) {
	for _, d := range days {
		if d.HostID == hostID && !d.Day.Before(start) {
			rx = saturatingAdd(rx, d.RX)
			tx = saturatingAdd(tx, d.TX)
		}
	}
	return rx, tx
}

// ParseLimitGiB reads the limit as the edit page takes it: a number of GiB,
// possibly with decimals, or nothing for no limit.
func ParseLimitGiB(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	g, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(g) || g < MinLimitGiB || g > MaxLimitGiB {
		return 0, errors.New("traffic: the limit must be between 0.01 and 1048576 GiB")
	}
	return int64(math.Round(g * bytesPerGiB)), nil
}

// FormatLimitGiB writes a limit for the edit page, in GiB with at most two
// decimals, or nothing for no limit.
func FormatLimitGiB(b int64) string {
	if b <= 0 {
		return ""
	}
	return strconv.FormatFloat(math.Round(float64(b)/bytesPerGiB*100)/100, 'f', -1, 64)
}

// saturatingAdd adds two non-negative amounts, stopping at the largest
// int64 instead of overflowing.
func saturatingAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
