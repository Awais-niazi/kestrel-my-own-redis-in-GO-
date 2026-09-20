package engine

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/cespare/xxhash/v2"
)

// testHash mirrors what the keyspace will use: the low 32 bits of the key
// hash choose the shard, the high 32 the bucket inside it.
func testHash(key string) uint32 { return uint32(xxhash.Sum64String(key) >> 32) }

func dictSet(d *dict, key string, val *Object) bool {
	return d.set(key, testHash(key), val)
}

func dictGet(d *dict, key string) *Object {
	return d.get([]byte(key), testHash(key))
}

func dictDel(d *dict, key string) bool {
	return d.delete([]byte(key), testHash(key))
}

func obj(s string) *Object { return &Object{Type: TypeString, Value: []byte(s)} }

// finishRehash drains any rehash left running by the inserts above, so a
// test that drives a rehash by hand starts from an idle table.
func finishRehash(d *dict) {
	for d.rehashing() {
		d.rehashSome(64)
	}
}

// checkDict asserts the structural invariants every operation must preserve.
//
// They are checked rather than assumed because the arena is compacted on
// delete: an entry is moved and a link repointed, and a link left pointing
// at the vacated slot is a corruption that a get would not necessarily
// notice -- it would find the wrong key, or walk off the end, depending on
// what landed there next.
func checkDict(t *testing.T, d *dict, want map[string]*Object) {
	t.Helper()

	seen := map[string]bool{}
	for i := range d.tab {
		tb := &d.tab[i]
		if tb.size() == 0 {
			continue
		}
		live, fromOverflow := 0, 0
		for b := range tb.heads {
			h := &tb.heads[b]
			if h.next == noHead {
				if h.key != "" || h.val != nil {
					t.Fatalf("table %d bucket %d is empty but still holds %q",
						i, b, h.key)
				}
				continue
			}
			for e, first := int32(b), true; ; first = false {
				var en *entry
				if first {
					en = h
				} else {
					if int(e) >= len(tb.over) {
						t.Fatalf("table %d bucket %d: link to slot %d, overflow holds %d",
							i, b, e, len(tb.over))
					}
					en = &tb.over[e]
					fromOverflow++
				}
				if got := tb.bucketOf(en.hash); got != uint32(b) {
					t.Fatalf("table %d: key %q sits in bucket %d but hashes to %d",
						i, en.key, b, got)
				}
				if seen[en.key] {
					t.Fatalf("key %q is in the table twice", en.key)
				}
				seen[en.key] = true
				live++
				if en.next == noEntry {
					break
				}
				e = en.next
			}
		}
		// The overflow arena has no holes, so every slot must be reachable
		// from some chain. Anything less means dropOverflow lost a link.
		if fromOverflow != len(tb.over) {
			t.Fatalf("table %d: %d entries in the overflow arena but %d reachable",
				i, len(tb.over), fromOverflow)
		}
		if live != tb.live {
			t.Fatalf("table %d: %d keys reachable but live says %d", i, live, tb.live)
		}
	}

	if d.len() != len(want) {
		t.Fatalf("dict holds %d keys, want %d", d.len(), len(want))
	}
	for k, v := range want {
		got := dictGet(d, k)
		if got != v {
			t.Fatalf("key %q reads back as %v, want %v", k, got, v)
		}
	}
}

func TestDictHoldsWhatIsPutInIt(t *testing.T) {
	d := newDict()
	want := map[string]*Object{}

	for i := 0; i < 500; i++ {
		k := fmt.Sprintf("key%d", i)
		o := obj(k)
		if !dictSet(d, k, o) {
			t.Fatalf("%q reported as already present", k)
		}
		want[k] = o
	}
	checkDict(t, d, want)

	// Rewriting a key replaces the value and adds nothing.
	o := obj("replaced")
	if dictSet(d, "key7", o) {
		t.Error("rewriting an existing key reported it as new")
	}
	want["key7"] = o
	checkDict(t, d, want)

	for i := 0; i < 500; i += 2 {
		k := fmt.Sprintf("key%d", i)
		if !dictDel(d, k) {
			t.Fatalf("%q would not delete", k)
		}
		delete(want, k)
	}
	checkDict(t, d, want)

	if dictDel(d, "nothing here") {
		t.Error("deleting an absent key reported success")
	}
	if got := dictGet(d, "key0"); got != nil {
		t.Errorf("deleted key still reads back as %v", got)
	}
}

