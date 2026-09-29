package main

import (
	"bytes"
	"context"
	"database/sql"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kergeio/kerge-panel/internal/store"
)

func TestHealthURL(t *testing.T) {
	cases := map[string]string{
		":3000":          "http://127.0.0.1:3000/healthz",
		"0.0.0.0:3000":   "http://127.0.0.1:3000/healthz",
		"[::]:3000":      "http://127.0.0.1:3000/healthz",
		"127.0.0.1:8080": "http://127.0.0.1:8080/healthz",
		"[::1]:3000":     "http://[::1]:3000/healthz",
	}
	for in, want := range cases {
		if got, err := healthURL(in); err != nil || got != want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := healthURL("3000"); err == nil {
		t.Error("healthURL accepted an address without a port")
	}
}

func TestHealthcheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	status := http.StatusOK
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	getenv := env(map[string]string{"KERGE_LISTEN": ln.Addr().String()})
	if err := healthcheck(getenv); err != nil {
		t.Errorf("healthy: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := healthcheck(getenv); err == nil {
		t.Error("unhealthy panel reported healthy")
	}
	srv.Close()
	if err := healthcheck(getenv); err == nil {
		t.Error("stopped panel reported healthy")
	}
}

func TestConfirm(t *testing.T) {
	for in, want := range map[string]bool{
		"y\n": true, "Y\n": true, "yes\n": true, " YES \n": true, "y": true,
		"\n": false, "n\n": false, "no\n": false, "yep\n": false, "": false,
	} {
		var out bytes.Buffer
		got, err := confirm(strings.NewReader(in), &out)
		if err != nil || got != want {
			t.Errorf("confirm(%q) = %v, %v; want %v", in, got, err, want)
		}
		if !strings.Contains(out.String(), "[y/N]") {
			t.Errorf("prompt missing: %q", out.String())
		}
	}
}

// seedAdmin creates a database in dir with one user and one host.
func seedAdmin(t *testing.T, dir string) {
	t.Helper()
	db, err := store.Open(t.Context(), dir+"/kerge.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Write(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO users (username, password_hash, created_at, updated_at) VALUES ('admin', 'x', 0, 0)"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO hosts (name, created_at) VALUES ('web-1', 0)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func counts(t *testing.T, dir string) (users, hosts int) {
	t.Helper()
	db, err := store.Open(t.Context(), dir+"/kerge.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.Read().QueryRow("SELECT count(*) FROM users").Scan(&users)
	_ = db.Read().QueryRow("SELECT count(*) FROM hosts").Scan(&hosts)
	return users, hosts
}

func TestResetAdminWithoutTerminalNeedsYes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KERGE_DATA_DIR", dir)
	seedAdmin(t, dir)

	// A pipe is not a terminal, like "docker exec" without -it.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	defer r.Close()
	var out bytes.Buffer
	err = resetAdmin(nil, r, &out)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("without --yes: err = %v", err)
	}
	if users, _ := counts(t, dir); users != 1 {
		t.Fatal("admin deleted without confirmation")
	}

	if err := resetAdmin([]string{"--force"}, r, &out); err == nil {
		t.Error("unknown flag accepted")
	}

	if err := resetAdmin([]string{"--yes"}, r, &out); err != nil {
		t.Fatal(err)
	}
	users, hosts := counts(t, dir)
	if users != 0 || hosts != 1 {
		t.Errorf("after reset: users=%d hosts=%d, want 0 and 1", users, hosts)
	}
	if !strings.Contains(out.String(), "Restart the panel") {
		t.Errorf("output %q", out.String())
	}
}

func TestIsTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Error("/dev/null reported as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("pipe reported as a terminal")
	}
}

func TestCheckDataDir(t *testing.T) {
	dir := t.TempDir()
	if err := checkDataDir(dir); err != nil {
		t.Fatalf("writable directory: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("probe left %d files behind", len(entries))
	}

	err := checkDataDir(dir + "/missing")
	if err == nil || !strings.Contains(err.Error(), "chown -R 65532:65532") {
		t.Errorf("missing directory: %v", err)
	}
	// Every command opens the database through openStore.
	_, err = openStore(context.Background(), config{dataDir: dir + "/missing"})
	if err == nil || !strings.Contains(err.Error(), "chown -R 65532:65532") {
		t.Errorf("openStore on a missing directory: %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	err = checkDataDir(dir)
	if err == nil || !strings.Contains(err.Error(), "chown -R 65532:65532") {
		t.Errorf("read-only directory: %v", err)
	}
}
