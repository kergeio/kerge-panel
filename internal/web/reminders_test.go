package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/reminder"
)

// clockAt makes the panel believe it is t.
func (e *env) clockAt(t time.Time) {
	e.srv.opts.Now = func() time.Time { return t }
}

// callAPI posts to an API route with the page's CSRF token in the header,
// as reminder.js does, and returns the status and the error message.
func (b *browser) callAPI(path, csrf string) (int, string) {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.server.URL+path, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var msg struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &msg)
	return resp.StatusCode, msg.Error
}

// editReminder saves the edit form of a host with the given reminder
// fields and returns the response.
func editReminder(t *testing.T, b *browser, id int64, fields url.Values) (*http.Response, string) {
	t.Helper()
	path := "/hosts/" + strconv.FormatInt(id, 10) + "/edit"
	_, page := b.get(path)
	form := url.Values{"csrf_token": {csrfFrom(t, page)}, "name": {"web-1"}, "note": {""}, "sort": {"0"}}
	for k, v := range fields {
		form[k] = v
	}
	return b.post(path, withEditDefaults(form))
}

func mustDate(t *testing.T, s string) reminder.Date {
	t.Helper()
	d, err := reminder.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The edit page sets, changes and clears the reminder, and shows what is
// stored.
func TestEditPageSetsTheReminder(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	path := "/hosts/" + strconv.FormatInt(id, 10) + "/edit"

	_, page := b.get(path)
	if !strings.Contains(page, `name="remind_start" type="date" value=""`) ||
		strings.Contains(page, "reminder_clear") {
		t.Fatalf("edit page of a host without a reminder:\n%s", page)
	}

	resp, page := editReminder(t, b, id, url.Values{"remind_start": {"2026-10-31"}, "cycle_months": {"3"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", resp.StatusCode, page)
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reminder == nil || rec.Reminder.Start.String() != "2026-10-31" || rec.Reminder.CycleMonths != 3 {
		t.Fatalf("stored: %+v", rec.Reminder)
	}

	_, page = b.get(path)
	if !strings.Contains(page, `value="2026-10-31"`) || !strings.Contains(page, `<option value="3" selected>Every 3 months</option>`) ||
		!strings.Contains(page, "reminder_clear") {
		t.Fatalf("edit page does not show the reminder:\n%s", page)
	}

	// "Clear reminder" turns it off and still saves the other fields.
	resp, page = editReminder(t, b, id, url.Values{
		"remind_start": {"2026-10-31"}, "cycle_months": {"3"}, "reminder_clear": {"1"}, "note": {"cleared"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear: %d\n%s", resp.StatusCode, page)
	}
	if rec, _ = e.hosts.Get(t.Context(), id); rec.Reminder != nil || rec.Note != "cleared" {
		t.Fatalf("after clear: %+v note %q", rec.Reminder, rec.Note)
	}

	// An empty date means no reminder as well.
	if err := e.hosts.SetReminder(t.Context(), id, mustDate(t, "2026-10-01"), 1); err != nil {
		t.Fatal(err)
	}
	if resp, page = editReminder(t, b, id, url.Values{"remind_start": {""}, "cycle_months": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("empty date: %d\n%s", resp.StatusCode, page)
	}
	if rec, _ = e.hosts.Get(t.Context(), id); rec.Reminder != nil {
		t.Fatalf("an empty date kept the reminder: %+v", rec.Reminder)
	}
}

func TestEditPageRejectsBadReminders(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")

	cases := []struct {
		fields url.Values
		want   string
	}{
		{url.Values{"remind_start": {"2026-02-30"}, "cycle_months": {"1"}}, "Enter a valid first reminder date."},
		{url.Values{"remind_start": {"next week"}, "cycle_months": {"1"}}, "Enter a valid first reminder date."},
		{url.Values{"remind_start": {"2026-10-01"}, "cycle_months": {"2"}}, "Choose a reminder cycle from the list."},
		{url.Values{"remind_start": {"2026-10-01"}}, "Choose a reminder cycle from the list."},
	}
	for _, c := range cases {
		c.fields.Set("name", "renamed")
		resp, page := editReminder(t, b, id, c.fields)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, c.want) {
			t.Errorf("%v: %d\n%s", c.fields, resp.StatusCode, page)
		}
		// The form keeps what was typed.
		if d := c.fields.Get("remind_start"); !strings.Contains(page, `value="`+d+`"`) {
			t.Errorf("%v: the form lost the date", c.fields)
		}
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reminder != nil || rec.Name != "web-1" {
		t.Fatalf("a rejected form saved something: %+v %q", rec.Reminder, rec.Name)
	}
}

// The renewal is one field: the due or next reminder date, coloured by
// the fixed thresholds, with how far away it is as the tooltip; a due
// reminder is red and says since when it is due.
func TestRenewalOf(t *testing.T) {
	tr := realCatalog(t).Translator("en")
	today := mustDate(t, "2026-09-23")
	cases := []struct {
		start, acked string
		date, title  string
		class        string
		due, close   bool
	}{
		{"2026-10-01", "", "2026-10-01", "in 8 days", plainClass, false, false},
		{"2026-09-30", "", "2026-09-30", "in 7 days", reminderSoonClass, false, true},
		{"2026-09-27", "", "2026-09-27", "in 4 days", reminderSoonClass, false, true},
		{"2026-09-26", "", "2026-09-26", "in 3 days", reminderUrgentClass, false, true},
		{"2026-09-24", "", "2026-09-24", "in 1 day", reminderUrgentClass, false, true},
		{"2026-09-23", "", "2026-09-23", "Due since 2026-09-23", reminderUrgentClass, true, true},
		{"2026-02-15", "", "2026-09-15", "Due since 2026-09-15", reminderUrgentClass, true, true},
		{"2026-02-15", "2026-09-15", "2026-10-15", "in 22 days", plainClass, false, false},
	}
	for _, c := range cases {
		sched := &reminder.Schedule{Start: mustDate(t, c.start), CycleMonths: 1}
		if c.acked != "" {
			sched.AckedOn = mustDate(t, c.acked)
		}
		r := renewalOf(tr, sched, today)
		if r.Date != c.date || r.Title != c.title || r.Class != c.class || r.Due != c.due || r.Close != c.close || !r.set {
			t.Errorf("start %s acked %q: got %+v", c.start, c.acked, r)
		}
	}
	if r := renewalOf(tr, nil, today); r != (renewal{Date: "None", Class: noneClass}) {
		t.Errorf("no reminder: got %+v", r)
	}
	// The date follows the panel's date format.
	us := tr.WithDateFormat("01/02/2006")
	if r := renewalOf(us, &reminder.Schedule{Start: mustDate(t, "2026-09-15"), CycleMonths: 1}, today); r.Date != "09/15/2026" || r.Title != "Due since 09/15/2026" {
		t.Errorf("US format: got %+v", r)
	}
}

// A due reminder opens the dialog on every signed-in page. "I've renewed"
// ends it for this cycle and the card turns into a countdown; "Remind me
// later" hides it for the day only.
func TestReminderDialogFlow(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	// The reminder is on the 15th, and the operator first looks on
	// 10 April: only 15 March is asked about.
	e.clockAt(time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC))
	b := signedIn(t, e)
	renewed, _ := addHost(t, e, b, "renewed-host")
	later, _ := addHost(t, e, b, "later-host")
	quiet, _ := addHost(t, e, b, "quiet-host")
	for _, id := range []int64{renewed, later} {
		if err := e.hosts.SetReminder(t.Context(), id, mustDate(t, "2026-02-15"), 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.hosts.SetReminder(t.Context(), quiet, mustDate(t, "2026-05-15"), 1); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/hosts", "/settings", "/hosts/" + strconv.FormatInt(quiet, 10)} {
		_, page := b.get(path)
		if !strings.Contains(page, "data-reminders") || !strings.Contains(page, "js/reminder.js") {
			t.Fatalf("%s has no reminder dialog:\n%s", path, page)
		}
		dialog := page[strings.Index(page, "<dialog"):]
		if !strings.Contains(dialog, `data-reminder-host="`+strconv.FormatInt(renewed, 10)+`"`) ||
			!strings.Contains(dialog, `data-reminder-host="`+strconv.FormatInt(later, 10)+`"`) ||
			strings.Contains(dialog, "quiet-host") {
			t.Fatalf("%s lists the wrong hosts:\n%s", path, dialog)
		}
		if !strings.Contains(dialog, "2026-03-15") || strings.Contains(dialog, "2026-02-15") {
			t.Fatalf("%s asks about the wrong date:\n%s", path, dialog)
		}
	}

	_, page := b.get("/")
	if !strings.Contains(page, `title="Due since 2026-03-15"`) || !strings.Contains(page, ">2026-03-15<") {
		t.Fatalf("card does not say the reminder is due:\n%s", page)
	}
	csrf := csrfFrom(t, page)

	if code, _ := b.callAPI("/api/hosts/"+strconv.FormatInt(renewed, 10)+"/reminder/ack", csrf); code != http.StatusNoContent {
		t.Fatalf("ack: %d", code)
	}
	if code, _ := b.callAPI("/api/hosts/"+strconv.FormatInt(later, 10)+"/reminder/snooze", csrf); code != http.StatusNoContent {
		t.Fatalf("snooze: %d", code)
	}

	// Both are out of the dialog today, which leaves no dialog at all.
	_, page = b.get("/hosts")
	if strings.Contains(page, "data-reminders") || strings.Contains(page, "js/reminder.js") {
		t.Fatalf("dialog still shown after answering:\n%s", page)
	}
	if got := e.cardOf(renewed); got.Renewal != "2026-04-15" || got.RenewalTitle != "in 5 days" ||
		got.RenewalClass != reminderSoonClass || got.Tag != "" {
		t.Errorf("renewed card: %+v", got)
	}
	if got := e.cardOf(later); got.Renewal != "2026-03-15" || got.RenewalTitle != "Due since 2026-03-15" ||
		got.RenewalClass != reminderUrgentClass || got.Tag != "" {
		t.Errorf("snoozed card: %+v", got)
	}

	// The next day the snoozed host is asked about again; the renewed one
	// is not.
	e.clockAt(time.Date(2026, 4, 11, 9, 0, 0, 0, time.UTC))
	_, page = b.get("/hosts")
	if !strings.Contains(page, "later-host") || strings.Count(page, "data-reminder-host=") != 1 {
		t.Fatalf("the day after:\n%s", page)
	}

	// Once the renewed host's next date arrives, it is due again.
	e.clockAt(time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC))
	_, page = b.get("/hosts")
	if strings.Count(page, "data-reminder-host=") != 2 || !strings.Contains(page, "2026-04-15") {
		t.Fatalf("on the next reminder date:\n%s", page)
	}
}

// "Today" is the date in the panel's time zone, not in UTC.
func TestReminderUsesThePanelTimeZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	e := newEnv(t, nil)
	e.initialize()
	// 20:00 UTC on 22 September is already 23 September in Tokyo.
	e.clockAt(time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC))
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	if err := e.hosts.SetReminder(t.Context(), id, mustDate(t, "2026-09-23"), 1); err != nil {
		t.Fatal(err)
	}

	if _, page := b.get("/hosts"); strings.Contains(page, "data-reminders") {
		t.Fatal("due in UTC already")
	}
	e.srv.opts.TimeZone = func() *time.Location { return tokyo }
	_, page := b.get("/hosts")
	if !strings.Contains(page, "data-reminders") {
		t.Fatalf("not due in the panel's zone:\n%s", page)
	}
	// Snoozing records Tokyo's date, so the dialog stays away there.
	if code, _ := b.callAPI("/api/hosts/"+strconv.FormatInt(id, 10)+"/reminder/snooze", csrfFrom(t, page)); code != http.StatusNoContent {
		t.Fatalf("snooze: %d", code)
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reminder.SnoozedOn.String() != "2026-09-23" {
		t.Fatalf("snoozed_on = %s", rec.Reminder.SnoozedOn)
	}
}

// The API answers what cannot be done with a status and a translated
// message, and needs a session and the CSRF token.
func TestReminderAPIErrors(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC))
	b := signedIn(t, e)
	id, page := addHost(t, e, b, "web-1")
	csrf := csrfFrom(t, page)
	base := "/api/hosts/" + strconv.FormatInt(id, 10) + "/reminder/"

	for _, action := range []string{"ack", "snooze"} {
		if code, msg := b.callAPI(base+action, csrf); code != http.StatusConflict || msg != "This host has no renewal reminder." {
			t.Errorf("%s without a reminder: %d %q", action, code, msg)
		}
		if code, _ := b.callAPI("/api/hosts/999/reminder/"+action, csrf); code != http.StatusNotFound {
			t.Errorf("%s unknown host: %d", action, code)
		}
		if code, _ := b.callAPI("/api/hosts/x/reminder/"+action, csrf); code != http.StatusNotFound {
			t.Errorf("%s bad id: %d", action, code)
		}
		if code, _ := b.callAPI(base+action, ""); code != http.StatusForbidden {
			t.Errorf("%s without a token: %d", action, code)
		}
	}

	if err := e.hosts.SetReminder(t.Context(), id, mustDate(t, "2026-10-01"), 1); err != nil {
		t.Fatal(err)
	}
	if code, msg := b.callAPI(base+"ack", csrf); code != http.StatusConflict || msg != "This host has no renewal reminder due." {
		t.Errorf("ack before the date: %d %q", code, msg)
	}

	anonymous := e.browser()
	if code, _ := anonymous.callAPI(base+"ack", csrf); code != http.StatusUnauthorized {
		t.Errorf("ack without a session: %d", code)
	}
}

// The live push carries the renewal, so the page follows an answer
// without a reload.
func TestCardJSONCarriesTheRenewal(t *testing.T) {
	body, err := json.Marshal(card{Renewal: "2026-09-01", RenewalTitle: "Due since 2026-09-01", RenewalClass: reminderUrgentClass})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"renewal":"2026-09-01"`, `"renewal_title":"Due since 2026-09-01"`,
		`"renewal_class":"font-medium text-red-700"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("card JSON lacks %s: %s", want, body)
		}
	}
}
