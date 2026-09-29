package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/i18n"
	"github.com/kergeio/kerge-panel/internal/metrics"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/traffic"
)

// The host detail page and the history behind its charts. The page
// itself carries no readings: it renders the chart frames and the script
// then asks for the window it needs, so switching the range or pressing
// refresh never reloads a day of samples that is already on screen.

// detailPage is the data of host_detail.html.
type detailPage struct {
	ID   int64
	Name string
	Note string
	// System describes the host as the agent reported it.
	System string
	// IP is shown or masked as the site-wide switch says.
	IP         masked
	StateText  string
	StateClass string
	// The overview under the heading. A value the panel does not know is
	// empty; TrafficLimit, the limit with what the current cycle has used
	// of it, is empty when no limit is set, and then left out.
	CPU          string
	Memory       string
	Disk         string
	Uptime       string
	TrafficLimit string
	Created      string
	LastSeen     string
	// Renewal is the reminder as the dashboard shows it.
	Renewal renewal
	// Notice reports what the previous request did, such as saving the
	// host.
	Notice msgKey
	Ranges []rangeOption
	Stats  []statOption
	// StatLabel names the chosen statistic on the menu's button.
	StatLabel msgKey
	// Charts are the frames the script fills, in the order they appear.
	Charts []chartFrame
	// Config is the JSON block the chart script reads.
	Config chartConfig
}

// rangeOption is one entry of the range selector.
type rangeOption struct {
	Key   string
	Label msgKey
	// Short is the label on narrow screens, where the five spans share
	// one row; Label stays the accessible name.
	Short    msgKey
	URL      string
	Selected bool
}

// chartFrame is one chart on the page. The script reads its columns and
// its axis from these attributes, so what a chart draws is decided here
// rather than in the script.
type chartFrame struct {
	Name  string
	Title msgKey
	// Unit picks the script's formatter: percent, bytes, rate or number.
	Unit string
	// Mean and Max name the columns of the two statistics.
	Mean, Max string
	// Capacity names the column whose value is the axis ceiling, and
	// AxisMax a fixed ceiling. A chart with neither scales to its data.
	Capacity string
	AxisMax  string
}

// statOption is one entry of the statistic menu. URL shows the same span
// with this statistic, which is what the entry does without a script.
type statOption struct {
	Key      string
	Label    msgKey
	URL      string
	Selected bool
}

// chartConfig tells the script which host and window to ask for, which
// statistic to draw and in which zone to label the time.
type chartConfig struct {
	Host     int64  `json:"host"`
	Range    string `json:"range"`
	Stat     string `json:"stat"`
	TimeZone string `json:"time_zone"`
	// DateFormat and ShortDateFormat are the Go layouts of dates with and
	// without the year.
	DateFormat      string `json:"date_format"`
	ShortDateFormat string `json:"short_date_format"`
	// OfflineSince is when an offline host was last seen, for the note
	// of a window without data; empty for a host that is not offline.
	OfflineSince string `json:"offline_since,omitempty"`
}

// The statistics a summarized window can be read as.
const (
	statMean = "mean"
	statMax  = "max"
)

// chart describes one of the six trends on the detail page. Memory and
// disk are drawn against what the host has, CPU against 100 percent; the
// rest scale to their data.
type chart struct {
	name              string
	title             msgKey
	unit              string
	mean, max         string
	capacity, axisMax string
}

var charts = []chart{
	{name: "cpu", title: msgKey("chart.cpu"), unit: "percent",
		mean: "cpu", max: "cpu_max", axisMax: "100"},
	{name: "mem", title: msgKey("chart.mem"), unit: "bytes",
		mean: "mem_used", max: "mem_used_max", capacity: "mem_total"},
	{name: "load", title: msgKey("chart.load"), unit: "number",
		mean: "load1", max: "load1_max"},
	{name: "disk", title: msgKey("chart.disk"), unit: "bytes",
		mean: "disk_used", max: "disk_used_max", capacity: "disk_total"},
	{name: "download", title: msgKey("chart.download"), unit: "rate",
		mean: "rx_rate", max: "rx_rate_max"},
	{name: "upload", title: msgKey("chart.upload"), unit: "rate",
		mean: "tx_rate", max: "tx_rate_max"},
}

