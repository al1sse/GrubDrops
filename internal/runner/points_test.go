package runner

import (
	"context"
	"errors"
	"github.com/aalejandrofer/grubdrops/internal/platform"
	"github.com/aalejandrofer/grubdrops/internal/platform/platformtest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestZeroGameListForceWatchIsSafe(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	r := newRunner(f, func(c *Config) {
		c.Games = nil
		forceCfg(f, "a")(c)
	})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	forceOn(t, r, "a")
	if hasCall(f, "List") {
		t.Fatalf("zero game runner scanned drops: %v", f.Calls)
	}
}

func TestPointsOnlyDoesNotMineDrops(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f, Games: rustGames, PointsOnly: true,
		ForceChannels: func() []string { return []string{"a"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.State() != StateForceWatching {
		t.Fatalf("points-only runner with games state=%s", r.State())
	}
	for _, call := range f.Calls {
		if call == "List" || call == "Inventory" || strings.HasPrefix(call, "Claim:") {
			t.Fatalf("points-only runner mined drops: %v", f.Calls)
		}
	}
	// Due recheck: it must still not scan for drops.
	r.mu.Lock()
	r.forceCheckAt = time.Now().Add(-forcePointsScanInterval)
	r.mu.Unlock()
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.Calls {
		if call == "List" || call == "Inventory" || strings.HasPrefix(call, "Claim:") {
			t.Fatalf("points-only heartbeat mined drops: %v", f.Calls)
		}
	}
	if f.CallCount("Lookup:a") < 2 {
		t.Fatalf("force recheck did not run: %v", f.Calls)
	}
}
func TestSafetyChangedOnlyWakesSleepingRunner(t *testing.T) {
	f := platformtest.NewFake()
	r := newRunner(f, forceCfg(f, "a"))
	setLive(f, "a", true)
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	forceOn(t, r, "a")
	starts, stops := f.CallCount("Start:a"), f.CallCount("Stop:a")
	for _, st := range []State{StateForceWatching, StateWatching} {
		r.mu.Lock()
		r.state = st
		r.mu.Unlock()
		r.SafetyChanged()
		if r.forceScan.Load() || len(r.rescan) != 0 {
			t.Fatalf("%s runner was rescanned by a safety change", st)
		}
	}
	if f.CallCount("Start:a") != starts || f.CallCount("Stop:a") != stops {
		t.Fatalf("safety change restarted the force watch: %v", f.Calls)
	}
	r.mu.Lock()
	r.state = StateSleeping
	r.mu.Unlock()
	r.SafetyChanged()
	select {
	case <-r.rescan:
	default:
		t.Fatal("sleeping runner was not woken")
	}
}

func TestOneSlotRecheckLooksUpNoLowerChannel(t *testing.T) {
	f := platformtest.NewFake()
	for _, name := range []string{"a", "b", "c"} {
		setLive(f, name, true)
	}
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return 1 },
		ForceChannels: func() []string { return []string{"a", "b", "c"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.reconcileExtraForce(context.Background(), "a", []string{"a", "b", "c"})
	if n := f.CallCount("Lookup:b") + f.CallCount("Lookup:c"); n != 0 {
		t.Fatalf("one-slot recheck looked up %d lower channels: %v", n, f.Calls)
	}
}

func TestPrimaryStreamDownStopsEveryExtraSlot(t *testing.T) {
	f := platformtest.NewFake()
	for _, name := range []string{"a", "b", "c"} {
		setLive(f, name, true)
	}
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return 3 },
		ForceChannels: func() []string { return []string{"a", "b", "c"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.onStreamDown("id-a")
	for _, channel := range []string{"a", "b", "c"} {
		if !hasCall(f, "Stop:"+channel) {
			t.Fatalf("stream-down leaked %s: %v", channel, f.Calls)
		}
	}
}
func TestExtraSlotRetriesNoMoreOftenThanFiveMinutes(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	setLive(f, "b", false)
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return 3 },
		ForceChannels: func() []string { return []string{"a", "b"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !hasCall(f, "Start:a") || hasCall(f, "Start:b") {
		t.Fatalf("initial fill did not skip offline slot: %v", f.Calls)
	}
	r.mu.Lock()
	retryAt := r.extraRetryAt
	r.mu.Unlock()
	if left := time.Until(retryAt); left <= 0 || left > recheckInterval {
		t.Fatalf("extra-slot retry deadline in %v, want within %v", left, recheckInterval)
	}
	setLive(f, "b", true)
	r.startExtraForce(context.Background(), "a")
	if hasCall(f, "Start:b") {
		t.Fatalf("retried extra slot before five-minute deadline: %v", f.Calls)
	}
	r.mu.Lock()
	r.extraRetryAt = time.Now().Add(-time.Second)
	r.mu.Unlock()
	r.startExtraForce(context.Background(), "a")
	if !hasCall(f, "Start:b") {
		t.Fatalf("did not fill slot after retry deadline: %v", f.Calls)
	}
}

func TestPointsForceStreamSlotsAndTeardown(t *testing.T) {
	for _, tc := range []struct{ slots, want int }{{1, 1}, {3, 3}} {
		t.Run(string(rune('0'+tc.slots)), func(t *testing.T) {
			f := platformtest.NewFake()
			for _, name := range []string{"a", "b", "c", "d"} {
				setLive(f, name, true)
			}
			r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
				PointsStreams: func() int { return tc.slots },
				ForceChannels: func() []string { return []string{"a", "b", "c", "d"} },
				ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
					return f.LookupChannel(ctx, platform.Session{}, login)
				}})
			if err := r.pick(context.Background()); err != nil {
				t.Fatal(err)
			}
			starts := 0
			for _, call := range f.Calls {
				if strings.HasPrefix(call, "Start:") {
					starts++
				}
			}
			if starts != tc.want {
				t.Fatalf("started %d watches, want %d: %v", starts, tc.want, f.Calls)
			}
			if tc.slots == 3 {
				r.onStreamDown("id-b")
				if !hasCall(f, "Stop:b") || len(r.Snapshot().ExtraChannels) != 1 {
					t.Fatalf("extra stream-down did not tear down only that slot: state=%+v calls=%v", r.Snapshot(), f.Calls)
				}
			}
			r.stopWatch(context.Background())
			for _, name := range []string{"a", "b", "c"} {
				if tc.slots == 1 && name != "a" {
					continue
				}
				if !hasCall(f, "Stop:"+name) {
					t.Fatalf("watch %s leaked on teardown: %v", name, f.Calls)
				}
			}
		})
	}
}

func TestRaidJoinHonorsLiveToggleAndActiveSource(t *testing.T) {
	oldDelay := raidJoinDelay
	raidJoinDelay = func() time.Duration { return 0 }
	defer func() { raidJoinDelay = oldDelay }()
	f := &raidFake{Fake: platformtest.NewFake(), joined: make(chan string, 2)}
	var follow atomic.Bool
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f, FollowRaids: follow.Load})
	r.mu.Lock()
	r.runCtx = context.Background()
	r.state = StateForceWatching
	h := platform.WatchHandle{Channel: "primary"}
	r.handle = &h
	r.stream = &platform.Stream{ChannelID: "source"}
	r.mu.Unlock()
	r.onRaid("other", "inactive")
	follow.Store(true)
	r.onRaid("source", "enabled")
	select {
	case id := <-f.joined:
		if id != "enabled" {
			t.Fatalf("joined raid %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("enabled raid was not joined")
	}
}

func TestRepeatedRaidUpdatesJoinOncePerRaid(t *testing.T) {
	oldDelay := raidJoinDelay
	var spawned atomic.Int32
	release := make(chan struct{})
	raidJoinDelay = func() time.Duration {
		spawned.Add(1)
		<-release // every join goroutine stays pending while the updates arrive
		return 0
	}
	defer func() { raidJoinDelay = oldDelay }()
	f := &raidFake{Fake: platformtest.NewFake(), joined: make(chan string, 8)}
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f, FollowRaids: func() bool { return true }})
	r.mu.Lock()
	r.runCtx = context.Background()
	r.state = StateForceWatching
	h := platform.WatchHandle{Channel: "primary"}
	r.handle = &h
	r.stream = &platform.Stream{ChannelID: "source"}
	r.mu.Unlock()
	// Twitch repeats raid_update_v2 through the countdown; the last call is a
	// new raid that must join again.
	for _, id := range []string{"r1", "r1", "r2", "r2", "r3"} {
		r.onRaid("source", id)
	}
	if !waitFor(func() bool { return spawned.Load() >= 3 }, time.Second) {
		t.Fatalf("only %d join goroutines started", spawned.Load())
	}
	close(release)
	var joined []string
	for range 3 {
		select {
		case id := <-f.joined:
			joined = append(joined, id)
		case <-time.After(time.Second):
			t.Fatalf("joined only %v", joined)
		}
	}
	slices.Sort(joined)
	if !slices.Equal(joined, []string{"r1", "r2", "r3"}) {
		t.Fatalf("joined %v, want each raid once", joined)
	}
	// A duplicate goroutine would have entered the delay hook by now.
	if n := spawned.Load(); n != 3 || len(f.joined) != 0 {
		t.Fatalf("join goroutines=%d extra joins=%d, want 3 and 0", n, len(f.joined))
	}
	r.stopWatch(context.Background())
	r.mu.Lock()
	remembered := len(r.raidSeen)
	r.mu.Unlock()
	if remembered != 0 {
		t.Fatalf("raid memory kept after the watch stopped: %d", remembered)
	}
}

