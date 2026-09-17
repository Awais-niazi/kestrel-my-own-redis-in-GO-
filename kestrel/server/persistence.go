package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"kestrel/command"
	"kestrel/config"
	"kestrel/persist"
)

// snapshotFileName is the snapshot inside the data directory. The append log
// is a set of segment files that persist names and discovers for itself.
const snapshotFileName = "kestrel.snapshot"

// persistence owns the append log and drives startup recovery.
//
// It is nil when appendonly is off, which is the one case where the server
// genuinely keeps nothing. Everywhere else the rule is that a write is not
// acknowledged until its effect has reached the log, subject to the
// appendfsync policy.
type persistence struct {
	dir      string
	snapPath string

	log *persist.Log

	// failed holds the first write or fsync error. Once it is set the server
	// refuses writes: continuing would acknowledge data that is not in the
	// log, which is worse than an outage because it looks like success.
	failed atomic.Pointer[error]

	lastSave atomic.Int64 // unix seconds

	// running guards against two snapshots at once. A second pass would
	// overwrite the first's temporary file and produce anchors from two
	// different walks.
	running atomic.Bool
	// changesAtSave and sizeAtSave are the baselines the scheduling
	// triggers compare against.
	changesAtSave atomic.Int64
	sizeAtSave    atomic.Int64
}

// openPersistence prepares the data directory and returns the subsystem, or
// nil when persistence is disabled.
func (s *Server) openPersistence(snap *config.Values) (*persistence, error) {
	if !snap.AppendOnly {
		return nil, nil
	}
	if snap.Dir == "" {
		return nil, errors.New("appendonly is on but dir is empty")
	}
	if err := os.MkdirAll(snap.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", snap.Dir, err)
	}
	p := &persistence{
		dir:      snap.Dir,
		snapPath: filepath.Join(snap.Dir, snapshotFileName),
	}
	p.lastSave.Store(time.Now().Unix())
	return p, nil
}

// restore rebuilds the keyspace from whatever is on disk and leaves the log
// open for appending.
//
// The order is snapshot first, then the log from the snapshot's earliest
// anchor, with each log record applied only to the shards that had not
// already absorbed it. The keyspace is in loading mode throughout, so a TTL
// that elapsed while the server was down does not delete a key part-way
// through the replay: the log carries the leader's own DEL for anything that
// really expired (see engine.Keyspace.SetLoading).
func (s *Server) restore(p *persistence, snap *config.Values) error {
	policy, err := persist.ParseCorruptPolicy(snap.CorruptLogPolicy)
	if err != nil {
		return err
	}

	s.ks.SetLoading(true)
	defer s.ks.SetLoading(false)

	start := time.Now()
	var from uint64
	var anchors persist.Anchors

	switch load, err := persist.LoadSnapshot(p.snapPath, command.NewReplayer(s)); {
	case err == nil:
		from, anchors = load.First, load.Anchors
		s.log.Info("loaded snapshot", "path", p.snapPath, "records", load.Records,
			"shards", len(load.Anchors), "window_first", load.First,
			"window_last", load.Last)
	case errors.Is(err, fs.ErrNotExist):
		s.log.Info("no snapshot found; rebuilding from the append log alone",
			"path", p.snapPath)
	default:
		return fmt.Errorf("loading snapshot: %w", err)
	}

	replayer := command.NewReplayer(s)
	if anchors != nil {
		// Every shard was serialized at a different instant, so a record in
		// the tail belongs to some shards and not others.
		replayer.Filter(func(db, shard int) uint64 {
			return anchors[persist.ShardRef{DB: db, Shard: shard}]
		})
	}

	res, err := persist.Recover(persist.RecoverOptions{
		Dir: p.dir, Policy: policy, Logger: s.log, From: from,
	}, replayer)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		// No segments at all is the normal state of a fresh data directory.
		s.log.Info("no append log found", "dir", p.dir)
	default:
		return fmt.Errorf("replaying the append log in %s: %w", p.dir, err)
	}

	if err := p.openLog(snap); err != nil {
		return err
	}
	s.log.Info("recovery complete",
		"keys", s.ks.TotalKeys(), "log_records", res.Records,
		"log_records_skipped", res.Skipped, "log_segments", res.Segments,
		"offset", p.log.Offset(), "elapsed", time.Since(start))
	return nil
}

