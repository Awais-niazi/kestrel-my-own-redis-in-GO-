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
			// The shard captures its own anchor, because only the engine
			// can read the log offset at the instant that makes it exact:
			// with the shard's propagation lock held, so that no write to
			// it is between having been applied and having been logged.
			//
			// That means the anchor is known only once the shard has been
			// walked, so the records are buffered and the anchor is written
			// before them.
			w := shardWriter{db: i, batch: batch}
			anchor, err := db.SnapshotShard(shard, opts.Offset, w.entry)
			if err != nil {
				return err
			}
			if err := sink.Anchor(i, shard, anchor); err != nil {
				return err
			}
			for _, rec := range w.buffered {
				if err := sink.Record(i, rec); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// shardWriter turns snapshot entries into rebuild commands.
//
// Records are buffered rather than written straight through, for two
// reasons. The shard's anchor is not known until the walk has started, and
// it has to precede the records it describes; and the walk runs with the
// shard's data lock held, where a write to a file has no business being.
type shardWriter struct {
	db       int
	batch    int
	buffered [][][]byte
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
		args := make([][]byte, 0, 2+end-start)
		args = append(args, verb, copyOf(key))
		for _, e := range elems[start:end] {
			args = append(args, copyOf(e))
		}
		w.buffered = append(w.buffered, args)
	}
	return nil
}

func (w *shardWriter) emit(parts ...[]byte) error {
	args := make([][]byte, 0, len(parts))
	for _, p := range parts {
		args = append(args, copyOf(p))
	}
	w.buffered = append(w.buffered, args)
	return nil
}

// copyOf detaches a slice from the keyspace.
//
// The walk hands out slices that alias stored values, and an entry buffer
// that is reused between keys; both are valid only while the shard lock is
// held. Buffering the records past that point means copying, which is the
// price of not writing to a file under a lock.
func copyOf(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
