package command

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"kestrel/persist"
)

// memSink collects a snapshot without touching a file, so a test can assert
// on the commands the walker produces.
type memSink struct {
	anchors []string
	records []string
	failAt  int
}

func (m *memSink) Anchor(db, shard int, offset uint64) error {
	m.anchors = append(m.anchors, fmt.Sprintf("db%d/shard%d@%d", db, shard, offset))
	return nil
}

func (m *memSink) Record(db int, args [][]byte) error {
	if m.failAt > 0 && len(m.records) == m.failAt-1 {
		return fmt.Errorf("sink is full")
	}
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = string(a)
	}
	m.records = append(m.records, fmt.Sprintf("%d:%s", db, strings.Join(parts, " ")))
	return nil
}

func snapshotOptions(offset uint64) SnapshotOptions {
	return SnapshotOptions{Offset: func() uint64 { return offset }}
}

// writeSnapshotFile takes a snapshot of h into a real file and returns its
// path.
func writeSnapshotFile(t *testing.T, h *testHost, offset uint64, batch int) (string, persist.SnapshotInfo) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kestrel.snapshot")
	w, err := persist.CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := SnapshotOptions{Offset: func() uint64 { return offset }, BatchElements: batch}
	if err := WriteSnapshot(h.ks, w, opts); err != nil {
		w.Abort()
		t.Fatal(err)
	}
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	return path, info
}

// loadSnapshotInto replays a snapshot file into a fresh host.
func loadSnapshotInto(t *testing.T, path string, clock int64) (*testHost, persist.SnapshotLoad) {
	t.Helper()
	h := newTestHost(t)
	h.now.Store(clock)
	h.mu.Lock()
	h.sink = func(db int, args [][]byte) {
		t.Error("loading a snapshot propagated an effect")
	}
	h.mu.Unlock()

	h.ks.SetLoading(true)
	load, err := persist.LoadSnapshot(path, NewReplayer(h))
	h.ks.SetLoading(false)

	h.mu.Lock()
	h.sink = func(db int, args [][]byte) {}
	h.mu.Unlock()

	if err != nil {
		t.Fatalf("loading the snapshot failed after %d records: %v", load.Records, err)
	}
	return h, load
}

// TestSnapshotRoundTrip is the proof for chunk D: a dataset written to a
// snapshot file and loaded into an empty keyspace must be the same dataset,
// key for key, value for value, TTL for TTL.
func TestSnapshotRoundTrip(t *testing.T) {
	leader := newTestHost(t)
	randomWorkload(t, leader, 20240917, 9000)
	leader.mu.Lock()
	leader.sink = func(db int, args [][]byte) {}
	leader.mu.Unlock()

	path, info := writeSnapshotFile(t, leader, 4242, 0)
	if info.Records == 0 {
		t.Fatal("the snapshot is empty; the test proves nothing")
	}
	t.Logf("snapshot: %d anchors, %d records, %d bytes", info.Anchors, info.Records, info.Size)

	restored, load := loadSnapshotInto(t, path, leader.now.Load())
	if load.Records != info.Records {
		t.Errorf("loaded %d records, wrote %d", load.Records, info.Records)
	}
	if load.First != 4242 || load.Last != 4242 {
		t.Errorf("window is [%d,%d], want [4242,4242]", load.First, load.Last)
	}
	if got, want := len(load.Anchors), leader.ks.NumDatabases()*leader.ks.DB(0).NumShards(); got != want {
		t.Errorf("snapshot covers %d shards, keyspace has %d", got, want)
	}

	want := dump(t, leader)
	got := dump(t, restored)
	if want != got {
		t.Errorf("the restored keyspace differs at %s", firstDiff(want, got))
	}
}

