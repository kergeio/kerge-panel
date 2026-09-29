package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/auth"
	"github.com/kergeio/kerge-panel/internal/i18n"
	"github.com/kergeio/kerge-panel/internal/rollup"
	"github.com/kergeio/kerge-protocol/ifacefilter"
)

// The settings page. Each section is its own form and posts to its own
// route, so saving one never rewrites the others.

// settingsData is the data of settings.html.
type settingsData struct {
	PasswordError   msgKey
	PasswordChanged bool

	DisplayError msgKey
	DisplaySaved bool
	Languages    []option
	TimeZones    []option
	DateFormats  []option

	DataError       msgKey
	DataSaved       bool
	RetentionRaw    []option
	RetentionMinute []option
	RetentionHour   []option
	// IfaceExclude is the default exclusion list as text, empty when the
	// operator has cleared it.
	IfaceExclude string
	// IfaceExcludeDefault reports whether the field shows the built-in
	// list because nothing has been stored yet.
	IfaceExcludeDefault bool
}

// Retention choices are shown with a label of their own, one key per
// value, because the translations differ in more than the number.
var (
	retentionHourLabels = map[int]msgKey{
		6:  msgKey("settings.retention.hours.6"),
		12: msgKey("settings.retention.hours.12"),
		24: msgKey("settings.retention.hours.24"),
		48: msgKey("settings.retention.hours.48"),
		72: msgKey("settings.retention.hours.72"),
	}
	retentionDayLabels = map[int]msgKey{
		3:   msgKey("settings.retention.days.3"),
		7:   msgKey("settings.retention.days.7"),
		14:  msgKey("settings.retention.days.14"),
		30:  msgKey("settings.retention.days.30"),
		90:  msgKey("settings.retention.days.90"),
		180: msgKey("settings.retention.days.180"),
		365: msgKey("settings.retention.days.365"),
	}
)

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.newSettingsData(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "settings.html", data)
}

// newSettingsData fills the page from the stored preferences.
func (s *Server) newSettingsData(ctx context.Context) (settingsData, error) {
	p, err := s.opts.Settings.Load(ctx)
	if err != nil {
		return settingsData{}, err
	}
	data := settingsData{
		IfaceExclude:        p.IfaceExclude,
		IfaceExcludeDefault: !p.IfaceExcludeChosen,
	}
	if !p.IfaceExcludeChosen {
		// The same list panelIfaceExclude reports.
		data.IfaceExclude = strings.Join(ifacefilter.DefaultExclude, ",")
	}
	s.fillDisplay(&data, p.Language, p.TimeZone, p.DateFormat)
	s.fillRetention(&data, p.Retention)
	return data, nil
}

func (s *Server) fillDisplay(data *settingsData, language, timeZone, dateFormat string) {
	if language == "" {
		language = i18n.Default
	}
	if timeZone == "" {
		timeZone = "UTC"
	}
	if dateFormat == "" {
		dateFormat = i18n.DefaultDateFormat
	}
	// Each format is shown as the date it makes of one example day, which
	// needs no translation.
	example := time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC)
	for _, f := range i18n.DateFormats {
		data.DateFormats = append(data.DateFormats, option{Value: f, Label: example.Format(f), Selected: f == dateFormat})
	}
	for _, l := range s.opts.Catalog.Languages() {
		data.Languages = append(data.Languages, option{
			Value: l, Label: s.opts.Catalog.Translator(l).T(string(msgKey("language.name"))),
			Selected: l == language,
		})
	}
	for _, z := range i18n.TimeZones {
		data.TimeZones = append(data.TimeZones, option{Value: z, Label: z, Selected: z == timeZone})
	}
}

func (s *Server) fillRetention(data *settingsData, r rollup.Retention) {
	tr := s.translator()
	build := func(choices []int, labels map[int]msgKey, selected int) []option {
		out := make([]option, 0, len(choices))
		for _, c := range choices {
			out = append(out, option{
				Value:    strconv.Itoa(c),
				Label:    tr.T(string(labels[c])),
				Selected: c == selected,
			})
		}
		return out
	}
	data.RetentionRaw = build(rollup.RawHoursChoices, retentionHourLabels, r.RawHours)
	data.RetentionMinute = build(rollup.MinuteDaysChoices, retentionDayLabels, r.MinuteDays)
	data.RetentionHour = build(rollup.HourDaysChoices, retentionDayLabels, r.HourDays)
}

