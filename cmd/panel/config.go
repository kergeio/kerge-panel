package main

import (
	"fmt"
	"net/netip"

	"github.com/kergeio/kerge-panel/internal/install"
	"github.com/kergeio/kerge-panel/internal/web"
)

// config is the panel configuration read from KERGE_* environment variables.
type config struct {
	listen         string
	dataDir        string
	trustedProxies []netip.Prefix
	// agentEndpoint is the WebSocket address the install command points
	// agents at, derived from the panel's public URL. It is empty when
	// neither KERGE_PUBLIC_URL nor KERGE_DOMAIN is set, which leaves the
	// panel to use the address a request arrived on.
	agentEndpoint string
}

func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		listen:  getenv("KERGE_LISTEN"),
		dataDir: getenv("KERGE_DATA_DIR"),
	}
	if c.listen == "" {
		c.listen = ":3000"
	}
	if c.dataDir == "" {
		c.dataDir = "/data"
	}
	proxies := getenv("KERGE_TRUSTED_PROXIES")
	if proxies == "" {
		proxies = web.DefaultTrustedProxies
	}
	var err error
	if c.trustedProxies, err = web.ParseTrustedProxies(proxies); err != nil {
		return config{}, fmt.Errorf("KERGE_TRUSTED_PROXIES: %w", err)
	}

	public, name := getenv("KERGE_PUBLIC_URL"), "KERGE_PUBLIC_URL"
	if public == "" {
		if domain := getenv("KERGE_DOMAIN"); domain != "" {
			public, name = "https://"+domain, "KERGE_DOMAIN"
		}
	}
	if public != "" {
		if c.agentEndpoint, err = install.AgentEndpoint(public); err != nil {
			return config{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	return c, nil
}
