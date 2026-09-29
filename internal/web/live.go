package web

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/traffic"
)

// The dashboard. The page is rendered by the server and then kept
// current over a WebSocket: the panel formats every value, so the page
// only writes text into the elements it already has and the translations
// stay in one place.

// liveWriteTimeout bounds one push to one browser.
const liveWriteTimeout = 10 * time.Second

// card is one host on the dashboard, shown as a card or as a row of the
// list. The metric fields are formatted strings, or the placeholder dash
// when the host has not reported that value.
type card struct {
	ID    int64  `json:"id"`
	Name  string `json:"-"`
	IP    masked `json:"-"`
	State string `json:"state"`
	// StateText is the translated state name, StateClass the colours of
	// its badge and DotClass those of the list's state dot, all picked
	// here so that a push carries them.
	StateText  string `json:"state_text"`
	StateClass string `json:"state_class"`
	DotClass   string `json:"dot_class"`
	// Detail says when a host that is delayed or offline last sent data
	// or was last seen, in full; DetailTime is that time alone, shown next
	// to Icon, which names the state's icon. All three are empty for the
	// other states.
	Detail     string `json:"detail"`
	DetailTime string `json:"detail_time"`
	Icon       string `json:"icon"`
	// System describes the host as the agent reported it.
	System string `json:"-"`
	// Tag is the label a card has next to the system while the host is
	// not reporting: the short detail with its icon. TagTitle is its full
	// text, TagIcon its icon and TagClass its colours.
	Tag      string `json:"tag"`
	TagTitle string `json:"tag_title"`
	TagIcon  string `json:"tag_icon"`
	TagClass string `json:"tag_class"`
	// Cores, MemTotal and DiskTotal describe the machine. They are empty
	// in a push that has no sample, and the page then keeps what it has.
	Cores     string `json:"cores"`
	MemTotal  string `json:"mem_total"`
	DiskTotal string `json:"disk_total"`
	CPU       meter  `json:"cpu"`
	Mem       meter  `json:"mem"`
	Disk      meter  `json:"disk"`
	// Up and Down are the rates with their arrows. A host without rates
	// shows a single dash: Up holds it and Down is empty.
	Up     string `json:"up"`
	Down   string `json:"down"`
	Uptime string `json:"uptime"`
	// Renewal is the date of the renewal reminder, RenewalTitle how far
	// away it is and RenewalClass its colour. A push carries them, so the
	// page follows an acknowledgement and the change of day without a
	// reload.
	Renewal      string `json:"renewal"`
	RenewalTitle string `json:"renewal_title"`
	RenewalClass string `json:"renewal_class"`
	// Traffic is the usage of the current cycle, TrafficLabel names the
	// direction that counts with arrows and TrafficTitle in words the
	// direction that counts. The push carries the usage; the label only
	// changes with the settings, on another page.
	Traffic      string `json:"traffic"`
	TrafficLabel string `json:"-"`
	TrafficTitle string `json:"-"`

	// renewal and rates feed the overview; they are not shown per host.
	renewal renewal
	rx, tx  *float64
}

// meter is a share of a resource: its text and the bar drawn under or
// beside it. Level colours the bar and is empty when there is nothing to
// draw.
type meter struct {
	Text  string  `json:"text"`
	Value float64 `json:"value"`
	Level string  `json:"level"`
}

// Bar colours by share used, matched in web/input.css.
const (
	levelLow  = "low"
	levelMid  = "mid"
	levelHigh = "high"
)

// newMeter describes used out of total, or nothing when either is
// unknown or there is no capacity at all.
func newMeter(tr translator, used, total *uint64) meter {
	if used == nil || total == nil || *total == 0 {
		return meter{}
	}
	return percentMeter(tr, float64(*used)/float64(*total)*100)
}

// percentMeter describes a share given in percent.
func percentMeter(tr translator, p float64) meter {
	p = math.Max(0, math.Min(p, 100))
	m := meter{Text: tr.Percent(p), Value: math.Round(p*10) / 10, Level: levelLow}
	switch {
	case p >= 90:
		m.Level = levelHigh
	case p >= 70:
		m.Level = levelMid
	}
	return m
}

