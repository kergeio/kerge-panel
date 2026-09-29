package hosts

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/kergeio/kerge-protocol"
)

func TestListIsOrderedAndCarriesTheAgentFields(t *testing.T) {
	s, _ := newService(t)
	ctx := t.Context()

	// Created out of order on purpose: sort decides, then the name.
	for _, name := range []string{"zeta", "alpha"} {
		if _, err := s.Create(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.Create(ctx, "sorted first")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, last, "sorted first", -5, "note"); err != nil {
		t.Fatal(err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range list {
		names = append(names, r.Name)
		if !r.Pending() {
			t.Errorf("%q is not pending although nothing enrolled", r.Name)
		}
		if r.CreatedAt.IsZero() {
			t.Errorf("%q has no created_at", r.Name)
		}
	}
	if got := strings.Join(names, ","); got != "sorted first,alpha,zeta" {
		t.Errorf("order = %s", got)
	}

	// Once an agent enrolls and reports, the record carries its fields.
	hostID, auth := enrolled(t, s)
	if err := s.SetHostInfo(ctx, hostID, &protocol.HostInfo{
		Hostname: "web-1.internal", OS: "linux", Platform: "debian", Arch: "amd64",
		CPUModel: "Xeon", CPUCores: 4, AgentVersion: "0.1.0", IntervalMS: 5000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Connected(ctx, hostID, addr(t, "192.0.2.10")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pending() || r.AgentID != auth.AgentID {
		t.Errorf("record = %+v, want an enrolled host", r)
	}
	if r.Hostname != "web-1.internal" || r.OS != "linux" || r.CPUCores != 4 || r.AgentVersion != "0.1.0" {
		t.Errorf("agent fields = %+v", r)
	}
	if r.RemoteIP != "192.0.2.10" || r.RemoteIPChangedAt.IsZero() || r.LastSeenAt.IsZero() {
		t.Errorf("connection fields = %+v", r)
	}
}

func TestGetUpdateAndDeleteReportMissingHosts(t *testing.T) {
	s, _ := newService(t)
	ctx := t.Context()
	if _, err := s.Get(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: %v", err)
	}
	if err := s.Update(ctx, 404, "name", 0, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update: %v", err)
	}
	if _, err := s.Delete(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete: %v", err)
	}
	if _, _, err := s.ResetAccess(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("ResetAccess: %v", err)
	}
}

func TestUpdateRejectsBadInput(t *testing.T) {
	s, _ := newService(t)
	id, err := s.Create(t.Context(), "web-1")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		name string
		sort int64
		note string
	}{
		"empty name":        {"   ", 0, ""},
		"name too long":     {strings.Repeat("x", MaxNameLen+1), 0, ""},
		"name with a tab":   {"web\t1", 0, ""},
		"note too long":     {"web-1", 0, strings.Repeat("x", MaxNoteLen+1)},
		"note with newline": {"web-1", 0, "line\nline"},
		"sort out of range": {"web-1", MaxSort + 1, ""},
	}
	for label, c := range cases {
		if err := s.Update(t.Context(), id, c.name, c.sort, c.note); err == nil {
			t.Errorf("%s was accepted", label)
		}
	}
	// The record is untouched after every rejection.
	r, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "web-1" || r.Note != "" || r.Sort != 0 {
		t.Errorf("record changed: %+v", r)
	}
}

// Deleting a host takes its history and its credential with it.
func TestDeleteRemovesEverythingOfTheHost(t *testing.T) {
	s, db := newService(t)
	ctx := t.Context()
	hostID, auth := enrolled(t, s)
	credential := protocol.AgentAuthorization(auth.Registered.AgentID + "." + auth.Registered.Secret)
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO metrics_raw (host_id, ts, cpu) VALUES (?, ?, ?)", hostID, 1726560000, 10.0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// An unused token of that host must not survive it either.
	if _, err := s.NewEnrollToken(ctx, hostID); err != nil {
		t.Fatal(err)
	}

	agentID, err := s.Delete(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if agentID != auth.AgentID {
		t.Errorf("Delete returned agent id %q, want %q", agentID, auth.AgentID)
	}
	if _, err := s.Authenticate(ctx, credential); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the credential of a deleted host still works: %v", err)
	}
	for _, q := range []string{
		"SELECT count(*) FROM hosts WHERE id = ?",
		"SELECT count(*) FROM metrics_raw WHERE host_id = ?",
		"SELECT count(*) FROM enroll_tokens WHERE host_id = ?",
	} {
		var n int
		if err := db.Read().QueryRowContext(ctx, q, hostID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows left behind by %q", n, q)
		}
	}
}

// Resetting access revokes the credential, drops tokens that were still
// outstanding and hands back exactly one fresh token.
func TestResetAccessRevokesAndReissues(t *testing.T) {
	s, _ := newService(t)
	ctx := t.Context()
	hostID, auth := enrolled(t, s)
	credential := protocol.AgentAuthorization(auth.Registered.AgentID + "." + auth.Registered.Secret)

	stale, err := s.NewEnrollToken(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}

	agentID, token, err := s.ResetAccess(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if agentID != auth.AgentID {
		t.Errorf("ResetAccess returned agent id %q, want %q", agentID, auth.AgentID)
	}
	if _, err := s.Authenticate(ctx, credential); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the revoked credential still works: %v", err)
	}
	if _, err := s.Authenticate(ctx, protocol.EnrollAuthorization(stale)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a token issued before the reset still enrolls: %v", err)
	}

	fresh, err := s.Authenticate(ctx, protocol.EnrollAuthorization(token))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.HostID != hostID || fresh.Registered == nil {
		t.Errorf("re-enrollment = %+v", fresh)
	}
	if fresh.AgentID == auth.AgentID {
		t.Error("the host kept its old agent id after a reset")
	}
	r, err := s.Get(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pending() {
		t.Error("the host is still pending after re-enrolling")
	}
}

// A reset leaves the host pending until an agent enrolls again.
func TestResetAccessLeavesTheHostPending(t *testing.T) {
	s, _ := newService(t)
	hostID, _ := enrolled(t, s)
	if _, _, err := s.ResetAccess(t.Context(), hostID); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(t.Context(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Pending() || r.AgentID != "" {
		t.Errorf("record = %+v, want a pending host", r)
	}
}
