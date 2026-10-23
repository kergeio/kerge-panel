// Package notify holds the outbound notification settings and sends
// messages to the one webhook the operator configures.
package notify

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// Placeholder is replaced in the request body with the message text.
const Placeholder = "{{message}}"

// Limits on what the settings page accepts.
const (
	MaxURLLength  = 2048
	MaxBodyLength = 8192
)

// Config is the notification settings: the webhook and which events are
// sent. Thresholds apply to every host.
type Config struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Body    string `json:"body"`

	Offline bool `json:"offline"`
	Renewal bool `json:"renewal"`
	Traffic bool `json:"traffic"`

	Disk   Threshold `json:"disk"`
	Memory Threshold `json:"memory"`
	CPU    Threshold `json:"cpu"`
}

// Threshold is a usage event: it fires once usage has stayed at or above
// Percent for Minutes.
type Threshold struct {
	On      bool `json:"on"`
	Percent int  `json:"percent"`
	Minutes int  `json:"minutes"`
}

// Ranges a threshold accepts.
const (
	MinPercent = 1
	MaxPercent = 100
	MinMinutes = 1
	MaxMinutes = 60
)

// Default is the configuration before the operator saves one: no webhook,
// and the events switched on or off as the settings page first shows them.
func Default() Config {
	return Config{
		Offline: true,
		Renewal: true,
		Traffic: true,
		Disk:    Threshold{On: true, Percent: 90, Minutes: 5},
		Memory:  Threshold{On: false, Percent: 90, Minutes: 10},
		CPU:     Threshold{On: false, Percent: 90, Minutes: 10},
	}
}

// Decode reads a stored configuration. Fields it lacks keep their
// defaults, and an unreadable value gives the defaults.
func Decode(value string) Config {
	c := Default()
	if err := json.Unmarshal([]byte(value), &c); err != nil {
		return Default()
	}
	return c
}

// Encode returns the configuration in the form Decode reads.
func (c Config) Encode() (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

// Reasons Check rejects a configuration.
var (
	ErrURL       = errors.New("notify: the webhook URL must be an absolute http or https address")
	ErrBody      = errors.New("notify: the request body must be JSON once the placeholder is filled in")
	ErrThreshold = errors.New("notify: a threshold or duration is out of range")
)

// Check reports whether the configuration can be saved. The webhook must
// be complete while notifications are on; while they are off, whatever
// has been entered must still be valid.
func (c Config) Check() error {
	if c.Enabled || c.URL != "" {
		if err := CheckURL(c.URL); err != nil {
			return err
		}
	}
	if c.Enabled || c.Body != "" {
		if err := CheckBody(c.Body); err != nil {
			return err
		}
	}
	for _, t := range []Threshold{c.Disk, c.Memory, c.CPU} {
		if t.Percent < MinPercent || t.Percent > MaxPercent || t.Minutes < MinMinutes || t.Minutes > MaxMinutes {
			return ErrThreshold
		}
	}
	return nil
}

// CheckURL reports whether raw is an absolute http or https address.
func CheckURL(raw string) error {
	if raw == "" || len(raw) > MaxURLLength || strings.ContainsAny(raw, " \t\r\n") {
		return ErrURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrURL
	}
	return nil
}

// CheckBody reports whether body is valid JSON once the placeholder is
// filled in.
func CheckBody(body string) error {
	if len(body) > MaxBodyLength || !json.Valid([]byte(Render(body, "message"))) {
		return ErrBody
	}
	return nil
}

// Render returns body with every placeholder replaced by message, escaped
// for use inside a JSON string.
func Render(body, message string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = enc.Encode(message)
	quoted := strings.TrimSuffix(b.String(), "\n")
	return strings.ReplaceAll(body, Placeholder, quoted[1:len(quoted)-1])
}
