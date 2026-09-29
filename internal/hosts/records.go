package hosts

import (
	"context"
	"database/sql"
	"net/netip"

	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-protocol"
	"github.com/kergeio/kerge-protocol/ifacefilter"
)

// Connected records the source address of an authenticated agent
// connection. remote_ip_changed_at only moves when the address itself
// changes.
func (s *Service) Connected(ctx context.Context, hostID int64, ip netip.Addr) error {
	var addr any
	if ip.IsValid() {
		addr = ip.String()
	}
	now := s.now().Unix()
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE hosts SET
				remote_ip_changed_at = CASE
					WHEN remote_ip IS ? THEN remote_ip_changed_at ELSE ?
				END,
				remote_ip = ?,
				last_seen_at = ?
			WHERE id = ?`, addr, now, addr, now, hostID)
		return err
	})
}

// SetHostInfo stores the description an agent reported. The values have
// already been sanitized by the protocol package.
func (s *Service) SetHostInfo(ctx context.Context, hostID int64, info *protocol.HostInfo) error {
	now := s.now().Unix()
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE hosts SET
				hostname = ?, os = ?, platform = ?, platform_version = ?,
				kernel = ?, arch = ?, cpu_model = ?, cpu_cores = ?,
				agent_version = ?, last_seen_at = ?
			WHERE id = ?`,
			info.Hostname, info.OS, info.Platform, info.PlatformVersion,
			info.Kernel, info.Arch, info.CPUModel, info.CPUCores,
			info.AgentVersion, now, hostID)
		return err
	})
}

// IfaceExclude returns the interface exclusion patterns that apply to a
// host: its own override, otherwise the panel default, otherwise the
// built-in list.
func (s *Service) IfaceExclude(ctx context.Context, hostID int64) ([]string, error) {
	var override sql.NullString
	err := s.db.Read().QueryRowContext(ctx,
		"SELECT net_iface_exclude FROM hosts WHERE id = ?", hostID).Scan(&override)
	if err != nil {
		return nil, err
	}
	if override.Valid {
		return ifacefilter.ParsePatterns(override.String), nil
	}
	value, ok, err := s.db.GetSetting(ctx, store.SettingNetIfaceExclude)
	if err != nil {
		return nil, err
	}
	if ok {
		return ifacefilter.ParsePatterns(value), nil
	}
	return ifacefilter.DefaultExclude, nil
}
