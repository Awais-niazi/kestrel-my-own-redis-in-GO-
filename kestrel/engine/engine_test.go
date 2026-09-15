package engine

import (
	"fmt"
	"go/build"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testKeyspace returns a keyspace with a controllable clock and the active
// expiry cycle disabled, so expiry tests are deterministic.
func testKeyspace(t *testing.T) (*Keyspace, *int64) {
	t.Helper()
	opts := DefaultOptions()
	opts.ActiveExpire = false
	opts.Shards = 4
	ks := New(opts)
	t.Cleanup(ks.Close)
	var now int64 = 1_700_000_000_000
	ks.SetClock(func() int64 { return atomic.LoadInt64(&now) })
	return ks, &now
}

func mustSet(t *testing.T, db *DB, key, val string) {
	t.Helper()
	if _, err := db.Set([]byte(key), []byte(val), SetOptions{}); err != nil {
		t.Fatalf("SET %s: %v", key, err)
	}
}

func getString(t *testing.T, db *DB, key string) (string, bool) {
	t.Helper()
	v, ok, err := db.Get([]byte(key))
	if err != nil {
		t.Fatalf("GET %s: %v", key, err)
	}
	return string(v), ok
}

func TestSetGet(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)

	if _, ok := getString(t, db, "missing"); ok {
		t.Fatal("missing key reported as present")
	}
	mustSet(t, db, "k", "hello")
	if v, ok := getString(t, db, "k"); !ok || v != "hello" {
		t.Fatalf("got %q %v", v, ok)
	}
	mustSet(t, db, "k", "")
	if v, ok := getString(t, db, "k"); !ok || v != "" {
		t.Fatalf("empty value lost: %q %v", v, ok)
	}
}

func TestSetOptions(t *testing.T) {
	ks, now := testKeyspace(t)
	db := ks.DB(0)

	res, _ := db.Set([]byte("k"), []byte("a"), SetOptions{XX: true})
	if res.Set {
		t.Fatal("XX set a missing key")
	}
	res, _ = db.Set([]byte("k"), []byte("a"), SetOptions{NX: true})
	if !res.Set {
		t.Fatal("NX failed to set a missing key")
	}
	res, _ = db.Set([]byte("k"), []byte("b"), SetOptions{NX: true})
	if res.Set {
		t.Fatal("NX overwrote an existing key")
	}
	res, _ = db.Set([]byte("k"), []byte("c"), SetOptions{Get: true})
	if !res.Set || !res.OldExists || string(res.Old) != "a" {
		t.Fatalf("GET option: %+v", res)
	}

	// A plain SET clears the TTL; KEEPTTL retains it.
	db.Set([]byte("k"), []byte("v"), SetOptions{At: *now + 1000})
	if ttl := db.PTTL([]byte("k")); ttl != 1000 {
		t.Fatalf("ttl %d", ttl)
	}
	db.Set([]byte("k"), []byte("v2"), SetOptions{KeepTTL: true})
	if ttl := db.PTTL([]byte("k")); ttl != 1000 {
		t.Fatalf("KEEPTTL lost the ttl: %d", ttl)
	}
	db.Set([]byte("k"), []byte("v3"), SetOptions{})
	if ttl := db.PTTL([]byte("k")); ttl != int64(TTLNoExpiry) {
		t.Fatalf("plain SET kept the ttl: %d", ttl)
	}
}

func TestSetGetOptionWrongType(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)
	s := db.lockKey([]byte("l"))
	db.store(s, []byte("l"), &Object{Type: TypeList, Value: nil})
	db.unlockKey(s)

	if _, err := db.Set([]byte("l"), []byte("x"), SetOptions{Get: true}); err != ErrWrongType {
		t.Fatalf("got %v want ErrWrongType", err)
	}
	// Without GET, SET overwrites a value of any type.
	if res, err := db.Set([]byte("l"), []byte("x"), SetOptions{}); err != nil || !res.Set {
		t.Fatalf("plain SET over a list: %+v %v", res, err)
	}
}

func TestLazyExpiration(t *testing.T) {
	ks, now := testKeyspace(t)
	db := ks.DB(0)

	db.Set([]byte("k"), []byte("v"), SetOptions{At: *now + 100})
	if _, ok := getString(t, db, "k"); !ok {
		t.Fatal("key expired early")
	}
	atomic.StoreInt64(now, *now+101)
	if _, ok := getString(t, db, "k"); ok {
		t.Fatal("expired key still readable")
	}
	if n := db.Size(); n != 0 {
		t.Fatalf("expired key still counted: %d", n)
	}
}

