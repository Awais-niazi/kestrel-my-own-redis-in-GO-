package persist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openTestLog(t *testing.T, dir string) *Log {
	t.Helper()
	l, err := OpenLog(dir, FsyncNo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestLogStartsOneSegment(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	segs := l.Segments()
	if len(segs) != 1 || segs[0].Seq != 1 || segs[0].Base != 0 {
		t.Fatalf("got %+v, want one segment numbered 1 based at 0", segs)
	}
	if _, err := os.Stat(filepath.Join(dir, "kestrel-000001.log")); err != nil {
		t.Errorf("the first segment is not where it should be: %v", err)
	}
}

// TestRollContinuesTheStream is the property compaction depends on: a new
// segment picks up the offset the previous one reached, so an offset a
// snapshot recorded stays comparable across the roll.
func TestRollContinuesTheStream(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	at := l.Offset()

	if err := l.Roll(); err != nil {
		t.Fatal(err)
	}
	next, err := l.Append(0, cmd("SET", "b", "2"))
	if err != nil {
		t.Fatal(err)
	}
	if next != at {
		t.Errorf("the first record of the new segment is at %d, want %d", next, at)
	}

	segs := l.Segments()
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if segs[0].End != segs[1].Base {
		t.Errorf("segment 1 ends at %d and segment 2 begins at %d; the stream has a gap",
			segs[0].End, segs[1].Base)
	}
}

func TestRecoverAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	var want []string
	for i := 0; i < 9; i++ {
		if i%3 == 0 && i > 0 {
			if err := l.Roll(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v")); err != nil {
			t.Fatal(err)
		}
		want = append(want, fmt.Sprintf("0:SET %d v", i))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	var c collector
	res, err := Recover(RecoverOptions{Dir: dir}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Segments != 3 || res.Records != 9 {
		t.Errorf("read %d segments and %d records, want 3 and 9", res.Segments, res.Records)
	}
	if got := strings.Join(c.applied, "|"); got != strings.Join(want, "|") {
		t.Errorf("records came back out of order:\n%s", got)
	}
}

// TestPruneRemovesOnlyDeadSegments is compaction. A segment is droppable
// once every record in it is below the snapshot's first anchor.
func TestPruneRemovesOnlyDeadSegments(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)

	var bounds []uint64
	for i := 0; i < 4; i++ {
		for j := 0; j < 3; j++ {
			if _, err := l.Append(0, cmd("SET", fmt.Sprint(i*3+j), "v")); err != nil {
				t.Fatal(err)
			}
		}
		bounds = append(bounds, l.Offset())
		if err := l.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	// Four finished segments plus the live one.
	if got := len(l.Segments()); got != 5 {
		t.Fatalf("got %d segments, want 5", got)
	}

	// An anchor inside the third segment keeps it and everything after.
	removed, freed, err := l.Prune(bounds[1])
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("pruned %d segments, want 2", removed)
	}
	if freed <= 0 {
		t.Errorf("freed %d bytes", freed)
	}
	segs := l.Segments()
	if len(segs) != 3 {
		t.Fatalf("%d segments remain, want 3", len(segs))
	}
	if segs[0].Seq != 3 {
		t.Errorf("the oldest remaining segment is %d, want 3", segs[0].Seq)
	}

	// What is left must still replay cleanly from the anchor.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var c collector
	res, err := Recover(RecoverOptions{Dir: dir, From: bounds[1]}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 6 {
		t.Errorf("replayed %d records from the anchor, want 6", res.Records)
	}
}

// TestPruneNeverRemovesTheLiveSegment guards the case where a snapshot's
// anchor is past everything written so far.
func TestPruneNeverRemovesTheLiveSegment(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	removed, _, err := l.Prune(^uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || len(l.Segments()) != 1 {
		t.Errorf("pruning removed the segment being written to")
	}
	if _, err := l.Append(0, cmd("SET", "b", "2")); err != nil {
		t.Errorf("the log is unusable after a prune: %v", err)
	}
}

func TestReopenContinuesTheLastSegment(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	if err := l.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(0, cmd("SET", "b", "2")); err != nil {
		t.Fatal(err)
	}
	end := l.Offset()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	again := openTestLog(t, dir)
	if again.Offset() != end {
		t.Fatalf("reopened at %d, want %d", again.Offset(), end)
	}
	if len(again.Segments()) != 2 {
		t.Errorf("reopening found %d segments, want 2", len(again.Segments()))
	}
	if _, err := again.Append(0, cmd("SET", "c", "3")); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	var c collector
	res, err := Recover(RecoverOptions{Dir: dir}, &c)
	if err != nil || res.Records != 3 {
		t.Errorf("after a reopen the log holds %d records (%v), want 3", res.Records, err)
	}
}

// TestGapBetweenSegmentsIsRefused covers the case an operator creates by
// deleting a file by hand.
func TestGapBetweenSegmentsIsRefused(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	for i := 0; i < 3; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v")); err != nil {
			t.Fatal(err)
		}
		if err := l.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, segmentName(2))); err != nil {
		t.Fatal(err)
	}

	if _, err := Segments(dir); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a missing middle segment reported %v, want corruption", err)
	}
	var c collector
	if _, err := Recover(RecoverOptions{Dir: dir}, &c); !errors.Is(err, ErrCorrupt) {
		t.Errorf("recovery ran over a gap: %v", err)
	}
}

// TestDamageInAnEarlierSegmentIsFatal is deliberately stricter than the
// truncate policy. Truncating a middle segment would orphan every segment
// after it, discarding far more than an operator asked for.
func TestDamageInAnEarlierSegmentIsFatal(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	for i := 0; i < 6; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "value")); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			if err := l.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	first := filepath.Join(dir, segmentName(1))
	raw, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-5] ^= 0xff
	if err := os.WriteFile(first, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	var c collector
	_, err = Recover(RecoverOptions{Dir: dir, Policy: PolicyTruncate}, &c)
	if err == nil {
		t.Fatal("recovery truncated a middle segment and carried on")
	}
	if !strings.Contains(err.Error(), "not the last segment") {
		t.Errorf("error %v does not explain why truncating was refused", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, segmentName(2))); statErr != nil {
		t.Error("the later segment was removed")
	}
}

