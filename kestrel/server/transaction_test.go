package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"kestrel/resp"
)

func TestMultiExecRunsEverything(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)

	if got := text(c.do("MULTI")); got != "OK" {
		t.Fatalf("MULTI returned %q", got)
	}
	for _, args := range [][]string{
		{"SET", "a", "1"}, {"INCR", "counter"}, {"RPUSH", "l", "x"},
	} {
		if got := text(c.do(args...)); got != "QUEUED" {
			t.Fatalf("%v queued as %q", args, got)
		}
	}
	// Nothing has run yet.
	q := ts.connect(t)
	if got := text(q.do("GET", "a")); got != "<nil>" {
		t.Errorf("a queued command took effect before EXEC: %q", got)
	}

	replies := c.do("EXEC")
	if len(replies.Elems) != 3 {
		t.Fatalf("EXEC returned %d replies, want 3", len(replies.Elems))
	}
	if got := arr(replies); got != "OK 1 1" {
		t.Errorf("EXEC returned %q", got)
	}
	if got := text(q.do("GET", "a")); got != "1" {
		t.Errorf("the transaction did not take effect: %q", got)
	}
}

func TestDiscard(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("MULTI")
	c.do("SET", "a", "1")
	if got := text(c.do("DISCARD")); got != "OK" {
		t.Fatalf("DISCARD returned %q", got)
	}
	if got := text(c.do("GET", "a")); got != "<nil>" {
		t.Errorf("a discarded command took effect: %q", got)
	}
	if got := text(c.do("EXEC")); !strings.Contains(got, "without MULTI") {
		t.Errorf("EXEC after DISCARD returned %q", got)
	}
}

// TestQueueErrorPoisonsTheTransaction: a command that cannot be queued must
// not simply be skipped. Running the rest would be a transaction the caller
// did not ask for, with the missing command left to infer from the count.
func TestQueueErrorPoisonsTheTransaction(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("MULTI")
	c.do("SET", "a", "1")
	if got := text(c.do("NOTACOMMAND")); !strings.Contains(got, "unknown command") {
		t.Errorf("an unknown command queued as %q", got)
	}
	if got := text(c.do("EXEC")); !strings.HasPrefix(got, "EXECABORT") {
		t.Errorf("EXEC after a queue error returned %q", got)
	}
	if got := text(c.do("GET", "a")); got != "<nil>" {
		t.Errorf("an aborted transaction took effect: %q", got)
	}
	// Wrong arity is caught at queue time too.
	c.do("MULTI")
	c.do("SET", "onlykey")
	if got := text(c.do("EXEC")); !strings.HasPrefix(got, "EXECABORT") {
		t.Errorf("EXEC after an arity error returned %q", got)
	}
}

// TestRuntimeErrorDoesNotAbortTheRest matches the reference implementation:
// an error discovered while running is reported in its slot, and the other
// commands still run.
func TestRuntimeErrorDoesNotAbortTheRest(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("SET", "str", "value")
	c.do("MULTI")
	c.do("SET", "a", "1")
	c.do("LPUSH", "str", "x") // WRONGTYPE, only discoverable at run time
	c.do("SET", "b", "2")

	replies := c.do("EXEC")
	if len(replies.Elems) != 3 {
		t.Fatalf("EXEC returned %d replies, want 3", len(replies.Elems))
	}
	if !strings.HasPrefix(text(replies.Elems[1]), "WRONGTYPE") {
		t.Errorf("the failing command's slot holds %q", text(replies.Elems[1]))
	}
	if got := text(c.do("GET", "b")); got != "2" {
		t.Errorf("a command after the failing one did not run: %q", got)
	}
}

func TestNestedMultiIsRefused(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("MULTI")
	if got := text(c.do("MULTI")); !strings.Contains(got, "nested") {
		t.Errorf("a nested MULTI returned %q", got)
	}
	c.do("DISCARD")
}

func TestWatchAbortsOnAConcurrentWrite(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	other := ts.connect(t)

	c.do("SET", "k", "1")
	if got := text(c.do("WATCH", "k")); got != "OK" {
		t.Fatalf("WATCH returned %q", got)
	}
	other.do("SET", "k", "2") // breaks the watch

	c.do("MULTI")
	c.do("SET", "k", "3")
	reply := c.do("EXEC")
	if reply.Kind != resp.KindNullArray && reply.Kind != resp.KindNull {
		t.Errorf("EXEC on a broken watch returned %v, want a nil array", arr(reply))
	}
	if got := text(c.do("GET", "k")); got != "2" {
		t.Errorf("an aborted transaction still wrote: %q", got)
	}
}

