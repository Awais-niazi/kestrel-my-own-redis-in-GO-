package persist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func tempLog(t *testing.T, opts Options) *Log {
	t.Helper()
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "kestrel.log")
	}
	l, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func cmd(parts ...string) [][]byte {
	out := make([][]byte, len(parts))
	for i, p := range parts {
		out[i] = []byte(p)
	}
	return out
}

// readAll drains a log file and returns its records rendered for assertion.
func readAll(t *testing.T, path string) ([]string, *Reader) {
	t.Helper()
	r, f, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	var out []string
	for r.Next() {
		rec := r.Record()
		parts := make([]string, len(rec.Args))
		for i, a := range rec.Args {
			parts[i] = string(a)
		}
		out = append(out, fmt.Sprintf("%d:%s", rec.DB, strings.Join(parts, " ")))
	}
	return out, r
}

func TestLogRoundTrip(t *testing.T) {
	l := tempLog(t, Options{})
	want := []string{"0:SET a 1", "3:DEL a", "0:RPUSH k x y z"}
	for _, w := range []struct {
		db   int
		args [][]byte
	}{
		{0, cmd("SET", "a", "1")},
		{3, cmd("DEL", "a")},
		{0, cmd("RPUSH", "k", "x", "y", "z")},
	} {
		if _, err := l.Append(w.db, w.args); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}

	got, r := readAll(t, l.Path())
	if r.Err() != nil {
		t.Fatalf("clean log reported %v", r.Err())
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v, want %v", got, want)
	}
	if r.Offset() != l.Offset() {
		t.Errorf("reader ended at %d, writer at %d", r.Offset(), l.Offset())
	}
}

// TestAppendReturnsRecordStartOffset matters more than it looks: a snapshot
// anchors a shard to an offset, and recovery compares each record's offset
// against it. An off-by-one here replays a record twice or not at all.
func TestAppendReturnsRecordStartOffset(t *testing.T) {
	l := tempLog(t, Options{})
	var offsets []uint64
	for i := 0; i < 5; i++ {
		off, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v"))
		if err != nil {
			t.Fatal(err)
		}
		offsets = append(offsets, off)
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}

	r, f, err := OpenReader(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; r.Next(); i++ {
		if got := r.Record().Offset; got != offsets[i] {
			t.Errorf("record %d: reader says offset %d, Append said %d", i, got, offsets[i])
		}
	}
}

// TestBaseOffsetSurvivesRewrite covers the property a compaction depends on:
// stream offsets run across files, so an offset a snapshot recorded before a
// rewrite still names the same point in the stream afterwards.
func TestBaseOffsetSurvivesRewrite(t *testing.T) {
	dir := t.TempDir()
	first := tempLog(t, Options{Path: filepath.Join(dir, "a.log")})
	if _, err := first.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	end := first.Offset()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Create(Options{Path: filepath.Join(dir, "b.log"), Base: end})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	at, err := second.Append(0, cmd("SET", "b", "2"))
	if err != nil {
		t.Fatal(err)
	}
	if at != end {
		t.Errorf("rewritten log restarted at %d, want %d", at, end)
	}

	r, f, err := OpenReader(second.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !r.Next() || r.Record().Offset != end {
		t.Errorf("reader lost the base offset: %d", r.Record().Offset)
	}
}

func TestReopenContinuesAtEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kestrel.log")
	l := tempLog(t, Options{Path: path})
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	end := l.Offset()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Offset() != end {
		t.Fatalf("reopened at %d, want %d", again.Offset(), end)
	}
	if _, err := again.Append(0, cmd("SET", "b", "2")); err != nil {
		t.Fatal(err)
	}
	if err := again.Sync(); err != nil {
		t.Fatal(err)
	}
	got, _ := readAll(t, path)
	if len(got) != 2 || got[1] != "0:SET b 2" {
		t.Errorf("append after reopen produced %v", got)
	}
}

// TestTornTailIsNotCorruption is the crash case: the process died part-way
// through a write. It must be reported as a torn tail, because recovery
// truncates one and refuses the other.
func TestTornTailIsNotCorruption(t *testing.T) {
	for _, cut := range []int{1, 6, 12, 15} {
		t.Run(fmt.Sprint("cut", cut), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kestrel.log")
			l := tempLog(t, Options{Path: path})
			if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
				t.Fatal(err)
			}
			good := l.Offset()
			if _, err := l.Append(0, cmd("SET", "bbbbbbbb", "2")); err != nil {
				t.Fatal(err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}

			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, fi.Size()-int64(cut)); err != nil {
				t.Fatal(err)
			}

			got, r := readAll(t, path)
			if !errors.Is(r.Err(), ErrTornTail) {
				t.Fatalf("cutting %d bytes reported %v, want a torn tail", cut, r.Err())
			}
			if len(got) != 1 || got[0] != "0:SET a 1" {
				t.Errorf("records before the tear were lost: %v", got)
			}
			if r.Offset() != good {
				t.Errorf("truncation point is %d, want %d", r.Offset(), good)
			}
		})
	}
}

