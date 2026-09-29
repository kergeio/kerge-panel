package web

import (
	"strings"
	"testing"
)

// Every page, signed in or not, ends with the notes on the units the panel
// shows.
func TestFooterExplainsTheUnits(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	anonymous := e.browser()
	_, login := anonymous.get("/login")
	b := signedIn(t, e)
	_, home := b.get("/")
	_, settings := b.get("/settings")

	for name, page := range map[string]string{"login": login, "dashboard": home, "settings": settings} {
		i := strings.Index(page, "<footer")
		if i < 0 {
			t.Fatalf("%s has no footer:\n%s", name, page)
		}
		footer := page[i:]
		for _, want := range []string{
			`aria-labelledby="units-title"`,
			"KiB, MiB, GiB, TiB",
			"bytes per second, in the same binary units: K/s, M/s, G/s",
			"a 100 Mbps port moves at most about 11.9 M/s",
			"Chart axes show only the prefix",
			"1 TB is about 931 GiB",
		} {
			if !strings.Contains(footer, want) {
				t.Errorf("%s: the footer lacks %q:\n%s", name, want, footer)
			}
		}
	}
}