// TestSnapshotRoundTripAcrossBatchSizes runs the same proof with batch sizes
// that force collections to be split across many records, including an odd
// size that would break a field/value pair if the stride were ignored.
func TestSnapshotRoundTripAcrossBatchSizes(t *testing.T) {
	for _, batch := range []int{1, 2, 3, 7, 512} {
		t.Run(fmt.Sprint("batch", batch), func(t *testing.T) {
			leader := newTestHost(t)
			randomWorkload(t, leader, 20240918, 2500)
			leader.mu.Lock()
			leader.sink = func(db int, args [][]byte) {}
			leader.mu.Unlock()

			path, _ := writeSnapshotFile(t, leader, 1, batch)
			restored, _ := loadSnapshotInto(t, path, leader.now.Load())

			want := dump(t, leader)
			got := dump(t, restored)
			if want != got {
				t.Errorf("batch %d lost data at %s", batch, firstDiff(want, got))
			}
		})
	}
}

// TestSnapshotAnchorsAreOrdered pins the invariant the recovery filter rests
// on. Shards must be visited in ascending order, databases likewise.
func TestSnapshotAnchorsAreOrdered(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "k", "v")

	var n uint64
	sink := &memSink{}
	// A counter for an offset makes any reordering visible, because the
	// anchors would then not be ascending.
	opts := SnapshotOptions{Offset: func() uint64 { n++; return n }}
	if err := WriteSnapshot(h.ks, sink, opts); err != nil {
		t.Fatal(err)
	}

	shards := h.ks.DB(0).NumShards()
	want := make([]string, 0, len(sink.anchors))
	for db := 0; db < h.ks.NumDatabases(); db++ {
		for sh := 0; sh < shards; sh++ {
			want = append(want, fmt.Sprintf("db%d/shard%d@%d", db, sh, len(want)+1))
		}
	}
	if strings.Join(sink.anchors, ",") != strings.Join(want, ",") {
		t.Errorf("anchors were emitted as\n%v\nwant\n%v", sink.anchors, want)
	}
}

// TestSnapshotRejectsBackwardsAnchor covers the writer's own check. The
// walker emits anchors in order, so this can only fire if something else
// drives the writer -- which is exactly when the check earns its place.
func TestSnapshotRejectsBackwardsAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	w, err := persist.CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	if err := w.Anchor(0, 0, 100); err != nil {
		t.Fatal(err)
	}
	if err := w.Anchor(0, 1, 99); err == nil {
		t.Error("an anchor that went backwards was accepted")
	}
}

func TestSnapshotRecordBeforeAnchorIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	w, err := persist.CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	if err := w.Record(0, [][]byte{[]byte("SET"), []byte("k"), []byte("v")}); err == nil {
		t.Error("a record before any anchor was accepted")
	}
}

