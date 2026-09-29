package i18n

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	_ "time/tzdata"
)

func load(t *testing.T, files map[string]string) (*Catalog, *bytes.Buffer, error) {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	var logs bytes.Buffer
	c, err := Load(fsys, slog.New(slog.NewTextHandler(&logs, nil)))
	return c, &logs, err
}

func TestLoadRejectsBadFiles(t *testing.T) {
	cases := map[string]map[string]string{
		"missing en":      {"de.json": `{"a.b": "x"}`},
		"not object":      {"en.json": `["a"]`},
		"nested object":   {"en.json": `{"a": {"b": "x"}}`},
		"number value":    {"en.json": `{"a.b": 1}`},
		"duplicate key":   {"en.json": `{"a.b": "x", "a.b": "y"}`},
		"undotted key":    {"en.json": `{"title": "x"}`},
		"upper key":       {"en.json": `{"Page.title": "x"}`},
		"trailing data":   {"en.json": `{"a.b": "x"} {}`},
		"bad lang code":   {"en.json": `{"a.b": "x"}`, "english.json": `{"a.b": "x"}`},
		"malformed json":  {"en.json": `{"a.b": "x",}`},
		"bad other lang":  {"en.json": `{"a.b": "x"}`, "de.json": `{"a.b": 2}`},
		"empty key value": {"en.json": `{"": "x"}`},
	}
	for name, files := range cases {
		if _, _, err := load(t, files); err == nil {
			t.Errorf("%s: Load succeeded", name)
		}
	}
}

func TestTranslateAndFallback(t *testing.T) {
	c, logs, err := load(t, map[string]string{
		"en.json":    `{"a.one": "One", "a.two": "Two", "js.hint": "Hint"}`,
		"zh-CN.json": `{"a.one": "Yi"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Languages(); strings.Join(got, ",") != "en,zh-CN" {
		t.Errorf("Languages = %v", got)
	}

	zh := c.Translator("zh-CN")
	if got := zh.T("a.one"); got != "Yi" {
		t.Errorf("T(a.one) = %q", got)
	}
	if got := zh.T("a.two"); got != "Two" {
		t.Errorf("fallback T(a.two) = %q", got)
	}
	if got := zh.T("a.none"); got != "a.none" {
		t.Errorf("missing key = %q, want key itself", got)
	}
	zh.T("a.none") // a second lookup must not log again
	// Logged once for zh-CN and once for en.
	if n := strings.Count(logs.String(), "key=a.none"); n != 2 {
		t.Errorf("missing-key log lines = %d, want 2:\n%s", n, logs.String())
	}

	if got := c.Translator("xx").Lang(); got != Default {
		t.Errorf("unknown language resolved to %q", got)
	}
	if got := zh.WithPrefix("js."); len(got) != 1 || got["js.hint"] != "Hint" {
		t.Errorf("WithPrefix = %v", got)
	}
}

func TestBytes(t *testing.T) {
	tr := mustEN(t)
	cases := map[uint64]string{
		0:                    "0 B",
		1023:                 "1023 B",
		1024:                 "1.0 KiB",
		1536:                 "1.5 KiB",
		10 * 1024 * 1024:     "10.0 MiB",
		3 << 30:              "3.0 GiB",
		1<<40 + 1<<39:        "1.5 TiB",
		^uint64(0):           "16.0 EiB",
		1024*1024 - 1:        "1.0 MiB",
		2*1024*1024*1024 - 1: "2.0 GiB",
	}
	for n, want := range cases {
		if got := tr.Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestByteRate(t *testing.T) {
	tr := mustEN(t)
	cases := map[float64]string{
		-5:              "0 B/s",
		0:               "0 B/s",
		1023:            "1023 B/s",
		1024:            "1.0 K/s",
		1536:            "1.5 K/s",
		1024*1024 - 1:   "1.0 M/s",
		12.5e6:          "11.9 M/s", // a 100 Mbps port at full speed
		3 << 30:         "3.0 G/s",
		1 << 50:         "1024.0 T/s",
		1024*1024 + 512: "1.0 M/s",
	}
	for in, want := range cases {
		if got := tr.ByteRate(in); got != want {
			t.Errorf("ByteRate(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestDates(t *testing.T) {
	tr := mustEN(t)
	tm := time.Date(2026, 9, 19, 23, 30, 0, 0, time.UTC)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.DateTime(tm, time.UTC); got != "2026-09-19 23:30" {
		t.Errorf("DateTime UTC = %q", got)
	}
	if got := tr.DateTime(tm, tokyo); got != "2026-09-20 08:30" {
		t.Errorf("DateTime Tokyo = %q", got)
	}
	if got := tr.Date(tm, tokyo); got != "2026-09-20" {
		t.Errorf("Date Tokyo = %q", got)
	}
	if got := tr.MonthDay(tm, tokyo); got != "09-20" {
		t.Errorf("MonthDay Tokyo = %q", got)
	}
	if got := tr.Clock(tm.Add(7*time.Second), tokyo); got != "08:30:07" {
		t.Errorf("Clock Tokyo = %q", got)
	}
	if got := tr.Percent(42.46); got != "42.5%" {
		t.Errorf("Percent = %q", got)
	}
}

func mustEN(t *testing.T) *Translator {
	t.Helper()
	c, _, err := load(t, map[string]string{"en.json": `{"a.b": "x"}`})
	if err != nil {
		t.Fatal(err)
	}
	return c.Translator("en")
}
