package engine

import (
	"math/rand"
	"sync/atomic"
)

// Eviction (FR-6.3, ADR-013).
//
// When the estimated dataset exceeds maxmemory, keys are removed to get back
// under it. Which keys is decided by sampling rather than by an exact
// ordering: keeping a true LRU list would mean a doubly linked list threaded
// through every object, which costs two pointers per key and a write to
// three cache lines on every read, to answer a question that only has to be
// approximately right.
//
// This is the same trade the reference implementation makes, and for the
// same reason. The sample size is maxmemory-samples; a larger one gets
// closer to true LRU at a cost paid only while evicting.

// EvictionPolicy decides which key to remove when memory is short.
type EvictionPolicy uint8

// Eviction policies, matching maxmemory-policy.
const (
	// EvictNone refuses writes rather than removing anything.
	EvictNone EvictionPolicy = iota
	EvictAllKeysLRU
	EvictAllKeysLFU
	EvictAllKeysRandom
	EvictVolatileLRU
	EvictVolatileLFU
	EvictVolatileRandom
	EvictVolatileTTL
)

// ParseEvictionPolicy converts a maxmemory-policy value.
func ParseEvictionPolicy(s string) (EvictionPolicy, bool) {
	switch s {
	case "noeviction":
		return EvictNone, true
	case "allkeys-lru":
		return EvictAllKeysLRU, true
	case "allkeys-lfu":
		return EvictAllKeysLFU, true
	case "allkeys-random":
		return EvictAllKeysRandom, true
	case "volatile-lru":
		return EvictVolatileLRU, true
	case "volatile-lfu":
		return EvictVolatileLFU, true
	case "volatile-random":
		return EvictVolatileRandom, true
	case "volatile-ttl":
		return EvictVolatileTTL, true
	}
	return EvictNone, false
}

// volatileOnly reports whether the policy considers only keys with a TTL.
func (p EvictionPolicy) volatileOnly() bool {
	switch p {
	case EvictVolatileLRU, EvictVolatileLFU, EvictVolatileRandom, EvictVolatileTTL:
		return true
	}
	return false
}

// tracksAccess reports whether the policy needs the access clock maintained
// on every read. Nothing else does, so nothing else pays for it.
func (p EvictionPolicy) tracksAccess() bool {
	switch p {
	case EvictAllKeysLRU, EvictAllKeysLFU, EvictVolatileLRU, EvictVolatileLFU:
		return true
	}
	return false
}

func (p EvictionPolicy) usesLFU() bool {
	return p == EvictAllKeysLFU || p == EvictVolatileLFU
}

// evictionConfig is the live eviction setting, replaced wholesale on a
// CONFIG SET so a reader never sees half of one.
type evictionConfig struct {
	policy    EvictionPolicy
	maxMemory int64
	samples   int
}

// SetEviction installs the eviction policy.
func (ks *Keyspace) SetEviction(policy EvictionPolicy, maxMemory int64, samples int) {
	if samples < 1 {
		samples = 5
	}
	ks.eviction.Store(&evictionConfig{policy: policy, maxMemory: maxMemory, samples: samples})
	ks.trackAccess.Store(maxMemory > 0 && policy.tracksAccess())
	ks.lfu.Store(policy.usesLFU())
}

func (ks *Keyspace) evictionSettings() evictionConfig {
	if p := ks.eviction.Load(); p != nil {
		return *p
	}
	return evictionConfig{samples: 5}
}

// OverMemoryLimit reports whether the dataset exceeds maxmemory.
func (ks *Keyspace) OverMemoryLimit() bool {
	cfg := ks.evictionSettings()
	return cfg.maxMemory > 0 && ks.MemoryEstimate() > cfg.maxMemory
}

// Evict removes keys until the dataset is back under maxmemory, and returns
// how many went.
//
// It gives up rather than looping forever when a pass finds nothing to
// remove, which happens under a volatile policy with no TTL'd keys left. The
// caller then refuses the write, which is the honest answer: the policy
// asked for volatile keys and there are none.
func (ks *Keyspace) Evict() int {
	cfg := ks.evictionSettings()
	if cfg.maxMemory <= 0 || cfg.policy == EvictNone {
		return 0
	}
	// A replica never evicts on its own. Its leader's DEL arrives on the
	// stream, and evicting independently would diverge the two (FR-3.4).
	if ks.IsReplica() || ks.IsLoading() {
		return 0
	}

	// The loop targets strictly under the limit, because the check that
	// refuses a write treats being exactly at it as over.
	evicted := 0
	for ks.MemoryEstimate() >= cfg.maxMemory {
		if !ks.evictOne(cfg) {
			break
		}
		evicted++
	}
	return evicted
}

