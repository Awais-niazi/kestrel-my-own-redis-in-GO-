package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kestrel/config"
	"kestrel/persist"
)

func logBytes(t *testing.T, dir string) int64 {
	t.Helper()
	segs, err := persist.Segments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, s := range segs {
		fi, err := os.Stat(s.Path)
		if err != nil {
			t.Fatal(err)
		}
		n += fi.Size()
	}
	return n
}

// TestSaveCompactsTheLog is what M3 was for: the log stops growing, and the
// data is still there afterwards.
func TestSaveCompactsTheLog(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)

	// A workload that rewrites the same keys, so the log is mostly dead
	// records and a snapshot of the live data is far smaller.
	for i := 0; i < 4000; i++ {
		c.do("SET", fmt.Sprintf("k%d", i%50), fmt.Sprintf("value-%d", i))
	}
	before := logBytes(t, dir)
	if before == 0 {
		t.Fatal("nothing was written to the log")
	}

	if got := text(c.do("SAVE")); got != "OK" {
		t.Fatalf("SAVE returned %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotFileName)); err != nil {
		t.Fatalf("SAVE wrote no snapshot: %v", err)
	}

	// The segments the snapshot covered are gone, so the log is a fraction
	// of what it was.
	after := logBytes(t, dir)
	if after >= before {
		t.Errorf("the log is %d bytes after compaction and was %d before", after, before)
	}
	segs, err := persist.Segments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Errorf("%d segments remain after a snapshot with no writes since, want 1", len(segs))
	}

	// And the data survives a restart built from the snapshot plus what is
	// left of the log.
	c.do("SET", "after-save", "yes")
	stop(t, ts)

	second := durableServer(t, dir)
	d := second.connect(t)
	if got := text(d.do("GET", "k7")); !strings.HasPrefix(got, "value-") {
		t.Errorf("k7 = %q after a compacted restart", got)
	}
	if got := text(d.do("DBSIZE")); got != "51" {
		t.Errorf("DBSIZE = %q after a compacted restart, want 51", got)
	}
	if got := text(d.do("GET", "after-save")); got != "yes" {
		t.Errorf("the write after the snapshot was lost: %q", got)
	}
}

// TestRepeatedSavesKeepTheLogBounded is the property that makes the whole
// scheme worth having.
func TestRepeatedSavesKeepTheLogBounded(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)

	var sizes []int64
	for round := 0; round < 4; round++ {
		for i := 0; i < 2000; i++ {
			c.do("SET", fmt.Sprintf("k%d", i%40), fmt.Sprintf("r%d-%d", round, i))
		}
		if got := text(c.do("SAVE")); got != "OK" {
			t.Fatalf("round %d: SAVE returned %q", round, got)
		}
		sizes = append(sizes, logBytes(t, dir))
	}
	t.Logf("log bytes after each save: %v", sizes)

	// Without compaction these would grow without bound. They must instead
	// settle, so the last is no larger than the second by any margin that
	// would indicate accumulation.
	if sizes[3] > sizes[1]*2 {
		t.Errorf("the log is still growing across saves: %v", sizes)
	}
	stop(t, ts)

	second := durableServer(t, dir)
	d := second.connect(t)
	if got := text(d.do("DBSIZE")); got != "40" {
		t.Errorf("DBSIZE = %q after four compactions, want 40", got)
	}
	if got := text(d.do("GET", "k5")); !strings.HasPrefix(got, "r3-") {
		t.Errorf("k5 = %q, want the last round's value", got)
	}
}

func TestBgSaveAndBgRewriteAof(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "k", "v")

	if got := text(c.do("BGSAVE")); got != "Background saving started" {
		t.Errorf("BGSAVE returned %q", got)
	}
	waitForSnapshot(t, ts)
	if _, err := os.Stat(filepath.Join(dir, snapshotFileName)); err != nil {
		t.Fatalf("BGSAVE wrote no snapshot: %v", err)
	}

	c.do("SET", "k2", "v2")
	if got := text(c.do("BGREWRITEAOF")); got != "Background append only file rewriting started" {
		t.Errorf("BGREWRITEAOF returned %q", got)
	}
	waitForSnapshot(t, ts)

	stop(t, ts)
	second := durableServer(t, dir)
	d := second.connect(t)
	if got := text(d.do("GET", "k2")); got != "v2" {
		t.Errorf("k2 = %q after a background compaction", got)
	}
}

// waitForSnapshot blocks until no snapshot is in flight.
func waitForSnapshot(t *testing.T, ts *testServer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !ts.persist.running.Load() && ts.persist.lastSave.Load() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the snapshot did not finish within 10s")
}

func TestLastSave(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	before := text(c.do("LASTSAVE"))
	c.do("SET", "k", "v")
	c.do("SAVE")
	after := text(c.do("LASTSAVE"))
	if before == "0" {
		t.Error("LASTSAVE was 0 before any save; it should report startup time")
	}
	if after < before {
		t.Errorf("LASTSAVE went backwards: %q then %q", before, after)
	}
}

