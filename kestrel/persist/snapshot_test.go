package persist

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildSnapshot writes a small snapshot and returns its path.
func buildSnapshot(t *testing.T, anchors [][3]int, records ...[][]byte) (string, SnapshotInfo) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kestrel.snapshot")
	w, err := CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range anchors {
		if err := w.Anchor(a[0], a[1], uint64(a[2])); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range records {
		if err := w.Record(0, r); err != nil {
			t.Fatal(err)
		}
	}
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	return path, info
}

func TestSnapshotWriteAndLoad(t *testing.T) {
	path, info := buildSnapshot(t,
		[][3]int{{0, 0, 10}, {0, 1, 20}, {1, 0, 30}},
		cmd("SET", "a", "1"),
		cmd("RPUSH", "l", "x", "y"),
	)
	if info.Anchors != 3 || info.Records != 2 {
		t.Errorf("info is %+v, want 3 anchors and 2 records", info)
	}
	if info.First != 10 || info.Last != 30 {
		t.Errorf("window is [%d,%d], want [10,30]", info.First, info.Last)
	}
	if info.Size <= fileHeaderSize {
		t.Errorf("file is %d bytes", info.Size)
	}

	var c collector
	load, err := LoadSnapshot(path, &c)
	if err != nil {
		t.Fatal(err)
	}
	if load.Records != 2 || load.First != 10 || load.Last != 30 {
		t.Errorf("load is %+v", load)
	}
	want := Anchors{{0, 0}: 10, {0, 1}: 20, {1, 0}: 30}
	if len(load.Anchors) != len(want) {
		t.Fatalf("got %d anchors, want %d", len(load.Anchors), len(want))
	}
	for ref, off := range want {
		if load.Anchors[ref] != off {
			t.Errorf("anchor %+v is %d, want %d", ref, load.Anchors[ref], off)
		}
	}
	if got := strings.Join(c.applied, "|"); got != "0:SET a 1|0:RPUSH l x y" {
		t.Errorf("applied %q", got)
	}
}

func TestSnapshotWithNoAnchorsIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	w, err := CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err == nil {
		t.Error("a snapshot covering no shards was committed")
	}
	w.Abort()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a refused snapshot left a file behind")
	}
}

func TestSnapshotAbortRemovesTheTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s")
	w, err := CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Anchor(0, 0, 1); err != nil {
		t.Fatal(err)
	}
	w.Abort()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("abort left %v behind", entries)
	}
	if err := w.Anchor(0, 1, 2); err == nil {
		t.Error("an aborted writer kept accepting anchors")
	}
}

