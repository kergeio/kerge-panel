package web

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies(" 172.30.0.2/32, 10.0.0.1 ,,::ffff:192.0.2.1, 2001:db8::/32, 192.168.1.7/24")
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	want := "172.30.0.2/32 10.0.0.1/32 192.0.2.1/32 2001:db8::/32 192.168.1.0/24"
	if strings.Join(s, " ") != want {
		t.Errorf("got %v, want %s", s, want)
	}
	for _, bad := range []string{"not-an-ip", "10.0.0.0/33", "1.2.3.4:80"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Errorf("ParseTrustedProxies(%q) succeeded", bad)
		}
	}
	if got, err := ParseTrustedProxies(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
}

func TestClientIP(t *testing.T) {
	trusted, err := ParseTrustedProxies("172.30.0.2/32, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	r := NewClientIPResolver(trusted, nil)
	cases := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"direct, no header", "203.0.113.5:4000", nil, "203.0.113.5"},
		{"untrusted peer ignores header", "203.0.113.5:4000", []string{"1.1.1.1"}, "203.0.113.5"},
		{"trusted proxy", "172.30.0.2:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"spoofed left entries ignored", "172.30.0.2:5000", []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7"},
		{"chain of trusted proxies", "172.30.0.2:5000", []string{"6.6.6.6, 198.51.100.7, 10.1.2.3"}, "198.51.100.7"},
		{"multiple header lines", "172.30.0.2:5000", []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7"},
		{"all trusted", "172.30.0.2:5000", []string{"10.0.0.9, 10.0.0.8"}, "10.0.0.9"},
		{"trusted proxy, no header", "172.30.0.2:5000", nil, "172.30.0.2"},
		{"malformed hop stops walk", "172.30.0.2:5000", []string{"198.51.100.7, garbage"}, "172.30.0.2"},
		{"malformed after trusted hop", "172.30.0.2:5000", []string{"garbage, 10.0.0.9"}, "10.0.0.9"},
		{"ipv6 client", "172.30.0.2:5000", []string{"2001:db8::1"}, "2001:db8::1"},
		{"ipv6 with port", "172.30.0.2:5000", []string{"[2001:db8::1]:443"}, "2001:db8::1"},
		{"ipv4 with port", "172.30.0.2:5000", []string{"198.51.100.7:1234"}, "198.51.100.7"},
		{"mapped ipv4 peer", "[::ffff:172.30.0.2]:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"zone rejected", "172.30.0.2:5000", []string{"fe80::1%eth0"}, "172.30.0.2"},
		{"empty entry", "172.30.0.2:5000", []string{"198.51.100.7, "}, "172.30.0.2"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		for _, v := range c.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		if got := r.ClientIP(req).String(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestUntrustedForwardedHeaderWarningIsRateLimited(t *testing.T) {
	var logs bytes.Buffer
	r := NewClientIPResolver(nil, slog.New(slog.NewTextHandler(&logs, nil)))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.5:4000"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	r.ClientIP(req)
	r.ClientIP(req)
	now = now.Add(30 * time.Second)
	r.ClientIP(req)
	if n := strings.Count(logs.String(), "untrusted peer"); n != 1 {
		t.Errorf("warnings within a minute = %d, want 1", n)
	}
	now = now.Add(31 * time.Second)
	r.ClientIP(req)
	if n := strings.Count(logs.String(), "untrusted peer"); n != 2 {
		t.Errorf("warnings after a minute = %d, want 2", n)
	}
}

func TestCloudflareMismatchWarning(t *testing.T) {
	trusted, err := ParseTrustedProxies("172.30.0.2/32")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	r := NewClientIPResolver(trusted, slog.New(slog.NewTextHandler(&logs, nil)))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	const msg = "CF-Connecting-IP differs"

	request := func(xff, cf string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "172.30.0.2:5000"
		req.Header.Set("X-Forwarded-For", xff)
		if cf != "" {
			req.Header.Set("CF-Connecting-IP", cf)
		}
		return r.ClientIP(req).String()
	}

	// Cloudflare mode done right: the proxy forwards the visitor's address.
	if got := request("198.51.100.7", "198.51.100.7"); got != "198.51.100.7" {
		t.Errorf("matching header: client %s", got)
	}
	if got := request("198.51.100.7", "2001:db8::1%eth0"); got != "198.51.100.7" {
		t.Errorf("malformed header: client %s", got)
	}
	if strings.Contains(logs.String(), msg) {
		t.Fatalf("warned without a mismatch:\n%s", logs.String())
	}

	// Proxied by Cloudflare without Cloudflare mode: the proxy forwards a
	// Cloudflare edge address. The header is reported, never trusted.
	if got := request("162.158.1.1", "198.51.100.7"); got != "162.158.1.1" {
		t.Errorf("mismatch changed the client to %s", got)
	}
	request("162.158.1.1", "198.51.100.7")
	if n := strings.Count(logs.String(), msg); n != 1 {
		t.Errorf("warnings within a minute = %d, want 1", n)
	}
	if !strings.Contains(logs.String(), "cf_connecting_ip=198.51.100.7") {
		t.Errorf("warning lacks the header value:\n%s", logs.String())
	}

	// The two warnings are limited separately.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.5:4000"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")
	r.ClientIP(req)
	if n := strings.Count(logs.String(), "untrusted peer"); n != 1 {
		t.Errorf("untrusted-peer warnings = %d, want 1", n)
	}

	now = now.Add(61 * time.Second)
	request("162.158.1.1", "198.51.100.7")
	if n := strings.Count(logs.String(), msg); n != 2 {
		t.Errorf("warnings after a minute = %d, want 2", n)
	}
}
