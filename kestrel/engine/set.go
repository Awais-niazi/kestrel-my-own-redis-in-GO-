package engine

import (
	"math/rand"
	"sort"
	"strconv"
)

// intset is a sorted slice of distinct int64 values.
//
// A set whose members are all integers is extremely common -- id sets,
// bitmap-ish membership -- and holding them as eight bytes each in one
// contiguous slice costs a single pointer and gives binary-search lookup.
type intset struct{ v []int64 }

func (s *intset) Len() int { return len(s.v) }

func (s *intset) search(n int64) (int, bool) {
	i := sort.Search(len(s.v), func(i int) bool { return s.v[i] >= n })
	return i, i < len(s.v) && s.v[i] == n
}

func (s *intset) Contains(n int64) bool {
	_, ok := s.search(n)
	return ok
}

func (s *intset) Add(n int64) bool {
	i, found := s.search(n)
	if found {
		return false
	}
	s.v = append(s.v, 0)
	copy(s.v[i+1:], s.v[i:])
	s.v[i] = n
	return true
}

func (s *intset) Remove(n int64) bool {
	i, found := s.search(n)
	if !found {
		return false
	}
	s.v = append(s.v[:i], s.v[i+1:]...)
	return true
}

func (s *intset) EstimatedSize() int64 { return int64(sliceHeaderSize + 8*cap(s.v)) }

// Set is a collection of distinct byte strings.
//
// It has three encodings (ADR-006): an intset while every member is an
// integer, a listpack while the set is small, and a map once it outgrows
// either. Promotion is one-way, and the intset can promote to either of the
// other two depending on why it outgrew itself.
type Set struct {
	is *intset
	lp *listpack
	m  map[string]struct{}
}

func newSet(firstMember []byte, t *Thresholds) *Set {
	if _, ok := canonicalInt(firstMember); ok {
		return &Set{is: &intset{}}
	}
	if len(firstMember) > t.SetMaxListpackValue {
		return &Set{m: make(map[string]struct{})}
	}
	return &Set{lp: newListpack(0)}
}

// Len returns the member count.
func (s *Set) Len() int {
	switch {
	case s.m != nil:
		return len(s.m)
	case s.is != nil:
		return s.is.Len()
	default:
		return s.lp.Len()
	}
}

// Encoding reports the physical representation in use.
func (s *Set) Encoding() Encoding {
	switch {
	case s.m != nil:
		return EncodingHashtable
	case s.is != nil:
		return EncodingIntset
	default:
		return EncodingListpack
	}
}

// EstimatedSize reports the bytes attributable to this set.
func (s *Set) EstimatedSize() int64 {
	switch {
	case s.m != nil:
		n := int64(48)
		for k := range s.m {
			n += int64(mapEntryOverhead + len(k))
		}
		return n
	case s.is != nil:
		return s.is.EstimatedSize()
	default:
		return s.lp.EstimatedSize()
	}
}

// Clone returns an independent copy.
func (s *Set) Clone() any {
	switch {
	case s.m != nil:
		m := make(map[string]struct{}, len(s.m))
		for k := range s.m {
			m[k] = struct{}{}
		}
		return &Set{m: m}
	case s.is != nil:
		v := make([]int64, len(s.is.v))
		copy(v, s.is.v)
		return &Set{is: &intset{v: v}}
	default:
		return &Set{lp: s.lp.Clone()}
	}
}

// Contains reports membership.
func (s *Set) Contains(member []byte) bool {
	switch {
	case s.m != nil:
		_, ok := s.m[string(member)]
		return ok
	case s.is != nil:
		n, ok := canonicalInt(member)
		return ok && s.is.Contains(n)
	default:
		return s.lp.IndexOf(member) >= 0
	}
}

// Add inserts a member and reports whether it was new.
func (s *Set) Add(member []byte, t *Thresholds) bool {
	if s.m != nil {
		if _, ok := s.m[string(member)]; ok {
			return false
		}
		s.m[string(member)] = struct{}{}
		return true
	}
	if s.is != nil {
		n, isInt := canonicalInt(member)
		if !isInt {
			// A non-integer member ends the intset encoding regardless of
			// size, so convert first and let the normal path handle it.
			s.demoteIntset(t, len(member))
			return s.Add(member, t)
		}
		if !s.is.Add(n) {
			return false
		}
		if s.is.Len() > t.SetMaxIntsetEntries {
			s.demoteIntset(t, 20)
		}
		return true
	}
	if s.lp.IndexOf(member) >= 0 {
		return false
	}
	s.lp.Append(member)
	if s.lp.Len() > t.SetMaxListpackEntries || len(member) > t.SetMaxListpackValue {
		s.promoteToMap()
	}
	return true
}

// demoteIntset converts an intset to a listpack, or straight to a map when
// it is already too large for one.
func (s *Set) demoteIntset(t *Thresholds, incomingLen int) {
	toMap := s.is.Len() >= t.SetMaxListpackEntries || incomingLen > t.SetMaxListpackValue
	if toMap {
		m := make(map[string]struct{}, s.is.Len())
		for _, n := range s.is.v {
			m[strconv.FormatInt(n, 10)] = struct{}{}
		}
		s.m, s.is = m, nil
		return
	}
	lp := newListpack(s.is.Len() * 4)
	var buf [20]byte
	for _, n := range s.is.v {
		lp.Append(strconv.AppendInt(buf[:0], n, 10))
	}
	s.lp, s.is = lp, nil
}

