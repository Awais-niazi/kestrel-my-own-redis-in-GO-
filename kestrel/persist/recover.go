package persist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// Applier applies one logged effect to a keyspace. The command package
// provides the implementation; persist stays free of it so that the log
// format has no opinion about the command table.
//
// Args alias the reader's buffer and must not be retained past the call.
type Applier interface {
	Apply(db int, args [][]byte) error
}

// CorruptPolicy decides what happens when a log cannot be read to its end.
type CorruptPolicy int

// Corrupt-log policies, matching the corrupt-log-policy directive.
const (
	// PolicyTruncate discards the unreadable remainder and starts with what
	// could be read.
	PolicyTruncate CorruptPolicy = iota
	// PolicyRefuse declines to start, leaving the file untouched for an
	// operator to inspect.
	PolicyRefuse
)

func (p CorruptPolicy) String() string {
	if p == PolicyRefuse {
		return "refuse"
	}
	return "truncate"
}

// ParseCorruptPolicy converts a corrupt-log-policy configuration value.
func ParseCorruptPolicy(s string) (CorruptPolicy, error) {
	switch s {
	case "truncate":
		return PolicyTruncate, nil
	case "refuse":
		return PolicyRefuse, nil
	}
	return 0, fmt.Errorf("persist: unknown corrupt-log-policy %q", s)
}

// RecoverOptions configures a recovery pass.
type RecoverOptions struct {
	// Path is the log to replay.
	Path string
	// Policy decides what to do with a log that cannot be read to its end.
	Policy CorruptPolicy
	// Logger receives progress and any warning about discarded records. It
	// may be nil.
	Logger *slog.Logger
}

// Result describes what a recovery pass did.
type Result struct {
	// Records is how many effects were applied.
	Records int64
	// Offset is the stream offset just past the last applied record. A log
	// reopened for appending continues from here.
	Offset uint64
	// Damage is the reason the log could not be read to its end, wrapping
	// ErrTornTail or ErrCorrupt. It is nil for a clean log.
	Damage error
	// Discarded is how many bytes were cut from the end of the file.
	Discarded int64
	// Elapsed is how long the pass took.
	Elapsed time.Duration
}

// ErrDiverged reports a record that did not replay cleanly. It is not
// corruption: the bytes were intact and the command was well formed, but
// running it against the recovered keyspace produced an error the leader did
// not get. That means the state being rebuilt is already not the state the
// log describes.
var ErrDiverged = errors.New("persist: log record did not replay")

// Recover replays a log into a keyspace through a.
//
// The caller must have put the keyspace into loading mode first, and must
// not have installed anything that would propagate the replayed effects back
// into a log.
//
// A missing file is returned as an error wrapping fs.ErrNotExist, which the
// caller reads as "no log yet" rather than as a failure.
func Recover(opts RecoverOptions, a Applier) (Result, error) {
	start := time.Now()
	var res Result

	r, f, err := OpenReader(opts.Path)
	if err != nil {
		return res, err
	}
	defer f.Close()

	res.Offset = r.Base()
	for r.Next() {
		rec := r.Record()
		if err := a.Apply(rec.DB, rec.Args); err != nil {
			return res, fmt.Errorf("%w at offset %d: %w", ErrDiverged, rec.Offset, err)
		}
		res.Records++
		res.Offset = r.Offset()
	}
	res.Damage = r.Err()
	res.Elapsed = time.Since(start)

	if res.Damage == nil {
		logf(opts.Logger, slog.LevelInfo, "recovered from append log",
			"path", opts.Path, "records", res.Records, "offset", res.Offset,
			"elapsed", res.Elapsed)
		return res, nil
	}

	torn := errors.Is(res.Damage, ErrTornTail)
	if opts.Policy == PolicyRefuse {
		return res, fmt.Errorf("persist: %s is damaged after %d records and "+
			"corrupt-log-policy is refuse: %w", opts.Path, res.Records, res.Damage)
	}

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return res, err
	}
	res.Discarded = size - r.FileSize()

	// A torn tail is the ordinary consequence of a crash during a write and
	// costs at most the command that was in flight. A checksum failure is
	// storage damage, and everything after it is being thrown away too --
	// that is a different sentence for an operator to read, so it gets one.
	if torn {
		logf(opts.Logger, slog.LevelWarn, "append log ends mid-record; "+
			"discarding the incomplete tail",
			"path", opts.Path, "records", res.Records, "offset", res.Offset,
			"discarded_bytes", res.Discarded, "reason", res.Damage)
	} else {
		logf(opts.Logger, slog.LevelError, "append log is corrupt; discarding it "+
			"from the damaged record onwards. Every write after that point is lost. "+
			"Set corrupt-log-policy to refuse to stop instead of truncating",
			"path", opts.Path, "records", res.Records, "offset", res.Offset,
			"discarded_bytes", res.Discarded, "reason", res.Damage)
	}

	if err := truncateAndSync(opts.Path, r.FileSize()); err != nil {
		return res, err
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

// truncateAndSync cuts a log to size and forces the change, so that a crash
// during recovery cannot leave the damaged tail to be found again and
// half-applied a second time.
func truncateAndSync(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}
	return f.Sync()
}

func logf(l *slog.Logger, level slog.Level, msg string, args ...any) {
	if l == nil {
		return
	}
	l.Log(context.Background(), level, msg, args...)
}
