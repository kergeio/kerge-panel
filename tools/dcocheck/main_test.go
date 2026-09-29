package main

import (
	"strings"
	"testing"
)

func TestSignedOff(t *testing.T) {
	const author = "Jane Doe <jane@example.com>"
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{"signed by the author", "feat: add x\n\nBody.\n\nSigned-off-by: Jane Doe <jane@example.com>\n", true},
		{"email differs only in case", "fix: y\n\nSigned-off-by: Jane Doe <Jane@Example.com>\n", true},
		{"name differs, email matches", "fix: y\n\nSigned-off-by: J. Doe <jane@example.com>\n", true},
		{"one of several sign-offs", "fix: y\n\nSigned-off-by: Bob <bob@example.com>\nSigned-off-by: Jane Doe <jane@example.com>\n", true},
		{"indented trailer", "fix: y\n\n  Signed-off-by: Jane Doe <jane@example.com>\n", true},
		{"no sign-off", "feat: add x\n\nBody.\n", false},
		{"signed by someone else", "fix: y\n\nSigned-off-by: Bob <bob@example.com>\n", false},
		{"sign-off without an address", "fix: y\n\nSigned-off-by: Jane Doe\n", false},
		{"only a co-author line", "fix: y\n\nCo-Authored-By: Jane Doe <jane@example.com>\n", false},
		{"different spelling of the trailer", "fix: y\n\nsigned-off-by: Jane Doe <jane@example.com>\n", false},
	}
	for _, tt := range tests {
		if got := signedOff(author, tt.msg); got != tt.want {
			t.Errorf("%s: signedOff = %v, want %v", tt.name, got, tt.want)
		}
	}
	if signedOff("Jane Doe", "x\n\nSigned-off-by: Jane Doe <>\n") {
		t.Error("an author without an email address passed")
	}
}

func TestCheckCommits(t *testing.T) {
	input := "\n" + strings.Join([]string{
		"1111111111111111111111111111111111111111\nJane Doe <jane@example.com>\nfeat: a\n\nSigned-off-by: Jane Doe <jane@example.com>\n",
		"2222222222222222222222222222222222222222\nJane Doe <jane@example.com>\nfix: b\n",
		"3333333333333333333333333333333333333333\nBob <bob@example.com>\nfix: c\n\nSigned-off-by: Jane Doe <jane@example.com>\n",
	}, "\x00") + "\x00"

	var out strings.Builder
	if !checkCommits(splitNUL([]byte(input)), &out) {
		t.Fatal("unsigned commits passed")
	}
	got := out.String()
	if strings.Contains(got, "111111111111") {
		t.Errorf("the signed commit was reported:\n%s", got)
	}
	for _, want := range []string{
		"commit 222222222222: no Signed-off-by line for its author Jane Doe <jane@example.com>",
		"commit 333333333333: no Signed-off-by line for its author Bob <bob@example.com>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	out.Reset()
	if checkCommits(splitNUL([]byte(strings.SplitN(input, "\x00", 2)[0])), &out) {
		t.Errorf("a signed commit failed:\n%s", out.String())
	}
}
