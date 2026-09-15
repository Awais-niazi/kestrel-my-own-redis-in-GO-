package persist

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Fsync selects how often the log is forced to stable storage.
type Fsync int32

// Fsync policies, matching the appendfsync directive.
const (
	// FsyncEverySec forces the log once a second in the background. A crash
	// can lose up to a second of writes; a kill of the process alone loses
	// nothing, because every append reaches the operating system before the
	// reply is sent.
	FsyncEverySec Fsync = iota
	// FsyncAlways forces the log before the append returns, so a write that
	// has been acknowledged is on stable storage.
	FsyncAlways
	// FsyncNo never forces the log and leaves the decision to the operating
	// system.
	FsyncNo
)

func (f Fsync) String() string {
	switch f {
	case FsyncAlways:
		return "always"
	case FsyncNo:
		return "no"
	default:
		return "everysec"
	}
}

// ParseFsync converts an appendfsync configuration value.
func ParseFsync(s string) (Fsync, error) {
	switch s {
	case "always":
		return FsyncAlways, nil
	case "everysec":
		return FsyncEverySec, nil
	case "no":
		return FsyncNo, nil
	}
	return 0, fmt.Errorf("persist: unknown appendfsync policy %q", s)
}

// Options configures a log.
type segmentOptions struct {
	// Path is the log file.
	Path string
	// Fsync is the initial durability policy. It can be changed later with
	// SetFsync, because appendfsync is a mutable directive.
	Fsync Fsync
	// Base is the stream offset of the first record, used only by Create.
	// A rewrite passes the offset the old log had reached, so that offsets
	// recorded by a snapshot stay meaningful across a compaction.
	Base uint64
}

// Log is an append-only file of canonical effects.
//
// Append is safe for concurrent use and is the one point at which every
// write in the server serializes. That is inherent to a single ordered log:
// the order recorded here is the order a replica and a recovery will replay,
// so it has to be decided somewhere.
type segment struct {
	path string

	mu     sync.Mutex
	f      *os.File
	buf    []byte // reused encode buffer
	offset uint64 // stream offset just past the last record
	dirty  bool   // written to the OS but not yet forced
	err    error

	fsync atomic.Int32

	writes    atomic.Int64
	syncs     atomic.Int64
	lastSyncN atomic.Int64 // nanoseconds of the most recent fsync

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// Create starts a new log file. It fails if the path already exists, so that
// a rewrite cannot silently destroy the log it is replacing.
func createSegment(opts segmentOptions) (*segment, error) {
	f, err := os.OpenFile(opts.Path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	h := fileHeader{
		Version: fileVersion,
		Base:    opts.Base,
		Created: uint64(time.Now().UnixMilli()),
	}
	if _, err := f.Write(h.encode(logMagic)); err != nil {
		f.Close()
		return nil, err
	}
	// The header and the directory entry are both forced before the log is
	// used. Without the directory sync a crash can leave a file that the
	// filesystem has not recorded, which recovery would read as "no log at
	// all" rather than as an empty one -- a silent, total data loss.
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := syncDir(filepath.Dir(opts.Path)); err != nil {
		f.Close()
		return nil, err
	}
	return startSegment(f, opts, opts.Base), nil
}

// Open reopens an existing log for appending.
//
// The caller must already have removed any torn tail, which recovery does:
// the stream offset is derived from the file size rather than by scanning,
// so appending to a file that still ends mid-record would bury the damage
// under valid data.
func openSegment(opts segmentOptions) (*segment, error) {
	f, err := os.OpenFile(opts.Path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	var hb [fileHeaderSize]byte
	if _, err := f.ReadAt(hb[:], 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("persist: reading header of %s: %w", opts.Path, err)
	}
	h, err := decodeFileHeader(hb[:], logMagic)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("persist: %s: %w", opts.Path, err)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, err
	}
	return startSegment(f, opts, h.Base+uint64(size)-fileHeaderSize), nil
}

func startSegment(f *os.File, opts segmentOptions, offset uint64) *segment {
	l := &segment{
		path:   opts.Path,
		f:      f,
		offset: offset,
		buf:    make([]byte, 0, 4096),
		stop:   make(chan struct{}),
	}
	l.fsync.Store(int32(opts.Fsync))
	l.wg.Add(1)
	go l.syncLoop()
	return l
}

// Append writes one effect and returns the stream offset it was written at.
//
// The record reaches the operating system before Append returns whatever the
// policy is; the policy governs only whether it is also forced to stable
// storage. That is what makes everysec survive a process kill and lose data
// only to a machine failure.
func (l *segment) Append(db int, args [][]byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return 0, l.err
	}

	if n := recordSize(args); cap(l.buf) < n {
		l.buf = make([]byte, 0, n)
	}
	l.buf = encodeRecord(l.buf[:0], db, KindEffect, args)

	at := l.offset
	if _, err := l.f.Write(l.buf); err != nil {
		return 0, l.fail(err)
	}
	l.offset += uint64(len(l.buf))
	l.dirty = true
	l.writes.Add(1)

	if Fsync(l.fsync.Load()) == FsyncAlways {
		if err := l.syncLocked(); err != nil {
			return 0, err
		}
	}
	return at, nil
}

// Offset is the stream offset just past the last record written. A snapshot
// anchors each shard to the value this returns at the instant the shard is
// serialized.
func (l *segment) Offset() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.offset
}