func (s *Set) promoteToMap() {
	m := make(map[string]struct{}, s.lp.Len())
	s.lp.Each(func(_ int, e []byte) bool {
		m[string(e)] = struct{}{}
		return true
	})
	s.m, s.lp = m, nil
}

// Remove deletes a member and reports whether it was present.
func (s *Set) Remove(member []byte) bool {
	switch {
	case s.m != nil:
		if _, ok := s.m[string(member)]; !ok {
			return false
		}
		delete(s.m, string(member))
		return true
	case s.is != nil:
		n, ok := canonicalInt(member)
		return ok && s.is.Remove(n)
	default:
		i := s.lp.IndexOf(member)
		if i < 0 {
			return false
		}
		s.lp.DeleteAt(i)
		return true
	}
}

// Members returns every member.
func (s *Set) Members() [][]byte {
	out := make([][]byte, 0, s.Len())
	switch {
	case s.m != nil:
		for k := range s.m {
			out = append(out, []byte(k))
		}
	case s.is != nil:
		var buf [20]byte
		for _, n := range s.is.v {
			out = append(out, append([]byte(nil), strconv.AppendInt(buf[:0], n, 10)...))
		}
	default:
		s.lp.Each(func(_ int, e []byte) bool {
			out = append(out, copyBytes(e))
			return true
		})
	}
	return out
}

// RandomMembers returns count members. A negative count allows repeats and
// returns exactly that many; a positive one returns distinct members capped
// at the set size.
func (s *Set) RandomMembers(count int, rng *rand.Rand) [][]byte {
	all := s.Members()
	if len(all) == 0 {
		return nil
	}
	if count < 0 {
		out := make([][]byte, 0, -count)
		for i := 0; i < -count; i++ {
			out = append(out, all[rng.Intn(len(all))])
		}
		return out
	}
	if count > len(all) {
		count = len(all)
	}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	return all[:count]
}

// ---------------------------------------------------------------- database

func (db *DB) setAt(s *shard, key []byte) (*Object, *Set, error) {
	return asSet(db.collectionAt(s, key, TypeSet))
}

func (db *DB) setRead(s *shard, key []byte) (*Object, *Set, error) {
	return asSet(db.collectionRead(s, key, TypeSet))
}

func asSet(o *Object, err error) (*Object, *Set, error) {
	if err != nil || o == nil {
		return nil, nil, err
	}
	return o, o.Value.(*Set), nil
}

// SAdd inserts members, returning how many were new.
func (db *DB) SAdd(key []byte, members [][]byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, set, err := db.setAt(s, key)
	if err != nil {
		return 0, err
	}
	t := db.ks.Thresholds()
	if set == nil {
		if len(members) == 0 {
			return 0, nil
		}
		set = newSet(members[0], t)
		o = db.newCollection(s, key, TypeSet, set)
	}
	var added int64
	for _, m := range members {
		if set.Add(m, t) {
			added++
		}
	}
	db.finishWrite(s, key, o, set)
	return added, nil
}

// SRem removes members, returning how many were present.
func (db *DB) SRem(key []byte, members [][]byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, set, err := db.setAt(s, key)
	if err != nil || set == nil {
		return 0, err
	}
	var removed int64
	for _, m := range members {
		if set.Remove(m) {
			removed++
		}
	}
	if removed > 0 {
		db.finishWrite(s, key, o, set)
	}
	return removed, nil
}

// SCard returns the member count.
func (db *DB) SCard(key []byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, set, err := db.setRead(s, key)
	if err != nil || set == nil {
		return 0, err
	}
	return int64(set.Len()), nil
}

// SIsMember reports membership for one member.
func (db *DB) SIsMember(key, member []byte) (bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, set, err := db.setRead(s, key)
	if err != nil || set == nil {
		return false, err
	}
	return set.Contains(member), nil
}

// SMIsMember reports membership for several members.
func (db *DB) SMIsMember(key []byte, members [][]byte) ([]bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	out := make([]bool, len(members))
	_, set, err := db.setRead(s, key)
	if err != nil || set == nil {
		return out, err
	}
	for i, m := range members {
		out[i] = set.Contains(m)
	}
	return out, nil
}

// SMembers returns every member.
func (db *DB) SMembers(key []byte) ([][]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, set, err := db.setRead(s, key)
	if err != nil || set == nil {
		return nil, err
	}
	return set.Members(), nil
}

// SRandMember returns random members without removing them.
func (db *DB) SRandMember(key []byte, count int) ([][]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, set, err := db.setRead(s, key)
	if err != nil || set == nil {
		return nil, err
	}
	return set.RandomMembers(count, rand.New(rand.NewSource(rand.Int63()))), nil
}

