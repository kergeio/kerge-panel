package reminder

import (
	"testing"
	"time"
)

func date(t *testing.T, s string) Date {
	t.Helper()
	d, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// occurrences returns the first count reminder dates of a reminder.
func occurrences(start Date, cycleMonths, count int) []string {
	var out []string
	for n := range count {
		out = append(out, Occurrence(start, cycleMonths, n).String())
	}
	return out
}

func assertDates(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("occurrence %d: got %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
}

// A monthly reminder on the 31st falls back to the last day of shorter
// months and returns to the 31st after them.
func TestMonthlyOnThe31st(t *testing.T) {
	got := occurrences(date(t, "2026-01-31"), 1, 8)
	assertDates(t, got, []string{
		"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30",
		"2026-05-31", "2026-06-30", "2026-07-31", "2026-08-31",
	})
}

// February has 29 days in a leap year and 28 otherwise.
func TestFebruaryInLeapAndCommonYears(t *testing.T) {
	cases := []struct {
		start string
		want  string
	}{
		{"2028-01-31", "2028-02-29"}, // divisible by 4
		{"2027-01-31", "2027-02-28"},
		{"2100-01-31", "2100-02-28"}, // divisible by 100 but not 400
		{"2000-01-31", "2000-02-29"}, // divisible by 400
		{"2028-01-30", "2028-02-29"},
		{"2028-01-29", "2028-02-29"},
		{"2027-01-29", "2027-02-28"},
	}
	for _, c := range cases {
		if got := Occurrence(date(t, c.start), 1, 1).String(); got != c.want {
			t.Errorf("month after %s: got %s, want %s", c.start, got, c.want)
		}
	}
}

// A quarterly reminder crosses into the next year.
func TestQuarterlyAcrossTheYear(t *testing.T) {
	got := occurrences(date(t, "2026-11-30"), 3, 5)
	assertDates(t, got, []string{
		"2026-11-30", "2027-02-28", "2027-05-30", "2027-08-30", "2027-11-30",
	})
}

// A half-yearly reminder advances six months at a time.
func TestHalfYearly(t *testing.T) {
	got := occurrences(date(t, "2026-08-31"), 6, 4)
	assertDates(t, got, []string{
		"2026-08-31", "2027-02-28", "2027-08-31", "2028-02-29",
	})
}

// A yearly reminder that starts on 29 February is on the 28th in common
// years and the 29th again in leap years.
func TestYearlyFrom29February(t *testing.T) {
	got := occurrences(date(t, "2024-02-29"), 12, 6)
	assertDates(t, got, []string{
		"2024-02-29", "2025-02-28", "2026-02-28", "2027-02-28", "2028-02-29", "2029-02-28",
	})
}

// Every occurrence comes from the start, so a late start day is not lost
// after a short month, even far into the future.
func TestOccurrenceIsComputedFromTheStart(t *testing.T) {
	start := date(t, "2026-01-31")
	if got := Occurrence(start, 1, 1).String(); got != "2026-02-28" {
		t.Fatalf("got %s", got)
	}
	if got := Occurrence(start, 1, 1200).String(); got != "2126-01-31" {
		t.Fatalf("got %s", got)
	}
}

func TestLastOnOrBefore(t *testing.T) {
	start := date(t, "2026-01-31")
	cases := []struct {
		day  string
		want string // empty: none
	}{
		{"2025-12-31", ""},
		{"2026-01-30", ""},
		{"2026-01-31", "2026-01-31"},
		{"2026-02-27", "2026-01-31"},
		{"2026-02-28", "2026-02-28"},
		{"2026-03-01", "2026-02-28"},
		{"2026-03-30", "2026-02-28"},
		{"2026-03-31", "2026-03-31"},
		{"2026-04-29", "2026-03-31"},
		{"2026-04-30", "2026-04-30"},
		{"2027-01-01", "2026-12-31"},
	}
	for _, c := range cases {
		got, ok := LastOnOrBefore(start, 1, date(t, c.day))
		switch {
		case c.want == "" && ok:
			t.Errorf("on %s: got %s, want none", c.day, got)
		case c.want != "" && !ok:
			t.Errorf("on %s: got none, want %s", c.day, c.want)
		case c.want != "" && got.String() != c.want:
			t.Errorf("on %s: got %s, want %s", c.day, got, c.want)
		}
	}
}

func TestFirstAfter(t *testing.T) {
	start := date(t, "2026-11-30")
	cases := []struct{ day, want string }{
		{"2020-01-01", "2026-11-30"},
		{"2026-11-29", "2026-11-30"},
		{"2026-11-30", "2027-02-28"}, // strictly after
		{"2027-02-27", "2027-02-28"},
		{"2027-02-28", "2027-05-30"},
		{"2027-05-29", "2027-05-30"},
		{"2027-05-30", "2027-08-30"},
	}
	for _, c := range cases {
		if got := FirstAfter(start, 3, date(t, c.day)).String(); got != c.want {
			t.Errorf("after %s: got %s, want %s", c.day, got, c.want)
		}
	}
}

// Missed reminders collapse into the latest one: a reminder on the 15th
// that is first looked at on 10 April points at 15 March, not at 15
// February, and the next one is 15 April.
func TestMissedRemindersPointAtTheLatest(t *testing.T) {
	start := date(t, "2026-02-15")
	today := date(t, "2026-04-10")
	last, ok := LastOnOrBefore(start, 1, today)
	if !ok || last.String() != "2026-03-15" {
		t.Fatalf("last: got %s %v, want 2026-03-15", last, ok)
	}
	if next := FirstAfter(start, 1, today).String(); next != "2026-04-15" {
		t.Fatalf("next: got %s, want 2026-04-15", next)
	}
}

// LastOnOrBefore and FirstAfter agree with a plain walk through the
// occurrences, for every day over several years, every cycle and start
// days that do and do not exist in every month.
func TestAgainstEnumeration(t *testing.T) {
	starts := []string{
		"2026-01-01", "2026-01-15", "2026-01-28", "2026-01-29", "2026-01-30",
		"2026-01-31", "2024-02-29", "2026-03-31", "2026-08-31", "2026-12-31",
	}
	for _, s := range starts {
		start := date(t, s)
		for _, cycle := range []int{1, 3, 6, 12} {
			var occ []Date
			for n := range 12*6/cycle + 2 {
				occ = append(occ, Occurrence(start, cycle, n))
			}
			for i := 1; i < len(occ); i++ {
				if !occ[i].After(occ[i-1]) {
					t.Fatalf("start %s cycle %d: %s is not after %s", s, cycle, occ[i], occ[i-1])
				}
			}

			first := time.Date(start.Year, start.Month, start.Day-40, 0, 0, 0, 0, time.UTC)
			end := time.Date(start.Year+5, start.Month, 1, 0, 0, 0, 0, time.UTC)
			for tm := first; tm.Before(end); tm = tm.AddDate(0, 0, 1) {
				day := DateOf(tm)
				var wantLast *Date
				var wantNext Date
				for i := range occ {
					if !occ[i].After(day) {
						wantLast = &occ[i]
						continue
					}
					wantNext = occ[i]
					break
				}

				last, ok := LastOnOrBefore(start, cycle, day)
				if ok != (wantLast != nil) || (ok && last != *wantLast) {
					t.Fatalf("start %s cycle %d on %s: last %s %v, want %v", s, cycle, day, last, ok, wantLast)
				}
				if next := FirstAfter(start, cycle, day); next != wantNext {
					t.Fatalf("start %s cycle %d after %s: got %s, want %s", s, cycle, day, next, wantNext)
				}
			}
		}
	}
}

// Today is the date in the panel's time zone, which can differ from the
// date in UTC.
func TestDateOfUsesTheLocation(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	now := time.Date(2026, 3, 30, 20, 0, 0, 0, time.UTC)
	if got := DateOf(now).String(); got != "2026-03-30" {
		t.Fatalf("UTC: got %s", got)
	}
	if got := DateOf(now.In(tokyo)).String(); got != "2026-03-31" {
		t.Fatalf("Tokyo: got %s", got)
	}
}

func TestParse(t *testing.T) {
	d, err := Parse("2028-02-29")
	if err != nil || d != (Date{2028, time.February, 29}) {
		t.Fatalf("got %v %v", d, err)
	}
	for _, s := range []string{"", "2027-02-29", "2026-04-31", "2026-13-01", "2026-1-5", "2026-01-05T00:00:00Z", " 2026-01-05"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q: accepted", s)
		}
	}
}

func TestCompare(t *testing.T) {
	order := []string{"2025-12-31", "2026-01-01", "2026-01-02", "2026-02-01", "2027-01-01"}
	for i, a := range order {
		for j, b := range order {
			want := sign(i - j)
			if got := date(t, a).Compare(date(t, b)); got != want {
				t.Errorf("%s vs %s: got %d, want %d", a, b, got, want)
			}
		}
	}
}

func TestInvalidCyclePanics(t *testing.T) {
	for _, f := range []func(){
		func() { Occurrence(Date{2026, 1, 1}, 0, 0) },
		func() { Occurrence(Date{2026, 1, 1}, 1, -1) },
		func() { LastOnOrBefore(Date{2026, 1, 1}, 0, Date{2026, 2, 1}) },
		func() { FirstAfter(Date{2026, 1, 1}, -3, Date{2026, 2, 1}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("no panic")
				}
			}()
			f()
		}()
	}
}

