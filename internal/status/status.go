// Package status keeps the live view of the hosts: whether an agent is
// connected, when it last reported and what it last reported.
//
// Nothing here is stored. A state is a function of the connection, the
// time since the last sample, the reporting interval and the panel's start
// time, so it is computed when it is read and recomputed every few seconds
// by the evaluator, which pushes the result to the browsers watching.
package status

import (
	"context"
	"sync"
	"time"
)

// State is what the panel shows for a host.
type State string

const (
	// Pending is a host that has no agent credential yet. It comes from
	// the database, not from this registry.
	Pending State = "pending"
	// Connecting is a registered host that has not connected yet, within
	// the grace period after the panel started.
	Connecting State = "connecting"
	// Online is a connected host reporting on time.
	Online State = "online"
	// Stale is a connected host whose samples stopped arriving.
	Stale State = "stale"
	// Offline is a host with no connection, or one that stopped
	// reporting for so long that the connection counts for nothing.
	Offline State = "offline"
)

// Timing of the state machine.
const (
	// StartGrace is how long after the panel started a registered host
	// may stay "connecting" while its agent reconnects.
	StartGrace = 90 * time.Second
	// MinInterval and MaxInterval clamp the interval an agent reports
	// before it is used for the thresholds.
	MinInterval = 3 * time.Second
	MaxInterval = 60 * time.Second
	// DefaultInterval is used until an agent has reported its own.
	DefaultInterval = 5 * time.Second
	// StaleFloor and OfflineFloor are the lower bounds of the two
	// thresholds, which are otherwise 3 and 6 reporting intervals.
	StaleFloor   = 60 * time.Second
	OfflineFloor = 180 * time.Second
	// EvalInterval is how often the evaluator recomputes and pushes.
	EvalInterval = 5 * time.Second
)

// Sample is the latest metrics of one host, as the cards show them. An
// absent field was not reported in that sample.
type Sample struct {
	At        time.Time
	CPU       *float64
	MemUsed   *uint64
	MemTotal  *uint64
	SwapUsed  *uint64
	SwapTotal *uint64
	DiskUsed  *uint64
	DiskTotal *uint64
	RxRate    *float64
	TxRate    *float64
	Uptime    *uint64
}

// Live is the registry's view of one host.
type Live struct {
	HostID     int64
	State      State
	Connected  bool
	LastDataAt time.Time
	Interval   time.Duration
	Sample     Sample
	// HasSample reports whether Sample holds a reading.
	HasSample bool
}

// entry is what the registry keeps per connected or once-connected host.
type entry struct {
	connected bool
	// everConnected marks a host whose agent has connected at least once
	// since the panel started. Such a host is offline when the connection
	// drops, rather than falling back into the start-up grace period.
	everConnected bool
	// closeConn ends the agent connection when the host is written off as
	// offline. It is nil when nothing is connected.
	closeConn  func()
	lastDataAt time.Time
	interval   time.Duration
	sample     Sample
	hasSample  bool
}

// Registry is the panel's live state. It is safe for concurrent use: the
// agent endpoint writes to it, the pages and the evaluator read from it.
type Registry struct {
	now       func() time.Time
	startedAt time.Time

	mu      sync.Mutex
	entries map[int64]*entry
	subs    map[*subscriber]struct{}
}

// Options configure a Registry.
type Options struct {
	// Now is the clock; zero means time.Now.
	Now func() time.Time
}

// New returns a registry whose grace period starts now.
func New(opts Options) *Registry {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Registry{
		now:       now,
		startedAt: now(),
		entries:   make(map[int64]*entry),
		subs:      make(map[*subscriber]struct{}),
	}
}

// Connected records that an agent of this host is connected. closeConn is
// called if the host is later written off as offline while still holding
// the connection.
func (r *Registry) Connected(hostID int64, interval time.Duration, closeConn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entry(hostID)
	e.connected = true
	e.everConnected = true
	e.closeConn = closeConn
	// A connection with no sample yet counts from the moment it opened,
	// so a silent agent still runs into the stale and offline thresholds.
	e.lastDataAt = r.now()
	if interval > 0 {
		e.interval = clampInterval(interval)
	}
}

// Disconnected records that the connection ended. The entry stays, so the
// host keeps its last sample and its last data time.
func (r *Registry) Disconnected(hostID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[hostID]
	if !ok {
		return
	}
	e.connected = false
	e.closeConn = nil
}

// SetInterval records the reporting interval an agent announced.
func (r *Registry) SetInterval(hostID int64, interval time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entry(hostID).interval = clampInterval(interval)
}

// Report records one sample.
func (r *Registry) Report(hostID int64, s Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entry(hostID)
	e.lastDataAt = s.At
	e.sample = s
	e.hasSample = true
}

