package engine

import "time"

// Expiration has two mechanisms (ADR-007): lazy, where any access to a key
// checks its expiry first (see DB.peek and DB.lookup), and active, where a
// bounded background cycle samples the TTL index and reaps what it finds.
// That cycle is backgroundCycle, which also finishes table rehashes.

// expired reports whether o is past due.
func (ks *Keyspace) expired(o *Object) bool {
	return o.ExpireAt != 0 && ks.expiredAt(o, ks.Now())
}

// expiredAt reports whether o is past due at now, which loops hoist out of
// the iteration.
//
// This is the only place the question is answered. It was eight separate
// conditions before loading mode existed, and eight places to forget it is
// eight ways for a recovered keyspace to differ from the log that produced
// it. The clock comparison short-circuits first, so a key with no TTL never
// reaches the atomic load.
func (ks *Keyspace) expiredAt(o *Object, now int64) bool {
	return o.ExpireAt != 0 && o.ExpireAt <= now && !ks.loading.Load()
}

// ExpireFlags constrains when a TTL update applies, matching the NX/XX/GT/LT
// options of EXPIRE.
type ExpireFlags uint8

// TTL update conditions.
const (
	ExpireNX ExpireFlags = 1 << iota // set only when no TTL exists
	ExpireXX                         // set only when a TTL exists
	ExpireGT                         // set only when strictly greater
	ExpireLT                         // set only when strictly less
)

// Expire sets key's absolute expiry to atMS, subject to flags.
//
// An expiry at or before the present deletes the key, which is what the
// reference implementation does and what replicas expect to see. The second
// return value reports whether the key was deleted rather than re-dated, so
// the command layer can propagate DEL instead of PEXPIREAT.
func (db *DB) Expire(key []byte, atMS int64, flags ExpireFlags) (applied, deleted bool) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o := db.lookup(s, key)
	if o == nil {
		return false, false
	}
	if !expireConditionMet(o.ExpireAt, atMS, flags) {
		return false, false
	}
	k := string(key)
	// A TTL at or before the present deletes the key -- except while a log
	// is being replayed, where every timestamp in it is in the past by
	// definition. The leader propagated DEL rather than PEXPIREAT for the
	// keys it actually expired, so the log already says which ones go.
	if atMS <= db.ks.Now() && !db.ks.IsLoading() {
		db.removeLocked(s, k, o)
		db.touched(key)
		return true, true
	}
	db.setExpireLocked(s, k, o, atMS)
	db.touched(key)
	return true, false
}

func expireConditionMet(current, next int64, flags ExpireFlags) bool {
	if flags&ExpireNX != 0 {
		return current == 0
	}
	if flags&ExpireXX != 0 && current == 0 {
		return false
	}
	// A key with no TTL is treated as having an infinite one, so GT can
	// never raise it and LT always lowers it.
	if flags&ExpireGT != 0 && (current == 0 || next <= current) {
		return false
	}
	if flags&ExpireLT != 0 && current != 0 && next >= current {
		return false
	}
	return true
}

// Persist removes key's TTL, reporting whether there was one.
func (db *DB) Persist(key []byte) bool {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o := db.lookup(s, key)
	if o == nil || o.ExpireAt == 0 {
		return false
	}
	db.setExpireLocked(s, string(key), o, 0)
	db.touched(key)
	return true
}

// TTLResult distinguishes the three answers TTL can give.
type TTLResult int

// TTL sentinel values, matching the wire protocol.
const (
	TTLNoKey    TTLResult = -2
	TTLNoExpiry TTLResult = -1
)

// PTTL returns the remaining time to live in milliseconds, or TTLNoKey /
// TTLNoExpiry.
func (db *DB) PTTL(key []byte) int64 {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o := db.peek(s, key)
	if o == nil {
		return int64(TTLNoKey)
	}
	if o.ExpireAt == 0 {
		return int64(TTLNoExpiry)
	}
	if d := o.ExpireAt - db.ks.Now(); d > 0 {
		return d
	}
	return 0
}

// ExpireTime returns the absolute expiry in milliseconds, or TTLNoKey /
// TTLNoExpiry.
func (db *DB) ExpireTime(key []byte) int64 {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o := db.peek(s, key)
	if o == nil {
		return int64(TTLNoKey)
	}
	if o.ExpireAt == 0 {
		return int64(TTLNoExpiry)
	}
	return o.ExpireAt
}

