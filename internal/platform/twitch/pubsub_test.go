package twitch

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPubSub_UsesProxyDial(t *testing.T) {
	var used int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt32(&used, 1)
		return nil, context.Canceled
	}
	p := newPubSubClientWithDial(dial)
	_ = p.dialAndPump(context.Background())
	if atomic.LoadInt32(&used) == 0 {
		t.Fatal("PubSub did not use the injected dialer")
	}
}

func TestPubSub_RaidDispatchRequiresMatchingSourceAndEvent(t *testing.T) {
	var gotChannel, gotRaid string
	p := &PubSubClient{handlers: PubSubHandlers{OnRaid: func(channelID, raidID string) {
		gotChannel, gotRaid = channelID, raidID
	}}}
	p.dispatch(TopicRaid("source-17"), []byte(`{"type":"raid_update_v2","raid":{"id":"raid-42","source_id":"source-17"}}`))
	if gotChannel != "source-17" || gotRaid != "raid-42" {
		t.Fatalf("raid callback = (%q, %q), want source-17 and raid-42", gotChannel, gotRaid)
	}
	for _, payload := range []string{
		`{"type":"raid_update_v2","raid":{"id":"raid-42","source_id":"another-source"}}`,
		`{"type":"raid","raid":{"id":"raid-42","source_id":"source-17"}}`,
	} {
		gotChannel, gotRaid = "", ""
		p.dispatch(TopicRaid("source-17"), []byte(payload))
		if gotChannel != "" || gotRaid != "" {
			t.Fatalf("unexpected callback for %s: (%q, %q)", payload, gotChannel, gotRaid)
		}
	}
	if TopicRaid("source-17") != "raid.source-17" {
		t.Fatalf("unexpected raid topic %q", TopicRaid("source-17"))
	}
}

func TestPubSub_PointsDispatchFiresOnlyForClaimAvailable(t *testing.T) {
	var calls [][2]string
	p := &PubSubClient{handlers: PubSubHandlers{OnPointsClaim: func(channelID, claimID string) {
		calls = append(calls, [2]string{channelID, claimID})
	}}}
	topic := TopicCommunityPointsUser(77)
	if topic != "community-points-user-v1.77" {
		t.Fatalf("unexpected points topic %q", topic)
	}
	p.dispatch(topic, []byte(`{"type":"claim-available","data":{"timestamp":"t","claim":{"id":"claim-9","user_id":"77","channel_id":"chan-5","point_gain":{"total_points":50},"created_at":"t"}}}`))
	if len(calls) != 1 || calls[0] != [2]string{"chan-5", "claim-9"} {
		t.Fatalf("claim-available calls = %v, want [chan-5 claim-9]", calls)
	}
	calls = nil
	for _, payload := range []string{
		`{"type":"points-earned","data":{"timestamp":"t","channel_id":"chan-5","point_gain":{"total_points":10},"balance":{"balance":60}}}`,
		`{"type":"claim-available","data":{"claim":{"id":"","channel_id":"chan-5"}}}`,
		`{"type":"claim-available","data":{"claim":{"id":"claim-9","channel_id":""}}}`,
		`not json`,
	} {
		p.dispatch(topic, []byte(payload))
	}
	if len(calls) != 0 {
		t.Fatalf("unexpected OnPointsClaim calls: %v", calls)
	}
	// A nil hook must not panic.
	(&PubSubClient{}).dispatch(topic, []byte(`{"type":"claim-available","data":{"claim":{"id":"c","channel_id":"h"}}}`))
}

func TestUserTopicsIncludePointsTopic(t *testing.T) {
	assert.ElementsMatch(t, []string{
		"user-drop-events.5", "onsite-notifications.5", "community-points-user-v1.5",
	}, userTopics(5))
}

func TestBackend_SubscribeChannelManagesRaidTopic(t *testing.T) {
	p := &PubSubClient{topics: map[string]struct{}{}}
	b := &Backend{pubsub: p}
	b.SubscribeChannel("account", "channel-1")
	p.mu.Lock()
	_, present := p.topics[TopicRaid("channel-1")]
	p.mu.Unlock()
	if !present {
		t.Fatal("raid topic was not subscribed")
	}
	b.UnsubscribeChannel("account", "channel-1")
	p.mu.Lock()
	_, present = p.topics[TopicRaid("channel-1")]
	p.mu.Unlock()
	if present {
		t.Fatal("raid topic was not unsubscribed")
	}
}
