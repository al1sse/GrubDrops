package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aalejandrofer/grubdrops/internal/plan"
	"github.com/aalejandrofer/grubdrops/internal/platform"
	"github.com/aalejandrofer/grubdrops/internal/platform/platformtest"
)

func TestPickClearsExpiredSleepDeadlineBeforeSelectingDrop(t *testing.T) {
	r := newRunner(platformtest.NewFake(), nil)
	target := plan.Target{
		Campaign: platform.Campaign{ID: "campaign", EndsAt: time.Now().Add(time.Hour)},
		Benefit:  platform.DropBenefit{ID: "drop"},
	}
	r.pinned = &target
	r.sleepUntil = time.Now().Add(-time.Second)

	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.State() != StateStarting {
		t.Fatalf("selected drop state = %s, want %s", r.State(), StateStarting)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sleepUntil.IsZero() {
		t.Fatalf("expired scheduled sleep remains set: %v", r.sleepUntil)
	}
}

func asleepFor(d time.Duration) func(*Config) {
	return func(c *Config) {
		c.SleepUntil = func(now time.Time) (time.Time, bool) { return now.Add(d), true }
	}
}

func TestSleepingDropMinerStillRescansBeforeSleepEnds(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	var mu sync.Mutex
	var states []State
	r := newRunner(f, func(c *Config) {
		forceCfg(f, "a")(c)
		asleepFor(time.Hour)(c)
		c.ScanEvery = func() time.Duration { return 5 * time.Millisecond }
		c.OnChange = func(s Snapshot) {
			mu.Lock()
			states = append(states, s.State)
			mu.Unlock()
		}
	})
	defer runUntil(t, r)()
	if !waitFor(func() bool { return f.CallCount("List") >= 3 }, 5*time.Second) {
		t.Fatalf("sleeping drop miner stopped scanning; lists=%d", f.CallCount("List"))
	}
	if hasCall(f, "Start:a") {
		t.Fatal("force channel watched during the sleep")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range states {
		if s != StateSleeping {
			t.Fatalf("rescans during the sleep flapped through %s: %v", s, states)
		}
	}
}

func TestSleepingPointsOnlyRunnerWaitsOutTheSleep(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	r := newRunner(f, func(c *Config) {
		forceCfg(f, "a")(c)
		asleepFor(time.Hour)(c)
		c.PointsOnly = true
		c.ScanEvery = func() time.Duration { return 5 * time.Millisecond }
	})
	defer runUntil(t, r)()
	var armed time.Time
	if !waitFor(func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		armed = r.nextRecheck
		return !armed.IsZero()
	}, 5*time.Second) {
		t.Fatalf("no idle wait armed; state=%s", r.State())
	}
	if left := time.Until(armed); left < 50*time.Minute {
		t.Fatalf("points-only sleep wait = %v, want the rest of the sleep", left)
	}
}

func TestStartHonoursSleepBeforeForceFallback(t *testing.T) {
	f := platformtest.NewFake()
	f.Campaigns = []platform.Campaign{camp("c1", "Rust", "d1")} // queued, no live stream
	setLive(f, "a", true)
	r := newRunner(f, func(c *Config) {
		forceCfg(f, "a")(c)
		asleepFor(time.Hour)(c)
	})
	steps(t, r, 2) // pick the drop, start finds no stream
	if r.State() != StateSleeping {
		t.Fatalf("state=%s, want sleeping", r.State())
	}
	if hasCall(f, "Start:a") {
		t.Fatalf("force channel started during the sleep: %v", f.Calls)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sleepUntil.IsZero() || !r.sleepNoStream {
		t.Fatalf("sleepUntil=%v sleepNoStream=%t", r.sleepUntil, r.sleepNoStream)
	}
}

func TestAsleepRescanStaysSleepingWhenNoQueuedCampaignIsLive(t *testing.T) {
	f := platformtest.NewFake()
	f.Campaigns = []platform.Campaign{camp("c1", "Rust", "d1")} // queued, no live stream
	setLive(f, "a", true)
	var mu sync.Mutex
	var states []State
	r := newRunner(f, func(c *Config) {
		forceCfg(f, "a")(c)
		asleepFor(time.Hour)(c)
		c.OnChange = func(s Snapshot) {
			mu.Lock()
			states = append(states, s.State)
			mu.Unlock()
		}
	})
	r.mu.Lock()
	r.state = StateSleeping
	r.sleepUntil = time.Now().Add(time.Hour)
	r.mu.Unlock()
	for range 2 {
		if err := r.pick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	for _, s := range states {
		if s != StateSleeping {
			t.Fatalf("asleep rescans left Sleeping: %v", states)
		}
	}
	mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != StateSleeping || !r.sleepNoStream || r.sleepUntil.IsZero() {
		t.Fatalf("state=%s sleepNoStream=%t sleepUntil=%v", r.state, r.sleepNoStream, r.sleepUntil)
	}
}

func TestSleepingDropMinerStartsACampaignThatGoesLiveBeforeSleepEnds(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	r := newRunner(f, func(c *Config) {
		forceCfg(f, "a")(c)
		asleepFor(time.Hour)(c)
		c.TickInterval = 60 * time.Second
		c.ScanEvery = func() time.Duration { return 5 * time.Millisecond }
	})
	defer runUntil(t, r)()
	if !waitFor(func() bool { return f.CallCount("List") >= 2 }, 5*time.Second) {
		t.Fatalf("sleeping drop miner did not rescan; state=%s", r.State())
	}
	c := camp("c1", "Rust", "d1")
	c.Benefits[0].RequiredMinutes = 100000
	f.Mu.Lock()
	f.Campaigns = []platform.Campaign{c}
	f.Progress["d1"] = &platform.Progress{BenefitID: "d1"}
	f.Streams["c1"] = []platform.Stream{{Channel: "d", ViewerCount: 1}}
	f.Mu.Unlock()
	if !waitFor(func() bool { return hasCall(f, "Start:d") }, 5*time.Second) {
		t.Fatalf("campaign that went live was not watched; state=%s calls=%v", r.State(), f.Calls)
	}
	if hasCall(f, "Start:a") {
		t.Fatal("force channel watched during the sleep")
	}
}

func TestSleepEndsAtItsDeadlineAndForceWatchesAgain(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	deadline := time.Now().Add(150 * time.Millisecond)
	r := newRunner(f, func(c *Config) {
		forceCfg(f, "a")(c)
		c.SleepUntil = func(now time.Time) (time.Time, bool) {
			if now.Before(deadline) {
				return deadline, true
			}
			return time.Time{}, false
		}
	})
	defer runUntil(t, r)()
	if !waitFor(func() bool { return hasCall(f, "Start:a") }, 5*time.Second) {
		t.Fatalf("force channel not watched after the sleep; state=%s", r.State())
	}
	if time.Now().Before(deadline) {
		t.Fatal("force channel watched before the sleep ended")
	}
	if !waitFor(func() bool { return r.State() == StateForceWatching }, 5*time.Second) {
		t.Fatalf("state=%s, want force watching", r.State())
	}
}
