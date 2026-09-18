package server

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// waitFor polls until cond holds, so a test says what it is waiting for
// rather than sleeping and hoping.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pair starts a leader and a replica following it.
func pair(t *testing.T) (leader, replica *testServer, lc, rc *conn) {
	t.Helper()
	leader = durableServer(t, t.TempDir())
	replica = durableServer(t, t.TempDir())
	lc, rc = leader.connect(t), replica.connect(t)

	if got := text(rc.do("REPLICAOF", "127.0.0.1", strconv.Itoa(leader.port))); got != "OK" {
		t.Fatalf("REPLICAOF returned %q", got)
	}
	waitFor(t, "the link to come up", func() bool {
		return strings.Contains(text(rc.do("INFO", "replication")), "master_link_status:up")
	})
	return leader, replica, lc, rc
}

// waitOffset blocks until the replica has caught up with the leader.
func waitOffset(t *testing.T, leader, replica *testServer) {
	t.Helper()
	waitFor(t, "the replica to catch up", func() bool {
		st := replica.replicaLink()
		return st != nil && st.offset.Load() >= leader.replicationOffset()
	})
}

// TestReplicaFollowsLeader is the milestone in one test: writes on the
// leader appear on the replica, of every type.
func TestReplicaFollowsLeader(t *testing.T) {
	leader, replica, lc, rc := pair(t)

	lc.do("SET", "greeting", "hello")
	lc.do("INCR", "counter")
	lc.do("INCR", "counter")
	lc.do("RPUSH", "queue", "a", "b", "c")
	lc.do("HSET", "profile", "name", "ana")
	lc.do("SADD", "tags", "x", "y")
	lc.do("ZADD", "board", "10", "ana")
	lc.do("SET", "temporary", "v", "EX", "600")
	lc.do("SELECT", "3")
	lc.do("SET", "other-db", "yes")
	waitOffset(t, leader, replica)

	for _, tc := range []struct {
		want string
		args []string
	}{
		{"hello", []string{"GET", "greeting"}},
		{"2", []string{"GET", "counter"}},
		{"3", []string{"LLEN", "queue"}},
		{"ana", []string{"HGET", "profile", "name"}},
		{"2", []string{"SCARD", "tags"}},
		{"10", []string{"ZSCORE", "board", "ana"}},
	} {
		if got := text(rc.do(tc.args...)); got != tc.want {
			t.Errorf("replica %v = %q, want %q", tc.args, got, tc.want)
		}
	}
	if ttl := text(rc.do("TTL", "temporary")); ttl == "-1" || ttl == "-2" {
		t.Errorf("the TTL did not replicate: %q", ttl)
	}
	rc.do("SELECT", "3")
	if got := text(rc.do("GET", "other-db")); got != "yes" {
		t.Errorf("database 3 did not replicate: %q", got)
	}
}

// TestReplicaConvergesUnderAMixedWorkload is the differential test from §12
// run across a real socket between two processes' worth of state.
func TestReplicaConvergesUnderAMixedWorkload(t *testing.T) {
	leader, replica, lc, rc := pair(t)

	for i := 0; i < 2000; i++ {
		k := fmt.Sprintf("k%d", i%50)
		switch i % 10 {
		case 0:
			lc.do("SET", k, fmt.Sprint(i))
		case 1:
			lc.do("INCR", "counter"+fmt.Sprint(i%7))
		case 2:
			lc.do("APPEND", "acc"+fmt.Sprint(i%7), "z")
		case 3:
			lc.do("RPUSH", "list"+fmt.Sprint(i%5), fmt.Sprint(i))
		case 4:
			lc.do("LPOP", "list"+fmt.Sprint(i%5))
		case 5:
			lc.do("HINCRBYFLOAT", "hash"+fmt.Sprint(i%5), "f", "0.5")
		case 6:
			lc.do("ZADD", "z"+fmt.Sprint(i%5), fmt.Sprint(i%20), k)
		case 7:
			lc.do("SADD", "set"+fmt.Sprint(i%5), k)
		case 8:
			lc.do("DEL", k)
		case 9:
			lc.do("SET", k, fmt.Sprint(i), "EX", "600")
		}
	}
	waitOffset(t, leader, replica)

	for _, probe := range probeCommands() {
		want := text(lc.do(probe...))
		got := text(rc.do(probe...))
		if want != got {
			t.Errorf("%v: leader %q, replica %q", probe, want, got)
		}
	}
	if want, got := text(lc.do("DBSIZE")), text(rc.do("DBSIZE")); want != got {
		t.Errorf("DBSIZE: leader %s, replica %s", want, got)
	}
}

