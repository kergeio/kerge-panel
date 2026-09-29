package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-protocol"
)

// reporting adds a host, enrolls it and puts one sample in the registry,
// as an agent connection would.
func reporting(t *testing.T, e *env, name string) int64 {
	t.Helper()
	ctx := t.Context()
	id, err := e.hosts.Create(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	token, err := e.hosts.NewEnrollToken(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.hosts.Authenticate(ctx, protocol.EnrollAuthorization(token)); err != nil {
		t.Fatal(err)
	}
	if err := e.hosts.SetHostInfo(ctx, id, &protocol.HostInfo{
		Hostname: name, OS: "linux", Platform: "ubuntu", PlatformVersion: "22.04",
		Arch: "arm64", CPUCores: 1, AgentVersion: "0.1.0", IntervalMS: 5000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.hosts.Connected(ctx, id, testIP()); err != nil {
		t.Fatal(err)
	}
	e.status.Connected(id, 5*time.Second, nil)
	e.status.Report(id, sample(time.Now()))
	return id
}

func sample(at time.Time) status.Sample {
	cpu := 12.5
	memUsed, memTotal := uint64(512<<20), uint64(1<<30)
	swapUsed, swapTotal := uint64(0), uint64(512<<20)
	diskUsed, diskTotal := uint64(4<<30), uint64(40<<30)
	rx, tx := 1500.0, 2500.0
	uptime := uint64(90000)
	return status.Sample{
		At: at, CPU: &cpu,
		MemUsed: &memUsed, MemTotal: &memTotal,
		SwapUsed: &swapUsed, SwapTotal: &swapTotal,
		DiskUsed: &diskUsed, DiskTotal: &diskTotal,
		RxRate: &rx, TxRate: &tx, Uptime: &uptime,
	}
}

// The dashboard renders every host as a card and as a row of the list,
// with the values formatted by the panel and the address masked.
func TestDashboardCards(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)

	_, page := b.get("/")
	if !strings.Contains(page, "No hosts yet") || !strings.Contains(page, "data-overview") ||
		strings.Contains(page, "data-view-button") {
		t.Errorf("empty dashboard:\n%s", page)
	}

	id := reporting(t, e, "web-1")
	if _, err := e.hosts.Create(t.Context(), "not-installed"); err != nil {
		t.Fatal(err)
	}
	_, page = b.get("/")

	for _, want := range []string{
		`data-host-id="` + itoa(id) + `"`,
		">Online<",               // the live state of the reporting host
		">Pending<",              // the host whose agent is not installed
		">ubuntu 22.04 (arm64)<", // the system tag
		">1 core<",               // the machine
		">1.0 GiB<",
		">40.0 GiB<",
		">12.5%<", // cpu
		">50.0%<", // memory
		">10.0%<", // disk
		`value="50" data-level="low"`,
		">↓ 1.5 K/s<", // rx rate, 1500 bytes a second
		">↑ 2.4 K/s<", // tx rate
		">1 day<",     // uptime, in whole days
		">None<",      // no renewal reminder
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the dashboard is missing %q", want)
		}
	}
	if !strings.Contains(page, "&#42;&#42;&#42;") {
		t.Errorf("the address is not masked:\n%s", page)
	}
	// The card has no swap bar.
	if strings.Contains(page, ">Swap<") || strings.Contains(page, `data-meter="swap"`) {
		t.Errorf("the dashboard shows swap:\n%s", page)
	}
	// The machine's size is on the card and under the bars of the list
	// row.
	for _, size := range []string{">1 core<", ">1.0 GiB<", ">40.0 GiB<"} {
		if n := strings.Count(page, size); n != 2 {
			t.Errorf("%s appears %d times, want 2", size, n)
		}
	}
	// Each host is a card and a row of the list; the cards show until the
	// list is chosen, and on a narrow screen whatever is chosen: the list
	// and its switch show only from 980 pixels.
	if n := strings.Count(page, `data-host-id="`+itoa(id)+`"`); n != 2 {
		t.Errorf("host %d appears %d times", id, n)
	}
	if !strings.Contains(page, `data-views="cards" class="group"`) ||
		!strings.Contains(page, `data-view="cards" class="mt-6 grid items-start gap-4 sm:grid-cols-2 lg:grid-cols-3 min-[980px]:group-data-[views=list]:hidden"`) ||
		!strings.Contains(page, `data-view="list" class="mt-6 hidden flex-col gap-2.5 min-[980px]:group-data-[views=list]:flex"`) ||
		!strings.Contains(page, `class="hidden overflow-hidden rounded-md border border-gray-300 bg-white min-[980px]:flex"`) ||
		!strings.Contains(page, `data-view-button="cards" aria-pressed="true"`) {
		t.Errorf("the cards are not the default view:\n%s", page)
	}
}

// A host that has not reported anything keeps the shape of a card: every
// value is a dash and no bar is drawn.
func TestDashboardBlanks(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	id, err := e.hosts.Create(t.Context(), "not-installed")
	if err != nil {
		t.Fatal(err)
	}
	c := e.cardOf(id)
	for name, v := range map[string]string{"cpu": c.CPU.Text, "mem": c.Mem.Text,
		"disk": c.Disk.Text, "up": c.Up, "uptime": c.Uptime, "traffic": c.Traffic} {
		if v != "—" {
			t.Errorf("%s = %q, want a dash", name, v)
		}
	}
	// Without rates the network is a single dash.
	if c.Down != "" || c.CPU.Level != "" || c.Tag != "" || c.Detail != "" || c.Renewal != "None" || c.RenewalClass != noneClass {
		t.Errorf("pending card = %+v", c)
	}
}

// The dashboard says how long a host has run in whole days, or whole
// hours below a day; the detail page says it in full, leaving out what is
// zero.
func TestUptime(t *testing.T) {
	tr := realCatalog(t).Translator("en")
	for seconds, want := range map[uint64][2]string{
		0:                        {"less than 1 hour", "0 minutes"},
		59:                       {"less than 1 hour", "0 minutes"},
		60:                       {"less than 1 hour", "1 minute"},
		3599:                     {"less than 1 hour", "59 minutes"},
		3600:                     {"1 hour", "1 hour"},
		7*3600 + 5:               {"7 hours", "7 hours"},
		86399:                    {"23 hours", "23 hours 59 minutes"},
		86400:                    {"1 day", "1 day"},
		90000:                    {"1 day", "1 day 1 hour"},
		3*86400 + 21*3600 + 7*60: {"3 days", "3 days 21 hours 7 minutes"},
	} {
		if got := uptimeShort(tr, seconds); got != want[0] {
			t.Errorf("uptimeShort(%d) = %q, want %q", seconds, got, want[0])
		}
		if got := uptimeFull(tr, seconds); got != want[1] {
			t.Errorf("uptimeFull(%d) = %q, want %q", seconds, got, want[1])
		}
	}
}

// Bars are green below 70 percent, amber from 70 and red from 90; a share
// that cannot be worked out shows a dash and no bar.
func TestMeterLevels(t *testing.T) {
	tr := realCatalog(t).Translator("en")
	for _, c := range []struct {
		p     float64
		text  string
		level string
	}{
		{0, "0.0%", levelLow}, {69.9, "69.9%", levelLow}, {70, "70.0%", levelMid},
		{89.9, "89.9%", levelMid}, {90, "90.0%", levelHigh}, {120, "100.0%", levelHigh},
	} {
		if m := percentMeter(tr, c.p); m.Text != c.text || m.Level != c.level {
			t.Errorf("%v%%: %+v", c.p, m)
		}
	}
	zero, used := uint64(0), uint64(5)
	if m := newMeter(tr, &used, &zero); m != (meter{}) {
		t.Errorf("no capacity: %+v", m)
	}
	if m := newMeter(tr, nil, &used); m != (meter{}) {
		t.Errorf("unknown use: %+v", m)
	}
}

// A host that is not reporting says since when in the tag, with an icon
// and the time alone, in place of a renewal tag; the full text is the
// tooltip. An offline host shows none of what it last measured, only the
// size of the machine.
func TestDashboardOfflineHost(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	e.clockAt(time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC))
	id := reporting(t, e, "web-1")
	e.addDay(id, "2026-09-23", 1<<30, 1<<30)
	if err := e.hosts.SetReminder(t.Context(), id, mustDate(t, "2026-09-25"), 1); err != nil {
		t.Fatal(err)
	}
	c := e.cardOf(id)
	// A renewal is never a tag: the tag is for a host that is not reporting.
	if c.Tag != "" || c.TagIcon != "" || c.RenewalTitle != "in 2 days" || c.RenewalClass != reminderUrgentClass ||
		c.Detail != "" || c.Icon != "" || c.Traffic != "2.0 GiB" {
		t.Fatalf("online card = %+v", c)
	}
	e.status.Disconnected(id)
	c = e.cardOf(id)
	// The time is written in full, the year included.
	if c.Detail != "Last seen at "+c.DetailTime || len(c.DetailTime) != len("2026-09-23 09:00") {
		t.Errorf("offline detail %q, time %q", c.Detail, c.DetailTime)
	}
	if !strings.HasPrefix(c.Detail, "Last seen at ") || c.Icon != iconOffline || c.DetailTime == "" || !strings.HasPrefix(c.DetailTime, "20") ||
		!strings.HasSuffix(c.Detail, c.DetailTime) || c.Tag != c.DetailTime || c.TagTitle != c.Detail ||
		c.TagIcon != iconOffline || c.TagClass != detailTagClass || c.DotClass != "bg-red-500" {
		t.Fatalf("offline card = %+v", c)
	}
	for name, v := range map[string]string{"cpu": c.CPU.Text, "mem": c.Mem.Text, "disk": c.Disk.Text,
		"up": c.Up, "uptime": c.Uptime, "traffic": c.Traffic} {
		if v != "—" {
			t.Errorf("offline %s = %q, want a dash", name, v)
		}
	}
	if c.Down != "" || c.CPU.Level != "" {
		t.Errorf("offline rates and bars = %q %+v", c.Down, c.CPU)
	}
	if c.Cores != "1 core" || c.MemTotal != "1.0 GiB" || c.DiskTotal != "40.0 GiB" || c.System != "ubuntu 22.04 (arm64)" {
		t.Errorf("offline machine = %+v", c)
	}
	// The renewal date and its countdown are shown in the card footer and
	// in the list.
	if c.Renewal != "2026-09-25" || c.RenewalTitle != "in 2 days" {
		t.Errorf("offline renewal = %+v", c)
	}

	b := signedIn(t, e)
	_, page := b.get("/")
	for _, want := range []string{
		`data-tag title="` + c.Detail + `"`,
		`data-detail title="` + c.Detail + `"`,
		`data-icon="offline" data-icon-field="tag_icon"`,
		`>` + c.DetailTime + `</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, `data-icon="offline" data-icon-field="icon" class="size-3 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" hidden`) {
		t.Error("the list hides the offline icon of an offline host")
	}
}

// A host whose data is late keeps its values and says since when with
// the clock icon.
func TestDashboardStaleHost(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	id := reporting(t, e, "web-1")
	e.status.Report(id, sample(time.Now().Add(-2*time.Minute)))
	c := e.cardOf(id)
	if c.State != "stale" || !strings.HasPrefix(c.Detail, "Last data at ") || c.Icon != iconStale ||
		c.TagIcon != iconStale || c.CPU.Text != "12.5%" || c.Uptime != "1 day" {
		t.Fatalf("stale card = %+v", c)
	}
}

// The browser gets the dashboard over the WebSocket and again whenever the
// evaluator publishes.
func TestLivePushesTheDashboard(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id := reporting(t, e, "web-1")

	conn := b.dialLive(t)
	defer conn.CloseNow()

	first := readPush(t, conn)
	if len(first.Hosts) != 1 || first.Hosts[0].ID != id || first.Hosts[0].State != string(status.Online) {
		t.Fatalf("first push = %+v", first)
	}
	if h := first.Hosts[0]; h.CPU.Text != "12.5%" || h.Uptime != "1 day" || h.MemTotal != "1.0 GiB" {
		t.Errorf("formatted values = %+v", h)
	}
	if first.Overview.Online != "1 / 1" || first.Overview.NowDown != "1.5 K/s" {
		t.Errorf("overview = %+v", first.Overview)
	}

	// A new sample reaches the browser on the next publish.
	cpu := 99.0
	s := sample(time.Now())
	s.CPU = &cpu
	e.status.Report(id, s)
	e.status.Publish(e.status.All([]int64{id}))

	second := readPush(t, conn)
	if len(second.Hosts) != 1 || second.Hosts[0].CPU.Text != "99.0%" || second.Hosts[0].CPU.Level != levelHigh {
		t.Errorf("second push = %+v", second)
	}

	// A host that stopped reporting shows up as offline with the styling
	// that goes with it, and the overview marks it.
	e.status.Disconnected(id)
	e.status.Publish(e.status.All([]int64{id}))
	third := readPush(t, conn)
	if len(third.Hosts) != 1 || third.Hosts[0].State != string(status.Offline) {
		t.Fatalf("third push = %+v", third)
	}
	if h := third.Hosts[0]; !strings.Contains(h.StateClass, "red") || h.StateText != "Offline" {
		t.Errorf("offline card = %+v", h)
	}
	if ov := third.Overview; ov.Online != "0 / 1" || !ov.OnlineAlert || ov.NowDown != "0 B/s" {
		t.Errorf("offline overview = %+v", ov)
	}
}

// The live socket is part of the panel, not something another page may
// open: without a session there is no socket at all.
func TestLiveNeedsASession(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := e.browser()

	ctx, cancel := contextWithTimeout(t)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL(b.server.URL), &websocket.DialOptions{HTTPClient: b.client})
	if err == nil {
		conn.CloseNow()
		t.Fatal("the socket opened without a session")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

func (b *browser) dialLive(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := contextWithTimeout(t)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(b.server.URL), &websocket.DialOptions{HTTPClient: b.client})
	if err != nil {
		t.Fatalf("dialing the live socket: %v", err)
	}
	return conn
}

func wsURL(serverURL string) string {
	return "wss" + strings.TrimPrefix(serverURL, "https") + "/api/live"
}

// readPush reads one push.
func readPush(t *testing.T, conn *websocket.Conn) livePush {
	t.Helper()
	ctx, cancel := contextWithTimeout(t)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("reading a push: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("the panel sent a %v message", typ)
	}
	var payload livePush
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decoding a push: %v\n%s", err, data)
	}
	return payload
}

func contextWithTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), 10*time.Second)
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
