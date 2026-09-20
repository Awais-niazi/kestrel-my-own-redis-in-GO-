package engine

import (
	"fmt"
	"testing"
)

// evictionKeyspace returns a keyspace with a policy and a limit in place.
func evictionKeyspace(t *testing.T, policy EvictionPolicy, maxMemory int64) *Keyspace {
	t.Helper()
	ks := New(Options{Databases: 1, Shards: 4, ActiveExpire: false})
	t.Cleanup(ks.Close)
	ks.SetEviction(policy, maxMemory, 5)
	return ks
}

// fill writes n keys and returns the resulting memory estimate.
func fill(t *testing.T, db *DB, n int, prefix string) int64 {
	t.Helper()
	val := []byte("a value of a fairly realistic length for a cache entry")
	for i := 0; i < n; i++ {
		if _, err := db.Set(fmt.Appendf(nil, "%s%d", prefix, i), val, SetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return db.MemoryEstimate()
}

func TestParseEvictionPolicy(t *testing.T) {
	for name, want := range map[string]EvictionPolicy{
		"noeviction": EvictNone, "allkeys-lru": EvictAllKeysLRU,
		"allkeys-lfu": EvictAllKeysLFU, "allkeys-random": EvictAllKeysRandom,
		"volatile-lru": EvictVolatileLRU, "volatile-lfu": EvictVolatileLFU,
		"volatile-random": EvictVolatileRandom, "volatile-ttl": EvictVolatileTTL,
	} {
		got, ok := ParseEvictionPolicy(name)
		if !ok || got != want {
			t.Errorf("ParseEvictionPolicy(%q) = %v, %v", name, got, ok)
		}
	}
	if _, ok := ParseEvictionPolicy("allkeys-guess"); ok {
		t.Error("an unknown policy was accepted")
	}
}

func TestNoEvictionRemovesNothing(t *testing.T) {
	ks := evictionKeyspace(t, EvictNone, 1)
	db := ks.DB(0)
	fill(t, db, 100, "k")
	if n := ks.Evict(); n != 0 {
		t.Errorf("noeviction removed %d keys", n)
	}
	if got := db.Size(); got != 100 {
		t.Errorf("%d keys remain, want 100", got)
	}
}

// TestEvictionGetsBackUnderTheLimit is the property that matters: whatever
// the policy chooses, it keeps choosing until the dataset fits.
func TestEvictionGetsBackUnderTheLimit(t *testing.T) {
	for _, policy := range []EvictionPolicy{
		EvictAllKeysLRU, EvictAllKeysLFU, EvictAllKeysRandom,
	} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			ks := evictionKeyspace(t, policy, 1<<30)
			db := ks.DB(0)
			full := fill(t, db, 400, "k")

			// Halve the budget and let the policy make room.
			ks.SetEviction(policy, full/2, 5)
			ks.Evict()

			if got := db.MemoryEstimate(); got > full/2 {
				t.Errorf("still %d bytes after eviction, limit is %d", got, full/2)
			}
			if db.Size() == 0 {
				t.Error("eviction emptied the database")
			}
			if db.Size() == 400 {
				t.Error("eviction removed nothing")
			}
		})
	}
}

// TestVolatilePolicyOnlyTakesKeysWithATTL is what separates the two
// families, and getting it wrong means a cache silently deleting data an
// operator marked as permanent.
func TestVolatilePolicyOnlyTakesKeysWithATTL(t *testing.T) {
	ks := evictionKeyspace(t, EvictVolatileLRU, 1<<30)
	now := int64(1_700_000_000_000)
	ks.SetClock(func() int64 { return now })
	db := ks.DB(0)

	fill(t, db, 200, "permanent")
	fill(t, db, 200, "temporary")
	for i := 0; i < 200; i++ {
		db.Expire(fmt.Appendf(nil, "temporary%d", i), now+3_600_000, 0)
	}
	full := db.MemoryEstimate()

	ks.SetEviction(EvictVolatileLRU, full/2, 5)
	ks.Evict()

	for i := 0; i < 200; i++ {
		if _, ok, _ := db.Get(fmt.Appendf(nil, "permanent%d", i)); !ok {
			t.Fatalf("permanent%d was evicted under a volatile policy", i)
		}
	}
}

// TestVolatilePolicyStopsWhenNothingHasATTL: the honest outcome is to give
// up, so the caller can refuse the write, rather than to start taking keys
// the policy excluded.
func TestVolatilePolicyStopsWhenNothingHasATTL(t *testing.T) {
	ks := evictionKeyspace(t, EvictVolatileLRU, 1<<30)
	db := ks.DB(0)
	full := fill(t, db, 200, "permanent")

	ks.SetEviction(EvictVolatileLRU, full/2, 5)
	if n := ks.Evict(); n != 0 {
		t.Errorf("a volatile policy removed %d keys with no TTL anywhere", n)
	}
	if got := db.Size(); got != 200 {
		t.Errorf("%d keys remain, want all 200", got)
	}
}

