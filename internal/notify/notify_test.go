package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRenderEscapesTheMessage(t *testing.T) {
	body := `{"chat_id":"1","text":"{{message}}","copy":"{{message}}"}`
	msg := "[Kerge] web-1 \"quoted\" <tag> & back\\slash\nline two"
	got := Render(body, msg)
	var v struct{ Text, Copy string }
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("rendered body is not JSON: %v\n%s", err, got)
	}
	if v.Text != msg || v.Copy != msg {
		t.Errorf("text = %q, copy = %q; want %q", v.Text, v.Copy, msg)
	}
	if strings.Contains(got, `\`+"u003c") {
		t.Errorf("HTML characters were escaped: %s", got)
	}
}

func TestCheck(t *testing.T) {
	good := Default()
	good.Enabled = true
	good.URL = "https://api.telegram.org/bot1:x/sendMessage"
	good.Body = `{"text":"{{message}}"}`
	if err := good.Check(); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	if err := Default().Check(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}

	tests := []struct {
		name   string
		change func(*Config)
		want   error
	}{
		{"enabled without url", func(c *Config) { c.URL = "" }, ErrURL},
		{"relative url", func(c *Config) { c.URL = "/hook" }, ErrURL},
		{"other scheme", func(c *Config) { c.URL = "ftp://example.com/x" }, ErrURL},
		{"no host", func(c *Config) { c.URL = "https:///x" }, ErrURL},
		{"space in url", func(c *Config) { c.URL = "https://example.com/a b" }, ErrURL},
		{"long url", func(c *Config) { c.URL = "https://example.com/" + strings.Repeat("a", MaxURLLength) }, ErrURL},
		{"enabled without body", func(c *Config) { c.Body = "" }, ErrBody},
		{"body not json", func(c *Config) { c.Body = `text={{message}}` }, ErrBody},
		{"placeholder outside a string", func(c *Config) { c.Body = `{"text":{{message}}}` }, ErrBody},
		{"long body", func(c *Config) { c.Body = `"` + strings.Repeat("a", MaxBodyLength) + `"` }, ErrBody},
		{"disabled with bad url", func(c *Config) { c.Enabled = false; c.URL = "nope" }, ErrURL},
		{"percent zero", func(c *Config) { c.Disk.Percent = 0 }, ErrThreshold},
		{"percent over", func(c *Config) { c.Memory.Percent = 101 }, ErrThreshold},
		{"minutes zero", func(c *Config) { c.CPU.Minutes = 0 }, ErrThreshold},
		{"minutes over", func(c *Config) { c.Disk.Minutes = 61 }, ErrThreshold},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := good
			tt.change(&c)
			if err := c.Check(); !errors.Is(err, tt.want) {
				t.Errorf("Check() = %v, want %v", err, tt.want)
			}
		})
	}

	// Switched off and empty is fine: nothing has been entered yet.
	off := Default()
	if err := off.Check(); err != nil {
		t.Errorf("empty disabled webhook rejected: %v", err)
	}
}

func TestEncodeDecode(t *testing.T) {
	c := Default()
	c.Enabled = true
	c.URL = "https://example.com/hook"
	c.Body = `{"text":"{{message}}"}`
	c.Traffic = false
	c.CPU = Threshold{On: true, Percent: 75, Minutes: 3}
	value, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got := Decode(value); got != c {
		t.Errorf("Decode(Encode(c)) = %+v, want %+v", got, c)
	}
	if got := Decode("not json"); got != Default() {
		t.Errorf("unreadable value gave %+v, want the defaults", got)
	}
	// A stored value missing a field keeps that field's default.
	if got := Decode(`{"enabled":true}`); got.Disk != Default().Disk || !got.Enabled {
		t.Errorf("partial value gave %+v", got)
	}
}

func TestSendReportsTheReply(t *testing.T) {
	var gotBody, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotType = string(b), r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("X-Multi", "b")
		w.Header().Add("X-Multi", "a")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1}}`)
	}))
	defer srv.Close()

	reply, err := NewSender().Send(t.Context(), srv.URL, `{"text":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"text":"hi"}` || gotType != "application/json" {
		t.Errorf("webhook got body %q, content type %q", gotBody, gotType)
	}
	if !reply.OK() || reply.Code != 200 {
		t.Errorf("code = %d, ok = %v", reply.Code, reply.OK())
	}
	want := "{\n  \"ok\": true,\n  \"result\": {\n    \"id\": 1\n  }\n}"
	if reply.Body != want {
		t.Errorf("body = %q, want %q", reply.Body, want)
	}
	if !strings.Contains(reply.Headers, "Content-Type: application/json") ||
		!strings.Contains(reply.Headers, "X-Multi: b\nX-Multi: a") {
		t.Errorf("headers = %q", reply.Headers)
	}
	if strings.Index(reply.Headers, "Content-Type") > strings.Index(reply.Headers, "X-Multi") {
		t.Errorf("headers are not sorted: %q", reply.Headers)
	}
}

func TestSendKeepsOtherBodiesAsTheyAre(t *testing.T) {
	cases := []struct {
		code int
		body string
	}{
		{http.StatusOK, "ok"},
		{http.StatusNoContent, ""},
		{http.StatusBadRequest, "<html>bad</html>"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
			_, _ = io.WriteString(w, c.body)
		}))
		reply, err := NewSender().Send(t.Context(), srv.URL, `{}`)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if reply.Code != c.code || reply.Body != c.body || reply.OK() != (c.code < 300) {
			t.Errorf("got %d %q ok=%v, want %d %q", reply.Code, reply.Body, reply.OK(), c.code, c.body)
		}
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	reply, err := NewSender().Send(t.Context(), srv.URL, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Code != http.StatusFound || reply.OK() {
		t.Errorf("code = %d, ok = %v; want the redirect itself", reply.Code, reply.OK())
	}
}

func TestSendCutsLongReplies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxReplyBytes+100))
	}))
	defer srv.Close()
	reply, err := NewSender().Send(t.Context(), srv.URL, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Body) != maxReplyBytes {
		t.Errorf("body length = %d, want %d", len(reply.Body), maxReplyBytes)
	}
}

func TestReasonHidesTheAddress(t *testing.T) {
	// A port nothing listens on: take one, then close it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	_, err = NewSender().Send(t.Context(), "http://"+addr+"/bot123:secret/sendMessage", `{}`)
	if err == nil {
		t.Fatal("no error from a closed port")
	}
	reason, timedOut := Reason(err)
	if timedOut || reason != "connection refused" {
		t.Errorf("Reason = %q, %v; want \"connection refused\"", reason, timedOut)
	}
	if strings.Contains(reason, "secret") || strings.Contains(reason, addr) {
		t.Errorf("reason leaks the address: %q", reason)
	}
}

func TestReasonReportsTimeouts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	s := NewSender()
	s.client.Timeout = 50 * time.Millisecond
	_, err := s.Send(context.Background(), srv.URL, `{}`)
	if err == nil {
		t.Fatal("no error from a webhook that never answers")
	}
	if _, timedOut := Reason(err); !timedOut {
		t.Errorf("Reason(%v) did not report a timeout", err)
	}
}
