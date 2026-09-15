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

// File names inside the data directory.
const (
	logFileName      = "kestrel.log"
	snapshotFileName = "kestrel.snapshot"
)

// persistence owns the append log and drives startup recovery.
//
// It is nil when appendonly is off, which is the one case where the server
// genuinely keeps nothing. Everywhere else the rule is that a write is not
// acknowledged until its effect has reached the log, subject to the
// appendfsync policy.
type persistence struct {
	dir      string
	logPath  string
	snapPath string

	log *persist.Log

	// failed holds the first write or fsync error. Once it is set the server
	// refuses writes: continuing would acknowledge data that is not in the
	// log, which is worse than an outage because it looks like success.
	failed atomic.Pointer[error]

	lastSave atomic.Int64 // unix seconds
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
		logPath:  filepath.Join(snap.Dir, logFileName),
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
		Path: p.logPath, Policy: policy, Logger: s.log, From: from,
	}, replayer)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		// A snapshot with no log beside it is the normal state right after a
		// rewrite that has not been written to since.
		s.log.Info("no append log found", "path", p.logPath)
	default:
		return fmt.Errorf("replaying %s: %w", p.logPath, err)
	}

	if err := p.openLog(snap, res.Offset); err != nil {
		return err
	}
	s.log.Info("recovery complete",
		"keys", s.ks.TotalKeys(), "log_records", res.Records,
		"log_records_skipped", res.Skipped, "offset", p.log.Offset(),
		"elapsed", time.Since(start))
	return nil
}

// openLog opens the log for appending, creating it when recovery found none.
//
// A new log created beside an existing snapshot starts at the offset
// recovery reached, not at zero, so that the stream stays continuous across
// the gap and a later snapshot's anchors remain comparable with it.
func (p *persistence) openLog(snap *config.Values, resumeAt uint64) error {
	fsync, err := persist.ParseFsync(snap.AppendFsync)
	if err != nil {
		return err
	}
	opts := persist.Options{Path: p.logPath, Fsync: fsync, Base: resumeAt}

	switch _, statErr := os.Stat(p.logPath); {
	case statErr == nil:
		p.log, err = persist.Open(opts)
	case errors.Is(statErr, fs.ErrNotExist):
		p.log, err = persist.Create(opts)
	default:
		return statErr
	}
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