func TestExpireFlags(t *testing.T) {
	ks, now := testKeyspace(t)
	db := ks.DB(0)
	base := *now

	mustSet(t, db, "k", "v")
	if ok, _ := db.Expire([]byte("k"), base+1000, ExpireXX); ok {
		t.Fatal("XX applied to a key with no TTL")
	}
	if ok, _ := db.Expire([]byte("k"), base+1000, ExpireNX); !ok {
		t.Fatal("NX rejected a key with no TTL")
	}
	if ok, _ := db.Expire([]byte("k"), base+2000, ExpireNX); ok {
		t.Fatal("NX applied to a key that already has a TTL")
	}
	if ok, _ := db.Expire([]byte("k"), base+500, ExpireGT); ok {
		t.Fatal("GT lowered a TTL")
	}
	if ok, _ := db.Expire([]byte("k"), base+2000, ExpireGT); !ok {
		t.Fatal("GT rejected a raise")
	}
	if ok, _ := db.Expire([]byte("k"), base+3000, ExpireLT); ok {
		t.Fatal("LT raised a TTL")
	}
	if ok, _ := db.Expire([]byte("k"), base+1000, ExpireLT); !ok {
		t.Fatal("LT rejected a lower")
	}

	// An expiry in the past deletes the key outright.
	applied, deleted := db.Expire([]byte("k"), base-1, 0)
	if !applied || !deleted {
		t.Fatalf("past expiry: applied=%v deleted=%v", applied, deleted)
	}
	if _, ok := getString(t, db, "k"); ok {
		t.Fatal("key survived a past expiry")
	}
}

func TestPersistAndTTL(t *testing.T) {
	ks, now := testKeyspace(t)
	db := ks.DB(0)

	if got := db.PTTL([]byte("nope")); got != int64(TTLNoKey) {
		t.Fatalf("got %d", got)
	}
	mustSet(t, db, "k", "v")
	if got := db.PTTL([]byte("k")); got != int64(TTLNoExpiry) {
		t.Fatalf("got %d", got)
	}
	if db.Persist([]byte("k")) {
		t.Fatal("PERSIST reported work on a key with no TTL")
	}
	db.Expire([]byte("k"), *now+5000, 0)
	if got := db.ExpireTime([]byte("k")); got != *now+5000 {
		t.Fatalf("got %d", got)
	}
	if !db.Persist([]byte("k")) {
		t.Fatal("PERSIST did nothing")
	}
	if got := db.PTTL([]byte("k")); got != int64(TTLNoExpiry) {
		t.Fatalf("got %d", got)
	}
}

func TestActiveExpirePass(t *testing.T) {
	ks, now := testKeyspace(t)
	db := ks.DB(0)

	for i := 0; i < 500; i++ {
		k := fmt.Appendf(nil, "k%d", i)
		db.Set(k, []byte("v"), SetOptions{At: *now + 100})
	}
	for i := 0; i < 50; i++ {
		mustSet(t, db, fmt.Sprintf("keep%d", i), "v")
	}
	atomic.StoreInt64(now, *now+101)

	// Run passes until the reaper reports nothing left to do.
	for i := 0; i < 200; i++ {
		if ks.ExpirePass(time.Now().Add(50*time.Millisecond)) == 0 {
			break
		}
	}
	db.lockAll()
	var remaining int
	for _, s := range db.shards {
		remaining += len(s.dict)
	}
	db.unlockAll()
	if remaining != 50 {
		t.Fatalf("active expiry left %d keys, want 50", remaining)
	}
	if got := ks.Stats().ExpiredKeys; got != 500 {
		t.Fatalf("expired counter %d", got)
	}
}

// recordingSink captures the effects the engine produces on its own.
type recordingSink struct {
	mu   sync.Mutex
	cmds []string
}

func (r *recordingSink) Effect(db int, args ...[]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = string(a)
	}
	r.cmds = append(r.cmds, fmt.Sprintf("%d:%s", db, strings.Join(parts, " ")))
}

func (r *recordingSink) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

func TestExpiryPropagatesDelete(t *testing.T) {
	ks, now := testKeyspace(t)
	sink := &recordingSink{}
	ks.SetEffectSink(sink)
	db := ks.DB(0)

	db.Set([]byte("k"), []byte("v"), SetOptions{At: *now + 10})
	atomic.StoreInt64(now, *now+11)
	getString(t, db, "k")

	got := sink.all()
	if len(got) != 1 || got[0] != "0:DEL k" {
		t.Fatalf("got %v want [0:DEL k]", got)
	}
}

