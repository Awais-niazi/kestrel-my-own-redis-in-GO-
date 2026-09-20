package engine

import "math/rand"

// Del removes keys and returns how many existed.
func (db *DB) Del(keys [][]byte) int64 {
	locked := db.lockKeys(keys)
	defer db.unlockShards(locked)

	var n int64
	for _, k := range keys {
		s := db.shardFor(k)
		if o := db.lookup(s, k); o != nil {
			db.removeLocked(s, string(k), o)
			db.touched(k)
			n++
		}
	}
	return n
}

// Exists counts how many of keys exist, counting duplicates separately.
func (db *DB) Exists(keys [][]byte) int64 {
	locked := db.lockKeys(keys)
	defer db.unlockShards(locked)

	var n int64
	for _, k := range keys {
		if db.peekRead(db.shardFor(k), k) != nil {
			n++
		}
	}
	return n
}

// Type returns the logical type of key.
func (db *DB) Type(key []byte) (ObjectType, bool) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o := db.peekRead(s, key)
	if o == nil {
		return 0, false
	}
	return o.Type, true
}

// Encoding returns the physical encoding of key, for OBJECT ENCODING.
func (db *DB) Encoding(key []byte) (Encoding, bool) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o := db.peek(s, key)
	if o == nil {
		return 0, false
	}
	return o.Encoding, true
}

// Rename moves src to dst, replacing dst. It reports ErrNoSuchKey when src
// does not exist. When nx is set and dst exists, it makes no change and
// reports ok=false.
func (db *DB) Rename(src, dst []byte, nx bool) (ok bool, err error) {
	locked := db.lockKeys([][]byte{src, dst})
	defer db.unlockShards(locked)

	ss, ds := db.shardFor(src), db.shardFor(dst)
	o := db.lookup(ss, src)
	if o == nil {
		return false, ErrNoSuchKey
	}
	if string(src) == string(dst) {
		// Renaming a key to itself succeeds and changes nothing.
		return true, nil
	}
	if nx && db.lookup(ds, dst) != nil {
		return false, nil
	}
	db.removeLocked(ss, string(src), o)
	db.store(ds, dst, o)
	db.touched(src)
	db.touched(dst)
	return true, nil
}

// Copy duplicates src to dst, optionally into another database. It reports
// false when dst exists and replace is not set, or when src is missing.
func (db *DB) Copy(src, dst []byte, dest *DB, replace bool) bool {
	if dest == nil {
		dest = db
	}
	if dest == db {
		locked := db.lockKeys([][]byte{src, dst})
		defer db.unlockShards(locked)
		return db.copyLocked(src, dst, db, replace)
	}
	// Cross-database copy: the barrier read lock covers both databases, and
	// the two shard locks are taken in a fixed (source, destination) order.
	db.ks.barrier.RLock()
	defer db.ks.barrier.RUnlock()
	ss, ds := db.shardFor(src), dest.shardFor(dst)
	ss.mu.Lock()
	if ds != ss {
		ds.mu.Lock()
		defer ds.mu.Unlock()
	}
	defer ss.mu.Unlock()
	return db.copyLocked(src, dst, dest, replace)
}

func (db *DB) copyLocked(src, dst []byte, dest *DB, replace bool) bool {
	o := db.lookup(db.shardFor(src), src)
	if o == nil {
		return false
	}
	ds := dest.shardFor(dst)
	if !replace && dest.lookup(ds, dst) != nil {
		return false
	}
	dest.store(ds, dst, cloneObject(o))
	dest.touched(dst)
	return true
}

// cloneObject makes an independent copy. Values are treated as immutable, so
// only the header needs duplicating for scalar types; collection types
// deep-copy themselves as they land in M2.
func cloneObject(o *Object) *Object {
	c := *o
	if b, ok := o.Value.([]byte); ok {
		c.Value = copyBytes(b)
	}
	if d, ok := o.Value.(interface{ Clone() any }); ok {
		c.Value = d.Clone()
	}
	return &c
}

// RandomKey returns a random live key, or nil when the database is empty.
func (db *DB) RandomKey() []byte {
	db.ks.barrier.RLock()
	defer db.ks.barrier.RUnlock()

	// Try a few random shards before falling back to a full sweep, so an
	// empty shard does not make the command look empty.
	start := rand.Intn(len(db.shards))
	for i := 0; i < len(db.shards); i++ {
		s := db.shards[(start+i)%len(db.shards)]
		s.mu.Lock()
		for k := range s.dict {
			o := s.dict[k]
			if db.ks.expired(o) {
				continue
			}
			s.mu.Unlock()
			return []byte(k)
		}
		s.mu.Unlock()
	}
	return nil
}

// Keys returns every live key matching a glob pattern.
//
// It walks the whole keyspace and is documented as O(n) and blocking
// (ADR-003). Shards are visited one at a time, so it holds one shard lock at
// a time rather than the whole database.
func (db *DB) Keys(pattern []byte) [][]byte {
	all := len(pattern) == 1 && pattern[0] == '*'
	out := make([][]byte, 0, 16)
	db.ks.barrier.RLock()
	defer db.ks.barrier.RUnlock()
	now := db.ks.Now()
	for _, s := range db.shards {
		s.mu.Lock()
		for k, o := range s.dict {
			if db.ks.expiredAt(o, now) {
				continue
			}
			if all || MatchPattern(pattern, []byte(k)) {
				out = append(out, []byte(k))
			}
		}
		s.mu.Unlock()
	}
	return out
}

// ScanOptions filters a SCAN.
type ScanOptions struct {
	Match []byte     // glob pattern, nil for everything
	Count int        // lower bound on keys returned per call
	Type  ObjectType // filter by type when TypeFilter is set
	// TypeFilter enables the Type filter. A zero ObjectType is a valid type
	// (string), so the filter needs its own flag.
	TypeFilter bool
}

// Scan iterates the keyspace incrementally.
//
// The cursor is a shard index: each call drains whole shards until it has
// produced at least Count keys, and returns the index of the next shard, or
// 0 when the iteration is complete.
//
// This is a documented deviation from the reference implementation, which
// uses a reverse-binary cursor over its own hash table. Go's built-in map
// exposes no stable iteration order, so that cursor cannot be implemented on
// top of it (ADR-004, Q2). The guarantee this design gives is in one respect
// stronger: because each shard is drained under its own lock, a key present
// for the whole iteration is returned exactly once. The cost is that COUNT
// bounds the reply from below rather than above, so a reply can be as large
// as one shard.
func (db *DB) Scan(cursor uint64, opts ScanOptions) (uint64, [][]byte) {
	if opts.Count <= 0 {
		opts.Count = 10
	}
	out := make([][]byte, 0, opts.Count)
	if cursor >= uint64(len(db.shards)) {
		return 0, out
	}

	db.ks.barrier.RLock()
	defer db.ks.barrier.RUnlock()
	now := db.ks.Now()

	i := int(cursor)
	for ; i < len(db.shards); i++ {
		s := db.shards[i]
		s.mu.Lock()
		for k, o := range s.dict {
			if db.ks.expiredAt(o, now) {
				continue
			}
			if opts.TypeFilter && o.Type != opts.Type {
				continue
			}
			if opts.Match != nil && !MatchPattern(opts.Match, []byte(k)) {
				continue
			}
			out = append(out, []byte(k))
		}
		s.mu.Unlock()
		if len(out) >= opts.Count {
			i++
			break
		}
	}
	if i >= len(db.shards) {
		return 0, out
	}
	return uint64(i), out
}