func TestFailedRaidJoinIsRetriedOnTheNextUpdate(t *testing.T) {
	oldDelay := raidJoinDelay
	raidJoinDelay = func() time.Duration { return 0 }
	defer func() { raidJoinDelay = oldDelay }()
	f := &raidFake{Fake: platformtest.NewFake(), joined: make(chan string, 4)}
	f.failNext.Store(true)
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f, FollowRaids: func() bool { return true }})
	r.mu.Lock()
	r.runCtx = context.Background()
	r.state = StateForceWatching
	h := platform.WatchHandle{Channel: "primary"}
	r.handle = &h
	r.stream = &platform.Stream{ChannelID: "source"}
	r.mu.Unlock()
	awaitJoin := func() {
		t.Helper()
		select {
		case id := <-f.joined:
			if id != "r1" {
				t.Fatalf("joined %q", id)
			}
		case <-time.After(time.Second):
			t.Fatal("raid was not joined")
		}
	}
	r.onRaid("source", "r1")
	awaitJoin() // fails
	if !waitFor(func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.raidSeen) == 0
	}, time.Second) {
		t.Fatal("failed join kept the raid marked as handled")
	}
	r.onRaid("source", "r1")
	awaitJoin() // succeeds
	r.mu.Lock()
	seen := r.raidSeen["source"]
	r.mu.Unlock()
	if seen != "r1" {
		t.Fatalf("successful join forgot the raid: %q", seen)
	}
}

