package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"html"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/traffic"
	"github.com/kergeio/kerge-protocol"
)

// addBucket writes one summarized minute for a host, as the rollup would.
// The charts read these tables, never the raw samples.
func addBucket(t *testing.T, e *env, hostID int64, at time.Time, cpu, cpuMax, load1 float64) {
	t.Helper()
	err := e.db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO metrics_1m (host_id, ts, cpu, cpu_max,
				mem_used, mem_used_max, mem_total, swap_used, swap_total,
				load1, load1_max, load5, load15,
				disk_used, disk_used_max, disk_total, disk_free,
				rx_rate, rx_rate_max, tx_rate, tx_rate_max, uptime)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			hostID, at.Unix(), cpu, cpuMax,
			512, 640, 1024, 0, 0,
			load1, load1, load1, load1,
			100, 120, 200, 100,
			1000.0, 2000.0, 500.0, 900.0, 3600)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The detail page describes the host, renders one frame per trend and
// carries no readings itself.
func TestHostDetailPage(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")

	resp, page := b.get("/hosts/" + strconv.FormatInt(id, 10))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail page: %d", resp.StatusCode)
	}
	for _, want := range []string{"web-1", `data-charts`, `id="chart-config"`, "uPlot",
		// One frame per chart, each saying which column it draws.
		`data-chart="cpu"`, `data-chart="mem"`, `data-chart="load"`, `data-chart="disk"`,
		`data-chart="download"`, `data-chart="upload"`,
		`data-mean="cpu"`, `data-mean="rx_rate"`, `data-mean="tx_rate"`,
		// Memory and disk are drawn against what the host has, CPU
		// against 100 percent.
		`data-capacity="mem_total"`, `data-capacity="disk_total"`, `data-axis-max="100"`,
		// The hover readout, the statistic drop-down with the mean chosen,
		// and the refresh button.
		`data-chart-tip`, `data-stat-menu`, `data-chart-stat="mean" aria-current="true"`,
		`data-chart-stat="max" aria-current="false"`, `<span data-stat-label>Mean</span>`,
		`data-chart-refresh`,
		// A title says which reading its one line draws, and in what
		// unit, because the axis labels carry only a prefix.
		"CPU usage", "Memory used (bytes)", "Disk used (bytes)",
		"Network download (B/s)", "Network upload (B/s)"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %q:\n%s", want, page)
		}
	}
	// Swap has no chart, and there is no legend under the charts.
	for _, unwanted := range []string{`data-chart="swap"`, `data-chart="net"`, "data-series"} {
		if strings.Contains(page, unwanted) {
			t.Errorf("the page contains %q:\n%s", unwanted, page)
		}
	}
	for _, span := range []string{"1h", "24h", "7d", "30d", "90d"} {
		if !strings.Contains(page, "?range="+span) {
			t.Errorf("the page offers no %s range:\n%s", span, page)
		}
	}
	// The readings are fetched by the script, not shipped with the page.
	for _, unwanted := range []string{`"mem_used":[`, `"t":[`} {
		if strings.Contains(page, unwanted) {
			t.Errorf("the page carries chart data (%s):\n%s", unwanted, page)
		}
	}
}