// chartFrames returns the frames of the six charts.
func chartFrames() []chartFrame {
	frames := make([]chartFrame, 0, len(charts))
	for _, c := range charts {
		frames = append(frames, chartFrame{
			Name: c.name, Title: c.title, Unit: c.unit,
			Mean: c.mean, Max: c.max,
			Capacity: c.capacity, AxisMax: c.axisMax,
		})
	}
	return frames
}

// rangeLabel is the translated name of a span. The keys are written out
// so that the check for missing translations sees them.
func rangeLabel(key string) msgKey {
	switch key {
	case "1h":
		return msgKey("chart.range.1h")
	case "24h":
		return msgKey("chart.range.24h")
	case "7d":
		return msgKey("chart.range.7d")
	case "30d":
		return msgKey("chart.range.30d")
	case "90d":
		return msgKey("chart.range.90d")
	}
	return ""
}

// rangeShortLabel is the narrow-screen name of a span.
func rangeShortLabel(key string) msgKey {
	switch key {
	case "1h":
		return msgKey("chart.range.short.1h")
	case "24h":
		return msgKey("chart.range.short.24h")
	case "7d":
		return msgKey("chart.range.short.7d")
	case "30d":
		return msgKey("chart.range.short.30d")
	case "90d":
		return msgKey("chart.range.short.90d")
	}
	return ""
}

// resolutionLabel says how wide one point of a table is.
func resolutionLabel(resolution string) msgKey {
	if resolution == "1h" {
		return msgKey("chart.resolution.1h")
	}
	return msgKey("chart.resolution.1m")
}

// statFor reads the statistic from the URL, defaulting to the average.
func statFor(query string) string {
	if query == statMax {
		return statMax
	}
	return statMean
}

func (s *Server) hostDetailPage(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.hostByPath(w, r)
	if !ok {
		return
	}
	// An unknown range in the URL shows the default window rather than
	// an error: the address bar is not an API.
	rng, ok := metrics.Lookup(r.URL.Query().Get("range"))
	if !ok {
		rng, _ = metrics.Lookup(metrics.DefaultRange)
	}
	stat := statFor(r.URL.Query().Get("stat"))
	live := s.opts.Status.All([]int64{rec.ID})[0]
	c := s.buildCard(rec, live)
	page := detailPage{
		ID:         rec.ID,
		Name:       rec.Name,
		Note:       rec.Note,
		System:     systemLabel(rec),
		IP:         masked{Value: rec.RemoteIP, Shown: s.revealIPs(r.Context())},
		StateText:  c.StateText,
		StateClass: c.StateClass,
		Notice:     noticeFor(r.URL.Query().Get("done")),
		Charts:     chartFrames(),
		Config: chartConfig{
			Host:            rec.ID,
			Range:           rng.Key,
			Stat:            stat,
			TimeZone:        s.location().String(),
			DateFormat:      s.dateFormat(),
			ShortDateFormat: i18n.ShortDateFormat(s.dateFormat()),
		},
	}
	if c.State == string(status.Offline) && !rec.LastSeenAt.IsZero() {
		page.Config.OfflineSince = s.translator().DateTime(rec.LastSeenAt, s.location())
	}
	if err := s.fillOverview(r.Context(), &page, rec, live); err != nil {
		s.internalError(w, r, err)
		return
	}
	base := "/hosts/" + strconv.FormatInt(rec.ID, 10) + "?range="
	for _, option := range metrics.Ranges {
		page.Ranges = append(page.Ranges, rangeOption{
			Key:   option.Key,
			Label: rangeLabel(option.Key),
			Short: rangeShortLabel(option.Key),
			// The chosen statistic survives a change of span.
			URL:      base + option.Key + "&stat=" + stat,
			Selected: option.Key == rng.Key,
		})
	}
	for _, option := range []struct {
		key   string
		label msgKey
	}{{statMean, msgKey("chart.stat.mean")}, {statMax, msgKey("chart.stat.max")}} {
		page.Stats = append(page.Stats, statOption{
			Key: option.key, Label: option.label, Selected: option.key == stat,
			URL: base + rng.Key + "&stat=" + option.key,
		})
		if option.key == stat {
			page.StatLabel = option.label
		}
	}
	s.render(w, r, http.StatusOK, "host_detail.html", page)
}

