package persist

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The append log is a sequence of segment files in the data directory:
//
//	kestrel-000001.log
//	kestrel-000002.log
//
// Stream offsets run across them, so a record's offset means the same thing
// whichever file it is in, and an offset a snapshot recorded stays
// comparable after the log has been compacted.
//
// Segments exist so that compaction is a deletion rather than a rewrite. A
// snapshot names the offset below which every record is dead, and any
// segment that ends at or before it can be unlinked. Nothing is copied, no
// buffer accumulates writes while a rewrite runs, and a ten gigabyte log
// costs an unlink instead of ten gigabytes of I/O.
//
// A new segment is started only when a snapshot has just been committed, so
// no size directive is needed and the steady state is two segments: the one
// straddling the current snapshot's first anchor, and the live one. The
// straddler is deleted at the next snapshot.

const (
	segmentPrefix = "kestrel-"
	segmentSuffix = ".log"
	// legacyLogName is the single-file log written before the log was
	// segmented. It is adopted as the first segment on startup.
	legacyLogName = "kestrel.log"
)

// Segment describes one log file.
type Segment struct {
	// Path is the file.
	Path string
	// Seq is its position in the sequence, from 1.
	Seq int
	// Base is the stream offset of its first record.
	Base uint64
	// End is the stream offset just past its last complete byte, derived
	// from the file size. A segment with a torn tail ends earlier in
	// practice, which the reader discovers.
	End uint64
}

func segmentName(seq int) string {
	return fmt.Sprintf("%s%06d%s", segmentPrefix, seq, segmentSuffix)
}

func parseSegmentSeq(name string) (int, bool) {
	if !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentSuffix) {
		return 0, false
	}
	mid := name[len(segmentPrefix) : len(name)-len(segmentSuffix)]
	n, err := strconv.Atoi(mid)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// Segments lists the log segments in dir, in stream order.
//
// It validates that the segments form one unbroken stream: each begins where
// the previous one ended. A gap means records exist in no file, and there is
// no way to tell which, so it is reported rather than replayed around.
func Segments(dir string) ([]Segment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var out []Segment
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		seq, ok := parseSegmentSeq(e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		seg, err := readSegmentHeader(path, seq)
		if err != nil {
			return nil, err
		}
		out = append(out, seg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })

	for i := 1; i < len(out); i++ {
		if out[i].Base != out[i-1].End {
			return nil, fmt.Errorf("%w: %s ends at offset %d but %s begins at %d; "+
				"the records between them are in no file",
				ErrCorrupt, filepath.Base(out[i-1].Path), out[i-1].End,
				filepath.Base(out[i].Path), out[i].Base)
		}
	}
	return out, nil
}

func readSegmentHeader(path string, seq int) (Segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return Segment{}, err
	}
	defer f.Close()

	var hb [fileHeaderSize]byte
	if _, err := f.ReadAt(hb[:], 0); err != nil {
		// A file too short to hold a header is not a kestrel log, whatever
		// it is named, and saying so is more use than reporting an EOF.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Segment{}, fmt.Errorf("%w: %s is shorter than a header",
				ErrBadHeader, path)
		}
		return Segment{}, fmt.Errorf("persist: reading header of %s: %w", path, err)
	}
	h, err := decodeFileHeader(hb[:], logMagic)
	if err != nil {
		return Segment{}, fmt.Errorf("persist: %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		return Segment{}, err
	}
	return Segment{Path: path, Seq: seq, Base: h.Base,
		End: h.Base + uint64(fi.Size()) - fileHeaderSize}, nil
}

// adoptLegacyLog renames a pre-segment kestrel.log into the first segment.
//
// It is a one-time migration for data directories written by the build that
// had a single log file. It runs only when there are no segments at all, so
// it cannot disturb a directory that has already been converted.
func adoptLegacyLog(dir string) (bool, error) {
	legacy := filepath.Join(dir, legacyLogName)
	if _, err := os.Stat(legacy); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	target := filepath.Join(dir, segmentName(1))
	if _, err := os.Stat(target); err == nil {
		return false, fmt.Errorf("persist: %s and %s both exist; remove whichever "+
			"is not wanted", legacy, target)
	}
	if err := os.Rename(legacy, target); err != nil {
		return false, err
	}
	return true, syncDir(dir)
}

// Log is the append log: one directory of segments, with the last one open
// for writing.
type Log struct {
	dir string

	mu   sync.Mutex
	cur  *segment
	segs []Segment // every segment, including the one being written
	// appended is closed and replaced every time a record is written, so a
	// follower can wait for the next one without polling. A closed channel
	// is the cheapest broadcast Go has, and unlike a sync.Cond it composes
	// with a select on a context.
	appended chan struct{}
	closed   bool

	fsync atomic.Int32
}

// OpenLog opens or creates the log in dir.
//
// An existing log is reopened at the end of its last segment, which the
// caller must already have made clean: recovery truncates a torn tail before
// this is called, because appending past one would bury the damage under
// valid data.
func OpenLog(dir string, fsync Fsync) (*Log, error) {
	if _, err := adoptLegacyLog(dir); err != nil {
		return nil, err
	}
	segs, err := Segments(dir)
	if err != nil {
		return nil, err
	}

	l := &Log{dir: dir, segs: segs, appended: make(chan struct{})}
	l.fsync.Store(int32(fsync))

	if len(segs) == 0 {
		return l, l.startSegmentLocked(1, 0)
	}
	last := segs[len(segs)-1]
	cur, err := openSegment(segmentOptions{Path: last.Path, Fsync: fsync})
	if err != nil {
		return nil, err
	}
	l.cur = cur
	return l, nil
}