func TestRaidJoinIsCanceledWhenSourceStopsDuringDelay(t *testing.T) {
	oldDelay := raidJoinDelay
	delayStarted := make(chan struct{})
	release := make(chan struct{})
	raidJoinDelay = func() time.Duration {
		close(delayStarted)
		<-release // hold the join until the source has stopped
		return 0
	}
	defer func() { raidJoinDelay = oldDelay }()
	f := &raidFake{Fake: platformtest.NewFake(), joined: make(chan string, 1)}
	var follows atomic.Int32
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		FollowRaids: func() bool {
			follows.Add(1)
			return true
		}})
	r.mu.Lock()
	r.runCtx = context.Background()
	r.state = StateForceWatching
	h := platform.WatchHandle{Channel: "primary"}
	r.handle = &h
	r.extraWatches = []extraWatch{{handle: platform.WatchHandle{Channel: "extra"}, stream: platform.Stream{ChannelID: "extra-id"}}}
	r.extraChannels = []string{"extra"}
	r.mu.Unlock()
	r.onRaid("extra-id", "stale")
	<-delayStarted
	r.mu.Lock()
	r.extraWatches = nil
	r.extraChannels = nil
	r.mu.Unlock()
	close(release)
	// The join goroutine re-reads FollowRaids right after its delay, then rechecks the source.
	if !waitFor(func() bool { return follows.Load() >= 2 }, time.Second) {
		t.Fatal("join goroutine never resumed after its delay")
	}
	select {
	case id := <-f.joined:
		t.Fatalf("joined stale raid %q after source stopped", id)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestForceExtraStartRacingTeardownIsStoppedAndUnsubscribed(t *testing.T) {
	f := &extraRunnerFake{Fake: platformtest.NewFake(), entered: make(chan struct{}), release: make(chan struct{})}
	setLive(f.Fake, "a", true)
	setLive(f.Fake, "b", true)
	slots := 1
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return slots },
		ForceChannels: func() []string { return []string{"a", "b"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	slots = 2
	started := make(chan struct{})
	go func() {
		r.startExtraForce(context.Background(), "a")
		close(started)
	}()
	<-f.entered
	r.stopForceWatch(context.Background())
	slots = 1
	stream, live, err := r.cfg.ForceLookup(context.Background(), "a")
	if err != nil || !live {
		t.Fatalf("primary lookup: live=%v err=%v", live, err)
	}
	if err := r.beginForce(context.Background(), "a", stream); err != nil {
		t.Fatal(err)
	}
	slots = 2
	close(f.release)
	<-started
	r.stopWatch(context.Background())
	if !hasCall(f.Fake, "Stop:b") {
		t.Fatalf("extra watch returned during teardown was not stopped: %v", f.Calls)
	}
	f.Mu.Lock()
	unsubscribed := contains(f.unsubscribed, "id-b")
	f.Mu.Unlock()
	if !unsubscribed {
		t.Fatal("extra channel subscription was not removed")
	}
}
func TestForceListChangeRejectsExtraAlreadyStarting(t *testing.T) {
	f := &extraRunnerFake{Fake: platformtest.NewFake(), entered: make(chan struct{}), release: make(chan struct{})}
	for _, name := range []string{"a", "b", "c"} {
		setLive(f.Fake, name, true)
	}
	channels := []string{"a", "b"}
	slots := 1
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return slots },
		ForceChannels: func() []string { return channels },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	slots = 2
	started := make(chan struct{})
	go func() {
		r.startExtraForce(context.Background(), "a")
		close(started)
	}()
	<-f.entered
	channels = []string{"a", "c"}
	r.mu.Lock()
	r.extraRetryAt = time.Now().Add(recheckInterval)
	r.mu.Unlock()
	r.ForceListChanged()
	r.mu.Lock()
	retryAt := r.extraRetryAt
	r.mu.Unlock()
	if !retryAt.IsZero() {
		t.Fatalf("force-list edit retained extra cooldown: %v", retryAt)
	}
	close(f.release)
	<-started
	if got := strings.Join(r.Snapshot().ExtraChannels, ","); got != "" {
		t.Fatalf("stale in-flight extra accepted after force-list change: %q", got)
	}
	if !hasCall(f.Fake, "Stop:b") {
		t.Fatalf("stale in-flight extra was not stopped: %v", f.Calls)
	}
	f.Mu.Lock()
	unsubscribed := contains(f.unsubscribed, "id-b")
	f.Mu.Unlock()
	if !unsubscribed {
		t.Fatal("stale in-flight extra was not unsubscribed")
	}
}

func TestForceRecheckReconcilesRankedExtrasAndSlots(t *testing.T) {
	f := platformtest.NewFake()
	for _, name := range []string{"a", "b", "c", "d"} {
		setLive(f, name, true)
	}
	channels := []string{"a", "b", "c"}
	slots := 3
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return slots },
		ForceChannels: func() []string { return channels },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	channels = []string{"a", "c", "d"}
	if err := r.forceRecheck(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Snapshot().ExtraChannels, ","); got != "c,d" {
		t.Fatalf("extras after ranked assignment change = %q, want c,d", got)
	}
	if !hasCall(f, "Stop:b") || !hasCall(f, "Start:d") {
		t.Fatalf("stale extra was not replaced: %v", f.Calls)
	}
	slots = 2
	if err := r.forceRecheck(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Snapshot().ExtraChannels, ","); got != "c" {
		t.Fatalf("extras after slot reduction = %q, want c", got)
	}
	if !hasCall(f, "Stop:d") {
		t.Fatalf("excess extra was not stopped: %v", f.Calls)
	}
}
func TestExtraFillSkipsOfflineHigherRankedChannel(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	setLive(f, "b", false)
	setLive(f, "c", true)
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return 2 },
		ForceChannels: func() []string { return []string{"a", "b", "c"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Snapshot().ExtraChannels, ","); got != "c" {
		t.Fatalf("extra after offline higher-ranked channel = %q, want c", got)
	}
	starts := f.CallCount("Start:c")
	r.startExtraForce(context.Background(), "a")
	if got := f.CallCount("Start:c"); got != starts || hasCall(f, "Stop:c") {
		t.Fatalf("ordinary heartbeat restarted healthy fallback c: starts %d -> %d calls %v", starts, got, f.Calls)
	}
}

