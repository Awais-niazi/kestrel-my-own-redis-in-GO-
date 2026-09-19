package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kestrel/config"
	"kestrel/persist"
)

// followServer starts a server in dir that immediately replicates from
// leader, the way a restarted replica does from its config file.
func followServer(t *testing.T, dir string, leader *testServer) *testServer {
	t.Helper()
	return durableServer(t, dir, func(c *config.Config) {
		must(t, c.LoadFlags([]string{
			"--replicaof", fmt.Sprintf("127.0.0.1:%d", leader.port),
		}))
	})
}

// TestReplicaPersistsWhatItApplies is the base of this chunk: the records
// the leader sent are in the replica's own log, byte for byte.
func TestReplicaPersistsWhatItApplies(t *testing.T) {
	leader, replica, lc, _ := pair(t)
	for i := 0; i < 50; i++ {
		lc.do("SET", fmt.Sprintf("k%d", i), fmt.Sprint(i))
	}
	waitOffset(t, leader, replica)

	if got, want := replica.persist.log.Offset(), leader.replicationOffset(); got != want {
		t.Errorf("the replica's log ends at %d, the leader's at %d; offsets must match",
			got, want)
	}
	segs, err := persist.Segments(replica.persist.dir)
	if err != nil || len(segs) == 0 {
		t.Fatalf("the replica wrote no log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(replica.persist.dir, leaderFileName)); err != nil {
		t.Errorf("the replica did not record whose history it holds: %v", err)
	}
}

// TestReplicaRestartResumesPartially is what persisting buys: a restarted
// replica quotes an offset its leader recognises and needs no transfer.
func TestReplicaRestartResumesPartially(t *testing.T) {
	leaderDir, replicaDir := t.TempDir(), t.TempDir()
	leader := durableServer(t, leaderDir)
	lc := leader.connect(t)
	lc.do("SET", "early", "value")

	replica := followServer(t, replicaDir, leader)
	waitFor(t, "the first link", func() bool {
		st := replica.replicaLink()
		return st != nil && st.linkUp.Load()
	})
	waitOffset(t, leader, replica)
	stop(t, replica)

	// Writes arrive while the replica is down, so the restart has something
	// to catch up on.
	lc.do("SET", "while-down", "value")
	lc.do("INCR", "counter")

	again := followServer(t, replicaDir, leader)
	waitFor(t, "the link after a restart", func() bool {
		st := again.replicaLink()
		return st != nil && st.linkUp.Load()
	})
	waitOffset(t, leader, again)

	rc := again.connect(t)
	if got := text(rc.do("GET", "while-down")); got != "value" {
		t.Errorf("a write made while the replica was down did not arrive: %q", got)
	}
	if got := text(rc.do("GET", "early")); got != "value" {
		t.Errorf("the restarted replica lost data it already had: %q", got)
	}
	if n := again.replicaLink().fullSync.Load(); n != 0 {
		t.Errorf("the restart cost %d full resynchronisations; the records were "+
			"still retained, so it should have resumed from its own log", n)
	}
}

// TestReplicaWithIncompleteStateResynchronises is the crash this chunk's
// invariant exists for. A snapshot covering the log up to some offset, with
// a log that stops short of it, describes shards at different instants and
// no single offset describes them.
func TestReplicaWithIncompleteStateResynchronises(t *testing.T) {
	leaderDir, replicaDir := t.TempDir(), t.TempDir()
	leader := durableServer(t, leaderDir)
	lc := leader.connect(t)
	// Enough data that the replica's own snapshot pass takes long enough
	// for records to arrive during it. A pass that completes between two
	// records produces a window with First equal to Last, which is a
	// self-contained snapshot and cannot be short of anything.
	for i := 0; i < 20000; i++ {
		lc.do("SET", fmt.Sprintf("k%d", i), "a value of some realistic length")
	}

	replica := followServer(t, replicaDir, leader)
	waitFor(t, "the first link", func() bool {
		st := replica.replicaLink()
		return st != nil && st.linkUp.Load()
	})
	waitOffset(t, leader, replica)
	// The replica takes its own snapshot, so its local state has a window.
	saveUnderLoad(t, leader, replica.connect(t))
	lc.do("SET", "after", "snapshot")
	waitOffset(t, leader, replica)
	stop(t, replica)

	// Truncate the replica's log to before the end of its snapshot's
	// window, which is what a crash during a transfer leaves behind.
	last := replica.persist.snapshotLast.Load()
	if first := replica.persist.log.OldestOffset(); last <= first {
		t.Skipf("the replica's snapshot window is a single point (%d); nothing "+
			"can be short of it", last)
	}
	cutLogShortOf(t, replicaDir, last)

	again := followServer(t, replicaDir, leader)
	waitFor(t, "a full resynchronisation", func() bool {
		st := again.replicaLink()
		return st != nil && st.linkUp.Load() && st.fullSync.Load() > 0
	})
	waitOffset(t, leader, again)

	rc := again.connect(t)
	if got := text(rc.do("GET", "after")); got != "snapshot" {
		t.Errorf("the resynchronised replica is missing data: %q", got)
	}
	if got := text(rc.do("DBSIZE")); got != text(lc.do("DBSIZE")) {
		t.Errorf("DBSIZE after resynchronising: replica %s, leader %s",
			got, text(lc.do("DBSIZE")))
	}
}

// TestLeaderWithIncompleteStateRefusesToStart is the same check on a node
// that has nowhere to resynchronise from. Coming up with a hole in the
// dataset and reporting success is the worst available outcome.
func TestLeaderWithIncompleteStateRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	for i := 0; i < 100; i++ {
		c.do("SET", fmt.Sprintf("k%d", i), fmt.Sprint(i))
	}
	saveUnderLoad(t, ts, c)
	c.do("SET", "after", "snapshot")
	last := ts.persist.snapshotLast.Load()
	stop(t, ts)

	cutLogShortOf(t, dir, last)

	cfg := config.Default()
	must(t, cfg.LoadFlags([]string{
		"--port", strconv.Itoa(freePort(t)),
		"--admin-port", strconv.Itoa(freePort(t)),
		"--loglevel", "error", "--appendonly", "yes", "--dir", dir,
	}))
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = srv.Serve(context.Background())
	if err == nil {
		t.Fatal("the server started on an incomplete dataset")
	}
	if !strings.Contains(err.Error(), "cannot be rebuilt") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
}

