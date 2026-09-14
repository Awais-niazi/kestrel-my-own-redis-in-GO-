package engine

import (
	"bytes"
	"math/rand"
	"sort"
)

// ZMember is one entry of a sorted set.
type ZMember struct {
	Member []byte
	Score  float64
}

// ZSet is a set of members each carrying a score, ordered by score and then
// by member bytes.
//
// While small it is a listpack of alternating member and score entries kept
// in sorted order, which makes every operation a linear scan over one
// contiguous buffer. Above the thresholds it becomes a skiplist for ordered
// and rank queries paired with a map for O(1) score lookup (ADR-005). The
// two structures must agree, which is a real correctness burden, so the
// invariant is checked from the tests rather than assumed.
type ZSet struct {
	lp *listpack // member, score, member, score ... sorted
	sl *skiplist
	m  map[string]float64
}

func newZSet() *ZSet { return &ZSet{lp: newListpack(0)} }

// Len returns the member count.
func (z *ZSet) Len() int {
	if z.lp != nil {
		return z.lp.Len() / 2
	}
	return len(z.m)
}

// Encoding reports the physical representation in use.
func (z *ZSet) Encoding() Encoding {
	if z.lp != nil {
		return EncodingListpack
	}
	return EncodingSkiplist
}

// EstimatedSize reports the bytes attributable to this sorted set.
func (z *ZSet) EstimatedSize() int64 {
	if z.lp != nil {
		return z.lp.EstimatedSize()
	}
	n := z.sl.EstimatedSize() + 48
	for k := range z.m {
		n += int64(mapEntryOverhead + len(k) + 8)
	}
	return n
}

// Clone returns an independent copy.
func (z *ZSet) Clone() any {
	if z.lp != nil {
		return &ZSet{lp: z.lp.Clone()}
	}
	c := &ZSet{sl: newSkiplist(), m: make(map[string]float64, len(z.m))}
	for x := z.sl.First(); x != nil; x = x.levels[0].next {
		c.sl.Insert(x.score, x.member)
		c.m[string(x.member)] = x.score
	}
	return c
}

// lpAt returns the member and score of the i-th logical element.
func (z *ZSet) lpAt(i int) ([]byte, float64) {
	member := z.lp.At(i * 2)
	score, _ := ParseFloat(z.lp.At(i*2 + 1))
	return member, score
}

// lpFind returns the logical index of a member, or -1.
func (z *ZSet) lpFind(member []byte) int {
	i := z.lp.IndexOfStep(member, 2, 0)
	if i < 0 {
		return -1
	}
	return i / 2
}

// lpSearch returns the logical position where (score, member) belongs.
func (z *ZSet) lpSearch(score float64, member []byte) int {
	n := z.lp.Len() / 2
	return sort.Search(n, func(i int) bool {
		m, s := z.lpAt(i)
		if s != score {
			return s > score
		}
		return bytes.Compare(m, member) >= 0
	})
}

// Score returns a member's score.
func (z *ZSet) Score(member []byte) (float64, bool) {
	if z.lp == nil {
		s, ok := z.m[string(member)]
		return s, ok
	}
	i := z.lpFind(member)
	if i < 0 {
		return 0, false
	}
	_, s := z.lpAt(i)
	return s, true
}

// Add inserts or re-scores a member, reporting whether it was new.
func (z *ZSet) Add(member []byte, score float64, t *Thresholds) bool {
	if z.lp == nil {
		old, existed := z.m[string(member)]
		if existed {
			if old == score {
				return false
			}
			z.sl.Delete(old, member)
		}
		z.sl.Insert(score, member)
		z.m[string(member)] = score
		return !existed
	}
	if i := z.lpFind(member); i >= 0 {
		_, old := z.lpAt(i)
		if old == score {
			return false
		}
		z.lp.DeleteRun(i*2, 2)
		z.lpInsert(score, member)
		return false
	}
	z.lpInsert(score, member)
	z.promoteIfNeeded(t, len(member))
	return true
}

func (z *ZSet) lpInsert(score float64, member []byte) {
	at := z.lpSearch(score, member)
	rendered := FormatFloat(score)
	z.lp.InsertAt(at*2, member)
	z.lp.InsertAt(at*2+1, rendered)
}

