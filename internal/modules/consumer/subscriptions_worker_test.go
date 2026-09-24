package consumer

// The subscription worker wakes at 12:00:05 IST (owner, 24 Sep, R4(ii)) as
// well as on its 15-minute ticker, so tomorrow's orders lock and reach the
// store seconds after the cut-off instead of up to 15 minutes later; it runs
// one tick at boot, and the wake is recomputed from the clock at every boot
// and after every wake, so a restart needs no stored state. Pure: the clock
// and both timers are injected, nothing sleeps.
//
//	go test ./internal/modules/consumer/ -run 'LockWake|SubscriptionWorker' -v

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNextLockWake(t *testing.T) {
	for _, c := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"11:59:59", istDayAt("2026-10-06", 11, 59).Add(59 * time.Second), istDayAt("2026-10-06", 12, 0).Add(5 * time.Second)},
		{"12:00:04", istDayAt("2026-10-06", 12, 0).Add(4 * time.Second), istDayAt("2026-10-06", 12, 0).Add(5 * time.Second)},
		{"exactly 12:00:05", istDayAt("2026-10-06", 12, 0).Add(5 * time.Second), istDayAt("2026-10-07", 12, 0).Add(5 * time.Second)},
		{"12:14:59", istDayAt("2026-10-06", 12, 14).Add(59 * time.Second), istDayAt("2026-10-07", 12, 0).Add(5 * time.Second)},
		{"23:30", istDayAt("2026-10-06", 23, 30), istDayAt("2026-10-07", 12, 0).Add(5 * time.Second)},
		{"00:00", istDayAt("2026-10-06", 0, 0), istDayAt("2026-10-06", 12, 0).Add(5 * time.Second)},
		{"month end", istDayAt("2026-10-31", 13, 0), istDayAt("2026-11-01", 12, 0).Add(5 * time.Second)},
		{"year end", istDayAt("2026-12-31", 23, 59), istDayAt("2027-01-01", 12, 0).Add(5 * time.Second)},
		// A UTC clock: 06:29 UTC is 11:59 IST, 06:31 UTC is 12:01 IST.
		{"UTC before", time.Date(2026, 10, 6, 6, 29, 0, 0, time.UTC), istDayAt("2026-10-06", 12, 0).Add(5 * time.Second)},
		{"UTC after", time.Date(2026, 10, 6, 6, 31, 0, 0, time.UTC), istDayAt("2026-10-07", 12, 0).Add(5 * time.Second)},
		// 20:00 UTC is 01:30 IST the NEXT day: that day's noon, not the UTC day's.
		{"UTC evening", time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC), istDayAt("2026-10-07", 12, 0).Add(5 * time.Second)},
	} {
		if got := nextLockWake(c.now); !got.Equal(c.want) {
			t.Errorf("%s: nextLockWake = %s, want %s", c.name, got.In(istZone), c.want.In(istZone))
		}
	}
}

// fakeWorkerClock drives runSubscriptionWorker: the test sets the time, fires
// the ticker and the wake, and reads which durations the worker armed.
type fakeWorkerClock struct {
	mu    sync.Mutex
	t     time.Time
	ticks chan time.Time
	wake  chan time.Time
	armed chan time.Duration
	runs  chan time.Time
}

func newFakeWorkerClock(at time.Time) *fakeWorkerClock {
	return &fakeWorkerClock{t: at, ticks: make(chan time.Time), wake: make(chan time.Time, 1),
		armed: make(chan time.Duration, 8), runs: make(chan time.Time, 8)}
}

func (f *fakeWorkerClock) set(at time.Time) { f.mu.Lock(); f.t = at; f.mu.Unlock() }
func (f *fakeWorkerClock) now() time.Time   { f.mu.Lock(); defer f.mu.Unlock(); return f.t }

func (f *fakeWorkerClock) clock() subscriptionWorkerClock {
	return subscriptionWorkerClock{
		now:   f.now,
		ticks: f.ticks,
		after: func(d time.Duration) <-chan time.Time { f.armed <- d; return f.wake },
	}
}

func (f *fakeWorkerClock) run(_ context.Context, at time.Time) { f.runs <- at }

func expectArmed(t *testing.T, f *fakeWorkerClock, want time.Duration) {
	t.Helper()
	select {
	case d := <-f.armed:
		if d != want {
			t.Fatalf("wake armed for %s, want %s", d, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the wake was not armed (want %s)", want)
	}
}

func expectRun(t *testing.T, f *fakeWorkerClock, want time.Time) {
	t.Helper()
	select {
	case at := <-f.runs:
		if !at.Equal(want) {
			t.Fatalf("tick ran for %s, want %s", at.In(istZone), want.In(istZone))
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no tick ran (want %s)", want.In(istZone))
	}
}

func TestSubscriptionWorkerWakesAtTheLock(t *testing.T) {
	const D = "2026-10-06"
	boot := istDayAt(D, 11, 59)
	f := newFakeWorkerClock(boot)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { runSubscriptionWorker(ctx, f.clock(), f.run); close(stopped) }()

	// Boot: the wake is armed for 12:00:05 from the boot clock, then one tick
	// runs at once.
	expectArmed(t, f, 65*time.Second)
	expectRun(t, f, boot)

	// 12:00:05: the wake fires, the lock tick runs, and the next wake is the
	// same moment tomorrow.
	lock := istDayAt(D, 12, 0).Add(5 * time.Second)
	f.set(lock)
	f.wake <- lock
	expectRun(t, f, lock)
	expectArmed(t, f, 24*time.Hour)

	// The 15-minute ticker keeps running beside it.
	tick := istDayAt(D, 12, 14)
	f.ticks <- tick
	expectRun(t, f, tick)

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatalf("the worker did not stop on cancel")
	}
}

// A boot after the cut-off is itself the lock tick (the lock decides on the
// wallet at 12:00 whenever it runs), and the wake waits for tomorrow's noon.
func TestSubscriptionWorkerBootAfterNoonLocksAtOnce(t *testing.T) {
	boot := istDayAt("2026-10-31", 12, 7)
	f := newFakeWorkerClock(boot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runSubscriptionWorker(ctx, f.clock(), f.run)
	expectArmed(t, f, istDayAt("2026-11-01", 12, 0).Add(5*time.Second).Sub(boot))
	expectRun(t, f, boot)
}