// saveDisplay stores the interface language, the panel time zone and the
// date format.
func (s *Server) saveDisplay(w http.ResponseWriter, r *http.Request) {
	language := r.PostFormValue("language")
	timeZone := r.PostFormValue("timezone")
	dateFormat := r.PostFormValue("date_format")
	fail := func(key msgKey) {
		data, err := s.newSettingsData(r.Context())
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		data.DisplayError = key
		s.fillDisplay(&data, language, timeZone, dateFormat)
		s.render(w, r, http.StatusUnprocessableEntity, "settings.html", data)
	}
	switch {
	case !s.opts.Catalog.Has(language):
		fail(msgKey("setup.error.language"))
		return
	case !validTimeZone(timeZone):
		fail(msgKey("setup.error.timezone"))
		return
	case !i18n.ValidDateFormat(dateFormat):
		fail(msgKey("settings.error.date_format"))
		return
	}
	if err := s.opts.Settings.SaveDisplay(r.Context(), language, timeZone, dateFormat); err != nil {
		s.internalError(w, r, err)
		return
	}
	// The page is rendered again in whatever language was just chosen.
	data, err := s.newSettingsData(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	data.DisplaySaved = true
	s.render(w, r, http.StatusOK, "settings.html", data)
}

// saveData stores the retention of the three metrics tables and the
// default interface exclusion list.
func (s *Server) saveData(w http.ResponseWriter, r *http.Request) {
	retention := rollup.Retention{
		RawHours:   formInt(r, "retention_raw_hours"),
		MinuteDays: formInt(r, "retention_1m_days"),
		HourDays:   formInt(r, "retention_1h_days"),
	}
	exclude := strings.TrimSpace(r.PostFormValue("net_iface_exclude"))
	fail := func(key msgKey) {
		data, err := s.newSettingsData(r.Context())
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		data.DataError = key
		data.IfaceExclude = exclude
		data.IfaceExcludeDefault = false
		s.fillRetention(&data, retention)
		s.render(w, r, http.StatusUnprocessableEntity, "settings.html", data)
	}
	if !retention.Valid() {
		fail(msgKey("settings.data.error.retention"))
		return
	}
	if _, err := ifacefilter.ValidatePatterns(exclude); err != nil {
		fail(msgKey("settings.data.error.iface_exclude"))
		return
	}
	if err := s.opts.Settings.SaveData(r.Context(), retention, exclude); err != nil {
		s.internalError(w, r, err)
		return
	}
	data, err := s.newSettingsData(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	data.DataSaved = true
	s.render(w, r, http.StatusOK, "settings.html", data)
}

// formInt reads a whole number from the form. Anything else becomes zero,
// which no choice list contains.
func formInt(r *http.Request, name string) int {
	n, err := strconv.Atoi(r.PostFormValue(name))
	if err != nil {
		return 0
	}
	return n
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	next := r.PostFormValue("new_password")
	fail := func(status int, key msgKey) {
		data, err := s.newSettingsData(r.Context())
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		data.PasswordError = key
		s.render(w, r, status, "settings.html", data)
	}
	switch {
	case !auth.ValidPassword(next):
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.password_length"))
		return
	case next != r.PostFormValue("new_password_confirm"):
		fail(http.StatusUnprocessableEntity, msgKey("setup.error.password_mismatch"))
		return
	}
	err := s.opts.Auth.ChangePassword(r.Context(), id, r.PostFormValue("current_password"), next)
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		setRetryAfter(w, limited.RetryAfter)
		fail(http.StatusTooManyRequests, msgKey("error.rate_limited"))
		return
	case errors.Is(err, auth.ErrWrongPassword):
		fail(http.StatusUnprocessableEntity, msgKey("settings.password.error.current"))
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	data, err := s.newSettingsData(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	data.PasswordChanged = true
	s.render(w, r, http.StatusOK, "settings.html", data)
}