// TestSnapshotAppearsOnlyOnCommit is the property that makes a snapshot safe
// to take over a live one: until Commit succeeds there is nothing at the
// real path, so a crash leaves the previous snapshot in place.
func TestSnapshotAppearsOnlyOnCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kestrel.snapshot")

	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "first", "1")

	w, err := persist.CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshot(h.ks, w, snapshotOptions(1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := persist.OpenSnapshotReader(path); err == nil {
		t.Error("the snapshot was readable before Commit")
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, f, err := persist.OpenSnapshotReader(path); err != nil {
		t.Errorf("the snapshot is not readable after Commit: %v", err)
	} else {
		f.Close()
	}

	// An aborted snapshot over an existing one must leave the old file.
	w2, err := persist.CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Anchor(0, 0, 2); err != nil {
		t.Fatal(err)
	}
	w2.Abort()

	restored, _ := loadSnapshotInto(t, path, h.now.Load())
	rs := newSession(t, restored)
	if got := str(rs.do("GET", "first")); got != "1" {
		t.Errorf("the committed snapshot was damaged by an aborted one: %q", got)
	}
}

func TestSnapshotAndLogAreNotInterchangeable(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "k", "v")
	path, _ := writeSnapshotFile(t, h, 1, 0)

	if _, _, err := persist.OpenReader(path); err == nil {
		t.Error("a snapshot was opened as a log")
	}

	logDir := t.TempDir()
	l, err := persist.OpenLog(logDir, persist.FsyncNo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(0, [][]byte{[]byte("SET"), []byte("k"), []byte("v")}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	segs, err := persist.Segments(logDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persist.LoadSnapshot(segs[0].Path, NewReplayer(h)); err == nil {
		t.Error("a log segment was loaded as a snapshot")
	}
}

// TestSnapshotWalkerStopsOnSinkError checks that a failing write aborts the
// pass instead of producing a snapshot with a hole in it.
func TestSnapshotWalkerStopsOnSinkError(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for i := 0; i < 50; i++ {
		s.do("SET", fmt.Sprintf("k%d", i), "v")
	}
	sink := &memSink{failAt: 5}
	if err := WriteSnapshot(h.ks, sink, snapshotOptions(1)); err == nil {
		t.Fatal("the walker ignored a sink error")
	}
	if len(sink.records) >= 50 {
		t.Errorf("the walker kept going after the error: %d records", len(sink.records))
	}
}

// TestSnapshotUnderConcurrentWrites is the fuzzy part of "chunked fuzzy
// snapshot". Writes keep arriving while the pass runs, so different shards
// are captured at different instants.
//
// What must hold regardless: the pass does not race, the anchors do not go
// backwards, and the file loads cleanly. The dataset it loads to is a
// mixture of instants and is not expected to equal anything in particular --
// reconciling it with the log is what the per-shard filter does, and that is
// the next chunk. This test exists so the race detector sees the walker
// running against live traffic.
func TestSnapshotUnderConcurrentWrites(t *testing.T) {
	h := newTestHost(t)
	randomWorkload(t, h, 20240919, 1500)
	h.mu.Lock()
	h.sink = func(db int, args [][]byte) {}
	h.mu.Unlock()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s := newSession(t, h)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			k := fmt.Sprintf("hot%d", i%40)
			s.do("SET", k, fmt.Sprint(i))
			s.do("RPUSH", "hotlist", fmt.Sprint(i))
			s.do("HSET", "hothash", k, fmt.Sprint(i))
			s.do("ZADD", "hotzset", fmt.Sprint(i%100), k)
			s.do("SADD", "hotset", k)
			s.do("LPOP", "hotlist")
		}
	}()

	var offset uint64
	path := filepath.Join(t.TempDir(), "kestrel.snapshot")
	w, err := persist.CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := SnapshotOptions{Offset: func() uint64 { offset += 7; return offset }}
	err = WriteSnapshot(h.ks, w, opts)
	close(stop)
	<-done
	if err != nil {
		w.Abort()
		t.Fatal(err)
	}
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if info.First >= info.Last {
		t.Errorf("window is [%d,%d]; a multi-shard snapshot should span one",
			info.First, info.Last)
	}

	// The file must be loadable even though it is a mixture of instants.
	restored, load := loadSnapshotInto(t, path, h.now.Load())
	if load.Records != info.Records {
		t.Errorf("loaded %d of %d records", load.Records, info.Records)
	}
	if load.First != info.First || load.Last != info.Last {
		t.Errorf("window read back as [%d,%d], written as [%d,%d]",
			load.First, load.Last, info.First, info.Last)
	}
	if n := restored.ks.TotalKeys(); n == 0 {
		t.Error("the snapshot restored nothing")
	}
}

// nullSink measures the walker without the cost of a file.
type nullSink struct{ records int64 }

func (n *nullSink) Anchor(db, shard int, offset uint64) error { return nil }
func (n *nullSink) Record(db int, args [][]byte) error        { n.records++; return nil }

func BenchmarkWriteSnapshot(b *testing.B) {
	t := &testing.T{}
	h := newTestHost(t)
	s := newSession(t, h)
	for i := 0; i < 20000; i++ {
		s.do("SET", fmt.Sprintf("k%d", i), "some value of a realistic length")
	}
	for i := 0; i < 200; i++ {
		s.do("RPUSH", fmt.Sprintf("list%d", i%20), fmt.Sprint(i))
		s.do("ZADD", fmt.Sprintf("z%d", i%20), fmt.Sprint(i), fmt.Sprint(i))
	}
	opts := SnapshotOptions{Offset: func() uint64 { return 1 }}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink := &nullSink{}
		if err := WriteSnapshot(h.ks, sink, opts); err != nil {
			b.Fatal(err)
		}
	}
}
