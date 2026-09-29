package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-protocol/ifacefilter"
)

// The interface exclusion of one host: either the panel's rules
// ("System") or the host's own list, which replaces them ("Custom").

// Values of the iface_mode field on the edit page.
const (
	ifaceModeSystem = "system"
	ifaceModeCustom = "custom"
)

// ifaceForm is the interface exclusion part of the edit page.
type ifaceForm struct {
	// Custom reports whether the host has its own list.
	Custom bool
	// List is the host's own list, or the panel's rules for a host that
	// follows them, so that choosing Custom starts from those.
	List string
}

// panelIfaceExclude is the panel's exclusion list as the settings page
// shows it: the stored list, or the built-in one until a list is saved.
func (s *Server) panelIfaceExclude(ctx context.Context) (string, error) {
	p, err := s.opts.Settings.Load(ctx)
	if err != nil {
		return "", err
	}
	if !p.IfaceExcludeChosen {
		return strings.Join(ifacefilter.DefaultExclude, ","), nil
	}
	return p.IfaceExclude, nil
}

// ifaceFormOf shows the saved exclusion of a host.
func (s *Server) ifaceFormOf(ctx context.Context, rec hosts.Record) (ifaceForm, error) {
	if rec.IfaceExclude != nil {
		return ifaceForm{Custom: true, List: *rec.IfaceExclude}, nil
	}
	list, err := s.panelIfaceExclude(ctx)
	return ifaceForm{List: list}, err
}

// postedIfaceForm shows the exclusion as posted, so that a form sent back
// with an error keeps what was chosen and typed.
func postedIfaceForm(r *http.Request) ifaceForm {
	return ifaceForm{
		Custom: r.PostFormValue("iface_mode") == ifaceModeCustom,
		List:   r.PostFormValue("net_iface_exclude"),
	}
}

// parseIfaceForm reads the posted exclusion: nil for System, the list for
// Custom. On failure it returns the message for the field.
func parseIfaceForm(r *http.Request) (list *string, bad msgKey) {
	switch r.PostFormValue("iface_mode") {
	case ifaceModeSystem:
		return nil, ""
	case ifaceModeCustom:
		trimmed := strings.TrimSpace(r.PostFormValue("net_iface_exclude"))
		if _, err := ifacefilter.ValidatePatterns(trimmed); err != nil {
			return nil, msgKey("hosts.error.iface_exclude")
		}
		return &trimmed, ""
	default:
		return nil, msgKey("hosts.error.iface_mode")
	}
}