func (l *Log) startSegmentLocked(seq int, base uint64) error {
	path := filepath.Join(l.dir, segmentName(seq))
	s, err := createSegment(segmentOptions{
		Path: path, Fsync: Fsync(l.fsync.Load()), Base: base,
	})
	if err != nil {
		return err
	}
	l.cur = s
	l.segs = append(l.segs, Segment{Path: path, Seq: seq, Base: base, End: base})
	return nil
}

// Append writes one effect and returns the stream offset it was written at.
func (l *Log) Append(db int, args [][]byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, err := l.cur.Append(db, args)
	if err == nil {
		l.wakeLocked()
	}
	return at, err
}

// wakeLocked releases every follower waiting for a new record.
func (l *Log) wakeLocked() {
	close(l.appended)
	l.appended = make(chan struct{})
}

// Appended returns a channel that is closed when the next record is written,
// or immediately if the log has been closed.
//
// The channel must be taken before the caller checks the offset it is
// waiting past. Taking it afterwards would miss a record written in between
// and the follower would sleep with data available.
func (l *Log) Appended() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appended
}

// OldestOffset is the earliest offset the log can still serve. Anything
// before it was in a segment that compaction unlinked, so a replica asking
// for it needs a full resynchronisation rather than a partial one.
func (l *Log) OldestOffset() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.segs[0].Base
}

// Offset is the stream offset just past the last record written.
func (l *Log) Offset() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur.Offset()
}

// Roll closes the current segment and starts the next one at the offset the
// stream has reached.
//
// It is called after a snapshot has been committed, so that the segments the
// snapshot made redundant become whole files that Prune can unlink.
func (l *Log) Roll() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	at := l.cur.Offset()
	seq := l.segs[len(l.segs)-1].Seq + 1
	defer l.wakeLocked()
	if err := l.cur.Sync(); err != nil {
		return err
	}
	// The finished segment's recorded end is fixed at the point the new one
	// begins, so that a later contiguity check compares the two directly.
	l.segs[len(l.segs)-1].End = at
	if err := l.cur.Close(); err != nil {
		return err
	}
	return l.startSegmentLocked(seq, at)
}

// Prune deletes every segment that ends at or before below, which a snapshot
// has made redundant.
//
// It must be called only after that snapshot has been committed. Deleting
// first and crashing before the rename would leave a dataset that cannot be
// rebuilt from anything.
//
// The segment currently being written is never deleted, whatever its range.
func (l *Log) Prune(below uint64) (removed int, freed int64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	keep := l.segs[:0:0]
	for i, s := range l.segs {
		last := i == len(l.segs)-1
		if last || s.End > below {
			keep = append(keep, s)
			continue
		}
		fi, statErr := os.Stat(s.Path)
		if statErr == nil {
			freed += fi.Size()
		}
		if rmErr := os.Remove(s.Path); rmErr != nil {
			// A segment that will not delete is not a correctness problem --
			// recovery skips its records -- so the rest are still tried and
			// the failure is reported.
			keep = append(keep, s)
			err = rmErr
			freed -= fi.Size()
			continue
		}
		removed++
	}
	l.segs = keep
	if removed > 0 {
		if dirErr := syncDir(l.dir); dirErr != nil && err == nil {
			err = dirErr
		}
	}
	return removed, freed, err
}

// AppendRaw writes a record that was framed elsewhere, which is how a
// replica persists what its leader sent.
//
// The bytes are written unchanged, so the replica's log is byte-identical to
// the leader's over the range they share, and its offsets are the leader's
// offsets. That is what lets a restarted replica quote a position its leader
// recognises without any translation.
func (l *Log) AppendRaw(raw []byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, err := l.cur.AppendRaw(raw)
	if err == nil {
		l.wakeLocked()
	}
	return at, err
}

// Reset discards every segment and starts again at base.
//
// A replica does this when its leader hands it a snapshot: the records it
// holds belong to a history it is being told to abandon, and keeping them
// would leave offsets that mean something else. Nothing else should call it.
func (l *Log) Reset(base uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.cur.Close(); err != nil {
		return err
	}
	for _, s := range l.segs {
		if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	l.segs = l.segs[:0]
	if err := l.startSegmentLocked(1, base); err != nil {
		return err
	}
	return syncDir(l.dir)
}

// Segments returns the segments the log currently spans.
func (l *Log) Segments() []Segment {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Segment, len(l.segs))
	copy(out, l.segs)
	out[len(out)-1].End = l.cur.Offset()
	return out
}

// Sync forces the current segment.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur.Sync()
}

// SetFsync changes the durability policy.
func (l *Log) SetFsync(f Fsync) {
	l.fsync.Store(int32(f))
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur.SetFsync(f)
}

// Fsync reports the current durability policy.
func (l *Log) Fsync() Fsync { return Fsync(l.fsync.Load()) }

// Err reports the first write or fsync failure.
func (l *Log) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur.Err()
}

// Close forces and closes the current segment.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		// Followers are woken so they observe the close rather than
		// blocking until a record that will never come.
		l.wakeLocked()
	}
	return l.cur.Close()
}

// Closed reports whether the log has been closed.
func (l *Log) Closed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Dir is the directory the log lives in.
func (l *Log) Dir() string { return l.dir }

// Stats samples the log. Size is the total across every segment, which is
// what an operator watching disk use wants, and what the growth-based
// compaction trigger compares against.
func (l *Log) Stats() Stats {
	l.mu.Lock()
	cur := l.cur
	segs := make([]Segment, len(l.segs))
	copy(segs, l.segs)
	l.mu.Unlock()

	s := cur.Stats()
	s.Segments = len(segs)
	var total int64
	for _, seg := range segs {
		if fi, err := os.Stat(seg.Path); err == nil {
			total += fi.Size()
		}
	}
	s.Size = total
	return s
}