func TestWatchAllowsAnUncontestedTransaction(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("SET", "k", "1")
	c.do("WATCH", "k")
	c.do("MULTI")
	c.do("SET", "k", "2")
	if got := arr(c.do("EXEC")); got != "OK" {
		t.Errorf("an uncontested transaction returned %q", got)
	}
	if got := text(c.do("GET", "k")); got != "2" {
		t.Errorf("the transaction did not apply: %q", got)
	}
}

func TestUnwatchClearsTheWatch(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	other := ts.connect(t)
	c.do("SET", "k", "1")
	c.do("WATCH", "k")
	c.do("UNWATCH")
	other.do("SET", "k", "2")

	c.do("MULTI")
	c.do("SET", "k", "3")
	if got := arr(c.do("EXEC")); got != "OK" {
		t.Errorf("EXEC after UNWATCH returned %q", got)
	}
}

func TestWatchInsideMultiIsRefused(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("MULTI")
	if got := text(c.do("WATCH", "k")); !strings.Contains(got, "WATCH inside MULTI") {
		t.Errorf("WATCH inside MULTI returned %q", got)
	}
	c.do("DISCARD")
}

// TestWatchIsClearedByExec: a watch lasts for one transaction, so a second
// EXEC must not inherit the first's registrations.
func TestWatchIsClearedByExec(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("SET", "k", "1")
	c.do("WATCH", "k")
	c.do("MULTI")
	c.do("SET", "other", "x")
	c.do("EXEC")

	waitFor(t, "the watch registry to empty", func() bool {
		return ts.watchers.Count() == 0
	})
}

// TestCompareAndSetUnderContention is what WATCH is for. Many clients race
// to increment a counter with a read-modify-write loop, and the result must
// equal the number of successful transactions.
func TestCompareAndSetUnderContention(t *testing.T) {
	ts := startServer(t)
	seed := ts.connect(t)
	seed.do("SET", "cas", "0")

	const workers, each = 8, 40
	var wg sync.WaitGroup
	var applied int64
	var mu sync.Mutex

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := ts.connect(t)
			for i := 0; i < each; i++ {
				for {
					c.do("WATCH", "cas")
					cur := text(c.do("GET", "cas"))
					n := 0
					fmt.Sscanf(cur, "%d", &n)
					c.do("MULTI")
					c.do("SET", "cas", fmt.Sprint(n+1))
					reply := c.do("EXEC")
					if reply.Kind == resp.KindNullArray || reply.Kind == resp.KindNull {
						continue // someone else won; retry
					}
					mu.Lock()
					applied++
					mu.Unlock()
					break
				}
			}
		}()
	}
	wg.Wait()

	want := fmt.Sprint(applied)
	if got := text(seed.do("GET", "cas")); got != want {
		t.Errorf("the counter is %q after %s successful transactions; a lost "+
			"update means WATCH did not detect a concurrent write", got, want)
	}
	if applied != workers*each {
		t.Errorf("%d transactions applied, want %d", applied, workers*each)
	}
}

// TestExecExcludesOtherWrites is the atomicity promise: no other write lands
// between two commands of a transaction.
func TestExecExcludesOtherWrites(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := ts.connect(t)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			w.do("SET", "interloper", fmt.Sprint(i))
		}
	}()

	for round := 0; round < 200; round++ {
		c.do("MULTI")
		for i := 0; i < 10; i++ {
			c.do("SET", fmt.Sprintf("t%d", i), fmt.Sprint(round))
		}
		c.do("EXEC")
		// Every key in the transaction must carry this round's value: a
		// write that interleaved would leave one behind.
		for i := 0; i < 10; i++ {
			if got := text(c.do("GET", fmt.Sprintf("t%d", i))); got != fmt.Sprint(round) {
				t.Fatalf("round %d: t%d = %q", round, i, got)
			}
		}
	}
	close(stop)
	<-done
}

// TestTransactionRepliesAreNotInterleaved guards the array header: EXEC
// opens an array and every queued command writes into it.
func TestTransactionRepliesAreNotInterleaved(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("MULTI")
	c.do("SET", "a", "1")
	c.do("GET", "a")
	c.do("LPUSH", "l", "x", "y")
	c.do("LRANGE", "l", "0", "-1")

	reply := c.do("EXEC")
	if len(reply.Elems) != 4 {
		t.Fatalf("EXEC returned %d replies, want 4", len(reply.Elems))
	}
	if got := arr(reply.Elems[3]); got != "y x" {
		t.Errorf("the nested array reply is %q", got)
	}
}