func TestExtraFillSkipsFailedHigherRankedChannel(t *testing.T) {
	f := platformtest.NewFake()
	for _, name := range []string{"a", "b", "c"} {
		setLive(f, name, true)
	}
	f.StartErrs = map[string]error{"b": errors.New("scripted start failure")}
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return 2 },
		ForceChannels: func() []string { return []string{"a", "b", "c"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Snapshot().ExtraChannels, ","); got != "c" {
		t.Fatalf("extra after failed higher-ranked channel = %q, want c", got)
	}
	if !hasCall(f, "Start:b") || !hasCall(f, "Start:c") {
		t.Fatalf("did not fall through failed candidate: %v", f.Calls)
	}
}

func TestExtraCooldownStillReconcilesReducedSlotCount(t *testing.T) {
	f := platformtest.NewFake()
	for _, name := range []string{"a", "b", "c"} {
		setLive(f, name, true)
	}
	slots := 3
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return slots },
		ForceChannels: func() []string { return []string{"a", "b", "c"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	slots = 2
	r.mu.Lock()
	r.extraRetryAt = time.Now().Add(recheckInterval)
	r.mu.Unlock()
	r.startExtraForce(context.Background(), "a")
	if got := strings.Join(r.Snapshot().ExtraChannels, ","); got != "b" {
		t.Fatalf("extras after slot reduction during cooldown = %q, want b", got)
	}
	if !hasCall(f, "Stop:c") {
		t.Fatalf("excess extra was not stopped during cooldown: %v", f.Calls)
	}
}

func TestVeryLargePointsStreamCountDoesNotPanic(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	setLive(f, "b", true)
	slots := 1
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return slots },
		ForceChannels: func() []string { return []string{"a", "b"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	slots = int(^uint(0) >> 1)
	r.reconcileExtraForce(context.Background(), "a", []string{"a", "b"})
	if got := r.Snapshot().ExtraChannels; len(got) != 1 || got[0] != "b" {
		t.Fatalf("extras with very large slot count = %v, want [b]", got)
	}
}

func TestFullExtraSlotsDoNotReadForceListOnHeartbeat(t *testing.T) {
	f := platformtest.NewFake()
	setLive(f, "a", true)
	setLive(f, "b", true)
	var reads atomic.Int32
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return 2 },
		ForceChannels: func() []string { reads.Add(1); return []string{"a", "b"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.forceCheckAt = time.Now()
	r.mu.Unlock()
	before := reads.Load()
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got != before {
		t.Fatalf("full-slot heartbeat read force list %d times, want no additional reads", got-before)
	}
}

func TestExtraHeartbeatFailureOnlyStopsThatExtra(t *testing.T) {
	f := &extraRunnerFake{Fake: platformtest.NewFake()}
	setLive(f.Fake, "a", true)
	setLive(f.Fake, "b", true)
	r := New(Config{AccountID: "acc", Platform: "twitch", Backend: f,
		PointsStreams: func() int { return 2 },
		ForceChannels: func() []string { return []string{"a", "b"} },
		ForceLookup: func(ctx context.Context, login string) (platform.Stream, bool, error) {
			return f.LookupChannel(ctx, platform.Session{}, login)
		}})
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.failHeartbeat = "b"
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatalf("extra heartbeat failure escaped force tick: %v", err)
	}
	if r.State() != StateForceWatching {
		t.Fatalf("extra heartbeat failure changed primary state to %s", r.State())
	}
	if !hasCall(f.Fake, "Stop:b") || hasCall(f.Fake, "Stop:a") {
		t.Fatalf("extra heartbeat failure stopped wrong watches: %v", f.Calls)
	}
	r.mu.Lock()
	retryAt := r.extraRetryAt
	r.mu.Unlock()
	if time.Until(retryAt) <= 0 || time.Until(retryAt) > recheckInterval {
		t.Fatalf("failed extra cooldown deadline %v, want five minutes", retryAt)
	}
}

type extraRunnerFake struct {
	*platformtest.Fake
	entered       chan struct{}
	release       chan struct{}
	failHeartbeat string
	muSubs        sync.Mutex
	unsubscribed  []string
}

func (f *extraRunnerFake) StartWatch(ctx context.Context, session platform.Session, stream platform.Stream) (platform.WatchHandle, error) {
	if stream.Channel == "b" && f.entered != nil {
		close(f.entered)
		<-f.release
	}
	return f.Fake.StartWatch(ctx, session, stream)
}

func (f *extraRunnerFake) Heartbeat(ctx context.Context, handle platform.WatchHandle) error {
	if handle.Channel == f.failHeartbeat {
		return errors.New("scripted heartbeat failure")
	}
	return f.Fake.Heartbeat(ctx, handle)
}

func (f *extraRunnerFake) SubscribeChannel(_, _ string) {}

func (f *extraRunnerFake) UnsubscribeChannel(_, channelID string) {
	f.muSubs.Lock()
	f.unsubscribed = append(f.unsubscribed, channelID)
	f.muSubs.Unlock()
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

type raidFake struct {
	*platformtest.Fake
	joined   chan string
	failNext atomic.Bool // the next JoinRaid is recorded, then fails
}

func (f *raidFake) JoinRaid(_ context.Context, _ platform.Session, raidID string) error {
	f.joined <- raidID
	if f.failNext.CompareAndSwap(true, false) {
		return errors.New("scripted join failure")
	}
	return nil
}

// claimFake records ClaimChannelPoints calls; err and claimed script the result.
type claimFake struct {
	*platformtest.Fake
	mu      sync.Mutex
	claims  []string
	claimed bool
	err     error
	// bonus records channelID/claimID of ClaimChannelPointsBonus calls;
	// bonusDone, when set, receives each claimID after the call is recorded.
	bonus     []string
	bonusErr  error
	bonusDone chan string
}

func (f *claimFake) ClaimChannelPointsBonus(_ context.Context, _ platform.Session, channelID, claimID string) error {
	f.mu.Lock()
	f.bonus = append(f.bonus, channelID+"/"+claimID)
	err := f.bonusErr
	f.mu.Unlock()
	if f.bonusDone != nil {
		f.bonusDone <- claimID
	}
	return err
}

func (f *claimFake) bonusSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bonus...)
}

func (f *claimFake) ClaimChannelPoints(_ context.Context, _ platform.Session, channel string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, channel)
	return f.claimed, f.err
}