// openLog opens the log for appending, creating the first segment when
// recovery found none.
//
// Recovery has already truncated any torn tail, which matters: appending past
// one would bury the damage under valid data.
func (p *persistence) openLog(snap *config.Values) error {
	fsync, err := persist.ParseFsync(snap.AppendFsync)
	if err != nil {
		return err
	}
	p.log, err = persist.OpenLog(p.dir, fsync)
	return err
}

// Append writes one effect. It satisfies EffectLog.
//
// A failure is recorded once and then refused for every subsequent write, by
// resolve: a log with a hole in it cannot be replayed, so the honest
// response is to stop accepting writes rather than to keep taking them and
// lose them quietly.
func (p *persistence) Append(db int, args [][]byte) {
	if p.failed.Load() != nil {
		return
	}
	if _, err := p.log.Append(db, args); err != nil {
		p.failed.CompareAndSwap(nil, &err)
	}
}

// Err reports the first write or fsync failure, or nil.
func (p *persistence) Err() error {
	if e := p.failed.Load(); e != nil {
		return *e
	}
	if p.log == nil {
		return nil
	}
	// A background fsync failure never passes through Append, so the log is
	// asked directly. An operator who has lost durability should find out
	// from the next write, not from the next crash.
	if err := p.log.Err(); err != nil {
		p.failed.CompareAndSwap(nil, &err)
		return err
	}
	return nil
}

// Sync forces the log, whatever the configured policy.
func (p *persistence) Sync() error {
	if p.log == nil {
		return nil
	}
	if err := p.log.Sync(); err != nil {
		return err
	}
	p.lastSave.Store(time.Now().Unix())
	return nil
}

// Close forces and closes the log.
func (p *persistence) Close() error {
	if p.log == nil {
		return nil
	}
	return p.log.Close()
}

// stats samples the log for INFO.
func (p *persistence) stats() persist.Stats {
	if p == nil || p.log == nil {
		return persist.Stats{}
	}
	return p.log.Stats()
}

// PersistenceError reports why writes are being refused, or nil.
func (s *Server) PersistenceError() error {
	if s.persist == nil {
		return nil
	}
	return s.persist.Err()
}

// Snapshot writes a point-in-time copy of the dataset and compacts the log.
//
// The order is the one crash safety depends on: the snapshot is renamed into
// place, then the log rolls, then the segments the snapshot made redundant
// are unlinked. A crash between the rename and the unlink leaves segments
// recovery will skip; a crash before the rename leaves the previous
// snapshot. At no point has a record that is still needed been deleted.
func (p *persistence) Snapshot(s *Server) (SnapshotOutcome, error) {
	var out SnapshotOutcome
	if !p.running.CompareAndSwap(false, true) {
		return out, errSnapshotRunning
	}
	defer p.running.Store(false)

	started := time.Now()
	w, err := persist.CreateSnapshot(p.snapPath)
	if err != nil {
		return out, err
	}

	cfg := s.cfg.Snapshot()
	opts := command.SnapshotOptions{
		Offset:        p.log.Offset,
		BatchElements: cfg.SnapshotBatchKeys,
	}
	// The guard is held for the whole pass, so no cross-shard
	// read-modify-write effect can land inside the window the anchors span.
	var walkErr error
	s.ks.SnapshotWindow(func() { walkErr = command.WriteSnapshot(s.ks, w, opts) })
	if walkErr != nil {
		w.Abort()
		return out, walkErr
	}

	info, err := w.Commit()
	if err != nil {
		w.Abort()
		return out, err
	}
	out.Records, out.First, out.Last = info.Records, info.First, info.Last
	out.SnapshotBytes = info.Size

	if err := p.log.Roll(); err != nil {
		// The snapshot is good and is in place; only the compaction failed.
		// Reporting it without discarding the snapshot is the useful
		// outcome, because the next attempt will compact both.
		return out, fmt.Errorf("rolling the log after a snapshot: %w", err)
	}
	removed, freed, err := p.log.Prune(info.First)
	out.SegmentsRemoved, out.BytesFreed = removed, freed

	p.lastSave.Store(time.Now().Unix())
	p.changesAtSave.Store(s.ks.Stats().Changes)
	p.sizeAtSave.Store(p.log.Stats().Size)
	out.Elapsed = time.Since(started)
	return out, err
}

