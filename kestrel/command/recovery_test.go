package command

import (
	"path/filepath"
	"testing"

	"kestrel/persist"
)

// recordWorkload runs a randomized write stream against a fresh host and
// writes every effect it produces to a real log file. It returns the host
// and the log's path.
func recordWorkload(t *testing.T, seed int64, iterations int) (*testHost, string) {
	t.Helper()
	leader := newTestHost(t)
	path := filepath.Join(t.TempDir(), "kestrel.log")

	log, err := persist.Create(persist.Options{Path: path, Fsync: persist.FsyncNo})
	if err != nil {
		t.Fatal(err)
	}
	leader.mu.Lock()
	leader.sink = func(db int, args [][]byte) {
		if _, err := log.Append(db, args); err != nil {
			t.Error(err)
		}
	}
	leader.mu.Unlock()

	randomWorkload(t, leader, seed, iterations)

	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// The log is closed, but the leader stays alive for comparison and will
	// still emit expiry DELs when it is read. They have nowhere to go now.
	leader.mu.Lock()
	leader.sink = func(db int, args [][]byte) {}
	leader.mu.Unlock()
	return leader, path
}

// recoverInto replays a log file into a fresh host, with the keyspace in
// loading mode for the duration and nothing propagating back out.
func recoverInto(t *testing.T, path string, clock int64) (*testHost, persist.Result) {
	t.Helper()
	h := newTestHost(t)
	h.now.Store(clock)
	h.mu.Lock()
	h.sink = func(db int, args [][]byte) {
		t.Error("a replayed effect propagated back into a log")
	}
	h.mu.Unlock()

	h.ks.SetLoading(true)
	res, err := persist.Recover(persist.RecoverOptions{Path: path}, NewReplayer(h))
	h.ks.SetLoading(false)

	// The guard above only covers the replay. Once loading ends the host is
	// an ordinary one, and reading it expires keys and emits DELs like any
	// other.
	h.mu.Lock()
	h.sink = func(db int, args [][]byte) {}
	h.mu.Unlock()

	if err != nil {
		t.Fatalf("recovery failed after %d records: %v", res.Records, err)
	}
	return h, res
}

// TestRecoveryConverges is the M3 counterpart of TestFollowerConverges: a
// randomized write stream is recorded to a real log file, the file is
// replayed into an empty keyspace, and the two must hold exactly the same
// data.
//
// It goes through the file rather than through a function call on purpose.
// Everything between the effect and the replayed command -- the framing, the
// checksum, the RESP encoding, the decoder -- is in the path being tested,
// and a bug in any of it is a dataset that does not survive a restart.
func TestRecoveryConverges(t *testing.T) {
	leader, path := recordWorkload(t, 20240914, 12000)

	recovered, res := recoverInto(t, path, leader.now.Load())
	if res.Records == 0 {
		t.Fatal("the log was empty; the test proves nothing")
	}
	t.Logf("replayed %d records, %d bytes of log", res.Records, res.Offset)

	want := dump(t, leader)
	got := dump(t, recovered)
	if want != got {
		t.Errorf("the recovered keyspace differs from the leader at %s", firstDiff(want, got))
	}
}

// TestRecoveryUnderALaterClock is the case a real restart always is: the log
// was written in the past and is replayed now, so every TTL in it has
// already elapsed.
//
// Without loading mode the replay diverges -- a key expires part-way through
// and the next record in the log acts on a key the leader still had. The
// comparison is against the leader advanced to the same later instant, which
// is what the recovered server must look like.
func TestRecoveryUnderALaterClock(t *testing.T) {
	leader, path := recordWorkload(t, 20240915, 8000)

	// An hour of TTLs in the workload have all elapsed by now.
	later := leader.now.Load() + 3_600_000
	recovered, res := recoverInto(t, path, later)
	if res.Records == 0 {
		t.Fatal("the log was empty; the test proves nothing")
	}

	// Bring the leader to the same instant and let it reap, so the two are
	// being asked the same question.
	leader.now.Store(later)
	leader.ks.ExpirePass(farFuture())
	recovered.ks.ExpirePass(farFuture())

	want := dump(t, leader)
	got := dump(t, recovered)
	if want != got {
		t.Errorf("recovery under a later clock diverged at %s", firstDiff(want, got))
	}
}

// TestRecoveryIsIdempotentAcrossRestarts covers the second restart. The log
// is replayed, more writes are appended to the same file, and the whole
// thing is replayed again into a third keyspace.
func TestRecoveryIsIdempotentAcrossRestarts(t *testing.T) {
	leader, path := recordWorkload(t, 20240916, 4000)

	first, res := recoverInto(t, path, leader.now.Load())
	if res.Records == 0 {
		t.Fatal("nothing was replayed")
	}

	// Reopen the same log and keep writing, as a restarted server would.
	log, err := persist.Open(persist.Options{Path: path, Fsync: persist.FsyncNo})
	if err != nil {
		t.Fatal(err)
	}
	if log.Offset() != res.Offset {
		t.Fatalf("reopened at %d, recovery ended at %d", log.Offset(), res.Offset)
	}
	first.mu.Lock()
	first.sink = func(db int, args [][]byte) {
		if _, err := log.Append(db, args); err != nil {
			t.Error(err)
		}
	}
	first.mu.Unlock()

	s := newSession(t, first)
	s.do("SET", "after-restart", "yes")
	s.do("RPUSH", "after-restart-list", "a", "b", "c")
	s.do("HSET", "after-restart-hash", "f", "v")
	s.do("DEL", "k1")
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	second, res2 := recoverInto(t, path, first.now.Load())
	if res2.Records <= res.Records {
		t.Errorf("the second recovery read %d records, want more than %d",
			res2.Records, res.Records)
	}

	want := dump(t, first)
	got := dump(t, second)
	if want != got {
		t.Errorf("the second restart diverged at %s", firstDiff(want, got))
	}
}