func (z *ZSet) promoteIfNeeded(t *Thresholds, longest int) {
	if z.lp == nil {
		return
	}
	if z.lp.Len()/2 <= t.ZSetMaxListpackEntries && longest <= t.ZSetMaxListpackValue {
		return
	}
	sl := newSkiplist()
	m := make(map[string]float64, z.lp.Len()/2)
	for i := 0; i < z.lp.Len()/2; i++ {
		member, score := z.lpAt(i)
		sl.Insert(score, member)
		m[string(member)] = score
	}
	z.sl, z.m, z.lp = sl, m, nil
}

// Remove deletes a member, reporting whether it was present.
func (z *ZSet) Remove(member []byte) bool {
	if z.lp == nil {
		score, ok := z.m[string(member)]
		if !ok {
			return false
		}
		z.sl.Delete(score, member)
		delete(z.m, string(member))
		return true
	}
	i := z.lpFind(member)
	if i < 0 {
		return false
	}
	z.lp.DeleteRun(i*2, 2)
	return true
}

// Rank returns a member's zero-based position, counting from the end when
// reverse is set.
func (z *ZSet) Rank(member []byte, reverse bool) (int, bool) {
	var rank int
	if z.lp == nil {
		score, ok := z.m[string(member)]
		if !ok {
			return 0, false
		}
		rank = z.sl.Rank(score, member) - 1
	} else {
		i := z.lpFind(member)
		if i < 0 {
			return 0, false
		}
		rank = i
	}
	if reverse {
		rank = z.Len() - 1 - rank
	}
	return rank, true
}

// Each walks the members in ascending order.
func (z *ZSet) Each(fn func(m ZMember) bool) {
	if z.lp == nil {
		for x := z.sl.First(); x != nil; x = x.levels[0].next {
			if !fn(ZMember{Member: copyBytes(x.member), Score: x.score}) {
				return
			}
		}
		return
	}
	for i := 0; i < z.lp.Len()/2; i++ {
		member, score := z.lpAt(i)
		if !fn(ZMember{Member: copyBytes(member), Score: score}) {
			return
		}
	}
}

// All returns every member in ascending order.
func (z *ZSet) All() []ZMember {
	out := make([]ZMember, 0, z.Len())
	z.Each(func(m ZMember) bool {
		out = append(out, m)
		return true
	})
	return out
}

// RangeByRank returns the members between two zero-based indices inclusive,
// with negative indices counting from the end.
func (z *ZSet) RangeByRank(start, stop int64, reverse bool) []ZMember {
	all := z.All()
	if reverse {
		reverseMembers(all)
	}
	lo, hi, ok := clampRange(start, stop, int64(len(all)))
	if !ok {
		return nil
	}
	return all[lo : hi+1]
}

// RangeByScore returns the members inside a score range, after skipping
// offset of them and limited to count when count is non-negative.
func (z *ZSet) RangeByScore(r ScoreRange, reverse bool, offset, count int64) []ZMember {
	out := make([]ZMember, 0, 16)
	z.Each(func(m ZMember) bool {
		if r.Contains(m.Score) {
			out = append(out, m)
		}
		return true
	})
	if reverse {
		reverseMembers(out)
	}
	return applyLimit(out, offset, count)
}

// RangeByLex returns the members inside a lexicographic range.
func (z *ZSet) RangeByLex(r LexRange, reverse bool, offset, count int64) []ZMember {
	out := make([]ZMember, 0, 16)
	z.Each(func(m ZMember) bool {
		if r.Contains(m.Member) {
			out = append(out, m)
		}
		return true
	})
	if reverse {
		reverseMembers(out)
	}
	return applyLimit(out, offset, count)
}

func applyLimit(in []ZMember, offset, count int64) []ZMember {
	if offset < 0 {
		return nil
	}
	if offset >= int64(len(in)) {
		return nil
	}
	in = in[offset:]
	if count >= 0 && count < int64(len(in)) {
		in = in[:count]
	}
	return in
}

func reverseMembers(m []ZMember) {
	for i, j := 0, len(m)-1; i < j; i, j = i+1, j-1 {
		m[i], m[j] = m[j], m[i]
	}
}

// Count returns how many members fall inside a score range.
func (z *ZSet) Count(r ScoreRange) int {
	n := 0
	z.Each(func(m ZMember) bool {
		if r.Contains(m.Score) {
			n++
		}
		return true
	})
	return n
}

// LexCount returns how many members fall inside a lexicographic range.
func (z *ZSet) LexCount(r LexRange) int {
	n := 0
	z.Each(func(m ZMember) bool {
		if r.Contains(m.Member) {
			n++
		}
		return true
	})
	return n
}

