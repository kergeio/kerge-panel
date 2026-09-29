package i18n

import (
	"math"
	"strconv"
	"time"
)

// Unit symbols are international notation and are not translated.
var (
	byteUnits = []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	rateUnits = []string{"K/s", "M/s", "G/s", "T/s"}
)

const (
	timeLayout  = "15:04"
	clockLayout = "15:04:05"
)

// DateFormats are the ways dates can be written, as Go layouts. The first
// is the default. Each has a short form without the year, in the same
// order.
var DateFormats = []string{"2006-01-02", "01/02/2006", "02/01/2006", "02.01.2006"}

var shortDateFormats = map[string]string{
	"2006-01-02": "01-02",
	"01/02/2006": "01/02",
	"02/01/2006": "02/01",
	"02.01.2006": "02.01",
}

// DefaultDateFormat is the date format until the operator picks one.
const DefaultDateFormat = "2006-01-02"

// ValidDateFormat reports whether f is one of DateFormats.
func ValidDateFormat(f string) bool {
	_, ok := shortDateFormats[f]
	return ok
}

// ShortDateFormat is the form of f without the year.
func ShortDateFormat(f string) string {
	if s, ok := shortDateFormats[f]; ok {
		return s
	}
	return shortDateFormats[DefaultDateFormat]
}

// WithDateFormat returns a translator that writes dates in format f, or
// in the default format when f is not one of DateFormats.
func (t *Translator) WithDateFormat(f string) *Translator {
	c := *t
	if ValidDateFormat(f) {
		c.dates = f
	} else {
		c.dates = DefaultDateFormat
	}
	return &c
}

// dateLayout is the layout of the translator's dates.
func (t *Translator) dateLayout() string {
	if t.dates == "" {
		return DefaultDateFormat
	}
	return t.dates
}

// Bytes formats a byte count with binary (1024-based) units, e.g. "1.5 GiB".
func (t *Translator) Bytes(n uint64) string {
	if n < 1024 {
		return strconv.FormatUint(n, 10) + " B"
	}
	v := float64(n) / 1024
	i := 0
	for roundTenth(v) >= 1024 && i < len(byteUnits)-1 {
		v /= 1024
		i++
	}
	return t.decimal(v) + " " + byteUnits[i]
}

// ByteRate formats a rate in bytes per second with binary (1024-based)
// units, like Bytes, e.g. "12.3 M/s".
func (t *Translator) ByteRate(bytesPerSec float64) string {
	v := math.Max(bytesPerSec, 0)
	if math.Round(v) < 1024 {
		return strconv.FormatFloat(v, 'f', 0, 64) + " B/s"
	}
	v /= 1024
	i := 0
	for roundTenth(v) >= 1024 && i < len(rateUnits)-1 {
		v /= 1024
		i++
	}
	return t.decimal(v) + " " + rateUnits[i]
}

// Percent formats p (0-100) with one decimal place, e.g. "42.5%".
func (t *Translator) Percent(p float64) string {
	return t.decimal(p) + "%"
}

// Date formats the calendar date of tm in loc in the translator's date
// format, e.g. "2026-09-19".
func (t *Translator) Date(tm time.Time, loc *time.Location) string {
	return tm.In(loc).Format(t.dateLayout())
}

// DateTime formats tm in loc to the minute, 24-hour clock, e.g.
// "2026-09-19 14:03".
func (t *Translator) DateTime(tm time.Time, loc *time.Location) string {
	return tm.In(loc).Format(t.dateLayout() + " " + timeLayout)
}

// MonthDay formats the month and day of tm in loc, e.g. "09-19", for
// dates that are close enough to need no year.
func (t *Translator) MonthDay(tm time.Time, loc *time.Location) string {
	return tm.In(loc).Format(ShortDateFormat(t.dateLayout()))
}

// Clock formats the time of day of tm in loc to the second, 24-hour
// clock, e.g. "14:03:27".
func (t *Translator) Clock(tm time.Time, loc *time.Location) string {
	return tm.In(loc).Format(clockLayout)
}

// decimal formats v with one decimal place. It is the single place to add
// locale-specific decimal separators when a language needs them.
func (t *Translator) decimal(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// roundTenth rounds v to one decimal place, matching decimal's output, so
// unit promotion never yields values like "1024.0 KiB".
func roundTenth(v float64) float64 { return math.Round(v*10) / 10 }