func (f *claimFake) claimsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.claims...)
}

// claimRunner is a twitch runner force watching "a" (and "b" with two slots).
func claimRunner(t *testing.T, f *claimFake, enabled bool, slots int) *Runner {
	t.Helper()
	setLive(f.Fake, "a", true)
	setLive(f.Fake, "b", true)
	cfg := Config{AccountID: "acc", Platform: "twitch", Backend: f, PointsEnabled: enabled,
		PointsStreams: func() int { return slots }}
	forceCfg(f.Fake, "a", "b")(&cfg)
	r := New(cfg)
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	forceOn(t, r, "a")
	return r
}

func TestPointsClaimThrottledPerChannel(t *testing.T) {
	f := &claimFake{Fake: platformtest.NewFake(), claimed: true}
	r := claimRunner(t, f, true, 1)
	for range 2 {
		if err := r.forceTick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.claimsSnapshot(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("claims within the fallback window = %v, want one on the first beat", got)
	}
	r.mu.Lock()
	r.pointsClaimAt["a"] = time.Now().Add(-pointsClaimEvery - time.Second)
	r.mu.Unlock()
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.claimsSnapshot(); !slices.Equal(got, []string{"a", "a"}) {
		t.Fatalf("claims after the throttle expired = %v", got)
	}
	// A watch restarted by error backoff must not claim again within the window.
	r.stopWatch(context.Background())
	if err := r.pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	forceOn(t, r, "a")
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.claimsSnapshot(); !slices.Equal(got, []string{"a", "a"}) {
		t.Fatalf("claims after a restart inside the window = %v", got)
	}
	// Expired entries are dropped when another channel claims.
	r.mu.Lock()
	r.pointsClaimAt["old"] = time.Now().Add(-pointsClaimEvery - time.Second)
	r.pointsClaimAt["a"] = time.Now().Add(-pointsClaimEvery - time.Second)
	r.mu.Unlock()
	r.claimPoints(context.Background(), "b")
	r.mu.Lock()
	_, oldKept := r.pointsClaimAt["old"]
	n := len(r.pointsClaimAt)
	r.mu.Unlock()
	if oldKept || n != 1 {
		t.Fatalf("claim throttle map not bounded: oldKept=%t len=%d", oldKept, n)
	}
}

func TestPointsClaimSkippedWhenPointsDisabled(t *testing.T) {
	f := &claimFake{Fake: platformtest.NewFake(), claimed: true}
	r := claimRunner(t, f, false, 1)
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.claimsSnapshot(); len(got) != 0 {
		t.Fatalf("claimed with points disabled: %v", got)
	}
}

func TestPointsClaimErrorDoesNotFailBeat(t *testing.T) {
	f := &claimFake{Fake: platformtest.NewFake(), err: errors.New("gql down")}
	r := claimRunner(t, f, true, 1)
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatalf("claim error failed the beat: %v", err)
	}
	if r.State() != StateForceWatching || len(f.claimsSnapshot()) != 1 {
		t.Fatalf("state=%s claims=%v", r.State(), f.claimsSnapshot())
	}
}

