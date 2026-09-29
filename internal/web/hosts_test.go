package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/install"
	"github.com/kergeio/kerge-protocol"
)

// signedIn returns a browser with an admin session.
func signedIn(t *testing.T, e *env) *browser {
	t.Helper()
	b := e.browser()
	_, page := b.get("/login")
	resp, _ := b.post("/login", url.Values{
		"csrf_token": {csrfFrom(t, page)}, "username": {"admin"}, "password": {adminPassword},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in: %d", resp.StatusCode)
	}
	return b
}

// withEditDefaults adds the traffic and interface exclusion fields the edit
// page always posts, with their defaults, unless the form already has them.
func withEditDefaults(form url.Values) url.Values {
	for k, v := range map[string]string{
		"traffic_reset_day": "1", "traffic_mode": "both", "traffic_limit": "",
		"iface_mode": "system", "net_iface_exclude": "",
	} {
		if _, ok := form[k]; !ok {
			form.Set(k, v)
		}
	}
	return form
}

// addHost walks the add form and returns the id of the new host and the
// install page it lands on.
func addHost(t *testing.T, e *env, b *browser, name string) (int64, string) {
	t.Helper()
	_, page := b.get("/hosts/new")
	resp, page := b.post("/hosts/new", url.Values{"csrf_token": {csrfFrom(t, page)}, "name": {name}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add host: %d\n%s", resp.StatusCode, page)
	}
	list, err := e.hosts.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range list {
		if rec.Name == name {
			return rec.ID, page
		}
	}
	t.Fatalf("host %q was not created", name)
	return 0, ""
}

// A host is added from the dashboard, edited and deleted from its own
// page, and the dashboard reflects each step; there is no list of hosts
// of its own.
func TestHostManagementFlow(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)

	_, page := b.get("/")
	if !strings.Contains(page, "No hosts yet") || !strings.Contains(page, `href="/hosts/new"`) {
		t.Errorf("empty dashboard:\n%s", page)
	}
	if resp, _ := b.get("/hosts"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/hosts: %d, want 404", resp.StatusCode)
	}

	id, _ := addHost(t, e, b, "web-1")
	_, page = b.get("/")
	if !strings.Contains(page, "web-1") || !strings.Contains(page, "Pending") {
		t.Errorf("dashboard after adding:\n%s", page)
	}

	// Edit: name, note and sort all take, and the host's page says so.
	path := "/hosts/" + strconv.FormatInt(id, 10)
	_, page = b.get(path + "/edit")
	if !strings.Contains(page, `href="`+path+`" class="text-sm text-gray-600 underline`) {
		t.Errorf("the edit page does not lead back to the host:\n%s", page)
	}
	resp, page := b.post(path+"/edit", withEditDefaults(url.Values{
		"csrf_token": {csrfFrom(t, page)}, "name": {"web-1 renamed"}, "note": {"production"}, "sort": {"-5"},
	}))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != path+"?done=saved" {
		t.Fatalf("edit: %d %q\n%s", resp.StatusCode, resp.Header.Get("Location"), page)
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "web-1 renamed" || rec.Note != "production" || rec.Sort != -5 {
		t.Errorf("record after editing = %+v", rec)
	}
	_, page = b.get(path + "?done=saved")
	if !strings.Contains(page, "Host saved") || !strings.Contains(page, "production") {
		t.Errorf("host page after editing:\n%s", page)
	}

	// Delete asks first, and the host is still there until it is confirmed.
	_, page = b.get(path + "/delete")
	if !strings.Contains(page, "Delete this host?") || !strings.Contains(page, "web-1 renamed") ||
		!strings.Contains(page, `href="`+path+`"`) {
		t.Errorf("delete confirmation:\n%s", page)
	}
	if _, err := e.hosts.Get(t.Context(), id); err != nil {
		t.Errorf("the host is gone before the confirmation: %v", err)
	}

	resp, page = b.post(path+"/delete", url.Values{"csrf_token": {csrfFrom(t, page)}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?done=deleted" {
		t.Fatalf("delete: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := e.hosts.Get(t.Context(), id); err == nil {
		t.Error("the host survived its deletion")
	}
	if len(e.agents.forgotten) != 1 || e.agents.forgotten[0] != id {
		t.Errorf("forgotten hosts = %v, want [%d]", e.agents.forgotten, id)
	}
	if _, page = b.get("/?done=deleted"); !strings.Contains(page, "Host deleted.") {
		t.Errorf("dashboard after deleting:\n%s", page)
	}
}

// The install command carries the token exactly once, in the attribute the
// copy button reads; what the page shows has it masked.
func TestInstallCommandMasksTheToken(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, page := addHost(t, e, b, "web-1")

	token := tokenFrom(t, page)
	if n := strings.Count(page, token); n != 1 {
		t.Errorf("the token appears %d times in the page, want once (the copy attribute)", n)
	}
	// What the page prints has the token replaced.
	shown, ok := strings.CutPrefix(page[strings.Index(page, "<code"):], "<code data-command-text>")
	if !ok {
		t.Fatalf("no command element in page:\n%s", page)
	}
	shown, _, _ = strings.Cut(shown, "</code>")
	if !strings.Contains(shown, "--token "+install.TokenMask) || strings.Contains(shown, token) {
		t.Errorf("the printed command does not mask the token: %s", shown)
	}
	// The command checks the install script's checksum before running it
	// and points the agent at the endpoint of this panel.
	for _, want := range []string{"sha256sum -c --quiet -", "install-agent.sh", "/api/agent/ws"} {
		if !strings.Contains(page, want) {
			t.Errorf("the command is missing %q:\n%s", want, page)
		}
	}
	// A development build says the command cannot be run as printed.
	if !strings.Contains(page, "pins no agent release") {
		t.Errorf("no warning about the unpinned build:\n%s", page)
	}
}

// tokenFrom digs the enrollment token out of the command the copy button
// would put on the clipboard.
func tokenFrom(t *testing.T, page string) string {
	t.Helper()
	m := commandAttr.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no install command in page:\n%s", page)
	}
	command := html.UnescapeString(m[1])
	_, rest, ok := strings.Cut(command, "--token ")
	if !ok {
		t.Fatalf("no token in the install command: %s", command)
	}
	token, _, _ := strings.Cut(rest, ";")
	token = strings.TrimSpace(token)
	if token == "" || token == install.TokenMask {
		t.Fatalf("the page carries no real token: %q", token)
	}
	return token
}

var commandAttr = regexp.MustCompile(`data-full="([^"]*)"`)

// Resetting access closes the agent's connection and hands out a new
// command.
func TestResetAccessIssuesANewCommand(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, page := addHost(t, e, b, "web-1")
	first := tokenFrom(t, page)

	// Enroll an agent, so the reset has a credential to revoke.
	auth, err := e.hosts.Authenticate(t.Context(), protocol.EnrollAuthorization(first))
	if err != nil {
		t.Fatal(err)
	}

	_, page = b.get("/hosts/" + strconv.FormatInt(id, 10) + "/reset")
	if !strings.Contains(page, "Reset access for this host?") {
		t.Errorf("reset confirmation:\n%s", page)
	}
	resp, page := b.post("/hosts/"+strconv.FormatInt(id, 10)+"/reset",
		url.Values{"csrf_token": {csrfFrom(t, page)}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Access reset") {
		t.Fatalf("reset: %d\n%s", resp.StatusCode, page)
	}
	if second := tokenFrom(t, page); second == first {
		t.Error("the reset handed out the same token again")
	}
	if len(e.agents.revoked) != 1 || e.agents.revoked[0] != auth.AgentID {
		t.Errorf("revoked agents = %v, want [%s]", e.agents.revoked, auth.AgentID)
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Pending() {
		t.Error("the host is not pending after a reset")
	}
}

// Host addresses follow one switch for the whole panel: masked until the
// operator turns it on, shown on every page once it is on, whatever page
// is loaded next.
func TestAddressesFollowTheRevealSwitch(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, page := addHost(t, e, b, "web-1")
	auth, err := e.hosts.Authenticate(t.Context(), protocol.EnrollAuthorization(tokenFrom(t, page)))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.hosts.Connected(t.Context(), auth.HostID, testIP()); err != nil {
		t.Fatal(err)
	}
	ip := testIP().String()
	pages := []string{"/", "/hosts/" + strconv.FormatInt(id, 10)}

	for _, path := range pages {
		_, page := b.get(path)
		if !strings.Contains(page, `data-value="`+ip+`">&#42;&#42;&#42;</span>`) {
			t.Errorf("%s: the address is not masked:\n%s", path, page)
		}
		// Outside the attribute the address is not printed anywhere. The
		// dashboard shows the host twice: as a card and as a row of the
		// list.
		want := 1
		if path == "/" {
			want = 2
		}
		if n, m := strings.Count(page, ip), strings.Count(page, `data-value="`+ip+`"`); n != want || m != want {
			t.Errorf("%s: the address appears %d times in %d attributes, want %d", path, n, m, want)
		}
		if !strings.Contains(page, `data-reveal-toggle aria-pressed="false" aria-label="Show IP addresses"`) {
			t.Errorf("%s: the switch is not shown off:\n%s", path, page)
		}
	}

	csrf := csrfFrom(t, page)
	if code := b.reveal("1", csrf); code != http.StatusNoContent {
		t.Fatalf("turning the switch on: %d", code)
	}
	for _, path := range pages {
		_, page := b.get(path)
		if !strings.Contains(page, `data-value="`+ip+`">`+ip+`</span>`) {
			t.Errorf("%s: the address is not shown:\n%s", path, page)
		}
		if !strings.Contains(page, `data-reveal-toggle aria-pressed="true" aria-label="Hide IP addresses"`) {
			t.Errorf("%s: the switch is not shown on:\n%s", path, page)
		}
	}
	// The choice outlives the session.
	fresh := signedIn(t, e)
	if _, page := fresh.get("/"); !strings.Contains(page, `">`+ip+`</span>`) {
		t.Errorf("a new session masks again:\n%s", page)
	}

	if code := b.reveal("0", csrf); code != http.StatusNoContent {
		t.Fatalf("turning the switch off: %d", code)
	}
	if _, page := b.get("/"); !strings.Contains(page, `">&#42;&#42;&#42;</span>`) {
		t.Errorf("the address is shown after turning the switch off:\n%s", page)
	}

	for _, value := range []string{"", "yes", "2"} {
		if code := b.reveal(value, csrf); code != http.StatusBadRequest {
			t.Errorf("reveal=%q: %d, want 400", value, code)
		}
	}
	if code := b.reveal("1", ""); code != http.StatusForbidden {
		t.Errorf("without a token: %d, want 403", code)
	}
	if on, _ := e.settings.RevealIPs(t.Context()); on {
		t.Error("a rejected request turned the switch on")
	}
}

// reveal posts the site-wide switch as mask.js does and returns the status.
func (b *browser) reveal(value, csrf string) int {
	b.t.Helper()
	return b.postSetting("/api/settings/reveal", url.Values{"reveal": {value}}, csrf)
}

// postSetting posts a form to one of the settings endpoints the page's
// scripts call, and returns the status.
func (b *browser) postSetting(path string, form url.Values, csrf string) int {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.server.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestHostFormsRejectBadInput(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)

	_, page := b.get("/hosts/new")
	token := csrfFrom(t, page)
	resp, page := b.post("/hosts/new", url.Values{"csrf_token": {token}, "name": {"   "}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "The name must be") {
		t.Errorf("empty name: %d\n%s", resp.StatusCode, page)
	}
	if list, err := e.hosts.List(t.Context()); err != nil || len(list) != 0 {
		t.Errorf("hosts = %v, %v; want none", list, err)
	}

	id, _ := addHost(t, e, b, "web-1")
	path := "/hosts/" + strconv.FormatInt(id, 10) + "/edit"
	_, page = b.get(path)
	token = csrfFrom(t, page)
	cases := map[string]struct {
		form url.Values
		want string
	}{
		"empty name": {url.Values{"name": {""}, "sort": {"0"}}, "The name must be"},
		"long name": {url.Values{
			"name": {strings.Repeat("x", hosts.MaxNameLen+1)}, "sort": {"0"},
		}, "The name must be"},
		"long note": {url.Values{
			"name": {"web-1"}, "note": {strings.Repeat("x", hosts.MaxNoteLen+1)}, "sort": {"0"},
		}, "The note must be"},
		"sort is not a number": {url.Values{"name": {"web-1"}, "sort": {"soon"}}, "The sort value must be"},
		"sort out of range": {url.Values{
			"name": {"web-1"}, "sort": {strconv.FormatInt(hosts.MaxSort+1, 10)},
		}, "The sort value must be"},
	}
	for label, c := range cases {
		c.form.Set("csrf_token", token)
		resp, page := b.post(path, withEditDefaults(c.form))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, c.want) {
			t.Errorf("%s: %d\n%s", label, resp.StatusCode, page)
		}
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "web-1" || rec.Note != "" || rec.Sort != 0 {
		t.Errorf("the record changed despite the rejections: %+v", rec)
	}
}

func TestHostPagesNeedASessionAndAToken(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	id, _ := addHost(t, e, b, "web-1")
	idPath := "/hosts/" + strconv.FormatInt(id, 10)

	// Without a session every page redirects to the login form.
	anon := e.browser()
	for _, path := range []string{"/hosts", "/hosts/new", idPath + "/edit", idPath + "/delete", idPath + "/reset"} {
		resp, _ := anon.get(path)
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
			t.Errorf("GET %s without a session: %d %q", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	// With a session but no CSRF token nothing changes state.
	for _, path := range []string{"/hosts/new", idPath + "/edit", idPath + "/delete", idPath + "/reset"} {
		resp, _ := b.post(path, url.Values{"name": {"other"}, "sort": {"0"}})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without a CSRF token: %d", path, resp.StatusCode)
		}
	}
	rec, err := e.hosts.Get(t.Context(), id)
	if err != nil || rec.Name != "web-1" {
		t.Errorf("record = %+v, %v", rec, err)
	}
	if len(e.agents.revoked) != 0 || len(e.agents.forgotten) != 0 {
		t.Errorf("agent endpoint was touched: %v %v", e.agents.revoked, e.agents.forgotten)
	}
}

func TestUnknownHostIsNotFound(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	// A page is needed only for its CSRF token.
	_, page := b.get("/hosts")
	token := csrfFrom(t, page)

	for _, path := range []string{"/hosts/404/edit", "/hosts/404/delete", "/hosts/404/reset", "/hosts/x/edit"} {
		if resp, _ := b.get(path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, resp.StatusCode)
		}
		resp, _ := b.post(path, url.Values{"csrf_token": {token}, "name": {"web-1"}, "sort": {"0"}})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %d", path, resp.StatusCode)
		}
	}
}