// TestLegacyLogIsAdopted covers the data directory written by the build that
// had a single log file.
func TestLegacyLogIsAdopted(t *testing.T) {
	dir := t.TempDir()
	old, err := createSegment(segmentOptions{Path: filepath.Join(dir, legacyLogName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Append(0, cmd("SET", "carried", "over")); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	var c collector
	res, err := Recover(RecoverOptions{Dir: dir}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 1 || len(c.applied) != 1 || c.applied[0] != "0:SET carried over" {
		t.Errorf("the legacy log was not replayed: %+v %v", res, c.applied)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyLogName)); !os.IsNotExist(err) {
		t.Error("the legacy file is still there after being adopted")
	}
	if _, err := os.Stat(filepath.Join(dir, segmentName(1))); err != nil {
		t.Errorf("it was not renamed into a segment: %v", err)
	}
}

func TestLogStatsCountEverySegment(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	for i := 0; i < 4; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "value")); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err := l.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	st := l.Stats()
	if st.Segments != 2 {
		t.Errorf("Stats reports %d segments, want 2", st.Segments)
	}
	var onDisk int64
	for _, s := range l.Segments() {
		fi, err := os.Stat(s.Path)
		if err != nil {
			t.Fatal(err)
		}
		onDisk += fi.Size()
	}
	if st.Size != onDisk {
		t.Errorf("Stats reports %d bytes, the files hold %d", st.Size, onDisk)
	}
}

// TestRollUnderConcurrentAppendsKeepsTheStreamWhole rolls the log while
// several goroutines are appending to it.
//
// Two things went wrong here and neither showed up in a test that rolled a
// quiet log. Append chose l.cur under the read lock and then released it
// before appending, so Roll could close that segment underneath an appender:
// the write failed for no reason, and any record that did land arrived after
// Roll had recorded where the file ended. Roll took that end from the
// segment's flushed offset before syncing it, which under group commit is
// behind what has been handed out, so the file grew past its recorded end and
// the next segment began before the previous one finished.
//
// Either one produces a log that loads no more: Segments reports overlapping
// or non-contiguous files and recovery refuses to start. The server that
// wrote it keeps running and looks healthy until it is restarted.
func TestRollUnderConcurrentAppendsKeepsTheStreamWhole(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)

	const writers = 8
	const perWriter = 400

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var failures atomic.Int64
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_, err := l.Append(0, cmd("SET", fmt.Sprintf("k%d-%d", w, i), "v"))
				if err != nil {
					failures.Add(1)
					t.Errorf("append %d of writer %d failed: %v", i, w, err)
					return
				}
			}
		}(w)
	}
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := l.Roll(); err != nil {
				failures.Add(1)
				t.Errorf("roll failed: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	close(stop)

	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}

	// The in-memory view and the files on disk must agree, and both must be
	// one unbroken stream.
	for i, s := range l.Segments() {
		if i > 0 && s.Base != l.Segments()[i-1].End {
			t.Fatalf("segment %d begins at %d but the previous one ends at %d",
				s.Seq, s.Base, l.Segments()[i-1].End)
		}
	}
	onDisk, err := Segments(dir)
	if err != nil {
		t.Fatalf("the log will not load: %v", err)
	}
	if got, want := onDisk[len(onDisk)-1].End, l.Offset(); got != want {
		t.Errorf("the files end at %d but the log says %d", got, want)
	}

	// Every record must still be there, exactly once.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var c collector
	res, err := Recover(RecoverOptions{Dir: dir}, &c)
	if err != nil {
		t.Fatalf("the rolled log will not replay: %v", err)
	}
	if want := int64(writers * perWriter); res.Records != want && failures.Load() == 0 {
		t.Errorf("replayed %d records, want %d", res.Records, want)
	}
}
