// Package alerts watches the hosts and sends the outbound notifications the
// operator has switched on.
//
// Each event a host is in the middle of is kept in the alerts table: a row
// is written when the event starts and deleted when it ends. A start or an
// end is notified only when the row changes, so nothing is sent twice,
// not even across a panel restart. Rows are kept whether or not the event
// is switched on, so switching it on later does not report events that
// started long before.
package alerts

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/notify"
	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/store"
)

// Kinds of event, as stored in the alerts table.
const kindOffline = "offline"

// Options configure a Watcher.
type Options struct {
	DB       *store.DB
	Hosts    *hosts.Service
	Status   *status.Registry
	Settings *settings.Service
	Sender   *notify.Sender
	// Text returns the text of a translation key in the interface
	// language.
	Text   func(key string) string
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Watcher checks the hosts and sends notifications.
type Watcher struct {
	opts Options
}

// New returns a watcher.
func New(opts Options) *Watcher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Watcher{opts: opts}
}

// Run checks every status.EvalInterval until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(status.EvalInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Check(ctx); err != nil && ctx.Err() == nil {
				w.opts.Logger.Error("checking the hosts for notifications", "err", err)
			}
		}
	}
}

// Check compares every enrolled host with the events stored for it,
// records what started or ended and sends the notifications that are on.
func (w *Watcher) Check(ctx context.Context) error {
	records, err := w.opts.Hosts.List(ctx)
	if err != nil {
		return err
	}
	cfg, err := w.opts.Settings.Notifications(ctx)
	if err != nil {
		return err
	}
	active, err := w.active(ctx)
	if err != nil {
		return err
	}

	byID := make(map[int64]hosts.Record, len(records))
	ids := make([]int64, 0, len(records))
	for _, rec := range records {
		if rec.Pending() {
			continue
		}
		byID[rec.ID] = rec
		ids = append(ids, rec.ID)
	}
	for _, live := range w.opts.Status.All(ids) {
		rec := byID[live.HostID]
		offline := active[alertKey{rec.ID, kindOffline}]
		switch {
		case live.State == status.Offline && !offline:
			if err := w.start(ctx, rec.ID, kindOffline); err != nil {
				return err
			}
			if cfg.Enabled && cfg.Offline {
				w.send(ctx, cfg, rec, "notify.message.offline")
			}
		case live.State == status.Online && offline:
			if err := w.end(ctx, rec.ID, kindOffline); err != nil {
				return err
			}
			if cfg.Enabled && cfg.Offline {
				w.send(ctx, cfg, rec, "notify.message.online")
			}
		}
	}
	return nil
}

type alertKey struct {
	hostID int64
	kind   string
}

// active returns the stored events.
func (w *Watcher) active(ctx context.Context) (map[alertKey]bool, error) {
	rows, err := w.opts.DB.Read().QueryContext(ctx, "SELECT host_id, kind FROM alerts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[alertKey]bool)
	for rows.Next() {
		var k alertKey
		if err := rows.Scan(&k.hostID, &k.kind); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

func (w *Watcher) start(ctx context.Context, hostID int64, kind string) error {
	return w.opts.DB.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// A host deleted since it was listed has no row to refer to;
		// inserting nothing then is right.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO alerts (host_id, kind, since)
			 SELECT id, ?, ? FROM hosts WHERE id = ?
			 ON CONFLICT (host_id, kind) DO NOTHING`,
			kind, w.opts.Now().Unix(), hostID)
		return err
	})
}

func (w *Watcher) end(ctx context.Context, hostID int64, kind string) error {
	return w.opts.DB.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM alerts WHERE host_id = ? AND kind = ?", hostID, kind)
		return err
	})
}

// send delivers one message. A failure is logged and not retried; the
// event has been recorded, so it is not sent again either way.
func (w *Watcher) send(ctx context.Context, cfg notify.Config, rec hosts.Record, key string) {
	msg := strings.ReplaceAll(w.opts.Text(key), "{host}", hostLabel(rec))
	reply, err := w.opts.Sender.Send(ctx, cfg.URL, notify.Render(cfg.Body, msg))
	switch {
	case err != nil:
		reason, timedOut := notify.Reason(err)
		if timedOut {
			reason = "no reply within " + notify.Timeout.String()
		}
		// Never the URL: it may hold a token.
		w.opts.Logger.Warn("notification not delivered", "host_id", rec.ID, "event", key, "reason", reason)
	case !reply.OK():
		w.opts.Logger.Warn("notification refused by the webhook", "host_id", rec.ID, "event", key, "status", reply.Code)
	}
}

// hostLabel names a host in a message: its name, then its address when
// the panel has one.
func hostLabel(rec hosts.Record) string {
	if rec.RemoteIP == "" {
		return rec.Name
	}
	return rec.Name + " (" + rec.RemoteIP + ")"
}