// dashboard is the data of home.html.
type dashboard struct {
	Overview overview
	Cards    []card
	// List is set when the hosts are listed rather than shown as cards.
	List bool
	// Notice reports what the previous request did, such as deleting a
	// host.
	Notice msgKey
}

func (s *Server) homePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cards, ov, err := s.dashboardData(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	reveal := s.revealIPs(ctx)
	tr := s.translator()
	for i := range cards {
		cards[i].IP.Shown = reveal
		// Memory and disk capacity stay on the card of a host that is
		// not reporting, as on the detail page. A push leaves them as
		// they are, so only the page needs the stored ones.
		if cards[i].MemTotal != "" && cards[i].DiskTotal != "" {
			continue
		}
		mem, disk, err := s.opts.Metrics.Totals(ctx, cards[i].ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if cards[i].MemTotal == "" && mem != nil {
			cards[i].MemTotal = tr.Bytes(*mem)
		}
		if cards[i].DiskTotal == "" && disk != nil {
			cards[i].DiskTotal = tr.Bytes(*disk)
		}
	}
	blank := tr.T(string(msgKey("card.blank")))
	for i := range cards {
		for _, f := range []*string{&cards[i].Cores, &cards[i].MemTotal, &cards[i].DiskTotal} {
			if *f == "" {
				*f = blank
			}
		}
	}
	view, err := s.opts.Settings.DashboardView(ctx)
	if err != nil {
		s.opts.Logger.Error("reading the dashboard view", "err", err)
	}
	s.render(w, r, http.StatusOK, "home.html", dashboard{
		Overview: ov,
		Cards:    cards,
		List:     view == settings.ViewList,
		Notice:   noticeFor(r.URL.Query().Get("done")),
	})
}

// dashboardData builds the cards and the overview above them.
func (s *Server) dashboardData(ctx context.Context) ([]card, overview, error) {
	cards, today, err := s.cards(ctx)
	if err != nil {
		return nil, overview{}, err
	}
	return cards, s.overviewOf(cards, today), nil
}

// cards builds the dashboard from the host records and the live registry.
// It also returns the traffic of today, which the overview sums up.
func (s *Server) cards(ctx context.Context) ([]card, []traffic.Day, error) {
	records, err := s.opts.Hosts.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, len(records))
	for i, rec := range records {
		ids[i] = rec.ID
	}
	live := s.opts.Status.All(ids)
	tr, today := s.translator(), s.today()
	days, err := s.opts.Hosts.DailyTraffic(ctx, traffic.EarliestCycleStart(today))
	if err != nil {
		return nil, nil, err
	}
	out := make([]card, len(records))
	for i, rec := range records {
		c := s.buildCard(rec, live[i])
		c.renewal = renewalOf(tr, rec.Reminder, today)
		c.Renewal, c.RenewalTitle, c.RenewalClass = c.renewal.Date, c.renewal.Title, c.renewal.Class
		rx, tx := traffic.Sum(days, rec.ID, traffic.CycleStart(today, rec.Traffic.ResetDay))
		// A host that never had an agent shows no figures at all, like
		// its other fields, and one that is offline shows none of the
		// values that change while it runs.
		if c.State != string(status.Offline) && (!rec.Pending() || rx != 0 || tx != 0) {
			c.Traffic = trafficLine(tr, rec.Traffic, rx, tx)
		}
		c.TrafficLabel, c.TrafficTitle = trafficLabel(tr, rec.Traffic.Mode)
		c.fillBlanks(tr.T(string(msgKey("card.blank"))))
		out[i] = c
	}
	var todays []traffic.Day
	for _, d := range days {
		if d.Day == today {
			todays = append(todays, d)
		}
	}
	return out, todays, nil
}

