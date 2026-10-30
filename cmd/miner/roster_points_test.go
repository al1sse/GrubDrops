package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aalejandrofer/grubdrops/internal/platform"
	"github.com/aalejandrofer/grubdrops/internal/store"
)

func TestScheduledSleepJitteredBoundaries(t *testing.T) {
	loc := time.FixedZone("local", -7*60*60)
	midnight := time.Date(2026, time.July, 12, 0, 0, 0, 0, loc)
	settings := store.PointsSafety{SleepEnabled: true, SleepStart: 120, SleepEnd: 480}
	start := midnight.Add(2*time.Hour + sleepJitter("acc", midnight, "start"))
	end := midnight.Add(8*time.Hour + sleepJitter("acc", midnight, "end"))
	for _, jitter := range []time.Duration{sleepJitter("acc", midnight, "start"), sleepJitter("acc", midnight, "end")} {
		if jitter < -20*time.Minute || jitter > 20*time.Minute {
			t.Fatalf("jitter out of range: %v", jitter)
		}
	}
	if until, asleep := scheduledSleep(start, "acc", settings, loc); !asleep || !until.Equal(end) {
		t.Fatalf("sleep start boundary: asleep=%t until=%v, want %v", asleep, until, end)
	}
	if _, asleep := scheduledSleep(end, "acc", settings, loc); asleep {
		t.Fatal("sleep end boundary must be awake")
	}
	if sleepJitter("acc", midnight, "start") != sleepJitter("acc", midnight, "start") {
		t.Fatal("daily jitter is not stable")
	}
}
func TestRunnerConfigGatesForceChannelsByPointsSetting(t *testing.T) {
	e := newRosterEnv(t)
	notifier := pointsNotifier{}
	e.ro.notifier = notifier
	e.withForce()
	a := e.account(t, "a1", 1, true)
	e.setForce(t, []store.ForceChannel{{Platform: "kick", Channel: "xqc", Accounts: []string{"a1"}}})
	require.NoError(t, store.SavePointsAccountSettings(e.ctx, e.q, a.ID,
		store.PointsAccountSettings{Enabled: false, Streams: 3}))
	cfg := e.ro.runnerConfig(e.ctx, a, e.fake, platform.Session{}, nil)
	assert.Equal(t, notifier, cfg.Notifier)
	assert.Empty(t, cfg.ForceChannels(), "disabled points must suppress only this account's force channels")
	require.NoError(t, store.SavePointsAccountSettings(e.ctx, e.q, a.ID,
		store.PointsAccountSettings{Enabled: true, Only: true, Streams: 3}))
	cfg = e.ro.runnerConfig(e.ctx, a, e.fake, platform.Session{}, nil)
	assert.Equal(t, []string{"xqc"}, cfg.ForceChannels())
	assert.True(t, cfg.PointsOnly)
	assert.Equal(t, 3, cfg.PointsStreams())
}

func TestRunnerConfigClampsStreamsAndMirrorsPointsEnabled(t *testing.T) {
	e := newRosterEnv(t)
	a := e.account(t, "a1", 1, true)
	require.NoError(t, store.SavePointsAccountSettings(e.ctx, e.q, a.ID,
		store.PointsAccountSettings{Enabled: true, Streams: 100}))
	cfg := e.ro.runnerConfig(e.ctx, a, e.fake, platform.Session{}, nil)
	assert.True(t, cfg.PointsEnabled)
	assert.Equal(t, store.MaxPointsStreams, cfg.PointsStreams(), "a stored value above the cap is clamped")
	require.NoError(t, store.SavePointsAccountSettings(e.ctx, e.q, a.ID,
		store.PointsAccountSettings{Enabled: false, Streams: 2}))
	cfg = e.ro.runnerConfig(e.ctx, a, e.fake, platform.Session{}, nil)
	assert.False(t, cfg.PointsEnabled)
	assert.Equal(t, 2, cfg.PointsStreams())
}

func TestScheduledSleepAroundMidnight(t *testing.T) {
	loc := time.FixedZone("local", 2*60*60)
	day := time.Date(2026, time.July, 12, 0, 0, 0, 0, loc)
	next := day.AddDate(0, 0, 1)
	nextEnd := func(acc string, hour int) time.Time {
		return next.Add(time.Duration(hour)*time.Hour + sleepJitter(acc, next, "end"))
	}

	// An account whose jitter pulls tomorrow's 00:10 start back before midnight.
	early := ""
	for i := 0; i < 1000 && early == ""; i++ {
		acc := fmt.Sprintf("acc%d", i)
		if sleepJitter(acc, next, "start") < -10*time.Minute {
			early = acc
		}
	}
	require.NotEmpty(t, early, "no account with a negative start jitter found")
	earlyStart := next.Add(10*time.Minute + sleepJitter(early, next, "start"))
	require.True(t, earlyStart.Before(next), "window must start the previous evening")

	shortSleep := store.PointsSafety{SleepEnabled: true, SleepStart: 10, SleepEnd: 480}
	wrapSleep := store.PointsSafety{SleepEnabled: true, SleepStart: 23*60 + 30, SleepEnd: 360}
	for _, tc := range []struct {
		name     string
		acc      string
		settings store.PointsSafety
		at       time.Time
		asleep   bool
		until    time.Time
	}{
		{"next-day window starts the previous evening", early, shortSleep, earlyStart, true, nextEnd(early, 8)},
		{"just before that start", early, shortSleep, earlyStart.Add(-time.Minute), false, time.Time{}},
		{"wrapping window at 23:50", "acc", wrapSleep, day.Add(23*time.Hour + 50*time.Minute), true, nextEnd("acc", 6)},
		{"wrapping window at 00:10", "acc", wrapSleep, next.Add(10 * time.Minute), true, nextEnd("acc", 6)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			until, asleep := scheduledSleep(tc.at, tc.acc, tc.settings, loc)
			assert.Equal(t, tc.asleep, asleep)
			assert.True(t, until.Equal(tc.until), "until=%v want %v", until, tc.until)
		})
	}
}
func TestRunnerSleepUsesLiveConfiguredTimezone(t *testing.T) {
	e := newRosterEnv(t)
	a := e.account(t, "a1", 1, true)
	now := time.Date(2026, time.July, 12, 6, 0, 0, 0, time.UTC)
	cfg := e.ro.runnerConfig(e.ctx, a, e.fake, platform.Session{}, nil)
	if _, asleep := cfg.SleepUntil(now); !asleep {
		t.Fatal("fallback UTC zone should place 06:00 UTC inside the sleep window")
	}
	loc := time.FixedZone("live", 3*60*60)
	e.ro.zone = func() *time.Location { return loc }
	cfg = e.ro.runnerConfig(e.ctx, a, e.fake, platform.Session{}, nil)
	if _, asleep := cfg.SleepUntil(now); asleep {
		t.Fatal("live UTC+3 zone should place 06:00 UTC outside the sleep window")
	}
}

type pointsNotifier struct{}

func (pointsNotifier) Notify(context.Context, string, map[string]any) error { return nil }
