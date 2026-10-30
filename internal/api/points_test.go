package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aalejandrofer/grubdrops/internal/platform"
	"github.com/aalejandrofer/grubdrops/internal/platform/platformtest"
	"github.com/aalejandrofer/grubdrops/internal/store"
	"github.com/aalejandrofer/grubdrops/internal/store/gen"
)

type balanceBackend struct {
	platform.Backend
	balance   int
	err       error
	accountID string
	calls     chan string
	done      chan struct{}
}

func (*balanceBackend) Name() string { return "twitch" }
func (b *balanceBackend) ChannelPointsBalance(_ context.Context, session platform.Session, channel string) (int, error) {
	b.accountID = session.AccountID
	b.calls <- channel
	defer close(b.done)
	return b.balance, b.err
}

type delayedBalanceRequest struct {
	token   string
	release chan struct{}
	done    chan struct{}
}

type delayedBalanceBackend struct {
	platform.Backend
	calls chan delayedBalanceRequest
}

func (*delayedBalanceBackend) Name() string { return "twitch" }
func (b *delayedBalanceBackend) ChannelPointsBalance(_ context.Context, session platform.Session, _ string) (int, error) {
	request := delayedBalanceRequest{token: session.AccessToken, release: make(chan struct{}), done: make(chan struct{})}
	b.calls <- request
	defer close(request.done)
	<-request.release
	if request.token == "old" {
		return 111, nil
	}
	return 222, nil
}

func TestPointsHandlersDefaultsValidationAndAccountSettings(t *testing.T) {
	e, _ := accountsEnv(t)
	id := e.createAccount("twitch", "T")
	_, token := e.session()

	resp, body := e.do("GET", "/api/points", "", nil)
	require.Equal(t, 200, resp.StatusCode, string(body))
	assert.JSONEq(t, `{"sleep_enabled":true,"sleep_start":"02:00","sleep_end":"08:00","follow_raids":true}`, string(body))
	resp, body = e.do("GET", "/api/accounts", "", nil)
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), `"points_enabled":true,"points_only":false,"points_streams":2`)

	for _, invalid := range []map[string]any{
		{"sleep_enabled": true, "sleep_start": "02:00", "sleep_end": "02:00", "follow_raids": true},
		{"sleep_enabled": true, "sleep_start": "24:00", "sleep_end": "08:00", "follow_raids": true},
		{"sleep_enabled": true, "sleep_start": "2:00", "sleep_end": "08:00", "follow_raids": true},
	} {
		resp, _ = e.do("PUT", "/api/points/safety", token, invalid)
		assert.Equal(t, 400, resp.StatusCode)
	}
	resp, body = e.do("PUT", "/api/points/safety", token, map[string]any{"sleep_enabled": false, "sleep_start": "22:30", "sleep_end": "06:30", "follow_raids": false})
	require.Equal(t, 200, resp.StatusCode, string(body))
	assert.JSONEq(t, `{"sleep_enabled":false,"sleep_start":"22:30","sleep_end":"06:30","follow_raids":false}`, string(body))

	resp, body = e.do("PUT", "/api/accounts/"+id+"/points", token, map[string]any{"points_enabled": false, "points_only": true, "points_streams": store.MaxPointsStreams})
	require.Equal(t, 204, resp.StatusCode, string(body))
	resp, body = e.do("GET", "/api/accounts", "", nil)
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), `"points_enabled":false,"points_only":true,"points_streams":3`)
	for _, invalid := range []map[string]any{
		{"points_enabled": true, "points_only": false},
		{"points_enabled": true, "points_only": false, "points_streams": 0},
		{"points_enabled": true, "points_only": false, "points_streams": -1},
		{"points_enabled": true, "points_only": false, "points_streams": store.MaxPointsStreams + 1},
		{"points_enabled": true, "points_only": false, "points_streams": 10000},
	} {
		resp, _ = e.do("PUT", "/api/accounts/"+id+"/points", token, invalid)
		assert.Equal(t, 400, resp.StatusCode)
	}
	resp, body = e.do("GET", "/api/accounts", "", nil)
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), `"points_enabled":false,"points_only":true,"points_streams":3`, "rejected requests write nothing")
	kickID := e.createAccount("kick", "K")
	resp, body = e.do("PUT", "/api/accounts/"+kickID+"/points", token, map[string]any{"points_enabled": true, "points_only": false})
	require.Equal(t, 204, resp.StatusCode, string(body))
	resp, body = e.do("GET", "/api/accounts", "", nil)
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), `"platform":"kick","name":"K","platform_name":"K","avatar_url":"","state":"idle","needs_login":true,"enabled":true,"own_games":false,"own_game_count":0,"points_enabled":true,"points_only":false,"points_streams":1`)
}

