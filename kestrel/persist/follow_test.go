package persist

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func recordText(rec Record) string {
	parts := make([]string, len(rec.Args))
	for i, a := range rec.Args {
		parts[i] = string(a)
	}
	return fmt.Sprintf("%d:%s", rec.DB, strings.Join(parts, " "))
}

// collect reads n records with a deadline, so a follower that stalls fails
// the test rather than hanging it.
func collect(t *testing.T, f *Follower, n int) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []string
	for i := 0; i < n; i++ {
		rec, err := f.Next(ctx)
		if err != nil {
			t.Fatalf("after %d of %d records: %v", i, n, err)
		}
		out = append(out, recordText(rec))
	}
	return out
}

func TestFollowFromTheStart(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	for i := 0; i < 5; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v")); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Follow(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got := collect(t, f, 5)
	if got[0] != "0:SET 0 v" || got[4] != "0:SET 4 v" {
		t.Errorf("got %v", got)
	}
	if f.Offset() != l.Offset() {
		t.Errorf("follower is at %d, log at %d", f.Offset(), l.Offset())
	}
}

// TestFollowBlocksThenWakes is the live case: the follower is caught up and
// must be woken by the next write rather than polling for it.
func TestFollowBlocksThenWakes(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	f, err := Follow(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	collect(t, f, 1)

	got := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rec, err := f.Next(ctx)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		got <- recordText(rec)
	}()

	// The follower must still be waiting, not spinning on an empty read.
	select {
	case v := <-got:
		t.Fatalf("Next returned %q with nothing written", v)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := l.Append(3, cmd("SET", "b", "2")); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != "3:SET b 2" {
			t.Errorf("woke with %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follower was not woken by an append")
	}
}

// TestFollowAcrossARoll is the property that lets compaction happen while a
// replica is connected.
func TestFollowAcrossARoll(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	f, err := Follow(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var want []string
	for i := 0; i < 9; i++ {
		if i == 3 || i == 6 {
			if err := l.Roll(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v")); err != nil {
			t.Fatal(err)
		}
		want = append(want, fmt.Sprintf("0:SET %d v", i))
	}

	got := collect(t, f, 9)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("records crossing a roll came back as\n%v\nwant\n%v", got, want)
	}
	if len(l.Segments()) != 3 {
		t.Errorf("the log has %d segments, want 3", len(l.Segments()))
	}
}

// TestFollowWhileWritingConcurrently runs the two against each other, which
// is what a live replica link is.
func TestFollowWhileWritingConcurrently(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	const n = 2000

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v")); err != nil {
				t.Error(err)
				return
			}
			if i%500 == 499 {
				if err := l.Roll(); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()

	f, err := Follow(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := collect(t, f, n)
	wg.Wait()

	for i, g := range got {
		if want := fmt.Sprintf("0:SET %d v", i); g != want {
			t.Fatalf("record %d is %q, want %q", i, g, want)
		}
	}
}

// TestFollowFromACompactedOffsetIsRefused is the case that forces a full
// resynchronisation, and the reason a replica needs to be told which it is.
func TestFollowFromACompactedOffsetIsRefused(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	for i := 0; i < 3; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v")); err != nil {
			t.Fatal(err)
		}
	}
	cut := l.Offset()
	if err := l.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(0, cmd("SET", "later", "v")); err != nil {
		t.Fatal(err)
	}
	if err := l.Roll(); err != nil {
		t.Fatal(err)
	}
	if removed, _, err := l.Prune(cut); err != nil || removed == 0 {
		t.Fatalf("pruned %d segments: %v", removed, err)
	}

	if _, err := Follow(l, 0); !errors.Is(err, ErrTooFarBehind) {
		t.Errorf("following from a compacted offset returned %v, want ErrTooFarBehind", err)
	}
	if got := l.OldestOffset(); got != cut {
		t.Errorf("OldestOffset is %d, want %d", got, cut)
	}
	// An offset that is still retained works.
	f, err := Follow(l, cut)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := collect(t, f, 1); got[0] != "0:SET later v" {
		t.Errorf("resuming from the retained offset gave %v", got)
	}
}

func TestFollowStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	f, err := Follow(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := f.Next(ctx); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not stop the follower")
	}
}

func TestFollowStopsWhenTheLogCloses(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir, FsyncNo)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Follow(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	done := make(chan error, 1)
	go func() {
		_, err := f.Next(context.Background())
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrLogClosed) {
			t.Errorf("got %v, want ErrLogClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the log did not release the follower")
	}
}

// TestFollowSeeksRatherThanScans guards the property that keeps a live tail
// from being quadratic: catching up after an end-of-file must not re-read
// the segment from its start.
func TestFollowSeeksRatherThanScans(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir)
	for i := 0; i < 20000; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "value")); err != nil {
			t.Fatal(err)
		}
	}
	at := l.Offset()

	start := time.Now()
	f, err := Follow(l, at)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Errorf("seeking to the end of a 20,000-record segment took %v; it should "+
			"be a byte seek, not a scan", elapsed)
	}
	if _, err := l.Append(0, cmd("SET", "next", "v")); err != nil {
		t.Fatal(err)
	}
	if got := collect(t, f, 1); got[0] != "0:SET next v" {
		t.Errorf("resuming at the end gave %v", got)
	}
}