func probeCommands() [][]string {
	var out [][]string
	for i := 0; i < 50; i++ {
		out = append(out, []string{"GET", fmt.Sprintf("k%d", i)})
	}
	for i := 0; i < 7; i++ {
		out = append(out, []string{"GET", "counter" + fmt.Sprint(i)})
		out = append(out, []string{"GET", "acc" + fmt.Sprint(i)})
	}
	for i := 0; i < 5; i++ {
		out = append(out, []string{"LLEN", "list" + fmt.Sprint(i)})
		out = append(out, []string{"LINDEX", "list" + fmt.Sprint(i), "0"})
		out = append(out, []string{"HGET", "hash" + fmt.Sprint(i), "f"})
		out = append(out, []string{"ZCARD", "z" + fmt.Sprint(i)})
		out = append(out, []string{"SCARD", "set" + fmt.Sprint(i)})
	}
	return out
}

// TestReplicaIsReadOnly checks the guard that keeps a replica from diverging
// by being written to directly.
func TestReplicaIsReadOnly(t *testing.T) {
	_, _, _, rc := pair(t)
	if got := text(rc.do("SET", "local", "write")); !strings.HasPrefix(got, "READONLY") {
		t.Errorf("a write on a replica returned %q, want READONLY", got)
	}
	if got := text(rc.do("PING")); got != "PONG" {
		t.Errorf("reads stopped working on the replica: %q", got)
	}
}

// TestReplicaDoesNotExpireOnItsOwnClock covers FR-3.4: a replica hides an
// expired key but waits for the leader's DEL to remove it.
func TestReplicaDoesNotExpireOnItsOwnClock(t *testing.T) {
	leader, replica, lc, rc := pair(t)
	lc.do("SET", "brief", "v", "PX", "50")
	waitOffset(t, leader, replica)
	time.Sleep(120 * time.Millisecond)

	if got := text(rc.do("GET", "brief")); got != "<nil>" {
		t.Errorf("an expired key is visible on the replica: %q", got)
	}
	info := text(rc.do("INFO", "replication"))
	if !strings.Contains(info, "role:slave") {
		t.Errorf("the replica does not report its role:\n%s", info)
	}
}

// TestReplicaOfNoOnePromotes checks that a promoted replica keeps its data
// and starts accepting writes.
func TestReplicaOfNoOnePromotes(t *testing.T) {
	leader, replica, lc, rc := pair(t)
	lc.do("SET", "inherited", "yes")
	waitOffset(t, leader, replica)

	if got := text(rc.do("REPLICAOF", "NO", "ONE")); got != "OK" {
		t.Fatalf("REPLICAOF NO ONE returned %q", got)
	}
	if got := text(rc.do("GET", "inherited")); got != "yes" {
		t.Errorf("promotion lost the data: %q", got)
	}
	if got := text(rc.do("SET", "now-writable", "yes")); got != "OK" {
		t.Errorf("a promoted server still refuses writes: %q", got)
	}
	info := text(rc.do("INFO", "replication"))
	if !strings.Contains(info, "role:master") {
		t.Errorf("a promoted server still reports itself a replica:\n%s", info)
	}

	// Writes on the old leader must no longer reach it.
	lc.do("SET", "after-promotion", "no")
	time.Sleep(200 * time.Millisecond)
	if got := text(rc.do("GET", "after-promotion")); got != "<nil>" {
		t.Errorf("a promoted server is still applying the old leader's stream: %q", got)
	}
}

// TestReplicaResumesAfterLinkLoss covers the reconnect path, including the
// partial resynchronisation the leader offers.
func TestReplicaResumesAfterLinkLoss(t *testing.T) {
	leader, replica, lc, rc := pair(t)
	lc.do("SET", "before", "break")
	waitOffset(t, leader, replica)
	syncsBefore := replica.replicaLink().fullSync.Load()

	// Cancelling the leader's side of the link breaks it without shutting
	// either server down, and without disturbing ordinary clients.
	for _, l := range leader.replicaLinks() {
		l.cancel()
	}
	lc.do("SET", "during", "break")

	waitFor(t, "the link to come back", func() bool {
		st := replica.replicaLink()
		return st != nil && st.linkUp.Load() && st.offset.Load() >= leader.replicationOffset()
	})
	if got := text(rc.do("GET", "during")); got != "break" {
		t.Errorf("a write during the break was lost: %q", got)
	}
	if got := replica.replicaLink().fullSync.Load(); got != syncsBefore {
		t.Errorf("reconnecting cost a full resynchronisation (%d then %d); the "+
			"records were still retained, so it should have resumed",
			syncsBefore, got)
	}
}

// TestReplicaSurvivesCompactionOnTheLeader checks that a snapshot and prune
// under a live link does not break it.
func TestReplicaSurvivesCompactionOnTheLeader(t *testing.T) {
	leader, replica, lc, rc := pair(t)
	for i := 0; i < 500; i++ {
		lc.do("SET", fmt.Sprintf("k%d", i%20), fmt.Sprint(i))
	}
	lc.do("SAVE")
	lc.do("SET", "after-compaction", "yes")
	waitOffset(t, leader, replica)

	if got := text(rc.do("GET", "after-compaction")); got != "yes" {
		t.Errorf("the link broke across compaction: %q", got)
	}
	if got := text(rc.do("DBSIZE")); got != text(lc.do("DBSIZE")) {
		t.Errorf("DBSIZE diverged after compaction: replica %s, leader %s",
			got, text(lc.do("DBSIZE")))
	}
}

