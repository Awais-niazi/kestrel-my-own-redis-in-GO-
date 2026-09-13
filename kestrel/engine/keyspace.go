package engine

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
)

// Errors returned by engine operations. The command layer maps these onto
// wire error replies; the engine itself knows nothing about RESP.
var (
	// ErrWrongType is returned when an operation is applied to a key holding
	// a value of the wrong type.
	ErrWrongType = errors.New("wrong kind of value")
	// ErrNotInteger is returned when a value cannot be parsed as an integer.
	ErrNotInteger = errors.New("value is not an integer or out of range")
	// ErrNotFloat is returned when a value cannot be parsed as a float.
	ErrNotFloat = errors.New("value is not a valid float")
	// ErrOverflow is returned when an increment would overflow.
	ErrOverflow = errors.New("increment or decrement would overflow")
	// ErrOutOfRange is returned for offsets outside the allowed range.
	ErrOutOfRange = errors.New("offset is out of range")
	// ErrNoSuchKey is returned by operations that require an existing key.
	ErrNoSuchKey = errors.New("no such key")
	// ErrValueTooLarge is returned when a write would exceed the proto limit.
	ErrValueTooLarge = errors.New("string exceeds maximum allowed size")
)

// Clock returns the current wall-clock time in milliseconds since the Unix
// epoch. Absolute expiry uses wall time and tolerates clock jumps (NFR-6).
type Clock func() int64

func systemClock() int64 { return time.Now().UnixMilli() }

// EffectSink receives canonical, replay-safe writes produced by the engine
// itself, which today means only the DEL generated when an expired key is
// reaped (FR-3.4). Command-initiated effects are produced by the command
// layer, which knows the original arguments.
//
// Implementations are called with a shard lock held and must not call back
// into the engine.
type EffectSink interface {
	Effect(db int, args ...[]byte)
}

// KeyWatcher is notified when a key's value changes, so that WATCH can be
// invalidated and keyspace notifications can fire.
//
// Implementations are called with a shard lock held and must not call back
// into the engine.
type KeyWatcher interface {
	KeyModified(db int, key []byte)
}

// Options configures a Keyspace.
type Options struct {
	// Databases is the number of logical databases addressable by SELECT.
	Databases int
	// Shards is the number of partitions per database. Must be a power of
	// two. Fixed for the life of the process (ADR-003).
	Shards int
	// MaxStringLength caps a single string value.
	MaxStringLength int
	// ActiveExpire enables the background expiry cycle.
	ActiveExpire bool
	// ActiveExpireSampleSize is how many TTL'd keys are sampled per shard
	// per pass.
	ActiveExpireSampleSize int
	// ActiveExpireCPUPercent bounds the expiry cycle's duty cycle.
	ActiveExpireCPUPercent int
	// CachedClock replaces the per-call time.Now with an atomic read of a
	// value refreshed every millisecond by a background goroutine.
	//
	// Every expiry check on the read path reads the clock, and a clock read
	// is not uniformly cheap: on a host without a vDSO fast path it costs
	// more than the rest of a GET put together. The price is that expiry
	// resolves to the nearest millisecond, which is the same resolution the
	// reference implementation offers for the same reason.
	CachedClock bool
}

// DefaultOptions matches Appendix B of the PRD.
func DefaultOptions() Options {
	return Options{
		Databases:              16,
		Shards:                 16,
		MaxStringLength:        512 * 1024 * 1024,
		ActiveExpire:           true,
		ActiveExpireSampleSize: 20,
		ActiveExpireCPUPercent: 25,
	}
}

func (o *Options) normalize() {
	if o.Databases <= 0 {
		o.Databases = 16
	}
	if o.Shards <= 0 {
		o.Shards = 1
	}
	// Round up to a power of two so the shard index is a mask, not a modulo.
	n := 1
	for n < o.Shards {
		n <<= 1
	}
	o.Shards = n
	if o.MaxStringLength <= 0 {
		o.MaxStringLength = 512 * 1024 * 1024
	}
	if o.ActiveExpireSampleSize <= 0 {
		o.ActiveExpireSampleSize = 20
	}
	if o.ActiveExpireCPUPercent <= 0 || o.ActiveExpireCPUPercent > 100 {
		o.ActiveExpireCPUPercent = 25
	}
}

// Stats is a point-in-time reading of the engine counters exported through
// INFO and /metrics.
//
// The counters themselves live on the shards and are updated under the shard
// lock rather than atomically on a shared word. Sharing one cache line
// across every core would put a contention point on the hot path, which is
// precisely what the sharded design exists to avoid (ADR-003).
type Stats struct {
	Hits        int64
	Misses      int64
	ExpiredKeys int64
	EvictedKeys int64
	Changes     int64 // writes since the last snapshot
}

