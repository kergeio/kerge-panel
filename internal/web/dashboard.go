package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/traffic"
)

// The overview above the hosts and the switch between cards and list.

// overview is the row of four blocks at the top of the dashboard. The
// clock is not pushed: the page runs it from the time zone it was given.
type overview struct {
	// Online counts the hosts shown as online out of all hosts; the
	// block is marked while any host is in another state.
	Online      string `json:"online"`
	OnlineNote  string `json:"online_note"`
	OnlineAlert bool   `json:"online_alert"`
	// TodayUp and TodayDown are the traffic of all hosts since midnight
	// in the panel's time zone, NowUp and NowDown their current rates.
	TodayUp   string `json:"today_up"`
	TodayDown string `json:"today_down"`
	NowUp     string `json:"now_up"`
	NowDown   string `json:"now_down"`
	// NowShort is the same pair for narrow screens, with the unit said
	// once when both rates share it.
	NowShort string `json:"now_short"`
	// Renewals counts the hosts whose reminder is due or at most seven
	// days away; the block is marked while any reminder is due.
	Renewals      string `json:"renewals"`
	RenewalsNote  string `json:"renewals_note"`
	RenewalsAlert bool   `json:"renewals_alert"`

	// Time, Zone and Date start the clock: the time to the second, the
	// panel's time zone and today's weekday and date there.
	Time string `json:"-"`
	Zone string `json:"-"`
	// DateFormat is the Go layout the clock writes its date in.
	DateFormat string `json:"-"`
	Date       string `json:"-"`
}

// overviewOf sums up the cards and today's traffic.
func (s *Server) overviewOf(cards []card, today []traffic.Day) overview {
	tr := s.translator()
	var ov overview

	online := 0
	for _, c := range cards {
		if c.State == string(status.Online) {
			online++
		}
	}
	ov.Online = strconv.Itoa(online) + " / " + strconv.Itoa(len(cards))
	switch missing := len(cards) - online; {
	case len(cards) == 0:
		ov.OnlineNote = tr.T(string(msgKey("home.online.no_hosts")))
	case missing == 0:
		ov.OnlineNote = tr.T(string(msgKey("home.online.all")))
	default:
		ov.OnlineNote = strings.ReplaceAll(tr.T(string(msgKey("home.online.missing"))), "{n}", strconv.Itoa(missing))
		ov.OnlineAlert = true
	}

	var rx, tx int64
	for _, d := range today {
		rx, tx = saturatingSum(rx, d.RX), saturatingSum(tx, d.TX)
	}
	ov.TodayUp, ov.TodayDown = tr.Bytes(uint64(tx)), tr.Bytes(uint64(rx))
	var rxRate, txRate float64
	for _, c := range cards {
		if c.rx != nil {
			rxRate += *c.rx
		}
		if c.tx != nil {
			txRate += *c.tx
		}
	}
	ov.NowUp, ov.NowDown = tr.ByteRate(txRate), tr.ByteRate(rxRate)
	ov.NowShort = shortRates(tr, ov.NowUp, ov.NowDown)

	soon, due := 0, 0
	var next *card
	for i, c := range cards {
		r := c.renewal
		if r.Close {
			soon++
		}
		if r.Due {
			due++
		}
		// Only named while nothing is due, when every date is ahead.
		if r.set && (next == nil || r.at.Before(next.renewal.at)) {
			next = &cards[i]
		}
	}
	ov.Renewals = strconv.Itoa(soon)
	switch {
	case due > 0:
		ov.RenewalsNote = strings.ReplaceAll(tr.T(string(msgKey("home.renewals.due"))), "{n}", strconv.Itoa(due))
		ov.RenewalsAlert = true
	case next != nil:
		ov.RenewalsNote = strings.NewReplacer(
			"{host}", next.Name,
			"{date}", tr.MonthDay(next.renewal.at.Time(), time.UTC),
		).Replace(tr.T(string(msgKey("home.renewals.next"))))
	default:
		ov.RenewalsNote = tr.T(string(msgKey("home.renewals.none")))
	}

	now, loc := s.now(), s.location()
	ov.Time = tr.Clock(now, loc)
	ov.Zone = loc.String()
	ov.DateFormat = s.dateFormat()
	ov.Date = tr.T(weekdayKey(now.In(loc).Weekday())) + " " + tr.Date(now, loc)
	return ov
}

// shortRates writes "↑ 1.2 ↓ 3.4 M/s" when both rates are in the same
// unit, and both in full otherwise.
func shortRates(tr translator, up, down string) string {
	upArrow, downArrow := tr.T(string(msgKey("card.arrow.up"))), tr.T(string(msgKey("card.arrow.down")))
	upValue, upUnit, ok1 := strings.Cut(up, " ")
	downValue, downUnit, ok2 := strings.Cut(down, " ")
	if ok1 && ok2 && upUnit == downUnit {
		return upArrow + " " + upValue + " " + downArrow + " " + downValue + " " + upUnit
	}
	return upArrow + " " + up + " " + downArrow + " " + down
}

// saturatingSum adds two byte counts without wrapping around.
func saturatingSum(a, b int64) int64 {
	if b > 0 && a > (1<<63-1)-b {
		return 1<<63 - 1
	}
	return a + b
}

// weekdayKey names a weekday. The page's clock uses the same keys, so
// they are exposed to scripts.
func weekdayKey(d time.Weekday) string {
	return string([...]msgKey{
		msgKey("js.weekday.0"), msgKey("js.weekday.1"), msgKey("js.weekday.2"),
		msgKey("js.weekday.3"), msgKey("js.weekday.4"), msgKey("js.weekday.5"),
		msgKey("js.weekday.6"),
	}[d])
}

// saveView stores how the dashboard lists the hosts
// (POST /api/settings/view with view=cards or view=list) and answers 204.
func (s *Server) saveView(w http.ResponseWriter, r *http.Request) {
	view := r.PostFormValue("view")
	if view != settings.ViewCards && view != settings.ViewList {
		s.apiError(w, http.StatusBadRequest, msgKey("error.bad_request"))
		return
	}
	if err := s.opts.Settings.SetDashboardView(r.Context(), view); err != nil {
		s.opts.Logger.Error("request failed", "path", r.URL.Path, "err", err)
		s.apiError(w, http.StatusInternalServerError, msgKey("error.internal"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
