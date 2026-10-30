package twitch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aalejandrofer/grubdrops/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pointsRequest struct {
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
	Extensions    struct {
		PersistedQuery struct {
			Hash string `json:"sha256Hash"`
		} `json:"persistedQuery"`
	} `json:"extensions"`
}

func pointsServer(t *testing.T, bonus bool, requests *[]pointsRequest) (*client, *int) {
	t.Helper()
	claims := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req pointsRequest
		require.NoError(t, json.Unmarshal(body, &req))
		if requests != nil {
			*requests = append(*requests, req)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.OperationName {
		case "ChannelPointsContext":
			claim := `null`
			if bonus {
				claim = `{"id":"bonus-1"}`
			}
			_, _ = io.WriteString(w, `{"data":{"community":{"channel":{"id":"channel-id-1","self":{"communityPoints":{"balance":123,"availableClaim":`+claim+`}}}}}}`)
		case "ClaimCommunityPoints":
			claims++
			_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{}}}`)
		case "JoinRaid":
			_, _ = io.WriteString(w, `{"data":{"joinRaid":{}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv.URL), &claims
}

func TestChannelPointsBalanceIsReadOnly(t *testing.T) {
	var requests []pointsRequest
	c, claims := pointsServer(t, true, &requests)
	balance, err := channelPointsBalance(context.Background(), c, platform.Session{AccessToken: "token"}, "channel-login")
	require.NoError(t, err)
	assert.Equal(t, 123, balance)
	assert.Zero(t, *claims, "balance reads must never claim bonuses")
	require.Len(t, requests, 1)
	assert.Equal(t, "ChannelPointsContext", requests[0].OperationName)
	assert.Equal(t, OpChannelPointsContext.Hash, requests[0].Extensions.PersistedQuery.Hash)
	assert.Equal(t, "channel-login", requests[0].Variables["channelLogin"])
}

// opServer answers each operation with a fixed JSON body and records the
// operation names in order.
func opServer(t *testing.T, bodies map[string]string, ops *[]string) *client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req pointsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		*ops = append(*ops, req.OperationName)
		w.Header().Set("Content-Type", "application/json")
		body, ok := bodies[req.OperationName]
		if !ok {
			body = `{"data":{}}`
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv.URL)
}

func TestChannelPointsBalanceNoBalanceIsErrNoChannelPoints(t *testing.T) {
	var ops []string
	c := opServer(t, map[string]string{"ChannelPointsContext": `{"data":{"community":{"channel":{"id":"1","self":{"communityPoints":{"balance":null,"availableClaim":null}}}}}}`}, &ops)
	_, err := channelPointsBalance(context.Background(), c, platform.Session{AccessToken: "token"}, "channel-login")
	assert.ErrorIs(t, err, platform.ErrNoChannelPoints)
}

func TestChannelPointsBalanceZeroIsValid(t *testing.T) {
	var ops []string
	c := opServer(t, map[string]string{"ChannelPointsContext": `{"data":{"community":{"channel":{"id":"1","self":{"communityPoints":{"balance":0,"availableClaim":null}}}}}}`}, &ops)
	balance, err := channelPointsBalance(context.Background(), c, platform.Session{AccessToken: "token"}, "channel-login")
	require.NoError(t, err)
	assert.Zero(t, balance)
}

func TestClaimChannelPoints(t *testing.T) {
	sess := platform.Session{AccessToken: "token"}
	t.Run("claims available bonus", func(t *testing.T) {
		var requests []pointsRequest
		c, claims := pointsServer(t, true, &requests)
		claimed, err := claimChannelPoints(context.Background(), c, sess, "source")
		require.NoError(t, err)
		assert.True(t, claimed)
		assert.Equal(t, 1, *claims)
		require.Len(t, requests, 2)
		assert.Equal(t, OpChannelPointsContext.Hash, requests[0].Extensions.PersistedQuery.Hash)
		assert.Equal(t, OpClaimCommunityPoints.Hash, requests[1].Extensions.PersistedQuery.Hash)
		input, ok := requests[1].Variables["input"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "bonus-1", input["claimID"])
		assert.Equal(t, "channel-id-1", input["channelID"])
	})
	t.Run("no bonus waiting", func(t *testing.T) {
		var requests []pointsRequest
		c, claims := pointsServer(t, false, &requests)
		claimed, err := claimChannelPoints(context.Background(), c, sess, "source")
		require.NoError(t, err)
		assert.False(t, claimed)
		assert.Zero(t, *claims)
		require.Len(t, requests, 1)
	})
	t.Run("wraps context error", func(t *testing.T) {
		var ops []string
		c := opServer(t, map[string]string{"ChannelPointsContext": `{"errors":[{"message":"boom"}]}`}, &ops)
		claimed, err := claimChannelPoints(context.Background(), c, sess, "source")
		assert.False(t, claimed)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get channel points context")
	})
	t.Run("wraps claim error", func(t *testing.T) {
		var ops []string
		c := opServer(t, map[string]string{
			"ChannelPointsContext": `{"data":{"community":{"channel":{"id":"1","self":{"communityPoints":{"balance":5,"availableClaim":{"id":"b"}}}}}}}`,
			"ClaimCommunityPoints": `{"errors":[{"message":"boom"}]}`,
		}, &ops)
		claimed, err := claimChannelPoints(context.Background(), c, sess, "source")
		assert.False(t, claimed)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "claim channel points bonus")
	})
	t.Run("null claim payload is not success", func(t *testing.T) {
		var ops []string
		c := opServer(t, map[string]string{
			"ChannelPointsContext": `{"data":{"community":{"channel":{"id":"1","self":{"communityPoints":{"balance":5,"availableClaim":{"id":"b"}}}}}}}`,
			"ClaimCommunityPoints": `{"data":{"claimCommunityPoints":null}}`,
		}, &ops)
		claimed, err := claimChannelPoints(context.Background(), c, sess, "source")
		assert.False(t, claimed)
		assert.Error(t, err)
	})
}

