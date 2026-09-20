package engine

import "fmt"

// Snapshot support (ADR-009).
//
// A snapshot is taken one shard at a time. Each shard is serialized under a
// single hold of its lock, so its contents correspond to exactly one instant
// of the log, and that instant is recorded as the shard's anchor offset.
// Different shards in the same snapshot are at different instants, which is
// what makes the snapshot fuzzy and what the per-shard recovery filter
// exists to reconcile.
//
// The lock cannot be released part-way through a shard. If it were, a write
// arriving between two batches would be visible in the second batch but not
// the first, while the anchor names a single offset for both -- and recovery
// would then replay that write on top of a value that already contains it.
// For INCR or RPUSH that is a wrong value, not a redundant one.

// SnapshotEntry is one key's contents, captured under its shard lock.
//
// The engine fills it rather than handing out an *Object, because an
// *Object is only stable while the lock is held and its fields are updated
// in place. Everything here is either copied or, for byte slices, covered by
// the package's immutability rule.
//
// The entry and every slice in it are reused between calls. A consumer that
// needs to retain anything must copy it.
type SnapshotEntry struct {
	// Key is the key name.
	Key []byte
	// Type is the logical value type.
	Type ObjectType
	// ExpireAt is the absolute expiry in milliseconds, or 0 for none.
	ExpireAt int64
	// Value holds the payload of a string.
	Value []byte
	// Elements holds a collection's contents, flattened: list elements in
	// order, set members, hash field and value alternating, or sorted set
	// score and member alternating.
	Elements [][]byte
}

// NumShards is the number of shards each database is split into.
func (db *DB) NumShards() int { return len(db.shards) }

// SnapshotShard calls fn once for every key in one shard, with the shard
// locked for the duration.
//
// anchorAt is called once, with the shard's propagation lock held and before
// its data lock is taken, and its result is returned as the shard's anchor.
// The caller passes the log's current offset.
//
// fn must not call back into the engine, and must not block: it holds up
// every command touching this shard. Serializing into memory is what it is
// for; writing to a file is not.
//
// Logically expired keys are included. The leader will propagate a DEL for
// each of them at an offset after this shard's anchor, and recovery replays
// it, so including them costs a little space and keeps the shard faithful to
// the instant it was taken at. Excluding them would need the clock, which
// would make the snapshot depend on when it was loaded.
func (db *DB) SnapshotShard(shard int, anchorAt func() uint64, fn func(*SnapshotEntry) error) (uint64, error) {
	if shard < 0 || shard >= len(db.shards) {
		return 0, fmt.Errorf("engine: no shard %d", shard)
	}
	s := db.shards[shard]
	db.ks.barrier.RLock()
	defer db.ks.barrier.RUnlock()

	// The anchor is read while the shard's propagation lock is held, which
	// is what makes it exact. Holding prop means no write to this shard is
	// between having mutated it and having reached the log, so every effect
	// already in the shard has an offset below the one read here, and every
	// effect above it is not in the shard yet.
	//
	// Taking mu before releasing prop keeps that true for the walk: a write
	// admitted after prop is released blocks on mu until the shard has been
	// serialized, and its record lands after the anchor.
	s.prop.Lock()
	anchor := anchorAt()
	s.mu.Lock()
	s.prop.Unlock()
	defer s.mu.Unlock()

	var e SnapshotEntry
	var walkErr error
	s.dict.forEach(func(k string, o *Object) bool {
		e.Key = append(e.Key[:0], k...)
		e.Type = o.Type
		e.ExpireAt = o.ExpireAt
		e.Value = nil
		e.Elements = e.Elements[:0]

		switch o.Type {
		case TypeString:
			// stringBytes renders an integer-encoded value rather than
			// asserting []byte, which is what an INCR leaves behind.
			e.Value = o.stringBytes()
		case TypeList:
			o.Value.(*List).Each(func(_ int, el []byte) bool {
				e.Elements = append(e.Elements, el)
				return true
			})
		case TypeHash:
			e.Elements = append(e.Elements, o.Value.(*Hash).All()...)
		case TypeSet:
			e.Elements = append(e.Elements, o.Value.(*Set).Members()...)
		case TypeZSet:
			o.Value.(*ZSet).Each(func(m ZMember) bool {
				e.Elements = append(e.Elements, FormatFloat(m.Score), m.Member)
				return true
			})
		default:
			walkErr = fmt.Errorf("engine: key %q has unknown type %d", k, o.Type)
			return false
		}
		if err := fn(&e); err != nil {
			walkErr = err
			return false
		}
		return true
	})
	return anchor, walkErr
}

// ShardIndexOf reports which shard a key belongs to. Recovery uses it to
// decide whether a log record predates the shard's anchor.
func (db *DB) ShardIndexOf(key []byte) int { return int(db.shardIndex(key)) }
