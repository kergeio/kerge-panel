// Command langcheck enforces the English-only rule for repository content.
//
// Usage:
//
//	git ls-files -z | langcheck files
//	git log -z --format='%H%n%B' | langcheck commits
//
// In "files" mode it reads NUL-separated paths from stdin and scans each file,
// skipping non-English translation files (web/locales/ except en.json) and
// files that look binary. In "commits" mode it reads NUL-separated records
// whose first line is the commit hash and whose remaining lines are the
// commit message.
//
// A rune is rejected when it is a letter outside the Latin and Common scripts
// (CJK, Cyrillic, Greek, and so on), or when it falls in the CJK symbols and
// punctuation or halfwidth/fullwidth forms blocks.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// localesDir is the only directory allowed to contain non-English text;
// its English file is still checked.
const (
	localesDir  = "web/locales/"
	englishFile = localesDir + "en.json"
)

func exempt(p string) bool {
	return strings.HasPrefix(p, localesDir) && p != englishFile
}

// finding describes one disallowed rune.
type finding struct {
	Line int
	Col  int
	Rune rune
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: langcheck files|commits (input on stdin, NUL-separated)")
		os.Exit(2)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "langcheck: read stdin: %v\n", err)
		os.Exit(2)
	}

	var failed bool
	switch os.Args[1] {
	case "files":
		failed, err = checkFiles(splitNUL(input), os.Stdout)
	case "commits":
		failed = checkCommits(splitNUL(input), os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "langcheck: unknown mode %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "langcheck: %v\n", err)
		os.Exit(2)
	}
	if failed {
		fmt.Fprintln(os.Stdout, "langcheck: non-English text found; only non-English files in web/locales/ may contain other languages")
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

func checkFiles(paths []string, w io.Writer) (bool, error) {
	failed := false
	for _, p := range paths {
		if exempt(p) {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return failed, err
		}
		if isBinary(data) {
			continue
		}
		for _, f := range scan(data) {
			failed = true
			fmt.Fprintf(w, "%s:%d:%d: disallowed character %U\n", p, f.Line, f.Col, f.Rune)
		}
	}
	return failed, nil
}

func checkCommits(records []string, w io.Writer) bool {
	failed := false
	for _, rec := range records {
		rec = strings.TrimLeft(rec, "\n")
		hash, msg, _ := strings.Cut(rec, "\n")
		if len(hash) > 12 {
			hash = hash[:12]
		}
		for _, f := range scan([]byte(msg)) {
			failed = true
			fmt.Fprintf(w, "commit %s: message line %d col %d: disallowed character %U\n", hash, f.Line, f.Col, f.Rune)
		}
	}
	return failed
}

// isBinary reports whether data should be skipped: it contains a NUL byte or
// is not valid UTF-8.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}

// scan returns every disallowed rune in data with its 1-based line and column.
func scan(data []byte) []finding {
	var out []finding
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	line := 0
	for sc.Scan() {
		line++
		col := 0
		for _, r := range sc.Text() {
			col++
			if disallowed(r) {
				out = append(out, finding{Line: line, Col: col, Rune: r})
			}
		}
	}
	return out
}

// cjkPunct covers CJK symbols and punctuation (U+3000-U+303F) and
// halfwidth/fullwidth forms (U+FF00-U+FFEF).
var cjkPunct = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x3000, Hi: 0x303F, Stride: 1},
		{Lo: 0xFF00, Hi: 0xFFEF, Stride: 1},
	},
}

func disallowed(r rune) bool {
	if unicode.Is(cjkPunct, r) {
		return true
	}
	return unicode.IsLetter(r) && !unicode.In(r, unicode.Latin, unicode.Common)
}
