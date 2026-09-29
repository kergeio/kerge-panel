package hosts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kergeio/kerge-panel/internal/reminder"
	"github.com/kergeio/kerge-panel/internal/traffic"
)

// ErrNotFound means no host has the requested id.
var ErrNotFound = errors.New("hosts: no such host")

// Field limits for what the operator types. They bound what a page has to
// render; the agent's own strings are limited by the protocol instead.
const (
	MaxNameLen = 64
	MaxNoteLen = 200
	// MaxSort keeps the ordering value inside what a number input and an
	// INTEGER column handle comfortably.
	MaxSort = 100000
)

// Record is one host as the panel's pages show it. Fields the agent has
// not reported yet are empty, and the times are zero when unset.
type Record struct {
	ID   int64
	Name string
	Sort int64
	Note string

	// AgentID is empty until the host enrolls; Enrolled reports whether
	// the host has a long-lived credential. A host without one is
	// "pending".
	AgentID  string
	Enrolled bool

	Hostname        string
	OS              string
	Platform        string
	PlatformVersion string
	Kernel          string
	Arch            string
	CPUModel        string
	CPUCores        int64
	AgentVersion    string

	RemoteIP          string
	RemoteIPChangedAt time.Time
	LastSeenAt        time.Time
	CreatedAt         time.Time

	// Reminder is the renewal reminder, or nil when the host has none.
	Reminder *reminder.Schedule
	// Traffic is the traffic accounting of the host, with the defaults
	// filled in where nothing was saved.
	Traffic traffic.Settings
	// IfaceExclude is the host's own interface exclusion list, which
	// replaces the panel's; nil means the host follows the panel's rules.
	// An empty list excludes nothing.
	IfaceExclude *string
}

// Pending reports whether the host still has to enroll an agent.
func (r Record) Pending() bool { return !r.Enrolled }

const recordColumns = `id, name, sort, note, agent_id, secret_hash IS NOT NULL,
	hostname, os, platform, platform_version, kernel, arch, cpu_model, cpu_cores,
	agent_version, remote_ip, remote_ip_changed_at, last_seen_at, created_at,
	remind_start, cycle_months, acked_on, snoozed_on,
	traffic_mode, traffic_reset_day, traffic_limit_bytes, net_iface_exclude`

// List returns every host in display order.
func (s *Service) List(ctx context.Context) ([]Record, error) {
	rows, err := s.db.Read().QueryContext(ctx,
		"SELECT "+recordColumns+" FROM hosts ORDER BY sort, name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one host, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id int64) (Record, error) {
	row := s.db.Read().QueryRowContext(ctx,
		"SELECT "+recordColumns+" FROM hosts WHERE id = ?", id)
	r, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	return r, err
}

// scanner covers *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanRecord(sc scanner) (Record, error) {
	var (
		r       Record
		agentID sql.NullString
		hostname, osName, platform, platformVersion, kernel, arch, cpuModel,
		agentVersion, remoteIP sql.NullString
		cpuCores                           sql.NullInt64
		ipChangedAt, lastSeenAt, createdAt sql.NullInt64
		remindStart, ackedOn, snoozedOn    sql.NullString
		cycleMonths                        sql.NullInt64
		trafficMode, ifaceExclude          sql.NullString
		resetDay, limitBytes               sql.NullInt64
	)
	err := sc.Scan(&r.ID, &r.Name, &r.Sort, &r.Note, &agentID, &r.Enrolled,
		&hostname, &osName, &platform, &platformVersion, &kernel, &arch, &cpuModel, &cpuCores,
		&agentVersion, &remoteIP, &ipChangedAt, &lastSeenAt, &createdAt,
		&remindStart, &cycleMonths, &ackedOn, &snoozedOn,
		&trafficMode, &resetDay, &limitBytes, &ifaceExclude)
	if err != nil {
		return Record{}, err
	}
	if r.Reminder, err = scanSchedule(remindStart, cycleMonths, ackedOn, snoozedOn); err != nil {
		return Record{}, fmt.Errorf("hosts: reminder of host %d: %w", r.ID, err)
	}
	r.Traffic = traffic.Default()
	if trafficMode.Valid {
		r.Traffic.Mode = traffic.Mode(trafficMode.String)
	}
	if resetDay.Valid {
		r.Traffic.ResetDay = int(resetDay.Int64)
	}
	r.Traffic.LimitBytes = limitBytes.Int64
	if ifaceExclude.Valid {
		r.IfaceExclude = &ifaceExclude.String
	}
	r.AgentID = agentID.String
	r.Hostname = hostname.String
	r.OS = osName.String
	r.Platform = platform.String
	r.PlatformVersion = platformVersion.String
	r.Kernel = kernel.String
	r.Arch = arch.String
	r.CPUModel = cpuModel.String
	r.CPUCores = cpuCores.Int64
	r.AgentVersion = agentVersion.String
	r.RemoteIP = remoteIP.String
	r.RemoteIPChangedAt = unixTime(ipChangedAt)
	r.LastSeenAt = unixTime(lastSeenAt)
	r.CreatedAt = unixTime(createdAt)
	return r, nil
}

func unixTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

// Update changes the fields the operator edits. The reminder and traffic
// settings of the same record are changed by SetReminder and SetTraffic.
func (s *Service) Update(ctx context.Context, id int64, name string, sort int64, note string) error {
	name, err := CleanName(name)
	if err != nil {
		return err
	}
	note, err = CleanNote(note)
	if err != nil {
		return err
	}
	if err := checkSort(sort); err != nil {
		return err
	}
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE hosts SET name = ?, sort = ?, note = ? WHERE id = ?", name, sort, note, id)
		if err != nil {
			return err
		}
		return requireOneRow(res)
	})
}

// Delete removes a host. The metrics, the traffic history and any unused
// enrollment token go with it through the foreign keys, and the long-lived
// credential stops working because the row that held its hash is gone. The
// agent id of the deleted host is returned so the caller can close a
// connection that is still open.
func (s *Service) Delete(ctx context.Context, id int64) (agentID string, err error) {
	err = s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var stored sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT agent_id FROM hosts WHERE id = ?", id).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		agentID = stored.String
		res, err := tx.ExecContext(ctx, "DELETE FROM hosts WHERE id = ?", id)
		if err != nil {
			return err
		}
		return requireOneRow(res)
	})
	if err != nil {
		return "", err
	}
	return agentID, nil
}

// ResetAccess revokes the host's credential and issues a fresh one-time
// token, which is what the operator installs on the host again. Unused
// tokens of that host are dropped as well, so exactly one token can enroll
// it. The revoked agent id is returned for closing a connection that is
// still open.
func (s *Service) ResetAccess(ctx context.Context, id int64) (agentID, token string, err error) {
	token, err = randomValue(tokenBytes)
	if err != nil {
		return "", "", err
	}
	err = s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var stored sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT agent_id FROM hosts WHERE id = ?", id).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		agentID = stored.String
		if _, err := tx.ExecContext(ctx,
			"UPDATE hosts SET agent_id = NULL, secret_hash = NULL WHERE id = ?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM enroll_tokens WHERE host_id = ?", id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO enroll_tokens (token_hash, host_id, expires_at) VALUES (?, ?, ?)",
			hashValue(token), id, s.now().Add(TokenTTL).Unix())
		return err
	})
	if err != nil {
		return "", "", err
	}
	return agentID, token, nil
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CleanName trims a host name and checks its length. The name is the
// operator's label for the host and has no format beyond that.
func CleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("hosts: the name must not be empty")
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return "", fmt.Errorf("hosts: the name must be at most %d characters", MaxNameLen)
	}
	if hasControl(name) {
		return "", errors.New("hosts: the name must not contain control characters")
	}
	return name, nil
}

// CleanNote trims a note and checks its length.
func CleanNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > MaxNoteLen {
		return "", fmt.Errorf("hosts: the note must be at most %d characters", MaxNoteLen)
	}
	if hasControl(note) {
		return "", errors.New("hosts: the note must not contain control characters")
	}
	return note, nil
}

func checkSort(sort int64) error {
	if sort < -MaxSort || sort > MaxSort {
		return fmt.Errorf("hosts: the sort value must be between %d and %d", -MaxSort, MaxSort)
	}
	return nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}
