package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/install"
	"github.com/kergeio/kerge-panel/internal/reminder"
)

// Host management. There is no list of hosts of its own: hosts are added
// from the dashboard and managed from their detail page. The pages are
// server-rendered forms: adding, editing, deleting and resetting access
// all post to the URL that showed the form, so every state change carries
// the page's CSRF token.

// masked is a host address, which the page shows or hides behind *** as
// the site-wide switch says. Shown is the switch's state when the page
// was rendered. An empty value renders as "none".
type masked struct {
	Value string
	Shown bool
}

// hostFormPage is the data of host_form.html, used for both adding and
// editing. ID is zero when adding.
type hostFormPage struct {
	ID   int64
	Name string
	Note string
	Sort string
	// RemindStart is the first reminder date as the date input writes it,
	// empty when the host has no reminder, and Cycles offers the reminder
	// periods.
	RemindStart string
	Cycles      []option
	HasReminder bool
	// Traffic holds the traffic settings.
	Traffic trafficForm
	// Iface is the host's interface exclusion.
	Iface ifaceForm
	Error msgKey
	// Title and Submit differ between adding and editing.
	Title  msgKey
	Submit msgKey
}

// installPage shows the command that installs the agent on one host. The
// token it carries is shown once: the panel keeps only its hash.
type installPage struct {
	Host    string
	Heading msgKey
	// Command carries the token, Masked has it replaced by ***. The copy
	// button copies Command.
	Command string
	Masked  string
	// Pinned reports whether this build pins an agent release. A
	// development build prints the command with placeholders.
	Pinned bool
	// Back leads to the page the operator came from: the dashboard for
	// a new host, the host's page after resetting its access.
	Back      string
	BackLabel msgKey
}

// confirmPage asks before an action that cannot be undone.
type confirmPage struct {
	Host   string
	Action string
	// Cancel leads back to the host's page.
	Cancel string
	Title  msgKey
	Body   msgKey
	Submit msgKey
}

// noticeFor maps the marker a redirect carries to the message shown. An
// unknown value shows nothing.
func noticeFor(done string) msgKey {
	switch done {
	case "saved":
		return msgKey("hosts.notice.saved")
	case "deleted":
		return msgKey("hosts.notice.deleted")
	}
	return ""
}

// systemLabel describes the host in one line, from whichever fields the
// agent reported. A host that has never reported has none.
func systemLabel(rec hosts.Record) string {
	var b strings.Builder
	platform := rec.Platform
	if platform == "" {
		platform = rec.OS
	}
	b.WriteString(platform)
	if rec.PlatformVersion != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(rec.PlatformVersion)
	}
	if rec.Arch != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString("(" + rec.Arch + ")")
	}
	return b.String()
}

func (s *Server) hostNewPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "host_form.html", newHostForm())
}

func newHostForm() hostFormPage {
	return hostFormPage{
		Sort:   "0",
		Title:  msgKey("hosts.add.title"),
		Submit: msgKey("hosts.add.submit"),
	}
}