// Pop removes and returns members from the low or high end.
func (z *ZSet) Pop(count int, highest bool) []ZMember {
	all := z.All()
	if highest {
		reverseMembers(all)
	}
	if count > len(all) {
		count = len(all)
	}
	out := all[:count]
	for _, m := range out {
		z.Remove(m.Member)
	}
	return out
}

// RemoveMembers deletes the given members, returning how many were present.
func (z *ZSet) RemoveMembers(members []ZMember) int {
	n := 0
	for _, m := range members {
		if z.Remove(m.Member) {
			n++
		}
	}
	return n
}

// RandomMembers returns count members. A negative count allows repeats.
func (z *ZSet) RandomMembers(count int, rng *rand.Rand) []ZMember {
	all := z.All()
	if len(all) == 0 {
		return nil
	}
	if count < 0 {
		out := make([]ZMember, 0, -count)
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

// checkInvariant verifies that the skiplist and the member map agree. The
// dual structure is the cost ADR-005 accepts for O(1) score lookup, and this
// is the check that keeps it honest.
func (z *ZSet) checkInvariant() error {
	if z.lp != nil {
		if z.lp.Len()%2 != 0 {
			return errUnevenListpack
		}
		return nil
	}
	if z.sl.length != len(z.m) {
		return errZSetLengthMismatch
	}
	for x := z.sl.First(); x != nil; x = x.levels[0].next {
		score, ok := z.m[string(x.member)]
		if !ok || score != x.score {
			return errZSetScoreMismatch
		}
	}
	return z.sl.checkInvariants()
}

var (
	errUnevenListpack     = errorString("sorted set listpack has an odd entry count")
	errZSetLengthMismatch = errorString("skiplist and member map disagree on length")
	errZSetScoreMismatch  = errorString("skiplist and member map disagree on a score")
)

type errorString string

func (e errorString) Error() string { return string(e) }

// ---------------------------------------------------------------- database

// ZAddFlags carries the option set of ZADD.
type ZAddFlags uint8

// ZADD options.
const (
	ZAddNX   ZAddFlags = 1 << iota // only add new members
	ZAddXX                         // only update existing members
	ZAddGT                         // only raise a score
	ZAddLT                         // only lower a score
	ZAddCH                         // count updates as changes
	ZAddINCR                       // treat the score as an increment
)

func (db *DB) zsetAt(s *shard, key []byte) (*Object, *ZSet, error) {
	o, err := db.collectionAt(s, key, TypeZSet)
	if err != nil || o == nil {
		return nil, nil, err
	}
	return o, o.Value.(*ZSet), nil
}

// ZAddResult reports what ZAdd did.
type ZAddResult struct {
	// Added counts members that did not exist before.
	Added int64
	// Changed counts members added or re-scored, which is what CH returns.
	Changed int64
	// Applied lists every member actually written, with the score it ended
	// up with. The command layer logs these rather than the original
	// arguments, because NX, XX, GT, LT and INCR all make the request
	// conditional or relative and therefore unsafe to replay (ADR-008).
	Applied []ZMember
}

// ZAdd inserts or re-scores members subject to flags.
func (db *DB) ZAdd(key []byte, members []ZMember, flags ZAddFlags) (ZAddResult, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	var res ZAddResult
	o, z, err := db.zsetAt(s, key)
	if err != nil {
		return res, err
	}
	if z == nil {
		if flags&ZAddXX != 0 {
			return res, nil
		}
		z = newZSet()
		o = db.newCollection(s, key, TypeZSet, z)
	}
	t := db.ks.Thresholds()

	for _, m := range members {
		cur, exists := z.Score(m.Member)
		score := m.Score

		if flags&ZAddINCR != 0 && exists {
			score = cur + m.Score
			if score != score { // NaN, from adding opposite infinities
				return res, ErrNaN
			}
		}
		switch {
		case exists && flags&ZAddNX != 0:
			continue
		case !exists && flags&ZAddXX != 0:
			continue
		case exists && flags&ZAddGT != 0 && score <= cur:
			continue
		case exists && flags&ZAddLT != 0 && score >= cur:
			continue
		}
		if z.Add(m.Member, score, t) {
			res.Added++
			res.Changed++
		} else if cur != score {
			res.Changed++
		}
		res.Applied = append(res.Applied, ZMember{Member: m.Member, Score: score})
	}
	db.finishWrite(s, key, o, z)
	return res, nil
}

// ErrNaN is returned when an increment would produce a NaN score.
var ErrNaN = errorString("resulting score is not a number (NaN)")

// ZRem removes members, returning how many were present.
func (db *DB) ZRem(key []byte, members [][]byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return 0, err
	}
	var n int64
	for _, m := range members {
		if z.Remove(m) {
			n++
		}
	}
	if n > 0 {
		db.finishWrite(s, key, o, z)
	}
	return n, nil
}

// ZScore returns a member's score.
func (db *DB) ZScore(key, member []byte) (float64, bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return 0, false, err
	}
	score, ok := z.Score(member)
	return score, ok, nil
}

