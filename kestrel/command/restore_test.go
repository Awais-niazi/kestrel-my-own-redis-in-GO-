package command

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"kestrel/persist"
)

// restore replays a snapshot and then the log tail, filtering the tail
// against the snapshot's anchors. This is what a restarting server does.
func restore(t *testing.T, snapPath, logPath string, clock int64) (*testHost, persist.Result) {
	t.Helper()
	h := newTestHost(t)
	h.now.Store(clock)
	h.mu.Lock()
	h.sink = func(db int, args [][]byte) {}
	h.mu.Unlock()

	h.ks.SetLoading(true)
	defer h.ks.SetLoading(false)

	load, err := persist.LoadSnapshot(snapPath, NewReplayer(h))
	if err != nil {
		t.Fatalf("loading the snapshot failed: %v", err)
	}

	filtered := NewReplayer(h).Filter(func(db, shard int) uint64 {
		return load.Anchors[persist.ShardRef{DB: db, Shard: shard}]
	})
	res, err := persist.Recover(persist.RecoverOptions{
		Path: logPath, From: load.First,
	}, filtered)
	if err != nil {
		t.Fatalf("replaying the log tail failed after %d records: %v", res.Records, err)
	}
	return h, res
}

// TestRestoreFromSnapshotAndLog is the proof ADR-009 needs: a snapshot taken
// while writes are in flight, plus the log that kept running underneath it,
// must rebuild exactly the dataset the live host ended up with.
//
// Every shard in the snapshot is at a different instant, so the log tail has
// to be applied to some shards and not others. Getting that wrong does not
// produce a crash; it produces a counter that is off by one.
func TestRestoreFromSnapshotAndLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kestrel.log")
	snapPath := filepath.Join(dir, "kestrel.snapshot")

	leader := newTestHost(t)
	log, err := persist.Create(persist.Options{Path: logPath, Fsync: persist.FsyncNo})
	if err != nil {
		t.Fatal(err)
	}
	var logMu sync.Mutex
	leader.mu.Lock()
	leader.sink = func(db int, args [][]byte) {
		logMu.Lock()
		defer logMu.Unlock()
		if _, err := log.Append(db, args); err != nil {
			t.Error(err)
		}
	}
	leader.mu.Unlock()

	// Enough history that the snapshot pass takes real time, so the window
	// it spans is wide and the filter has many records to decide about.
	randomWorkload(t, leader, 20240920, 3000)
	bulk := newSession(t, leader)
	for i := 0; i < 20000; i++ {
		bulk.do("SET", fmt.Sprintf("bulk%d", i), "a value of some realistic length")
	}

	// Writes keep arriving while the snapshot runs. They are the whole
	// point: without them every shard would be at the same offset and the
	// filter would never be exercised.
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			s := newSession(t, leader)
			for i := w; ; i += 4 {
				select {
				case <-stop:
					return
				default:
				}
				k := fmt.Sprintf("hot%d", i%64)
				s.do("INCR", "counter"+fmt.Sprint(i%16))
				s.do("SET", k, fmt.Sprint(i))
				s.do("APPEND", "acc"+fmt.Sprint(i%16), "z")
				s.do("RPUSH", "hotlist"+fmt.Sprint(i%8), fmt.Sprint(i))
				s.do("HINCRBY", "hothash"+fmt.Sprint(i%8), k, "1")
				s.do("ZADD", "hotzset"+fmt.Sprint(i%8), fmt.Sprint(i%50), k)
				s.do("DEL", k, fmt.Sprintf("hot%d", (i+1)%64))
				s.do("MSET", "m"+fmt.Sprint(i%32), fmt.Sprint(i), "m"+fmt.Sprint((i+7)%32), "x")
			}
		}(w)
	}

	w, err := persist.CreateSnapshot(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	opts := SnapshotOptions{Offset: func() uint64 {
		logMu.Lock()
		defer logMu.Unlock()
		return log.Offset()
	}}
	leader.ks.SnapshotWindow(func() {
		if err := WriteSnapshot(leader.ks, w, opts); err != nil {
			t.Error(err)
		}
	})
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}

	// Keep writing after the snapshot too, so the tail has records every
	// shard needs.
	close(stop)
	writers.Wait()
	randomWorkload(t, leader, 20240921, 1500)

	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	leader.mu.Lock()
	leader.sink = func(db int, args [][]byte) {}
	leader.mu.Unlock()

	if info.First >= info.Last {
		t.Fatalf("the snapshot window is [%d,%d]; nothing was written during the "+
			"pass, so the filter is not being tested", info.First, info.Last)
	}
	t.Logf("snapshot window [%d,%d], %d records; log ends at %d",
		info.First, info.Last, info.Records, log.Offset())

	restored, res := restore(t, snapPath, logPath, leader.now.Load())
	t.Logf("replayed %d log records, skipped %d", res.Records, res.Skipped)

	want := dump(t, leader)
	got := dump(t, restored)
	if want != got {
		t.Errorf("restore diverged at %s", firstDiff(want, got))
	}
}
