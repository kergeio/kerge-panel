package traffic

import (
	"math"
	"testing"

	"github.com/kergeio/kerge-panel/internal/reminder"
)

func date(t *testing.T, s string) reminder.Date {
	t.Helper()
	d, err := reminder.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A cycle starts on the latest reset day on or before today.
func TestCycleStart(t *testing.T) {
	cases := []struct {
		today    string
		resetDay int
		want     string
	}{
		{"2026-09-23", 1, "2026-09-01"},
		{"2026-09-01", 1, "2026-09-01"},
		{"2026-09-23", 23, "2026-09-23"},
		{"2026-09-22", 23, "2026-08-23"},
		{"2026-09-23", 28, "2026-08-28"},
		{"2026-01-05", 15, "2025-12-15"},
		{"2026-03-01", 28, "2026-02-28"},
		{"2028-03-10", 28, "2028-02-28"},
		{"2026-12-31", 28, "2026-12-28"},
	}
	for _, c := range cases {
		if got := CycleStart(date(t, c.today), c.resetDay).String(); got != c.want {
			t.Errorf("today %s reset %d: got %s, want %s", c.today, c.resetDay, got, c.want)
		}
	}
}

// Reading from the earliest start covers the current cycle of every reset
// day.
func TestEarliestCycleStart(t *testing.T) {
	for _, today := range []string{"2026-09-01", "2026-09-15", "2026-09-27", "2026-09-28", "2026-09-30", "2026-01-03", "2026-03-01"} {
		d := date(t, today)
		earliest := EarliestCycleStart(d)
		for r := 1; r <= MaxResetDay; r++ {
			if CycleStart(d, r).Before(earliest) {
				t.Errorf("today %s: reset day %d starts before %s", today, r, earliest)
			}
		}
		found := false
		for r := 1; r <= MaxResetDay; r++ {
			found = found || CycleStart(d, r) == earliest
		}
		if !found {
			t.Errorf("today %s: %s is no cycle's start", today, earliest)
		}
	}
}

func TestUsed(t *testing.T) {
	cases := []struct {
		mode Mode
		want int64
	}{
		{Out, 20},
		{In, 10},
		{Both, 30},
	}
	for _, c := range cases {
		if got := (Settings{Mode: c.mode}).Used(10, 20); got != c.want {
			t.Errorf("%s: got %d, want %d", c.mode, got, c.want)
		}
	}
	if got := (Settings{Mode: Both}).Used(math.MaxInt64, 5); got != math.MaxInt64 {
		t.Errorf("sum overflowed: %d", got)
	}
}

// A cycle's usage is the sum of the host's days from its start on.
func TestSum(t *testing.T) {
	days := []Day{
		{HostID: 1, Day: date(t, "2026-08-31"), RX: 1000, TX: 1000},
		{HostID: 1, Day: date(t, "2026-09-01"), RX: 10, TX: 20},
		{HostID: 1, Day: date(t, "2026-09-23"), RX: 1, TX: 2},
		{HostID: 2, Day: date(t, "2026-09-10"), RX: 500, TX: 500},
	}
	if rx, tx := Sum(days, 1, date(t, "2026-09-01")); rx != 11 || tx != 22 {
		t.Errorf("host 1 from 09-01: %d %d", rx, tx)
	}
	if rx, tx := Sum(days, 1, date(t, "2026-08-23")); rx != 1011 || tx != 1022 {
		t.Errorf("host 1 from 08-23: %d %d", rx, tx)
	}
	if rx, tx := Sum(days, 3, date(t, "2026-01-01")); rx != 0 || tx != 0 {
		t.Errorf("unknown host: %d %d", rx, tx)
	}
	huge := []Day{{HostID: 1, Day: date(t, "2026-09-01"), RX: math.MaxInt64}, {HostID: 1, Day: date(t, "2026-09-02"), RX: 1}}
	if rx, _ := Sum(huge, 1, date(t, "2026-09-01")); rx != math.MaxInt64 {
		t.Errorf("sum overflowed: %d", rx)
	}
}

// Moving the reset day recomputes the current cycle from the stored days,
// which is why they are kept per day.
func TestMovingTheResetDay(t *testing.T) {
	today := date(t, "2026-09-23")
	var days []Day
	for d := 1; d <= 23; d++ {
		days = append(days, Day{HostID: 1, Day: reminder.Date{Year: 2026, Month: 9, Day: d}, RX: 1})
	}
	if rx, _ := Sum(days, 1, CycleStart(today, 1)); rx != 23 {
		t.Errorf("reset on the 1st: %d", rx)
	}
	if rx, _ := Sum(days, 1, CycleStart(today, 15)); rx != 9 {
		t.Errorf("reset on the 15th: %d", rx)
	}
}

func TestSettingsValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Errorf("defaults: %v", err)
	}
	good := []Settings{
		{Mode: Out, ResetDay: 28, LimitBytes: 1},
		{Mode: In, ResetDay: 1, LimitBytes: MaxLimitGiB * bytesPerGiB},
	}
	for _, s := range good {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := []Settings{
		{Mode: "sideways", ResetDay: 1},
		{Mode: Both, ResetDay: 0},
		{Mode: Both, ResetDay: 29},
		{Mode: Both, ResetDay: 1, LimitBytes: -1},
		{Mode: Both, ResetDay: 1, LimitBytes: MaxLimitGiB*bytesPerGiB + 1},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

func TestLimitGiB(t *testing.T) {
	cases := []struct {
		in    string
		bytes int64
		back  string
	}{
		{"", 0, ""},
		{"  ", 0, ""},
		{"1", 1 << 30, "1"},
		{"1000", 1000 << 30, "1000"},
		{"931.32", int64(math.Round(931.32 * (1 << 30))), "931.32"},
		{"0.5", 1 << 29, "0.5"},
		{"0.01", int64(math.Round(0.01 * (1 << 30))), "0.01"},
		{" 2 ", 2 << 30, "2"},
	}
	for _, c := range cases {
		b, err := ParseLimitGiB(c.in)
		if err != nil || b != c.bytes {
			t.Errorf("%q: got %d %v, want %d", c.in, b, err, c.bytes)
			continue
		}
		if got := FormatLimitGiB(b); got != c.back {
			t.Errorf("%q: written back as %q, want %q", c.in, got, c.back)
		}
	}
	for _, s := range []string{"0", "-1", "0.001", "abc", "NaN", "Inf", "1048577", "1,5"} {
		if _, err := ParseLimitGiB(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