// evictOne samples candidates and removes the best one, reporting whether it
// found anything.
func (ks *Keyspace) evictOne(cfg evictionConfig) bool {
	ks.barrier.RLock()
	defer ks.barrier.RUnlock()

	var (
		bestDB   *DB
		bestKey  string
		bestObj  *Object
		bestRank uint64
		found    bool
	)

	now := ks.Now()

	// Sampling walks shards from a random starting point rather than
	// picking each one at random.
	//
	// Uniform picks look right and are not: with sixteen databases and data
	// in one of them, fifteen of every sixteen samples land on an empty
	// shard, every sample can miss, and eviction concludes there is nothing
	// to evict while the server is over its limit. A walk finds candidates
	// whenever any exist, and still starts somewhere random so it does not
	// favour the same shards every time.
	perDB := len(ks.dbs[0].shards)
	total := len(ks.dbs) * perDB
	start := rand.Intn(total)
	seen := 0

	for i := 0; i < total && seen < cfg.samples; i++ {
		idx := (start + i) % total
		db := ks.dbs[idx/perDB]
		s := db.shards[idx%perDB]

		s.mu.Lock()
		key, o := sampleCandidate(s, cfg.policy)
		if o != nil {
			seen++
			rank := evictionRank(o, cfg.policy, now, ks.lruNow())
			if !found || rank < bestRank {
				bestDB, bestKey, bestObj, bestRank, found = db, key, o, rank, true
			}
		}
		s.mu.Unlock()
	}
	if !found {
		return false
	}

	// The victim is removed under its own shard's locks, and may have been
	// changed or deleted since it was sampled, so it is looked up again
	// rather than trusted.
	s := bestDB.shardFor([]byte(bestKey))
	s.prop.Lock()
	s.mu.Lock()
	o := s.dict.getString(bestKey)
	ok := o != nil && o == bestObj
	if ok {
		bestDB.removeLocked(s, bestKey, o)
		s.evictedKeys++
		ks.emit(bestDB.Index, delCommand, []byte(bestKey))
		ks.signal(bestDB.Index, []byte(bestKey))
	}
	s.mu.Unlock()
	s.prop.Unlock()
	return ok
}

// sampleCandidate picks one key from a locked shard, honouring whether the
// policy wants only keys with a TTL.
func sampleCandidate(s *shard, policy EvictionPolicy) (string, *Object) {
	if policy.volatileOnly() {
		for k := range s.expires {
			if o := s.dict.getString(k); o != nil {
				return k, o
			}
		}
		return "", nil
	}
	k, o, _ := s.dict.randomEntry(rand.Intn)
	return k, o
}

// evictionRank scores a candidate: lower is evicted first.
func evictionRank(o *Object, policy EvictionPolicy, nowMS int64, lruNow uint32) uint64 {
	switch policy {
	case EvictAllKeysLRU, EvictVolatileLRU:
		// Idle time, inverted so that the least recently used ranks lowest.
		return uint64(o.LRU)
	case EvictAllKeysLFU, EvictVolatileLFU:
		return uint64(lfuCounter(o.LRU))
	case EvictVolatileTTL:
		// The soonest to expire goes first. A key with no TTL cannot be
		// sampled under a volatile policy, so the zero case does not arise.
		return uint64(o.ExpireAt)
	default:
		// Random: every candidate ranks the same, so the first sampled wins.
		return 0
	}
}

// Access clock.
//
// LRU stores a coarse second counter; LFU packs a decay minute and an eight
// bit logarithmic counter into the same field. One field serves both because
// no policy uses both at once, which keeps Object the size it was.

const lfuCounterBits = 8

// lruNow is the access clock in seconds since the keyspace started.
func (ks *Keyspace) lruNow() uint32 { return uint32(ks.Now() / 1000) }

// touchAccess records an access for the eviction policy, if one needs it.
//
// The check is an atomic load of a bool, so a server with no maxmemory set
// pays one load per lookup and writes nothing -- which matters because this
// is on the read path of every command.
func (ks *Keyspace) touchAccess(o *Object) {
	if !ks.trackAccess.Load() {
		return
	}
	if ks.lfu.Load() {
		o.LRU = lfuTouch(o.LRU, uint32(ks.Now()/60000))
		return
	}
	o.LRU = ks.lruNow()
}

// lfuCounter extracts the frequency counter from a packed LFU field.
func lfuCounter(v uint32) uint8 { return uint8(v & 0xff) }

// lfuMinute extracts the decay minute.
func lfuMinute(v uint32) uint32 { return v >> lfuCounterBits }

// lfuTouch decays the counter for elapsed time and then raises it.
//
// The counter is logarithmic: the higher it already is, the less likely an
// access is to raise it further. That is what lets eight bits distinguish a
// key read twice from one read ten thousand times, which a linear counter
// could not do without saturating almost immediately.
func lfuTouch(v uint32, minute uint32) uint32 {
	counter := lfuCounter(v)
	last := lfuMinute(v)
	if v == 0 {
		// A new key starts above zero so that it is not evicted
		// immediately, before it has had a chance to be used again.
		counter = 5
		last = minute
	}
	if minute > last {
		elapsed := minute - last
		if uint32(counter) > elapsed {
			counter -= uint8(elapsed)
		} else {
			counter = 0
		}
		last = minute
	}
	if counter < 255 {
		// The probability of an increment falls as the counter rises.
		if rand.Float64() < 1.0/(float64(counter)*10.0+1.0) {
			counter++
		}
	}
	return last<<lfuCounterBits | uint32(counter)
}

// evictionHolders are the atomics Keyspace embeds for eviction. They are
// kept together so the fields are obvious at the use site.
type evictionHolder = atomic.Pointer[evictionConfig]