func TestPointsClaimCoversExtraStreams(t *testing.T) {
	f := &claimFake{Fake: platformtest.NewFake()}
	r := claimRunner(t, f, true, 2)
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.claimsSnapshot(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("claims = %v, want the primary then the extra stream", got)
	}
}

// bonusRunner is claimRunner with a runCtx, as during Run.
func bonusRunner(t *testing.T, f *claimFake, enabled bool, slots int) *Runner {
	t.Helper()
	f.bonusDone = make(chan string, 8)
	r := claimRunner(t, f, enabled, slots)
	r.mu.Lock()
	r.runCtx = context.Background()
	r.mu.Unlock()
	return r
}

func awaitBonus(t *testing.T, f *claimFake, want string) {
	t.Helper()
	select {
	case got := <-f.bonusDone:
		if got != want {
			t.Fatalf("bonus claim %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no bonus claim for %q", want)
	}
}

func TestPointsClaimEventClaimsOnceAndDelaysFallback(t *testing.T) {
	f := &claimFake{Fake: platformtest.NewFake(), claimed: true}
	r := bonusRunner(t, f, true, 2)
	r.onPointsClaim("id-a", "c1")
	awaitBonus(t, f, "c1")
	r.onPointsClaim("id-a", "c1") // repeated event
	r.onPointsClaim("id-b", "c2") // extra stream
	awaitBonus(t, f, "c2")
	if got := f.bonusSnapshot(); !slices.Equal(got, []string{"id-a/c1", "id-b/c2"}) {
		t.Fatalf("bonus claims = %v", got)
	}
	// The success stamp lands after the call returns.
	if !waitFor(func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.pointsClaimAt["a"].IsZero() && !r.pointsClaimAt["b"].IsZero()
	}, 5*time.Second) {
		t.Fatal("event claim did not delay the fallback poll")
	}
	if err := r.forceTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.claimsSnapshot(); len(got) != 0 {
		t.Fatalf("fallback polled right after an event claim: %v", got)
	}
}

