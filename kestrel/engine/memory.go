package engine

// Memory accounting is an estimate maintained incrementally (ADR-013).
//
// Exact accounting is not available in Go: the runtime owns the heap and
// runtime.ReadMemStats is both approximate and far too expensive to call per
// command. Instead each write adjusts a per-shard counter by the estimated
// cost of the entry. The estimate is expected to land within roughly 15% of
// the real resident cost, which is why the documentation tells operators to
// set maxmemory to 60-70% of the container limit rather than 90%.
const (
	// mapEntryOverhead approximates what one Go map entry costs in bucket
	// storage, including the amortized cost of empty slots. The promoted
	// hash, set and sorted-set encodings are Go maps, so this stays an
	// estimate: the runtime does not publish what a bucket costs.
	//
	// The keyspace itself is no longer one. Its table reports its own size
	// exactly (dict.overhead), so it is counted once per shard in
	// MemoryEstimate rather than amortised over the keys -- which is both
	// accurate and, unlike a per-key constant, correct while the table is
	// half empty or midway through a rehash.
	mapEntryOverhead = 48
	// objectOverhead is the allocator size class for an Object header.
	objectOverhead = 48
	// sliceHeaderSize is the cost of a []byte header.
	sliceHeaderSize = 24
	// expiresEntryOverhead is the extra cost of indexing a key as TTL'd.
	expiresEntryOverhead = 32
)

// objectSize estimates the bytes attributable to one key/value pair.
//
// It covers the key's own bytes, the object header and the value. The hash
// table slot the key occupies is not included: the table knows its own size
// and is counted whole, per shard.
func objectSize(key string, o *Object) int64 {
	n := int64(len(key) + objectOverhead)
	if o.ExpireAt > 0 {
		n += int64(expiresEntryOverhead + len(key))
	}
	n += valueSize(o)
	return n
}

// valueSize estimates the bytes held by an object's payload.
func valueSize(o *Object) int64 {
	switch v := o.Value.(type) {
	case int64:
		return 8
	case []byte:
		return int64(sliceHeaderSize + cap(v))
	case nil:
		return 0
	default:
		// Collection types add their own estimators as they land in M2.
		if s, ok := o.Value.(interface{ EstimatedSize() int64 }); ok {
			return s.EstimatedSize()
		}
		return 0
	}
}

// MemoryUsage estimates the bytes used by a single key, for MEMORY USAGE.
// It returns false if the key does not exist.
func (db *DB) MemoryUsage(key []byte) (int64, bool) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o := db.peek(s, key)
	if o == nil {
		return 0, false
	}
	return objectSize(string(key), o), true
}