func TestClaimChannelPointsBonus(t *testing.T) {
	sess := platform.Session{AccessToken: "token"}
	t.Run("claims by id without a context lookup", func(t *testing.T) {
		var requests []pointsRequest
		c, claims := pointsServer(t, true, &requests)
		require.NoError(t, claimBonusByID(context.Background(), c, sess, "chan-5", "claim-9"))
		assert.Equal(t, 1, *claims)
		require.Len(t, requests, 1)
		assert.Equal(t, OpClaimCommunityPoints.Hash, requests[0].Extensions.PersistedQuery.Hash)
		input, ok := requests[0].Variables["input"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "claim-9", input["claimID"])
		assert.Equal(t, "chan-5", input["channelID"])
	})
	t.Run("null payload is an error", func(t *testing.T) {
		var ops []string
		c := opServer(t, map[string]string{"ClaimCommunityPoints": `{"data":{"claimCommunityPoints":null}}`}, &ops)
		assert.Error(t, claimBonusByID(context.Background(), c, sess, "chan-5", "claim-9"))
	})
	t.Run("empty ids send no request", func(t *testing.T) {
		var requests []pointsRequest
		c, _ := pointsServer(t, true, &requests)
		assert.Error(t, claimBonusByID(context.Background(), c, sess, "", "claim-9"))
		assert.Error(t, claimBonusByID(context.Background(), c, sess, "chan-5", ""))
		assert.Empty(t, requests)
	})
}

func TestWatchStartAndHeartbeatSendNoChannelPointsRequests(t *testing.T) {
	var requests []pointsRequest
	c, claims := pointsServer(t, true, &requests)
	wt := &watch{c: c, cachedUserID: 123, spadeURLs: map[string]string{}, playlistURLs: map[string]playlistEntry{}}
	h, err := wt.start(context.Background(), platform.Session{AccessToken: "token"}, platform.Stream{Channel: "source", ChannelID: "42"})
	require.NoError(t, err)
	_ = wt.heartbeat(context.Background(), h) // Playback itself is intentionally unavailable in this fixture.
	assert.Zero(t, *claims)
	for _, req := range requests {
		assert.NotEqual(t, "ChannelPointsContext", req.OperationName)
		assert.NotEqual(t, "ClaimCommunityPoints", req.OperationName)
	}
}

func TestJoinRaidUsesPersistedOperationAndRaidID(t *testing.T) {
	var requests []pointsRequest
	c, _ := pointsServer(t, false, &requests)
	require.NoError(t, joinRaid(context.Background(), c, platform.Session{AccessToken: "token"}, "raid-9"))
	require.Len(t, requests, 1)
	assert.Equal(t, "JoinRaid", requests[0].OperationName)
	assert.Equal(t, OpJoinRaid.Hash, requests[0].Extensions.PersistedQuery.Hash)
	input, ok := requests[0].Variables["input"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "raid-9", input["raidID"])
}

func TestChannelPointsBalancePartialResponseIsNotNoPoints(t *testing.T) {
	for name, body := range map[string]string{
		"self null":      `{"data":{"community":{"channel":{"id":"1","self":null}}}}`,
		"channel null":   `{"data":{"community":{"channel":null}}}`,
		"community null": `{"data":{"community":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var ops []string
			c := opServer(t, map[string]string{"ChannelPointsContext": body}, &ops)
			_, err := channelPointsBalance(context.Background(), c, platform.Session{AccessToken: "token"}, "channel-login")
			require.Error(t, err)
			assert.NotErrorIs(t, err, platform.ErrNoChannelPoints)
		})
	}
}

func TestChannelPointsBalanceNullCommunityPointsIsErrNoChannelPoints(t *testing.T) {
	var ops []string
	c := opServer(t, map[string]string{"ChannelPointsContext": `{"data":{"community":{"channel":{"id":"1","self":{"communityPoints":null}}}}}`}, &ops)
	_, err := channelPointsBalance(context.Background(), c, platform.Session{AccessToken: "token"}, "channel-login")
	assert.ErrorIs(t, err, platform.ErrNoChannelPoints)
}

type countingTransport struct{ n int }

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.n++
	return http.DefaultTransport.RoundTrip(r)
}

// Backend.ClaimChannelPoints must send through b.c (the proxy-routed client).
func TestBackendClaimChannelPointsUsesProxyRoutedClient(t *testing.T) {
	var requests []pointsRequest
	c, _ := pointsServer(t, true, &requests)
	b := &Backend{c: c}
	rt := &countingTransport{}
	c.http.Transport = rt
	claimed, err := b.ClaimChannelPoints(context.Background(), platform.Session{AccessToken: "token"}, "channel-login")
	require.NoError(t, err)
	assert.True(t, claimed)
	assert.Equal(t, 2, rt.n, "context + claim requests must use b.c's transport")
}

// Backend.ClaimChannelPointsBonus must send through b.c (the proxy-routed client).
func TestBackendClaimChannelPointsBonusUsesProxyRoutedClient(t *testing.T) {
	var requests []pointsRequest
	c, _ := pointsServer(t, true, &requests)
	b := &Backend{c: c}
	rt := &countingTransport{}
	c.http.Transport = rt
	require.NoError(t, b.ClaimChannelPointsBonus(context.Background(), platform.Session{AccessToken: "token"}, "chan-5", "claim-9"))
	assert.Equal(t, 1, rt.n, "exactly one claim request through b.c's transport")
	require.Len(t, requests, 1)
	assert.Equal(t, "ClaimCommunityPoints", requests[0].OperationName)
}
