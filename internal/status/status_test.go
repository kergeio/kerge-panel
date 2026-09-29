package status

import (
	"context"
	"sync"
	"testing"
	"time"
)

// clock is a test clock the registry reads.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newRegistry(t *testing.T) (*Registry, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	return New(Options{Now: c.now}), c
}

// A connected host is online until the samples stop, stale for a while
// after that, and finally offline.
func TestStateOfAConnectedHost(t *testing.T) {
	r, c := newRegistry(t)
	r.Connected(1, 5*time.Second, nil)
	r.Report(1, Sample{At: c.now()})

	cases := []struct {
		after time.Duration
		want  State
	}{
		{0, Online},
		{59 * time.Second, Online},
		// The floors win at a 5 second interval: 3 x 5s is below the
		// 60 second floor, and 6 x 5s below the 180 second one.
		{61 * time.Second, Stale},
		{179 * time.Second, Stale},
		{181 * time.Second, Offline},
	}
	start := c.now()
	for _, tc := range cases {
		c.t = start.Add(tc.after)
		if got := r.Get(1).State; got != tc.want {
			t.Errorf("%v after the last sample: %s, want %s", tc.after, got, tc.want)
		}
	}
}

// With a slow agent the thresholds follow the interval instead of the
// floors: 3 and 6 reporting intervals.
func TestThresholdsFollowTheInterval(t *testing.T) {
	r, c := newRegistry(t)
	r.Connected(1, 60*time.Second, nil)
	r.Report(1, Sample{At: c.now()})
	start := c.now()

	for _, tc := range []struct {
		after time.Duration
		want  State
	}{
		{170 * time.Second, Online}, // below 3 x 60s
		{190 * time.Second, Stale},  // above 3 x 60s, below 6 x 60s
		{370 * time.Second, Offline},
	} {
		c.t = start.Add(tc.after)
		if got := r.Get(1).State; got != tc.want {
			t.Errorf("%v after the last sample: %s, want %s", tc.after, got, tc.want)
		}
	}
}

// An interval outside the allowed range is clamped before it is used.
func TestIntervalIsClamped(t *testing.T) {
	r, _ := newRegistry(t)
	r.SetInterval(1, time.Millisecond)
	if got := r.Get(1).Interval; got != MinInterval {
		t.Errorf("interval = %v, want %v", got, MinInterval)
	}
	r.SetInterval(2, time.Hour)
	if got := r.Get(2).Interval; got != MaxInterval {
		t.Errorf("interval = %v, want %v", got, MaxInterval)
	}
}

// A connection that has not delivered a sample yet still ages: the clock
// runs from the moment it opened.
func TestSilentConnectionGoesStale(t *testing.T) {
	r, c := newRegistry(t)
	r.Connected(1, 5*time.Second, nil)
	if got := r.Get(1).State; got != Online {
		t.Errorf("a fresh connection is %s, want %s", got, Online)
	}
	c.advance(61 * time.Second)
	if got := r.Get(1).State; got != Stale {
		t.Errorf("a silent connection is %s, want %s", got, Stale)
	}
}

// After the panel starts, a registered host that has not connected yet is
// "connecting" for the grace period and offline afterwards.
func TestConnectingGracePeriod(t *testing.T) {
	r, c := newRegistry(t)
	if got := r.Get(7).State; got != Connecting {
		t.Errorf("unseen host right after start: %s, want %s", got, Connecting)
	}
	c.advance(StartGrace + time.Second)
	if got := r.Get(7).State; got != Offline {
		t.Errorf("unseen host after the grace period: %s, want %s", got, Offline)
	}
}