// SnapshotOutcome describes a completed snapshot.
type SnapshotOutcome struct {
	Records         int64
	First, Last     uint64
	SnapshotBytes   int64
	SegmentsRemoved int
	BytesFreed      int64
	Elapsed         time.Duration
}

var (
	// ErrNoPersistence is returned when a save is asked for on a server that
	// is not persisting anything.
	ErrNoPersistence   = errors.New("persistence is disabled")
	errSnapshotRunning = errors.New("a snapshot is already in progress")
)

// Snapshot satisfies the command layer's Host. background decides whether
// the caller waits for the result.
func (s *Server) Snapshot(background bool) error {
	p := s.persist
	if p == nil {
		return ErrNoPersistence
	}
	if background {
		if p.running.Load() {
			return errSnapshotRunning
		}
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			s.snapshotAndLog(p, "background")
		}()
		return nil
	}
	return s.snapshotAndLog(p, "foreground")
}

func (s *Server) snapshotAndLog(p *persistence, why string) error {
	out, err := p.Snapshot(s)
	if err != nil {
		s.log.Error("snapshot failed", "trigger", why, "error", err)
		return err
	}
	s.log.Info("snapshot complete", "trigger", why, "records", out.Records,
		"window_first", out.First, "window_last", out.Last,
		"snapshot_bytes", out.SnapshotBytes, "segments_removed", out.SegmentsRemoved,
		"bytes_freed", out.BytesFreed, "elapsed", out.Elapsed)
	return nil
}

// LastSave is when the last snapshot completed, in unix seconds.
func (s *Server) LastSave() int64 {
	if s.persist == nil {
		return 0
	}
	return s.persist.lastSave.Load()
}

// maintenance takes snapshots on a schedule.
//
// It checks once a second rather than sleeping for the whole interval, so
// that a CONFIG SET of snapshot-interval takes effect without a restart and
// the growth trigger is noticed promptly under a write burst.
func (s *Server) maintenance(p *persistence) {
	defer s.workers.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-tick.C:
			if why := s.snapshotDue(p); why != "" {
				s.snapshotAndLog(p, why)
			}
		}
	}
}

// snapshotDue reports why a snapshot should run now, or "".
func (s *Server) snapshotDue(p *persistence) string {
	if p.running.Load() || s.IsLoading() || p.Err() != nil {
		return ""
	}
	cfg := s.cfg.Snapshot()

	// An idle server does not need a new snapshot of the same data, however
	// long it has been idle: a snapshot that changes nothing still costs a
	// full serialization pass and a shard-blocking pause.
	changed := s.ks.Stats().Changes - p.changesAtSave.Load()
	if changed <= 0 {
		return ""
	}

	if cfg.SnapshotInterval > 0 {
		age := time.Now().Unix() - p.lastSave.Load()
		if age >= int64(cfg.SnapshotInterval) {
			return "interval"
		}
	}

	// Growth is measured against the log's size just after the last
	// snapshot, which is what the percentage is a percentage of.
	if cfg.AutoRewritePercentage > 0 {
		size := p.log.Stats().Size
		if size >= cfg.AutoRewriteMinSize {
			base := p.sizeAtSave.Load()
			if base <= 0 {
				return "growth"
			}
			if size >= base+base*int64(cfg.AutoRewritePercentage)/100 {
				return "growth"
			}
		}
	}
	return ""
}
