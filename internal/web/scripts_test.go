package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	webfiles "github.com/kergeio/kerge-panel/web"
)

// Every script keeps its own translation helper: none is shared between
// files, so a script that looks text up must define t itself, or it fails
// at run time.
func TestScriptsDefineTheTranslationHelper(t *testing.T) {
	calls := regexp.MustCompile(`\bt\("`)
	names, err := fs.Glob(webfiles.Files, "static/js/*.js")
	if err != nil || len(names) == 0 {
		t.Fatalf("no scripts found: %v", err)
	}
	for _, name := range names {
		src, err := fs.ReadFile(webfiles.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		if calls.Match(src) && !strings.Contains(string(src), "function t(key)") {
			t.Errorf("%s looks up text with t() but does not define it", name)
		}
	}
}
