package persist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// Applier applies one logged effect to a keyspace. The command package
// provides the implementation; persist stays free of it so that the log
// format has no opinion about the command table.
//
// offset is the record's position in the log stream. An applier that is
// reconciling a fuzzy snapshot needs it to decide, per key, whether the
// shard holding that key already absorbed this record. One that is replaying
// a log from empty ignores it.
//
// When a snapshot is being loaded, offset is the anchor of the shard whose
// contents are being restored, so the value means the same thing in both
// paths: "the log position these bytes correspond to".
//
// Args alias the reader's buffer and must not be retained past the call.
type Applier interface {
	Apply(db int, offset uint64, args [][]byte) error
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
	// Dir is the directory holding the log segments.
	Dir string
	// Policy decides what to do with a log that cannot be read to its end.
	Policy CorruptPolicy
	// Logger receives progress and any warning about discarded records. It
	// may be nil.
	Logger *slog.Logger
	// From skips records that start before this offset. A recovery that
	// begins from a snapshot passes the snapshot's first anchor: everything
	// earlier is already in the snapshot for every shard.
	From uint64
}

// Result describes what a recovery pass did.
type Result struct {
	// Records is how many effects were applied.
	Records int64
	// Skipped is how many records were passed over because they predate
	// RecoverOptions.From, and are therefore already in the snapshot.
	Skipped int64
	// Offset is the stream offset just past the last applied record. A log
	// reopened for appending continues from here.
	Offset uint64
	// Damage is the reason the log could not be read to its end, wrapping
	// ErrTornTail or ErrCorrupt. It is nil for a clean log.
	Damage error
	// Discarded is how many bytes were cut from the end of the last
	// segment.
	Discarded int64
	// Segments is how many segment files were read.
	Segments int
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

	if _, err := adoptLegacyLog(opts.Dir); err != nil {
		return res, err
	}
	segs, err := Segments(opts.Dir)
	if err != nil {
		return res, err
	}
	res.Segments = len(segs)
	if len(segs) == 0 {
		return res, fmt.Errorf("persist: no log segments in %s: %w",
			opts.Dir, fs.ErrNotExist)
	}

	// A log that begins after the point recovery must start from is missing
	// records, and there is no way to tell which. Continuing would rebuild a
	// dataset with a hole in the middle of it and report success.
	if segs[0].Base > opts.From {
		return res, fmt.Errorf("persist: the log begins at offset %d but recovery "+
			"must start at %d: the records between them are in no file",
			segs[0].Base, opts.From)
	}
	res.Offset = segs[0].Base

	for i, seg := range segs {
		last := i == len(segs)-1
		if seg.End <= opts.From && !last {
			// Every record in this segment is already in the snapshot, so
			// it is not even opened. That is the point of segmenting: a
			// compacted-away range costs nothing to skip.
			res.Offset = seg.End
			continue
		}
		damaged, err := replaySegment(opts, seg, last, a, &res)
		if err != nil {
			return res, err
		}
		if damaged {
			break
		}
	}

	res.Elapsed = time.Since(start)
	if res.Damage == nil {
		logf(opts.Logger, slog.LevelInfo, "recovered from append log",
			"dir", opts.Dir, "segments", res.Segments, "records", res.Records,
			"skipped", res.Skipped, "offset", res.Offset, "elapsed", res.Elapsed)
	}
	return res, nil
}

// replaySegment applies one segment and deals with any damage at its end.
//
// Damage in a segment that is not the last is always fatal, whatever the
// policy. Truncating there would orphan every segment after it, so
// "truncate" would silently discard far more than the operator asked for --
// and a torn tail cannot occur anywhere but the last segment, because a new
// one is only started from a clean state.
func replaySegment(opts RecoverOptions, seg Segment, last bool, a Applier,
	res *Result) (damaged bool, err error) {

	r, f, err := OpenReader(seg.Path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	for r.Next() {
		rec := r.Record()
		if rec.Kind != KindEffect {
			return false, fmt.Errorf("%w: record at offset %d in %s is not an effect",
				ErrCorrupt, rec.Offset, seg.Path)
		}
		if rec.Offset < opts.From {
			res.Skipped++
			res.Offset = r.Offset()
			continue
		}
		if err := a.Apply(rec.DB, rec.Offset, rec.Args); err != nil {
			return false, fmt.Errorf("%w at offset %d: %w", ErrDiverged, rec.Offset, err)
		}
		res.Records++
		res.Offset = r.Offset()
	}
	if r.Err() == nil {
		return false, nil
	}
	res.Damage = r.Err()

	if !last {
		return true, fmt.Errorf("persist: %s is damaged but is not the last segment; "+
			"truncating it would discard every segment after it: %w",
			seg.Path, res.Damage)
	}
	if opts.Policy == PolicyRefuse {
		return true, fmt.Errorf("persist: %s is damaged after %d records and "+
			"corrupt-log-policy is refuse: %w", seg.Path, res.Records, res.Damage)
	}

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return true, err
	}
	res.Discarded = size - r.FileSize()

	// A torn tail is the ordinary consequence of a crash during a write and
	// costs at most the command that was in flight. A checksum failure is
	// storage damage, and everything after it is being thrown away too --
	// that is a different sentence for an operator to read, so it gets one.
	if errors.Is(res.Damage, ErrTornTail) {
		logf(opts.Logger, slog.LevelWarn, "append log ends mid-record; "+
			"discarding the incomplete tail",
			"path", seg.Path, "records", res.Records, "offset", res.Offset,
			"discarded_bytes", res.Discarded, "reason", res.Damage)
	} else {
		logf(opts.Logger, slog.LevelError, "append log is corrupt; discarding it "+
			"from the damaged record onwards. Every write after that point is lost. "+
			"Set corrupt-log-policy to refuse to stop instead of truncating",
			"path", seg.Path, "records", res.Records, "offset", res.Offset,
			"discarded_bytes", res.Discarded, "reason", res.Damage)
	}
	return true, truncateAndSync(seg.Path, r.FileSize())
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