// ZMScore returns the scores of several members.
func (db *DB) ZMScore(key []byte, members [][]byte) ([]float64, []bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	scores, found := make([]float64, len(members)), make([]bool, len(members))
	_, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return scores, found, err
	}
	for i, m := range members {
		scores[i], found[i] = z.Score(m)
	}
	return scores, found, nil
}

// ZCard returns the member count.
func (db *DB) ZCard(key []byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return 0, err
	}
	return int64(z.Len()), nil
}

// ZRank returns a member's zero-based rank.
func (db *DB) ZRank(key, member []byte, reverse bool) (int, bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return 0, false, err
	}
	rank, ok := z.Rank(member, reverse)
	return rank, ok, nil
}

// ZCount returns how many members fall inside a score range.
func (db *DB) ZCount(key []byte, r ScoreRange) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return 0, err
	}
	return int64(z.Count(r)), nil
}

// ZLexCount returns how many members fall inside a lexicographic range.
func (db *DB) ZLexCount(key []byte, r LexRange) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return 0, err
	}
	return int64(z.LexCount(r)), nil
}

// RangeBy names the three ways a sorted set range can be expressed.
type RangeBy uint8

// Range selectors.
const (
	RangeByRank RangeBy = iota
	RangeByScore
	RangeByLex
)

// ZRangeSpec describes any ZRANGE variant in one shape.
type ZRangeSpec struct {
	By      RangeBy
	Start   int64 // rank ranges
	Stop    int64
	Score   ScoreRange
	Lex     LexRange
	Reverse bool
	Offset  int64
	Count   int64 // negative means unlimited
}

// ZRange returns the members a range selects.
func (db *DB) ZRange(key []byte, spec ZRangeSpec) ([]ZMember, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return nil, err
	}
	return z.rangeBy(spec), nil
}

func (z *ZSet) rangeBy(spec ZRangeSpec) []ZMember {
	switch spec.By {
	case RangeByScore:
		return z.RangeByScore(spec.Score, spec.Reverse, spec.Offset, spec.Count)
	case RangeByLex:
		return z.RangeByLex(spec.Lex, spec.Reverse, spec.Offset, spec.Count)
	default:
		return z.RangeByRank(spec.Start, spec.Stop, spec.Reverse)
	}
}

// ZRangeStore computes a range of src and stores it at dst, returning the
// resulting cardinality.
func (db *DB) ZRangeStore(dst, src []byte, spec ZRangeSpec) (int64, error) {
	locked := db.lockKeys([][]byte{dst, src})
	defer db.unlockShards(locked)

	so, err := db.lookupType(db.shardFor(src), src, TypeZSet)
	if err != nil {
		return 0, err
	}
	var members []ZMember
	if so != nil {
		members = so.Value.(*ZSet).rangeBy(spec)
	}
	return db.replaceZSet(dst, members), nil
}

// replaceZSet writes members to key, deleting it when they are empty. The
// destination shard must already be held.
func (db *DB) replaceZSet(key []byte, members []ZMember) int64 {
	ds := db.shardFor(key)
	if existing := db.lookup(ds, key); existing != nil {
		db.removeLocked(ds, string(key), existing)
	}
	if len(members) == 0 {
		db.touched(key)
		return 0
	}
	z := newZSet()
	t := db.ks.Thresholds()
	for _, m := range members {
		z.Add(m.Member, m.Score, t)
	}
	o := db.newCollection(ds, key, TypeZSet, z)
	db.finishWrite(ds, key, o, z)
	return int64(z.Len())
}

// ZPop removes and returns members from either end.
func (db *DB) ZPop(key []byte, count int, highest bool) ([]ZMember, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return nil, err
	}
	out := z.Pop(count, highest)
	if len(out) > 0 {
		db.finishWrite(s, key, o, z)
	}
	return out, nil
}

// ZRemRange removes the members a range selects, returning how many went.
func (db *DB) ZRemRange(key []byte, spec ZRangeSpec) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return 0, err
	}
	n := z.RemoveMembers(z.rangeBy(spec))
	if n > 0 {
		db.finishWrite(s, key, o, z)
	}
	return int64(n), nil
}

