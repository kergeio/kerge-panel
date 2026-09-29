package hosts

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/kergeio/kerge-panel/internal/reminder"
)

func day(t *testing.T, s string) reminder.Date {
	t.Helper()
	d, err := reminder.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func schedule(t *testing.T, s *Service, id int64) *reminder.Schedule {
	t.Helper()
	rec, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Reminder
}

// A reminder is off until set, stores the day of the month with the start
// date, and is off again once cleared.
func TestSetAndClearReminder(t *testing.T) {
	s, db := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := schedule(t, s, id); got != nil {
		t.Fatalf("new host has a reminder: %+v", got)
	}

	if err := s.SetReminder(ctx, id, day(t, "2026-01-31"), 3); err != nil {
		t.Fatal(err)
	}
	got := schedule(t, s, id)
	if got == nil || got.Start.String() != "2026-01-31" || got.CycleMonths != 3 ||
		!got.AckedOn.IsZero() || !got.SnoozedOn.IsZero() {
		t.Fatalf("after set: %+v", got)
	}
	var remindDay int
	if err := db.Read().QueryRowContext(ctx, "SELECT remind_day FROM hosts WHERE id = ?", id).Scan(&remindDay); err != nil {
		t.Fatal(err)
	}
	if remindDay != 31 {
		t.Errorf("remind_day = %d, want 31", remindDay)
	}

	// The list carries the reminder as well.
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Reminder == nil || list[0].Reminder.CycleMonths != 3 {
		t.Fatalf("list: %+v", list)
	}

	if err := s.ClearReminder(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := schedule(t, s, id); got != nil {
		t.Fatalf("after clear: %+v", got)
	}
	var cols int
	if err := db.Read().QueryRowContext(ctx, `SELECT count(*) FROM hosts WHERE id = ? AND
		remind_start IS NULL AND remind_day IS NULL AND cycle_months IS NULL AND
		acked_on IS NULL AND snoozed_on IS NULL`, id).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 1 {
		t.Error("clearing left reminder columns behind")
	}
}

func TestSetReminderRejectsBadInput(t *testing.T) {
	s, _ := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetReminder(ctx, id, reminder.Date{}, 1); err == nil {
		t.Error("accepted a reminder without a start date")
	}
	for _, cycle := range []int{0, 2, 24, -1} {
		if err := s.SetReminder(ctx, id, day(t, "2026-09-23"), cycle); err == nil {
			t.Errorf("accepted cycle %d", cycle)
		}
	}
	if err := s.SetReminder(ctx, id+1, day(t, "2026-09-23"), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown host: %v", err)
	}
	if err := s.ClearReminder(ctx, id+1); !errors.Is(err, ErrNotFound) {
		t.Errorf("clear unknown host: %v", err)
	}
}

// Confirming a renewal stores the due reminder date, which ends the
// reminder until the next date.
func TestAckReminder(t *testing.T) {
	s, _ := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	today := day(t, "2026-04-10")

	if err := s.AckReminder(ctx, id, today); !errors.Is(err, ErrNoReminder) {
		t.Fatalf("no reminder: %v", err)
	}
	if err := s.SetReminder(ctx, id, day(t, "2026-02-15"), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AckReminder(ctx, id, today); err != nil {
		t.Fatal(err)
	}
	got := schedule(t, s, id)
	if got.AckedOn.String() != "2026-03-15" {
		t.Fatalf("acked_on = %s, want the latest reminder date 2026-03-15", got.AckedOn)
	}
	if st := got.At(today); st.Due || st.Date.String() != "2026-04-15" {
		t.Fatalf("after ack: %+v", st)
	}
	// A second confirmation of the same date has nothing to confirm.
	if err := s.AckReminder(ctx, id, today); !errors.Is(err, ErrReminderNotDue) {
		t.Fatalf("second ack: %v", err)
	}
	// Nor has a reminder whose first date is still ahead.
	if err := s.SetReminder(ctx, id, day(t, "2026-05-01"), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AckReminder(ctx, id, today); !errors.Is(err, ErrReminderNotDue) {
		t.Fatalf("ack before the first date: %v", err)
	}
	if err := s.AckReminder(ctx, id+1, today); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
}

// Asking to be reminded later keeps the reminder due but out of the dialog
// for the day.
func TestSnoozeReminder(t *testing.T) {
	s, _ := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	today := day(t, "2026-09-23")
	if err := s.SnoozeReminder(ctx, id, today); !errors.Is(err, ErrNoReminder) {
		t.Fatalf("no reminder: %v", err)
	}
	if err := s.SetReminder(ctx, id, day(t, "2026-09-01"), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SnoozeReminder(ctx, id, today); err != nil {
		t.Fatal(err)
	}
	got := schedule(t, s, id)
	if st := got.At(today); !st.Due || st.Prompt {
		t.Fatalf("snoozed today: %+v", st)
	}
	if st := got.At(day(t, "2026-09-24")); !st.Due || !st.Prompt {
		t.Fatalf("the day after: %+v", st)
	}
	if err := s.SnoozeReminder(ctx, id+1, today); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
}

// Changing the start or the cycle starts a new schedule, which drops the
// confirmation and the snooze of the old one; saving the same values keeps
// them.
func TestChangingTheReminderResetsItsState(t *testing.T) {
	s, _ := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	today := day(t, "2026-09-23")
	arm := func(start string, cycle int) {
		t.Helper()
		if err := s.SetReminder(ctx, id, day(t, start), cycle); err != nil {
			t.Fatal(err)
		}
		if err := s.AckReminder(ctx, id, today); err != nil {
			t.Fatal(err)
		}
		if err := s.SnoozeReminder(ctx, id, today); err != nil {
			t.Fatal(err)
		}
	}

	arm("2026-09-01", 1)
	if err := s.SetReminder(ctx, id, day(t, "2026-09-01"), 1); err != nil {
		t.Fatal(err)
	}
	if got := schedule(t, s, id); got.AckedOn.IsZero() || got.SnoozedOn.IsZero() {
		t.Errorf("saving the same reminder dropped its state: %+v", got)
	}

	if err := s.SetReminder(ctx, id, day(t, "2026-09-02"), 1); err != nil {
		t.Fatal(err)
	}
	if got := schedule(t, s, id); !got.AckedOn.IsZero() || !got.SnoozedOn.IsZero() {
		t.Errorf("a new start kept the old state: %+v", got)
	}

	arm("2026-09-01", 1)
	if err := s.SetReminder(ctx, id, day(t, "2026-09-01"), 3); err != nil {
		t.Fatal(err)
	}
	if got := schedule(t, s, id); !got.AckedOn.IsZero() || !got.SnoozedOn.IsZero() {
		t.Errorf("a new cycle kept the old state: %+v", got)
	}
}

// A malformed date in the database is reported, not read as no reminder.
func TestCorruptReminderIsAnError(t *testing.T) {
	s, db := newService(t)
	ctx := t.Context()
	id, err := s.Create(ctx, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE hosts SET remind_start = '2026-02-30', cycle_months = 1 WHERE id = ?", id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, id); err == nil {
		t.Error("a malformed start date was accepted")
	}
}