func TestReplicaHidesButDoesNotDelete(t *testing.T) {
	ks, now := testKeyspace(t)
	sink := &recordingSink{}
	ks.SetEffectSink(sink)
	db := ks.DB(0)

	db.Set([]byte("k"), []byte("v"), SetOptions{At: *now + 10})
	ks.SetReplica(true)
	atomic.StoreInt64(now, *now+11)

	if _, ok := getString(t, db, "k"); ok {
		t.Fatal("replica served an expired key")
	}
	if got := sink.all(); len(got) != 0 {
		t.Fatalf("replica expired a key on its own clock: %v", got)
	}
	// The leader's DEL is what actually removes it.
	db.lockAll()
	present := len(db.shards[db.shardIndex([]byte("k"))].dict)
	db.unlockAll()
	if present != 1 {
		t.Fatal("replica deleted the key locally")
	}
}

func TestIncr(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)

	n, err := db.IncrBy([]byte("c"), 1)
	if err != nil || n != 1 {
		t.Fatalf("got %d %v", n, err)
	}
	if n, _ = db.IncrBy([]byte("c"), 41); n != 42 {
		t.Fatalf("got %d", n)
	}
	if v, _ := getString(t, db, "c"); v != "42" {
		t.Fatalf("got %q", v)
	}
	if enc, _ := db.Encoding([]byte("c")); enc != EncodingInt {
		t.Fatalf("counter encoding %v", enc)
	}

	mustSet(t, db, "s", "abc")
	if _, err := db.IncrBy([]byte("s"), 1); err != ErrNotInteger {
		t.Fatalf("got %v", err)
	}
	mustSet(t, db, "big", "9223372036854775807")
	if _, err := db.IncrBy([]byte("big"), 1); err != ErrOverflow {
		t.Fatalf("got %v", err)
	}

	// INCR must not disturb an existing TTL.
	db.Expire([]byte("c"), ks.Now()+5000, 0)
	db.IncrBy([]byte("c"), 1)
	if ttl := db.PTTL([]byte("c")); ttl <= 0 {
		t.Fatalf("INCR cleared the ttl: %d", ttl)
	}
}

func TestIncrByFloat(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)

	_, rendered, err := db.IncrByFloat([]byte("f"), 10.5)
	if err != nil || string(rendered) != "10.5" {
		t.Fatalf("got %q %v", rendered, err)
	}
	_, rendered, _ = db.IncrByFloat([]byte("f"), 0.1)
	if string(rendered) != "10.6" {
		t.Fatalf("float accumulation rendered %q, want 10.6", rendered)
	}
	mustSet(t, db, "s", "abc")
	if _, _, err := db.IncrByFloat([]byte("s"), 1); err != ErrNotFloat {
		t.Fatalf("got %v", err)
	}
}

func TestFormatFloat(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{3, "3"}, {3.0, "3"}, {10.5, "10.5"}, {0.1, "0.1"},
		{-1.25, "-1.25"}, {3.0e3, "3000"},
	}
	for _, c := range cases {
		if got := string(FormatFloat(c.in)); got != c.want {
			t.Errorf("FormatFloat(%v) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestAppendGetRangeSetRange(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)

	if n, _ := db.Append([]byte("k"), []byte("Hello")); n != 5 {
		t.Fatalf("got %d", n)
	}
	if n, _ := db.Append([]byte("k"), []byte(" World")); n != 11 {
		t.Fatalf("got %d", n)
	}
	if v, _ := getString(t, db, "k"); v != "Hello World" {
		t.Fatalf("got %q", v)
	}

	for _, c := range []struct {
		start, end int64
		want       string
	}{
		{0, 4, "Hello"}, {-5, -1, "World"}, {0, -1, "Hello World"},
		{5, 1, ""}, {20, 30, ""}, {-100, -200, ""},
	} {
		got, err := db.GetRange([]byte("k"), c.start, c.end)
		if err != nil || string(got) != c.want {
			t.Errorf("GETRANGE %d %d = %q, %v; want %q", c.start, c.end, got, err, c.want)
		}
	}

	if n, _ := db.SetRange([]byte("k"), 6, []byte("Redis")); n != 11 {
		t.Fatalf("got %d", n)
	}
	if v, _ := getString(t, db, "k"); v != "Hello Redis" {
		t.Fatalf("got %q", v)
	}
	if n, _ := db.SetRange([]byte("pad"), 3, []byte("x")); n != 4 {
		t.Fatalf("got %d", n)
	}
	if v, _ := getString(t, db, "pad"); v != "\x00\x00\x00x" {
		t.Fatalf("got %q", v)
	}
	if _, err := db.SetRange([]byte("k"), -1, []byte("x")); err != ErrOutOfRange {
		t.Fatalf("got %v", err)
	}
}

// TestAppendDoesNotMutateSharedValue guards the invariant that stored values
// are immutable, which is what makes lock-free reads of returned slices safe.
func TestAppendDoesNotMutateSharedValue(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)
	mustSet(t, db, "k", "abc")
	before, _, _ := db.Get([]byte("k"))
	db.Append([]byte("k"), []byte("defghijklmnop"))
	if string(before) != "abc" {
		t.Fatalf("APPEND mutated a value another reader held: %q", before)
	}
}

func TestMGetMSet(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)

	db.MSet([][]byte{[]byte("a"), []byte("1"), []byte("b"), []byte("2")})
	got := db.MGet([][]byte{[]byte("a"), []byte("b"), []byte("missing")})
	if len(got) != 3 || string(got[0]) != "1" || string(got[1]) != "2" || got[2] != nil {
		t.Fatalf("got %q", got)
	}
	if db.MSetNX([][]byte{[]byte("b"), []byte("x"), []byte("c"), []byte("y")}) {
		t.Fatal("MSETNX wrote despite an existing key")
	}
	if _, ok := getString(t, db, "c"); ok {
		t.Fatal("MSETNX partially applied")
	}
	if !db.MSetNX([][]byte{[]byte("c"), []byte("y")}) {
		t.Fatal("MSETNX refused a clean write")
	}
}