// Keyspace owns every database and the barrier lock above the shards.
type Keyspace struct {
	// barrier serializes cross-cutting operations against normal command
	// execution. Normal commands hold the read side for their duration;
	// FLUSHALL, SWAPDB and snapshot start hold the write side (ADR-003).
	barrier sync.RWMutex

	opts  Options
	dbs   []*DB
	clock atomic.Pointer[Clock]

	sink    atomic.Pointer[EffectSink]
	watcher atomic.Pointer[KeyWatcher]
	replica atomic.Bool

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// New returns a started Keyspace. Call Close to stop its background workers.
func New(opts Options) *Keyspace {
	opts.normalize()
	ks := &Keyspace{opts: opts, stop: make(chan struct{})}
	var c Clock = systemClock
	ks.clock.Store(&c)
	ks.dbs = make([]*DB, opts.Databases)
	for i := range ks.dbs {
		ks.dbs[i] = newDB(ks, i, opts.Shards)
	}
	if opts.CachedClock {
		ks.startCachedClock()
	}
	if opts.ActiveExpire {
		ks.wg.Add(1)
		go ks.activeExpireCycle()
	}
	return ks
}

// Close stops background workers. It does not flush data.
func (ks *Keyspace) Close() {
	ks.stopOnce.Do(func() { close(ks.stop) })
	ks.wg.Wait()
}

// Options returns the effective configuration.
func (ks *Keyspace) Options() Options { return ks.opts }

// NumDatabases reports how many databases SELECT can address.
func (ks *Keyspace) NumDatabases() int { return len(ks.dbs) }

// DB returns database i, or nil if i is out of range.
func (ks *Keyspace) DB(i int) *DB {
	if i < 0 || i >= len(ks.dbs) {
		return nil
	}
	return ks.dbs[i]
}

// startCachedClock refreshes a cached millisecond timestamp in the
// background and points the keyspace at it.
func (ks *Keyspace) startCachedClock() {
	var cached atomic.Int64
	cached.Store(systemClock())
	ks.SetClock(func() int64 { return cached.Load() })

	ks.wg.Add(1)
	go func() {
		defer ks.wg.Done()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ks.stop:
				return
			case <-ticker.C:
				cached.Store(systemClock())
			}
		}
	}()
}

// SetClock replaces the wall clock. Intended for tests and for a cached
// clock supplied by the server's cron.
func (ks *Keyspace) SetClock(c Clock) { ks.clock.Store(&c) }

// Now returns the current time in milliseconds since the epoch.
func (ks *Keyspace) Now() int64 { return (*ks.clock.Load())() }

// SetEffectSink installs the destination for engine-generated effects.
func (ks *Keyspace) SetEffectSink(s EffectSink) { ks.sink.Store(&s) }

// SetKeyWatcher installs the key-modification hook.
func (ks *Keyspace) SetKeyWatcher(w KeyWatcher) { ks.watcher.Store(&w) }

// SetReplica switches expiry behaviour. On a replica, expired keys are
// hidden from reads but not deleted: the leader's DEL is authoritative, so
// replica and leader cannot diverge on their own clocks (FR-3.4).
func (ks *Keyspace) SetReplica(v bool) { ks.replica.Store(v) }

// IsReplica reports whether the keyspace is in replica mode.
func (ks *Keyspace) IsReplica() bool { return ks.replica.Load() }

func (ks *Keyspace) emit(db int, args ...[]byte) {
	if p := ks.sink.Load(); p != nil {
		(*p).Effect(db, args...)
	}
}

func (ks *Keyspace) signal(db int, key []byte) {
	if p := ks.watcher.Load(); p != nil {
		(*p).KeyModified(db, key)
	}
}

// Barrier runs fn while holding the exclusive cross-shard lock. Snapshot
// start, SWAPDB and FLUSHALL use it.
func (ks *Keyspace) Barrier(fn func()) {
	ks.barrier.Lock()
	defer ks.barrier.Unlock()
	fn()
}

// SwapDB exchanges the contents of two databases.
func (ks *Keyspace) SwapDB(i, j int) error {
	if ks.DB(i) == nil || ks.DB(j) == nil {
		return errors.New("DB index is out of range")
	}
	if i == j {
		return nil
	}
	ks.barrier.Lock()
	defer ks.barrier.Unlock()
	ks.dbs[i].shards, ks.dbs[j].shards = ks.dbs[j].shards, ks.dbs[i].shards
	return nil
}

