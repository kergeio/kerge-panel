package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// DefaultTrustedProxies is the fixed Caddy address in the bundled compose
// network.
const DefaultTrustedProxies = "172.30.0.2/32"

// warnInterval limits how often each kind of forwarding warning is logged.
const warnInterval = time.Minute

// ParseTrustedProxies parses a comma-separated list of IP addresses and
// CIDR prefixes. Empty entries are ignored.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, fmt.Errorf("trusted proxies: %q: %w", f, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("trusted proxies: %q: %w", f, err)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// ClientIPResolver determines the client address of a request. It honors
// X-Forwarded-For only when the connection comes from a trusted proxy.
type ClientIPResolver struct {
	trusted []netip.Prefix
	logger  *slog.Logger

	mu             sync.Mutex
	lastUntrusted  time.Time // last warning about X-Forwarded-For from an untrusted peer
	lastCloudflare time.Time // last warning about a mismatched CF-Connecting-IP
	now            func() time.Time
}

// NewClientIPResolver returns a resolver trusting the given proxies.
func NewClientIPResolver(trusted []netip.Prefix, logger *slog.Logger) *ClientIPResolver {
	return &ClientIPResolver{trusted: trusted, logger: logger, now: time.Now}
}

func (c *ClientIPResolver) isTrusted(a netip.Addr) bool {
	for _, p := range c.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the client address. Starting from the connection's peer,
// it walks X-Forwarded-For from right to left while the current hop is a
// trusted proxy, and returns the first untrusted hop. A malformed entry stops
// the walk at the last hop that could be verified. The result is the zero
// Addr only if RemoteAddr itself cannot be parsed.
//
// CF-Connecting-IP never decides the result. When it is present and names a
// different address, the proxy in front of the panel did not restore the
// visitor's address from Cloudflare, and a warning is logged.
func (c *ClientIPResolver) ClientIP(r *http.Request) netip.Addr {
	client := c.resolve(r)
	if v := r.Header.Get("CF-Connecting-IP"); v != "" && client.IsValid() {
		if cf := parseHop(v); cf.IsValid() && cf != client {
			c.warnCloudflare(client, cf)
		}
	}
	return client
}

func (c *ClientIPResolver) resolve(r *http.Request) netip.Addr {
	peer := parseHop(r.RemoteAddr)
	if !peer.IsValid() {
		return netip.Addr{}
	}
	values := r.Header.Values("X-Forwarded-For")
	if !c.isTrusted(peer) {
		if len(values) > 0 {
			c.warnUntrusted(peer)
		}
		return peer
	}

	var hops []string
	for _, v := range values {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		if !c.isTrusted(client) {
			return client
		}
		next := parseHop(hops[i])
		if !next.IsValid() {
			return client
		}
		client = next
	}
	return client
}

func (c *ClientIPResolver) warnUntrusted(peer netip.Addr) {
	if c.due(&c.lastUntrusted) {
		c.logger.Warn("ignoring X-Forwarded-For from untrusted peer; check KERGE_TRUSTED_PROXIES", "peer", peer.String())
	}
}

func (c *ClientIPResolver) warnCloudflare(client, cf netip.Addr) {
	if c.due(&c.lastCloudflare) {
		c.logger.Warn("CF-Connecting-IP differs from the resolved client address; "+
			"if the domain is proxied by Cloudflare, install in Cloudflare mode, "+
			"or re-run install.sh to refresh the Cloudflare address ranges",
			"client", client.String(), "cf_connecting_ip", cf.String())
	}
}

// due reports whether a warning last logged at *last may be logged again,
// and if so records the current time. It is false without a logger.
func (c *ClientIPResolver) due(last *time.Time) bool {
	if c.logger == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if now.Sub(*last) < warnInterval {
		return false
	}
	*last = now
	return true
}

// parseHop parses an address that may carry a port ("1.2.3.4:5",
// "[::1]:5") or not. Zones are rejected.
func parseHop(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return normalize(ap.Addr())
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return normalize(a)
	}
	return netip.Addr{}
}

func normalize(a netip.Addr) netip.Addr {
	if a.Zone() != "" {
		return netip.Addr{}
	}
	return a.Unmap()
}
