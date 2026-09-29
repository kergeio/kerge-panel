// Package settings holds the panel-wide preferences the settings page
// edits: the interface language, the time zone, how long metrics are kept
// and the default interface exclusion rules.
package settings

import (
	"context"
	"database/sql"
	"errors"

	"github.com/kergeio/kerge-panel/internal/rollup"
	"github.com/kergeio/kerge-panel/internal/store"
)

// Panel is every preference the settings page shows. An empty Language or
// TimeZone means the caller's own default.
type Panel struct {
	Language string
	TimeZone string
	// DateFormat is empty until the operator picks one.
	DateFormat string
	Retention  rollup.Retention
	// IfaceExclude is the stored default exclusion list, and Chosen
	// reports whether one has been stored at all: until then the
	// built-in list applies, and an empty stored list means that no
	// interface is excluded.
	IfaceExclude       string
	IfaceExcludeChosen bool
}

// Service reads and writes the preferences.
type Service struct{ db *store.DB }

// NewService returns a service backed by db.
func NewService(db *store.DB) *Service { return &Service{db: db} }

// Load reads every preference.
func (s *Service) Load(ctx context.Context) (Panel, error) {
	var p Panel
	var err error
	if p.Language, _, err = s.db.GetSetting(ctx, store.SettingLanguage); err != nil {
		return Panel{}, err
	}
	if p.TimeZone, _, err = s.db.GetSetting(ctx, store.SettingTimeZone); err != nil {
		return Panel{}, err
	}
	if p.DateFormat, _, err = s.db.GetSetting(ctx, store.SettingDateFormat); err != nil {
		return Panel{}, err
	}
	if p.IfaceExclude, p.IfaceExcludeChosen, err = s.db.GetSetting(ctx, store.SettingNetIfaceExclude); err != nil {
		return Panel{}, err
	}
	if p.Retention, err = rollup.LoadRetention(ctx, s.db); err != nil {
		return Panel{}, err
	}
	return p, nil
}

// SaveDisplay stores the language, the time zone and the date format.
// The caller has checked that all three are ones the panel offers.
func (s *Service) SaveDisplay(ctx context.Context, language, timeZone, dateFormat string) error {
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := store.SetSetting(ctx, tx, store.SettingLanguage, language); err != nil {
			return err
		}
		if err := store.SetSetting(ctx, tx, store.SettingTimeZone, timeZone); err != nil {
			return err
		}
		return store.SetSetting(ctx, tx, store.SettingDateFormat, dateFormat)
	})
}

// SaveData stores the retention and the default exclusion list. The caller
// has checked both; an empty list excludes nothing.
func (s *Service) SaveData(ctx context.Context, retention rollup.Retention, ifaceExclude string) error {
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := rollup.StoreRetention(ctx, tx, retention); err != nil {
			return err
		}
		return store.SetSetting(ctx, tx, store.SettingNetIfaceExclude, ifaceExclude)
	})
}

// RevealIPs reports whether host addresses are shown across the panel
// rather than masked. They are masked until the operator says otherwise.
func (s *Service) RevealIPs(ctx context.Context) (bool, error) {
	value, _, err := s.db.GetSetting(ctx, store.SettingRevealIPs)
	return value == "1", err
}

// The ways the dashboard lists the hosts.
const (
	ViewCards = "cards"
	ViewList  = "list"
)

// DashboardView returns how the dashboard lists the hosts: ViewCards
// until the operator picks the list, and for anything unreadable.
func (s *Service) DashboardView(ctx context.Context) (string, error) {
	value, _, err := s.db.GetSetting(ctx, store.SettingDashboardView)
	if value == ViewList {
		return ViewList, err
	}
	return ViewCards, err
}

// SetDashboardView stores how the dashboard lists the hosts, which is
// ViewCards or ViewList.
func (s *Service) SetDashboardView(ctx context.Context, view string) error {
	if view != ViewCards && view != ViewList {
		return errors.New("settings: unknown dashboard view " + view)
	}
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return store.SetSetting(ctx, tx, store.SettingDashboardView, view)
	})
}

// SetRevealIPs stores whether host addresses are shown.
func (s *Service) SetRevealIPs(ctx context.Context, reveal bool) error {
	value := "0"
	if reveal {
		value = "1"
	}
	return s.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return store.SetSetting(ctx, tx, store.SettingRevealIPs, value)
	})
}
