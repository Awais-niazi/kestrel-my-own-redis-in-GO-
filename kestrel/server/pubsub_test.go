package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"kestrel/resp"
)

// arr renders an array reply for comparison, descending into nested arrays
// so that a map-shaped reply like ACL GETUSER is readable.
func arr(v resp.Value) string {
	if len(v.Elems) == 0 {
		return text(v)
	}
	parts := make([]string, len(v.Elems))
	for i, e := range v.Elems {
		parts[i] = arr(e)
	}
	return strings.Join(parts, " ")
}

func TestSubscribeAndPublish(t *testing.T) {
	ts := startServer(t)
	sub := ts.connect(t)
	pub := ts.connect(t)

	if got := arr(sub.do("SUBSCRIBE", "news")); got != "subscribe news 1" {
		t.Fatalf("SUBSCRIBE confirmed with %q", got)
	}
	waitFor(t, "the subscription to register", func() bool {
		return ts.pubsub.SubscriberCount("news") == 1
	})

	if got := text(pub.do("PUBLISH", "news", "hello")); got != "1" {
		t.Errorf("PUBLISH reached %q subscribers, want 1", got)
	}
	if got := arr(sub.reply()); got != "message news hello" {
		t.Errorf("the subscriber received %q", got)
	}
}

func TestPublishToNobody(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	if got := text(c.do("PUBLISH", "empty", "hello")); got != "0" {
		t.Errorf("PUBLISH to an empty channel returned %q", got)
	}
}

func TestPatternSubscription(t *testing.T) {
	ts := startServer(t)
	sub := ts.connect(t)
	pub := ts.connect(t)

	if got := arr(sub.do("PSUBSCRIBE", "news.*")); got != "psubscribe news.* 1" {
		t.Fatalf("PSUBSCRIBE confirmed with %q", got)
	}
	waitFor(t, "the pattern to register", func() bool { return ts.pubsub.PatternCount() == 1 })

	if got := text(pub.do("PUBLISH", "news.sport", "goal")); got != "1" {
		t.Errorf("PUBLISH returned %q", got)
	}
	if got := arr(sub.reply()); got != "pmessage news.* news.sport goal" {
		t.Errorf("received %q", got)
	}
	// A channel outside the pattern must not arrive.
	if got := text(pub.do("PUBLISH", "weather", "rain")); got != "0" {
		t.Errorf("a non-matching channel reached %q subscribers", got)
	}
}

// TestOneMessagePerMatchingSubscription pins that a client subscribed both
// directly and by pattern receives the message twice, which is what the
// reference implementation does and what PUBLISH's count reports.
func TestOneMessagePerMatchingSubscription(t *testing.T) {
	ts := startServer(t)
	sub := ts.connect(t)
	pub := ts.connect(t)
	sub.do("SUBSCRIBE", "news")
	sub.do("PSUBSCRIBE", "ne*")
	waitFor(t, "both subscriptions", func() bool {
		return ts.pubsub.SubscriberCount("news") == 1 && ts.pubsub.PatternCount() == 1
	})

	if got := text(pub.do("PUBLISH", "news", "hello")); got != "2" {
		t.Errorf("PUBLISH counted %q deliveries, want 2", got)
	}
	got := []string{arr(sub.reply()), arr(sub.reply())}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "message news hello") ||
		!strings.Contains(joined, "pmessage ne* news hello") {
		t.Errorf("received %v", got)
	}
}

func TestUnsubscribe(t *testing.T) {
	ts := startServer(t)
	sub := ts.connect(t)
	pub := ts.connect(t)
	sub.do("SUBSCRIBE", "a", "b")
	sub.reply() // the second confirmation
	waitFor(t, "both channels", func() bool {
		return ts.pubsub.SubscriberCount("a") == 1 && ts.pubsub.SubscriberCount("b") == 1
	})

	if got := arr(sub.do("UNSUBSCRIBE", "a")); got != "unsubscribe a 1" {
		t.Errorf("UNSUBSCRIBE confirmed with %q", got)
	}
	waitFor(t, "the channel to clear", func() bool {
		return ts.pubsub.SubscriberCount("a") == 0
	})
	if got := text(pub.do("PUBLISH", "a", "x")); got != "0" {
		t.Errorf("a message still reached an unsubscribed channel: %q", got)
	}
	if got := text(pub.do("PUBLISH", "b", "y")); got != "1" {
		t.Errorf("the remaining subscription stopped working: %q", got)
	}
}

// TestUnsubscribeFromEverything covers the bare form, which must confirm
// every channel the client held.
func TestUnsubscribeFromEverything(t *testing.T) {
	ts := startServer(t)
	sub := ts.connect(t)
	sub.do("SUBSCRIBE", "a", "b", "c")
	sub.reply()
	sub.reply()

	sub.send("UNSUBSCRIBE")
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		f := strings.Fields(arr(sub.reply()))
		if len(f) != 3 || f[0] != "unsubscribe" {
			t.Fatalf("confirmation %d was %v", i, f)
		}
		seen[f[1]] = true
	}
	for _, ch := range []string{"a", "b", "c"} {
		if !seen[ch] {
			t.Errorf("no confirmation for %q", ch)
		}
	}
}

// TestUnsubscribeWithNothingSubscribedStillReplies keeps a client that waits
// for one reply per request from hanging.
func TestUnsubscribeWithNothingSubscribedStillReplies(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	if got := arr(c.do("UNSUBSCRIBE")); !strings.HasPrefix(got, "unsubscribe") {
		t.Errorf("UNSUBSCRIBE with no subscriptions returned %q", got)
	}
}

