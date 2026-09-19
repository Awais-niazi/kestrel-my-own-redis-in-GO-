package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"kestrel/resp"
)

// replyWithin runs a command on its own goroutine and returns a channel
// carrying the reply, so a test can assert that it is still blocked.
func replyWithin(t *testing.T, c *conn, args ...string) <-chan resp.Value {
	t.Helper()
	c.wait = 30 * time.Second
	out := make(chan resp.Value, 1)
	go func() { out <- c.do(args...) }()
	return out
}

func mustStillBlock(t *testing.T, ch <-chan resp.Value, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s returned %q instead of blocking", what, arr(v))
	case <-time.After(150 * time.Millisecond):
	}
}

func mustReturn(t *testing.T, ch <-chan resp.Value, what string) resp.Value {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never returned", what)
		return resp.Value{}
	}
}

func TestBlpopWaitsForAPush(t *testing.T) {
	ts := startServer(t)
	waiter := ts.connect(t)
	pusher := ts.connect(t)

	ch := replyWithin(t, waiter, "BLPOP", "queue", "0")
	mustStillBlock(t, ch, "BLPOP on an empty list")
	waitFor(t, "the waiter to register", func() bool { return ts.blocked.Waiting() == 1 })

	pusher.do("RPUSH", "queue", "job")
	if got := arr(mustReturn(t, ch, "BLPOP")); got != "queue job" {
		t.Errorf("BLPOP returned %q", got)
	}
	waitFor(t, "the waiter to deregister", func() bool { return ts.blocked.Waiting() == 0 })

	// The element really was removed.
	if got := text(pusher.do("LLEN", "queue")); got != "0" {
		t.Errorf("the list still holds %q elements", got)
	}
}

func TestBlpopReturnsImmediatelyWhenDataIsThere(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("RPUSH", "queue", "a", "b")
	if got := arr(c.do("BLPOP", "queue", "0")); got != "queue a" {
		t.Errorf("BLPOP returned %q", got)
	}
	if got := arr(c.do("BRPOP", "queue", "0")); got != "queue b" {
		t.Errorf("BRPOP returned %q", got)
	}
}

