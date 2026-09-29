package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisallowed(t *testing.T) {
	tests := []struct {
		r    rune
		want bool
	}{
		{'a', false},
		{'Z', false},
		{'7', false},
		{0x00e9, false}, // Latin small e with acute
		{0x2014, false}, // em dash
		{0x00b5, false}, // micro sign
		{0x2713, false}, // check mark
		{'\U0001F916', false},
		{0x4e2d, true}, // CJK ideograph
		{0x3042, true}, // Hiragana
		{0x30a2, true}, // Katakana
		{0xd55c, true}, // Hangul
		{0x0436, true}, // Cyrillic
		{0x03bb, true}, // Greek
		{0x3002, true}, // ideographic full stop
		{0xff0c, true}, // fullwidth comma
		{0xff21, true}, // fullwidth Latin A
	}
	for _, tt := range tests {
		if got := disallowed(tt.r); got != tt.want {
			t.Errorf("disallowed(%U) = %v, want %v", tt.r, got, tt.want)
		}
	}
}

func TestScanPositions(t *testing.T) {
	got := scan([]byte("ok\n// x\u4e2d\u6587\n"))
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	if got[0] != (finding{Line: 2, Col: 5, Rune: 0x4e2d}) {
		t.Errorf("first finding = %+v", got[0])
	}
}

func TestCheckFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("clean.go", "package x // plain English\n")
	write("web/locales/zh-CN.json", "{\"k\": \"\u4e2d\u6587\"}\n")
	write("bin.dat", "\x00\u4e2d")
	write("bad.go", "package x // \u4e2d\n")
	write("web/locales/en.json", "{\"k\": \"\u4e2d\"}\n")

	var out strings.Builder
	failed, err := checkFiles([]string{"clean.go", "web/locales/zh-CN.json", "bin.dat"}, &out)
	if err != nil || failed {
		t.Fatalf("clean set: failed=%v err=%v out=%q", failed, err, out.String())
	}

	out.Reset()
	failed, err = checkFiles([]string{"clean.go", "bad.go"}, &out)
	if err != nil || !failed {
		t.Fatalf("bad set: failed=%v err=%v", failed, err)
	}
	if !strings.Contains(out.String(), "bad.go:1:14") {
		t.Errorf("output %q does not name bad.go:1:14", out.String())
	}

	out.Reset()
	failed, err = checkFiles([]string{"web/locales/en.json"}, &out)
	if err != nil || !failed {
		t.Errorf("en.json is not checked: failed=%v err=%v", failed, err)
	}
}

func TestCheckCommits(t *testing.T) {
	var out strings.Builder
	good := "0123456789abcdef\nfeat(agent): add net collector\n\nBody text.\n"
	if checkCommits(splitNUL([]byte(good+"\x00")), &out) {
		t.Fatalf("good commit flagged: %q", out.String())
	}
	bad := "\nfedcba9876543210\nfix: \u4fee\u590d\n"
	if !checkCommits(splitNUL([]byte(good+"\x00"+bad+"\x00")), &out) {
		t.Fatal("bad commit not flagged")
	}
	if !strings.Contains(out.String(), "commit fedcba987654: message line 1 col 6") {
		t.Errorf("unexpected output %q", out.String())
	}
}
