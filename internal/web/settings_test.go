package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/rollup"
	"github.com/kergeio/kerge-panel/internal/store"
)

// settingsForm returns the browser's current settings page and its CSRF
// token.
func settingsForm(t *testing.T, b *browser) (string, string) {
	t.Helper()
	resp, page := b.get("/settings")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings page: %d", resp.StatusCode)
	}
	return page, csrfFrom(t, page)
}

func dataForm(token, raw, minute, hour, exclude string) url.Values {
	return url.Values{
		"csrf_token":          {token},
		"retention_raw_hours": {raw},
		"retention_1m_days":   {minute},
		"retention_1h_days":   {hour},
		"net_iface_exclude":   {exclude},
	}
}

func TestSettingsShowsTheDefaults(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	page, _ := settingsForm(t, b)

	// The stored retention is selected, and the exclusion field shows the
	// built-in list with the note that nothing has been chosen yet.
	if !strings.Contains(page, `<option value="24" selected>24 hours</option>`) {
		t.Error("the raw retention does not default to 24 hours")
	}
	if !strings.Contains(page, `value="lo,docker*,veth*,br-*,cni*,flannel*,cali*,kube-*,virbr*,vnet*,tun*,tap*,wg*,tailscale*,zt*,dummy*"`) {
		t.Error("the exclusion field does not show the built-in list")
	}
	if !strings.Contains(page, "built-in list") {
		t.Error("the page does not say that the built-in list applies")
	}
}

func TestSaveDataSettings(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)

	resp, page := b.post("/settings/data", dataForm(token, "48", "14", "365", "lo, eth1"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Settings saved") {
		t.Fatalf("save: %d\n%s", resp.StatusCode, page)
	}
	got, err := rollup.LoadRetention(t.Context(), e.db)
	if err != nil {
		t.Fatal(err)
	}
	if want := (rollup.Retention{RawHours: 48, MinuteDays: 14, HourDays: 365}); got != want {
		t.Errorf("stored retention = %+v, want %+v", got, want)
	}
	value, ok, err := e.db.GetSetting(t.Context(), store.SettingNetIfaceExclude)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "lo, eth1" {
		t.Errorf("stored exclusion list = %q, %v; want \"lo, eth1\"", value, ok)
	}

	// The saved values come back selected, and the built-in note is gone.
	page, _ = settingsForm(t, b)
	if !strings.Contains(page, `<option value="48" selected>2 days</option>`) {
		t.Error("the saved raw retention is not selected")
	}
	if strings.Contains(page, "built-in list") {
		t.Error("the page still claims the built-in list applies")
	}
}

// An empty list is a choice of its own: nothing is excluded.
func TestSaveEmptyExclusionList(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)

	if resp, page := b.post("/settings/data", dataForm(token, "24", "7", "90", "  ")); resp.StatusCode != http.StatusOK {
		t.Fatalf("save: %d\n%s", resp.StatusCode, page)
	}
	value, ok, err := e.db.GetSetting(t.Context(), store.SettingNetIfaceExclude)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "" {
		t.Errorf("stored exclusion list = %q, %v; want an empty list", value, ok)
	}
	exclude, err := e.hosts.IfaceExclude(t.Context(), mustAddHost(t, e))
	if err != nil {
		t.Fatal(err)
	}
	if len(exclude) != 0 {
		t.Errorf("hosts still exclude %q", exclude)
	}
}

func mustAddHost(t *testing.T, e *env) int64 {
	t.Helper()
	id, err := e.hosts.Create(t.Context(), "web-1")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSettingsRejectBadValues(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)

	for name, c := range map[string]struct {
		form    url.Values
		message string
	}{
		"retention not offered": {dataForm(token, "9999", "7", "90", "lo"), "retention period"},
		"retention empty":       {dataForm(token, "", "7", "90", "lo"), "retention period"},
		"pattern with a space":  {dataForm(token, "24", "7", "90", "eth 0"), "printable characters"},
		"empty pattern":         {dataForm(token, "24", "7", "90", "lo,,eth0"), "printable characters"},
		"pattern too long":      {dataForm(token, "24", "7", "90", strings.Repeat("e", 33)), "printable characters"},
	} {
		resp, page := b.post("/settings/data", c.form)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, c.message) {
			t.Errorf("%s: %d, message %q missing", name, resp.StatusCode, c.message)
		}
	}
	// Nothing was stored by any of the refused requests.
	if _, ok, err := e.db.GetSetting(t.Context(), store.SettingNetIfaceExclude); ok || err != nil {
		t.Errorf("a refused request stored an exclusion list: %v", err)
	}
	if got, err := rollup.LoadRetention(t.Context(), e.db); err != nil || got != rollup.Default {
		t.Errorf("a refused request changed the retention: %+v, %v", got, err)
	}
}