// TestLRUPrefersColdKeys checks that recently read keys survive. Sampling
// makes this approximate, so the bar is a clear majority rather than all.
func TestLRUPrefersColdKeys(t *testing.T) {
	ks := evictionKeyspace(t, EvictAllKeysLRU, 1<<30)
	now := int64(1_700_000_000_000)
	ks.SetClock(func() int64 { return now })
	db := ks.DB(0)

	fill(t, db, 400, "k")
	// Move the clock on, then touch the first hundred so they are the
	// recently used ones.
	now += 600_000
	for i := 0; i < 100; i++ {
		db.Get(fmt.Appendf(nil, "k%d", i))
	}

	ks.SetEviction(EvictAllKeysLRU, db.MemoryEstimate()/2, 10)
	ks.Evict()

	warm := 0
	for i := 0; i < 100; i++ {
		if _, ok, _ := db.Get(fmt.Appendf(nil, "k%d", i)); ok {
			warm++
		}
	}
	if warm < 80 {
		t.Errorf("only %d of the 100 recently used keys survived; LRU is not "+
			"preferring cold keys", warm)
	}
}

// TestEvictionPropagatesDeletes: a replica must be told, or the two diverge.
func TestEvictionPropagatesDeletes(t *testing.T) {
	ks := evictionKeyspace(t, EvictAllKeysRandom, 1<<30)
	db := ks.DB(0)
	full := fill(t, db, 200, "k")

	var effects int
	ks.SetEffectSink(sinkFunc(func(db int, args ...[]byte) {
		if string(args[0]) == "DEL" {
			effects++
		}
	}))
	ks.SetEviction(EvictAllKeysRandom, full/2, 5)
	n := ks.Evict()

	if n == 0 {
		t.Fatal("nothing was evicted")
	}
	if effects != n {
		t.Errorf("%d keys evicted but %d DELs propagated; a replica would "+
			"keep what this node dropped", n, effects)
	}
}

type sinkFunc func(db int, args ...[]byte)

func (f sinkFunc) Effect(db int, args ...[]byte) { f(db, args...) }

// TestReplicaDoesNotEvict: its leader's DEL arrives on the stream, and
// evicting independently would diverge the two.
func TestReplicaDoesNotEvict(t *testing.T) {
	ks := evictionKeyspace(t, EvictAllKeysRandom, 1<<30)
	db := ks.DB(0)
	full := fill(t, db, 200, "k")
	ks.SetEviction(EvictAllKeysRandom, full/2, 5)
	ks.SetReplica(true)

	if n := ks.Evict(); n != 0 {
		t.Errorf("a replica evicted %d keys on its own", n)
	}
}

func TestEvictionCountsAreReported(t *testing.T) {
	ks := evictionKeyspace(t, EvictAllKeysRandom, 1<<30)
	db := ks.DB(0)
	full := fill(t, db, 200, "k")
	ks.SetEviction(EvictAllKeysRandom, full/2, 5)
	n := ks.Evict()
	if got := ks.Stats().EvictedKeys; got != int64(n) {
		t.Errorf("Stats reports %d evicted, %d were", got, n)
	}
}

// TestLFUCounterRisesAndDecays covers the packed counter directly, because
// its behaviour is not observable from the outside in a short test.
func TestLFUCounterRisesAndDecays(t *testing.T) {
	v := lfuTouch(0, 100)
	if c := lfuCounter(v); c != 5 {
		t.Errorf("a new key starts at %d, want 5 so it is not evicted immediately", c)
	}
	if m := lfuMinute(v); m != 100 {
		t.Errorf("the decay minute is %d, want 100", m)
	}

	// Many accesses raise it, logarithmically, so it does not saturate.
	for i := 0; i < 10000; i++ {
		v = lfuTouch(v, 100)
	}
	hot := lfuCounter(v)
	if hot <= 5 {
		t.Errorf("ten thousand accesses left the counter at %d", hot)
	}

	// Time passing brings it back down.
	v = lfuTouch(v, 100+uint32(hot)+10)
	if c := lfuCounter(v); c >= hot {
		t.Errorf("the counter is %d after a long idle period, was %d; it should decay",
			c, hot)
	}
}

// TestAccessClockIsOnlyMaintainedWhenNeeded: nothing but an LRU or LFU
// policy should pay for it on the read path.
func TestAccessClockIsOnlyMaintainedWhenNeeded(t *testing.T) {
	ks := evictionKeyspace(t, EvictAllKeysRandom, 1<<30)
	db := ks.DB(0)
	db.Set([]byte("k"), []byte("v"), SetOptions{})
	db.Get([]byte("k"))

	s := db.shardFor([]byte("k"))
	s.mu.Lock()
	lru := s.dict.getString("k").LRU
	s.mu.Unlock()
	if lru != 0 {
		t.Errorf("the access clock was written under a random policy: %d", lru)
	}

	ks.SetEviction(EvictAllKeysLRU, 1<<30, 5)
	db.Get([]byte("k"))
	s.mu.Lock()
	lru = s.dict.getString("k").LRU
	s.mu.Unlock()
	if lru == 0 {
		t.Error("the access clock was not written under an LRU policy")
	}
}
