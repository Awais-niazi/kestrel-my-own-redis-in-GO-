package engine

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func benchKeyspace(b *testing.B, keys int) (*Keyspace, *DB) {
	b.Helper()
	opts := DefaultOptions()
	opts.ActiveExpire = false
	ks := New(opts)
	b.Cleanup(ks.Close)
	db := ks.DB(0)
	for i := 0; i < keys; i++ {
		db.Set([]byte("key:"+strconv.Itoa(i)), []byte("value of moderate length here"), SetOptions{})
	}
	return ks, db
}

func BenchmarkEngineGet(b *testing.B) {
	_, db := benchKeyspace(b, 100_000)
	key := []byte("key:50000")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Get(key)
	}
}

func BenchmarkEngineGetWithTTL(b *testing.B) {
	ks, db := benchKeyspace(b, 100_000)
	key := []byte("ttl-key")
	db.Set(key, []byte("v"), SetOptions{At: ks.Now() + 3_600_000})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Get(key)
	}
}

// BenchmarkEngineGetWithTTLCachedClock is the same read against the cached
// clock the server enables, isolating what the clock read costs.
func BenchmarkEngineGetWithTTLCachedClock(b *testing.B) {
	opts := DefaultOptions()
	opts.ActiveExpire = false
	opts.CachedClock = true
	ks := New(opts)
	defer ks.Close()
	db := ks.DB(0)
	key := []byte("ttl-key")
	db.Set(key, []byte("v"), SetOptions{At: ks.Now() + 3_600_000})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Get(key)
	}
}

func BenchmarkEngineSet(b *testing.B) {
	_, db := benchKeyspace(b, 0)
	key := []byte("key")
	val := []byte("value of moderate length here")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Set(key, val, SetOptions{})
	}
}

func BenchmarkEngineIncr(b *testing.B) {
	_, db := benchKeyspace(b, 0)
	key := []byte("counter")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.IncrBy(key, 1)
	}
}

// BenchmarkEngineGetParallel is the scaling measurement behind the M5 exit
// criterion: throughput must rise with cores for single-key commands.
func BenchmarkEngineGetParallel(b *testing.B) {
	_, db := benchKeyspace(b, 100_000)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			db.Get([]byte("key:" + strconv.Itoa(i%100_000)))
			i++
		}
	})
}

func BenchmarkEngineSetParallel(b *testing.B) {
	_, db := benchKeyspace(b, 0)
	val := []byte("value of moderate length here")
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			db.Set([]byte("key:"+strconv.Itoa(i%100_000)), val, SetOptions{})
			i++
		}
	})
}

// BenchmarkShardScaling reports how throughput moves with the shard count,
// which is the data Q1 asks for.
func BenchmarkShardScaling(b *testing.B) {
	for _, shards := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("shards=%d", shards), func(b *testing.B) {
			opts := DefaultOptions()
			opts.Shards = shards
			opts.ActiveExpire = false
			ks := New(opts)
			defer ks.Close()
			db := ks.DB(0)
			val := []byte("v")
			for i := 0; i < 10_000; i++ {
				db.Set([]byte("key:"+strconv.Itoa(i)), val, SetOptions{})
			}
			b.ResetTimer()
			var wg sync.WaitGroup
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					k := []byte("key:" + strconv.Itoa(i%10_000))
					if i%4 == 0 {
						db.Set(k, val, SetOptions{})
					} else {
						db.Get(k)
					}
					i++
				}
			})
			wg.Wait()
		})
	}
}
