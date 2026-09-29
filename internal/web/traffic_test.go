package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/traffic"
)

// addDay stores traffic of one host on one day, as the agent endpoint does.
func (e *env) addDay(hostID int64, day string, rx, tx int64) {
	e.t.Helper()
	if err := e.db.Write(e.t.Context(), func(ctx context.Context, tx2 *sql.Tx) error {
		_, err := tx2.ExecContext(ctx,
			"INSERT INTO traffic_daily (host_id, day, rx_bytes, tx_bytes) VALUES (?, ?, ?, ?)",
			hostID, day, rx, tx)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// cardOf returns the dashboard card of one host.
func (e *env) cardOf(id int64) card {
	e.t.Helper()
	cards, _, err := e.srv.cards(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == id {
			return c
		}
	}
	e.t.Fatalf("no card for host %d", id)
	return card{}
}

// The edit page shows the defaults of a host that never saved traffic
// settings, saves new ones, and shows them back.
func TestEditPageSetsTraffic(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	path := "/hosts/" + strconv.FormatInt(id, 10) + "/edit"

	_, page := b.get(path)
	for _, want := range []string{
		`<option value="1" selected>1</option>`,
		`<option value="28">28</option>`,
		`<option value="both" selected>Inbound and outbound</option>`,
		`name="traffic_limit" type="text" inputmode="decimal" value=""`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("defaults: no %q in\n%s", want, page)
		}
	}

	resp, page := b.post(path, withEditDefaults(url.Values{
		"csrf_token": {csrfFrom(t, page)}, "name": {"web-1"}, "sort": {"0"},
		"traffic_reset_day": {"15"}, "traffic_mode": {"out"}, "traffic_limit": {"931.32"},
	}))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", resp.StatusCode, page)
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := traffic.ParseLimitGiB("931.32")
	if rec.Traffic != (traffic.Settings{Mode: traffic.Out, ResetDay: 15, LimitBytes: want}) {
		t.Fatalf("stored: %+v", rec.Traffic)
	}

	_, page = b.get(path)
	for _, want := range []string{
		`<option value="15" selected>15</option>`,
		`<option value="out" selected>Outbound only</option>`,
		`value="931.32"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("saved values: no %q in\n%s", want, page)
		}
	}

	// Emptying the limit removes it.
	resp, _ = b.post(path, withEditDefaults(url.Values{
		"csrf_token": {csrfFrom(t, page)}, "name": {"web-1"}, "sort": {"0"},
		"traffic_reset_day": {"15"}, "traffic_mode": {"out"}, "traffic_limit": {""},
	}))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear limit: %d", resp.StatusCode)
	}
	if rec, _ = e.hosts.Get(t.Context(), id); rec.Traffic.LimitBytes != 0 {
		t.Fatalf("limit kept: %+v", rec.Traffic)
	}
}

func TestEditPageRejectsBadTraffic(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	path := "/hosts/" + strconv.FormatInt(id, 10) + "/edit"
	_, page := b.get(path)
	token := csrfFrom(t, page)

	cases := []struct {
		fields url.Values
		want   string
	}{
		{url.Values{"traffic_reset_day": {"0"}}, "Choose a reset day from 1 to 28."},
		{url.Values{"traffic_reset_day": {"29"}}, "Choose a reset day from 1 to 28."},
		{url.Values{"traffic_reset_day": {"first"}}, "Choose a reset day from 1 to 28."},
		{url.Values{"traffic_mode": {"sideways"}}, "Choose which traffic counts from the list."},
		{url.Values{"traffic_limit": {"0"}}, "The traffic limit must be"},
		{url.Values{"traffic_limit": {"-5"}}, "The traffic limit must be"},
		{url.Values{"traffic_limit": {"lots"}}, "The traffic limit must be"},
		{url.Values{"traffic_limit": {"2000000"}}, "The traffic limit must be"},
	}
	for _, c := range cases {
		form := withEditDefaults(url.Values{"csrf_token": {token}, "name": {"renamed"}, "sort": {"0"}})
		for k, v := range c.fields {
			form[k] = v
		}
		resp, page := b.post(path, form)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, c.want) {
			t.Errorf("%v: %d\n%s", c.fields, resp.StatusCode, page)
		}
		// The form keeps what was typed.
		if l := c.fields.Get("traffic_limit"); l != "" && !strings.Contains(page, `value="`+l+`"`) {
			t.Errorf("%v: the form lost the limit", c.fields)
		}
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "web-1" || rec.Traffic != traffic.Default() {
		t.Fatalf("a rejected form saved something: %q %+v", rec.Name, rec.Traffic)
	}
}

// The card shows the traffic of the current cycle under the host's mode,
// without the limit; moving the reset day recounts the cycle from the
// stored days.
func TestCardShowsTheCycleTraffic(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	other, _ := addHost(t, e, b, "web-2")

	// A host that never had an agent shows nothing, like its other fields.
	if c := e.cardOf(id); c.Traffic != "—" {
		t.Errorf("pending host without traffic: %q", c.Traffic)
	}

	const gib = 1 << 30
	e.addDay(id, "2026-08-31", 100*gib, 100*gib) // the previous cycle
	e.addDay(id, "2026-09-01", gib/2, gib)
	e.addDay(id, "2026-09-20", gib/2, 2*gib)
	e.addDay(other, "2026-09-10", 7*gib, 7*gib)

	if c := e.cardOf(id); c.TrafficLabel != "Traffic ↑↓" || c.TrafficTitle != "Counted: in and out" || c.Traffic != "4.0 GiB" {
		t.Errorf("defaults: %q %q %q", c.TrafficLabel, c.TrafficTitle, c.Traffic)
	}
	if err := e.hosts.SetTraffic(t.Context(), id, traffic.Settings{Mode: traffic.Out, ResetDay: 1, LimitBytes: 10 * gib}); err != nil {
		t.Fatal(err)
	}
	// The limit is on the detail page, not on the dashboard.
	if c := e.cardOf(id); c.TrafficLabel != "Traffic ↑" || c.TrafficTitle != "Counted: out only" || c.Traffic != "3.0 GiB" {
		t.Errorf("outbound with a limit: %q %q", c.TrafficLabel, c.Traffic)
	}
	if err := e.hosts.SetTraffic(t.Context(), id, traffic.Settings{Mode: traffic.In, ResetDay: 15}); err != nil {
		t.Fatal(err)
	}
	if c := e.cardOf(id); c.TrafficLabel != "Traffic ↓" || c.TrafficTitle != "Counted: in only" || c.Traffic != "512.0 MiB" {
		t.Errorf("inbound from the 15th: %q %q", c.TrafficLabel, c.Traffic)
	}
	// Reset on the 28th: the cycle began on 28 August, before the day
	// counted into the previous cycle above.
	if err := e.hosts.SetTraffic(t.Context(), id, traffic.Settings{Mode: traffic.Both, ResetDay: 28}); err != nil {
		t.Fatal(err)
	}
	if c := e.cardOf(id); c.Traffic != "204.0 GiB" {
		t.Errorf("from 28 August: %q", c.Traffic)
	}
	if c := e.cardOf(other); c.Traffic != "14.0 GiB" {
		t.Errorf("other host: %q", c.Traffic)
	}

	_, page := b.get("/")
	// The label names the direction with arrows, the tooltip in words.
	if n := strings.Count(page, `title="Counted: in and out">Traffic ↑↓<`); n != 4 {
		t.Errorf("the both-ways label appears %d times, want 4 (card and row of two hosts)", n)
	}
	if !strings.Contains(page, `data-field="traffic" class="tabular-nums">204.0 GiB</dd>`) ||
		!strings.Contains(page, `data-field="traffic" class="truncate text-sm font-semibold tabular-nums">204.0 GiB</div>`) {
		t.Fatalf("dashboard:\n%s", page)
	}
}

// The cycle follows the panel's time zone: on the evening of the 30th in
// UTC it is already the 1st in Tokyo, where a new cycle has begun.
func TestCardCycleFollowsThePanelTimeZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC))
	id := reporting(t, e, "web-1")
	e.addDay(id, "2026-09-30", 1<<30, 0)

	if c := e.cardOf(id); c.Traffic != "1.0 GiB" {
		t.Errorf("UTC: %q", c.Traffic)
	}
	e.srv.opts.TimeZone = func() *time.Location { return tokyo }
	if c := e.cardOf(id); c.Traffic != "0 B" {
		t.Errorf("Tokyo: %q", c.Traffic)
	}
}

// The live push carries the traffic line.
func TestCardJSONCarriesTheTraffic(t *testing.T) {
	raw, err := json.Marshal(card{Traffic: "3.0 GiB", TrafficLabel: "Traffic ↑", TrafficTitle: "Counted: out only"})
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"traffic":"3.0 GiB"`) || strings.Contains(body, "Counted") {
		t.Fatalf("card JSON: %s", body)
	}
}
