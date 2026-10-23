package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kergeio/kerge-panel/internal/notify"
)

func notifyFormValues(token string) url.Values {
	return url.Values{
		"csrf_token":     {token},
		"notify_enabled": {"1"},
		"webhook_url":    {" https://api.telegram.org/bot1:x/sendMessage "},
		"webhook_body":   {`{"chat_id":"1","text":"{{message}}"}`},
		"event_offline":  {"1"},
		"event_traffic":  {"1"},
		"disk_on":        {"1"},
		"disk_percent":   {"85"},
		"disk_minutes":   {"5"},
		"memory_percent": {"90"},
		"memory_minutes": {"10"},
		"cpu_on":         {"1"},
		"cpu_percent":    {"95"},
		"cpu_minutes":    {"15"},
	}
}

func TestNotificationsShowTheDefaults(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	page, _ := settingsForm(t, b)

	for _, want := range []string{
		`action="/settings/notifications"`,
		`name="event_offline" value="1" checked`,
		`name="event_renewal" value="1" checked`,
		`name="event_traffic" value="1" checked`,
		`name="disk_on" value="1" checked`,
		`name="disk_percent" value="90"`,
		`name="disk_minutes" value="5"`,
		`name="memory_minutes" value="10"`,
		`name="cpu_minutes" value="10"`,
		`js/notify.js`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page lacks %s", want)
		}
	}
	for _, unwanted := range []string{
		`name="notify_enabled" value="1" checked`,
		`name="memory_on" value="1" checked`,
		`name="cpu_on" value="1" checked`,
	} {
		if strings.Contains(page, unwanted) {
			t.Errorf("settings page has %s by default", unwanted)
		}
	}
}

func TestSaveNotifications(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)

	resp, page := b.post("/settings/notifications", notifyFormValues(token))
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Settings saved") {
		t.Fatalf("save: %d\n%s", resp.StatusCode, page)
	}
	got, err := e.settings.Notifications(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := notify.Config{
		Enabled: true,
		URL:     "https://api.telegram.org/bot1:x/sendMessage",
		Body:    `{"chat_id":"1","text":"{{message}}"}`,
		Offline: true, Renewal: false, Traffic: true,
		Disk:   notify.Threshold{On: true, Percent: 85, Minutes: 5},
		Memory: notify.Threshold{On: false, Percent: 90, Minutes: 10},
		CPU:    notify.Threshold{On: true, Percent: 95, Minutes: 15},
	}
	if got != want {
		t.Errorf("stored %+v, want %+v", got, want)
	}
	// The saved values come back on the page.
	for _, s := range []string{
		`name="notify_enabled" value="1" checked`,
		`value="https://api.telegram.org/bot1:x/sendMessage"`,
		`name="cpu_percent" value="95"`,
	} {
		if !strings.Contains(page, s) {
			t.Errorf("page after saving lacks %s", s)
		}
	}
	if strings.Contains(page, `name="event_renewal" value="1" checked`) {
		t.Error("the renewal event is still checked after switching it off")
	}
}

func TestSaveNotificationsRejectsBadInput(t *testing.T) {
	cases := []struct {
		name, field, value, message string
	}{
		{"url", "webhook_url", "example.com/hook", "full address"},
		{"body", "webhook_body", `{"text": {{message}}}`, "valid JSON"},
		{"percent", "disk_percent", "0", "1 to 100 percent"},
		{"minutes", "cpu_minutes", "x", "1 to 60 minutes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.initialize()
			b := signedIn(t, e)
			_, token := settingsForm(t, b)
			form := notifyFormValues(token)
			form.Set(c.field, c.value)
			resp, page := b.post("/settings/notifications", form)
			if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, c.message) {
				t.Fatalf("got %d, want 422 with %q\n%s", resp.StatusCode, c.message, page)
			}
			// What was typed stays in the form, and nothing is stored.
			if !strings.Contains(page, `name="cpu_percent" value="95"`) {
				t.Error("the form lost what was typed")
			}
			if got, _ := e.settings.Notifications(t.Context()); got != notify.Default() {
				t.Errorf("stored %+v after a rejected save", got)
			}
		})
	}
}

// sendTest posts the test form as notify.js does.
func sendTest(t *testing.T, b *browser, csrf, target, body string) (int, testReply) {
	t.Helper()
	form := url.Values{"url": {target}, "body": {body}}
	req, err := http.NewRequest(http.MethodPost, b.server.URL+"/api/settings/notifications/test", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out testReply
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	return resp.StatusCode, out
}

func TestSendTestNotification(t *testing.T) {
	var got struct{ Text string }
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer hook.Close()

	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)

	// Nothing has been saved: the test uses what is in the form.
	code, reply := sendTest(t, b, token, hook.URL, `{"text":"{{message}}"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %+v", code, reply)
	}
	if got.Text != "[Kerge] Test notification" {
		t.Errorf("webhook got text %q", got.Text)
	}
	if !reply.OK || reply.Status != "HTTP 200 OK" || reply.Body != "{\n  \"ok\": true\n}" || reply.Error != "" {
		t.Errorf("reply = %+v", reply)
	}
	if !strings.Contains(reply.Headers, "Content-Type: application/json") {
		t.Errorf("headers = %q", reply.Headers)
	}
}

func TestSendTestNotificationReportsFailures(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Bad Request: chat not found"}`)
	}))
	defer hook.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String() + "/hook"
	l.Close()

	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	_, token := settingsForm(t, b)
	body := `{"text":"{{message}}"}`

	code, reply := sendTest(t, b, token, hook.URL, body)
	if code != http.StatusOK || reply.OK || reply.Status != "HTTP 400 Bad Request" ||
		!strings.Contains(reply.Body, `"description": "Bad Request: chat not found"`) {
		t.Errorf("non-2xx reply: %d %+v", code, reply)
	}

	code, reply = sendTest(t, b, token, closed, body)
	if code != http.StatusOK || reply.Status != "" || reply.Error != "No reply: connection refused" {
		t.Errorf("closed port: %d %+v", code, reply)
	}

	code, reply = sendTest(t, b, token, "not a url", body)
	if code != http.StatusUnprocessableEntity || !strings.Contains(reply.Error, "full address") {
		t.Errorf("bad url: %d %+v", code, reply)
	}
	code, reply = sendTest(t, b, token, hook.URL, "plain text")
	if code != http.StatusUnprocessableEntity || !strings.Contains(reply.Error, "valid JSON") {
		t.Errorf("bad body: %d %+v", code, reply)
	}
}

func TestSendTestNotificationNeedsTheCSRFToken(t *testing.T) {
	e := newEnv(t, nil)
	e.initialize()
	b := signedIn(t, e)
	code, _ := sendTest(t, b, "wrong", "https://example.com/hook", `{}`)
	if code != http.StatusForbidden {
		t.Errorf("status %d, want 403", code)
	}
}