func TestDaysBetween(t *testing.T) {
	cases := []struct {
		from, to string
		want     int
	}{
		{"2026-09-23", "2026-09-23", 0},
		{"2026-09-23", "2026-09-24", 1},
		{"2026-09-24", "2026-09-23", -1},
		{"2026-02-28", "2026-03-01", 1},
		{"2028-02-28", "2028-03-01", 2},
		{"2026-12-31", "2027-01-01", 1},
		{"2026-01-01", "2027-01-01", 365},
		{"2028-01-01", "2029-01-01", 366},
		// Days in a zone with daylight saving time are still whole days.
		{"2026-03-01", "2026-04-01", 31},
		{"2026-10-01", "2026-11-01", 31},
	}
	for _, c := range cases {
		if got := DaysBetween(date(t, c.from), date(t, c.to)); got != c.want {
			t.Errorf("%s to %s: got %d, want %d", c.from, c.to, got, c.want)
		}
	}
}

func TestValidCycle(t *testing.T) {
	for _, m := range []int{1, 3, 6, 12} {
		if !ValidCycle(m) {
			t.Errorf("%d rejected", m)
		}
	}
	for _, m := range []int{-1, 0, 2, 4, 24} {
		if ValidCycle(m) {
			t.Errorf("%d accepted", m)
		}
	}
}

