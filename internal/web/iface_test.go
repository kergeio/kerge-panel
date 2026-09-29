package web

import (
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kergeio/kerge-protocol/ifacefilter"
)

// ifaceShown reports which mode the edit page has checked and what its
// list field holds.
func ifaceShown(t *testing.T, page string) (mode, list string) {
	t.Helper()
	for _, m := range []string{"system", "custom"} {
		if strings.Contains(page, `name="iface_mode" value="`+m+`" class="accent-gray-900" checked>`) {
			if mode != "" {
				t.Fatalf("both modes are checked:\n%s", page)
			}
			mode = m
		}
	}
	i := strings.Index(page, `name="net_iface_exclude" type="text" value="`)
	if i < 0 {
		t.Fatalf("no list field:\n%s", page)
	}
	rest := page[i+len(`name="net_iface_exclude" type="text" value="`):]
	return mode, html.UnescapeString(rest[:strings.Index(rest, `"`)])
}

// A host follows the panel's rules until it is given its own list; the
// field starts from the panel's rules, an empty custom list is kept as
// such, and going back to System drops the list.
func TestEditPageSetsTheInterfaceExclusion(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	path := "/hosts/" + strconv.FormatInt(id, 10) + "/edit"
	stored := func() *string {
		t.Helper()
		rec, err := e.hosts.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return rec.IfaceExclude
	}
	save := func(page string, fields url.Values) (*http.Response, string) {
		t.Helper()
		form := withEditDefaults(url.Values{"csrf_token": {csrfFrom(t, page)}, "name": {"web-1"}, "sort": {"0"}})
		for k, v := range fields {
			form[k] = v
		}
		return b.post(path, form)
	}

	// A new host follows the built-in list the panel starts with.
	_, page := b.get(path)
	if mode, list := ifaceShown(t, page); mode != "system" || list != strings.Join(ifacefilter.DefaultExclude, ",") {
		t.Fatalf("new host: %q %q", mode, list)
	}
	if !strings.Contains(page, "This host follows the interface rules on the Settings page.") ||
		!strings.Contains(page, "These rules replace the ones on the Settings page") {
		t.Fatalf("hints missing:\n%s", page)
	}

	// Once the panel has its own rules, those are what Custom starts from.
	p, err := e.settings.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.settings.SaveData(t.Context(), p.Retention, "lo,eth9"); err != nil {
		t.Fatal(err)
	}
	_, page = b.get(path)
	if mode, list := ifaceShown(t, page); mode != "system" || list != "lo,eth9" {
		t.Fatalf("after the panel rules changed: %q %q", mode, list)
	}

	resp, body := save(page, url.Values{"iface_mode": {"custom"}, "net_iface_exclude": {"  lo, docker*  "}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save custom: %d\n%s", resp.StatusCode, body)
	}
	if got := stored(); got == nil || *got != "lo, docker*" {
		t.Fatalf("stored: %v", got)
	}
	_, page = b.get(path)
	if mode, list := ifaceShown(t, page); mode != "custom" || list != "lo, docker*" {
		t.Fatalf("custom shown as %q %q", mode, list)
	}

	// An empty custom list counts every interface, which is not the same
	// as following the panel.
	if resp, body = save(page, url.Values{"iface_mode": {"custom"}, "net_iface_exclude": {""}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save empty custom: %d\n%s", resp.StatusCode, body)
	}
	if got := stored(); got == nil || *got != "" {
		t.Fatalf("empty custom stored as %v", got)
	}

	// Back to System: whatever the field holds, the host follows the panel.
	_, page = b.get(path)
	if resp, body = save(page, url.Values{"iface_mode": {"system"}, "net_iface_exclude": {"eth0"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save system: %d\n%s", resp.StatusCode, body)
	}
	if got := stored(); got != nil {
		t.Fatalf("system stored as %q", *got)
	}
}

func TestEditPageRejectsBadInterfaceExclusion(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	path := "/hosts/" + strconv.FormatInt(id, 10) + "/edit"
	_, page := b.get(path)
	token := csrfFrom(t, page)

	cases := []struct {
		fields url.Values
		want   string
	}{
		{url.Values{"iface_mode": {"custom"}, "net_iface_exclude": {"eth0,,eth1"}}, "Each pattern must be"},
		{url.Values{"iface_mode": {"custom"}, "net_iface_exclude": {"has space"}}, "Each pattern must be"},
		{url.Values{"iface_mode": {"custom"}, "net_iface_exclude": {strings.Repeat("x", 33)}}, "Each pattern must be"},
		{url.Values{"iface_mode": {"both"}}, "Choose System or Custom"},
		{url.Values{"iface_mode": {""}}, "Choose System or Custom"},
	}
	for _, c := range cases {
		form := withEditDefaults(url.Values{"csrf_token": {token}, "name": {"renamed"}, "sort": {"0"}})
		for k, v := range c.fields {
			form[k] = v
		}
		resp, page := b.post(path, form)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, c.want) {
			t.Errorf("%v: %d\n%s", c.fields, resp.StatusCode, page)
			continue
		}
		// The form keeps what was chosen and typed.
		if c.fields.Get("iface_mode") == "custom" {
			if mode, list := ifaceShown(t, page); mode != "custom" || list != c.fields.Get("net_iface_exclude") {
				t.Errorf("%v: sent back as %q %q", c.fields, mode, list)
			}
		}
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "web-1" || rec.IfaceExclude != nil {
		t.Fatalf("a rejected form saved something: %q %v", rec.Name, rec.IfaceExclude)
	}
}