// TestReplicaCompactsItsOwnLog checks that a replica's log does not grow
// without bound just because its writes arrive over a socket.
func TestReplicaCompactsItsOwnLog(t *testing.T) {
	leader, replica, lc, rc := pair(t)
	for i := 0; i < 2000; i++ {
		lc.do("SET", fmt.Sprintf("k%d", i%30), fmt.Sprint(i))
	}
	waitOffset(t, leader, replica)

	before := logBytes(t, replica.persist.dir)
	if got := text(rc.do("SAVE")); got != "OK" {
		t.Fatalf("SAVE on a replica returned %q", got)
	}
	after := logBytes(t, replica.persist.dir)
	if after >= before {
		t.Errorf("the replica's log is %d bytes after compaction, was %d", after, before)
	}

	// It must still be following afterwards, and still correct.
	lc.do("SET", "after-compaction", "yes")
	waitOffset(t, leader, replica)
	if got := text(rc.do("GET", "after-compaction")); got != "yes" {
		t.Errorf("the replica stopped following after compacting: %q", got)
	}
	if got := text(rc.do("DBSIZE")); got != text(lc.do("DBSIZE")) {
		t.Errorf("DBSIZE: replica %s, leader %s", got, text(lc.do("DBSIZE")))
	}
}

// cutLogShortOf leaves the data directory in the shape a crash during a
// transfer does: a snapshot whose window ends at last, and a log that stops
// before it. The live segment is removed and the one straddling the window
// is cut back, because truncating the live segment alone leaves the log
// ending exactly where the window does, which is complete rather than torn.
func cutLogShortOf(t *testing.T, dir string, last uint64) {
	t.Helper()
	segs, err := persist.Segments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var straddler *persist.Segment
	for i := range segs {
		if segs[i].Base < last && segs[i].End >= last {
			straddler = &segs[i]
			break
		}
	}
	if straddler == nil {
		t.Skipf("no segment straddles the snapshot window end %d (segments %+v)", last, segs)
	}
	for _, sg := range segs {
		if sg.Seq > straddler.Seq {
			if err := os.Remove(sg.Path); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Cut the straddler back to its header, so the log ends at its base.
	if err := os.Truncate(straddler.Path, 32); err != nil {
		t.Fatal(err)
	}
	if straddler.Base >= last {
		t.Skipf("the straddling segment begins at %d, not before %d", straddler.Base, last)
	}
}

// saveUnderLoad takes a snapshot on saveOn while writes arrive at writer, so
// that the snapshot's anchors span a real window rather than a single
// instant. A quiet snapshot has First equal to Last and there is nothing to
// be short of.
//
// On a replica the load has to come from its leader: its own port refuses
// writes, so a writer pointed at it would produce nothing but READONLY.
func saveUnderLoad(t *testing.T, writer *testServer, saveOn *conn) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := writer.connect(t)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			w.do("SET", fmt.Sprintf("hot%d", i%32), fmt.Sprint(i))
		}
	}()
	if got := text(saveOn.do("SAVE")); got != "OK" {
		close(stop)
		<-done
		t.Fatalf("SAVE returned %q", got)
	}
	close(stop)
	<-done
}