// TestCorruptPayloadIsDetected flips a byte inside a record that is
// otherwise complete. That is storage damage, not a crash, and the checksum
// is the only thing standing between it and a silently wrong recovery.
func TestCorruptPayloadIsDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kestrel.log")
	l := tempLog(t, Options{Path: path})
	for i := 0; i < 3; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "value")); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt a byte inside the second record's payload.
	raw[fileHeaderSize+recordHeaderSize+len("*3\r\n$3\r\nSET\r\n$1\r\n0\r\n$5\r\nvalue\r\n")+20] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	got, r := readAll(t, path)
	if !errors.Is(r.Err(), ErrCorrupt) {
		t.Fatalf("a flipped byte reported %v, want corruption", r.Err())
	}
	if len(got) != 1 {
		t.Errorf("read %d records past the damage, want 1", len(got))
	}
}

func TestBadFileHeaderIsRejected(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"empty":       {},
		"short":       []byte("KESTR"),
		"wrong magic": append([]byte("NOTALOG!"), make([]byte, 24)...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := OpenReader(path); !errors.Is(err, ErrBadHeader) {
				t.Errorf("got %v, want ErrBadHeader", err)
			}
		})
	}

	t.Run("damaged header", func(t *testing.T) {
		path := filepath.Join(dir, "damaged")
		l := tempLog(t, Options{Path: path})
		l.Close()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw[12] ^= 0xff // inside Base, covered by the header checksum
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := OpenReader(path); !errors.Is(err, ErrCorrupt) {
			t.Errorf("got %v, want ErrCorrupt", err)
		}
	})
}

func TestCreateRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kestrel.log")
	l := tempLog(t, Options{Path: path})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(Options{Path: path}); !errors.Is(err, os.ErrExist) {
		t.Errorf("Create over an existing log returned %v, want ErrExist", err)
	}
}

func TestTruncateTo(t *testing.T) {
	l := tempLog(t, Options{})
	first, err := l.Append(0, cmd("SET", "a", "1"))
	if err != nil {
		t.Fatal(err)
	}
	keep := l.Offset()
	if _, err := l.Append(0, cmd("SET", "b", "2")); err != nil {
		t.Fatal(err)
	}

	if err := l.TruncateTo(keep); err != nil {
		t.Fatal(err)
	}
	if l.Offset() != keep {
		t.Errorf("offset is %d after truncation, want %d", l.Offset(), keep)
	}
	if _, err := l.Append(0, cmd("SET", "c", "3")); err != nil {
		t.Fatal(err)
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	got, r := readAll(t, l.Path())
	if r.Err() != nil {
		t.Fatalf("log is unreadable after truncation: %v", r.Err())
	}
	want := []string{"0:SET a 1", "0:SET c 3"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v, want %v", got, want)
	}
	if first != 0 {
		t.Errorf("first record started at %d, want 0", first)
	}
}

func TestTruncateRejectsOutOfRange(t *testing.T) {
	l := tempLog(t, Options{})
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	if err := l.TruncateTo(l.Offset() + 1); err == nil {
		t.Error("truncating past the end was accepted")
	}
}

func TestFsyncAlwaysForcesEveryAppend(t *testing.T) {
	l := tempLog(t, Options{Fsync: FsyncAlways})
	for i := 0; i < 3; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v")); err != nil {
			t.Fatal(err)
		}
	}
	if got := l.Stats().Syncs; got != 3 {
		t.Errorf("appendfsync always performed %d syncs for 3 appends", got)
	}
}

