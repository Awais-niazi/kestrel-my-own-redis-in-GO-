package engine

import "math/bits"

// dict is the keyspace's hash table: chained buckets, power-of-two sizing,
// incremental rehash, and a cursor that survives both.
//
// # Why not Go's map
//
// Go's map has no stable iteration order, and no way to ask for one. That
// makes the reference SCAN cursor impossible to implement on top of it
// (ADR-004), which is why the shipped cursor was a shard index and why COUNT
// bounded the reply from below rather than above. Closing that is the reason
// this type exists. It also removes the rehash pause ADR-004 flags as R6,
// and makes the memory estimate exact rather than a guess at the runtime's
// overhead.
//
// # Why chaining
//
// The scan cursor dictates the structure. Growing from 2^k to 2^(k+1)
// buckets splits each bucket's contents into exactly two: i and i + 2^k,
// decided by one bit of the hash. Nothing else moves. Reverse-binary
// increment (see scan) visits buckets in an order that exploits that, so a
// key present for the whole iteration is returned at least once however many
// times the table resized underneath it.
//
// Open addressing would be denser and friendlier to the cache, and would
// break that property completely: a probe sequence moves entries to slots
// the cursor may already have passed.
//
// # Why the head of each chain sits in the bucket
//
// The first version held an int32 index per bucket and kept every entry in a
// side array, which made the bucket array pointer-free. That array is also a
// second dependent load on the way to a key, and at a million keys it is a
// second cache miss.
//
// Storing the first entry of each chain inline removes that load for the
// roughly two thirds of keys that are the head of their chain. Measured by
// BenchmarkDictGetBigger and BenchmarkMapGetBigger, a million keys in random
// order:
//
//	indexed buckets   755 ns/op
//	Go's map          725 ns/op
//	inline heads      669 ns/op
//
// So the layout is what turns this from slightly slower than the map into
// about 8% faster, and it does that while the map hashes the key a second
// time that this table does not need. At a hundred thousand keys, where the
// working set is small enough that the extra load is usually a cache hit,
// the two are level.
//
// The cost is that the bucket array holds entries rather than indices, so an
// empty bucket costs 32 bytes rather than 4 and the array is traced by the
// collector. That is the trade, and it is the right way round on the hottest
// path in the server.
//
// # Concurrency
//
// None. A dict is guarded by its shard's mutex, exactly as the map it
// replaces was.

// Sizing. The table grows when it is full and shrinks when it is nearly
// empty, both by factors of two, so bucket indices stay related by one bit.
const (
	dictMinBuckets = 8
	// dictShrinkAt is the reciprocal of the load factor below which the
	// table halves. A gap this wide between the grow and shrink points is
	// what stops a key count oscillating around a boundary from rehashing on
	// every other operation.
	dictShrinkAt = 8
)

// Chain link values. A link is an index into the overflow arena, or one of
// these.
const (
	// noEntry ends a chain.
	noEntry = -1
	// noHead marks a bucket with nothing in it. It is distinct from noEntry
	// because a bucket holds its first entry inline: "empty" and "one entry
	// with no overflow" are different states of the same slot.
	noHead = -2
)

// entry is one key: 32 bytes holding the hash, the chain link, and the key
// and value themselves.
type entry struct {
	// hash is the high 32 bits of the key's 64-bit hash. The low bits pick
	// the shard, so the two uses never share bits and a key's position in
	// its shard is independent of which shard it landed in.
	hash uint32
	// next links to the rest of this chain in the overflow arena, or is
	// noEntry / noHead.
	next int32
	key  string
	val  *Object
}

// table is one hash table. A dict holds two of them while rehashing.
type table struct {
	// heads holds the first entry of every bucket, inline.
	heads []entry
	// over holds every entry after the first. It is kept dense: removing one
	// moves the last into its place, so len(over) is exactly the overflow
	// count and the arena never accumulates holes.
	over []entry
	mask uint32
	// live is the number of keys, which heads cannot supply because it has
	// gaps.
	live int
}

func newTable(buckets int) table {
	h := make([]entry, buckets)
	for i := range h {
		h[i].next = noHead
	}
	return table{
		heads: h,
		// Overflow runs at roughly a third of the bucket count at a load
		// factor of one, by the usual Poisson argument.
		over: make([]entry, 0, buckets/2),
		mask: uint32(buckets - 1),
	}
}

func (t *table) size() int { return len(t.heads) }
func (t *table) used() int { return t.live }

// bucketOf is where a hash belongs in this table.
func (t *table) bucketOf(hash uint32) uint32 { return hash & t.mask }

