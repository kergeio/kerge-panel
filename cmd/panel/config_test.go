package main

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.listen != ":3000" || c.dataDir != "/data" {
		t.Errorf("defaults: listen=%q dataDir=%q", c.listen, c.dataDir)
	}
	if len(c.trustedProxies) != 1 || c.trustedProxies[0].String() != "172.30.0.2/32" {
		t.Errorf("default trusted proxies = %v", c.trustedProxies)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	c, err := loadConfig(env(map[string]string{
		"KERGE_LISTEN":          "127.0.0.1:8080",
		"KERGE_DATA_DIR":        "/srv/kerge",
		"KERGE_TRUSTED_PROXIES": "172.17.0.1, 10.0.0.0/8",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.listen != "127.0.0.1:8080" || c.dataDir != "/srv/kerge" || len(c.trustedProxies) != 2 {
		t.Errorf("got %+v", c)
	}
	if _, err := loadConfig(env(map[string]string{"KERGE_TRUSTED_PROXIES": "bogus"})); err == nil {
		t.Error("invalid KERGE_TRUSTED_PROXIES accepted")
	}
}

// The install command needs the panel's public address, which comes from
// KERGE_PUBLIC_URL or, failing that, KERGE_DOMAIN.
func TestLoadConfigAgentEndpoint(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"neither is set", nil, ""},
		{"domain only", map[string]string{"KERGE_DOMAIN": "panel.example.com"},
			"wss://panel.example.com/api/agent/ws"},
		{"public url wins", map[string]string{
			"KERGE_DOMAIN":     "panel.example.com",
			"KERGE_PUBLIC_URL": "https://other.example.com",
		}, "wss://other.example.com/api/agent/ws"},
		{"plain http stays ws", map[string]string{"KERGE_PUBLIC_URL": "http://localhost:3000"},
			"ws://localhost:3000/api/agent/ws"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := loadConfig(env(c.vars))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.agentEndpoint != c.want {
				t.Errorf("agentEndpoint = %q, want %q", cfg.agentEndpoint, c.want)
			}
		})
	}
}

// A public URL the panel cannot turn into an endpoint stops it at startup
// rather than producing install commands that point nowhere.
func TestLoadConfigRejectsABadPublicURL(t *testing.T) {
	for _, value := range []string{"panel.example.com", "ftp://panel.example.com"} {
		if _, err := loadConfig(env(map[string]string{"KERGE_PUBLIC_URL": value})); err == nil {
			t.Errorf("KERGE_PUBLIC_URL=%q was accepted", value)
		}
	}
}
