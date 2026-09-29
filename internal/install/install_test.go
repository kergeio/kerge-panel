package install

import (
	"strings"
	"testing"
)

func TestCommandPinned(t *testing.T) {
	restore(t, "0.3.1", "abc123")

	got := Command("wss://panel.example.com/api/agent/ws", "TOKEN")
	for _, want := range []string{
		"https://github.com/kergeio/kerge-agent/releases/download/v0.3.1/install-agent.sh",
		`echo "abc123  $f" | sha256sum -c --quiet -`,
		"--server wss://panel.example.com/api/agent/ws --token TOKEN",
		`rm -f "$f"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command is missing %q:\n%s", want, got)
		}
	}
	if !Pinned() {
		t.Error("Pinned() = false for a build with a version and a checksum")
	}
}

// A development build has no release to point at. The command still shows
// the shape of the real one, with placeholders where the pinned values go.
func TestCommandUnpinned(t *testing.T) {
	restore(t, "", "")

	if Pinned() {
		t.Error("Pinned() = true for a build without a pinned release")
	}
	got := Command("wss://panel.example.com/api/agent/ws", TokenMask)
	for _, want := range []string{versionPlaceholder, sha256Placeholder, "--token " + TokenMask} {
		if !strings.Contains(got, want) {
			t.Errorf("command is missing %q:\n%s", want, got)
		}
	}
}

// Only one of the two pinned values is not a pinned release: a command
// with a real version and no checksum would download without verifying.
func TestPinnedNeedsBothValues(t *testing.T) {
	restore(t, "0.3.1", "")
	if Pinned() {
		t.Error("Pinned() = true without a checksum")
	}
	restore(t, "", "abc123")
	if Pinned() {
		t.Error("Pinned() = true without a version")
	}
}

func TestAgentEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://panel.example.com":       "wss://panel.example.com/api/agent/ws",
		"https://panel.example.com/":      "wss://panel.example.com/api/agent/ws",
		"http://localhost:3000":           "ws://localhost:3000/api/agent/ws",
		"  https://panel.example.com  ":   "wss://panel.example.com/api/agent/ws",
		"https://panel.example.com/kerge": "wss://panel.example.com/kerge/api/agent/ws",
		// Credentials, queries and fragments have no place in the
		// endpoint an agent dials.
		"https://user:pw@panel.example.com?a=b#c": "wss://panel.example.com/api/agent/ws",
	}
	for in, want := range cases {
		got, err := AgentEndpoint(in)
		if err != nil {
			t.Errorf("AgentEndpoint(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("AgentEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentEndpointRejects(t *testing.T) {
	for _, in := range []string{"", "panel.example.com", "ftp://panel.example.com", "https://", "://"} {
		if got, err := AgentEndpoint(in); err == nil {
			t.Errorf("AgentEndpoint(%q) = %q, want an error", in, got)
		}
	}
}

// restore sets the build-time values for one test and puts them back
// afterwards.
func restore(t *testing.T, version, sum string) {
	t.Helper()
	oldVersion, oldSum := AgentVersion, ScriptSHA256
	t.Cleanup(func() { AgentVersion, ScriptSHA256 = oldVersion, oldSum })
	AgentVersion, ScriptSHA256 = version, sum
}
