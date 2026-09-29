package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/auth"
	"github.com/kergeio/kerge-panel/internal/hosts"
)

// healthcheck requests the local /healthz endpoint and fails unless it
// answers 200. It is the container's HEALTHCHECK.
func healthcheck(getenv func(string) string) error {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	url, err := healthURL(cfg.listen)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: status %d", resp.StatusCode)
	}
	return nil
}

// healthURL builds the loopback URL of /healthz for a listen address such
// as ":3000", "0.0.0.0:3000" or "127.0.0.1:3000".
func healthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("KERGE_LISTEN %q: %w", listen, err)
	}
	if a, err := netip.ParseAddr(host); host == "" || (err == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// resetAdmin deletes the admin account and all sessions after confirmation:
// an interactive y/N prompt when stdin is a terminal, or --yes otherwise.
func resetAdmin(args []string, stdin *os.File, stdout io.Writer) error {
	yes := false
	for _, a := range args {
		if a != "--yes" {
			return fmt.Errorf("reset-admin: unknown argument %q", a)
		}
		yes = true
	}
	if !yes {
		if !isTerminal(stdin) {
			return errors.New("reset-admin: no terminal to confirm on; run with 'docker exec -it' or pass --yes")
		}
		ok, err := confirm(stdin, stdout)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(stdout, "Cancelled. Nothing was changed.")
			return nil
		}
	}

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := auth.ResetAdmin(ctx, db); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "The admin account and all sessions were deleted.")
	fmt.Fprintln(stdout, "Restart the panel; it will print a new setup code and open the setup wizard.")
	return nil
}

// confirm explains the reset and reads a y/N answer.
func confirm(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprintln(out, "This deletes the admin account and signs out all sessions.")
	fmt.Fprintln(out, "Hosts, metrics and settings are kept. After a restart the panel")
	fmt.Fprintln(out, "prints a new setup code and opens the setup wizard.")
	fmt.Fprint(out, "Reset the admin account? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// addHost creates a host and prints a one-time enrollment token, so that a
// development setup or a test can enroll an agent without the web pages.
func addHost(name string, stdout io.Writer) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	svc := hosts.NewService(db)
	id, err := svc.Create(ctx, name)
	if err != nil {
		return err
	}
	token, err := svc.NewEnrollToken(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Host id: %d\nEnrollment token: %s\nThe token is valid for %s and can be used once.\n",
		id, token, hosts.TokenTTL)
	return nil
}