func TestFsyncNoNeverForces(t *testing.T) {
	l := tempLog(t, Options{Fsync: FsyncNo})
	for i := 0; i < 3; i++ {
		if _, err := l.Append(0, cmd("SET", fmt.Sprint(i), "v")); err != nil {
			t.Fatal(err)
		}
	}
	if got := l.Stats().Syncs; got != 0 {
		t.Errorf("appendfsync no performed %d syncs", got)
	}
	// The records are still in the file: the policy governs fsync, not the
	// write, which is what lets everysec survive a process kill.
	got, r := readAll(t, l.Path())
	if r.Err() != nil || len(got) != 3 {
		t.Errorf("records were withheld from the file: %v, %v", got, r.Err())
	}
}

func TestSetFsyncIsLive(t *testing.T) {
	l := tempLog(t, Options{Fsync: FsyncNo})
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	l.SetFsync(FsyncAlways)
	if l.Fsync() != FsyncAlways {
		t.Fatal("SetFsync did not take effect")
	}
	if _, err := l.Append(0, cmd("SET", "b", "2")); err != nil {
		t.Fatal(err)
	}
	if got := l.Stats().Syncs; got != 1 {
		t.Errorf("got %d syncs, want 1: only the append after the change forces", got)
	}
}

func TestConcurrentAppendsAreOrderedAndComplete(t *testing.T) {
	l := tempLog(t, Options{Fsync: FsyncNo})
	const writers, each = 8, 200

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := l.Append(0, cmd("SET", fmt.Sprintf("k%d-%d", w, i), "v")); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}

	got, r := readAll(t, l.Path())
	if r.Err() != nil {
		t.Fatalf("concurrent appends produced an unreadable log: %v", r.Err())
	}
	if len(got) != writers*each {
		t.Fatalf("read %d records, want %d", len(got), writers*each)
	}
	// Every record must appear exactly once. Interleaving is allowed; a
	// half-written record interleaved with another is not.
	seen := make(map[string]bool, len(got))
	for _, g := range got {
		if seen[g] {
			t.Fatalf("duplicate record %q", g)
		}
		seen[g] = true
	}
	if r.Offset() != l.Offset() {
		t.Errorf("reader ended at %d, writer at %d", r.Offset(), l.Offset())
	}
}

func TestAppendAfterFailureKeepsFailing(t *testing.T) {
	l := tempLog(t, Options{Fsync: FsyncNo})
	if _, err := l.Append(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	// Closing the file underneath the log is the cheapest way to make the
	// next write fail the way a full disk would.
	l.mu.Lock()
	l.f.Close()
	l.mu.Unlock()

	_, first := l.Append(0, cmd("SET", "b", "2"))
	if first == nil {
		t.Fatal("append to a closed file succeeded")
	}
	_, second := l.Append(0, cmd("SET", "c", "3"))
	if !errors.Is(second, first) && second.Error() != first.Error() {
		t.Errorf("second append reported %v, want the sticky first error %v", second, first)
	}
	if l.Err() == nil {
		t.Error("Err did not report the failure")
	}
}

func TestParseFsync(t *testing.T) {
	for in, want := range map[string]Fsync{
		"always": FsyncAlways, "everysec": FsyncEverySec, "no": FsyncNo,
	} {
		got, err := ParseFsync(in)
		if err != nil || got != want {
			t.Errorf("ParseFsync(%q) = %v, %v", in, got, err)
		}
		if got.String() != in {
			t.Errorf("%v.String() = %q, want %q", got, got.String(), in)
		}
	}
	if _, err := ParseFsync("sometimes"); err == nil {
		t.Error("an unknown policy was accepted")
	}
}