// TestDictSurvivesTotalHashCollision drives every key into one bucket.
//
// A table whose hash is useless must still be correct, only slow. This is
// also the case where compact does the most work, because every entry shares
// one chain.
func TestDictSurvivesTotalHashCollision(t *testing.T) {
	d := newDict()
	keys := make([]string, 200)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
		d.set(keys[i], 0, obj(keys[i]))
	}
	if d.len() != len(keys) {
		t.Fatalf("holds %d keys, want %d", d.len(), len(keys))
	}
	for _, k := range keys {
		if got := d.get([]byte(k), 0); got == nil {
			t.Fatalf("%q is missing from a fully collided table", k)
		}
	}
	for i, k := range keys {
		if !d.delete([]byte(k), 0) {
			t.Fatalf("%q would not delete", k)
		}
		if got, want := d.len(), len(keys)-i-1; got != want {
			t.Fatalf("after deleting %q the table holds %d, want %d", k, got, want)
		}
	}
}

// TestDictMatchesAMapUnderRandomOperations is the differential test: a random
// sequence of writes, rewrites and deletes against a plain map, with the
// structure checked as it goes.
func TestDictMatchesAMapUnderRandomOperations(t *testing.T) {
	d := newDict()
	want := map[string]*Object{}
	r := rand.New(rand.NewSource(7))

	for i := 0; i < 20000; i++ {
		k := fmt.Sprintf("k%d", r.Intn(400))
		switch r.Intn(3) {
		case 0, 1:
			o := obj(fmt.Sprintf("v%d", i))
			gotNew := dictSet(d, k, o)
			_, existed := want[k]
			if gotNew == existed {
				t.Fatalf("set %q reported new=%v but the key existed=%v", k, gotNew, existed)
			}
			want[k] = o
		case 2:
			_, existed := want[k]
			if got := dictDel(d, k); got != existed {
				t.Fatalf("delete %q reported %v, want %v", k, got, existed)
			}
			delete(want, k)
		}
		if i%500 == 0 {
			checkDict(t, d, want)
		}
	}
	checkDict(t, d, want)
}

// ---------------------------------------------------------------- scanning

// scanAll runs a full iteration and returns every key it saw, with
// duplicates, plus the number of calls it took.
func scanAll(d *dict, between func()) ([]string, int) {
	var out []string
	var calls int
	var cursor uint64
	for {
		cursor = d.scan(cursor, func(key string, val *Object) {
			out = append(out, key)
		})
		calls++
		if between != nil {
			between()
		}
		if cursor == 0 {
			return out, calls
		}
	}
}

func TestDictScanVisitsEveryKeyExactlyOnceWhenNothingChanges(t *testing.T) {
	for _, n := range []int{0, 1, 7, 64, 1000} {
		d := newDict()
		want := map[string]bool{}
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("key%d", i)
			dictSet(d, k, obj(k))
			want[k] = true
		}

		got, _ := scanAll(d, nil)
		if len(got) != n {
			t.Errorf("%d keys: scan returned %d, want %d with no duplicates", n, len(got), n)
		}
		seen := map[string]bool{}
		for _, k := range got {
			if seen[k] {
				t.Errorf("%d keys: scan returned %q twice on a table that never resized", n, k)
			}
			seen[k] = true
		}
		for k := range want {
			if !seen[k] {
				t.Errorf("%d keys: scan missed %q", n, k)
			}
		}
	}
}

// TestDictScanReturnsEveryKeyThatStaysPut is the guarantee the whole
// structure exists for.
//
// The table is grown and shrunk repeatedly in the middle of an iteration. A
// key present for the whole scan must come back at least once however many
// times the buckets moved underneath the cursor; duplicates are allowed,
// which is what the reference implementation promises too.
func TestDictScanReturnsEveryKeyThatStaysPut(t *testing.T) {
	d := newDict()
	const stable = 300
	for i := 0; i < stable; i++ {
		k := fmt.Sprintf("stay%d", i)
		dictSet(d, k, obj(k))
	}

	// Between every call, churn enough transient keys to force the table
	// through a grow and later a shrink.
	churn := 0
	grow := true
	between := func() {
		if grow {
			for i := 0; i < 40; i++ {
				k := fmt.Sprintf("tmp%d", churn)
				dictSet(d, k, obj(k))
				churn++
			}
			if churn > 3000 {
				grow = false
			}
			return
		}
		for i := 0; i < 40 && churn > 0; i++ {
			churn--
			dictDel(d, fmt.Sprintf("tmp%d", churn))
		}
	}

	got, calls := scanAll(d, between)
	seen := map[string]bool{}
	for _, k := range got {
		seen[k] = true
	}
	for i := 0; i < stable; i++ {
		k := fmt.Sprintf("stay%d", i)
		if !seen[k] {
			t.Fatalf("%q was present for the whole scan and was not returned "+
				"(%d calls, %d keys seen)", k, calls, len(got))
		}
	}
	if calls < 2 {
		t.Fatalf("the scan finished in %d call(s); it cannot have resized", calls)
	}
}