// Forget drops a host, for when it is deleted.
func (r *Registry) Forget(hostID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, hostID)
}

// Get returns the live view of one host. A host the registry has never
// seen is connecting while the grace period lasts and offline afterwards.
func (r *Registry) Get(hostID int64) Live {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live(hostID, r.entries[hostID], r.now())
}

// All returns the live view of the given hosts, in the order asked for.
func (r *Registry) All(hostIDs []int64) []Live {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]Live, 0, len(hostIDs))
	for _, id := range hostIDs {
		out = append(out, r.live(id, r.entries[id], now))
	}
	return out
}

// entry returns the entry of a host, creating it with the defaults.
// The caller holds the lock.
func (r *Registry) entry(hostID int64) *entry {
	e, ok := r.entries[hostID]
	if !ok {
		e = &entry{interval: DefaultInterval}
		r.entries[hostID] = e
	}
	return e
}

// live computes the view of one host. The caller holds the lock.
func (r *Registry) live(hostID int64, e *entry, now time.Time) Live {
	if e == nil {
		return Live{HostID: hostID, State: r.unseenState(now), Interval: DefaultInterval}
	}
	l := Live{
		HostID:     hostID,
		Connected:  e.connected,
		LastDataAt: e.lastDataAt,
		Interval:   e.interval,
		Sample:     e.sample,
		HasSample:  e.hasSample,
	}
	l.State = state(e, now, r.unseenState(now))
	return l
}

// unseenState is what a host without a connection shows: the panel gives
// the agents a grace period to reconnect after it starts, because every
// agent is dialing in at once.
func (r *Registry) unseenState(now time.Time) State {
	if now.Sub(r.startedAt) < StartGrace {
		return Connecting
	}
	return Offline
}

// state computes the state of one entry from its connection and the time
// since its last sample.
func state(e *entry, now time.Time, unseen State) State {
	if !e.connected {
		if e.everConnected {
			return Offline
		}
		return unseen
	}
	since := now.Sub(e.lastDataAt)
	switch {
	case since > max(6*e.interval, OfflineFloor):
		return Offline
	case since > max(3*e.interval, StaleFloor):
		return Stale
	default:
		return Online
	}
}

func clampInterval(d time.Duration) time.Duration {
	return min(max(d, MinInterval), MaxInterval)
}

// Evaluate recomputes every state, closes the connection of a host that is
// now offline, and returns the live view of the hosts asked for.
//
// Closing an offline connection is what keeps a half-open TCP connection
// from holding a host "online" forever.
func (r *Registry) Evaluate(hostIDs []int64) []Live {
	r.mu.Lock()
	now := r.now()
	var closers []func()
	out := make([]Live, 0, len(hostIDs))
	for _, id := range hostIDs {
		e := r.entries[id]
		l := r.live(id, e, now)
		if e != nil && e.connected && l.State == Offline && e.closeConn != nil {
			closers = append(closers, e.closeConn)
			e.closeConn = nil
		}
		out = append(out, l)
	}
	r.mu.Unlock()

	// Outside the lock: closing a connection can block on a peer that is
	// no longer answering.
	for _, close := range closers {
		go close()
	}
	return out
}

// subscriber is one watcher, typically a browser. The channel holds at
// most one update: a watcher that falls behind gets the newest view, not a
// backlog.
type subscriber struct {
	ch chan []Live
}

// Subscribe returns a channel of updates and a function that stops it.
func (r *Registry) Subscribe() (<-chan []Live, func()) {
	s := &subscriber{ch: make(chan []Live, 1)}
	r.mu.Lock()
	r.subs[s] = struct{}{}
	r.mu.Unlock()
	return s.ch, func() {
		r.mu.Lock()
		delete(r.subs, s)
		r.mu.Unlock()
	}
}

// Publish sends a view to every watcher, replacing an update a watcher has
// not picked up yet.
func (r *Registry) Publish(view []Live) {
	r.mu.Lock()
	subs := make([]*subscriber, 0, len(r.subs))
	for s := range r.subs {
		subs = append(subs, s)
	}
	r.mu.Unlock()
	for _, s := range subs {
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- view:
		default:
		}
	}
}

// Run evaluates every EvalInterval until ctx is done, publishing the
// result to the watchers. hosts returns the hosts to evaluate, newest
// listing each time, so hosts added or deleted in the pages are picked up
// without further wiring.
func (r *Registry) Run(ctx context.Context, hosts func(context.Context) []int64) {
	t := time.NewTicker(EvalInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Publish(r.Evaluate(hosts(ctx)))
		}
	}
}