// SPop removes and returns random members. The members it chose are what the
// command layer must log, since the choice is not reproducible (ADR-008).
func (db *DB) SPop(key []byte, count int) ([][]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, set, err := db.setAt(s, key)
	if err != nil || set == nil {
		return nil, err
	}
	popped := set.RandomMembers(count, rand.New(rand.NewSource(rand.Int63())))
	for _, m := range popped {
		set.Remove(m)
	}
	if len(popped) > 0 {
		db.finishWrite(s, key, o, set)
	}
	return popped, nil
}

// SMove atomically moves a member from one set to another.
func (db *DB) SMove(src, dst, member []byte) (bool, error) {
	locked := db.lockKeys([][]byte{src, dst})
	defer db.unlockShards(locked)

	ss, ds := db.shardFor(src), db.shardFor(dst)
	so, err := db.lookupType(ss, src, TypeSet)
	if err != nil || so == nil {
		return false, err
	}
	do, err := db.lookupType(ds, dst, TypeSet)
	if err != nil {
		return false, err
	}
	source := so.Value.(*Set)
	if !source.Contains(member) {
		return false, nil
	}
	t := db.ks.Thresholds()
	if string(src) == string(dst) {
		return true, nil
	}
	source.Remove(member)
	if do == nil {
		target := newSet(member, t)
		target.Add(member, t)
		db.newCollection(ds, dst, TypeSet, target)
		db.touched(dst)
	} else {
		do.Value.(*Set).Add(member, t)
		db.finishWrite(ds, dst, do, do.Value.(*Set))
	}
	db.finishWrite(ss, src, so, source)
	return true, nil
}

// SetOp names the three set combinations.
type SetOp uint8

// Set combination operators.
const (
	SetInter SetOp = iota
	SetUnion
	SetDiff
)

// SetCombine computes the intersection, union or difference of several sets.
//
// limit, when positive, stops an intersection early once that many members
// have been found, which is what SINTERCARD needs.
func (db *DB) SetCombine(op SetOp, keys [][]byte, limit int) ([][]byte, error) {
	locked := db.lockKeys(keys)
	defer db.unlockShards(locked)
	return db.combineLocked(op, keys, limit, ReadAccess)
}

// combineLocked requires the shards of keys to be held. a is the access of
// the command being served: SUNION and friends only read their sources,
// SUNIONSTORE and friends replay verbatim and so must reap them.
func (db *DB) combineLocked(op SetOp, keys [][]byte, limit int, a Access) ([][]byte, error) {
	sets := make([]*Set, len(keys))
	for i, k := range keys {
		o, err := db.getType(db.shardFor(k), k, TypeSet, a)
		if err != nil {
			return nil, err
		}
		if o != nil {
			sets[i] = o.Value.(*Set)
		}
	}

	switch op {
	case SetInter:
		// An absent set makes the intersection empty.
		for _, s := range sets {
			if s == nil {
				return nil, nil
			}
		}
		// Starting from the smallest set keeps the work proportional to it
		// rather than to the largest.
		smallest := 0
		for i, s := range sets {
			if s.Len() < sets[smallest].Len() {
				smallest = i
			}
		}
		out := make([][]byte, 0, sets[smallest].Len())
		for _, m := range sets[smallest].Members() {
			in := true
			for i, s := range sets {
				if i != smallest && !s.Contains(m) {
					in = false
					break
				}
			}
			if in {
				out = append(out, m)
				if limit > 0 && len(out) >= limit {
					break
				}
			}
		}
		return out, nil

	case SetUnion:
		seen := make(map[string]struct{})
		out := make([][]byte, 0)
		for _, s := range sets {
			if s == nil {
				continue
			}
			for _, m := range s.Members() {
				if _, dup := seen[string(m)]; dup {
					continue
				}
				seen[string(m)] = struct{}{}
				out = append(out, m)
			}
		}
		return out, nil

	default: // SetDiff
		if len(sets) == 0 || sets[0] == nil {
			return nil, nil
		}
		out := make([][]byte, 0, sets[0].Len())
		for _, m := range sets[0].Members() {
			keep := true
			for _, s := range sets[1:] {
				if s != nil && s.Contains(m) {
					keep = false
					break
				}
			}
			if keep {
				out = append(out, m)
			}
		}
		return out, nil
	}
}

// SetCombineStore computes a combination and stores it at dst, returning the
// resulting cardinality. An empty result deletes dst.
func (db *DB) SetCombineStore(op SetOp, dst []byte, keys [][]byte) (int64, error) {
	all := append([][]byte{dst}, keys...)
	locked := db.lockKeys(all)
	defer db.unlockShards(locked)

	members, err := db.combineLocked(op, keys, 0, WriteAccess)
	if err != nil {
		return 0, err
	}
	ds := db.shardFor(dst)
	if existing := db.lookup(ds, dst); existing != nil {
		db.removeLocked(ds, string(dst), existing)
	}
	if len(members) == 0 {
		db.touched(dst)
		return 0, nil
	}
	t := db.ks.Thresholds()
	set := newSet(members[0], t)
	for _, m := range members {
		set.Add(m, t)
	}
	o := db.newCollection(ds, dst, TypeSet, set)
	db.finishWrite(ds, dst, o, set)
	return int64(set.Len()), nil
}