// Every span offers both statistics, and the chosen one is carried in
// the address so that a change of span keeps it.
func TestHostDetailStatisticFollowsTheSpan(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	host := "/hosts/" + strconv.FormatInt(id(t, e, b), 10)

	for _, span := range []string{"1h", "24h", "7d", "30d", "90d"} {
		_, page := b.get(host + "?range=" + span + "&stat=max")
		for _, want := range []string{`data-max="cpu_max"`, `data-max="mem_used_max"`,
			`data-max="rx_rate_max"`, `"stat":"max"`,
			// The other spans' links carry the statistic along.
			"stat=max"} {
			if !strings.Contains(page, want) {
				t.Errorf("the %s span does not contain %q:\n%s", span, want, page)
			}
		}
	}

	// Without a script, each entry of the statistic menu is a link to the
	// same span with that statistic.
	_, page := b.get(host + "?range=7d&stat=max")
	for _, want := range []string{
		`href="` + host + `?range=7d&amp;stat=mean" data-chart-stat="mean" aria-current="false"`,
		`href="` + host + `?range=7d&amp;stat=max" data-chart-stat="max" aria-current="true"`,
		`<span data-stat-label>Max</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the statistic menu lacks %q:\n%s", want, page)
		}
	}

	// An unknown statistic is the average, like no statistic at all.
	_, page = b.get(host + "?range=24h&stat=p99")
	if !strings.Contains(page, `"stat":"mean"`) {
		t.Errorf("an unknown statistic was accepted:\n%s", page)
	}
}

// id adds a host and returns its id.
func id(t *testing.T, e *env, b *browser) int64 {
	t.Helper()
	hostID, _ := addHost(t, e, b, "web-1")
	return hostID
}

// An unknown span in the address bar shows the default window, and an
// unknown host is a 404.
func TestHostDetailRangeFallback(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")

	_, page := b.get("/hosts/" + strconv.FormatInt(id, 10) + "?range=2h")
	if !strings.Contains(page, `"range":"24h"`) {
		t.Errorf("the default range is not selected:\n%s", page)
	}
	if resp, _ := b.get("/hosts/9999"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown host: %d, want 404", resp.StatusCode)
	}
}

// chartData is what the page's script reads.
type chartData struct {
	Range      string     `json:"range"`
	Resolution string     `json:"resolution"`
	Note       string     `json:"note"`
	From       int64      `json:"from"`
	To         int64      `json:"to"`
	T          []int64    `json:"t"`
	CPU        []*float64 `json:"cpu"`
	CPUMax     []*float64 `json:"cpu_max"`
	MemUsed    []*float64 `json:"mem_used"`
	MemUsedMax []*float64 `json:"mem_used_max"`
	MemTotal   []*float64 `json:"mem_total"`
	Load1      []*float64 `json:"load1"`
	RxRate     []*float64 `json:"rx_rate"`
	Raw        map[string]json.RawMessage
}

func getChartData(t *testing.T, b *browser, path string) (*http.Response, chartData) {
	t.Helper()
	resp, body := b.get(path)
	var data chartData
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, body)
		}
		if err := json.Unmarshal([]byte(body), &data.Raw); err != nil {
			t.Fatal(err)
		}
	}
	return resp, data
}

// The chart endpoint answers with the window the page asked for.
func TestHostMetricsEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	now := time.Now()
	addBucket(t, e, id, now.Add(-2*time.Minute), 10.5, 60.5, 0.5)
	addBucket(t, e, id, now.Add(-time.Minute), 20.5, 70.5, 0.75)

	path := "/api/hosts/" + strconv.FormatInt(id, 10) + "/metrics?range=1h"
	resp, data := getChartData(t, b, path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}
	if data.Range != "1h" || len(data.T) != 2 {
		t.Fatalf("answer = %+v, want two points of the 1h window", data)
	}
	if data.CPU[0] == nil || *data.CPU[0] != 10.5 || data.CPU[1] == nil || *data.CPU[1] != 20.5 {
		t.Errorf("cpu = %v, want 10.5 then 20.5", data.CPU)
	}
	if data.Load1 == nil || data.Load1[0] == nil || *data.Load1[0] != 0.5 {
		t.Errorf("load1 = %v, want the reported averages", data.Load1)
	}
	// Every span is summarized, so both statistics are there to draw.
	if len(data.CPUMax) != 2 || data.CPUMax[0] == nil || *data.CPUMax[0] != 60.5 {
		t.Errorf("cpu_max = %v, want the maxima of the two buckets", data.CPUMax)
	}
	// Memory's chart is drawn against what the host has, so the window
	// carries the capacity as well.
	if len(data.MemTotal) != 2 || data.MemTotal[0] == nil || *data.MemTotal[0] != 1024 {
		t.Errorf("mem_total = %v, want the host's memory", data.MemTotal)
	}
	// Swap and the other load averages have no chart, so they are not in
	// the answer at all.
	for _, key := range []string{"swap_used", "swap_total", "load5", "load15"} {
		if _, ok := data.Raw[key]; ok {
			t.Errorf("%s is in the answer, want it left out", key)
		}
	}
	// The script is told how wide a point is, and the note that says the
	// same thing is translated by the panel.
	if data.Resolution != "1m" {
		t.Errorf("resolution = %q, want 1m", data.Resolution)
	}
	if !strings.Contains(data.Note, "one minute") {
		t.Errorf("note = %q, want the translated note for one-minute points", data.Note)
	}
}

// A span that reads a summarized table carries the peaks next to the
// averages.
func TestHostMetricsCarriesPeaks(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	err := e.db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO metrics_1m (host_id, ts, cpu, cpu_max,
				mem_used, mem_used_max, mem_total, swap_used, swap_total,
				load1, load1_max, load5, load15,
				disk_used, disk_used_max, disk_total, disk_free,
				rx_rate, rx_rate_max, tx_rate, tx_rate_max, uptime)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, time.Now().Add(-time.Hour).Unix(), 5.0, 90.0,
			512, 640, 1024, 0, 0,
			0.1, 0.4, 0.1, 0.1,
			100, 120, 200, 100,
			10.0, 99.0, 5.0, 50.0, 3600)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, data := getChartData(t, b, "/api/hosts/"+strconv.FormatInt(id, 10)+"/metrics?range=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	if len(data.CPUMax) != 1 || data.CPUMax[0] == nil || *data.CPUMax[0] != 90 {
		t.Errorf("cpu_max = %v, want the peak of the bucket", data.CPUMax)
	}
	// Every charted metric offers both statistics, memory included.
	if len(data.MemUsedMax) != 1 || data.MemUsedMax[0] == nil || *data.MemUsedMax[0] != 640 {
		t.Errorf("mem_used_max = %v, want the peak of the bucket", data.MemUsedMax)
	}
	if data.Resolution != "1m" || !strings.Contains(data.Note, "one minute") {
		t.Errorf("resolution = %q, note = %q, want the one-minute table", data.Resolution, data.Note)
	}
}

// The endpoint refuses a span it does not offer, an unknown host and a
// request without a session.
func TestHostMetricsRejects(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	base := "/api/hosts/" + strconv.FormatInt(id, 10) + "/metrics"

	for _, tc := range []struct {
		path string
		want int
	}{
		{base + "?range=2h", http.StatusBadRequest},
		{base, http.StatusBadRequest},
		{"/api/hosts/9999/metrics?range=1h", http.StatusNotFound},
		{"/api/hosts/abc/metrics?range=1h", http.StatusNotFound},
	} {
		resp, body := b.get(tc.path)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d\n%s", tc.path, resp.StatusCode, tc.want, body)
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s: answered with %q", tc.path, resp.Header.Get("Content-Type"))
		}
	}

	anon := e.browser()
	if resp, _ := anon.get(base + "?range=1h"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %d, want 401", resp.StatusCode)
	}
	if resp, _ := anon.get("/hosts/" + strconv.FormatInt(id, 10)); resp.StatusCode != http.StatusFound {
		t.Errorf("detail page without a session: %d, want a redirect", resp.StatusCode)
	}
}

// The overview lists what the panel knows of the host; memory and disk
// come from the stored history when no live sample is at hand, and the
// traffic limit only appears once set.
func TestHostDetailOverview(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, page := addHost(t, e, b, "web-1")
	auth, err := e.hosts.Authenticate(t.Context(), protocol.EnrollAuthorization(tokenFrom(t, page)))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.hosts.SetHostInfo(t.Context(), auth.HostID, &protocol.HostInfo{
		Hostname: "web-1", OS: "linux", Platform: "debian", PlatformVersion: "12",
		Arch: "amd64", CPUCores: 2, AgentVersion: "0.1.0", IntervalMS: 5000,
	}); err != nil {
		t.Fatal(err)
	}
	path := "/hosts/" + strconv.FormatInt(id, 10)
	value := overviewValue

	_, page = b.get(path)
	for label, want := range map[string]string{
		"System": "debian 12 (amd64)",
		"CPU":    "2 cores",
		"Memory": `<span class="text-gray-400">None</span>`,
		"Disk":   `<span class="text-gray-400">None</span>`,
		"Note":   `<span class="text-gray-400">None</span>`,
	} {
		if got := value(page, label); got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
	if !strings.Contains(value(page, "Last seen"), "20") || !strings.Contains(value(page, "Created"), "20") {
		t.Errorf("created %q, last seen %q", value(page, "Created"), value(page, "Last seen"))
	}
	if strings.Contains(page, "Monthly traffic limit") {
		t.Error("a traffic limit is shown although none is set")
	}
	// The sort value orders the dashboard; it is not shown.
	if strings.Contains(page, ">Sort</dt>") {
		t.Error("the overview shows the sort value")
	}
	for _, want := range []string{
		`<details data-menu`, `href="` + path + `/edit"`, `href="` + path + `/reset"`, `href="` + path + `/delete"`,
		">Edit</a>", ">Reset access</a>", ">Remove</a>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Actions menu lacks %q", want)
		}
	}

	// A host that has reported shows what it has, from history alone.
	addBucket(t, e, id, time.Now().Add(-10*time.Minute).Truncate(time.Minute), 10, 20, 0.5)
	if err := e.hosts.SetTraffic(t.Context(), id, traffic.Settings{Mode: traffic.Both, ResetDay: 1, LimitBytes: 1 << 40}); err != nil {
		t.Fatal(err)
	}
	// The limit comes with what the cycle has used of it, both ways.
	e.addDay(id, e.srv.today().String(), 200<<30, 56<<30)
	_, page = b.get(path)
	for label, want := range map[string]string{
		"Memory":                "1.0 KiB",
		"Disk":                  "200 B",
		"Monthly traffic limit": "1.0 TiB (256.0 GiB used, 25.0%)",
	} {
		if got := value(page, label); got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
}

// The site-wide switch does not reach the enrollment token, which keeps
// its own button.
func TestRevealSwitchLeavesTheTokenMasked(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	if err := e.settings.SetRevealIPs(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	b := signedIn(t, e)
	_, page := addHost(t, e, b, "web-1")
	token := tokenFrom(t, page)
	if strings.Contains(page, "<code data-command-text>"+html.EscapeString(token)) || strings.Contains(page, ">"+token+"<") {
		t.Fatalf("the token is shown:\n%s", page)
	}
	if !strings.Contains(page, "--token &#39;***&#39;") && !strings.Contains(page, "--token ***") {
		t.Errorf("the command is not masked:\n%s", page)
	}
	if !strings.Contains(page, "data-command-toggle") || !strings.Contains(page, `href="/"`) {
		t.Errorf("the token's own button or the way back is missing:\n%s", page)
	}
}

// The overview says how long the host has been running, in full, from
// the live sample; an offline host has no such figure.
func TestDetailUptime(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id := reporting(t, e, "web-1")
	path := "/hosts/" + strconv.FormatInt(id, 10)
	_, page := b.get(path)
	if overviewValue(page, "Uptime") != "1 day 1 hour" {
		t.Errorf("online: Uptime = %q", overviewValue(page, "Uptime"))
	}
	// Only an offline host's charts say since when it has been gone.
	if strings.Contains(page, "offline_since") {
		t.Error("an online host's charts carry an offline time")
	}
	e.status.Disconnected(id)
	_, page = b.get(path)
	if overviewValue(page, "Uptime") != `<span class="text-gray-400">None</span>` {
		t.Errorf("offline: Uptime = %q", overviewValue(page, "Uptime"))
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	since := rec.LastSeenAt.UTC().Format("2006-01-02 15:04")
	if !strings.Contains(page, `"offline_since":"`+since+`"`) {
		t.Errorf("the offline host's charts lack %q:\n%s", since, page)
	}
}

// overviewValue is what the detail page's overview shows under label.
func overviewValue(page, label string) string {
	i := strings.Index(page, ">"+label+"</dt>")
	if i < 0 {
		return "(no " + label + ")"
	}
	rest := page[i:]
	rest = rest[strings.Index(rest, "<dd"):]
	rest = rest[strings.Index(rest, ">")+1:]
	return rest[:strings.Index(rest, "</dd>")]
}
