package command

import (
	"sync"

	"kestrel/engine"
	"kestrel/resp"
)

// Pub/Sub delivery (FR-5).
//
// A message is handed to a subscriber's queue, not written to its socket.
// The publisher's goroutine owns neither the subscriber's writer nor its
// connection, and writing there directly would race the connection's own
// replies -- and worse, would let one slow reader hold up every publisher in
// the server.
//
// The queue is bounded. A subscriber that cannot keep up is disconnected
// rather than allowed to grow a buffer without limit, which is the same
// trade the reference implementation makes with its pub/sub output buffer
// limit: unbounded buffering turns one slow client into an out-of-memory
// kill for everyone.

// outboxDepth is how many messages may be queued for one subscriber before
// it is considered too slow to keep.
const outboxDepth = 1024

// Message is one delivery to a subscriber.
type Message struct {
	// Pattern is the glob that matched, for a pattern subscription, and
	// empty for a direct one.
	Pattern string
	Channel string
	Payload []byte
}

// PubSub is the server's subscription registry.
//
// The per-client subscription sets live on the Client but are only ever
// touched under this lock, so there is one place that decides what a client
// is subscribed to rather than two that have to agree.
type PubSub struct {
	mu       sync.RWMutex
	channels map[string]map[*Client]struct{}
	patterns map[string]map[*Client]struct{}
}

// NewPubSub returns an empty registry.
func NewPubSub() *PubSub {
	return &PubSub{
		channels: make(map[string]map[*Client]struct{}),
		patterns: make(map[string]map[*Client]struct{}),
	}
}

// Subscribe adds a channel subscription and returns the client's new total.
func (ps *PubSub) Subscribe(c *Client, channel string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if _, dup := c.channels[channel]; !dup {
		c.channels[channel] = struct{}{}
		set := ps.channels[channel]
		if set == nil {
			set = make(map[*Client]struct{})
			ps.channels[channel] = set
		}
		set[c] = struct{}{}
		c.subCount.Add(1)
	}
	return len(c.channels) + len(c.patterns)
}

// PSubscribe adds a pattern subscription and returns the client's new total.
func (ps *PubSub) PSubscribe(c *Client, pattern string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if _, dup := c.patterns[pattern]; !dup {
		c.patterns[pattern] = struct{}{}
		set := ps.patterns[pattern]
		if set == nil {
			set = make(map[*Client]struct{})
			ps.patterns[pattern] = set
		}
		set[c] = struct{}{}
		c.subCount.Add(1)
	}
	return len(c.channels) + len(c.patterns)
}

// Unsubscribe removes one channel subscription.
func (ps *PubSub) Unsubscribe(c *Client, channel string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.removeChannelLocked(c, channel)
	return len(c.channels) + len(c.patterns)
}

// PUnsubscribe removes one pattern subscription.
func (ps *PubSub) PUnsubscribe(c *Client, pattern string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.removePatternLocked(c, pattern)
	return len(c.channels) + len(c.patterns)
}

// Channels lists a client's channel subscriptions.
func (ps *PubSub) Channels(c *Client) []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return keysOf(c.channels)
}

// Patterns lists a client's pattern subscriptions.
func (ps *PubSub) Patterns(c *Client) []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return keysOf(c.patterns)
}

// Remove drops every subscription a client holds. The server calls it when a
// connection ends, so that a registry entry can never outlive its client.
func (ps *PubSub) Remove(c *Client) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for ch := range c.channels {
		ps.removeChannelLocked(c, ch)
	}
	for p := range c.patterns {
		ps.removePatternLocked(c, p)
	}
}

func (ps *PubSub) removeChannelLocked(c *Client, channel string) {
	if _, ok := c.channels[channel]; !ok {
		return
	}
	delete(c.channels, channel)
	c.subCount.Add(-1)
	if set := ps.channels[channel]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			// Empty sets are removed rather than left behind: PUBSUB
			// CHANNELS reports what has subscribers, and a channel that
			// once had one is not the same thing.
			delete(ps.channels, channel)
		}
	}
}

func (ps *PubSub) removePatternLocked(c *Client, pattern string) {
	if _, ok := c.patterns[pattern]; !ok {
		return
	}
	delete(c.patterns, pattern)
	c.subCount.Add(-1)
	if set := ps.patterns[pattern]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(ps.patterns, pattern)
		}
	}
}