// fillOverview formats the facts the detail page lists under its
// heading. Memory and disk come from the live sample, or else from the
// latest one stored, so a host that is offline still shows them.
func (s *Server) fillOverview(ctx context.Context, page *detailPage, rec hosts.Record, live status.Live) error {
	tr := s.translator()
	page.CPU = coresLabel(tr, rec.CPUCores)
	var mem, disk *uint64
	if live.HasSample {
		mem, disk = live.Sample.MemTotal, live.Sample.DiskTotal
	}
	if mem == nil || disk == nil {
		storedMem, storedDisk, err := s.opts.Metrics.Totals(ctx, rec.ID)
		if err != nil {
			return err
		}
		if mem == nil {
			mem = storedMem
		}
		if disk == nil {
			disk = storedDisk
		}
	}
	if mem != nil {
		page.Memory = tr.Bytes(*mem)
	}
	if disk != nil {
		page.Disk = tr.Bytes(*disk)
	}
	// How long the host has been running comes from the live sample,
	// and not from one an offline host left behind.
	if live.HasSample && live.State != status.Offline && live.Sample.Uptime != nil {
		page.Uptime = uptimeFull(tr, *live.Sample.Uptime)
	}
	if limit := rec.Traffic.LimitBytes; limit > 0 {
		// The limit comes with what the current cycle has used of it.
		start := traffic.CycleStart(s.today(), rec.Traffic.ResetDay)
		days, err := s.opts.Hosts.DailyTraffic(ctx, start)
		if err != nil {
			return err
		}
		used := rec.Traffic.Used(traffic.Sum(days, rec.ID, start))
		page.TrafficLimit = strings.NewReplacer(
			"{limit}", tr.Bytes(uint64(limit)),
			"{used}", tr.Bytes(uint64(used)),
			"{percent}", tr.Percent(float64(used)/float64(limit)*100),
		).Replace(tr.T(string(msgKey("detail.traffic_limit.value"))))
	}
	if !rec.CreatedAt.IsZero() {
		page.Created = tr.DateTime(rec.CreatedAt, s.location())
	}
	if !rec.LastSeenAt.IsZero() {
		page.LastSeen = tr.DateTime(rec.LastSeenAt, s.location())
	}
	page.Renewal = renewalOf(tr, rec.Reminder, s.today())
	return nil
}

// hostMetrics answers the chart script with one window of history.
func (s *Server) hostMetrics(w http.ResponseWriter, r *http.Request) {
	notFound := func() { s.apiError(w, http.StatusNotFound, msgKey("error.not_found")) }
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound()
		return
	}
	switch _, err := s.opts.Hosts.Get(r.Context(), id); {
	case errors.Is(err, hosts.ErrNotFound):
		notFound()
		return
	case err != nil:
		s.opts.Logger.Error("request failed", "path", r.URL.Path, "err", err)
		s.apiError(w, http.StatusInternalServerError, msgKey("error.internal"))
		return
	}
	rng, ok := metrics.Lookup(r.URL.Query().Get("range"))
	if !ok {
		s.apiError(w, http.StatusBadRequest, msgKey("error.bad_request"))
		return
	}

	series, err := s.opts.Metrics.Query(r.Context(), id, rng, time.Now())
	if err != nil {
		s.opts.Logger.Error("reading the history", "host", id, "range", rng.Key, "err", err)
		s.apiError(w, http.StatusInternalServerError, msgKey("error.internal"))
		return
	}
	body, err := json.Marshal(struct {
		Range string `json:"range"`
		// Resolution is what one point stands for, for the script, and
		// Note is the same thing translated for the reader.
		Resolution string `json:"resolution"`
		Note       string `json:"note"`
		metrics.Series
	}{rng.Key, rng.Resolution, s.translator().T(string(resolutionLabel(rng.Resolution))), series})
	if err != nil {
		s.opts.Logger.Error("encoding the history", "host", id, "err", err)
		s.apiError(w, http.StatusInternalServerError, msgKey("error.internal"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}