// TestDictScanTerminatesFromAnyCursor checks that a client cannot wedge a
// scan by sending a cursor the server never issued.
func TestDictScanTerminatesFromAnyCursor(t *testing.T) {
	d := newDict()
	for i := 0; i < 200; i++ {
		k := fmt.Sprintf("key%d", i)
		dictSet(d, k, obj(k))
	}
	for _, start := range []uint64{1, 3, 17, 255, 1 << 20, ^uint64(0)} {
		cursor := start
		for calls := 0; ; calls++ {
			cursor = d.scan(cursor, func(string, *Object) {})
			if cursor == 0 {
				break
			}
			if calls > 5000 {
				t.Fatalf("a scan started at %d did not finish in 5000 calls", start)
			}
		}
	}
}

func TestDictScanWhileRehashingCoversBothTables(t *testing.T) {
	d := newDict()
	const n = 500
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("key%d", i)
		dictSet(d, k, obj(k))
	}
	// Force a rehash and leave it half done, so the scan has to walk a
	// small table and a large one together.
	finishRehash(d)
	if !d.resize(d.tab[0].size() * 4) {
		t.Fatal("resize refused to start")
	}
	d.rehashBuckets(d.tab[0].size() / 2)
	if !d.rehashing() {
		t.Fatal("the table finished rehashing; this test needs it in progress")
	}

	got, _ := scanAll(d, nil)
	seen := map[string]bool{}
	for _, k := range got {
		seen[k] = true
	}
	for i := 0; i < n; i++ {
		if k := fmt.Sprintf("key%d", i); !seen[k] {
			t.Fatalf("%q was missed by a scan that ran across a rehash", k)
		}
	}
}

// ---------------------------------------------------------------- rehashing

// TestRehashIsIncremental checks that no single operation pays for the whole
// table, which is the pause ADR-004 flags as R6.
func TestRehashIsIncremental(t *testing.T) {
	d := newDict()
	for i := 0; i < 1000; i++ {
		k := fmt.Sprintf("key%d", i)
		dictSet(d, k, obj(k))
	}
	finishRehash(d)
	if !d.resize(d.tab[0].size() * 2) {
		t.Fatal("resize refused to start")
	}
	moved := d.tab[1].used()
	if moved != 0 {
		t.Fatalf("resize moved %d entries itself; it should move none", moved)
	}

	// One step must move something, and must not move everything.
	d.step()
	after := d.tab[1].used()
	if after == 0 {
		t.Error("a rehash step moved nothing")
	}
	if !d.rehashing() {
		t.Fatalf("one step finished the whole rehash of %d keys", after)
	}
}

func TestRehashFinishesAndKeepsEveryKey(t *testing.T) {
	d := newDict()
	want := map[string]*Object{}
	for i := 0; i < 2000; i++ {
		k := fmt.Sprintf("key%d", i)
		o := obj(k)
		dictSet(d, k, o)
		want[k] = o
	}
	finishRehash(d)
	d.resize(d.tab[0].size() * 2)
	for i := 0; d.rehashing(); i++ {
		if i > 100000 {
			t.Fatal("the rehash did not finish")
		}
		d.rehashSome(1)
	}
	checkDict(t, d, want)
}

func TestDictShrinksWhenItEmptiesOut(t *testing.T) {
	d := newDict()
	for i := 0; i < 4000; i++ {
		k := fmt.Sprintf("key%d", i)
		dictSet(d, k, obj(k))
	}
	grown := d.tab[0].size()
	if grown <= dictMinBuckets {
		t.Fatalf("the table never grew past %d buckets", dictMinBuckets)
	}
	for i := 0; i < 4000; i++ {
		dictDel(d, fmt.Sprintf("key%d", i))
	}
	for d.rehashing() {
		d.rehashSome(64)
	}
	if got := d.tab[0].size(); got >= grown {
		t.Errorf("the table held %d buckets at its peak and %d when empty; "+
			"it never shrank", grown, got)
	}
	if got := d.len(); got != 0 {
		t.Errorf("the emptied table reports %d keys", got)
	}
}