// Sync forces the log to stable storage regardless of policy.
func (l *segment) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.syncLocked()
}

func (l *segment) syncLocked() error {
	if l.err != nil {
		return l.err
	}
	if !l.dirty {
		return nil
	}
	start := time.Now()
	if err := l.f.Sync(); err != nil {
		return l.fail(err)
	}
	l.lastSyncN.Store(int64(time.Since(start)))
	l.syncs.Add(1)
	l.dirty = false
	return nil
}

// syncLoop forces the log once a second while the policy asks for it.
//
// The ticker runs whatever the policy is, and checks it on each tick. That
// costs one wakeup a second under always and no, and in exchange
// appendfsync becomes a plain atomic store rather than a goroutine that has
// to be started and stopped underneath concurrent appends.
func (l *segment) syncLoop() {
	defer l.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			if Fsync(l.fsync.Load()) == FsyncEverySec {
				l.mu.Lock()
				//nolint:errcheck // recorded on the log; surfaced by Err.
				_ = l.syncLocked()
				l.mu.Unlock()
			}
		}
	}
}

// SetFsync changes the durability policy. CONFIG SET appendfsync reaches
// this.
func (l *segment) SetFsync(f Fsync) { l.fsync.Store(int32(f)) }

// Fsync reports the current durability policy.
func (l *segment) Fsync() Fsync { return Fsync(l.fsync.Load()) }

// Err reports the first write or fsync failure. Once set, every subsequent
// append fails with it: a log with a hole in it is worse than no log, so the
// server must refuse writes rather than carry on and produce a file that
// recovery will silently truncate.
func (l *segment) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *segment) fail(err error) error {
	if l.err == nil {
		l.err = fmt.Errorf("persist: log %s: %w", l.path, err)
	}
	return l.err
}

// TruncateTo cuts the log back to a stream offset, discarding a torn tail.
// It is the one operation that shortens a log, and recovery is its only
// caller.
func (l *segment) TruncateTo(off uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if off > l.offset {
		return fmt.Errorf("persist: cannot truncate %s to offset %d, past its end %d",
			l.path, off, l.offset)
	}
	base, err := l.baseLocked()
	if err != nil {
		return err
	}
	if off < base {
		return fmt.Errorf("persist: cannot truncate %s to offset %d, before its first record %d",
			l.path, off, base)
	}
	size := int64(off-base) + fileHeaderSize
	if err := l.f.Truncate(size); err != nil {
		return l.fail(err)
	}
	if _, err := l.f.Seek(size, io.SeekStart); err != nil {
		return l.fail(err)
	}
	l.offset = off
	l.dirty = true
	return l.syncLocked()
}

func (l *segment) baseLocked() (uint64, error) {
	var hb [fileHeaderSize]byte
	if _, err := l.f.ReadAt(hb[:], 0); err != nil {
		return 0, err
	}
	h, err := decodeFileHeader(hb[:], logMagic)
	return h.Base, err
}

// Path is the file this log writes to.
func (l *segment) Path() string { return l.path }

// Stats reports the counters INFO persistence needs.
type Stats struct {
	Writes       int64
	Syncs        int64
	Offset       uint64
	Size         int64
	LastSyncTime time.Duration
	Policy       Fsync
	// Segments is how many files the log spans.
	Segments int
}

// Stats samples the log's counters.
func (l *segment) Stats() Stats {
	l.mu.Lock()
	offset := l.offset
	l.mu.Unlock()
	s := Stats{
		Writes:       l.writes.Load(),
		Syncs:        l.syncs.Load(),
		Offset:       offset,
		LastSyncTime: time.Duration(l.lastSyncN.Load()),
		Policy:       Fsync(l.fsync.Load()),
	}
	if fi, err := os.Stat(l.path); err == nil {
		s.Size = fi.Size()
	}
	return s
}

// Close stops the background sync, forces the log a final time and closes
// the file. A close that cannot force the log reports the failure rather
// than discarding it, because that is exactly the case where an operator
// believes a clean shutdown was durable.
func (l *segment) Close() error {
	l.stopOnce.Do(func() { close(l.stop) })
	l.wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()
	syncErr := l.syncLocked()
	closeErr := l.f.Close()
	if l.err == nil {
		l.err = errors.New("persist: log is closed")
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// syncDir forces a directory entry, so that a newly created file survives a
// crash. It is a no-op on platforms that refuse to open a directory.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Some filesystems reject fsync on a directory. That is not a
		// failure of the log, and refusing to start over it would be worse
		// than the durability it buys.
		var perr *os.PathError
		if errors.As(err, &perr) {
			return nil
		}
		return err
	}
	return nil
}