func (s *Server) hostCreate(w http.ResponseWriter, r *http.Request) {
	form := newHostForm()
	form.Name = r.PostFormValue("name")
	name, err := hosts.CleanName(form.Name)
	if err != nil {
		form.Error = msgKey("hosts.error.name")
		s.render(w, r, http.StatusBadRequest, "host_form.html", form)
		return
	}
	id, err := s.opts.Hosts.Create(r.Context(), name)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	token, err := s.opts.Hosts.NewEnrollToken(r.Context(), id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.renderInstall(w, r, name, token, msgKey("hosts.install.added"), "/", msgKey("hosts.install.back.dashboard"))
}

// renderInstall shows the install command of a host. It renders instead of
// redirecting, because the token exists only here: the database keeps its
// hash, so a redirect would lose it for good.
func (s *Server) renderInstall(w http.ResponseWriter, r *http.Request, name, token string, heading msgKey,
	back string, backLabel msgKey) {
	endpoint := s.agentEndpoint(r)
	s.render(w, r, http.StatusOK, "host_install.html", installPage{
		Host:      name,
		Heading:   heading,
		Command:   install.Command(endpoint, token),
		Masked:    install.Command(endpoint, install.TokenMask),
		Pinned:    install.Pinned(),
		Back:      back,
		BackLabel: backLabel,
	})
}

// agentEndpoint is the address the install command points the agent at. It
// comes from the panel's configured public URL; a development build with
// none falls back to the address this request arrived on.
func (s *Server) agentEndpoint(r *http.Request) string {
	if s.opts.AgentEndpoint != "" {
		return s.opts.AgentEndpoint
	}
	scheme := "http://"
	if r.TLS != nil {
		scheme = "https://"
	}
	endpoint, err := install.AgentEndpoint(scheme + r.Host)
	if err != nil {
		return "wss://<panel>" + install.AgentPath
	}
	return endpoint
}

func (s *Server) hostEditPage(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.hostByPath(w, r)
	if !ok {
		return
	}
	form := hostFormPage{
		ID:     rec.ID,
		Name:   rec.Name,
		Note:   rec.Note,
		Sort:   strconv.FormatInt(rec.Sort, 10),
		Title:  msgKey("hosts.edit.title"),
		Submit: msgKey("hosts.edit.submit"),
	}
	cycle := strconv.Itoa(reminder.Cycles[0])
	if rec.Reminder != nil {
		form.RemindStart = rec.Reminder.Start.String()
		form.HasReminder = true
		cycle = strconv.Itoa(rec.Reminder.CycleMonths)
	}
	form.Cycles = s.cycleOptions(cycle)
	form.Traffic = s.trafficFormOf(rec.Traffic)
	iface, err := s.ifaceFormOf(r.Context(), rec)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	form.Iface = iface
	s.render(w, r, http.StatusOK, "host_form.html", form)
}

func (s *Server) hostUpdate(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.hostByPath(w, r)
	if !ok {
		return
	}
	form := hostFormPage{
		ID:          rec.ID,
		Name:        r.PostFormValue("name"),
		Note:        r.PostFormValue("note"),
		Sort:        r.PostFormValue("sort"),
		RemindStart: strings.TrimSpace(r.PostFormValue("remind_start")),
		Cycles:      s.cycleOptions(r.PostFormValue("cycle_months")),
		HasReminder: rec.Reminder != nil,
		Traffic: s.newTrafficForm(r.PostFormValue("traffic_reset_day"),
			r.PostFormValue("traffic_mode"), r.PostFormValue("traffic_limit")),
		Iface:  postedIfaceForm(r),
		Title:  msgKey("hosts.edit.title"),
		Submit: msgKey("hosts.edit.submit"),
	}
	// "Clear reminder" submits the same form, so the other fields are
	// saved along with it.
	clearReminder := r.PostFormValue("reminder_clear") != "" || form.RemindStart == ""
	if clearReminder {
		form.RemindStart = ""
	}
	fail := func(key msgKey) {
		form.Error = key
		s.render(w, r, http.StatusBadRequest, "host_form.html", form)
	}
	name, err := hosts.CleanName(form.Name)
	if err != nil {
		fail(msgKey("hosts.error.name"))
		return
	}
	note, err := hosts.CleanNote(form.Note)
	if err != nil {
		fail(msgKey("hosts.error.note"))
		return
	}
	sort, err := strconv.ParseInt(strings.TrimSpace(form.Sort), 10, 64)
	if err != nil {
		fail(msgKey("hosts.error.sort"))
		return
	}
	var (
		start reminder.Date
		cycle int
	)
	if !clearReminder {
		if start, err = reminder.Parse(form.RemindStart); err != nil {
			fail(msgKey("hosts.error.remind_start"))
			return
		}
		if cycle, err = strconv.Atoi(r.PostFormValue("cycle_months")); err != nil || !reminder.ValidCycle(cycle) {
			fail(msgKey("hosts.error.cycle"))
			return
		}
	}
	trafficSettings, bad := parseTrafficForm(r)
	if bad != "" {
		fail(bad)
		return
	}
	ifaceList, bad := parseIfaceForm(r)
	if bad != "" {
		fail(bad)
		return
	}
	switch err := s.opts.Hosts.Update(r.Context(), rec.ID, name, sort, note); {
	case errors.Is(err, hosts.ErrNotFound):
		s.render(w, r, http.StatusNotFound, "not_found.html", nil)
		return
	case err != nil:
		// The service checks the same rules as the form; anything it
		// still rejects is about the sort range.
		fail(msgKey("hosts.error.sort"))
		return
	}
	if clearReminder {
		err = s.opts.Hosts.ClearReminder(r.Context(), rec.ID)
	} else {
		err = s.opts.Hosts.SetReminder(r.Context(), rec.ID, start, cycle)
	}
	if err == nil {
		err = s.opts.Hosts.SetTraffic(r.Context(), rec.ID, trafficSettings)
	}
	if err == nil {
		err = s.opts.Hosts.SetIfaceExclude(r.Context(), rec.ID, ifaceList)
	}
	switch {
	case errors.Is(err, hosts.ErrNotFound):
		s.render(w, r, http.StatusNotFound, "not_found.html", nil)
	case err != nil:
		s.internalError(w, r, err)
	default:
		http.Redirect(w, r, hostPath(rec.ID)+"?done=saved", http.StatusSeeOther)
	}
}

func (s *Server) hostDeletePage(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.hostByPath(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "host_confirm.html", confirmPage{
		Host:   rec.Name,
		Action: hostPath(rec.ID) + "/delete",
		Cancel: hostPath(rec.ID),
		Title:  msgKey("hosts.delete.title"),
		Body:   msgKey("hosts.delete.body"),
		Submit: msgKey("hosts.delete.submit"),
	})
}

func (s *Server) hostDelete(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.hostByPath(w, r)
	if !ok {
		return
	}
	agentID, err := s.opts.Hosts.Delete(r.Context(), rec.ID)
	if errors.Is(err, hosts.ErrNotFound) {
		s.render(w, r, http.StatusNotFound, "not_found.html", nil)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.opts.Agents != nil {
		s.opts.Agents.Revoke(agentID)
		s.opts.Agents.Forget(rec.ID)
	}
	http.Redirect(w, r, "/?done=deleted", http.StatusSeeOther)
}

func (s *Server) hostResetPage(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.hostByPath(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "host_confirm.html", confirmPage{
		Host:   rec.Name,
		Action: hostPath(rec.ID) + "/reset",
		Cancel: hostPath(rec.ID),
		Title:  msgKey("hosts.reset.title"),
		Body:   msgKey("hosts.reset.body"),
		Submit: msgKey("hosts.reset.submit"),
	})
}

func (s *Server) hostReset(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.hostByPath(w, r)
	if !ok {
		return
	}
	agentID, token, err := s.opts.Hosts.ResetAccess(r.Context(), rec.ID)
	if errors.Is(err, hosts.ErrNotFound) {
		s.render(w, r, http.StatusNotFound, "not_found.html", nil)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.opts.Agents != nil {
		s.opts.Agents.Revoke(agentID)
	}
	s.renderInstall(w, r, rec.Name, token, msgKey("hosts.install.reset"), hostPath(rec.ID), msgKey("hosts.install.back.host"))
}

// hostPath is the address of a host's detail page.
func hostPath(id int64) string {
	return "/hosts/" + strconv.FormatInt(id, 10)
}

// hostByPath resolves the {id} of the route. It writes the 404 page and
// reports false when there is no such host.
func (s *Server) hostByPath(w http.ResponseWriter, r *http.Request) (hosts.Record, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		rec, err := s.opts.Hosts.Get(r.Context(), id)
		switch {
		case err == nil:
			return rec, true
		case !errors.Is(err, hosts.ErrNotFound):
			s.internalError(w, r, err)
			return hosts.Record{}, false
		}
	}
	s.render(w, r, http.StatusNotFound, "not_found.html", nil)
	return hosts.Record{}, false
}
