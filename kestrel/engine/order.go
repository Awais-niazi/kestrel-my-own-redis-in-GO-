package engine

// Write ordering.
//
// The log must record effects in the order they were applied, per key. If it
// does not, replaying it produces a different dataset than the one it was
// written from -- and the difference is invisible until someone restarts.
//
// That ordering is not free. A command's mutation happens under a shard's mu
// and its effect reaches the log afterwards, once mu has been released, so
// two commands writing the same key can mutate in one order and be logged in
// the other:
//
//	goroutine A: SET k 1   mutates (mu held, then released)
//	goroutine B: SET k 2   mutates (mu held, then released)
//	goroutine B: appends "SET k 2"
//	goroutine A: appends "SET k 1"
//
// The keyspace holds 2 and the log says 1. Nothing detects it until a
// restart, and then the value is silently wrong.
//
// shard.prop closes the gap. The command layer takes it for every shard a
// write touches, before the handler runs, and releases it after the effect
// has been propagated. Two writes to the same shard therefore enter the log
// in the order they were applied. Writes to different shards still run
// concurrently, and reads are not affected at all.
//
// The lock is also what makes a snapshot anchor exact. Holding prop for a
// shard means no write to it is between "mutated" and "logged", so the log
// offset read at that moment is precisely the point the shard's contents
// correspond to.

// WriteOrder holds the propagation-order locks a write needs. The zero value
// holds nothing and may be released.
type WriteOrder struct {
	shards []*shard
}

// Done releases the locks.
func (w WriteOrder) Done() {
	for i := len(w.shards) - 1; i >= 0; i-- {
		w.shards[i].prop.Unlock()
	}
}

// OrderWrites locks the propagation order for the shards holding keys.
//
// Locks are taken in ascending shard index, the same discipline the engine's
// own multi-shard operations follow (ADR-003), so two writes touching the
// same pair of shards cannot deadlock against each other.
//
// The returned WriteOrder must be released with Done once the effect has
// been propagated, not merely once the command has run.
func (db *DB) OrderWrites(keys [][]byte) WriteOrder {
	if len(keys) == 0 {
		return WriteOrder{}
	}
	if len(keys) == 1 {
		s := db.shardFor(keys[0])
		s.prop.Lock()
		return WriteOrder{shards: []*shard{s}}
	}

	idx := make([]int, 0, len(keys))
	for _, k := range keys {
		i := int(db.shardIndex(k))
		if !containsInt(idx, i) {
			idx = append(idx, i)
		}
	}
	sortInts(idx)

	out := make([]*shard, 0, len(idx))
	for _, i := range idx {
		s := db.shards[i]
		s.prop.Lock()
		out = append(out, s)
	}
	return WriteOrder{shards: out}
}

// OrderAllWrites locks the propagation order for every shard of every
// database.
//
// It is what a write uses when its key set does not describe the shards it
// touches: FLUSHALL and SWAPDB touch everything, and the cross-shard
// commands can write a key in a database their arguments do not name. Those
// are all rare and already blocked for the duration of a snapshot, so the
// blunt answer is the right one -- a wrong lock set here is a silently
// misordered log, which is the failure this whole file exists to prevent.
func (ks *Keyspace) OrderAllWrites() WriteOrder {
	out := make([]*shard, 0, len(ks.dbs)*len(ks.dbs[0].shards))
	for _, db := range ks.dbs {
		for _, s := range db.shards {
			s.prop.Lock()
			out = append(out, s)
		}
	}
	return WriteOrder{shards: out}
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// sortInts sorts a handful of shard indexes. Insertion sort is used because
// the slice is as long as a command's distinct shard count, which is
// typically one or two, and never allocates.
func sortInts(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