// fillBlanks puts the placeholder into every value the host has not
// reported, so a card or row keeps its shape. The rates come as a pair,
// so a missing pair is one dash in Up.
func (c *card) fillBlanks(blank string) {
	for _, f := range []*string{&c.Up, &c.Uptime, &c.Traffic,
		&c.CPU.Text, &c.Mem.Text, &c.Disk.Text} {
		if *f == "" {
			*f = blank
		}
	}
}

func (s *Server) buildCard(rec hosts.Record, live status.Live) card {
	tr := s.translator()
	c := card{
		ID:     rec.ID,
		Name:   rec.Name,
		IP:     masked{Value: rec.RemoteIP},
		State:  string(live.State),
		System: systemLabel(rec),
		Cores:  coresLabel(tr, rec.CPUCores),
	}
	// A host without a credential is pending whatever the registry says:
	// no agent can have connected yet.
	if rec.Pending() {
		c.State = string(status.Pending)
	}
	c.StateText = tr.T("state." + c.State)
	c.StateClass = stateClass(status.State(c.State))
	c.DotClass = dotClass(status.State(c.State))

	var since time.Time
	switch status.State(c.State) {
	case status.Stale:
		since, c.Icon = live.LastDataAt, iconStale
		c.Detail = tr.T("state.stale.detail")
	case status.Offline:
		since, c.Icon = rec.LastSeenAt, iconOffline
		c.Detail = tr.T("state.offline.detail")
	}
	if since.IsZero() {
		c.Detail, c.Icon = "", ""
	} else {
		c.Detail += " " + tr.DateTime(since, s.location())
		c.DetailTime = tr.DateTime(since, s.location())
		c.Tag, c.TagTitle, c.TagIcon, c.TagClass = c.DetailTime, c.Detail, c.Icon, detailTagClass
	}

	if !live.HasSample {
		return c
	}
	m := live.Sample
	// The size of the machine stays on the page of a host that is
	// offline; what it last measured does not.
	if m.MemTotal != nil {
		c.MemTotal = tr.Bytes(*m.MemTotal)
	}
	if m.DiskTotal != nil {
		c.DiskTotal = tr.Bytes(*m.DiskTotal)
	}
	if live.State == status.Offline {
		return c
	}
	if m.CPU != nil {
		c.CPU = percentMeter(tr, *m.CPU)
	}
	c.Mem = newMeter(tr, m.MemUsed, m.MemTotal)
	c.Disk = newMeter(tr, m.DiskUsed, m.DiskTotal)
	if m.RxRate != nil && m.TxRate != nil {
		c.Up = tr.T(string(msgKey("card.arrow.up"))) + " " + tr.ByteRate(*m.TxRate)
		c.Down = tr.T(string(msgKey("card.arrow.down"))) + " " + tr.ByteRate(*m.RxRate)
	}
	// Only a host that is reporting adds to the overview's current
	// rates; the last sample of a delayed one is old news.
	if live.State == status.Online {
		c.rx, c.tx = m.RxRate, m.TxRate
	}
	if m.Uptime != nil {
		c.Uptime = uptimeShort(tr, *m.Uptime)
	}
	return c
}

// uptimeShort is how long a host has been running as the dashboard says
// it: whole days from a day on, whole hours below that.
func uptimeShort(tr translator, seconds uint64) string {
	switch days, hours := seconds/86400, seconds/3600; {
	case days > 0:
		return count(tr, days, msgKey("uptime.day"), msgKey("uptime.days"))
	case hours > 0:
		return count(tr, hours, msgKey("uptime.hour"), msgKey("uptime.hours"))
	default:
		return tr.T(string(msgKey("uptime.less_than_hour")))
	}
}

// uptimeFull is how long a host has been running in days, hours and
// minutes, leaving out the parts that are zero.
func uptimeFull(tr translator, seconds uint64) string {
	days, hours, minutes := seconds/86400, seconds%86400/3600, seconds%3600/60
	var parts []string
	if days > 0 {
		parts = append(parts, count(tr, days, msgKey("uptime.day"), msgKey("uptime.days")))
	}
	if hours > 0 {
		parts = append(parts, count(tr, hours, msgKey("uptime.hour"), msgKey("uptime.hours")))
	}
	if minutes > 0 || len(parts) == 0 {
		parts = append(parts, count(tr, minutes, msgKey("uptime.minute"), msgKey("uptime.minutes")))
	}
	return strings.Join(parts, " ")
}

