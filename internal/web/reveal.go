package web

import (
	"context"
	"net/http"
)

// The site-wide switch that shows or masks host addresses. The panel
// keeps its state, so a page renders the addresses the way the operator
// last chose, and the navigation bar's button changes it for every page
// at once. The enrollment token on the
// install page is another kind of secret and keeps its own button.

// revealIPs reports whether host addresses are shown. A failure to read
// the setting masks them, which is the safe side.
func (s *Server) revealIPs(ctx context.Context) bool {
	reveal, err := s.opts.Settings.RevealIPs(ctx)
	if err != nil {
		s.opts.Logger.Error("reading the reveal setting", "err", err)
		return false
	}
	return reveal
}

// saveReveal stores the switch (POST /api/settings/reveal with
// reveal=1 or reveal=0) and answers 204.
func (s *Server) saveReveal(w http.ResponseWriter, r *http.Request) {
	var reveal bool
	switch r.PostFormValue("reveal") {
	case "1":
		reveal = true
	case "0":
	default:
		s.apiError(w, http.StatusBadRequest, msgKey("error.bad_request"))
		return
	}
	if err := s.opts.Settings.SetRevealIPs(r.Context(), reveal); err != nil {
		s.opts.Logger.Error("request failed", "path", r.URL.Path, "err", err)
		s.apiError(w, http.StatusInternalServerError, msgKey("error.internal"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