// A reminder is due from its date until the operator confirms the
// renewal, counts down to the next date otherwise, and shows the dialog
// only on days it was not snoozed.
func TestScheduleAt(t *testing.T) {
	type want struct {
		due, prompt bool
		date        string
		days        int
		level       Level
	}
	cases := []struct {
		name           string
		start          string
		cycle          int
		acked, snoozed string
		today          string
		want           want
	}{
		{name: "before the first date", start: "2026-10-15", cycle: 1, today: "2026-09-23",
			want: want{date: "2026-10-15", days: 22, level: Normal}},
		{name: "eight days away", start: "2026-10-01", cycle: 1, today: "2026-09-23",
			want: want{date: "2026-10-01", days: 8, level: Normal}},
		{name: "seven days away", start: "2026-09-30", cycle: 1, today: "2026-09-23",
			want: want{date: "2026-09-30", days: 7, level: Soon}},
		{name: "four days away", start: "2026-09-27", cycle: 1, today: "2026-09-23",
			want: want{date: "2026-09-27", days: 4, level: Soon}},
		{name: "three days away", start: "2026-09-26", cycle: 1, today: "2026-09-23",
			want: want{date: "2026-09-26", days: 3, level: Urgent}},
		{name: "tomorrow", start: "2026-09-24", cycle: 1, today: "2026-09-23",
			want: want{date: "2026-09-24", days: 1, level: Urgent}},
		{name: "due today", start: "2026-09-23", cycle: 1, today: "2026-09-23",
			want: want{due: true, prompt: true, date: "2026-09-23", level: Urgent}},
		{name: "due since last month", start: "2026-08-23", cycle: 12, today: "2026-09-23",
			want: want{due: true, prompt: true, date: "2026-08-23", level: Urgent}},
		{name: "missed dates collapse into the latest", start: "2026-02-15", cycle: 1, today: "2026-04-10",
			want: want{due: true, prompt: true, date: "2026-03-15", level: Urgent}},
		{name: "snoozed today", start: "2026-09-01", cycle: 1, snoozed: "2026-09-23", today: "2026-09-23",
			want: want{due: true, date: "2026-09-01", level: Urgent}},
		{name: "snoozed yesterday", start: "2026-09-01", cycle: 1, snoozed: "2026-09-22", today: "2026-09-23",
			want: want{due: true, prompt: true, date: "2026-09-01", level: Urgent}},
		{name: "acked this date", start: "2026-09-01", cycle: 1, acked: "2026-09-01", today: "2026-09-23",
			want: want{date: "2026-10-01", days: 8, level: Normal}},
		{name: "acked the date before", start: "2026-08-01", cycle: 1, acked: "2026-08-01", today: "2026-09-23",
			want: want{due: true, prompt: true, date: "2026-09-01", level: Urgent}},
		{name: "acked on the date itself", start: "2026-09-23", cycle: 3, acked: "2026-09-23", today: "2026-09-23",
			want: want{date: "2026-12-23", days: 91, level: Normal}},
		{name: "31st falls back in the countdown", start: "2026-01-31", cycle: 1, acked: "2026-08-31", today: "2026-09-23",
			want: want{date: "2026-09-30", days: 7, level: Soon}},
	}
	for _, c := range cases {
		s := Schedule{Start: date(t, c.start), CycleMonths: c.cycle}
		if c.acked != "" {
			s.AckedOn = date(t, c.acked)
		}
		if c.snoozed != "" {
			s.SnoozedOn = date(t, c.snoozed)
		}
		got := s.At(date(t, c.today))
		w := c.want
		if got.Due != w.due || got.Prompt != w.prompt || got.Date.String() != w.date ||
			got.Days != w.days || got.Level() != w.level {
			t.Errorf("%s: got %+v level %d, want %+v", c.name, got, got.Level(), w)
		}
	}
}