// backgroundCycle is the keyspace's single housekeeping goroutine.
//
// It runs on a fixed window and spends at most ActiveExpireCPUPercent of
// that window working, so it can never starve command execution. Timing uses
// the monotonic clock (NFR-6); only the expiry comparison itself uses wall
// time.
//
// It does two jobs. The reaper exists because lazily expiring a key that is
// never read again would leak its memory forever; its cost is bounded rather
// than exact, since a hard guarantee would need either a timer per key or a
// global priority queue, both of which trade the leak for a latency cliff.
// The rehasher exists because a table rehash only advances on writes, so a
// burst that fills a cache and then stops serving writes leaves one
// suspended indefinitely -- see RehashPass.
func (ks *Keyspace) backgroundCycle() {
	defer ks.wg.Done()

	const window = 100 * time.Millisecond
	budget := window * time.Duration(ks.opts.ActiveExpireCPUPercent) / 100

	ticker := time.NewTicker(window)
	defer ticker.Stop()
	for {
		select {
		case <-ks.stop:
			return
		case <-ticker.C:
			// Unconditional: a replica and a keyspace being loaded have
			// tables to settle like any other, and neither rehashing nor
			// its absence is observable to a client.
			ks.RehashPass(time.Now().Add(budget))

			// Replicas never expire on their own clock (FR-3.4), a keyspace
			// being loaded from a log expires nothing at all
			// (Keyspace.SetLoading), and the reaper can be switched off.
			if !ks.opts.ActiveExpire || ks.IsReplica() || ks.IsLoading() {
				continue
			}
			ks.ExpirePass(time.Now().Add(budget))
		}
	}
}

// rehashBatchBuckets is how many buckets one pass moves before releasing the
// shard. Small enough that a command waits behind a batch rather than behind
// a table; large enough that the lock is not taken thousands of times for
// nothing.
const rehashBatchBuckets = 128

// RehashPass advances any table rehash that command traffic left unfinished,
// until nothing is left to do or the deadline passes. It returns the number
// of batches it moved, and is exported so tests can drive it deterministically.
//
// A rehash steps on set and delete, which is enough while writes keep
// coming. A load that fills a keyspace and then serves reads from it stops
// stepping partway through, and nothing in the read path will ever finish
// the job: both tables stay allocated, which measured 56% more memory than
// the settled table, and every lookup probes two tables rather than one.
//
// Reads deliberately do not step it. The reference implementation rehashes
// on lookup too, which fixes this at the cost of putting the work on the
// hottest path in the server; doing it here keeps that path as it is and
// bounds the work besides.
func (ks *Keyspace) RehashPass(deadline time.Time) int {
	batches := 0
	for _, db := range ks.dbs {
		for _, s := range db.shards {
			for {
				// The shard is released between batches so a command waits
				// for one batch at most. No propagation lock is needed:
				// moving a key between tables changes nothing a client or
				// the log can observe.
				ks.barrier.RLock()
				s.mu.Lock()
				more := s.dict.rehashSome(rehashBatchBuckets)
				s.mu.Unlock()
				ks.barrier.RUnlock()
				if !more {
					break
				}
				batches++
				if time.Now().After(deadline) {
					return batches
				}
			}
		}
	}
	return batches
}

// ExpirePass reaps expired keys until nothing is left to do or the deadline
// passes. It is exported so tests and DEBUG can drive it deterministically.
//
// It returns the number of keys reaped.
func (ks *Keyspace) ExpirePass(deadline time.Time) int {
	if ks.IsLoading() {
		return 0
	}
	sample := ks.opts.ActiveExpireSampleSize
	total := 0
	for _, db := range ks.dbs {
		for _, s := range db.shards {
			// The propagation lock is held for the whole of this shard's
			// sampling. Reaping a key emits a DEL, and that DEL has to
			// reach the log in the same order relative to concurrent writes
			// as the deletion reached the keyspace -- the active cycle runs
			// on its own goroutine, so without this it is free to interleave
			// with a write that is between mutating a key and logging it.
			// See engine/order.go.
			s.prop.Lock()

			// Re-sample the same shard while the hit rate stays high: a
			// shard where most sampled keys were expired probably has many
			// more (ADR-007).
			for {
				checked, expired := db.expireSample(s, sample)
				total += expired
				if checked == 0 || expired*4 <= checked {
					break
				}
				if time.Now().After(deadline) {
					s.prop.Unlock()
					return total
				}
			}
			s.prop.Unlock()
			if time.Now().After(deadline) {
				return total
			}
		}
	}
	return total
}

// expireSample checks up to n TTL'd keys in one shard.
func (db *DB) expireSample(s *shard, n int) (checked, expired int) {
	db.ks.barrier.RLock()
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		db.ks.barrier.RUnlock()
	}()

	if len(s.expires) == 0 {
		return 0, 0
	}
	now := db.ks.Now()
	// Go randomizes the starting bucket of a map range, which is exactly the
	// sampling property this needs.
	for k := range s.expires {
		o := s.dict.getString(k)
		if o == nil {
			// Index entry for a key deleted by other means; drop it.
			delete(s.expires, k)
			continue
		}
		checked++
		if db.ks.expiredAt(o, now) {
			db.expireKey(s, k, o)
			expired++
		}
		if checked >= n {
			break
		}
	}
	return checked, expired
}

// TTLKeyCount reports how many keys carry a TTL, for INFO keyspace.
func (db *DB) TTLKeyCount() int64 {
	db.lockAll()
	defer db.unlockAll()
	var n int64
	for _, s := range db.shards {
		n += int64(len(s.expires))
	}
	return n
}
