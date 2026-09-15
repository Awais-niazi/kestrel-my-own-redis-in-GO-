package persist

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// collector records what recovery applied, and can be made to fail on a
// chosen record.
type collector struct {
	applied []string
	failOn  int
}

func (c *collector) Apply(db int, offset uint64, args [][]byte) error {
	if c.failOn > 0 && len(c.applied) == c.failOn-1 {
		return errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	}
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = string(a)
	}
	c.applied = append(c.applied, fmt.Sprintf("%d:%s", db, strings.Join(parts, " ")))
	return nil
}

// writeLog produces a log directory holding the given commands, and returns
// the directory, the single segment's path, and the final stream offset.
func writeLog(t *testing.T, cmds ...[][]byte) (dir, path string, end uint64) {
	t.Helper()
	dir = t.TempDir()
	l, err := OpenLog(dir, FsyncNo)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cmds {
		if _, err := l.Append(0, c); err != nil {
			t.Fatal(err)
		}
	}
	end = l.Offset()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, segmentName(1)), end
}

func TestRecoverCleanLog(t *testing.T) {
	dir, _, end := writeLog(t,
		cmd("SET", "a", "1"),
		cmd("INCR", "a"),
		cmd("DEL", "a"),
	)
	var c collector
	res, err := Recover(RecoverOptions{Dir: dir}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 3 || res.Offset != end || res.Damage != nil || res.Discarded != 0 {
		t.Errorf("got %+v, want 3 records ending at %d with no damage", res, end)
	}
	want := "0:SET a 1|0:INCR a|0:DEL a"
	if got := strings.Join(c.applied, "|"); got != want {
		t.Errorf("applied %q, want %q", got, want)
	}
}

func TestRecoverEmptyLog(t *testing.T) {
	dir, _, end := writeLog(t)
	var c collector
	res, err := Recover(RecoverOptions{Dir: dir}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 0 || res.Offset != end {
		t.Errorf("got %+v, want nothing applied at offset %d", res, end)
	}
}

func TestRecoverMissingLog(t *testing.T) {
	var c collector
	_, err := Recover(RecoverOptions{Dir: t.TempDir()}, &c)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want a not-exist error the caller can read as 'no log yet'", err)
	}
}

func TestRecoverTruncatesTornTail(t *testing.T) {
	dir, path, _ := writeLog(t, cmd("SET", "a", "1"), cmd("SET", "bbbbbbbbbb", "2"))
	keep := uint64(recordSize(cmd("SET", "a", "1"))) // only the first record survives
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, fi.Size()-8); err != nil {
		t.Fatal(err)
	}

	var c collector
	res, err := Recover(RecoverOptions{Dir: dir, Policy: PolicyTruncate}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(res.Damage, ErrTornTail) {
		t.Errorf("damage is %v, want a torn tail", res.Damage)
	}
	if res.Records != 1 || res.Offset != keep {
		t.Errorf("got %+v, want 1 record ending at %d", res, keep)
	}
	if res.Discarded == 0 {
		t.Error("nothing was reported as discarded")
	}

	// The file must now be clean, so that a second crash does not present
	// the same damaged tail again.
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(keep) + fileHeaderSize; fi2.Size() != want {
		t.Errorf("the file is %d bytes after truncation, want %d", fi2.Size(), want)
	}
	var again collector
	res2, err := Recover(RecoverOptions{Dir: dir}, &again)
	if err != nil || res2.Damage != nil {
		t.Errorf("re-recovering the truncated log reported %v / %v", err, res2.Damage)
	}
	if res2.Records != 1 {
		t.Errorf("re-recovery applied %d records, want 1", res2.Records)
	}
}

func TestRecoverRefusesTornTail(t *testing.T) {
	dir, path, _ := writeLog(t, cmd("SET", "a", "1"), cmd("SET", "bbbbbbbbbb", "2"))
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, fi.Size()-8); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var c collector
	_, err = Recover(RecoverOptions{Dir: dir, Policy: PolicyRefuse}, &c)
	if err == nil {
		t.Fatal("refuse policy started anyway")
	}
	if !errors.Is(err, ErrTornTail) {
		t.Errorf("error %v does not say what the damage was", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Error("refuse policy modified the file; an operator must find it as it was")
	}
}

func TestRecoverTruncatesCorruptRecord(t *testing.T) {
	dir, path, _ := writeLog(t,
		cmd("SET", "a", "1"),
		cmd("SET", "b", "2"),
		cmd("SET", "c", "3"),
	)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := recordSize(cmd("SET", "a", "1"))
	raw[fileHeaderSize+first+recordHeaderSize+5] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	var c collector
	res, err := Recover(RecoverOptions{Dir: dir, Policy: PolicyTruncate}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(res.Damage, ErrCorrupt) {
		t.Errorf("damage is %v, want corruption", res.Damage)
	}
	if res.Records != 1 {
		t.Errorf("applied %d records, want 1: everything after the damage is lost", res.Records)
	}
	// The third record was intact, and is discarded regardless: there is no
	// way to know the log is still in order after a hole in it.
	if res.Discarded == 0 {
		t.Error("the damaged remainder was not discarded")
	}
}

func TestRecoverRefusesCorruptRecord(t *testing.T) {
	dir, path, _ := writeLog(t, cmd("SET", "a", "1"), cmd("SET", "b", "2"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[fileHeaderSize+recordHeaderSize+5] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	var c collector
	_, err = Recover(RecoverOptions{Dir: dir, Policy: PolicyRefuse}, &c)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("got %v, want a refusal naming the corruption", err)
	}
}

// TestRecoverStopsOnDivergence covers the case that is neither a clean log
// nor a damaged one: intact bytes that do not replay. Truncating would be
// wrong, because nothing is wrong with the file.
func TestRecoverStopsOnDivergence(t *testing.T) {
	dir, path, _ := writeLog(t, cmd("SET", "a", "1"), cmd("LPUSH", "a", "x"), cmd("SET", "c", "3"))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	c := &collector{failOn: 2}
	res, err := Recover(RecoverOptions{Dir: dir, Policy: PolicyTruncate}, c)
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("got %v, want ErrDiverged", err)
	}
	if !strings.Contains(err.Error(), "WRONGTYPE") {
		t.Errorf("the error hides what actually failed: %v", err)
	}
	if res.Records != 1 {
		t.Errorf("applied %d records before stopping, want 1", res.Records)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Error("a divergence truncated the log; the file was not the problem")
	}
}

func TestRecoverRejectsForeignFile(t *testing.T) {
	dir := t.TempDir()
	// A file named like a segment but written by something else.
	path := filepath.Join(dir, segmentName(1))
	if err := os.WriteFile(path, []byte("this is not a kestrel log"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c collector
	if _, err := Recover(RecoverOptions{Dir: dir}, &c); !errors.Is(err, ErrBadHeader) {
		t.Errorf("got %v, want ErrBadHeader", err)
	}
}

// TestRecoverIgnoresUnrelatedFiles keeps a snapshot, a stray editor backup or
// an operator's copy of a segment from being read as part of the stream.
func TestRecoverIgnoresUnrelatedFiles(t *testing.T) {
	dir, _, _ := writeLog(t, cmd("SET", "a", "1"))
	for _, name := range []string{
		"kestrel.snapshot", "kestrel-000001.log.bak", "notes.txt", "kestrel-.log",
		"kestrel-abc.log", "kestrel-000002.log.tmp",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var c collector
	res, err := Recover(RecoverOptions{Dir: dir}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Segments != 1 || res.Records != 1 {
		t.Errorf("got %d segments and %d records, want 1 and 1", res.Segments, res.Records)
	}
}

func TestParseCorruptPolicy(t *testing.T) {
	for in, want := range map[string]CorruptPolicy{
		"truncate": PolicyTruncate, "refuse": PolicyRefuse,
	} {
		got, err := ParseCorruptPolicy(in)
		if err != nil || got != want || got.String() != in {
			t.Errorf("ParseCorruptPolicy(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseCorruptPolicy("maybe"); err == nil {
		t.Error("an unknown policy was accepted")
	}
}

// TestRecoveredOffsetContinuesTheStream is what makes a recovered server able
// to keep the same log: appending after recovery must carry on from where
// the replay stopped, not from zero.
func TestRecoveredOffsetContinuesTheStream(t *testing.T) {
	dir, path, end := writeLog(t, cmd("SET", "a", "1"), cmd("SET", "b", "2"))
	var c collector
	res, err := Recover(RecoverOptions{Dir: dir}, &c)
	if err != nil {
		t.Fatal(err)
	}

	l, err := openSegment(segmentOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Offset() != res.Offset || l.Offset() != end {
		t.Fatalf("reopened at %d, recovery ended at %d, log ended at %d",
			l.Offset(), res.Offset, end)
	}
	at, err := l.Append(0, cmd("SET", "c", "3"))
	if err != nil {
		t.Fatal(err)
	}
	if at != end {
		t.Errorf("the next record went to offset %d, want %d", at, end)
	}
}