// dict is a hash table with an incremental rehash.
type dict struct {
	// tab[0] is the live table. tab[1] exists only while rehashing and is
	// where new keys go; lookups consult both.
	tab [2]table
	// rehashAt is the next bucket of tab[0] to migrate, or -1 when idle.
	rehashAt int
}

func newDict() *dict {
	d := &dict{rehashAt: -1}
	d.tab[0] = newTable(dictMinBuckets)
	return d
}

func (d *dict) rehashing() bool { return d.rehashAt >= 0 }

// len is the number of live keys.
func (d *dict) len() int {
	n := d.tab[0].live
	if d.rehashing() {
		n += d.tab[1].live
	}
	return n
}

// get returns the object stored under key, or nil.
//
// key is not converted to a string: comparing a string field against
// string(b) is recognised by the compiler and does not allocate, which is
// what keeps GET on the zero-allocation path.
func (d *dict) get(key []byte, hash uint32) *Object {
	// tab[0] always has buckets -- newDict allocates them and the end of a
	// rehash moves a real table into it -- so the common path needs no
	// emptiness check and no loop over the two tables. Only a rehash in
	// progress costs the second lookup.
	if v := d.tab[0].lookup(key, hash); v != nil {
		return v
	}
	if d.rehashing() {
		return d.tab[1].lookup(key, hash)
	}
	return nil
}

// lookup finds key in one table. It returns nil both for "absent" and for a
// nil value, which is sound because the keyspace never stores one: a key
// exists exactly when it has an object.
func (t *table) lookup(key []byte, hash uint32) *Object {
	h := &t.heads[hash&t.mask]
	if h.next == noHead {
		return nil
	}
	if h.hash == hash && h.key == string(key) {
		return h.val
	}
	over := t.over
	for e := h.next; e != noEntry; {
		en := &over[e]
		if en.hash == hash && en.key == string(key) {
			return en.val
		}
		e = en.next
	}
	return nil
}

// set stores val under key, reporting whether the key was new.
//
// An existing key keeps its position: the value is replaced where it sits,
// so a rewrite costs no rehash and cannot move the key past a live cursor.
func (d *dict) set(key string, hash uint32, val *Object) (added bool) {
	d.step()

	if d.tab[0].replace(key, hash, val) {
		return false
	}
	if d.rehashing() && d.tab[1].replace(key, hash, val) {
		return false
	}

	d.growIfFull()
	// New keys go into the table being migrated to, so the one being
	// migrated from only ever shrinks and the rehash is guaranteed to end.
	t := &d.tab[0]
	if d.rehashing() {
		t = &d.tab[1]
	}
	t.insert(key, hash, val)
	return true
}

// replace overwrites an existing key's value, reporting whether it found it.
func (t *table) replace(key string, hash uint32, val *Object) bool {
	if len(t.heads) == 0 {
		return false
	}
	h := &t.heads[hash&t.mask]
	if h.next == noHead {
		return false
	}
	if h.hash == hash && h.key == key {
		h.val = val
		return true
	}
	for e := h.next; e != noEntry; e = t.over[e].next {
		if t.over[e].hash == hash && t.over[e].key == key {
			t.over[e].val = val
			return true
		}
	}
	return false
}

// insert adds an entry known not to be present.
func (t *table) insert(key string, hash uint32, val *Object) {
	h := &t.heads[hash&t.mask]
	if h.next == noHead {
		*h = entry{hash: hash, next: noEntry, key: key, val: val}
	} else {
		t.over = append(t.over, entry{hash: hash, next: h.next, key: key, val: val})
		h.next = int32(len(t.over) - 1)
	}
	t.live++
}

// delete removes key, reporting whether it was there.
func (d *dict) delete(key []byte, hash uint32) bool {
	d.step()

	if d.tab[0].remove(key, hash) || (d.rehashing() && d.tab[1].remove(key, hash)) {
		d.shrinkIfSparse()
		return true
	}
	return false
}

// remove unlinks key from t.
//
// Deleting the head of a chain promotes the first overflow entry into the
// bucket rather than leaving an indirection behind, so the fast path stays
// fast for whatever is left.
func (t *table) remove(key []byte, hash uint32) bool {
	if len(t.heads) == 0 {
		return false
	}
	h := &t.heads[hash&t.mask]
	if h.next == noHead {
		return false
	}
	if h.hash == hash && h.key == string(key) {
		if h.next == noEntry {
			// Cleared rather than merely marked, so the collector is not
			// left holding the key and its value through an empty bucket.
			*h = entry{next: noHead}
		} else {
			i := h.next
			*h = t.over[i]
			t.dropOverflow(i)
		}
		t.live--
		return true
	}

	prev := h
	for e := h.next; e != noEntry; {
		en := &t.over[e]
		if en.hash == hash && en.key == string(key) {
			// Unlinked before the arena is compacted, so the repointing walk
			// below cannot find this entry. prev may itself be the entry
			// that compaction moves, which is safe because the write happens
			// first and is carried along by the move.
			prev.next = en.next
			t.dropOverflow(e)
			t.live--
			return true
		}
		prev = en
		e = en.next
	}
	return false
}

