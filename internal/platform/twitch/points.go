package twitch

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aalejandrofer/grubdrops/internal/platform"
)

type claimCommunityPointsResult struct {
	ClaimCommunityPoints *struct{} `json:"claimCommunityPoints"`
}

// Channel, Self and CommunityPoints are pointers so a null (transient field
// error in a partial response) is distinguishable from "no points here".
type channelPointsContext struct {
	Community struct {
		Channel *struct {
			ID   string `json:"id"`
			Self *struct {
				CommunityPoints *struct {
					Balance        *int `json:"balance"`
					AvailableClaim *struct {
						ID string `json:"id"`
					} `json:"availableClaim"`
				} `json:"communityPoints"`
			} `json:"self"`
		} `json:"channel"`
	} `json:"community"`
}

func channelPointsBalance(ctx context.Context, c *client, s platform.Session, channel string) (int, error) {
	if channel == "" || s.AccessToken == "" {
		return 0, fmt.Errorf("channel and access token are required")
	}
	c.bind(s)
	var state channelPointsContext
	if err := c.gql(ctx, s.AccessToken, OpChannelPointsContext, map[string]any{"channelLogin": channel}, &state); err != nil {
		return 0, fmt.Errorf("get channel points balance: %w", err)
	}
	ch := state.Community.Channel
	if ch == nil || ch.Self == nil {
		// Plain error (not ErrNoChannelPoints): the API retries it soon instead of caching "no points".
		return 0, fmt.Errorf("get channel points balance: incomplete Twitch response for %q", channel)
	}
	if ch.Self.CommunityPoints == nil || ch.Self.CommunityPoints.Balance == nil {
		return 0, fmt.Errorf("%w: Twitch returned no channel points balance", platform.ErrNoChannelPoints)
	}
	return *ch.Self.CommunityPoints.Balance, nil
}

// claimChannelPoints claims the channel's waiting bonus, if any. A non-null
// claimCommunityPoints payload is the success signal.
func claimChannelPoints(ctx context.Context, c *client, s platform.Session, channel string) (bool, error) {
	if channel == "" || s.AccessToken == "" {
		return false, fmt.Errorf("channel and access token are required")
	}
	c.bind(s)
	var state channelPointsContext
	if err := c.gql(ctx, s.AccessToken, OpChannelPointsContext, map[string]any{"channelLogin": channel}, &state); err != nil {
		return false, fmt.Errorf("get channel points context: %w", err)
	}
	ch := state.Community.Channel
	if ch == nil || ch.Self == nil || ch.Self.CommunityPoints == nil {
		return false, nil
	}
	channelID := ch.ID
	claim := ch.Self.CommunityPoints.AvailableClaim
	if claim == nil || claim.ID == "" || channelID == "" {
		return false, nil
	}
	if err := claimBonusByID(ctx, c, s, channelID, claim.ID); err != nil {
		return false, err
	}
	return true, nil
}

// claimBonusByID sends the ClaimCommunityPoints mutation. A non-null
// claimCommunityPoints payload is the success signal.
func claimBonusByID(ctx context.Context, c *client, s platform.Session, channelID, claimID string) error {
	if channelID == "" || claimID == "" || s.AccessToken == "" {
		return fmt.Errorf("channel ID, claim ID and access token are required")
	}
	c.bind(s)
	var result claimCommunityPointsResult
	if err := c.gql(ctx, s.AccessToken, OpClaimCommunityPoints, map[string]any{
		"input": map[string]string{"claimID": claimID, "channelID": channelID},
	}, &result); err != nil {
		return fmt.Errorf("claim channel points bonus: %w", err)
	}
	if result.ClaimCommunityPoints == nil {
		return fmt.Errorf("claim channel points bonus: Twitch returned no claim result")
	}
	return nil
}

func joinRaid(ctx context.Context, c *client, s platform.Session, raidID string) error {
	c.bind(s)
	var result any
	if err := c.gql(ctx, s.AccessToken, OpJoinRaid, map[string]any{"input": map[string]string{"raidID": raidID}}, &result); err != nil {
		return fmt.Errorf("join raid: %w", err)
	}
	return nil
}

// withAuthRetry runs fn against the account's client after ensureAuthenticated.
// A sidecar restart loses its tabs while the authed cache still says authed, so
// a tab-missing error invalidates auth and retries exactly once.
func (b *BrowserBackend) withAuthRetry(ctx context.Context, s platform.Session, fn func(c *client) error) error {
	if err := b.ensureAuthenticated(ctx, s); err != nil {
		return err
	}
	err := fn(b.accountFor(s.AccountID).c)
	if isTabMissingErr(err) {
		slog.Info("twitch sidecar tab missing on channel points call; re-authenticating", "account", s.AccountID)
		b.invalidateAuth(s.AccountID)
		if err := b.ensureAuthenticated(ctx, s); err != nil {
			return err
		}
		err = fn(b.accountFor(s.AccountID).c)
	}
	return err
}

func (b *BrowserBackend) ChannelPointsBalance(ctx context.Context, s platform.Session, channel string) (balance int, err error) {
	err = b.withAuthRetry(ctx, s, func(c *client) (err error) {
		balance, err = channelPointsBalance(ctx, c, s, channel)
		return err
	})
	return balance, err
}

func (b *BrowserBackend) ClaimChannelPoints(ctx context.Context, s platform.Session, channel string) (claimed bool, err error) {
	err = b.withAuthRetry(ctx, s, func(c *client) (err error) {
		claimed, err = claimChannelPoints(ctx, c, s, channel)
		return err
	})
	return claimed, err
}

func (b *BrowserBackend) ClaimChannelPointsBonus(ctx context.Context, s platform.Session, channelID, claimID string) error {
	return b.withAuthRetry(ctx, s, func(c *client) error { return claimBonusByID(ctx, c, s, channelID, claimID) })
}

func (b *BrowserBackend) JoinRaid(ctx context.Context, s platform.Session, raidID string) error {
	return b.withAuthRetry(ctx, s, func(c *client) error { return joinRaid(ctx, c, s, raidID) })
}