func TestPointsClaimEventIgnored(t *testing.T) {
	f := &claimFake{Fake: platformtest.NewFake()}
	r := bonusRunner(t, f, true, 1)
	r.onPointsClaim("id-b", "c1") // not watched with one slot
	r.onPointsClaim("", "c2")
	r.onPointsClaim("id-a", "")
	off := bonusRunner(t, &claimFake{Fake: platformtest.NewFake()}, false, 1)
	off.onPointsClaim("id-a", "c3")
	r.mu.Lock()
	r.runCtx = nil // not running
	r.mu.Unlock()
	r.onPointsClaim("id-a", "c4")
	for _, rr := range []*Runner{r, off} {
		rr.mu.Lock()
		n := len(rr.pointsClaimSeen)
		rr.mu.Unlock()
		if n != 0 {
			t.Fatalf("event was accepted: %v", rr.pointsClaimSeen)
		}
	}
	// Control: the same runner does claim a watched channel.
	r.mu.Lock()
	r.runCtx = context.Background()
	r.mu.Unlock()
	r.onPointsClaim("id-a", "c5")
	awaitBonus(t, f, "c5")
	if got := f.bonusSnapshot(); !slices.Equal(got, []string{"id-a/c5"}) {
		t.Fatalf("bonus claims = %v", got)
	}
}

func TestPointsClaimEventErrorRetries(t *testing.T) {
	f := &claimFake{Fake: platformtest.NewFake(), bonusErr: errors.New("gql down")}
	r := bonusRunner(t, f, true, 1)
	r.onPointsClaim("id-a", "c1")
	awaitBonus(t, f, "c1")
	if !waitFor(func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.pointsClaimSeen) == 0
	}, 5*time.Second) {
		t.Fatal("failed claim ID was not forgotten")
	}
	r.mu.Lock()
	stamped := len(r.pointsClaimAt)
	r.mu.Unlock()
	if stamped != 0 {
		t.Fatal("failed claim delayed the fallback poll")
	}
	f.mu.Lock()
	f.bonusErr = nil
	f.mu.Unlock()
	r.onPointsClaim("id-a", "c1")
	awaitBonus(t, f, "c1")
	if got := f.bonusSnapshot(); len(got) != 2 {
		t.Fatalf("bonus claims = %v, want the retry", got)
	}
}