// ---------------------------------------------------------------- sampling

// TestDictSamplingReachesEveryKey checks that randomEntry can draw any key,
// including one that has been moved into a bucket head by a delete.
//
// The point is coverage, not distribution: what samples this compares
// several candidates, so what matters is that no key is unreachable.
func TestDictSamplingReachesEveryKey(t *testing.T) {
	d := newDict()
	const n = 200
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("key%d", i)
		dictSet(d, k, obj(k))
	}
	// Delete half, so chains have been unlinked and heads promoted.
	want := map[string]bool{}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("key%d", i)
		if i%2 == 0 {
			dictDel(d, k)
			continue
		}
		want[k] = true
	}

	r := rand.New(rand.NewSource(3))
	seen := map[string]bool{}
	for i := 0; i < 200000 && len(seen) < len(want); i++ {
		k, v, ok := d.randomEntry(r.Intn)
		if !ok {
			continue
		}
		if v == nil {
			t.Fatalf("sampling returned %q with a nil object", k)
		}
		if !want[k] {
			t.Fatalf("sampling returned %q, which was deleted", k)
		}
		seen[k] = true
	}
	if len(seen) != len(want) {
		t.Errorf("sampling reached %d of %d keys; some are unreachable", len(seen), len(want))
	}
	if _, _, ok := newDict().randomEntry(r.Intn); ok {
		t.Error("sampling an empty dict returned a key")
	}
}

func TestDictForEachVisitsEverything(t *testing.T) {
	d := newDict()
	want := []string{}
	for i := 0; i < 300; i++ {
		k := fmt.Sprintf("key%d", i)
		dictSet(d, k, obj(k))
		want = append(want, k)
	}
	finishRehash(d)
	if !d.resize(d.tab[0].size() * 2) {
		t.Fatal("resize refused to start")
	}
	d.rehashBuckets(4) // leave it mid-rehash

	var got []string
	d.forEach(func(key string, _ *Object) bool {
		got = append(got, key)
		return true
	})
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("forEach saw %d keys, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("forEach saw %q where %q was expected", got[i], want[i])
		}
	}

	// Returning false stops it.
	n := 0
	d.forEach(func(string, *Object) bool { n++; return n < 5 })
	if n != 5 {
		t.Errorf("forEach ran %d times after being asked to stop at 5", n)
	}
}

// ------------------------------------------------------------------ memory

func TestDictOverheadTracksTheTable(t *testing.T) {
	d := newDict()
	empty := d.overhead()
	if empty <= 0 {
		t.Fatalf("an empty dict reports %d bytes of overhead", empty)
	}
	for i := 0; i < 1000; i++ {
		k := fmt.Sprintf("key%d", i)
		dictSet(d, k, obj(k))
	}
	full := d.overhead()
	if full <= empty {
		t.Errorf("overhead did not grow with the table: %d then %d", empty, full)
	}
	// It must be in the right order of magnitude: a thousand keys cost a
	// thousand slots plus the spare ones, not a thousand times that.
	if perKey := full / 1000; perKey < dictEntrySize || perKey > 4*dictEntrySize {
		t.Errorf("overhead is %d bytes per key, which is not the table plus its overflow", perKey)
	}
}

// ------------------------------------------------------------------- fuzz

// FuzzDict drives a dict and a map through the same operation stream and
// requires them to agree, with the structure checked at the end.
func FuzzDict(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{1, 1, 1, 1, 2, 2, 2, 2})

	f.Fuzz(func(t *testing.T, ops []byte) {
		d := newDict()
		want := map[string]*Object{}
		for i := 0; i+1 < len(ops); i += 2 {
			k := fmt.Sprintf("k%d", ops[i+1])
			switch ops[i] % 3 {
			case 0, 1:
				o := obj(k)
				dictSet(d, k, o)
				want[k] = o
			case 2:
				dictDel(d, k)
				delete(want, k)
			}
		}
		if d.len() != len(want) {
			t.Fatalf("dict holds %d keys, map holds %d", d.len(), len(want))
		}
		for k, v := range want {
			if got := dictGet(d, k); got != v {
				t.Fatalf("key %q reads back as %v, want %v", k, got, v)
			}
		}
		// A full scan must reach everything a stable table holds.
		seen := map[string]bool{}
		var cursor uint64
		for calls := 0; ; calls++ {
			cursor = d.scan(cursor, func(key string, _ *Object) { seen[key] = true })
			if cursor == 0 {
				break
			}
			if calls > 100000 {
				t.Fatal("scan did not terminate")
			}
		}
		for k := range want {
			if !seen[k] {
				t.Fatalf("scan missed %q on a table that did not change", k)
			}
		}
	})
}