// dropOverflow removes slot i from the overflow arena by moving the last
// entry into it and repointing whatever referred to that last entry. i must
// already be unlinked from its chain.
func (t *table) dropOverflow(i int32) {
	last := int32(len(t.over) - 1)
	if i != last {
		moved := t.over[last]
		t.over[i] = moved
		h := &t.heads[moved.hash&t.mask]
		if h.next == last {
			h.next = i
		} else {
			for e := h.next; e != noEntry; e = t.over[e].next {
				if t.over[e].next == last {
					t.over[e].next = i
					break
				}
			}
		}
	}
	t.over[last] = entry{}
	t.over = t.over[:last]
}

// ---------------------------------------------------------------- resizing

func (d *dict) growIfFull() {
	if !d.rehashing() && d.tab[0].live >= d.tab[0].size() {
		d.resize(d.tab[0].size() * 2)
	}
}

func (d *dict) shrinkIfSparse() {
	if d.rehashing() {
		return
	}
	size := d.tab[0].size()
	if size > dictMinBuckets && d.tab[0].live*dictShrinkAt < size {
		d.resize(size / 2)
	}
}

// resize starts a rehash into a table of n buckets, reporting whether it
// started one. Nothing is moved here: the work is spread over the operations
// that follow.
//
// A rehash already in progress is left alone rather than replaced. Replacing
// it would discard every entry already migrated into tab[1] and silently
// lose those keys, and the loss would not surface until something went
// looking for one of them.
func (d *dict) resize(n int) bool {
	if d.rehashing() {
		return false
	}
	if n < dictMinBuckets {
		n = dictMinBuckets
	}
	d.tab[1] = newTable(n)
	d.rehashAt = 0
	return true
}

// step migrates a little of the rehash, and is called by every operation
// that can afford it.
//
// One non-empty bucket per operation is enough to finish a rehash well
// before the table needs the next one, because the table that keys are
// leaving never grows.
func (d *dict) step() {
	if d.rehashing() {
		d.rehashBuckets(1)
	}
}

// rehashBuckets migrates up to n non-empty buckets, reporting whether the
// rehash is still running.
//
// The empty-bucket budget stops a long run of empty buckets turning one step
// into a walk of the whole table, which is the pause this exists to avoid.
func (d *dict) rehashBuckets(n int) bool {
	if !d.rehashing() {
		return false
	}
	empty := n * 10
	src := &d.tab[0]
	for n > 0 && d.rehashAt < src.size() {
		b := uint32(d.rehashAt)
		if src.heads[b].next == noHead {
			d.rehashAt++
			if empty--; empty == 0 {
				return true
			}
			continue
		}
		// Taken from the head each time, which is the O(1) removal.
		for {
			h := &src.heads[b]
			if h.next == noHead {
				break
			}
			key, hash, val := h.key, h.hash, h.val
			if h.next == noEntry {
				*h = entry{next: noHead}
			} else {
				i := h.next
				*h = src.over[i]
				src.dropOverflow(i)
			}
			src.live--
			d.tab[1].insert(key, hash, val)
		}
		d.rehashAt++
		n--
	}
	if d.rehashAt >= src.size() {
		d.tab[0] = d.tab[1]
		d.tab[1] = table{}
		d.rehashAt = -1
		return false
	}
	return true
}

// rehashSome advances a paused rehash, for the background cycle to call. It
// reports whether more work remains.
func (d *dict) rehashSome(buckets int) bool { return d.rehashBuckets(buckets) }

// ---------------------------------------------------------------- scanning