// count says "1 day" or "3 days": one for exactly one, many with {n}
// otherwise.
func count(tr translator, n uint64, one, many msgKey) string {
	if n == 1 {
		return tr.T(string(one))
	}
	return strings.ReplaceAll(tr.T(string(many)), "{n}", strconv.FormatUint(n, 10))
}

// coresLabel says how many CPU cores a host has, or nothing before the
// agent has told.
func coresLabel(tr translator, cores int64) string {
	switch {
	case cores == 1:
		return tr.T(string(msgKey("detail.cpu.core")))
	case cores > 1:
		return strings.ReplaceAll(tr.T(string(msgKey("detail.cpu.cores"))), "{n}", strconv.FormatInt(cores, 10))
	}
	return ""
}

// Classes picked here are listed in web/input.css as well, because
// Tailwind does not read Go files.
const (
	// detailTagClass is the grey tag of a host that is not reporting.
	detailTagClass = "bg-gray-100 text-gray-600"
	// The icons next to the time a host that is not reporting was last
	// heard from, as the page names them in data-icon.
	iconOffline = "offline"
	iconStale   = "stale"
	// noneClass greys out a renewal date that is not set, and
	// plainClass shows one that is not close yet.
	noneClass  = "text-gray-400"
	plainClass = "text-gray-900"
)

// stateClass is the badge styling of a state. Green for a host that is
// reporting, amber while something is in progress, red once the panel has
// given up on it.
func stateClass(s status.State) string {
	switch s {
	case status.Online:
		return "bg-green-100 text-green-800"
	case status.Stale, status.Connecting, status.Pending:
		return "bg-amber-100 text-amber-800"
	default:
		return "bg-red-100 text-red-800"
	}
}

// dotClass is the colour of the list's state dot, following the badge.
func dotClass(s status.State) string {
	switch s {
	case status.Online:
		return "bg-green-500"
	case status.Stale, status.Connecting, status.Pending:
		return "bg-amber-500"
	default:
		return "bg-red-500"
	}
}

// translator is what card formatting needs, so the helpers can be tested
// without a catalog.
type translator interface {
	T(key string) string
	Bytes(n uint64) string
	ByteRate(bytesPerSec float64) string
	Percent(p float64) string
	Date(t time.Time, loc *time.Location) string
	MonthDay(t time.Time, loc *time.Location) string
	DateTime(t time.Time, loc *time.Location) string
}

// liveWS pushes the dashboard to a signed-in browser until the page goes
// away.
func (s *Server) liveWS(w http.ResponseWriter, r *http.Request) {
	// The library rejects an Origin other than this host by default,
	// which is what keeps another site from opening this socket.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	// Nothing is expected from the browser, so anything it sends is a
	// reason to stop reading.
	conn.SetReadLimit(1 << 10)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		// Reading is also what lets the library answer the browser's
		// ping frames and notice that the page was closed.
		_, _, _ = conn.Read(ctx)
	}()

	updates, unsubscribe := s.opts.Status.Subscribe()
	defer unsubscribe()

	if err := s.pushCards(ctx, conn); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-updates:
			if err := s.pushCards(ctx, conn); err != nil {
				return
			}
		}
	}
}

// livePush is one message of the live socket.
type livePush struct {
	Hosts    []card   `json:"hosts"`
	Overview overview `json:"overview"`
}

// pushCards sends the current dashboard as one message.
func (s *Server) pushCards(ctx context.Context, conn *websocket.Conn) error {
	cards, ov, err := s.dashboardData(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.opts.Logger.Error("building the dashboard", "err", err)
		}
		return err
	}
	body, err := json.Marshal(livePush{Hosts: cards, Overview: ov})
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, liveWriteTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, body); err != nil {
		return errors.New("web: writing to the browser: " + err.Error())
	}
	return nil
}
