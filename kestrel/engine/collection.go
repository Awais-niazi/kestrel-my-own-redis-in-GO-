package engine

import "sync/atomic"

// Thresholds decide when a collection is promoted from its compact encoding
// to its full one (ADR-006). They are read on every write that could cross a
// boundary, and CONFIG SET can change them at runtime, so they live behind an
// atomic pointer rather than being copied into each object.
//
// Promotion is one-way. A collection that grows past a threshold and shrinks
// back keeps the full encoding, because a workload oscillating around the
// boundary would otherwise turn an O(1) operation into a repeated O(n)
// rebuild.
type Thresholds struct {
	HashMaxListpackEntries int
	HashMaxListpackValue   int
	ListMaxListpackSize    int
	ListMaxListpackValue   int
	SetMaxIntsetEntries    int
	SetMaxListpackEntries  int
	SetMaxListpackValue    int
	ZSetMaxListpackEntries int
	ZSetMaxListpackValue   int
}

// DefaultThresholds matches Appendix B of the PRD.
func DefaultThresholds() Thresholds {
	return Thresholds{
		HashMaxListpackEntries: 128,
		HashMaxListpackValue:   64,
		ListMaxListpackSize:    128,
		ListMaxListpackValue:   64,
		SetMaxIntsetEntries:    512,
		SetMaxListpackEntries:  128,
		SetMaxListpackValue:    64,
		ZSetMaxListpackEntries: 128,
		ZSetMaxListpackValue:   64,
	}
}

func (t *Thresholds) normalize() {
	d := DefaultThresholds()
	for _, f := range []struct {
		p   *int
		def int
	}{
		{&t.HashMaxListpackEntries, d.HashMaxListpackEntries},
		{&t.HashMaxListpackValue, d.HashMaxListpackValue},
		{&t.ListMaxListpackSize, d.ListMaxListpackSize},
		{&t.ListMaxListpackValue, d.ListMaxListpackValue},
		{&t.SetMaxIntsetEntries, d.SetMaxIntsetEntries},
		{&t.SetMaxListpackEntries, d.SetMaxListpackEntries},
		{&t.SetMaxListpackValue, d.SetMaxListpackValue},
		{&t.ZSetMaxListpackEntries, d.ZSetMaxListpackEntries},
		{&t.ZSetMaxListpackValue, d.ZSetMaxListpackValue},
	} {
		if *f.p <= 0 {
			*f.p = f.def
		}
	}
}

// SetThresholds replaces the encoding thresholds. The server calls it at
// startup and again after a CONFIG SET that touches one of them.
func (ks *Keyspace) SetThresholds(t Thresholds) {
	t.normalize()
	ks.limits.Store(&t)
}

// Thresholds returns the current encoding thresholds.
func (ks *Keyspace) Thresholds() *Thresholds { return ks.limits.Load() }

// collectionValue is implemented by every non-scalar stored value. The
// engine uses it for memory accounting, COPY, and OBJECT ENCODING without
// switching on the concrete type.
type collectionValue interface {
	Len() int
	Encoding() Encoding
	EstimatedSize() int64
	Clone() any
}

var _ = []collectionValue(nil)

// collectionAt returns the collection stored at key.
//
// It reports ErrWrongType when the key holds a different type, and a nil
// object when the key is absent, which every collection read treats as an
// empty collection.
func (db *DB) collectionAt(s *shard, key []byte, t ObjectType) (*Object, error) {
	return db.lookupType(s, key, t)
}

// collectionRead is collectionAt for a command that only reads: it hides an
// expired key instead of reaping it, because it holds no propagation lock.
// Each type has a thin wrapper that unpacks the value -- listRead, setRead,
// zsetRead, hashRead -- paired with the listAt, setAt, zsetAt and hashAt the
// writes use. Choosing the wrong one of a pair is what
// SetWriteOrderingAssertions exists to catch.
func (db *DB) collectionRead(s *shard, key []byte, t ObjectType) (*Object, error) {
	return db.peekType(s, key, t)
}

// finishWrite updates a collection object's encoding, its contribution to the
// memory estimate, and removes the key entirely when nothing is left.
//
// Redis deletes a collection key the moment its last element goes, and code
// everywhere relies on "exists" and "is non-empty" being the same question,
// so the emptiness check belongs here rather than at each call site.
func (db *DB) finishWrite(s *shard, key []byte, o *Object, c collectionValue) {
	k := string(key)
	if c.Len() == 0 {
		db.removeLocked(s, k, o)
		db.touched(key)
		return
	}
	prev := objectSize(k, o)
	o.Encoding = c.Encoding()
	s.memory += objectSize(k, o) - prev
	s.changes++
	db.touched(key)
}

// newCollection installs a new collection object at key.
func (db *DB) newCollection(s *shard, key []byte, t ObjectType, c collectionValue) *Object {
	o := &Object{Type: t, Encoding: c.Encoding(), Value: c}
	db.store(s, key, o)
	return o
}

// limitsHolder is embedded in Keyspace; kept separate so the field's type is
// obvious at the use site.
type limitsHolder = atomic.Pointer[Thresholds]

// SortSource returns the elements of a list, set or sorted set as a flat
// slice, which is what SORT operates on. A sorted set contributes its
// members in score order.
//
// SORT is a write -- it replays from its own arguments, so a replay re-reads
// this key and must see what the original run saw -- while SORT_RO is not.
// The caller says which it is.
func (db *DB) SortSource(key []byte, a Access) ([][]byte, bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o := db.get(s, key, a)
	if o == nil {
		return nil, false, nil
	}
	switch o.Type {
	case TypeList:
		return o.Value.(*List).Range(0, -1), true, nil
	case TypeSet:
		return o.Value.(*Set).Members(), true, nil
	case TypeZSet:
		members := o.Value.(*ZSet).All()
		out := make([][]byte, len(members))
		for i, m := range members {
			out[i] = m.Member
		}
		return out, true, nil
	default:
		return nil, false, ErrWrongType
	}
}

// StoreList replaces a key with a list of the given elements, returning its
// length. An empty element list deletes the key. It is what SORT ... STORE
// and the future list-producing commands write through.
func (db *DB) StoreList(key []byte, elements [][]byte) int64 {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	if existing := db.lookup(s, key); existing != nil {
		db.removeLocked(s, string(key), existing)
	}
	if len(elements) == 0 {
		db.touched(key)
		return 0
	}
	l := newList()
	t := db.ks.Thresholds()
	for _, e := range elements {
		l.Push(e, false, t)
	}
	o := db.newCollection(s, key, TypeList, l)
	db.finishWrite(s, key, o, l)
	return int64(l.Len())
}