// ZRandMember returns random members.
func (db *DB) ZRandMember(key []byte, count int) ([]ZMember, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, z, err := db.zsetAt(s, key)
	if err != nil || z == nil {
		return nil, err
	}
	return z.RandomMembers(count, rand.New(rand.NewSource(rand.Int63()))), nil
}

// Aggregate names how scores combine when sorted sets are merged.
type Aggregate uint8

// Score aggregation modes.
const (
	AggregateSum Aggregate = iota
	AggregateMin
	AggregateMax
)

// ZCombine computes the union, intersection or difference of sorted sets.
//
// A plain set given as an input contributes a score of 1 per member, which
// is what makes ZUNIONSTORE over sets a usable counting primitive.
func (db *DB) ZCombine(op SetOp, keys [][]byte, weights []float64, agg Aggregate, limit int) ([]ZMember, error) {
	locked := db.lockKeys(keys)
	defer db.unlockShards(locked)
	return db.zcombineLocked(op, keys, weights, agg, limit)
}

func (db *DB) zcombineLocked(op SetOp, keys [][]byte, weights []float64, agg Aggregate, limit int) ([]ZMember, error) {
	inputs := make([][]ZMember, len(keys))
	for i, k := range keys {
		members, err := db.membersAsZSet(k)
		if err != nil {
			return nil, err
		}
		w := 1.0
		if i < len(weights) {
			w = weights[i]
		}
		for j := range members {
			members[j].Score *= w
			if members[j].Score != members[j].Score {
				// weight 0 against an infinite score yields NaN, which the
				// reference implementation defines as 0.
				members[j].Score = 0
			}
		}
		inputs[i] = members
	}

	type acc struct {
		score float64
		count int
		order int
	}
	combined := make(map[string]*acc)
	var order []string

	for i, members := range inputs {
		for _, m := range members {
			key := string(m.Member)
			a, seen := combined[key]
			if !seen {
				if op == SetInter && i > 0 {
					continue // cannot be in the intersection
				}
				a = &acc{score: m.Score, count: 1, order: len(order)}
				combined[key] = a
				order = append(order, key)
				continue
			}
			a.count++
			switch agg {
			case AggregateMin:
				if m.Score < a.score {
					a.score = m.Score
				}
			case AggregateMax:
				if m.Score > a.score {
					a.score = m.Score
				}
			default:
				a.score += m.Score
				if a.score != a.score {
					a.score = 0
				}
			}
		}
	}

	out := make([]ZMember, 0, len(order))
	for _, key := range order {
		a := combined[key]
		switch op {
		case SetInter:
			if a.count != len(keys) {
				continue
			}
		case SetDiff:
			if a.count > 1 {
				continue
			}
			// Only members of the first input survive a difference.
			if !containsMember(inputs[0], key) {
				continue
			}
		}
		out = append(out, ZMember{Member: []byte(key), Score: a.score})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sortMembers(out)
	return out, nil
}

func containsMember(members []ZMember, key string) bool {
	for _, m := range members {
		if string(m.Member) == key {
			return true
		}
	}
	return false
}

func sortMembers(m []ZMember) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].Score != m[j].Score {
			return m[i].Score < m[j].Score
		}
		return bytes.Compare(m[i].Member, m[j].Member) < 0
	})
}

// membersAsZSet reads a key as a sorted set, accepting a plain set by giving
// each of its members a score of 1. The shard must be held.
func (db *DB) membersAsZSet(key []byte) ([]ZMember, error) {
	s := db.shardFor(key)
	o := db.lookup(s, key)
	if o == nil {
		return nil, nil
	}
	switch o.Type {
	case TypeZSet:
		return o.Value.(*ZSet).All(), nil
	case TypeSet:
		set := o.Value.(*Set)
		out := make([]ZMember, 0, set.Len())
		for _, m := range set.Members() {
			out = append(out, ZMember{Member: m, Score: 1})
		}
		return out, nil
	default:
		return nil, ErrWrongType
	}
}

// ZCombineStore computes a combination and stores it at dst.
func (db *DB) ZCombineStore(op SetOp, dst []byte, keys [][]byte, weights []float64, agg Aggregate) (int64, error) {
	all := append([][]byte{dst}, keys...)
	locked := db.lockKeys(all)
	defer db.unlockShards(locked)

	members, err := db.zcombineLocked(op, keys, weights, agg, 0)
	if err != nil {
		return 0, err
	}
	return db.replaceZSet(dst, members), nil
}