func TestGenericCommands(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)

	mustSet(t, db, "a", "1")
	mustSet(t, db, "b", "2")
	if n := db.Exists([][]byte{[]byte("a"), []byte("a"), []byte("nope")}); n != 2 {
		t.Fatalf("EXISTS counted %d", n)
	}
	if typ, ok := db.Type([]byte("a")); !ok || typ != TypeString {
		t.Fatalf("got %v %v", typ, ok)
	}
	if n := db.Del([][]byte{[]byte("a"), []byte("nope")}); n != 1 {
		t.Fatalf("DEL removed %d", n)
	}

	if _, err := db.Rename([]byte("nope"), []byte("x"), false); err != ErrNoSuchKey {
		t.Fatalf("got %v", err)
	}
	if ok, err := db.Rename([]byte("b"), []byte("c"), false); !ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
	if v, ok := getString(t, db, "c"); !ok || v != "2" {
		t.Fatalf("got %q", v)
	}
	mustSet(t, db, "d", "4")
	if ok, _ := db.Rename([]byte("c"), []byte("d"), true); ok {
		t.Fatal("RENAMENX overwrote an existing key")
	}

	if !db.Copy([]byte("d"), []byte("e"), nil, false) {
		t.Fatal("COPY failed")
	}
	if db.Copy([]byte("d"), []byte("e"), nil, false) {
		t.Fatal("COPY overwrote without REPLACE")
	}
	if !db.Copy([]byte("d"), []byte("e"), ks.DB(1), false) {
		t.Fatal("cross-database COPY failed")
	}
	if v, ok := getString(t, ks.DB(1), "e"); !ok || v != "4" {
		t.Fatalf("got %q", v)
	}
}

func TestKeysAndScanCoverEverything(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)
	const n = 1000
	for i := 0; i < n; i++ {
		mustSet(t, db, fmt.Sprintf("key:%d", i), "v")
	}
	mustSet(t, db, "other", "v")

	if got := len(db.Keys([]byte("key:*"))); got != n {
		t.Fatalf("KEYS returned %d want %d", got, n)
	}
	if got := len(db.Keys([]byte("*"))); got != n+1 {
		t.Fatalf("KEYS * returned %d", got)
	}

	seen := map[string]int{}
	var cursor uint64
	calls := 0
	for {
		var keys [][]byte
		cursor, keys = db.Scan(cursor, ScanOptions{Count: 100})
		for _, k := range keys {
			seen[string(k)]++
		}
		calls++
		if cursor == 0 || calls > 1000 {
			break
		}
	}
	if len(seen) != n+1 {
		t.Fatalf("SCAN saw %d distinct keys, want %d", len(seen), n+1)
	}
	for k, c := range seen {
		if c != 1 {
			t.Fatalf("SCAN returned %s %d times; a stable key must appear exactly once", k, c)
		}
	}

	// MATCH and TYPE filters.
	cursor = 0
	matched := 0
	for {
		var keys [][]byte
		cursor, keys = db.Scan(cursor, ScanOptions{Match: []byte("key:1?"), Count: 10})
		matched += len(keys)
		if cursor == 0 {
			break
		}
	}
	if matched != 10 {
		t.Fatalf("SCAN MATCH returned %d, want 10", matched)
	}
}

