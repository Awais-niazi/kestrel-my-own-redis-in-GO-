package persist

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// A snapshot file holds the whole dataset as records in the same framing the
// log uses, interleaved with anchors.
//
// Each shard is preceded by an anchor naming the log offset its contents
// correspond to. Shards are written in ascending order within a database and
// databases in ascending order, so the anchors are monotonically
// non-decreasing and the window they span is a single interval
// [first, last]. Recovery replays the log from the first anchor, applying
// each record only to the shards whose anchor precedes it.
//
// Reusing the log's framing rather than inventing a second format means one
// checksum, one decoder and one fuzz target cover both files. The cost is
// size: a snapshot stores a sorted set as ZADD arguments, where a packed
// binary encoding would be smaller and faster to load. That is a real cost
// and it is written down in docs/design-notes.md; it buys a recovery path
// that is one function instead of two.

// SnapshotWriter builds a snapshot file.
//
// It writes to a temporary file and renames it into place on Commit, so a
// crash part-way through leaves the previous snapshot intact. A snapshot
// that is half written is worse than one that is a few minutes old.
type SnapshotWriter struct {
	path string
	tmp  string
	f    *os.File
	w    *bufio.Writer
	buf  []byte

	anchors int
	records int64
	first   uint64
	last    uint64
	haveAny bool
	err     error
}

// CreateSnapshot starts a snapshot at path. Nothing appears there until
// Commit succeeds.
func CreateSnapshot(path string) (*SnapshotWriter, error) {
	tmp := path + ".tmp"
	// A leftover temporary file is the debris of an earlier crash, not
	// something to preserve: the snapshot it belonged to was never valid.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	sw := &SnapshotWriter{
		path: path,
		tmp:  tmp,
		f:    f,
		w:    bufio.NewWriterSize(f, 256<<10),
		buf:  make([]byte, 0, 8192),
	}
	// The header is rewritten by Commit, once the anchors are known.
	if _, err := sw.w.Write(fileHeader{Version: fileVersion,
		Created: uint64(time.Now().UnixMilli())}.encode(snapshotMagic)); err != nil {
		sw.fail(err)
		return nil, err
	}
	return sw, nil
}

// Anchor records that the shard about to be written corresponds to the log
// at offset.
//
// Anchors must be non-decreasing, which follows from serializing shards in
// ascending order. The check is here rather than left to the caller because
// an out-of-order anchor produces a snapshot that recovery will read as a
// window it can filter against, and quietly get wrong.
func (w *SnapshotWriter) Anchor(db, shard int, offset uint64) error {
	if w.err != nil {
		return w.err
	}
	if w.haveAny && offset < w.last {
		return w.fail(fmt.Errorf("persist: anchor for db %d shard %d went backwards, "+
			"%d after %d; shards must be serialized in ascending order",
			db, shard, offset, w.last))
	}
	if !w.haveAny {
		w.first = offset
		w.haveAny = true
	}
	w.last = offset
	w.anchors++
	return w.write(db, KindAnchor, [][]byte{
		strconv.AppendInt(nil, int64(db), 10),
		strconv.AppendInt(nil, int64(shard), 10),
		strconv.AppendUint(nil, offset, 10),
	})
}

// Record writes one command that rebuilds part of the dataset.
func (w *SnapshotWriter) Record(db int, args [][]byte) error {
	if w.err != nil {
		return w.err
	}
	if !w.haveAny {
		return w.fail(fmt.Errorf("persist: snapshot record written before any anchor"))
	}
	w.records++
	return w.write(db, KindEffect, args)
}

func (w *SnapshotWriter) write(db int, kind RecordKind, args [][]byte) error {
	if n := recordSize(args); cap(w.buf) < n {
		w.buf = make([]byte, 0, n)
	}
	w.buf = encodeRecord(w.buf[:0], db, kind, args)
	if _, err := w.w.Write(w.buf); err != nil {
		return w.fail(err)
	}
	return nil
}

// SnapshotInfo describes a finished snapshot.
type SnapshotInfo struct {
	// Path is the file that was written.
	Path string
	// Anchors is how many shards it covers.
	Anchors int
	// Records is how many rebuild commands it holds.
	Records int64
	// First and Last bound the window the anchors span. Recovery replays
	// the log from First.
	First, Last uint64
	// Size is the file's length in bytes.
	Size int64
}