// TestReplayRejectsWhatIsNotAnEffect guards the boundary. The log should
// only ever hold write commands, and a reader that will run anything it
// finds is a file-format-shaped hole in the server.
func TestReplayRejectsWhatIsNotAnEffect(t *testing.T) {
	h := newTestHost(t)
	r := NewReplayer(h)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"read command", []string{"GET", "k"}},
		{"unknown command", []string{"NOTACOMMAND", "k"}},
		{"connection command", []string{"SELECT", "1"}},
		{"wrong arity", []string{"SET"}},
		{"empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := make([][]byte, len(tc.args))
			for i, a := range tc.args {
				args[i] = []byte(a)
			}
			if err := r.Apply(0, 0, args); err == nil {
				t.Errorf("%v was replayed", tc.args)
			}
		})
	}
}

func TestReplayAppliesToTheNamedDatabase(t *testing.T) {
	h := newTestHost(t)
	r := NewReplayer(h)
	if err := r.Apply(3, 0, [][]byte{[]byte("SET"), []byte("k"), []byte("v")}); err != nil {
		t.Fatal(err)
	}
	s := newSession(t, h)
	if got := str(s.do("GET", "k")); got != "<nil>" {
		t.Errorf("database 0 got the write: %q", got)
	}
	s.do("SELECT", "3")
	if got := str(s.do("GET", "k")); got != "v" {
		t.Errorf("database 3 holds %q, want \"v\"", got)
	}
}

func TestReplayReportsTheFailingCommand(t *testing.T) {
	h := newTestHost(t)
	r := NewReplayer(h)
	if err := r.Apply(0, 0, [][]byte{[]byte("SET"), []byte("k"), []byte("v")}); err != nil {
		t.Fatal(err)
	}
	err := r.Apply(0, 0, [][]byte{[]byte("LPUSH"), []byte("k"), []byte("x")})
	if err == nil {
		t.Fatal("a WRONGTYPE replay was accepted")
	}
	if got := err.Error(); !contains(got, "WRONGTYPE") || !contains(got, "LPUSH") {
		t.Errorf("error %q names neither the failure nor the command", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestIncrByFloatKeepsTTLThroughAReplay is a regression test for a bug the
// recovery convergence test found.
//
// INCRBYFLOAT leaves a key's expiry alone, but its canonical effect is a
// SET, and a plain SET clears it. The log therefore said "this key is
// permanent" about a key that was not, so every replica and every restart
// quietly resurrected keys that should have expired. Nothing caught it until
// a replay ran against a clock far enough ahead for the difference to show.
//
// The effect table asserts the KEEPTTL argument; this asserts the property
// the argument is there for, so that removing it fails on meaning rather
// than on a string comparison.
func TestIncrByFloatKeepsTTLThroughAReplay(t *testing.T) {
	leader := newTestHost(t)
	path := filepath.Join(t.TempDir(), "kestrel.log")
	log, err := persist.Create(persist.Options{Path: path, Fsync: persist.FsyncNo})
	if err != nil {
		t.Fatal(err)
	}
	leader.mu.Lock()
	leader.sink = func(db int, args [][]byte) {
		if _, err := log.Append(db, args); err != nil {
			t.Error(err)
		}
	}
	leader.mu.Unlock()

	s := newSession(t, leader)
	s.do("SET", "counter", "1.0")
	s.do("PEXPIREAT", "counter", "1700000900000")
	s.do("INCRBYFLOAT", "counter", "0.5")
	if got := str(s.do("PTTL", "counter")); got == "-1" {
		t.Fatal("precondition: INCRBYFLOAT cleared the TTL on the leader itself")
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	leader.mu.Lock()
	leader.sink = func(db int, args [][]byte) {}
	leader.mu.Unlock()

	recovered, _ := recoverInto(t, path, leader.now.Load())
	rs := newSession(t, recovered)
	if got := str(rs.do("GET", "counter")); got != "1.5" {
		t.Errorf("recovered value is %q, want \"1.5\"", got)
	}
	if got := str(rs.do("PTTL", "counter")); got == "-1" {
		t.Error("the replay made an expiring key permanent")
	}
	if want, got := str(s.do("PTTL", "counter")), str(rs.do("PTTL", "counter")); want != got {
		t.Errorf("recovered TTL is %s, leader has %s", got, want)
	}
}
