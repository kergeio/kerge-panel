// Package reminder computes the renewal reminder dates of a host and
// where its reminder stands on a given day.
//
// A reminder works like a repeating alarm: it goes off on remind_start and
// then every cycle months after it, on the same day of the month, or on the
// last day of a month that has no such day. Every occurrence is derived
// from remind_start, never from the one before it, so a 31st that falls
// back to the 28th in February is the 31st again in March.
//
// Everything here works on calendar dates. "Today" is the date in the
// panel's time zone, which the caller resolves with DateOf.
package reminder

import (
	"fmt"
	"slices"
	"time"
)

// Date is a calendar date without a time of day or a time zone. The zero
// value is not a valid date.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// dateLayout is how a date is written, in the database and on the wire.
const dateLayout = "2006-01-02"

// DateOf returns the calendar date of t in t's own location. To get today
// in the panel's time zone, pass time.Now().In(zone).
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}
}

// Parse reads a date written as YYYY-MM-DD and rejects dates that do not
// exist, such as 2026-02-30.
func Parse(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q", s)
	}
	return DateOf(t), nil
}

// String writes the date as YYYY-MM-DD.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// Compare returns -1 if d is before e, 0 if they are the same date and +1
// if d is after e.
func (d Date) Compare(e Date) int {
	switch {
	case d.Year != e.Year:
		return sign(d.Year - e.Year)
	case d.Month != e.Month:
		return sign(int(d.Month) - int(e.Month))
	default:
		return sign(d.Day - e.Day)
	}
}

// IsZero reports whether d is the zero value, which stands for "no date".
func (d Date) IsZero() bool { return d == Date{} }

// Before reports whether d is before e.
func (d Date) Before(e Date) bool { return d.Compare(e) < 0 }

// After reports whether d is after e.
func (d Date) After(e Date) bool { return d.Compare(e) > 0 }

// Occurrence returns the n-th reminder date (n ≥ 0) of a reminder that
// starts on start and repeats every cycleMonths months: the month is
// cycleMonths × n months after start's, and the day is start's day, or the
// last day of that month if it is shorter.
//
// cycleMonths must be positive and n must not be negative; anything else
// is a programming error and panics.
func Occurrence(start Date, cycleMonths, n int) Date {
	if cycleMonths < 1 || n < 0 {
		panic(fmt.Sprintf("reminder: occurrence %d of a %d-month cycle", n, cycleMonths))
	}
	months := int(start.Month) - 1 + n*cycleMonths
	year, month := start.Year+months/12, time.Month(months%12+1)
	return Date{Year: year, Month: month, Day: min(start.Day, daysIn(year, month))}
}

// LastOnOrBefore returns the latest reminder date that is not after day.
// It reports false when day is before start, which is the first one.
func LastOnOrBefore(start Date, cycleMonths int, day Date) (Date, bool) {
	n, ok := lastIndex(start, cycleMonths, day)
	if !ok {
		return Date{}, false
	}
	return Occurrence(start, cycleMonths, n), true
}

// FirstAfter returns the earliest reminder date that is strictly after
// day.
func FirstAfter(start Date, cycleMonths int, day Date) Date {
	n, ok := lastIndex(start, cycleMonths, day)
	if !ok {
		return Occurrence(start, cycleMonths, 0)
	}
	return Occurrence(start, cycleMonths, n+1)
}

// lastIndex returns the index of the latest reminder date that is not
// after day, and false when there is none.
func lastIndex(start Date, cycleMonths int, day Date) (int, bool) {
	if cycleMonths < 1 {
		panic(fmt.Sprintf("reminder: %d-month cycle", cycleMonths))
	}
	if day.Before(start) {
		return 0, false
	}
	// The occurrence with this index falls in day's month or earlier. If
	// it is in day's month but after day, the one before it is in an
	// earlier month and therefore the answer. Index 0 is start itself,
	// which is not after day.
	months := (day.Year-start.Year)*12 + int(day.Month) - int(start.Month)
	n := months / cycleMonths
	if Occurrence(start, cycleMonths, n).After(day) {
		n--
	}
	return n, true
}

// DaysBetween returns the number of days from d to e, negative when e is
// before d.
func DaysBetween(d, e Date) int {
	return int(e.Time().Sub(d.Time()).Hours() / 24)
}

// Time returns the start of d in UTC, where every day has 24 hours. It is
// for formatting and arithmetic, not a moment in the panel's time zone.
func (d Date) Time() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// Cycles are the reminder periods the panel offers, in months: monthly,
// quarterly, half-yearly and yearly.
var Cycles = []int{1, 3, 6, 12}

// ValidCycle reports whether m is one of Cycles.
func ValidCycle(m int) bool {
	return slices.Contains(Cycles, m)
}

// Schedule is the reminder of one host as the database keeps it. A zero
// AckedOn or SnoozedOn means the operator has not acted on it.
type Schedule struct {
	Start       Date
	CycleMonths int
	// AckedOn is the reminder date the operator last confirmed as
	// renewed.
	AckedOn Date
	// SnoozedOn is the day the operator last asked to be reminded later.
	SnoozedOn Date
}

// Level is how urgently a reminder asks for attention. The thresholds are
// fixed.
type Level int

const (
	// Normal is a reminder more than a week away.
	Normal Level = iota
	// Soon is a reminder at most 7 days away.
	Soon
	// Urgent is a reminder at most 3 days away, or one that is due.
	Urgent
)

// Countdown thresholds in days.
const (
	soonDays   = 7
	urgentDays = 3
)

// Status is where a reminder stands on one day.
type Status struct {
	// Due is set when a reminder date has passed and the operator has not
	// confirmed the renewal yet. Date is then that reminder date, the
	// latest one: missed dates before it are not reminded separately.
	Due bool
	// Prompt is set when a due reminder should open the reminder dialog,
	// which is every day it has not been snoozed on.
	Prompt bool
	// Date is the due reminder date, or else the next one.
	Date Date
	// Days is the number of days until Date when the reminder is not due.
	Days int
}

// Level returns how the reminder is coloured.
func (s Status) Level() Level {
	switch {
	case s.Due || s.Days <= urgentDays:
		return Urgent
	case s.Days <= soonDays:
		return Soon
	default:
		return Normal
	}
}

// At returns the status of the reminder on day today, which is a date in
// the panel's time zone.
func (s Schedule) At(today Date) Status {
	if last, ok := LastOnOrBefore(s.Start, s.CycleMonths, today); ok {
		if s.AckedOn.IsZero() || s.AckedOn.Before(last) {
			return Status{Due: true, Prompt: s.SnoozedOn != today, Date: last}
		}
	}
	next := FirstAfter(s.Start, s.CycleMonths, today)
	return Status{Date: next, Days: DaysBetween(today, next)}
}

// daysIn returns the number of days in the month.
func daysIn(year int, month time.Month) int {
	// Day 0 of the next month is the last day of this one.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	default:
		return 0
	}
}
