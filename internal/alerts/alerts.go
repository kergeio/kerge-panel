// Package alerts watches the hosts and sends the outbound notifications the
// operator has switched on.
//
// What has been notified is kept in the alerts table, one row per host and
// kind of event, so nothing is sent twice, not even across a panel
// restart:
//
//   - offline: the row exists while the host is offline. It is written
//     when the host goes offline and deleted when it is back online.
//   - renewal: since is the reminder date last notified.
//   - traffic: since is the first day of the billing cycle last notified.
//   - disk, memory, cpu: the row exists while usage is over the threshold.
//     It is written once usage has stayed at or above the threshold for
//     the set minutes, and deleted once it has stayed below for as long.
//     How long usage has been above or below is kept in memory, from the
//     latest sample every check, so after a restart the minutes start
//     over.
//
// Rows are kept whether or not the event is switched on, so switching it on
// later does not report events that happened before.
package alerts

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/i18n"
	"github.com/kergeio/kerge-panel/internal/notify"
	"github.com/kergeio/kerge-panel/internal/reminder"
	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-panel/internal/traffic"
)

// Kinds of event, as stored in the alerts table.
const (
	kindOffline = "offline"
	kindRenewal = "renewal"
	kindTraffic = "traffic"
	kindDisk    = "disk"
	kindMemory  = "memory"
	kindCPU     = "cpu"
)

// renewalHour is the hour of the reminder day, in the panel's time zone,
// from which the renewal notification is sent.
const renewalHour = 9

