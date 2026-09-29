package hosts

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-protocol"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "kerge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newService(t *testing.T) (*Service, *store.DB) {
	t.Helper()
	db := openDB(t)
	return NewService(db), db
}

func enrolled(t *testing.T, s *Service) (hostID int64, auth Auth) {
	t.Helper()
	hostID, err := s.Create(t.Context(), "web-1")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.NewEnrollToken(t.Context(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	auth, err = s.Authenticate(t.Context(), protocol.EnrollAuthorization(token))
	if err != nil {
		t.Fatal(err)
	}
	return hostID, auth
}

func TestEnrollThenUseTheCredential(t *testing.T) {
	s, db := newService(t)
	hostID, auth := enrolled(t, s)

	if auth.HostID != hostID || auth.Registered == nil {
		t.Fatalf("auth = %+v", auth)
	}
	reg := auth.Registered
	if err := reg.Validate(); err != nil {
		t.Errorf("the issued credential is not valid for the protocol: %v", err)
	}
	if reg.AgentID != auth.AgentID {
		t.Errorf("agent id = %q, header would use %q", reg.AgentID, auth.AgentID)
	}
	// The agent joins the two with a dot, so neither may contain one.
	if strings.Contains(reg.AgentID, ".") || strings.Contains(reg.Secret, ".") {
		t.Errorf("credential contains a dot: %q.%q", reg.AgentID, reg.Secret)
	}

	again, err := s.Authenticate(t.Context(), protocol.AgentAuthorization(reg.AgentID+"."+reg.Secret))
	if err != nil {
		t.Fatal(err)
	}
	if again.HostID != hostID || again.Registered != nil {
		t.Errorf("second authentication = %+v", again)
	}

	// The database must not hold the secret in the clear.
	var stored string
	if err := db.Read().QueryRowContext(t.Context(),
		"SELECT secret_hash FROM hosts WHERE id = ?", hostID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, reg.Secret) {
		t.Error("the secret is stored in the clear")
	}
}

// A one-time token is exactly that.
func TestEnrollTokenIsSingleUse(t *testing.T) {
	s, _ := newService(t)
	hostID, err := s.Create(t.Context(), "web-1")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.NewEnrollToken(t.Context(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), protocol.EnrollAuthorization(token)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), protocol.EnrollAuthorization(token)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("reusing the token: err = %v, want ErrUnauthorized", err)
	}
}

func TestEnrollTokenExpires(t *testing.T) {
	s, _ := newService(t)
	hostID, err := s.Create(t.Context(), "web-1")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.NewEnrollToken(t.Context(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(TokenTTL + time.Minute) }
	if _, err := s.Authenticate(t.Context(), protocol.EnrollAuthorization(token)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expired token: err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	s, _ := newService(t)
	_, auth := enrolled(t, s)
	credential := auth.Registered.AgentID + "." + auth.Registered.Secret

	cases := map[string]string{
		"empty":            "",
		"no scheme":        "enroll:" + "whatever",
		"unknown kind":     "Bearer other:whatever",
		"no kind":          "Bearer whatever",
		"unknown token":    protocol.EnrollAuthorization("not-a-token"),
		"unknown agent":    protocol.AgentAuthorization("nosuchagent.secret"),
		"wrong secret":     protocol.AgentAuthorization(auth.AgentID + ".wrong"),
		"credential as id": protocol.AgentAuthorization(credential + "x"),
		"no separator":     protocol.AgentAuthorization(auth.AgentID),
		"empty secret":     protocol.AgentAuthorization(auth.AgentID + "."),
	}
	for name, header := range cases {
		if _, err := s.Authenticate(t.Context(), header); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: err = %v, want ErrUnauthorized", name, err)
		}
	}
}

// Resetting access clears the credential, so the old one stops working and
// a new token enrolls the same host again.
func TestResetAccess(t *testing.T) {
	s, db := newService(t)
	hostID, auth := enrolled(t, s)
	old := protocol.AgentAuthorization(auth.Registered.AgentID + "." + auth.Registered.Secret)

	if err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE hosts SET agent_id = NULL, secret_hash = NULL WHERE id = ?", hostID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), old); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the revoked credential still works: %v", err)
	}

	token, err := s.NewEnrollToken(t.Context(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Authenticate(t.Context(), protocol.EnrollAuthorization(token))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.HostID != hostID || fresh.Registered == nil {
		t.Errorf("re-enrollment = %+v", fresh)
	}
}

func TestCreateAndToken(t *testing.T) {
	s, _ := newService(t)
	if _, err := s.Create(t.Context(), "  "); err == nil {
		t.Error("an empty name was accepted")
	}
	if _, err := s.NewEnrollToken(t.Context(), 999); err == nil {
		t.Error("a token was issued for a host that does not exist")
	}
}

// The source address is recorded on every connection, but its change time
// only moves when the address itself changes.
func TestConnectedTracksTheAddress(t *testing.T) {
	s, db := newService(t)
	hostID, _ := enrolled(t, s)

	read := func() (string, int64) {
		t.Helper()
		var ip string
		var changed int64
		if err := db.Read().QueryRowContext(t.Context(),
			"SELECT coalesce(remote_ip, ''), coalesce(remote_ip_changed_at, 0) FROM hosts WHERE id = ?",
			hostID).Scan(&ip, &changed); err != nil {
			t.Fatal(err)
		}
		return ip, changed
	}

	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	if err := s.Connected(t.Context(), hostID, addr(t, "203.0.113.7")); err != nil {
		t.Fatal(err)
	}
	ip, changed := read()
	if ip != "203.0.113.7" || changed != now.Unix() {
		t.Fatalf("after the first connection: ip=%q changed=%d", ip, changed)
	}

	first := now.Unix()
	now = now.Add(time.Hour)
	if err := s.Connected(t.Context(), hostID, addr(t, "203.0.113.7")); err != nil {
		t.Fatal(err)
	}
	if ip, changed = read(); ip != "203.0.113.7" || changed != first {
		t.Errorf("the same address moved the change time: ip=%q changed=%d", ip, changed)
	}

	now = now.Add(time.Hour)
	if err := s.Connected(t.Context(), hostID, addr(t, "198.51.100.9")); err != nil {
		t.Fatal(err)
	}
	if ip, changed = read(); ip != "198.51.100.9" || changed != now.Unix() {
		t.Errorf("a new address did not move the change time: ip=%q changed=%d", ip, changed)
	}
}

func TestSetHostInfo(t *testing.T) {
	s, db := newService(t)
	hostID, _ := enrolled(t, s)
	info := &protocol.HostInfo{
		Hostname: "web-1", OS: "linux", Platform: "debian", PlatformVersion: "12",
		Kernel: "6.1.0", Arch: "x86_64", CPUModel: "Xeon", CPUCores: 4,
		AgentVersion: "0.1.0", IntervalMS: 5000,
	}
	if err := s.SetHostInfo(t.Context(), hostID, info); err != nil {
		t.Fatal(err)
	}
	var hostname, arch, version string
	var cores int
	if err := db.Read().QueryRowContext(t.Context(),
		"SELECT hostname, arch, cpu_cores, agent_version FROM hosts WHERE id = ?",
		hostID).Scan(&hostname, &arch, &cores, &version); err != nil {
		t.Fatal(err)
	}
	if hostname != "web-1" || arch != "x86_64" || cores != 4 || version != "0.1.0" {
		t.Errorf("stored %q %q %d %q", hostname, arch, cores, version)
	}
}

// The exclusion rules fall back from the host override to the panel setting
// to the built-in list.
func TestIfaceExcludeFallsBack(t *testing.T) {
	s, db := newService(t)
	hostID, _ := enrolled(t, s)

	got, err := s.IfaceExclude(t.Context(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 10 || got[0] != "lo" {
		t.Errorf("built-in list = %v", got)
	}

	if err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return store.SetSetting(ctx, tx, store.SettingNetIfaceExclude, "lo, docker*")
	}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.IfaceExclude(t.Context(), hostID); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "docker*" {
		t.Errorf("panel setting = %v", got)
	}

	if err := db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE hosts SET net_iface_exclude = ? WHERE id = ?", "eth1", hostID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.IfaceExclude(t.Context(), hostID); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "eth1" {
		t.Errorf("host override = %v", got)
	}
}

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