// Commit finishes the snapshot and renames it into place.
//
// The order matters and is the usual one for an atomic replacement: flush,
// rewrite the header now that the window is known, force the file, rename,
// then force the directory. A crash before the rename leaves the previous
// snapshot; a crash after it leaves the new one; there is no point at which
// a reader can see a partial file under the real name.
func (w *SnapshotWriter) Commit() (SnapshotInfo, error) {
	var info SnapshotInfo
	if w.err != nil {
		return info, w.err
	}
	if !w.haveAny {
		return info, w.fail(fmt.Errorf("persist: snapshot has no anchors"))
	}
	if err := w.w.Flush(); err != nil {
		return info, w.fail(err)
	}
	h := fileHeader{Version: fileVersion, Base: w.first,
		Created: uint64(time.Now().UnixMilli())}
	if _, err := w.f.WriteAt(h.encode(snapshotMagic), 0); err != nil {
		return info, w.fail(err)
	}
	if err := w.f.Sync(); err != nil {
		return info, w.fail(err)
	}
	size, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		return info, w.fail(err)
	}
	if err := w.f.Close(); err != nil {
		return info, w.fail(err)
	}
	if err := os.Rename(w.tmp, w.path); err != nil {
		return info, w.fail(err)
	}
	if err := syncDir(filepath.Dir(w.path)); err != nil {
		return info, w.fail(err)
	}
	w.err = errSnapshotClosed
	return SnapshotInfo{Path: w.path, Anchors: w.anchors, Records: w.records,
		First: w.first, Last: w.last, Size: size}, nil
}

// Abort discards a snapshot in progress and removes its temporary file.
// Calling it after Commit is a no-op.
func (w *SnapshotWriter) Abort() {
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	os.Remove(w.tmp)
	if w.err == nil {
		w.err = errSnapshotAborted
	}
}

// Window is the interval the anchors span, and is meaningful only after at
// least one anchor has been written.
func (w *SnapshotWriter) Window() (first, last uint64) { return w.first, w.last }

func (w *SnapshotWriter) fail(err error) error {
	if w.err == nil {
		w.err = err
	}
	return w.err
}

var (
	errSnapshotClosed  = fmt.Errorf("persist: snapshot is committed")
	errSnapshotAborted = fmt.Errorf("persist: snapshot was aborted")
)

// ParseAnchor decodes an anchor record's payload.
func ParseAnchor(args [][]byte) (db, shard int, offset uint64, err error) {
	if len(args) != 3 {
		return 0, 0, 0, fmt.Errorf("%w: anchor has %d fields, want 3", ErrCorrupt, len(args))
	}
	d, err1 := strconv.Atoi(string(args[0]))
	s, err2 := strconv.Atoi(string(args[1]))
	o, err3 := strconv.ParseUint(string(args[2]), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || d < 0 || s < 0 {
		return 0, 0, 0, fmt.Errorf("%w: unreadable anchor %q", ErrCorrupt, args)
	}
	return d, s, o, nil
}

// ShardRef names one shard of one database.
type ShardRef struct{ DB, Shard int }

// Anchors maps each shard to the log offset its snapshot contents came from.
// Recovery applies a log record to a shard only when the shard's anchor
// precedes it.
type Anchors map[ShardRef]uint64

// SnapshotLoad describes what loading a snapshot did.
type SnapshotLoad struct {
	// Records is how many rebuild commands were applied.
	Records int64
	// Anchors is where each shard stood in the log.
	Anchors Anchors
	// First and Last bound the window the anchors span. The log is replayed
	// from First.
	First, Last uint64
}

// LoadSnapshot reads a snapshot file and applies it through a.
//
// Damage is always fatal here, unlike in a log. A snapshot is renamed into
// place only once it is complete, so a file under the real name that cannot
// be read to its end is not a crash artefact -- it is a file that has been
// corrupted since it was written, and there is no prefix of it that means
// anything on its own.
func LoadSnapshot(path string, a Applier) (SnapshotLoad, error) {
	var out SnapshotLoad
	out.Anchors = make(Anchors)

	r, f, err := OpenSnapshotReader(path)
	if err != nil {
		return out, err
	}
	defer f.Close()

	first := true
	var last uint64
	for r.Next() {
		rec := r.Record()
		if rec.Kind == KindAnchor {
			db, shard, off, err := ParseAnchor(rec.Args)
			if err != nil {
				return out, fmt.Errorf("persist: %s: %w", path, err)
			}
			if !first && off < last {
				return out, fmt.Errorf("%w: %s has anchor %d after %d; shards must be "+
					"serialized in ascending order", ErrCorrupt, path, off, last)
			}
			if first {
				out.First, first = off, false
			}
			last, out.Last = off, off
			out.Anchors[ShardRef{DB: db, Shard: shard}] = off
			continue
		}
		if err := a.Apply(rec.DB, rec.Args); err != nil {
			return out, fmt.Errorf("%w at offset %d: %w", ErrDiverged, rec.Offset, err)
		}
		out.Records++
	}
	if err := r.Err(); err != nil {
		return out, fmt.Errorf("persist: %s is unusable: %w", path, err)
	}
	if first {
		return out, fmt.Errorf("%w: %s has no anchors", ErrCorrupt, path)
	}
	return out, nil
}