// FlushAll empties every database.
func (ks *Keyspace) FlushAll() {
	ks.barrier.Lock()
	defer ks.barrier.Unlock()
	for _, db := range ks.dbs {
		db.flushLocked()
	}
}

// Stats aggregates the per-shard counters.
func (ks *Keyspace) Stats() Stats {
	var out Stats
	ks.barrier.RLock()
	for _, db := range ks.dbs {
		for _, s := range db.shards {
			s.mu.Lock()
			out.Hits += s.hits
			out.Misses += s.misses
			out.ExpiredKeys += s.expiredKeys
			out.EvictedKeys += s.evictedKeys
			out.Changes += s.changes
			s.mu.Unlock()
		}
	}
	ks.barrier.RUnlock()
	return out
}

// ResetStats zeroes the engine counters.
func (ks *Keyspace) ResetStats() {
	ks.barrier.RLock()
	for _, db := range ks.dbs {
		for _, s := range db.shards {
			s.mu.Lock()
			s.hits, s.misses, s.expiredKeys, s.evictedKeys = 0, 0, 0, 0
			s.mu.Unlock()
		}
	}
	ks.barrier.RUnlock()
}

// TotalKeys counts live keys across all databases.
func (ks *Keyspace) TotalKeys() int64 {
	var n int64
	for _, db := range ks.dbs {
		n += db.Size()
	}
	return n
}

// MemoryEstimate reports the estimated logical dataset size in bytes.
//
// It is an estimate maintained incrementally on every write, not a
// measurement of the Go heap (ADR-013).
func (ks *Keyspace) MemoryEstimate() int64 {
	var n int64
	for _, db := range ks.dbs {
		n += db.MemoryEstimate()
	}
	return n
}

// shard is one partition of a database.
type shard struct {
	mu   sync.Mutex
	dict map[string]*Object
	// expires indexes the keys carrying a TTL. It exists so the active
	// expiry cycle has a small population to sample rather than the whole
	// dictionary; the authoritative timestamp lives on the Object.
	expires map[string]struct{}
	memory  int64 // estimated bytes held by this shard

	// Counters, all mutated under mu.
	hits        int64
	misses      int64
	expiredKeys int64
	evictedKeys int64
	changes     int64
}

// DB is one logical database.
type DB struct {
	ks     *Keyspace
	Index  int
	shards []*shard
	mask   uint64
}

func newDB(ks *Keyspace, index, n int) *DB {
	db := &DB{ks: ks, Index: index, shards: make([]*shard, n), mask: uint64(n - 1)}
	for i := range db.shards {
		db.shards[i] = &shard{
			dict:    make(map[string]*Object),
			expires: make(map[string]struct{}),
		}
	}
	return db
}

// Keyspace returns the owning keyspace.
func (db *DB) Keyspace() *Keyspace { return db.ks }

func (db *DB) shardIndex(key []byte) uint64 { return xxhash.Sum64(key) & db.mask }

func (db *DB) shardFor(key []byte) *shard { return db.shards[db.shardIndex(key)] }

// lockKey takes the barrier read lock and the lock of key's shard.
func (db *DB) lockKey(key []byte) *shard {
	db.ks.barrier.RLock()
	s := db.shardFor(key)
	s.mu.Lock()
	return s
}

func (db *DB) unlockKey(s *shard) {
	s.mu.Unlock()
	db.ks.barrier.RUnlock()
}

// lockKeys takes the barrier read lock and the locks of every shard covering
// keys, in ascending shard order. Ordering is what makes multi-key commands
// deadlock-free (§6.4).
func (db *DB) lockKeys(keys [][]byte) []*shard {
	var stack [16]int
	idx := stack[:0]
	for _, k := range keys {
		idx = append(idx, int(db.shardIndex(k)))
	}
	sort.Ints(idx)
	// Compact duplicates in place.
	out := idx[:0]
	prev := -1
	for _, i := range idx {
		if i != prev {
			out = append(out, i)
			prev = i
		}
	}
	db.ks.barrier.RLock()
	locked := make([]*shard, 0, len(out))
	for _, i := range out {
		db.shards[i].mu.Lock()
		locked = append(locked, db.shards[i])
	}
	return locked
}

func (db *DB) unlockShards(locked []*shard) {
	for i := len(locked) - 1; i >= 0; i-- {
		locked[i].mu.Unlock()
	}
	db.ks.barrier.RUnlock()
}