func TestTwitchAccountLimitCountsDisabledButAllowsRelogin(t *testing.T) {
	e, _ := accountsEnv(t, &androidBackend{Fake: platformtest.NewFake()})
	ids := []string{e.createAccount("twitch", "A"), e.createAccount("twitch", "B"), e.createAccount("twitch", "C")}
	_, token := e.session()
	resp, body := e.do("PUT", "/api/accounts/"+ids[0]+"/enabled", token, map[string]bool{"enabled": false})
	require.Equal(t, 204, resp.StatusCode, string(body))

	resp, body = e.do("POST", "/api/accounts", token, map[string]string{"platform": "twitch", "name": "D"})
	assert.Equal(t, 400, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "account_limit")

	resp, body = e.do("POST", "/api/login/twitch/android", token, map[string]string{"url": "other"})
	assert.Equal(t, 400, resp.StatusCode, string(body), "new Android-login accounts obey the cap")

	balance := 123
	e.s.pointsMu.Lock()
	e.s.pointsCache = map[string]pointBalanceEntry{ids[0] + "/channel": {balance: &balance}}
	e.s.pointsMu.Unlock()
	resp, body = e.do("POST", "/api/login/twitch/android", token, map[string]string{"url": "good", "account_id": ids[0]})
	require.Equal(t, 200, resp.StatusCode, string(body), "existing-account relogin remains allowed at the cap")
	assert.Equal(t, ids[0], jsonField(t, body, "id"))
	e.s.pointsMu.Lock()
	assert.NotContains(t, e.s.pointsCache, ids[0]+"/channel")
	e.s.pointsMu.Unlock()
}

