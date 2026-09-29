package hosts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kergeio/kerge-panel/internal/reminder"
)

// Renewal reminders. The dates are stored as 'YYYY-MM-DD' text; "today" is
// always passed in by the caller, who knows the panel's time zone.

var (
	// ErrNoReminder means the host has no renewal reminder.
	ErrNoReminder = errors.New("hosts: the host has no renewal reminder")
	// ErrReminderNotDue means the host's reminder has no date to confirm:
	// the next one is still ahead, or the latest one is already confirmed.
	ErrReminderNotDue = errors.New("hosts: the renewal reminder is not due")
)

// SetReminder turns on the host's renewal reminder, or changes it. The day
// of the month is taken from start. Confirmations and snoozes belong to
// the old schedule, so they are dropped when the start or the cycle
// changes.
func (s *Service) SetReminder(ctx context.Context, id int64, start reminder.Date, cycleMonths int) error {
	if start.IsZero() {
		return errors.New("hosts: the reminder needs a start date")
	}
	if !reminder.ValidCycle(cycleMonths) {
		return fmt.Errorf("hosts: %d is not a reminder cycle", cycleMonths)
	}
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// The CASE reads the row as it was before this update.
		res, err := tx.ExecContext(ctx, `
			UPDATE hosts SET
				acked_on = CASE WHEN remind_start IS ?1 AND cycle_months IS ?2
					THEN acked_on ELSE NULL END,
				snoozed_on = CASE WHEN remind_start IS ?1 AND cycle_months IS ?2
					THEN snoozed_on ELSE NULL END,
				remind_start = ?1, cycle_months = ?2, remind_day = ?3
			WHERE id = ?4`, start.String(), cycleMonths, start.Day, id)
		if err != nil {
			return err
		}
		return requireOneRow(res)
	})
}

// ClearReminder turns the host's renewal reminder off.
func (s *Service) ClearReminder(ctx context.Context, id int64) error {
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE hosts SET remind_start = NULL, remind_day = NULL, cycle_months = NULL,
				acked_on = NULL, snoozed_on = NULL
			WHERE id = ?`, id)
		if err != nil {
			return err
		}
		return requireOneRow(res)
	})
}

// AckReminder records that the operator renewed the host: the due reminder
// date is confirmed, and the host is not reminded again until the next one.
// It fails with ErrReminderNotDue when nothing is due, for example when the
// renewal was already confirmed on another page.
func (s *Service) AckReminder(ctx context.Context, id int64, today reminder.Date) error {
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		sched, err := loadSchedule(ctx, tx, id)
		if err != nil {
			return err
		}
		st := sched.At(today)
		if !st.Due {
			return ErrReminderNotDue
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE hosts SET acked_on = ? WHERE id = ?", st.Date.String(), id)
		return err
	})
}

// SnoozeReminder records that the operator asked to be reminded later: the
// reminder dialog leaves the host out for the rest of today.
func (s *Service) SnoozeReminder(ctx context.Context, id int64, today reminder.Date) error {
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := loadSchedule(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"UPDATE hosts SET snoozed_on = ? WHERE id = ?", today.String(), id)
		return err
	})
}

// loadSchedule reads the reminder of a host inside a write transaction.
func loadSchedule(ctx context.Context, tx *sql.Tx, id int64) (reminder.Schedule, error) {
	var (
		start, acked, snoozed sql.NullString
		cycle                 sql.NullInt64
	)
	err := tx.QueryRowContext(ctx,
		"SELECT remind_start, cycle_months, acked_on, snoozed_on FROM hosts WHERE id = ?", id).
		Scan(&start, &cycle, &acked, &snoozed)
	if errors.Is(err, sql.ErrNoRows) {
		return reminder.Schedule{}, ErrNotFound
	}
	if err != nil {
		return reminder.Schedule{}, err
	}
	sched, err := scanSchedule(start, cycle, acked, snoozed)
	if err != nil {
		return reminder.Schedule{}, err
	}
	if sched == nil {
		return reminder.Schedule{}, ErrNoReminder
	}
	return *sched, nil
}

// scanSchedule builds a reminder from its columns, or nil when the host
// has none.
func scanSchedule(start sql.NullString, cycle sql.NullInt64, acked, snoozed sql.NullString) (*reminder.Schedule, error) {
	if !start.Valid {
		return nil, nil
	}
	var (
		sched reminder.Schedule
		err   error
	)
	if sched.Start, err = reminder.Parse(start.String); err != nil {
		return nil, err
	}
	sched.CycleMonths = int(cycle.Int64)
	if !reminder.ValidCycle(sched.CycleMonths) {
		return nil, fmt.Errorf("invalid cycle %d", cycle.Int64)
	}
	if acked.Valid {
		if sched.AckedOn, err = reminder.Parse(acked.String); err != nil {
			return nil, err
		}
	}
	if snoozed.Valid {
		if sched.SnoozedOn, err = reminder.Parse(snoozed.String); err != nil {
			return nil, err
		}
	}
	return &sched, nil
}