// scan visits one bucket's worth of keys and returns the cursor to resume
// from, or 0 when the iteration is complete.
//
// The cursor is the bucket index with its bits reversed, incremented, and
// reversed back. Counting that way means the bits that decide where an entry
// lands after a resize are the ones that change last, so a table that
// doubles or halves between calls cannot move an entry from a bucket the
// cursor has yet to reach into one it has already passed. An entry present
// for the whole iteration is therefore returned at least once; one that
// arrives or leaves midway may be returned or missed, and one already
// visited may be returned again after a resize. That is exactly the
// guarantee the reference implementation gives.
//
// While rehashing, the same cursor is applied to both tables: the smaller
// table's bucket is visited once, then every bucket of the larger table that
// the smaller one's index expands into.
func (d *dict) scan(cursor uint64, fn func(key string, val *Object)) uint64 {
	if d.len() == 0 {
		return 0
	}
	if !d.rehashing() {
		t := &d.tab[0]
		m := uint64(t.mask)
		t.visit(uint32(cursor&m), fn)
		return nextCursor(cursor, m)
	}

	// Order the tables by size: each bucket of the smaller corresponds to a
	// set of buckets in the larger.
	small, large := &d.tab[0], &d.tab[1]
	if small.size() > large.size() {
		small, large = large, small
	}
	m0, m1 := uint64(small.mask), uint64(large.mask)

	small.visit(uint32(cursor&m0), fn)
	for {
		large.visit(uint32(cursor&m1), fn)
		cursor = nextCursor(cursor, m1)
		if cursor&(m0^m1) == 0 {
			break
		}
	}
	return cursor & m1
}

// visit calls fn for every entry in one bucket.
func (t *table) visit(b uint32, fn func(key string, val *Object)) {
	if len(t.heads) == 0 {
		return
	}
	h := &t.heads[b]
	if h.next == noHead {
		return
	}
	fn(h.key, h.val)
	for e := h.next; e != noEntry; e = t.over[e].next {
		fn(t.over[e].key, t.over[e].val)
	}
}

// nextCursor advances a reverse-binary cursor within mask.
//
// The bits above the mask are set before the increment and cleared by the
// masking that follows, so the carry propagates from the top of the bucket
// index downwards rather than from the bottom up.
func nextCursor(v, mask uint64) uint64 {
	v |= ^mask
	v = bits.Reverse64(v)
	v++
	v = bits.Reverse64(v)
	return v & mask
}

// forEach visits every key, in no useful order, and stops early if fn
// returns false.
//
// It does not step the rehash: a caller iterating the whole table must not
// have entries move underneath it.
func (d *dict) forEach(fn func(key string, val *Object) bool) {
	for i := range d.tab {
		t := &d.tab[i]
		for j := range t.heads {
			if t.heads[j].next != noHead && !fn(t.heads[j].key, t.heads[j].val) {
				return
			}
		}
		for j := range t.over {
			if !fn(t.over[j].key, t.over[j].val) {
				return
			}
		}
		if !d.rehashing() {
			return
		}
	}
}

// ---------------------------------------------------------------- sampling

// randomEntry returns a key chosen by picking a random bucket and then a
// random member of its chain. It reports false when it found nothing.
//
// rnd is supplied so the caller owns the source of randomness and a test can
// make the choice deterministic.
//
// The choice is not uniform over keys: one sharing a bucket with others is
// less likely to be drawn than one sitting alone. That is the same bias the
// reference implementation's sampling has, it stays small because the load
// factor is held near one, and what samples this -- eviction and the expiry
// cycle -- draws several candidates and compares them rather than trusting
// any single draw.
func (d *dict) randomEntry(rnd func(n int) int) (string, *Object, bool) {
	if d.len() == 0 {
		return "", nil, false
	}
	// Bounded, so that an unlucky run of empty buckets cannot become an
	// unbounded loop. The caller treats a miss as a sample that found
	// nothing, which is an honest answer for a sparse table.
	for try := 0; try < 100; try++ {
		t := &d.tab[0]
		if d.rehashing() && rnd(d.len()) >= t.live {
			t = &d.tab[1]
		}
		if t.size() == 0 || t.live == 0 {
			continue
		}
		h := &t.heads[rnd(t.size())]
		if h.next == noHead {
			continue
		}
		n := 1
		for e := h.next; e != noEntry; e = t.over[e].next {
			n++
		}
		pick := rnd(n)
		if pick == 0 {
			return h.key, h.val, true
		}
		for e := h.next; e != noEntry; e = t.over[e].next {
			if pick--; pick == 0 {
				return t.over[e].key, t.over[e].val, true
			}
		}
	}
	return "", nil, false
}

// ------------------------------------------------------------------ memory

// dictEntrySize is what one slot costs: hash(4) + next(4) + string
// header(16) + pointer(8). It excludes the key's own bytes and the object.
const dictEntrySize = 32

// overhead is the dict's own cost in bytes: the bucket arrays and the
// capacity of the overflow arenas.
//
// Unlike the runtime's map this is exact rather than estimated, because
// every allocation belongs to the table and none of it is hidden.
func (d *dict) overhead() int64 {
	var n int64
	for i := range d.tab {
		t := &d.tab[i]
		n += int64(len(t.heads)+cap(t.over)) * dictEntrySize
	}
	return n
}