// TestCreateSnapshotClearsStaleTemporary covers the crash-then-restart case:
// a .tmp file left by an interrupted snapshot is debris, not something to
// preserve, because the snapshot it belonged to was never valid.
func TestCreateSnapshotClearsStaleTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kestrel.snapshot")
	if err := os.WriteFile(path+".tmp", []byte("junk from a crash"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := CreateSnapshot(path)
	if err != nil {
		t.Fatalf("a stale temporary file blocked a new snapshot: %v", err)
	}
	if err := w.Anchor(0, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := w.Record(0, cmd("SET", "a", "1")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	var c collector
	if _, err := LoadSnapshot(path, &c); err != nil {
		t.Errorf("the new snapshot is unreadable: %v", err)
	}
}

func TestSnapshotWriterIsClosedAfterCommit(t *testing.T) {
	path, _ := buildSnapshot(t, [][3]int{{0, 0, 1}}, cmd("SET", "a", "1"))
	w, err := CreateSnapshot(path + "2")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Anchor(0, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := w.Record(0, cmd("SET", "b", "2")); err == nil {
		t.Error("a committed snapshot accepted another record")
	}
	if _, err := w.Commit(); err == nil {
		t.Error("a snapshot was committed twice")
	}
}

// TestDamagedSnapshotIsNeverTruncated is the difference between a snapshot
// and a log. A snapshot is renamed into place only when complete, so damage
// is not a crash artefact and a prefix of it means nothing.
func TestDamagedSnapshotIsNeverTruncated(t *testing.T) {
	path, _ := buildSnapshot(t, [][3]int{{0, 0, 1}},
		cmd("SET", "a", "1"), cmd("SET", "b", "2"), cmd("SET", "c", "3"))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("torn", func(t *testing.T) {
		if err := os.WriteFile(path, before[:len(before)-6], 0o644); err != nil {
			t.Fatal(err)
		}
		var c collector
		if _, err := LoadSnapshot(path, &c); !errors.Is(err, ErrTornTail) {
			t.Errorf("got %v, want a refusal naming the torn tail", err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		raw := append([]byte(nil), before...)
		raw[len(raw)-4] ^= 0xff
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		var c collector
		if _, err := LoadSnapshot(path, &c); !errors.Is(err, ErrCorrupt) {
			t.Errorf("got %v, want a refusal naming the corruption", err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(raw) {
			t.Error("a damaged snapshot was truncated; there is no valid prefix of one")
		}
	})
}

func TestLoadSnapshotStopsOnDivergence(t *testing.T) {
	path, _ := buildSnapshot(t, [][3]int{{0, 0, 1}},
		cmd("SET", "a", "1"), cmd("LPUSH", "a", "x"), cmd("SET", "c", "3"))
	c := &collector{failOn: 2}
	load, err := LoadSnapshot(path, c)
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("got %v, want ErrDiverged", err)
	}
	if load.Records != 1 {
		t.Errorf("applied %d records before stopping, want 1", load.Records)
	}
}

func TestLoadSnapshotRejectsBackwardsAnchors(t *testing.T) {
	// The writer refuses to produce this, so it is built by hand: the file
	// has to be damaged or foreign for the loader's check to matter, which
	// is exactly when it does.
	path := filepath.Join(t.TempDir(), "s")
	var body []byte
	body = append(body, fileHeader{Version: fileVersion, Base: 100}.encode(snapshotMagic)...)
	body = encodeRecord(body, 0, KindAnchor, cmd("0", "0", "100"))
	body = encodeRecord(body, 0, KindAnchor, cmd("0", "1", "50"))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var c collector
	if _, err := LoadSnapshot(path, &c); !errors.Is(err, ErrCorrupt) {
		t.Errorf("got %v, want a refusal: descending anchors break the recovery filter", err)
	}
}

func TestLoadSnapshotRejectsAFileWithNoAnchors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	var body []byte
	body = append(body, fileHeader{Version: fileVersion}.encode(snapshotMagic)...)
	body = encodeRecord(body, 0, KindEffect, cmd("SET", "a", "1"))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var c collector
	if _, err := LoadSnapshot(path, &c); !errors.Is(err, ErrCorrupt) {
		t.Errorf("got %v, want a refusal", err)
	}
}

func TestParseAnchor(t *testing.T) {
	db, shard, off, err := ParseAnchor(cmd("3", "7", "12345"))
	if err != nil || db != 3 || shard != 7 || off != 12345 {
		t.Errorf("got %d/%d@%d, %v", db, shard, off, err)
	}
	for _, bad := range [][][]byte{
		cmd("3", "7"),
		cmd("3", "7", "12345", "extra"),
		cmd("x", "7", "1"),
		cmd("3", "y", "1"),
		cmd("3", "7", "z"),
		cmd("-1", "7", "1"),
		cmd("3", "-1", "1"),
	} {
		if _, _, _, err := ParseAnchor(bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("ParseAnchor(%q) returned %v", bad, err)
		}
	}
}

func TestSnapshotWindowAccessor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	w, err := CreateSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	for _, a := range [][3]int{{0, 0, 5}, {0, 1, 9}} {
		if err := w.Anchor(a[0], a[1], uint64(a[2])); err != nil {
			t.Fatal(err)
		}
	}
	if first, last := w.Window(); first != 5 || last != 9 {
		t.Errorf("window is [%d,%d], want [5,9]", first, last)
	}
}