// Publish delivers a message and returns how many subscribers received it.
//
// A subscriber whose queue is full is counted as not having received it and
// is marked for disconnection. Counting it would be a lie, and the count is
// the only thing PUBLISH tells the caller.
func (ps *PubSub) Publish(channel string, payload []byte) int {
	// The payload arrives aliasing the publisher's read buffer, which that
	// connection reuses for its next command. A queued message outlives
	// this call and is written by another goroutine entirely, so it is
	// copied once here -- at the point where it stops belonging to the
	// caller -- rather than per subscriber.
	payload = append([]byte(nil), payload...)

	ps.mu.RLock()
	defer ps.mu.RUnlock()

	// The reply is built once and handed to every subscriber. A push
	// renders differently in RESP2 and RESP3, but that is decided by each
	// connection's writer from the one Kind, so one value serves all of
	// them.
	direct := MessageValue(Message{Channel: channel, Payload: payload})

	n := 0
	for c := range ps.channels[channel] {
		if c.deliver(direct) {
			n++
		}
	}
	for pattern, set := range ps.patterns {
		if !engine.MatchPattern([]byte(pattern), []byte(channel)) {
			continue
		}
		v := MessageValue(Message{Pattern: pattern, Channel: channel, Payload: payload})
		for c := range set {
			if c.deliver(v) {
				n++
			}
		}
	}
	return n
}

// ChannelNames lists channels with at least one subscriber, optionally
// filtered by a glob.
func (ps *PubSub) ChannelNames(pattern string, filtered bool) []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]string, 0, len(ps.channels))
	for ch := range ps.channels {
		if filtered && !engine.MatchPattern([]byte(pattern), []byte(ch)) {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// SubscriberCount reports how many clients are subscribed to a channel.
func (ps *PubSub) SubscriberCount(channel string) int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.channels[channel])
}

// PatternCount reports how many distinct patterns are subscribed to.
func (ps *PubSub) PatternCount() int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.patterns)
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// deliver queues a push, reporting whether it was accepted.
func (c *Client) deliver(v resp.Value) bool {
	select {
	case c.outbox <- v:
		return true
	default:
		// The queue is full, so this subscriber is not keeping up. It is
		// dropped rather than blocking the publisher, which would make one
		// slow reader everyone's problem.
		c.overflowed.Store(true)
		return false
	}
}

// Outbox is the queue a connection's delivery goroutine reads. Pub/Sub
// messages and MONITOR lines both travel on it: from the connection's point
// of view they are the same thing, output it did not ask for.
func (c *Client) Outbox() <-chan resp.Value { return c.outbox }

// Push queues a value for delivery, reporting whether it was accepted.
func (c *Client) Push(v resp.Value) bool { return c.deliver(v) }

// Overflowed reports whether this client fell too far behind to keep.
func (c *Client) Overflowed() bool { return c.overflowed.Load() }

// Subscribed reports whether the client holds any subscription.
func (c *Client) Subscribed() bool { return c.subCount.Load() > 0 }

// MessageValue renders a delivery in the form a subscriber expects.
//
// It is a push in RESP3 and a plain array in RESP2, which the writer handles
// from the one Kind: the difference between the dialects is the framing, not
// the content.
func MessageValue(m Message) resp.Value {
	if m.Pattern != "" {
		return resp.Push(
			resp.BulkString("pmessage"),
			resp.BulkString(m.Pattern),
			resp.BulkString(m.Channel),
			resp.Bulk(m.Payload),
		)
	}
	return resp.Push(
		resp.BulkString("message"),
		resp.BulkString(m.Channel),
		resp.Bulk(m.Payload),
	)
}

