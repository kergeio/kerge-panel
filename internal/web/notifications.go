package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kergeio/kerge-panel/internal/notify"
)

// The Notifications section of the settings page: the webhook, the events
// it receives and the "Send test" button, which posts what is in the form
// (saved or not) to the API below and shows the webhook's reply.

// notifyForm is the Notifications section of settings.html.
type notifyForm struct {
	Enabled    bool
	URL        string
	Body       string
	Offline    bool
	Renewal    bool
	Traffic    bool
	Thresholds []thresholdField
}

// thresholdField is one usage event: its form field prefix, its name and
// its values.
type thresholdField struct {
	Field   string
	Label   msgKey
	On      bool
	Percent int
	Minutes int
}

func notifyFormOf(c notify.Config) notifyForm {
	field := func(name string, label msgKey, t notify.Threshold) thresholdField {
		return thresholdField{Field: name, Label: label, On: t.On, Percent: t.Percent, Minutes: t.Minutes}
	}
	return notifyForm{
		Enabled: c.Enabled, URL: c.URL, Body: c.Body,
		Offline: c.Offline, Renewal: c.Renewal, Traffic: c.Traffic,
		Thresholds: []thresholdField{
			field("disk", msgKey("settings.notify.event.disk"), c.Disk),
			field("memory", msgKey("settings.notify.event.memory"), c.Memory),
			field("cpu", msgKey("settings.notify.event.cpu"), c.CPU),
		},
	}
}

// notifyConfigFrom reads the section's form. A number that does not parse
// becomes zero, which Check rejects.
func notifyConfigFrom(r *http.Request) notify.Config {
	on := func(name string) bool { return r.PostFormValue(name) == "1" }
	threshold := func(name string) notify.Threshold {
		return notify.Threshold{
			On:      on(name + "_on"),
			Percent: formInt(r, name+"_percent"),
			Minutes: formInt(r, name+"_minutes"),
		}
	}
	return notify.Config{
		Enabled: on("notify_enabled"),
		URL:     strings.TrimSpace(r.PostFormValue("webhook_url")),
		Body:    r.PostFormValue("webhook_body"),
		Offline: on("event_offline"),
		Renewal: on("event_renewal"),
		Traffic: on("event_traffic"),
		Disk:    threshold("disk"),
		Memory:  threshold("memory"),
		CPU:     threshold("cpu"),
	}
}

// notifyErrorKey is the message for a reason notify.Config.Check gives.
func notifyErrorKey(err error) msgKey {
	switch {
	case errors.Is(err, notify.ErrURL):
		return msgKey("settings.notify.error.url")
	case errors.Is(err, notify.ErrBody):
		return msgKey("settings.notify.error.body")
	default:
		return msgKey("settings.notify.error.threshold")
	}
}

// saveNotifications stores the Notifications section.
func (s *Server) saveNotifications(w http.ResponseWriter, r *http.Request) {
	c := notifyConfigFrom(r)
	if err := c.Check(); err != nil {
		data, err2 := s.newSettingsData(r.Context())
		if err2 != nil {
			s.internalError(w, r, err2)
			return
		}
		data.NotifyError = notifyErrorKey(err)
		data.Notify = notifyFormOf(c)
		s.render(w, r, http.StatusUnprocessableEntity, "settings.html", data)
		return
	}
	if err := s.opts.Settings.SaveNotifications(r.Context(), c); err != nil {
		s.internalError(w, r, err)
		return
	}
	data, err := s.newSettingsData(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	data.NotifySaved = true
	s.render(w, r, http.StatusOK, "settings.html", data)
}

// testReply is the answer of POST /api/settings/notifications/test. With a
// reply from the webhook, Status reads like "HTTP 200 OK" and OK reports a
// 2xx code; without one, Error says why.
type testReply struct {
	OK      bool   `json:"ok"`
	Status  string `json:"status,omitempty"`
	Headers string `json:"headers"`
	Body    string `json:"body"`
	Error   string `json:"error,omitempty"`
}

// testNotification sends the test message to the webhook given in the
// form fields url and body.
func (s *Server) testNotification(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(r.PostFormValue("url"))
	body := r.PostFormValue("body")
	if err := notify.CheckURL(target); err != nil {
		s.apiError(w, http.StatusUnprocessableEntity, notifyErrorKey(err))
		return
	}
	if err := notify.CheckBody(body); err != nil {
		s.apiError(w, http.StatusUnprocessableEntity, notifyErrorKey(err))
		return
	}
	tr := s.translator()
	payload := notify.Render(body, tr.T(string(msgKey("notify.message.test"))))
	var out testReply
	reply, err := s.webhook.Send(r.Context(), target, payload)
	if err != nil {
		if reason, timedOut := notify.Reason(err); timedOut {
			out.Error = tr.T(string(msgKey("settings.notify.test.timeout")))
		} else {
			out.Error = strings.ReplaceAll(tr.T(string(msgKey("settings.notify.test.no_reply"))), "{reason}", reason)
		}
	} else {
		out.OK = reply.OK()
		out.Status = strings.TrimSpace("HTTP " + strconv.Itoa(reply.Code) + " " + http.StatusText(reply.Code))
		out.Headers = reply.Headers
		out.Body = reply.Body
	}
	b, err := json.Marshal(out)
	if err != nil {
		s.opts.Logger.Error("request failed", "path", r.URL.Path, "err", err)
		s.apiError(w, http.StatusInternalServerError, msgKey("error.internal"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