// TestResp2SubscriberModeRestriction is the reason the restriction exists: a
// RESP2 client cannot tell a push from a reply, so it may only send commands
// whose replies it can still account for.
func TestResp2SubscriberModeRestriction(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("SUBSCRIBE", "news")

	if got := text(c.do("GET", "k")); !strings.Contains(got, "only") {
		t.Errorf("GET in subscriber mode returned %q, want a refusal", got)
	}
	if got := text(c.do("PING")); got != "PONG" {
		t.Errorf("PING is allowed in subscriber mode but returned %q", got)
	}
	if got := arr(c.do("UNSUBSCRIBE", "news")); !strings.HasPrefix(got, "unsubscribe") {
		t.Errorf("UNSUBSCRIBE returned %q", got)
	}
	// Once the last subscription is gone the connection is ordinary again.
	if got := text(c.do("SET", "k", "v")); got != "OK" {
		t.Errorf("a client that left subscriber mode still refuses writes: %q", got)
	}
}

// TestResp3SubscriberKeepsFullCommandSet: RESP3 gives pushes their own type,
// so the restriction has no reason to exist there.
func TestResp3SubscriberKeepsFullCommandSet(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("HELLO", "3")
	c.do("SUBSCRIBE", "news")

	if got := text(c.do("SET", "k", "v")); got != "OK" {
		t.Errorf("a RESP3 subscriber was refused SET: %q", got)
	}
	if got := text(c.do("GET", "k")); got != "v" {
		t.Errorf("a RESP3 subscriber was refused GET: %q", got)
	}
}

func TestPubSubIntrospection(t *testing.T) {
	ts := startServer(t)
	sub := ts.connect(t)
	q := ts.connect(t)
	sub.do("SUBSCRIBE", "news.a")
	sub.do("SUBSCRIBE", "news.b")
	sub.do("PSUBSCRIBE", "other.*")
	waitFor(t, "subscriptions to register", func() bool {
		return ts.pubsub.SubscriberCount("news.a") == 1 && ts.pubsub.PatternCount() == 1
	})

	if got := text(q.do("PUBSUB", "NUMPAT")); got != "1" {
		t.Errorf("NUMPAT returned %q", got)
	}
	if got := arr(q.do("PUBSUB", "NUMSUB", "news.a", "nobody")); got != "news.a 1 nobody 0" {
		t.Errorf("NUMSUB returned %q", got)
	}
	channels := arr(q.do("PUBSUB", "CHANNELS"))
	if !strings.Contains(channels, "news.a") || !strings.Contains(channels, "news.b") {
		t.Errorf("CHANNELS returned %q", channels)
	}
	if got := arr(q.do("PUBSUB", "CHANNELS", "news.*")); !strings.Contains(got, "news.") {
		t.Errorf("CHANNELS with a pattern returned %q", got)
	}
	if got := arr(q.do("PUBSUB", "CHANNELS", "nothing.*")); got != "" {
		t.Errorf("CHANNELS with a non-matching pattern returned %q", got)
	}
}

// TestSubscriptionsEndWithTheConnection guards against the registry holding
// a client whose socket has gone.
func TestSubscriptionsEndWithTheConnection(t *testing.T) {
	ts := startServer(t)
	sub := ts.connect(t)
	sub.do("SUBSCRIBE", "news")
	waitFor(t, "the subscription", func() bool { return ts.pubsub.SubscriberCount("news") == 1 })

	sub.nc.Close()
	waitFor(t, "the subscription to be dropped", func() bool {
		return ts.pubsub.SubscriberCount("news") == 0
	})

	pub := ts.connect(t)
	if got := text(pub.do("PUBLISH", "news", "hello")); got != "0" {
		t.Errorf("PUBLISH counted %q subscribers after the socket closed", got)
	}
}

// TestManySubscribersEachGetTheMessage exercises the delivery goroutines
// against each other.
func TestManySubscribersEachGetTheMessage(t *testing.T) {
	ts := startServer(t)
	const n = 20
	subs := make([]*conn, n)
	for i := range subs {
		subs[i] = ts.connect(t)
		subs[i].do("SUBSCRIBE", "fanout")
	}
	waitFor(t, "every subscription", func() bool {
		return ts.pubsub.SubscriberCount("fanout") == n
	})

	pub := ts.connect(t)
	if got := text(pub.do("PUBLISH", "fanout", "broadcast")); got != fmt.Sprint(n) {
		t.Fatalf("PUBLISH reached %q subscribers, want %d", got, n)
	}
	for i, s := range subs {
		if got := arr(s.reply()); got != "message fanout broadcast" {
			t.Errorf("subscriber %d received %q", i, got)
		}
	}
}

// TestPublishIsNotHeldUpByASlowSubscriber is the property the queue exists
// for: a subscriber that never reads must not stall a publisher.
func TestPublishIsNotHeldUpByASlowSubscriber(t *testing.T) {
	ts := startServer(t)
	slow := ts.connect(t)
	slow.do("SUBSCRIBE", "busy")
	waitFor(t, "the subscription", func() bool { return ts.pubsub.SubscriberCount("busy") == 1 })

	pub := ts.connect(t)
	start := time.Now()
	for i := 0; i < 3000; i++ {
		pub.do("PUBLISH", "busy", strings.Repeat("x", 512))
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("publishing to a subscriber that never reads took %v", elapsed)
	}
	// The publisher stays usable whatever happened to the subscriber.
	if got := text(pub.do("PING")); got != "PONG" {
		t.Errorf("the publisher is stuck: %q", got)
	}
}
