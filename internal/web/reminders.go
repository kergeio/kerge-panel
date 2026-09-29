package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/reminder"
)

// Renewal reminders. The dashboard shows where each reminder stands,
// every signed-in page carries the dialog of the due ones, and the
// dialog's two buttons post to the API below.

// dueReminder is one line of the reminder dialog.
type dueReminder struct {
	ID   int64
	Name string
	Date string
}

// Reminder levels map to fixed colours. The classes are listed in
// web/input.css as well, because they are picked here.
const (
	reminderUrgentClass = "font-medium text-red-700"
	reminderSoonClass   = "font-medium text-amber-700"
)

// cycleLabels names the reminder cycles offered on the edit page.
var cycleLabels = map[int]msgKey{
	1:  msgKey("reminder.cycle.1"),
	3:  msgKey("reminder.cycle.3"),
	6:  msgKey("reminder.cycle.6"),
	12: msgKey("reminder.cycle.12"),
}

// now returns the current time.
func (s *Server) now() time.Time {
	if s.opts.Now == nil {
		return time.Now()
	}
	return s.opts.Now()
}

// today is the current date in the panel's time zone, which is the day
// every reminder is judged on.
func (s *Server) today() reminder.Date {
	return reminder.DateOf(s.now().In(s.location()))
}

// formatDay writes a calendar date the way every page writes dates.
func formatDay(tr translator, d reminder.Date) string {
	return tr.Date(d.Time(), time.UTC)
}

// renewal is what the panel says about a host's reminder: one field, the
// same on the cards, the list and the detail page.
type renewal struct {
	// Date is the due reminder date, or else the next one, or "None"
	// without a reminder; Class colours it and Title says how far away
	// it is, e.g. "in 6 days" or "Due since 2026-09-20".
	Date  string
	Title string
	Class string
	// Due and Close report whether the reminder is due, and whether it
	// is due or at most seven days away.
	Due   bool
	Close bool
	// at is the date behind Date; set reports whether there is a
	// reminder at all.
	at  reminder.Date
	set bool
}

// renewalOf describes a reminder as of today.
func renewalOf(tr translator, sched *reminder.Schedule, today reminder.Date) renewal {
	if sched == nil {
		return renewal{Date: tr.T(string(msgKey("common.none"))), Class: noneClass}
	}
	st := sched.At(today)
	r := renewal{Date: formatDay(tr, st.Date), Class: plainClass, Due: st.Due, at: st.Date, set: true}
	switch {
	case st.Due:
		r.Title = strings.ReplaceAll(tr.T(string(msgKey("reminder.due_since"))), "{date}", r.Date)
	case st.Days == 1:
		r.Title = tr.T(string(msgKey("reminder.card.in_day")))
	default:
		r.Title = strings.ReplaceAll(tr.T(string(msgKey("reminder.card.in_days"))), "{n}", strconv.Itoa(st.Days))
	}
	switch st.Level() {
	case reminder.Urgent:
		r.Class, r.Close = reminderUrgentClass, true
	case reminder.Soon:
		r.Class, r.Close = reminderSoonClass, true
	}
	return r
}

// dueReminders lists the hosts the reminder dialog asks about: those with
// a due reminder that was not snoozed today. The dialog is an addition to
// the page, so a failure leaves it out instead of failing the page.
func (s *Server) dueReminders(ctx context.Context) []dueReminder {
	records, err := s.opts.Hosts.List(ctx)
	if err != nil {
		s.opts.Logger.Error("listing due reminders", "err", err)
		return nil
	}
	tr := s.translator()
	today := s.today()
	var out []dueReminder
	for _, rec := range records {
		if rec.Reminder == nil {
			continue
		}
		if st := rec.Reminder.At(today); st.Prompt {
			out = append(out, dueReminder{ID: rec.ID, Name: rec.Name, Date: formatDay(tr, st.Date)})
		}
	}
	return out
}

// reminderAck confirms the renewal of a host (POST /api/hosts/{id}/reminder/ack).
func (s *Server) reminderAck(w http.ResponseWriter, r *http.Request) {
	s.reminderAction(w, r, s.opts.Hosts.AckReminder)
}

// reminderSnooze keeps a host out of the dialog for the rest of the day
// (POST /api/hosts/{id}/reminder/snooze).
func (s *Server) reminderSnooze(w http.ResponseWriter, r *http.Request) {
	s.reminderAction(w, r, s.opts.Hosts.SnoozeReminder)
}

func (s *Server) reminderAction(w http.ResponseWriter, r *http.Request,
	act func(context.Context, int64, reminder.Date) error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusNotFound, msgKey("error.not_found"))
		return
	}
	switch err := act(r.Context(), id, s.today()); {
	case errors.Is(err, hosts.ErrNotFound):
		s.apiError(w, http.StatusNotFound, msgKey("error.not_found"))
	case errors.Is(err, hosts.ErrNoReminder):
		s.apiError(w, http.StatusConflict, msgKey("error.reminder.none"))
	case errors.Is(err, hosts.ErrReminderNotDue):
		s.apiError(w, http.StatusConflict, msgKey("error.reminder.not_due"))
	case err != nil:
		s.opts.Logger.Error("request failed", "path", r.URL.Path, "err", err)
		s.apiError(w, http.StatusInternalServerError, msgKey("error.internal"))
	default:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// cycleOptions lists the reminder cycles with selected marked.
func (s *Server) cycleOptions(selected string) []option {
	tr := s.translator()
	out := make([]option, 0, len(reminder.Cycles))
	for _, c := range reminder.Cycles {
		v := strconv.Itoa(c)
		out = append(out, option{Value: v, Label: tr.T(string(cycleLabels[c])), Selected: v == selected})
	}
	return out
}