// Options configure a Watcher.
type Options struct {
	DB       *store.DB
	Hosts    *hosts.Service
	Status   *status.Registry
	Settings *settings.Service
	Sender   *notify.Sender
	// Translator returns the translator of the interface language.
	Translator func() *i18n.Translator
	// TimeZone returns the panel's time zone, which decides the day and
	// hour of the renewal notification and the billing cycle.
	TimeZone func() *time.Location
	Logger   *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Watcher checks the hosts and sends notifications. Check is not safe for
// concurrent use; Run calls it from one goroutine.
type Watcher struct {
	opts Options
	// usage tracks, per host and usage kind, since when usage has stayed
	// above or below its threshold.
	usage map[alertKey]*streak
}

// streak is how long usage has stayed on one side of a threshold. At most
// one of the two times is set.
type streak struct {
	above, below time.Time
}

// New returns a watcher.
func New(opts Options) *Watcher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Watcher{opts: opts, usage: make(map[alertKey]*streak)}
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

// Check compares every enrolled host with what has been notified for it,
// records what changed and sends the notifications that are on.
func (w *Watcher) Check(ctx context.Context) error {
	records, err := w.opts.Hosts.List(ctx)
	if err != nil {
		return err
	}
	cfg, err := w.opts.Settings.Notifications(ctx)
	if err != nil {
		return err
	}
	stored, err := w.stored(ctx)
	if err != nil {
		return err
	}
	enrolled := make([]hosts.Record, 0, len(records))
	for _, rec := range records {
		if !rec.Pending() {
			enrolled = append(enrolled, rec)
		}
	}
	ids := make([]int64, len(enrolled))
	for i, rec := range enrolled {
		ids[i] = rec.ID
	}
	lives := w.opts.Status.All(ids)
	if err := w.checkOffline(ctx, cfg, enrolled, lives, stored); err != nil {
		return err
	}
	if err := w.checkUsage(ctx, cfg, enrolled, lives, stored); err != nil {
		return err
	}
	local := w.opts.Now().In(w.location())
	today := reminder.DateOf(local)
	if err := w.checkRenewal(ctx, cfg, enrolled, stored, local, today); err != nil {
		return err
	}
	return w.checkTraffic(ctx, cfg, enrolled, stored, today)
}

// checkOffline reports a host going offline, and coming back online.
// lives holds the live view of records, in the same order.
func (w *Watcher) checkOffline(ctx context.Context, cfg notify.Config, records []hosts.Record, lives []status.Live, stored map[alertKey]int64) error {
	for i, live := range lives {
		rec := records[i]
		_, offline := stored[alertKey{rec.ID, kindOffline}]
		switch {
		case live.State == status.Offline && !offline:
			if err := w.record(ctx, rec.ID, kindOffline, w.opts.Now().Unix()); err != nil {
				return err
			}
			if cfg.Enabled && cfg.Offline {
				w.send(ctx, cfg, rec, "notify.message.offline", nil)
			}
		case live.State == status.Online && offline:
			if err := w.end(ctx, rec.ID, kindOffline); err != nil {
				return err
			}
			if cfg.Enabled && cfg.Offline {
				w.send(ctx, cfg, rec, "notify.message.online", nil)
			}
		}
	}
	return nil
}

// checkRenewal reports a renewal reminder on its day, from 09:00 on, unless
// the operator has already confirmed the renewal.
func (w *Watcher) checkRenewal(ctx context.Context, cfg notify.Config, records []hosts.Record, stored map[alertKey]int64, local time.Time, today reminder.Date) error {
	if local.Hour() < renewalHour {
		return nil
	}
	day := today.Time().Unix()
	for _, rec := range records {
		if rec.Reminder == nil {
			continue
		}
		st := rec.Reminder.At(today)
		if !st.Due || st.Date != today {
			continue
		}
		if since, ok := stored[alertKey{rec.ID, kindRenewal}]; ok && since == day {
			continue
		}
		if err := w.record(ctx, rec.ID, kindRenewal, day); err != nil {
			return err
		}
		if cfg.Enabled && cfg.Renewal {
			w.send(ctx, cfg, rec, "notify.message.renewal", nil)
		}
	}
	return nil
}

// checkTraffic reports a host whose traffic in the current billing cycle
// has reached its limit, once per cycle.
func (w *Watcher) checkTraffic(ctx context.Context, cfg notify.Config, records []hosts.Record, stored map[alertKey]int64, today reminder.Date) error {
	var days []traffic.Day
	loaded := false
	for _, rec := range records {
		set := rec.Traffic
		if set.LimitBytes <= 0 {
			continue
		}
		start := traffic.CycleStart(today, set.ResetDay)
		cycle := start.Time().Unix()
		if since, ok := stored[alertKey{rec.ID, kindTraffic}]; ok && since == cycle {
			continue
		}
		if !loaded {
			var err error
			if days, err = w.opts.Hosts.DailyTraffic(ctx, traffic.EarliestCycleStart(today)); err != nil {
				return err
			}
			loaded = true
		}
		if set.Used(traffic.Sum(days, rec.ID, start)) < set.LimitBytes {
			continue
		}
		if err := w.record(ctx, rec.ID, kindTraffic, cycle); err != nil {
			return err
		}
		if cfg.Enabled && cfg.Traffic {
			limit := w.opts.Translator().Bytes(uint64(set.LimitBytes))
			w.send(ctx, cfg, rec, "notify.message.traffic", strings.NewReplacer("{limit}", limit))
		}
	}
	return nil
}

// usageKind is one of the usage events.
type usageKind struct {
	kind string
	// value returns the usage in percent, and false when the sample does
	// not have it.
	value func(status.Sample) (float64, bool)
	// limit returns the event's settings.
	limit func(notify.Config) notify.Threshold
}

var usageKinds = []usageKind{
	{kindDisk, func(s status.Sample) (float64, bool) { return share(s.DiskUsed, s.DiskTotal) },
		func(c notify.Config) notify.Threshold { return c.Disk }},
	{kindMemory, func(s status.Sample) (float64, bool) { return share(s.MemUsed, s.MemTotal) },
		func(c notify.Config) notify.Threshold { return c.Memory }},
	{kindCPU, func(s status.Sample) (float64, bool) {
		if s.CPU == nil {
			return 0, false
		}
		return *s.CPU, true
	}, func(c notify.Config) notify.Threshold { return c.CPU }},
}

// share is used as a percentage of total, the way the dashboard shows it.
func share(used, total *uint64) (float64, bool) {
	if used == nil || total == nil || *total == 0 {
		return 0, false
	}
	return float64(*used) / float64(*total) * 100, true
}

// checkUsage reports usage that has stayed at or above its threshold for
// the set minutes, and its return below the threshold for as long. Only a
// host that is online counts; anything else starts the minutes over.
func (w *Watcher) checkUsage(ctx context.Context, cfg notify.Config, records []hosts.Record, lives []status.Live, stored map[alertKey]int64) error {
	now := w.opts.Now()
	seen := make(map[alertKey]bool)
	for i, live := range lives {
		rec := records[i]
		for _, u := range usageKinds {
			key := alertKey{rec.ID, u.kind}
			seen[key] = true
			v, ok := u.value(live.Sample)
			if live.State != status.Online || !live.HasSample || !ok {
				delete(w.usage, key)
				continue
			}
			st := w.usage[key]
			if st == nil {
				st = &streak{}
				w.usage[key] = st
			}
			limit := u.limit(cfg)
			hold := time.Duration(limit.Minutes) * time.Minute
			_, firing := stored[key]
			if v >= float64(limit.Percent) {
				st.below = time.Time{}
				if st.above.IsZero() {
					st.above = now
				}
				if firing || now.Sub(st.above) < hold {
					continue
				}
				if err := w.record(ctx, rec.ID, u.kind, now.Unix()); err != nil {
					return err
				}
				if cfg.Enabled && limit.On {
					w.send(ctx, cfg, rec, "notify.message."+u.kind+".high", w.usageText(v, limit))
				}
				continue
			}
			st.above = time.Time{}
			if st.below.IsZero() {
				st.below = now
			}
			if !firing || now.Sub(st.below) < hold {
				continue
			}
			if err := w.end(ctx, rec.ID, u.kind); err != nil {
				return err
			}
			if cfg.Enabled && limit.On {
				w.send(ctx, cfg, rec, "notify.message."+u.kind+".back", w.usageText(v, limit))
			}
		}
	}
	// Forget hosts that were deleted or are no longer enrolled.
	for key := range w.usage {
		if !seen[key] {
			delete(w.usage, key)
		}
	}
	return nil
}

// usageText fills the value, threshold and minutes of a usage message.
func (w *Watcher) usageText(v float64, limit notify.Threshold) *strings.Replacer {
	return strings.NewReplacer(
		"{value}", w.opts.Translator().Percent(min(v, 100)),
		"{threshold}", strconv.Itoa(limit.Percent)+"%",
		"{minutes}", strconv.Itoa(limit.Minutes),
	)
}

func (w *Watcher) location() *time.Location {
	if w.opts.TimeZone == nil {
		return time.UTC
	}
	if loc := w.opts.TimeZone(); loc != nil {
		return loc
	}
	return time.UTC
}

type alertKey struct {
	hostID int64
	kind   string
}

// stored returns what has been notified: the since value of every row.
func (w *Watcher) stored(ctx context.Context) (map[alertKey]int64, error) {
	rows, err := w.opts.DB.Read().QueryContext(ctx, "SELECT host_id, kind, since FROM alerts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[alertKey]int64)
	for rows.Next() {
		var k alertKey
		var since int64
		if err := rows.Scan(&k.hostID, &k.kind, &since); err != nil {
			return nil, err
		}
		out[k] = since
	}
	return out, rows.Err()
}

// record writes or replaces the row of one host and kind.
func (w *Watcher) record(ctx context.Context, hostID int64, kind string, since int64) error {
	return w.opts.DB.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// A host deleted since it was listed has no row to refer to;
		// writing nothing then is right.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO alerts (host_id, kind, since)
			 SELECT id, ?, ? FROM hosts WHERE id = ?
			 ON CONFLICT (host_id, kind) DO UPDATE SET since = excluded.since`,
			kind, since, hostID)
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
// extra fills further placeholders of the message, or is nil.
func (w *Watcher) send(ctx context.Context, cfg notify.Config, rec hosts.Record, key string, extra *strings.Replacer) {
	msg := strings.ReplaceAll(w.opts.Translator().T(key), "{host}", hostLabel(rec))
	if extra != nil {
		msg = extra.Replace(msg)
	}
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
