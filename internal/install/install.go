// Package install builds the one-line command an operator runs on a host to
// install the agent.
//
// The agent repository, the agent version and the checksum of the install
// script are compiled into the binary: a panel release pins exactly one
// agent release, and nothing at runtime -- no environment variable, no
// setting -- can point the command somewhere else.
package install

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Set at build time with -ldflags "-X .../internal/install.AgentVersion=...".
// The defaults describe a development build, which pins no release.
var (
	// Repo is the agent repository the script is downloaded from.
	Repo = "https://github.com/kergeio/kerge-agent"
	// AgentVersion is the pinned agent release, without the leading "v".
	AgentVersion = ""
	// ScriptSHA256 is the checksum of that release's install-agent.sh.
	ScriptSHA256 = ""
)

// AgentPath is the endpoint an agent connects to.
const AgentPath = "/api/agent/ws"

const scriptName = "install-agent.sh"

// Placeholders stand in for the values a development build does not have.
// They keep the command readable as an example while making it obvious
// that it cannot be run as printed.
const (
	versionPlaceholder = "<agent version>"
	sha256Placeholder  = "<sha256 of install-agent.sh>"
)

// TokenMask replaces the enrollment token in the command shown on screen.
const TokenMask = "***"

// Pinned reports whether this build pins a real agent release. A
// development build does not, and the command it prints is an example.
func Pinned() bool { return AgentVersion != "" && ScriptSHA256 != "" }

// Command returns the install command for one host: it downloads the
// pinned install-agent.sh, verifies its checksum, and only then runs it.
func Command(server, token string) string {
	version, sum := AgentVersion, ScriptSHA256
	if version == "" {
		version = versionPlaceholder
	}
	if sum == "" {
		sum = sha256Placeholder
	}
	script := fmt.Sprintf("%s/releases/download/v%s/%s", strings.TrimSuffix(Repo, "/"), version, scriptName)
	return fmt.Sprintf(`f=$(mktemp) && curl -fsSL -o "$f" %s `+
		`&& echo "%s  $f" | sha256sum -c --quiet - `+
		`&& sudo bash "$f" --server %s --token %s; rm -f "$f"`,
		script, sum, server, token)
}

// AgentEndpoint turns the panel's public URL into the WebSocket endpoint
// the agent connects to: https becomes wss, http becomes ws.
func AgentEndpoint(publicURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil {
		return "", fmt.Errorf("install: public URL: %w", err)
	}
	if u.Host == "" {
		return "", errors.New("install: the public URL needs a host")
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("install: the public URL must be http or https, not %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + AgentPath
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String(), nil
}