func TestSaveWithoutPersistenceIsRefused(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	for _, cmd := range []string{"SAVE", "BGSAVE", "BGREWRITEAOF"} {
		if got := text(c.do(cmd)); !strings.Contains(got, "persistence is disabled") {
			t.Errorf("%s on a non-persisting server returned %q", cmd, got)
		}
	}
	if got := text(c.do("LASTSAVE")); got != "0" {
		t.Errorf("LASTSAVE = %q with persistence off, want 0", got)
	}
}

// TestSnapshotIntervalTriggers checks the scheduler, with the interval turned
// down to a second.
func TestSnapshotIntervalTriggers(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir, func(c *config.Config) {
		must(t, c.Set("snapshot-interval", "1"))
	})
	c := ts.connect(t)
	c.do("SET", "k", "v")

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, snapshotFileName)); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the interval never produced a snapshot")
}

// TestIdleServerDoesNotSnapshot is the other half of the schedule. A
// snapshot that would change nothing still costs a full serialization pass
// and a shard-blocking pause.
func TestIdleServerDoesNotSnapshot(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir, func(c *config.Config) {
		must(t, c.Set("snapshot-interval", "1"))
	})
	c := ts.connect(t)
	c.do("SET", "k", "v")
	c.do("SAVE")
	first := ts.persist.lastSave.Load()

	// Nothing is written from here on, so the interval must not fire again.
	time.Sleep(2500 * time.Millisecond)
	if got := ts.persist.lastSave.Load(); got != first {
		t.Errorf("an idle server snapshotted again: %d then %d", first, got)
	}

	// A single write makes it due again.
	c.do("SET", "k2", "v2")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ts.persist.lastSave.Load() > first {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("a write did not make the next interval snapshot due")
}

// TestGrowthTriggers covers the auto-rewrite directives.
func TestGrowthTriggers(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir, func(c *config.Config) {
		must(t, c.Set("snapshot-interval", "0")) // only growth may fire
		must(t, c.Set("auto-rewrite-min-size", "16kb"))
		must(t, c.Set("auto-rewrite-percentage", "100"))
	})
	c := ts.connect(t)
	firstSave := ts.persist.lastSave.Load()

	// Write past auto-rewrite-min-size and keep going until the maintenance
	// loop notices. The loop checks once a second, so this has to outlast a
	// tick rather than stop as soon as the log is big enough.
	deadline := time.Now().Add(15 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		c.do("SET", fmt.Sprintf("k%d", i%30), fmt.Sprintf("value-%d-%s", i,
			strings.Repeat("x", 64)))
		if ts.persist.lastSave.Load() > firstSave {
			if _, err := os.Stat(filepath.Join(dir, snapshotFileName)); err != nil {
				t.Fatalf("a save was recorded but no snapshot exists: %v", err)
			}
			return
		}
	}
	t.Fatalf("growth never triggered a snapshot; log is %d bytes", logBytes(t, dir))
}

// TestSnapshotUnderLoad takes a snapshot while writes keep arriving, then
// restarts. This is the concurrency the restore test covers at the command
// layer, run against a real server over a socket.
func TestSnapshotUnderLoad(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := ts.connect(t)
		for i := 0; ; i++ {
			select {
			case <-stopCh:
				return
			default:
			}
			w.do("INCR", fmt.Sprintf("counter%d", i%16))
			w.do("RPUSH", fmt.Sprintf("list%d", i%8), fmt.Sprint(i))
			w.do("SET", fmt.Sprintf("k%d", i%64), fmt.Sprint(i))
		}
	}()
	time.Sleep(150 * time.Millisecond)

	c := ts.connect(t)
	if got := text(c.do("SAVE")); got != "OK" {
		t.Fatalf("SAVE under load returned %q", got)
	}
	time.Sleep(150 * time.Millisecond)
	close(stopCh)
	<-done

	before := map[string]string{}
	for i := 0; i < 16; i++ {
		k := fmt.Sprintf("counter%d", i)
		before[k] = text(c.do("GET", k))
	}
	for i := 0; i < 8; i++ {
		k := fmt.Sprintf("list%d", i)
		before[k] = text(c.do("LLEN", k))
	}
	stop(t, ts)

	second := durableServer(t, dir)
	d := second.connect(t)
	for i := 0; i < 16; i++ {
		k := fmt.Sprintf("counter%d", i)
		if got := text(d.do("GET", k)); got != before[k] {
			t.Errorf("%s = %q after restart, was %q: a counter replayed twice or not at all",
				k, got, before[k])
		}
	}
	for i := 0; i < 8; i++ {
		k := fmt.Sprintf("list%d", i)
		if got := text(d.do("LLEN", k)); got != before[k] {
			t.Errorf("%s has %q entries after restart, had %q", k, got, before[k])
		}
	}
}