func init() {
	for _, r := range []struct {
		name    string
		pattern bool
		summary string
	}{
		{"SUBSCRIBE", false, "Listens for messages published to channels."},
		{"PSUBSCRIBE", true, "Listens for messages published to channels matching patterns."},
	} {
		pattern := r.pattern
		register(&Descriptor{
			Name: r.name, Arity: -2,
			Flags:      Readonly | Fast | Loading | Stale | SubscriberOK | NoMulti,
			Categories: []string{"pubsub", "fast"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return subscribe(c, pattern) },
		})
	}
	for _, r := range []struct {
		name    string
		pattern bool
		summary string
	}{
		{"UNSUBSCRIBE", false, "Stops listening to channels."},
		{"PUNSUBSCRIBE", true, "Stops listening to channels matching patterns."},
	} {
		pattern := r.pattern
		register(&Descriptor{
			Name: r.name, Arity: -1,
			Flags:      Readonly | Fast | Loading | Stale | SubscriberOK | NoMulti,
			Categories: []string{"pubsub", "fast"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return unsubscribe(c, pattern) },
		})
	}
	register(&Descriptor{
		Name: "PUBLISH", Arity: 3, Flags: Readonly | Fast | Loading | Stale | SubscriberOK,
		Categories: []string{"pubsub", "fast"},
		Summary:    "Posts a message to a channel.",
		Handler:    cmdPublish,
	})
	register(&Descriptor{
		Name: "PUBSUB", Arity: -2, Flags: Readonly | Fast | Loading | Stale | SubscriberOK,
		Categories: []string{"pubsub", "slow"},
		Summary:    "A container for Pub/Sub introspection commands.",
		Subcommands: map[string]*Descriptor{
			"CHANNELS": {Arity: -2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Lists channels with at least one subscriber.",
				Handler: cmdPubSubChannels},
			"NUMSUB": {Arity: -2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Counts subscribers for the named channels.",
				Handler: cmdPubSubNumSub},
			"NUMPAT": {Arity: 2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Counts the distinct patterns subscribed to.",
				Handler: cmdPubSubNumPat},
			"HELP": {Arity: 2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Shows helpful text.", Handler: cmdPubSubHelp},
		},
	})
}

// subscribe confirms one line per channel, each carrying the running total.
//
// The count is the client's total across channels and patterns, not the
// number of arguments, because that is what tells a client when it may
// leave subscriber mode.
func subscribe(c *Ctx, pattern bool) resp.Value {
	ps := c.Host.PubSub()
	verb := "subscribe"
	if pattern {
		verb = "psubscribe"
	}
	for i := 1; i < c.Len(); i++ {
		name := string(c.Arg(i))
		var n int
		if pattern {
			n = ps.PSubscribe(c.Client, name)
		} else {
			n = ps.Subscribe(c.Client, name)
		}
		c.Client.Out.WriteValue(resp.Push(
			resp.BulkString(verb), resp.BulkString(name), resp.Int(int64(n))))
	}
	return resp.None()
}

// unsubscribe with no arguments drops everything of that kind.
func unsubscribe(c *Ctx, pattern bool) resp.Value {
	ps := c.Host.PubSub()
	verb := "unsubscribe"
	if pattern {
		verb = "punsubscribe"
	}

	names := make([]string, 0, c.Len())
	for i := 1; i < c.Len(); i++ {
		names = append(names, string(c.Arg(i)))
	}
	if len(names) == 0 {
		if pattern {
			names = ps.Patterns(c.Client)
		} else {
			names = ps.Channels(c.Client)
		}
		// Unsubscribing from nothing still gets a confirmation, with an
		// empty name. A client waiting for one reply per request would
		// otherwise hang here.
		if len(names) == 0 {
			c.Client.Out.WriteValue(resp.Push(
				resp.BulkString(verb), resp.Null(),
				resp.Int(int64(c.Client.subCount.Load()))))
			return resp.None()
		}
	}
	for _, name := range names {
		var n int
		if pattern {
			n = ps.PUnsubscribe(c.Client, name)
		} else {
			n = ps.Unsubscribe(c.Client, name)
		}
		c.Client.Out.WriteValue(resp.Push(
			resp.BulkString(verb), resp.BulkString(name), resp.Int(int64(n))))
	}
	return resp.None()
}

func cmdPublish(c *Ctx) resp.Value {
	return resp.Int(int64(c.Host.PubSub().Publish(string(c.Arg(1)), c.Arg(2))))
}

func cmdPubSubChannels(c *Ctx) resp.Value {
	if c.Len() > 3 {
		return errWrongArgs("pubsub|channels")
	}
	pattern, filtered := "", false
	if c.Len() == 3 {
		pattern, filtered = string(c.Arg(2)), true
	}
	names := c.Host.PubSub().ChannelNames(pattern, filtered)
	out := make([]resp.Value, len(names))
	for i, n := range names {
		out[i] = resp.BulkString(n)
	}
	return resp.ArrayOf(out)
}

func cmdPubSubNumSub(c *Ctx) resp.Value {
	ps := c.Host.PubSub()
	out := make([]resp.Value, 0, (c.Len()-2)*2)
	for i := 2; i < c.Len(); i++ {
		name := string(c.Arg(i))
		out = append(out, resp.BulkString(name),
			resp.Int(int64(ps.SubscriberCount(name))))
	}
	return resp.ArrayOf(out)
}

func cmdPubSubNumPat(c *Ctx) resp.Value {
	return resp.Int(int64(c.Host.PubSub().PatternCount()))
}

func cmdPubSubHelp(c *Ctx) resp.Value {
	lines := []string{
		"PUBSUB <subcommand>",
		"CHANNELS [pattern]    -- List channels with at least one subscriber.",
		"NUMSUB [channel ...]  -- Count subscribers per channel.",
		"NUMPAT                -- Count the distinct patterns subscribed to.",
	}
	out := make([]resp.Value, len(lines))
	for i, l := range lines {
		out[i] = resp.Simple(l)
	}
	return resp.ArrayOf(out)
}

// NeedsPusher reports whether this client is receiving output it did not
// ask for, and so needs a goroutine to deliver it.
func (c *Client) NeedsPusher() bool { return c.Subscribed() || c.monitoring.Load() }

// Monitoring reports whether the client is watching the command stream.
func (c *Client) Monitoring() bool { return c.monitoring.Load() }
