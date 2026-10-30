package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aalejandrofer/grubdrops/internal/platform"
	"github.com/aalejandrofer/grubdrops/internal/store"
)

type pointBalanceEntry struct {
	balance     *int
	updatedAt   *string
	lastAttempt time.Time
	noProgram   bool // channel has no points program; retry only after the TTL
}

type pointBalanceFlight struct{ _ byte }

type pointBalanceJSON struct {
	AccountID string  `json:"account_id"`
	Balance   *int    `json:"balance"`
	UpdatedAt *string `json:"updated_at"`
}

const (
	pointsBalanceTTL = time.Hour
	pointsRetryDelay = 5 * time.Minute
	// pointsReadSlots caps simultaneous Twitch balance reads across all accounts.
	pointsReadSlots = 2
)

// pointsFor lists balances only for accounts active() accepts; inactive ones are omitted
// because they are never read and would show "checking balance" forever.
func (s *server) pointsFor(platformName, channel string, accountIDs []string, active func(string) bool) []pointBalanceJSON {
	if platformName != "twitch" {
		return nil
	}
	out := make([]pointBalanceJSON, 0, len(accountIDs))
	seen := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		if _, ok := seen[accountID]; ok {
			continue
		}
		seen[accountID] = struct{}{}
		if !active(accountID) {
			continue
		}
		key := accountID + "/" + channel
		now := time.Now()
		s.pointsMu.Lock()
		if s.pointsCache == nil {
			s.pointsCache = make(map[string]pointBalanceEntry)
		}
		if s.pointsInFlight == nil {
			s.pointsInFlight = make(map[string]*pointBalanceFlight)
		}
		entry := s.pointsCache[key]
		delay := pointsRetryDelay
		if entry.noProgram {
			delay = pointsBalanceTTL
		}
		due := entry.lastAttempt.IsZero() || now.Sub(entry.lastAttempt) >= delay
		if entry.updatedAt != nil && now.Sub(parsePointTimestamp(entry.updatedAt)) < pointsBalanceTTL {
			due = false
		}
		if due && s.pointsInFlight[key] == nil {
			entry.lastAttempt = now
			s.pointsCache[key] = entry
			flight := &pointBalanceFlight{}
			s.pointsInFlight[key] = flight
			go s.refreshPointBalance(key, accountID, channel, flight)
		}
		s.pointsMu.Unlock()
		out = append(out, pointBalanceJSON{AccountID: accountID, Balance: entry.balance, UpdatedAt: entry.updatedAt})
	}
	return out
}

func parsePointTimestamp(v *string) time.Time {
	if v == nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, *v)
	return t
}

// pointsActive reports whether the account is running and has points enabled;
// paused or points-off accounts must not cause balance reads.
func (s *server) pointsActive(accountID string) bool {
	ctx := s.rootCtx()
	acc, err := s.d.Q.GetAccount(ctx, accountID)
	if err != nil || acc.Enabled == 0 {
		return false
	}
	return s.loadPointsSettings(ctx, accountID).Enabled
}

func (s *server) refreshPointBalance(key, accountID, channel string, flight *pointBalanceFlight) {
	defer func() {
		s.pointsMu.Lock()
		if s.pointsInFlight[key] == flight {
			delete(s.pointsInFlight, key)
		}
		s.pointsMu.Unlock()
	}()
	select {
	case s.pointsSlots <- struct{}{}:
		defer func() { <-s.pointsSlots }()
	case <-s.rootCtx().Done():
		return
	}
	// Timeout starts after the slot is won so queued reads keep their full budget.
	ctx, cancel := context.WithTimeout(s.rootCtx(), 20*time.Second)
	defer cancel()
	if s.d.Registry == nil || s.d.Sessions == nil {
		return
	}
	backend, ok := s.d.Registry.Get("twitch")
	if !ok {
		return
	}
	reader, ok := backend.(platform.ChannelPointsBalanceReader)
	if !ok {
		return
	}
	// The account may have been paused or switched to points-off while queued for a slot.
	if !s.pointsActive(accountID) {
		return
	}
	session, exists, err := s.d.Sessions.Get(ctx, accountID)
	if err != nil || !exists {
		return
	}
	session.AccountID = accountID
	balance, err := reader.ChannelPointsBalance(ctx, session, channel)
	if err != nil {
		if errors.Is(err, platform.ErrNoChannelPoints) {
			s.pointsMu.Lock()
			if s.pointsInFlight[key] == flight {
				entry := s.pointsCache[key]
				entry.noProgram, entry.balance, entry.updatedAt = true, nil, nil
				s.pointsCache[key] = entry
			}
			s.pointsMu.Unlock()
		}
		return
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	s.pointsMu.Lock()
	if s.pointsInFlight[key] != flight {
		s.pointsMu.Unlock()
		return
	}
	entry := s.pointsCache[key]
	entry.balance, entry.noProgram = &balance, false
	entry.updatedAt = &updatedAt
	s.pointsCache[key] = entry
	s.pointsMu.Unlock()
	if s.d.Hub != nil {
		s.d.Hub.Publish(Event{Name: EventForceWatch, Data: struct{}{}})
	}
}

func (s *server) forgetPointBalances(accountID string) {
	s.pointsMu.Lock()
	defer s.pointsMu.Unlock()
	prefix := accountID + "/"
	for key := range s.pointsCache {
		if strings.HasPrefix(key, prefix) {
			delete(s.pointsCache, key)
			delete(s.pointsInFlight, key)
		}
	}
}

// prunePointBalances drops cache entries of channels that left the force list.
func (s *server) prunePointBalances(list []store.ForceChannel) {
	keep := make(map[string]struct{}, len(list))
	for _, c := range list {
		if c.Platform == "twitch" {
			keep[c.Channel] = struct{}{}
		}
	}
	s.pointsMu.Lock()
	defer s.pointsMu.Unlock()
	for key := range s.pointsCache {
		_, channel, _ := strings.Cut(key, "/")
		if _, ok := keep[channel]; !ok {
			delete(s.pointsCache, key)
			delete(s.pointsInFlight, key)
		}
	}
}