func TestPointsBalanceRefreshIsAsyncAndFailureKeepsStaleValue(t *testing.T) {
	backend := &balanceBackend{balance: 321, calls: make(chan string, 2), done: make(chan struct{})}
	e, _ := accountsEnv(t, backend)
	id := e.createAccount("twitch", "T")
	require.NoError(t, e.s.d.Sessions.Put(context.Background(), id, platform.Session{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}))
	e.s.d.Hub = NewHub()
	events, cancel := e.s.d.Hub.Subscribe()
	defer cancel()

	first := e.s.pointsFor("twitch", "channel", []string{id}, e.s.pointsActive)
	require.Len(t, first, 1)
	assert.Nil(t, first[0].Balance)
	assert.Equal(t, "channel", <-backend.calls)
	select {
	case <-backend.done:
	case <-time.After(time.Second):
		t.Fatal("balance read did not finish")
	}
	assert.Equal(t, id, backend.accountID)
	assert.Equal(t, EventForceWatch, recv(t, events).Name)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got := e.s.pointsFor("twitch", "channel", []string{id}, e.s.pointsActive)
		if got[0].Balance != nil {
			assert.Equal(t, 321, *got[0].Balance)
			break
		}
		time.Sleep(time.Millisecond)
	}
	got := e.s.pointsFor("twitch", "channel", []string{id}, e.s.pointsActive)
	require.NotNil(t, got[0].Balance)
	assert.Equal(t, 321, *got[0].Balance)
	assert.NotNil(t, got[0].UpdatedAt)

	backend.err = errors.New("provider unavailable")
	oldTime := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	e.s.pointsMu.Lock()
	entry := e.s.pointsCache[id+"/channel"]
	entry.lastAttempt = time.Now().Add(-2 * time.Hour)
	entry.updatedAt = &oldTime
	e.s.pointsCache[id+"/channel"] = entry
	backend.done = make(chan struct{})
	e.s.pointsMu.Unlock()
	stale := e.s.pointsFor("twitch", "channel", []string{id}, e.s.pointsActive)
	require.NotNil(t, stale[0].Balance)
	assert.Equal(t, 321, *stale[0].Balance)
	select {
	case <-backend.calls:
	case <-time.After(time.Second):
		t.Fatal("stale balance was not refreshed")
	}
	select {
	case <-backend.done:
	case <-time.After(time.Second):
		t.Fatal("failed balance refresh did not finish")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.s.pointsMu.Lock()
		inFlight := e.s.pointsInFlight[id+"/channel"]
		retained := e.s.pointsCache[id+"/channel"].balance
		e.s.pointsMu.Unlock()
		if inFlight == nil {
			require.NotNil(t, retained)
			assert.Equal(t, 321, *retained)
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("refresh remained in flight")
}

func TestListAccountsSurvivesBadAndLegacyPointsSettings(t *testing.T) {
	e, _ := accountsEnv(t)
	bad := e.createAccount("twitch", "Bad")
	legacy := e.createAccount("twitch", "Legacy")
	ctx := context.Background()
	require.NoError(t, e.s.d.Q.UpsertSettingString(ctx, gen.UpsertSettingStringParams{Key: "points:account:" + bad, Value: []byte("{not json")}))
	require.NoError(t, store.SavePointsAccountSettings(ctx, e.s.d.Q, legacy, store.PointsAccountSettings{Enabled: true, Streams: 100}))

	resp, body := e.do("GET", "/api/accounts", "", nil)
	require.Equal(t, 200, resp.StatusCode, string(body))
	var list []struct {
		ID      string `json:"id"`
		Enabled bool   `json:"points_enabled"`
		Only    bool   `json:"points_only"`
		Streams int    `json:"points_streams"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	got := map[string][3]any{}
	for _, a := range list {
		got[a.ID] = [3]any{a.Enabled, a.Only, a.Streams}
	}
	assert.Equal(t, [3]any{true, false, 2}, got[bad], "unreadable row falls back to the roster defaults")
	assert.Equal(t, [3]any{true, false, store.MaxPointsStreams}, got[legacy], "legacy value above the cap is clamped")
}

func TestPutAccountPointsRepairsUnreadableRow(t *testing.T) {
	e, _ := accountsEnv(t)
	id := e.createAccount("twitch", "T")
	_, token := e.session()
	require.NoError(t, e.s.d.Q.UpsertSettingString(context.Background(), gen.UpsertSettingStringParams{Key: "points:account:" + id, Value: []byte("{not json")}))

	resp, body := e.do("PUT", "/api/accounts/"+id+"/points", token, map[string]any{"points_enabled": false, "points_only": true, "points_streams": 3})
	require.Equal(t, 204, resp.StatusCode, string(body))
	resp, body = e.do("GET", "/api/accounts", "", nil)
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), `"points_enabled":false,"points_only":true,"points_streams":3`)
}

// slotBackend blocks every balance read until release is closed and records
// the peak number of simultaneous reads.
type slotBackend struct {
	platform.Backend
	mu       sync.Mutex
	inflight int
	peak     int
	started  chan string
	release  chan struct{}
	errs     map[string]error
}

func newSlotBackend(open bool) *slotBackend {
	b := &slotBackend{started: make(chan string, 16), release: make(chan struct{}), errs: map[string]error{}}
	if open {
		close(b.release)
	}
	return b
}

func (*slotBackend) Name() string { return "twitch" }
func (b *slotBackend) ChannelPointsBalance(ctx context.Context, _ platform.Session, channel string) (int, error) {
	b.mu.Lock()
	b.inflight++
	b.peak = max(b.peak, b.inflight)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inflight--
		b.mu.Unlock()
	}()
	b.started <- channel
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return 7, b.errs[channel]
}

func recvStarted(t *testing.T, b *slotBackend) string {
	t.Helper()
	select {
	case ch := <-b.started:
		return ch
	case <-time.After(2 * time.Second):
		t.Fatal("balance read did not start")
		return ""
	}
}

func waitPointsIdle(t *testing.T, e *env) {
	t.Helper()
	assert.Eventually(t, func() bool {
		e.s.pointsMu.Lock()
		defer e.s.pointsMu.Unlock()
		return len(e.s.pointsInFlight) == 0
	}, 2*time.Second, time.Millisecond)
}

func TestPointsForSkipsPausedAndPointsDisabledAccounts(t *testing.T) {
	backend := newSlotBackend(true)
	e, _ := accountsEnv(t, backend)
	id := e.createAccount("twitch", "T")
	_, token := e.session()
	require.NoError(t, e.s.d.Sessions.Put(context.Background(), id, platform.Session{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}))

	resp, body := e.do("PUT", "/api/accounts/"+id+"/enabled", token, map[string]bool{"enabled": false})
	require.Equal(t, 204, resp.StatusCode, string(body))
	assert.Empty(t, e.s.pointsFor("twitch", "channel", []string{id}, e.s.pointsActive), "a paused account is not listed")

	resp, body = e.do("PUT", "/api/accounts/"+id+"/enabled", token, map[string]bool{"enabled": true})
	require.Equal(t, 204, resp.StatusCode, string(body))
	require.NoError(t, store.SavePointsAccountSettings(context.Background(), e.s.d.Q, id, store.PointsAccountSettings{Enabled: false, Streams: 2}))
	assert.Empty(t, e.s.pointsFor("twitch", "channel", []string{id}, e.s.pointsActive), "a points-off account is not listed")

	e.s.pointsMu.Lock()
	assert.Empty(t, e.s.pointsInFlight, "no read may start for a paused or points-off account")
	e.s.pointsMu.Unlock()
	assert.Empty(t, backend.started)

	require.NoError(t, store.SavePointsAccountSettings(context.Background(), e.s.d.Q, id, store.PointsAccountSettings{Enabled: true, Streams: 2}))
	got := e.s.pointsFor("twitch", "channel", []string{id}, e.s.pointsActive)
	require.Len(t, got, 1)
	assert.Equal(t, "channel", recvStarted(t, backend), "an active account does read")
}

func TestPointsReadsAreLimitedToTwoInFlight(t *testing.T) {
	backend := newSlotBackend(false)
	e, _ := accountsEnv(t, backend)
	id := e.createAccount("twitch", "T")
	require.NoError(t, e.s.d.Sessions.Put(context.Background(), id, platform.Session{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}))
	for _, ch := range []string{"c1", "c2", "c3", "c4", "c5"} {
		e.s.pointsFor("twitch", ch, []string{id}, e.s.pointsActive)
	}

	recvStarted(t, backend)
	recvStarted(t, backend)
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.inflight == pointsReadSlots
	}, 2*time.Second, time.Millisecond)
	require.Never(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.inflight > pointsReadSlots || len(backend.started) > 0
	}, 100*time.Millisecond, time.Millisecond, "a third read entered while two were in flight")
	close(backend.release)
	for range 3 {
		recvStarted(t, backend)
	}
	waitPointsIdle(t, e)
	backend.mu.Lock()
	assert.Equal(t, pointsReadSlots, backend.peak)
	backend.mu.Unlock()
}

func TestPointsQueuedReadSkipsAccountPausedWhileWaiting(t *testing.T) {
	backend := newSlotBackend(false)
	e, _ := accountsEnv(t, backend)
	id := e.createAccount("twitch", "T")
	_, token := e.session()
	require.NoError(t, e.s.d.Sessions.Put(context.Background(), id, platform.Session{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}))
	for _, ch := range []string{"c1", "c2", "c3"} {
		e.s.pointsFor("twitch", ch, []string{id}, e.s.pointsActive)
	}
	recvStarted(t, backend)
	recvStarted(t, backend) // the third read is now queued behind both slots

	resp, body := e.do("PUT", "/api/accounts/"+id+"/enabled", token, map[string]bool{"enabled": false})
	require.Equal(t, 204, resp.StatusCode, string(body))
	close(backend.release)
	waitPointsIdle(t, e)
	assert.Empty(t, backend.started, "the queued read must not run for an account paused while it waited")
}

func TestPointsNoProgramClearsStaleBalance(t *testing.T) {
	backend := newSlotBackend(true)
	backend.errs["nop"] = fmt.Errorf("read: %w", platform.ErrNoChannelPoints)
	e, _ := accountsEnv(t, backend)
	id := e.createAccount("twitch", "T")
	require.NoError(t, e.s.d.Sessions.Put(context.Background(), id, platform.Session{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}))
	balance := 99
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	e.s.pointsMu.Lock()
	e.s.pointsCache = map[string]pointBalanceEntry{id + "/nop": {balance: &balance, updatedAt: &old, lastAttempt: time.Now().Add(-2 * time.Hour)}}
	e.s.pointsMu.Unlock()

	got := e.s.pointsFor("twitch", "nop", []string{id}, e.s.pointsActive)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Balance, "the old value is shown until the read answers")
	recvStarted(t, backend)
	waitPointsIdle(t, e)

	e.s.pointsMu.Lock()
	entry := e.s.pointsCache[id+"/nop"]
	e.s.pointsMu.Unlock()
	assert.True(t, entry.noProgram)
	assert.Nil(t, entry.balance, "a channel without a points program keeps no stale balance")
	assert.Nil(t, entry.updatedAt)
}

func TestForceViewChecksEachAccountOncePerResponse(t *testing.T) {
	e, _ := accountsEnv(t) // no twitch backend: spawned reads return before logging
	id := e.createAccount("twitch", "T")
	require.NoError(t, e.s.d.Q.UpsertSettingString(context.Background(), gen.UpsertSettingStringParams{Key: "points:account:" + id, Value: []byte("{not json")}))
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	view := e.s.forceView([]store.ForceChannel{
		{Platform: "twitch", Channel: "c1", Accounts: []string{id}},
		{Platform: "twitch", Channel: "c2", Accounts: []string{id}},
		{Platform: "twitch", Channel: "c3", Accounts: []string{id}},
	})
	require.Len(t, view.Channels, 3)
	waitPointsIdle(t, e)
	assert.Equal(t, 1, strings.Count(logs.String(), "points settings unreadable"), "one settings read per account per response")
}

func TestPointsNoProgramIsRememberedForTTLButOtherErrorsRetry(t *testing.T) {
	backend := newSlotBackend(true)
	backend.errs["nop"] = fmt.Errorf("read: %w", platform.ErrNoChannelPoints)
	backend.errs["boom"] = errors.New("provider unavailable")
	e, _ := accountsEnv(t, backend)
	id := e.createAccount("twitch", "T")
	require.NoError(t, e.s.d.Sessions.Put(context.Background(), id, platform.Session{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}))
	setAttempt := func(channel string, age time.Duration) {
		e.s.pointsMu.Lock()
		defer e.s.pointsMu.Unlock()
		entry := e.s.pointsCache[id+"/"+channel]
		entry.lastAttempt = time.Now().Add(-age)
		e.s.pointsCache[id+"/"+channel] = entry
	}

	e.s.pointsFor("twitch", "nop", []string{id}, e.s.pointsActive)
	e.s.pointsFor("twitch", "boom", []string{id}, e.s.pointsActive)
	recvStarted(t, backend)
	recvStarted(t, backend)
	waitPointsIdle(t, e)
	e.s.pointsMu.Lock()
	assert.True(t, e.s.pointsCache[id+"/nop"].noProgram)
	assert.False(t, e.s.pointsCache[id+"/boom"].noProgram)
	e.s.pointsMu.Unlock()

	setAttempt("nop", 10*time.Minute)
	setAttempt("boom", 10*time.Minute)
	e.s.pointsFor("twitch", "nop", []string{id}, e.s.pointsActive)
	e.s.pointsFor("twitch", "boom", []string{id}, e.s.pointsActive)
	assert.Equal(t, "boom", recvStarted(t, backend), "the generic error retries after 5 minutes")
	waitPointsIdle(t, e)
	assert.Empty(t, backend.started, "no-program is not retried after 5 minutes")

	setAttempt("nop", 2*time.Hour)
	e.s.pointsFor("twitch", "nop", []string{id}, e.s.pointsActive)
	assert.Equal(t, "nop", recvStarted(t, backend), "no-program is retried after the TTL")
}

func TestPutForceWatchEvictsBalancesOfRemovedChannels(t *testing.T) {
	e := newResEnv(t, nil)
	_, tok := e.session()
	balance := 5
	now := time.Now()
	e.s.pointsMu.Lock()
	e.s.pointsCache = map[string]pointBalanceEntry{
		"a1/keep": {balance: &balance, lastAttempt: now},
		"a1/drop": {balance: &balance, lastAttempt: now},
	}
	e.s.pointsInFlight = map[string]*pointBalanceFlight{"a1/drop": {}}
	e.s.pointsMu.Unlock()

	resp, b := e.do("PUT", "/api/force-watch", tok, map[string]any{"channels": []map[string]any{
		{"platform": "twitch", "channel": "keep", "accounts": []string{"a1"}},
	}})
	require.Equal(t, 200, resp.StatusCode, string(b))
	e.s.pointsMu.Lock()
	defer e.s.pointsMu.Unlock()
	assert.Contains(t, e.s.pointsCache, "a1/keep")
	assert.NotContains(t, e.s.pointsCache, "a1/drop")
	assert.Empty(t, e.s.pointsInFlight)
}

func TestHomeSkipsPointRefreshWithoutActiveWatchers(t *testing.T) {
	backend := &balanceBackend{calls: make(chan string, 1), done: make(chan struct{})}
	e := newResEnv(t, func(d *Deps) {
		registry := platform.NewRegistry()
		registry.Register(backend)
		d.Registry = registry
		d.ForceLive = func(string, string) *bool {
			live := true
			return &live
		}
	})
	require.NoError(t, e.s.d.ForceWatch.Set(context.Background(), []store.ForceChannel{
		{Platform: "twitch", Channel: "channel", Accounts: []string{"a1"}},
	}))

	force := e.overview()["force_watch"].(map[string]any)
	live := force["live"].([]any)
	require.Len(t, live, 1)
	assert.NotContains(t, live[0].(map[string]any), "points")
	e.s.pointsMu.Lock()
	assert.Empty(t, e.s.pointsCache)
	assert.Empty(t, e.s.pointsInFlight)
	e.s.pointsMu.Unlock()
	select {
	case channel := <-backend.calls:
		t.Fatalf("unexpected balance refresh for %q", channel)
	default:
	}
}

func TestPointsBalanceKickHasNoProjection(t *testing.T) {
	e := newEnv(t)
	assert.Nil(t, e.s.pointsFor("kick", "channel", []string{"a1"}, e.s.pointsActive))
}

func TestPointBalanceIdentityChangeFencesOldRead(t *testing.T) {
	backend := &delayedBalanceBackend{calls: make(chan delayedBalanceRequest, 2)}
	e, _ := accountsEnv(t, backend)
	id := e.createAccount("twitch", "T")
	require.NoError(t, e.s.d.Sessions.Put(context.Background(), id, platform.Session{AccessToken: "old", ExpiresAt: time.Now().Add(time.Hour)}))

	const channel = "channel"
	key := id + "/" + channel
	oldFlight := &pointBalanceFlight{}
	e.s.pointsCache = make(map[string]pointBalanceEntry)
	e.s.pointsInFlight = make(map[string]*pointBalanceFlight)
	e.s.pointsMu.Lock()
	e.s.pointsCache[key] = pointBalanceEntry{}
	e.s.pointsInFlight[key] = oldFlight
	e.s.pointsMu.Unlock()
	oldDone := make(chan struct{})
	go func() {
		e.s.refreshPointBalance(key, id, channel, oldFlight)
		close(oldDone)
	}()
	oldCall := <-backend.calls
	require.Equal(t, "old", oldCall.token)

	e.s.forgetPointBalances(id)
	require.NoError(t, e.s.d.Sessions.Put(context.Background(), id, platform.Session{AccessToken: "new", ExpiresAt: time.Now().Add(time.Hour)}))
	e.s.pointsFor("twitch", channel, []string{id}, e.s.pointsActive)
	newCall := <-backend.calls
	require.Equal(t, "new", newCall.token)

	close(oldCall.release)
	<-oldCall.done
	<-oldDone
	e.s.pointsMu.Lock()
	assert.Nil(t, e.s.pointsCache[key].balance, "old identity must not repopulate an invalidated cache")
	assert.NotNil(t, e.s.pointsInFlight[key], "old completion must not clear the new identity's flight")
	e.s.pointsMu.Unlock()

	close(newCall.release)
	<-newCall.done
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.s.pointsMu.Lock()
		balance := e.s.pointsCache[key].balance
		inFlight := e.s.pointsInFlight[key]
		e.s.pointsMu.Unlock()
		if balance != nil && *balance == 222 && inFlight == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("new identity's balance was not retained")
}