// TestLeaderSeesReplicaAcks closes the loop: the replica reports its offset
// and the leader records it.
func TestLeaderSeesReplicaAcks(t *testing.T) {
	leader, replica, lc, _ := pair(t)
	lc.do("SET", "a", "1")
	waitOffset(t, leader, replica)

	waitFor(t, "the leader to see an acknowledgement", func() bool {
		links := leader.replicaLinks()
		return len(links) == 1 && links[0].ack.Load() >= leader.replicationOffset()
	})
	info := text(lc.do("INFO", "replication"))
	if !strings.Contains(info, "connected_slaves:1") || !strings.Contains(info, "state=online") {
		t.Errorf("the leader does not report the replica:\n%s", info)
	}
}

func TestReplicaOfRejectsBadArguments(t *testing.T) {
	ts := durableServer(t, t.TempDir())
	c := ts.connect(t)
	for _, args := range [][]string{
		{"REPLICAOF", "", "6379"},
		{"REPLICAOF", "localhost", "0"},
		{"REPLICAOF", "localhost", "notaport"},
		{"REPLICAOF", "localhost", "70000"},
	} {
		if got := text(c.do(args...)); !strings.HasPrefix(got, "ERR") {
			t.Errorf("%v returned %q", args, got)
		}
	}
}

// TestWaitWithARealReplica is the property WAIT exists for: when it returns
// the count, those replicas really do hold the writes.
func TestWaitWithARealReplica(t *testing.T) {
	leader, replica, lc, rc := pair(t)

	for i := 0; i < 200; i++ {
		lc.do("SET", fmt.Sprintf("k%d", i), fmt.Sprint(i))
	}
	if got := text(lc.do("WAIT", "1", "10000")); got != "1" {
		t.Fatalf("WAIT returned %q, want 1", got)
	}

	// WAIT returning 1 means the replica has them now, not eventually.
	if got := text(rc.do("GET", "k199")); got != "199" {
		t.Errorf("WAIT said the replica was caught up, but k199 = %q", got)
	}
	if got := text(rc.do("DBSIZE")); got != text(lc.do("DBSIZE")) {
		t.Errorf("DBSIZE differs right after WAIT: replica %s, leader %s",
			got, text(lc.do("DBSIZE")))
	}
	_ = replica
	_ = leader
}

// TestWaitTimesOutRatherThanHanging covers asking for more replicas than
// exist.
func TestWaitTimesOutRatherThanHanging(t *testing.T) {
	_, _, lc, _ := pair(t)
	lc.do("SET", "k", "v")

	start := time.Now()
	got := text(lc.do("WAIT", "5", "300"))
	elapsed := time.Since(start)
	if got != "1" {
		t.Errorf("WAIT for 5 replicas returned %q, want the 1 that exists", got)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("WAIT returned after %v without waiting out its timeout", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("WAIT overran its timeout by a long way: %v", elapsed)
	}
}

// TestWaitWithZeroTimeoutDoesNotBlock pins the meaning of a zero timeout,
// which is "do not wait" and not "wait forever".
func TestWaitWithZeroTimeoutDoesNotBlock(t *testing.T) {
	_, _, lc, _ := pair(t)
	lc.do("SET", "k", "v")
	start := time.Now()
	lc.do("WAIT", "5", "0")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("WAIT with a zero timeout blocked for %v", elapsed)
	}
}

// TestMinReplicasToWriteWithARealReplica exercises the directive against a
// link that can actually be broken.
func TestMinReplicasToWriteWithARealReplica(t *testing.T) {
	leader, _, lc, _ := pair(t)
	if err := leader.cfg.Set("min-replicas-to-write", "1"); err != nil {
		t.Fatal(err)
	}
	if got := text(lc.do("SET", "ok", "1")); got != "OK" {
		t.Fatalf("a write with the replica connected returned %q", got)
	}

	for _, l := range leader.replicaLinks() {
		l.cancel()
	}
	waitFor(t, "the leader to notice the replica has gone", func() bool {
		return leader.ReplicasInSync() == 0
	})

	got := text(lc.do("SET", "refused", "1"))
	if !strings.HasPrefix(got, "NOREPLICAS") {
		t.Errorf("a write with no replicas returned %q, want NOREPLICAS", got)
	}
	// Reads keep working: the directive bounds loss, it does not stop the
	// server serving.
	if got := text(lc.do("GET", "ok")); got != "1" {
		t.Errorf("reads were refused too: %q", got)
	}
}
