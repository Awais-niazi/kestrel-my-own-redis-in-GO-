package command

import (
	"fmt"

	"kestrel/engine"
)

// SnapshotSink receives a snapshot's contents.
//
// The interface is declared here rather than imported so that the command
// layer does not depend on the persist package: persist.SnapshotWriter
// satisfies it structurally, and the server wires the two together. The
// dependency would otherwise point sideways for no benefit, and a test can
// collect records without touching a file.
type SnapshotSink interface {
	// Anchor records the log offset the next shard's contents correspond to.
	Anchor(db, shard int, offset uint64) error
	// Record writes one command that rebuilds part of the dataset.
	Record(db int, args [][]byte) error
}

// SnapshotOptions configures a snapshot pass.
type SnapshotOptions struct {
	// Offset returns the log's current stream offset. It is called once per
	// shard, with that shard's lock not yet taken, and its value becomes the
	// shard's anchor.
	Offset func() uint64
	// BatchElements caps how many collection elements go into one rebuild
	// command, bounding the size of a single record. Zero means the default.
	BatchElements int
}

// defaultBatchElements keeps a rebuild command well inside the record size
// limit while staying large enough that a big collection does not turn into
// thousands of tiny records.
const defaultBatchElements = 512

// WriteSnapshot serializes a whole keyspace into sink.
//
// Shards are written in ascending order within a database, and databases in
// ascending order, because the recovery filter requires the anchors to be
// non-decreasing (ADR-009 and docs/design-notes.md issue 1). The order is
// not an implementation detail that happens to hold; it is the invariant the
// whole scheme rests on, and persist.SnapshotWriter.Anchor rejects a
// violation rather than trusting this loop.
//
// The caller must hold the snapshot guard for the whole pass, which
// Keyspace.SnapshotWindow does, so that no cross-shard read-modify-write
// effect lands inside the window.
func WriteSnapshot(ks *engine.Keyspace, sink SnapshotSink, opts SnapshotOptions) error {
	if opts.Offset == nil {
		return fmt.Errorf("command: WriteSnapshot needs an Offset function")
	}
	batch := opts.BatchElements
	if batch <= 0 {
		batch = defaultBatchElements
	}

	for i := 0; i < ks.NumDatabases(); i++ {
		db := ks.DB(i)
		for shard := 0; shard < db.NumShards(); shard++ {
			// The offset is read before the shard lock is taken, so it can
			// only be older than the contents that follow. An anchor that
			// were newer than its shard would tell recovery to skip a
			// record the shard does not have.
			if err := sink.Anchor(i, shard, opts.Offset()); err != nil {
				return err
			}
			w := shardWriter{sink: sink, db: i, batch: batch}
			if err := db.SnapshotShard(shard, w.entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// shardWriter turns snapshot entries into rebuild commands.
type shardWriter struct {
	sink  SnapshotSink
	db    int
	batch int
	args  [][]byte // reused across records
}

var (
	snapSet     = []byte("SET")
	snapRPush   = []byte("RPUSH")
	snapHSet    = []byte("HSET")
	snapSAdd    = []byte("SADD")
	snapZAdd    = []byte("ZADD")
	snapPExpire = []byte("PEXPIREAT")
)

func (w *shardWriter) entry(e *engine.SnapshotEntry) error {
	switch e.Type {
	case engine.TypeString:
		if err := w.emit(snapSet, e.Key, e.Value); err != nil {
			return err
		}
	case engine.TypeList:
		if err := w.chunked(snapRPush, e.Key, e.Elements, 1); err != nil {
			return err
		}
	case engine.TypeSet:
		if err := w.chunked(snapSAdd, e.Key, e.Elements, 1); err != nil {
			return err
		}
	case engine.TypeHash:
		if err := w.chunked(snapHSet, e.Key, e.Elements, 2); err != nil {
			return err
		}
	case engine.TypeZSet:
		if err := w.chunked(snapZAdd, e.Key, e.Elements, 2); err != nil {
			return err
		}
	default:
		return fmt.Errorf("command: cannot snapshot key %q of type %v", e.Key, e.Type)
	}

	if e.ExpireAt > 0 {
		// The expiry is absolute, so it means the same instant whenever the
		// snapshot is loaded. Nothing else would survive a restart.
		return w.emit(snapPExpire, e.Key, itob(e.ExpireAt))
	}
	return nil
}

// chunked writes a collection as one or more commands, never splitting a
// field/value or score/member pair across two of them.
//
// An empty collection cannot occur: the engine deletes a key whose
// collection becomes empty, so there is nothing to represent.
func (w *shardWriter) chunked(verb, key []byte, elems [][]byte, stride int) error {
	if len(elems) == 0 {
		return fmt.Errorf("command: key %q holds an empty collection, which the "+
			"engine should have deleted", key)
	}
	per := w.batch - w.batch%stride
	if per < stride {
		per = stride
	}
	for start := 0; start < len(elems); start += per {
		end := start + per
		if end > len(elems) {
			end = len(elems)
		}
		w.args = append(w.args[:0], verb, key)
		w.args = append(w.args, elems[start:end]...)
		if err := w.sink.Record(w.db, w.args); err != nil {
			return err
		}
	}
	return nil
}

func (w *shardWriter) emit(parts ...[]byte) error {
	w.args = append(w.args[:0], parts...)
	return w.sink.Record(w.db, w.args)
}
