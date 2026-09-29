package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/settings"
)

// Online counts only the hosts shown as online; every other state counts
// towards the total and marks the block.
func TestOverviewCountsOnlineHosts(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	if ov := e.overview(); ov.Online != "0 / 0" || ov.OnlineNote != "No hosts yet" || ov.OnlineAlert {
		t.Errorf("no hosts: %+v", ov)
	}
	up := reporting(t, e, "web-1")
	if ov := e.overview(); ov.Online != "1 / 1" || ov.OnlineNote != "All hosts reporting" || ov.OnlineAlert {
		t.Errorf("one online: %+v", ov)
	}
	if _, err := e.hosts.Create(t.Context(), "not-installed"); err != nil {
		t.Fatal(err)
	}
	if ov := e.overview(); ov.Online != "1 / 2" || ov.OnlineNote != "1 not online" || !ov.OnlineAlert {
		t.Errorf("with a pending host: %+v", ov)
	}
	// A host whose data is late is not counted as online either.
	late := reporting(t, e, "web-2")
	e.status.Report(late, sample(time.Now().Add(-2*time.Minute)))
	e.status.Report(up, sample(time.Now()))
	if ov := e.overview(); ov.Online != "1 / 3" || ov.OnlineNote != "2 not online" {
		t.Errorf("with a delayed host: %+v", ov)
	}
}

// Today's traffic adds up every host from midnight in the panel's time
// zone; the current rates add up the hosts that are online.
func TestOverviewTraffic(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC))
	a := reporting(t, e, "web-1")
	b := reporting(t, e, "web-2")
	const mib = 1 << 20
	e.addDay(a, "2026-09-29", 100*mib, 100*mib)
	e.addDay(a, "2026-09-30", 2*mib, 1*mib)
	e.addDay(b, "2026-09-30", 4*mib, 3*mib)
	e.addDay(b, "2026-10-01", 8*mib, 8*mib)

	ov := e.overview()
	if ov.TodayDown != "6.0 MiB" || ov.TodayUp != "4.0 MiB" {
		t.Errorf("UTC: %+v", ov)
	}
	// Two samples of 1500 and 2500 bytes a second.
	if ov.NowDown != "2.9 K/s" || ov.NowUp != "4.9 K/s" || ov.NowShort != "↑ 4.9 ↓ 2.9 K/s" {
		t.Errorf("rates: %+v", ov)
	}
	e.srv.opts.TimeZone = func() *time.Location { return tokyo }
	if ov := e.overview(); ov.TodayDown != "8.0 MiB" || ov.TodayUp != "8.0 MiB" {
		t.Errorf("Tokyo: %+v", ov)
	}
	e.status.Disconnected(b)
	if ov := e.overview(); ov.NowDown != "1.5 K/s" {
		t.Errorf("rates without the offline host: %+v", ov)
	}
}

// The renewal block counts reminders due or at most seven days away and
// names the next one, or the ones due, or says there are none; only a due
// reminder marks it.
func TestOverviewRenewals(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC))
	b := signedIn(t, e)
	far, _ := addHost(t, e, b, "far")
	soon, _ := addHost(t, e, b, "soon")
	addHost(t, e, b, "none")
	if ov := e.overview(); ov.Renewals != "0" || ov.RenewalsNote != "No reminders set" || ov.RenewalsAlert {
		t.Errorf("no reminders: %+v", ov)
	}
	if err := e.hosts.SetReminder(t.Context(), far, mustDate(t, "2026-10-20"), 1); err != nil {
		t.Fatal(err)
	}
	if ov := e.overview(); ov.Renewals != "0" || ov.RenewalsNote != "Next: far · 10-20" {
		t.Errorf("one far away: %+v", ov)
	}
	if err := e.hosts.SetReminder(t.Context(), soon, mustDate(t, "2026-09-30"), 1); err != nil {
		t.Fatal(err)
	}
	if ov := e.overview(); ov.Renewals != "1" || ov.RenewalsNote != "Next: soon · 09-30" || ov.RenewalsAlert {
		t.Errorf("one close: %+v", ov)
	}
	if err := e.hosts.SetReminder(t.Context(), far, mustDate(t, "2026-09-01"), 1); err != nil {
		t.Fatal(err)
	}
	if ov := e.overview(); ov.Renewals != "2" || ov.RenewalsNote != "1 due now" || !ov.RenewalsAlert {
		t.Errorf("one due: %+v", ov)
	}
}

// The short pair names the unit once only when both rates share it.
func TestShortRates(t *testing.T) {
	tr := realCatalog(t).Translator("en")
	if got := shortRates(tr, "1.2 M/s", "3.4 M/s"); got != "↑ 1.2 ↓ 3.4 M/s" {
		t.Errorf("same unit: %q", got)
	}
	if got := shortRates(tr, "900 B/s", "1.2 K/s"); got != "↑ 900 B/s ↓ 1.2 K/s" {
		t.Errorf("different units: %q", got)
	}
}

// The clock starts from the panel's time and zone.
func TestOverviewClock(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 23, 22, 4, 5, 0, time.UTC))
	e.srv.opts.TimeZone = func() *time.Location { return tokyo }
	b := signedIn(t, e)
	_, page := b.get("/")
	for _, want := range []string{`data-time-zone="Asia/Tokyo"`, ">07:04:05<", "Asia/Tokyo", ">Thu 2026-09-24<"} {
		if !strings.Contains(page, want) {
			t.Errorf("the clock lacks %q", want)
		}
	}
}

// The choice between cards and list is kept by the panel, so every page
// and every session shows the same view.
func TestDashboardViewSwitch(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	reporting(t, e, "web-1")
	_, page := b.get("/")
	csrf := csrfFrom(t, page)

	if code := b.postSetting("/api/settings/view", url.Values{"view": {"list"}}, csrf); code != http.StatusNoContent {
		t.Fatalf("choosing the list: %d", code)
	}
	for _, br := range []*browser{b, signedIn(t, e)} {
		_, page := br.get("/")
		if !strings.Contains(page, `data-views="list" class="group"`) ||
			!strings.Contains(page, `data-view-button="list" aria-pressed="true"`) {
			t.Errorf("the list is not shown:\n%s", page)
		}
	}

	for _, value := range []string{"", "table", "LIST"} {
		if code := b.postSetting("/api/settings/view", url.Values{"view": {value}}, csrf); code != http.StatusBadRequest {
			t.Errorf("view=%q: %d, want 400", value, code)
		}
	}
	if code := b.postSetting("/api/settings/view", url.Values{"view": {"cards"}}, ""); code != http.StatusForbidden {
		t.Errorf("without a token: %d, want 403", code)
	}
	if view, _ := e.settings.DashboardView(t.Context()); view != settings.ViewList {
		t.Errorf("a rejected request changed the view to %q", view)
	}
	if code := b.postSetting("/api/settings/view", url.Values{"view": {"cards"}}, csrf); code != http.StatusNoContent {
		t.Fatalf("choosing the cards: %d", code)
	}
	if view, _ := e.settings.DashboardView(t.Context()); view != settings.ViewCards {
		t.Errorf("view = %q, want cards", view)
	}
}

// overview returns the overview as the dashboard would show it now.
func (e *env) overview() overview {
	e.t.Helper()
	_, ov, err := e.srv.dashboardData(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	return ov
}
