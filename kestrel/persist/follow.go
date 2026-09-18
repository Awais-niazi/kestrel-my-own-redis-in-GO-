package persist

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Following the log is how a replica is fed.
//
// A replica needs "every effect from offset X onwards, then whatever comes
// next", and that is precisely what the log already holds: records in order,
// addressed by a stream offset that survives compaction and spans files. So
// there is no separate replication backlog and no ring buffer to size. The
// repl-backlog-size directive becomes a statement about how much log is
// retained, which is already decided by how often snapshots run.
//
// The consequence is that a partial resynchronisation is a seek, and the
// case where one is impossible -- the replica is further behind than the
// oldest retained segment -- is exactly the case where compaction has
// already removed the records, which the log can answer precisely rather
// than by guessing at a buffer's contents.

// ErrTooFarBehind reports that the records a follower asked for have been
// compacted away. The caller must fall back to a full resynchronisation.
var ErrTooFarBehind = errors.New("persist: requested offset is no longer in the log")

// ErrLogClosed reports that the log was closed while a follower was reading.
var ErrLogClosed = errors.New("persist: log is closed")

// Follower reads records from a stream offset and keeps up as more arrive.
//
// It never reads past the offset the log reports, so it cannot observe a
// record that is only part-way written. That bound is what makes a torn read
// impossible here, rather than something the reader has to detect: the log's
// offset only advances once a whole record has been handed to the operating
// system.
type Follower struct {
	log *Log
	off uint64

	f       *os.File
	r       *Reader
	seg     Segment
	keepRaw bool
}

// KeepRaw makes the follower fill Record.Raw, which replication forwards
// unchanged.
func (f *Follower) KeepRaw(v bool) {
	f.keepRaw = v
	if f.r != nil {
		f.r.KeepRaw(v)
	}
}

// Follow starts reading log at from.
func Follow(log *Log, from uint64) (*Follower, error) {
	fl := &Follower{log: log, off: from}
	if err := fl.seek(from); err != nil {
		return nil, err
	}
	return fl, nil
}

// Offset is the stream offset the follower has read up to.
func (f *Follower) Offset() uint64 { return f.off }

// seek opens the segment holding at and positions the reader on it.
func (f *Follower) seek(at uint64) error {
	f.closeSegment()

	segs := f.log.Segments()
	if at < segs[0].Base {
		return fmt.Errorf("%w: offset %d, oldest retained is %d",
			ErrTooFarBehind, at, segs[0].Base)
	}
	for i, s := range segs {
		if at >= s.End && i != len(segs)-1 {
			continue
		}
		r, file, err := OpenReaderAt(s.Path, at)
		if err != nil {
			return err
		}
		r.KeepRaw(f.keepRaw)
		f.f, f.r, f.seg = file, r, s
		return nil
	}
	return fmt.Errorf("%w: offset %d is past the end of the log", ErrCorrupt, at)
}

func (f *Follower) closeSegment() {
	if f.f != nil {
		f.f.Close()
		f.f, f.r = nil, nil
	}
}

// Close releases the segment the follower has open.
func (f *Follower) Close() { f.closeSegment() }

// Next returns the next record, blocking until one is available or ctx is
// done.
//
// The returned record's Args alias the follower's buffer and are valid only
// until the next call.
func (f *Follower) Next(ctx context.Context) (Record, error) {
	for {
		limit := f.log.Offset()
		if f.off < limit {
			rec, ok, err := f.read(limit)
			if err != nil {
				return Record{}, err
			}
			if ok {
				return rec, nil
			}
			continue
		}
		if f.log.Closed() {
			return Record{}, ErrLogClosed
		}
		// The channel is taken before the offset is re-checked, so a record
		// written in between wakes this rather than being missed.
		wait := f.log.Appended()
		if f.log.Offset() > f.off {
			continue
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}
}

// read returns the next record from the current segment, or ok=false when
// the follower has moved on to another segment and should try again.
func (f *Follower) read(limit uint64) (Record, bool, error) {
	if f.r == nil {
		return Record{}, false, ErrLogClosed
	}
	if f.r.Next() {
		rec := f.r.Record()
		f.off = f.r.Offset()
		return rec, true, nil
	}
	if err := f.r.Err(); err != nil {
		// A record cannot be part-written from a follower's point of view,
		// because the log's offset only advances once a whole one has been
		// written and nothing is read past it. Damage here is damage.
		return Record{}, false, fmt.Errorf("following %s: %w", f.seg.Path, err)
	}

	// Clean end of this segment. Either the log has rolled and the next one
	// holds the records, or the file has grown since it was opened.
	if f.off < limit {
		if err := f.seek(f.off); err != nil {
			return Record{}, false, err
		}
		return Record{}, false, nil
	}
	return Record{}, false, nil
}