func TestBlpopTimesOut(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	start := time.Now()
	reply := c.do("BLPOP", "nothing", "0.3")
	elapsed := time.Since(start)

	if reply.Kind != resp.KindNullArray && reply.Kind != resp.KindNull {
		t.Errorf("a timed-out BLPOP returned %q", arr(reply))
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("BLPOP returned after %v, before its timeout", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("BLPOP overran its timeout: %v", elapsed)
	}
}

// TestBlpopKeyOrderIsHonoured: the argument order is a priority order, so a
// client listing its urgent queue first expects it served first.
func TestBlpopKeyOrderIsHonoured(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("RPUSH", "low", "slow")
	c.do("RPUSH", "high", "urgent")

	if got := arr(c.do("BLPOP", "high", "low", "0")); got != "high urgent" {
		t.Errorf("BLPOP served %q, want the first key listed", got)
	}
	if got := arr(c.do("BLPOP", "high", "low", "0")); got != "low slow" {
		t.Errorf("BLPOP served %q once the first key emptied", got)
	}
}

func TestBlpopWakesOnAnyOfItsKeys(t *testing.T) {
	ts := startServer(t)
	waiter := ts.connect(t)
	pusher := ts.connect(t)

	ch := replyWithin(t, waiter, "BLPOP", "a", "b", "c", "0")
	waitFor(t, "the waiter to register", func() bool { return ts.blocked.Waiting() == 3 })
	pusher.do("RPUSH", "b", "found")
	if got := arr(mustReturn(t, ch, "BLPOP")); got != "b found" {
		t.Errorf("BLPOP returned %q", got)
	}
}

func TestBzpopminWaits(t *testing.T) {
	ts := startServer(t)
	waiter := ts.connect(t)
	pusher := ts.connect(t)

	ch := replyWithin(t, waiter, "BZPOPMIN", "scores", "0")
	mustStillBlock(t, ch, "BZPOPMIN on an empty sorted set")
	pusher.do("ZADD", "scores", "5", "member")
	if got := arr(mustReturn(t, ch, "BZPOPMIN")); got != "scores member 5" {
		t.Errorf("BZPOPMIN returned %q", got)
	}
}

func TestBlmoveWaits(t *testing.T) {
	ts := startServer(t)
	waiter := ts.connect(t)
	pusher := ts.connect(t)

	ch := replyWithin(t, waiter, "BLMOVE", "src", "dst", "LEFT", "RIGHT", "0")
	mustStillBlock(t, ch, "BLMOVE on an empty source")
	pusher.do("RPUSH", "src", "item")
	if got := text(mustReturn(t, ch, "BLMOVE")); got != "item" {
		t.Errorf("BLMOVE returned %q", got)
	}
	if got := text(pusher.do("LRANGE", "dst", "0", "-1")); got != "" {
		// LRANGE returns an array; check its length instead.
		_ = got
	}
	if got := text(pusher.do("LLEN", "dst")); got != "1" {
		t.Errorf("the destination holds %q elements", got)
	}
}

func TestBrpoplpushWaits(t *testing.T) {
	ts := startServer(t)
	waiter := ts.connect(t)
	pusher := ts.connect(t)

	ch := replyWithin(t, waiter, "BRPOPLPUSH", "src", "dst", "0")
	mustStillBlock(t, ch, "BRPOPLPUSH on an empty source")
	pusher.do("RPUSH", "src", "a", "b")
	if got := text(mustReturn(t, ch, "BRPOPLPUSH")); got != "b" {
		t.Errorf("BRPOPLPUSH returned %q", got)
	}
}

// TestOnlyOneWaiterGetsEachElement is the property that makes a blocking pop
// usable as a work queue.
func TestOnlyOneWaiterGetsEachElement(t *testing.T) {
	ts := startServer(t)
	const waiters = 6
	chans := make([]<-chan resp.Value, waiters)
	for i := range chans {
		chans[i] = replyWithin(t, ts.connect(t), "BLPOP", "work", "5")
	}
	waitFor(t, "every waiter to register", func() bool { return ts.blocked.Waiting() == 1 })

	pusher := ts.connect(t)
	for i := 0; i < waiters; i++ {
		pusher.do("RPUSH", "work", fmt.Sprintf("job%d", i))
	}

	seen := map[string]int{}
	for i, ch := range chans {
		got := arr(mustReturn(t, ch, fmt.Sprintf("waiter %d", i)))
		seen[got]++
	}
	for job, n := range seen {
		if n != 1 {
			t.Errorf("%q was delivered %d times; each element must go to one waiter", job, n)
		}
	}
	if len(seen) != waiters {
		t.Errorf("%d distinct jobs were delivered, want %d", len(seen), waiters)
	}
	if got := text(pusher.do("LLEN", "work")); got != "0" {
		t.Errorf("%q elements were left over", got)
	}
}

// TestBlockingInsideMultiDoesNotBlock: nothing can write while a transaction
// holds the write locks, so waiting would be a deadlock the caller could not
// break.
func TestBlockingInsideMultiDoesNotBlock(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("MULTI")
	c.do("BLPOP", "nothing", "0")
	c.do("SET", "after", "ran")

	c.wait = 30 * time.Second
	done := make(chan resp.Value, 1)
	go func() { done <- c.do("EXEC") }()
	reply := mustReturn(t, done, "EXEC with a queued BLPOP")

	if len(reply.Elems) != 2 {
		t.Fatalf("EXEC returned %d replies, want 2", len(reply.Elems))
	}
	if k := reply.Elems[0].Kind; k != resp.KindNullArray && k != resp.KindNull {
		t.Errorf("the queued BLPOP returned %q, want a nil array", arr(reply.Elems[0]))
	}
	if got := text(c.do("GET", "after")); got != "ran" {
		t.Errorf("the command after the blocking one did not run: %q", got)
	}
}

func TestBlockingRejectsABadTimeout(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	for _, args := range [][]string{
		{"BLPOP", "k", "notanumber"},
		{"BLPOP", "k", "-1"},
		{"BZPOPMIN", "k", "x"},
		{"BLMOVE", "a", "b", "LEFT", "RIGHT", "-2"},
	} {
		if got := text(c.do(args...)); !strings.HasPrefix(got, "ERR") {
			t.Errorf("%v returned %q", args, got)
		}
	}
}

// TestBlockingPropagatesTheNonBlockingForm is the ADR-008 requirement: a
// replica applying "BLPOP" would block on its own stream.
func TestBlockingPropagatesTheNonBlockingForm(t *testing.T) {
	dir := t.TempDir()
	leader, replica, lc, rc := pair(t)
	_ = dir

	lc.do("RPUSH", "queue", "job")
	if got := arr(lc.do("BLPOP", "queue", "0")); got != "queue job" {
		t.Fatalf("BLPOP returned %q", got)
	}
	waitOffset(t, leader, replica)

	if got := text(rc.do("LLEN", "queue")); got != "0" {
		t.Errorf("the replica's list holds %q elements, want 0", got)
	}
	if got := text(rc.do("PING")); got != "PONG" {
		t.Errorf("the replica is stuck: %q", got)
	}
}
