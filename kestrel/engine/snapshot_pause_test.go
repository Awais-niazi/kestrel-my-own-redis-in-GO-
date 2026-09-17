package engine

import (
	"fmt"
	"testing"
	"time"
)

// TestSnapshotShardPauseByShardCount measures the claim in
// docs/design-notes.md issue 9: a shard is locked for as long as it takes to
// serialize it, and raising the shard count divides that pause.
//
// It reports rather than asserts a latency, because a number measured on a
// loaded CI machine is not a fact about the code. What it does assert is the
// shape: more shards must mean a shorter worst-case hold, because that is
// the mitigation the design leans on and it would be worth knowing if it
// stopped being true.
func TestSnapshotShardPauseByShardCount(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a large keyspace")
	}
	const keys = 200_000

	type result struct {
		shards int
		worst  time.Duration
		total  time.Duration
	}
	var results []result

	for _, shards := range []int{16, 64, 256} {
		ks := New(Options{Databases: 1, Shards: shards, ActiveExpire: false})
		db := ks.DB(0)
		val := []byte("a value of a fairly realistic length for a cache entry")
		for i := 0; i < keys; i++ {
			db.Set(fmt.Appendf(nil, "key:%d", i), val, SetOptions{})
		}

		var worst, total time.Duration
		for shard := 0; shard < shards; shard++ {
			start := time.Now()
			n := 0
			_, err := db.SnapshotShard(shard, func() uint64 { return 0 },
				func(e *SnapshotEntry) error { n++; return nil })
			if err != nil {
				t.Fatal(err)
			}
			held := time.Since(start)
			total += held
			if held > worst {
				worst = held
			}
		}
		ks.Close()
		results = append(results, result{shards, worst, total})
		t.Logf("%3d shards: worst shard hold %8v, whole pass %v, %d keys",
			shards, worst.Round(time.Microsecond), total.Round(time.Millisecond), keys)
	}

	// The mitigation is that the pause divides by the shard count. Timing on
	// a shared machine is noisy, so the bar is deliberately loose: sixteen
	// times the shards must at least halve the worst hold.
	first, last := results[0], results[len(results)-1]
	if last.worst*2 > first.worst {
		t.Errorf("raising the shard count from %d to %d did not shorten the worst "+
			"hold (%v then %v); docs/design-notes.md issue 9 recommends this as "+
			"the mitigation for the snapshot pause",
			first.shards, last.shards, first.worst, last.worst)
	}
}
