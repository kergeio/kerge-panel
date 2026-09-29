// Command dcocheck checks that every commit carries a Developer Certificate
// of Origin sign-off by its author.
//
// Usage:
//
//	git log -z --no-merges --format='%H%n%an <%ae>%n%B' | dcocheck
//
// It reads NUL-separated records: the commit hash, the author as
// "Name <email>", then the commit message. A commit passes when its message
// has a "Signed-off-by:" line with the author's email address (compared
// without regard to case), which is the line `git commit -s` adds.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: git log -z --no-merges --format='%H%n%an <%ae>%n%B' | dcocheck")
		os.Exit(2)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dcocheck: read stdin: %v\n", err)
		os.Exit(2)
	}
	records := splitNUL(input)
	if len(records) == 0 {
		fmt.Fprintln(os.Stderr, "dcocheck: no commits on stdin")
		os.Exit(2)
	}
	if checkCommits(records, os.Stdout) {
		fmt.Fprintln(os.Stdout, "dcocheck: every commit needs a Signed-off-by line from its author (git commit -s); see CONTRIBUTING.md")
		os.Exit(1)
	}
}

func splitNUL(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// checkCommits reports every commit without a sign-off by its author and
// returns whether there was one.
func checkCommits(records []string, w io.Writer) bool {
	failed := false
	for _, rec := range records {
		rec = strings.TrimLeft(rec, "\n")
		hash, rest, _ := strings.Cut(rec, "\n")
		author, msg, _ := strings.Cut(rest, "\n")
		if len(hash) > 12 {
			hash = hash[:12]
		}
		if !signedOff(author, msg) {
			failed = true
			fmt.Fprintf(w, "commit %s: no Signed-off-by line for its author %s\n", hash, author)
		}
	}
	return failed
}

// signedOff reports whether msg has a Signed-off-by line with the email
// address of author.
func signedOff(author, msg string) bool {
	want := email(author)
	if want == "" {
		return false
	}
	for _, line := range strings.Split(msg, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "Signed-off-by:")
		if ok && strings.EqualFold(email(v), want) {
			return true
		}
	}
	return false
}

// email returns the address between the last pair of angle brackets in s,
// or "" when there is none.
func email(s string) string {
	i := strings.LastIndexByte(s, '<')
	j := strings.LastIndexByte(s, '>')
	if i < 0 || j < i {
		return ""
	}
	return strings.TrimSpace(s[i+1 : j])
}