// lockAll takes every shard lock in ascending order. Used by commands that
// need a consistent view of the whole database but do not need to exclude
// the other databases.
func (db *DB) lockAll() {
	db.ks.barrier.RLock()
	for _, s := range db.shards {
		s.mu.Lock()
	}
}

func (db *DB) unlockAll() {
	for i := len(db.shards) - 1; i >= 0; i-- {
		db.shards[i].mu.Unlock()
	}
	db.ks.barrier.RUnlock()
}

// lookup returns the live object for key, reaping it first if it has expired.
// The shard must be locked.
func (db *DB) lookup(s *shard, key []byte) *Object {
	o := s.dict[string(key)] // the compiler elides the string allocation here
	if o == nil {
		return nil
	}
	if o.ExpireAt > 0 && o.ExpireAt <= db.ks.Now() {
		if db.ks.IsReplica() {
			// Hide it, but let the leader's DEL do the deleting (FR-3.4).
			return nil
		}
		db.expireKey(s, string(key), o)
		return nil
	}
	return o
}

// lookupRead is lookup plus hit/miss accounting.
func (db *DB) lookupRead(s *shard, key []byte) *Object {
	o := db.lookup(s, key)
	if o == nil {
		s.misses++
	} else {
		s.hits++
	}
	return o
}

// lookupType is lookupRead with a type assertion.
func (db *DB) lookupType(s *shard, key []byte, t ObjectType) (*Object, error) {
	o := db.lookupRead(s, key)
	if o == nil {
		return nil, nil
	}
	if o.Type != t {
		return nil, ErrWrongType
	}
	return o, nil
}

// expireKey deletes an expired key and propagates the deletion. The shard
// must be locked.
func (db *DB) expireKey(s *shard, key string, o *Object) {
	db.removeLocked(s, key, o)
	s.expiredKeys++
	db.ks.emit(db.Index, delCommand, []byte(key))
	db.ks.signal(db.Index, []byte(key))
}

var delCommand = []byte("DEL")

// store installs o under key. The shard must be locked. key is copied.
func (db *DB) store(s *shard, key []byte, o *Object) {
	k := string(key)
	if old, ok := s.dict[k]; ok {
		s.memory -= objectSize(k, old)
		if old.ExpireAt > 0 && o.ExpireAt == 0 {
			delete(s.expires, k)
		}
	}
	s.dict[k] = o
	if o.ExpireAt > 0 {
		s.expires[k] = struct{}{}
	}
	s.memory += objectSize(k, o)
	s.changes++
}

// removeLocked deletes key from the shard. The shard must be locked.
func (db *DB) removeLocked(s *shard, key string, o *Object) {
	if o == nil {
		var ok bool
		if o, ok = s.dict[key]; !ok {
			return
		}
	}
	delete(s.dict, key)
	delete(s.expires, key)
	s.memory -= objectSize(key, o)
	s.changes++
}

// setExpireLocked sets or clears a key's TTL and keeps the index in sync.
func (db *DB) setExpireLocked(s *shard, key string, o *Object, at int64) {
	o.ExpireAt = at
	if at > 0 {
		s.expires[key] = struct{}{}
	} else {
		delete(s.expires, key)
	}
	s.changes++
}

// touched records a modification for WATCH and keyspace notifications.
func (db *DB) touched(key []byte) { db.ks.signal(db.Index, key) }

// Size returns the number of live keys, excluding keys that are expired but
// not yet reaped.
func (db *DB) Size() int64 {
	db.lockAll()
	defer db.unlockAll()
	now := db.ks.Now()
	var n int64
	for _, s := range db.shards {
		n += int64(len(s.dict))
		for k := range s.expires {
			if o := s.dict[k]; o != nil && o.ExpireAt > 0 && o.ExpireAt <= now {
				n--
			}
		}
	}
	return n
}

// MemoryEstimate reports this database's estimated size in bytes.
func (db *DB) MemoryEstimate() int64 {
	var n int64
	db.ks.barrier.RLock()
	for _, s := range db.shards {
		s.mu.Lock()
		n += s.memory
		s.mu.Unlock()
	}
	db.ks.barrier.RUnlock()
	return n
}

// Flush empties the database.
func (db *DB) Flush() {
	db.ks.barrier.Lock()
	defer db.ks.barrier.Unlock()
	db.flushLocked()
}

// flushLocked requires the barrier write lock.
func (db *DB) flushLocked() {
	for _, s := range db.shards {
		s.dict = make(map[string]*Object)
		s.expires = make(map[string]struct{})
		s.memory = 0
		s.changes++
	}
}