func TestRandomKey(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)
	if db.RandomKey() != nil {
		t.Fatal("RANDOMKEY on an empty database")
	}
	mustSet(t, db, "only", "v")
	if got := db.RandomKey(); string(got) != "only" {
		t.Fatalf("got %q", got)
	}
}

func TestFlushAndSwap(t *testing.T) {
	ks, _ := testKeyspace(t)
	mustSet(t, ks.DB(0), "a", "1")
	mustSet(t, ks.DB(1), "b", "2")

	if err := ks.SwapDB(0, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := getString(t, ks.DB(0), "b"); !ok {
		t.Fatal("SWAPDB did not move data")
	}
	if err := ks.SwapDB(0, 99); err == nil {
		t.Fatal("SWAPDB accepted an out-of-range index")
	}

	ks.DB(0).Flush()
	if n := ks.DB(0).Size(); n != 0 {
		t.Fatalf("FLUSHDB left %d keys", n)
	}
	if n := ks.DB(1).Size(); n != 1 {
		t.Fatalf("FLUSHDB touched another database")
	}
	ks.FlushAll()
	if n := ks.TotalKeys(); n != 0 {
		t.Fatalf("FLUSHALL left %d keys", n)
	}
}

func TestMemoryEstimateTracksWrites(t *testing.T) {
	ks, _ := testKeyspace(t)
	db := ks.DB(0)
	base := ks.MemoryEstimate()
	for i := 0; i < 100; i++ {
		mustSet(t, db, fmt.Sprintf("k%d", i), strings.Repeat("v", 100))
	}
	grown := ks.MemoryEstimate()
	if grown <= base {
		t.Fatalf("estimate did not grow: %d -> %d", base, grown)
	}
	if _, ok := db.MemoryUsage([]byte("k0")); !ok {
		t.Fatal("MEMORY USAGE reported a missing key")
	}
	db.Flush()
	if got := ks.MemoryEstimate(); got != 0 {
		t.Fatalf("estimate after flush: %d", got)
	}
}

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true}, {"*", "anything", true},
		{"h?llo", "hello", true}, {"h?llo", "heello", false},
		{"h*llo", "hllo", true}, {"h*llo", "heeeello", true},
		{"h[ae]llo", "hallo", true}, {"h[ae]llo", "hillo", false},
		{"h[^e]llo", "hallo", true}, {"h[^e]llo", "hello", false},
		{"h[a-c]llo", "hbllo", true}, {"h[a-c]llo", "hdllo", false},
		{"key:*", "key:1", true}, {"key:*", "other", false},
		{"a\\*b", "a*b", true}, {"a\\*b", "axb", false},
		{"", "", true}, {"", "x", false},
		{"*a*b*", "xaybz", true}, {"*a*b*", "ba", false},
		{"ab", "ab", true}, {"ab", "abc", false},
	}
	for _, c := range cases {
		if got := MatchPattern([]byte(c.pattern), []byte(c.s)); got != c.want {
			t.Errorf("MatchPattern(%q, %q) = %v want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// TestConcurrentAccess is the -race workhorse: many goroutines hitting
// overlapping keys through every locking path in the engine.
func TestConcurrentAccess(t *testing.T) {
	ks := New(Options{Databases: 2, Shards: 8, ActiveExpire: true,
		ActiveExpireSampleSize: 20, ActiveExpireCPUPercent: 25})
	defer ks.Close()
	db := ks.DB(0)

	const workers = 16
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				k := fmt.Appendf(nil, "k%d", i%50)
				switch i % 8 {
				case 0:
					db.Set(k, []byte("v"), SetOptions{})
				case 1:
					db.Get(k)
				case 2:
					db.IncrBy([]byte("counter"), 1)
				case 3:
					db.Append(k, []byte("x"))
				case 4:
					db.MGet([][]byte{k, []byte("counter"), []byte("k1")})
				case 5:
					db.Del([][]byte{k})
				case 6:
					db.Set(k, []byte("v"), SetOptions{At: ks.Now() + 5})
				case 7:
					db.Scan(0, ScanOptions{Count: 10})
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestEngineImportGraph enforces the layering rule from ADR-017: the engine
// is a library with no knowledge of the network or the wire protocol.
func TestEngineImportGraph(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"net", "net/http", "kestrel/resp", "kestrel/command",
		"kestrel/server", "kestrel/persist", "kestrel/config"}
	for _, imp := range pkg.Imports {
		for _, f := range forbidden {
			if imp == f || strings.HasPrefix(imp, f+"/") {
				t.Errorf("engine imports %q, which breaks the layering rule in ADR-017", imp)
			}
		}
	}
}
