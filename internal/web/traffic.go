package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/kergeio/kerge-panel/internal/traffic"
)

// Traffic accounting on the pages: the settings on the edit page and the
// usage of the current cycle on the dashboard card.

// trafficModeLabels names the modes on the edit page.
var trafficModeLabels = map[traffic.Mode]msgKey{
	traffic.Both: msgKey("traffic.mode.both"),
	traffic.Out:  msgKey("traffic.mode.out"),
	traffic.In:   msgKey("traffic.mode.in"),
}

// trafficForm is the traffic part of the edit page, as shown or as posted.
type trafficForm struct {
	ResetDays []option
	Modes     []option
	// Limit is the limit in GiB as typed, empty for none.
	Limit string
}

// newTrafficForm builds the fields with the given values selected.
func (s *Server) newTrafficForm(resetDay, mode, limit string) trafficForm {
	tr := s.translator()
	f := trafficForm{Limit: limit}
	for d := 1; d <= traffic.MaxResetDay; d++ {
		v := strconv.Itoa(d)
		f.ResetDays = append(f.ResetDays, option{Value: v, Label: v, Selected: v == resetDay})
	}
	for _, m := range traffic.Modes {
		f.Modes = append(f.Modes, option{
			Value: string(m), Label: tr.T(string(trafficModeLabels[m])), Selected: string(m) == mode,
		})
	}
	return f
}

// trafficFormOf shows the saved settings of a host.
func (s *Server) trafficFormOf(set traffic.Settings) trafficForm {
	return s.newTrafficForm(strconv.Itoa(set.ResetDay), string(set.Mode), traffic.FormatLimitGiB(set.LimitBytes))
}

// parseTrafficForm reads the posted traffic settings. On failure it returns
// the message for the field at fault.
func parseTrafficForm(r *http.Request) (traffic.Settings, msgKey) {
	var set traffic.Settings
	day, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("traffic_reset_day")))
	if err != nil || !traffic.ValidResetDay(day) {
		return set, msgKey("hosts.error.reset_day")
	}
	set.ResetDay = day
	set.Mode = traffic.Mode(r.PostFormValue("traffic_mode"))
	if !traffic.ValidMode(set.Mode) {
		return set, msgKey("hosts.error.traffic_mode")
	}
	if set.LimitBytes, err = traffic.ParseLimitGiB(r.PostFormValue("traffic_limit")); err != nil {
		return set, msgKey("hosts.error.traffic_limit")
	}
	return set, ""
}

// trafficLine is what the dashboard says about the current cycle: the
// traffic that counts under the host's mode, without the limit, which
// the detail page shows.
func trafficLine(tr translator, set traffic.Settings, rx, tx int64) string {
	return tr.Bytes(uint64(set.Used(rx, tx)))
}

// trafficLabel names the traffic after the direction that counts, with
// the arrows the network rates use, and trafficTitle says it in words
// for the tooltip.
func trafficLabel(tr translator, mode traffic.Mode) (label, title string) {
	switch mode {
	case traffic.Out:
		return tr.T(string(msgKey("card.traffic.out"))), tr.T(string(msgKey("card.traffic.out.title")))
	case traffic.In:
		return tr.T(string(msgKey("card.traffic.in"))), tr.T(string(msgKey("card.traffic.in.title")))
	default:
		return tr.T(string(msgKey("card.traffic"))), tr.T(string(msgKey("card.traffic.title")))
	}
}