// A host that connects and drops is offline, grace period or not: the
// grace period is about agents that have not dialed in yet.
func TestDisconnectedHostIsOfflineDuringTheGracePeriod(t *testing.T) {
	r, c := newRegistry(t)
	r.Connected(1, 5*time.Second, nil)
	r.Report(1, Sample{At: c.now()})
	r.Disconnected(1)
	if got := r.Get(1).State; got != Offline {
		t.Errorf("state after a disconnect: %s, want %s", got, Offline)
	}
	// The last sample survives the disconnect, for the card to show.
	if live := r.Get(1); !live.HasSample || live.LastDataAt.IsZero() {
		t.Errorf("the last sample was dropped: %+v", live)
	}
}

// A host written off as offline while a connection is still open has that
// connection closed.
func TestEvaluateClosesAnOfflineConnection(t *testing.T) {
	r, c := newRegistry(t)
	closed := make(chan struct{})
	r.Connected(1, 5*time.Second, func() { close(closed) })
	r.Report(1, Sample{At: c.now()})

	r.Evaluate([]int64{1})
	select {
	case <-closed:
		t.Fatal("an online connection was closed")
	default:
	}

	c.advance(OfflineFloor + time.Second)
	r.Evaluate([]int64{1})
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the offline connection was not closed")
	}

	// The closer runs once, not on every evaluation.
	r.Evaluate([]int64{1})
}

func TestForgetDropsTheHost(t *testing.T) {
	r, c := newRegistry(t)
	r.Connected(1, 5*time.Second, nil)
	r.Report(1, Sample{At: c.now()})
	r.Forget(1)
	if live := r.Get(1); live.HasSample {
		t.Errorf("a forgotten host kept its sample: %+v", live)
	}
}

func TestAllKeepsTheOrderAsked(t *testing.T) {
	r, c := newRegistry(t)
	r.Connected(2, 5*time.Second, nil)
	r.Report(2, Sample{At: c.now()})
	got := r.All([]int64{3, 2, 1})
	if len(got) != 3 || got[0].HostID != 3 || got[1].HostID != 2 || got[2].HostID != 1 {
		t.Fatalf("All returned %+v", got)
	}
	if got[1].State != Online {
		t.Errorf("the connected host is %s", got[1].State)
	}
}

// A watcher gets the newest view and never a backlog: an update that has
// not been picked up is replaced.
func TestSubscribeKeepsOnlyTheNewestView(t *testing.T) {
	r, _ := newRegistry(t)
	ch, stop := r.Subscribe()
	defer stop()

	r.Publish([]Live{{HostID: 1, State: Online}})
	r.Publish([]Live{{HostID: 1, State: Stale}})
	r.Publish([]Live{{HostID: 1, State: Offline}})

	select {
	case view := <-ch:
		if len(view) != 1 || view[0].State != Offline {
			t.Errorf("view = %+v, want the newest one", view)
		}
	default:
		t.Fatal("no update")
	}
	select {
	case view := <-ch:
		t.Errorf("a second update was queued: %+v", view)
	default:
	}

	// After unsubscribing a publish must not block or panic.
	stop()
	r.Publish([]Live{{HostID: 1, State: Online}})
}

func TestRunPublishesUntilTheContextEnds(t *testing.T) {
	r, _ := newRegistry(t)
	ch, stop := r.Subscribe()
	defer stop()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, func(context.Context) []int64 { return []int64{1} })
	}()

	select {
	case view := <-ch:
		if len(view) != 1 || view[0].HostID != 1 {
			t.Errorf("view = %+v", view)
		}
	case <-time.After(3 * EvalInterval):
		t.Fatal("the evaluator published nothing")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// The registry is written by the agent endpoint and read by the pages at
// the same time.
func TestConcurrentUse(t *testing.T) {
	r, c := newRegistry(t)
	var wg sync.WaitGroup
	for id := range int64(4) {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				r.Connected(id, 5*time.Second, func() {})
				r.Report(id, Sample{At: c.now()})
				r.Disconnected(id)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				r.Get(id)
				r.Evaluate([]int64{0, 1, 2, 3})
				r.Publish(nil)
			}
		}()
	}
	wg.Wait()
}