// -------------------------------------------------------------- benchmarks

func benchKeys(n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = fmt.Appendf(nil, "key:%d", i)
	}
	return keys
}

// The two lookup benchmarks measure the same work: take a key, find its
// object. Both pay the xxhash, because the keyspace needs it to pick the
// shard whichever table sits underneath; the map then hashes the key a
// second time internally, which is the cost this structure removes. Access
// is in random order, since walking keys in insertion order flatters
// whichever layout happens to be contiguous.

func BenchmarkDictGet(b *testing.B) {
	const n = 100000
	keys := benchKeys(n)
	d := newDict()
	for _, k := range keys {
		d.set(string(k), uint32(xxhash.Sum64(k)>>32), obj("v"))
	}
	order := rand.New(rand.NewSource(1)).Perm(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := keys[order[i%n]]
		h := xxhash.Sum64(k)
		_ = h & 15 // the shard index, as the keyspace would take it
		if d.get(k, uint32(h>>32)) == nil {
			b.Fatal("missing key")
		}
	}
}

func BenchmarkMapGet(b *testing.B) {
	const n = 100000
	keys := benchKeys(n)
	m := make(map[string]*Object, n)
	for _, k := range keys {
		m[string(k)] = obj("v")
	}
	order := rand.New(rand.NewSource(1)).Perm(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := keys[order[i%n]]
		h := xxhash.Sum64(k)
		_ = h & 15 // the shard index, as the keyspace would take it
		if m[string(k)] == nil {
			b.Fatal("missing key")
		}
	}
}

func BenchmarkDictSet(b *testing.B) {
	keys := benchKeys(1000)
	hashes := make([]uint32, len(keys))
	strs := make([]string, len(keys))
	for i, k := range keys {
		hashes[i] = uint32(xxhash.Sum64(k) >> 32)
		strs[i] = string(k)
	}
	d := newDict()
	o := obj("v")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(keys)
		d.set(strs[j], hashes[j], o)
	}
}

// BenchmarkDictScanFullIteration is the cost of one complete SCAN over a
// hundred thousand keys, which is what a client paginating the keyspace
// pays in total.
func BenchmarkDictScanFullIteration(b *testing.B) {
	const n = 100000
	d := newDict()
	for _, k := range benchKeys(n) {
		d.set(string(k), uint32(xxhash.Sum64(k)>>32), obj("v"))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var cursor uint64
		seen := 0
		for {
			cursor = d.scan(cursor, func(string, *Object) { seen++ })
			if cursor == 0 {
				break
			}
		}
		if seen != n {
			b.Fatalf("saw %d keys, want %d", seen, n)
		}
	}
}

// BenchmarkDictGetBigger and BenchmarkMapGetBigger repeat the comparison at
// a size where nothing fits in cache, which is where the layout decision was
// made: the design comment in dict.go quotes these two numbers.
func BenchmarkDictGetBigger(b *testing.B) {
	const n = 1000000
	keys := benchKeys(n)
	d := newDict()
	for _, k := range keys {
		d.set(string(k), uint32(xxhash.Sum64(k)>>32), obj("v"))
	}
	for d.rehashing() {
		d.rehashSome(256)
	}
	order := rand.New(rand.NewSource(1)).Perm(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := keys[order[i%n]]
		h := xxhash.Sum64(k)
		_ = h & 15
		if d.get(k, uint32(h>>32)) == nil {
			b.Fatal("missing key")
		}
	}
}

func BenchmarkMapGetBigger(b *testing.B) {
	const n = 1000000
	keys := benchKeys(n)
	m := make(map[string]*Object, n)
	for _, k := range keys {
		m[string(k)] = obj("v")
	}
	order := rand.New(rand.NewSource(1)).Perm(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := keys[order[i%n]]
		h := xxhash.Sum64(k)
		_ = h & 15
		if m[string(k)] == nil {
			b.Fatal("missing key")
		}
	}
}