// A refused save shows the values that were sent back, not the stored
// ones, so the operator can correct what they typed.
func TestRefusedSaveKeepsTheTypedValues(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)

	_, page := b.post("/settings/data", dataForm(token, "48", "14", "90", "eth 0"))
	if !strings.Contains(page, `value="eth 0"`) {
		t.Error("the typed exclusion list is not shown again")
	}
	if !strings.Contains(page, `<option value="48" selected>2 days</option>`) {
		t.Error("the chosen raw retention is not shown again")
	}
}

func TestSaveDisplaySettings(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)

	form := url.Values{"csrf_token": {token}, "language": {"en"}, "timezone": {"Asia/Tokyo"}, "date_format": {"02/01/2006"}}
	resp, page := b.post("/settings/display", form)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Settings saved") {
		t.Fatalf("save: %d\n%s", resp.StatusCode, page)
	}
	value, ok, err := e.db.GetSetting(t.Context(), store.SettingTimeZone)
	if err != nil || !ok || value != "Asia/Tokyo" {
		t.Fatalf("stored time zone = %q, %v, %v", value, ok, err)
	}
	if page, _ = settingsForm(t, b); !strings.Contains(page, `<option value="Asia/Tokyo" selected>`) {
		t.Error("the saved time zone is not selected")
	}
	if value, _, _ := e.db.GetSetting(t.Context(), store.SettingDateFormat); value != "02/01/2006" {
		t.Fatalf("stored date format = %q", value)
	}
	// The formats are offered as the dates they make, the saved one chosen.
	for _, want := range []string{`<option value="2006-01-02">2026-09-30</option>`, `<option value="01/02/2006">09/30/2026</option>`,
		`<option value="02/01/2006" selected>30/09/2026</option>`, `<option value="02.01.2006">30.09.2026</option>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the date format list lacks %s", want)
		}
	}

	for name, bad := range map[string]url.Values{
		"unknown language": {"csrf_token": {token}, "language": {"xx"}, "timezone": {"UTC"}},
		"unknown zone":     {"csrf_token": {token}, "language": {"en"}, "timezone": {"Mars/Olympus"}},
		"empty zone":       {"csrf_token": {token}, "language": {"en"}, "timezone": {""}, "date_format": {"2006-01-02"}},
		"unknown format":   {"csrf_token": {token}, "language": {"en"}, "timezone": {"UTC"}, "date_format": {"2006/01/02"}},
		"empty format":     {"csrf_token": {token}, "language": {"en"}, "timezone": {"UTC"}, "date_format": {""}},
	} {
		if resp, _ := b.post("/settings/display", bad); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d, want 422", name, resp.StatusCode)
		}
	}
	// The refused requests left the stored zone alone.
	if value, _, _ := e.db.GetSetting(t.Context(), store.SettingTimeZone); value != "Asia/Tokyo" {
		t.Errorf("stored time zone = %q after refused requests", value)
	}
}

func TestSettingsNeedASessionAndAToken(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)

	// No session.
	for _, path := range []string{"/settings", "/settings/data", "/settings/display"} {
		method := "GET"
		if path != "/settings" {
			method = "POST"
		}
		if rec := e.do(method, path, "", url.Values{"csrf_token": {token}}); rec.Code != http.StatusFound {
			t.Errorf("%s %s without a session: %d, want a redirect to the login page", method, path, rec.Code)
		}
	}
	// A session but no CSRF token.
	for _, path := range []string{"/settings/data", "/settings/display"} {
		if resp, _ := b.post(path, url.Values{"language": {"en"}}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without a token: %d, want 403", path, resp.StatusCode)
		}
	}
	if got, err := rollup.LoadRetention(t.Context(), e.db); err != nil || got != rollup.Default {
		t.Errorf("an unauthenticated request changed the retention: %+v, %v", got, err)
	}
}

// Every date on every page follows the date format, with and without
// the year, and the scripts get it for the dates they write.
func TestDatesFollowTheDateFormat(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 23, 9, 30, 0, 0, time.UTC))
	b := signedIn(t, e)
	id := reporting(t, e, "web-1")
	if err := e.hosts.SetReminder(t.Context(), id, mustDate(t, "2026-09-30"), 1); err != nil {
		t.Fatal(err)
	}
	e.srv.opts.DateFormat = func() string { return "02.01.2006" }

	_, home := b.get("/")
	for _, want := range []string{`>30.09.2026<`, `data-date-format="02.01.2006"`, `Next: web-1 · 30.09`, `>Wed 23.09.2026<`} {
		if !strings.Contains(home, want) {
			t.Errorf("the dashboard lacks %q", want)
		}
	}
	_, detail := b.get("/hosts/" + strconv.FormatInt(id, 10))
	for _, want := range []string{`title="in 7 days">30.09.2026<`, `"date_format":"02.01.2006"`, `"short_date_format":"02.01"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail page lacks %q", want)
		}
	}
	if strings.Contains(detail, "2026-09-") {
		t.Error("the detail page still writes an ISO date")
	}
	// An unknown stored format falls back to the default.
	e.srv.opts.DateFormat = func() string { return "nonsense" }
	if _, home := b.get("/"); !strings.Contains(home, ">2026-09-30<") {
		t.Error("an unknown format does not fall back to the default")
	}
}
