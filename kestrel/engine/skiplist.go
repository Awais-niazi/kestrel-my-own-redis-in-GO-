package engine

import (
	"bytes"
	"fmt"
)

// The skiplist behind sorted sets (ADR-005).
//
// Nodes are ordered by score, and by member bytes where scores tie. Each
// forward pointer carries a span: the number of nodes it jumps over. Summing
// spans along a search path yields a rank, which is what makes ZRANK and
// index-based ZRANGE O(log n) instead of O(n). That span trick is the reason
// this is a skiplist and not a B-tree: maintaining subtree sizes across
// B-tree splits and merges is markedly more intricate, and the balancing
// properties a B-tree would buy are ones a single-writer structure does not
// need.

const (
	skiplistMaxLevel = 32
	// skiplistBranch is the reciprocal of the promotion probability: a node
	// gains another level with probability 1/4, giving an average of 1.33
	// forward pointers per node.
	skiplistBranch = 4
)

type slLevel struct {
	next *slNode
	span int // nodes between this node and next, inclusive of next
}

type slNode struct {
	member []byte
	score  float64
	back   *slNode
	levels []slLevel
}

type skiplist struct {
	head   *slNode
	tail   *slNode
	length int
	level  int
	rng    uint32 // xorshift state, private so level choice needs no lock
}

func newSkiplist() *skiplist {
	return &skiplist{
		head:  &slNode{levels: make([]slLevel, skiplistMaxLevel)},
		level: 1,
		rng:   0x9e3779b9,
	}
}

// randomLevel picks a node height. The generator is per-list rather than the
// shared global one, because the shared one takes a mutex and every insert
// would contend on it.
func (sl *skiplist) randomLevel() int {
	level := 1
	for level < skiplistMaxLevel {
		sl.rng ^= sl.rng << 13
		sl.rng ^= sl.rng >> 17
		sl.rng ^= sl.rng << 5
		if sl.rng%skiplistBranch != 0 {
			break
		}
		level++
	}
	return level
}

// before reports whether (score, member) sorts strictly before n.
func before(n *slNode, score float64, member []byte) bool {
	if n.score != score {
		return n.score < score
	}
	return bytes.Compare(n.member, member) < 0
}

// Insert adds a node. The caller guarantees the member is not already
// present, which the ZSet layer enforces through its member map.
func (sl *skiplist) Insert(score float64, member []byte) *slNode {
	var update [skiplistMaxLevel]*slNode
	var rank [skiplistMaxLevel]int

	x := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		if i == sl.level-1 {
			rank[i] = 0
		} else {
			rank[i] = rank[i+1]
		}
		for x.levels[i].next != nil && before(x.levels[i].next, score, member) {
			rank[i] += x.levels[i].span
			x = x.levels[i].next
		}
		update[i] = x
	}

	level := sl.randomLevel()
	if level > sl.level {
		for i := sl.level; i < level; i++ {
			rank[i] = 0
			update[i] = sl.head
			update[i].levels[i].span = sl.length
		}
		sl.level = level
	}

	node := &slNode{member: copyBytes(member), score: score, levels: make([]slLevel, level)}
	for i := 0; i < level; i++ {
		node.levels[i].next = update[i].levels[i].next
		update[i].levels[i].next = node
		node.levels[i].span = update[i].levels[i].span - (rank[0] - rank[i])
		update[i].levels[i].span = (rank[0] - rank[i]) + 1
	}
	// Levels above the new node's height gained one node underneath them.
	for i := level; i < sl.level; i++ {
		update[i].levels[i].span++
	}

	if update[0] != sl.head {
		node.back = update[0]
	}
	if node.levels[0].next != nil {
		node.levels[0].next.back = node
	} else {
		sl.tail = node
	}
	sl.length++
	return node
}

// Delete removes the node with the given score and member.
func (sl *skiplist) Delete(score float64, member []byte) bool {
	var update [skiplistMaxLevel]*slNode
	x := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		for x.levels[i].next != nil && before(x.levels[i].next, score, member) {
			x = x.levels[i].next
		}
		update[i] = x
	}
	target := x.levels[0].next
	if target == nil || target.score != score || !bytes.Equal(target.member, member) {
		return false
	}
	sl.unlink(target, &update)
	return true
}

func (sl *skiplist) unlink(node *slNode, update *[skiplistMaxLevel]*slNode) {
	for i := 0; i < sl.level; i++ {
		if update[i].levels[i].next == node {
			update[i].levels[i].span += node.levels[i].span - 1
			update[i].levels[i].next = node.levels[i].next
		} else {
			update[i].levels[i].span--
		}
	}
	if node.levels[0].next != nil {
		node.levels[0].next.back = node.back
	} else {
		sl.tail = node.back
	}
	// Drop levels that no longer hold anything.
	for sl.level > 1 && sl.head.levels[sl.level-1].next == nil {
		sl.level--
	}
	sl.length--
}

// Rank returns the 1-based position of a member, or 0 when it is absent.
func (sl *skiplist) Rank(score float64, member []byte) int {
	rank := 0
	x := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		for x.levels[i].next != nil && !after(x.levels[i].next, score, member) {
			rank += x.levels[i].span
			x = x.levels[i].next
		}
		if x != sl.head && x.score == score && bytes.Equal(x.member, member) {
			return rank
		}
	}
	return 0
}

// after reports whether n sorts strictly after (score, member).
func after(n *slNode, score float64, member []byte) bool {
	if n.score != score {
		return n.score > score
	}
	return bytes.Compare(n.member, member) > 0
}

// NodeByRank returns the node at a 1-based rank.
func (sl *skiplist) NodeByRank(rank int) *slNode {
	if rank <= 0 || rank > sl.length {
		return nil
	}
	traversed := 0
	x := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		for x.levels[i].next != nil && traversed+x.levels[i].span <= rank {
			traversed += x.levels[i].span
			x = x.levels[i].next
		}
		if traversed == rank {
			return x
		}
	}
	return nil
}

// First returns the lowest-ranked node.
func (sl *skiplist) First() *slNode { return sl.head.levels[0].next }

// Last returns the highest-ranked node.
func (sl *skiplist) Last() *slNode { return sl.tail }

// ScoreRange bounds a score interval, with optional exclusive endpoints.
type ScoreRange struct {
	Min, Max         float64
	MinExcl, MaxExcl bool
}

func (r ScoreRange) empty() bool {
	if r.Min > r.Max {
		return true
	}
	return r.Min == r.Max && (r.MinExcl || r.MaxExcl)
}

func (r ScoreRange) aboveMin(score float64) bool {
	if r.MinExcl {
		return score > r.Min
	}
	return score >= r.Min
}

func (r ScoreRange) belowMax(score float64) bool {
	if r.MaxExcl {
		return score < r.Max
	}
	return score <= r.Max
}

// Contains reports whether a score falls inside the range.
func (r ScoreRange) Contains(score float64) bool {
	return r.aboveMin(score) && r.belowMax(score)
}

// FirstInScoreRange returns the lowest node inside a score range.
func (sl *skiplist) FirstInScoreRange(r ScoreRange) *slNode {
	if r.empty() {
		return nil
	}
	x := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		for x.levels[i].next != nil && !r.aboveMin(x.levels[i].next.score) {
			x = x.levels[i].next
		}
	}
	x = x.levels[0].next
	if x == nil || !r.belowMax(x.score) {
		return nil
	}
	return x
}

// LastInScoreRange returns the highest node inside a score range.
func (sl *skiplist) LastInScoreRange(r ScoreRange) *slNode {
	if r.empty() {
		return nil
	}
	x := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		for x.levels[i].next != nil && r.belowMax(x.levels[i].next.score) {
			x = x.levels[i].next
		}
	}
	if x == sl.head || !r.aboveMin(x.score) {
		return nil
	}
	return x
}

// LexRange bounds a member interval. Lexicographic ranges are only
// meaningful when every member shares the same score, which is the caller's
// responsibility, as it is in the reference implementation.
type LexRange struct {
	Min, Max         []byte
	MinExcl, MaxExcl bool
	MinInf, MaxInf   bool // the '-' and '+' endpoints
}

func (r LexRange) aboveMin(member []byte) bool {
	if r.MinInf {
		return true
	}
	c := bytes.Compare(member, r.Min)
	if r.MinExcl {
		return c > 0
	}
	return c >= 0
}

func (r LexRange) belowMax(member []byte) bool {
	if r.MaxInf {
		return true
	}
	c := bytes.Compare(member, r.Max)
	if r.MaxExcl {
		return c < 0
	}
	return c <= 0
}

// Contains reports whether a member falls inside the range.
func (r LexRange) Contains(member []byte) bool {
	return r.aboveMin(member) && r.belowMax(member)
}

func (r LexRange) empty() bool {
	if r.MinInf || r.MaxInf {
		return false
	}
	c := bytes.Compare(r.Min, r.Max)
	return c > 0 || (c == 0 && (r.MinExcl || r.MaxExcl))
}

// FirstInLexRange returns the lowest node inside a lexicographic range.
func (sl *skiplist) FirstInLexRange(r LexRange) *slNode {
	if r.empty() {
		return nil
	}
	x := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		for x.levels[i].next != nil && !r.aboveMin(x.levels[i].next.member) {
			x = x.levels[i].next
		}
	}
	x = x.levels[0].next
	if x == nil || !r.belowMax(x.member) {
		return nil
	}
	return x
}

// LastInLexRange returns the highest node inside a lexicographic range.
func (sl *skiplist) LastInLexRange(r LexRange) *slNode {
	if r.empty() {
		return nil
	}
	x := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		for x.levels[i].next != nil && r.belowMax(x.levels[i].next.member) {
			x = x.levels[i].next
		}
	}
	if x == sl.head || !r.aboveMin(x.member) {
		return nil
	}
	return x
}

// EstimatedSize reports the bytes attributable to the skiplist.
func (sl *skiplist) EstimatedSize() int64 {
	// Header, plus per node: the struct, its level slice, and the member.
	n := int64(64 + skiplistMaxLevel*16)
	for x := sl.First(); x != nil; x = x.levels[0].next {
		n += int64(64 + 16*len(x.levels) + len(x.member))
	}
	return n
}

// checkInvariants verifies the structural properties the span arithmetic
// depends on.
//
// Getting a span wrong produces a list that still iterates correctly but
// reports nonsense ranks, so ranks are checked against a count derived from
// the level-0 chain rather than being taken on trust. It lives here rather
// than in the tests so that DEBUG can run it against a live keyspace, which
// is what §12 means by every data structure having an invariant check.
func (sl *skiplist) checkInvariants() error {
	if sl.level < 1 || sl.level > skiplistMaxLevel {
		return fmt.Errorf("level %d out of range", sl.level)
	}

	// Level 0 is the authoritative sequence.
	var seq []*slNode
	for x := sl.head.levels[0].next; x != nil; x = x.levels[0].next {
		seq = append(seq, x)
	}
	if len(seq) != sl.length {
		return fmt.Errorf("length is %d but the level-0 chain has %d nodes", sl.length, len(seq))
	}
	if sl.length == 0 {
		if sl.tail != nil {
			return fmt.Errorf("empty list has a tail")
		}
		return nil
	}
	if sl.tail != seq[len(seq)-1] {
		return fmt.Errorf("tail does not point at the last node")
	}

	// Ordering and back pointers.
	for i, n := range seq {
		if i > 0 {
			prev := seq[i-1]
			if prev.score > n.score ||
				(prev.score == n.score && bytes.Compare(prev.member, n.member) > 0) {
				return fmt.Errorf("out of order at %d: %v/%q then %v/%q",
					i, prev.score, prev.member, n.score, n.member)
			}
			if n.back != prev {
				return fmt.Errorf("back pointer at %d is wrong", i)
			}
		} else if n.back != nil {
			return fmt.Errorf("first node has a back pointer")
		}
	}

	// Position of each node in the sequence, for span checking.
	pos := make(map[*slNode]int, len(seq))
	for i, n := range seq {
		pos[n] = i + 1 // 1-based
	}

	// Every forward pointer's span must equal the distance it covers.
	for level := 0; level < sl.level; level++ {
		x := sl.head
		at := 0
		for x.levels[level].next != nil {
			next := x.levels[level].next
			want := pos[next] - at
			if x.levels[level].span != want {
				return fmt.Errorf("span at level %d from position %d is %d, want %d",
					level, at, x.levels[level].span, want)
			}
			at = pos[next]
			x = next
		}
		// The trailing pointer's span is unused but must not be negative.
		if x.levels[level].span < 0 {
			return fmt.Errorf("negative trailing span at level %d", level)
		}
	}

	// Rank lookups must agree with the sequence.
	for i, n := range seq {
		if got := sl.Rank(n.score, n.member); got != i+1 {
			return fmt.Errorf("Rank(%v, %q) = %d, want %d", n.score, n.member, got, i+1)
		}
		if got := sl.NodeByRank(i + 1); got != n {
			return fmt.Errorf("NodeByRank(%d) returned the wrong node", i+1)
		}
	}
	if sl.NodeByRank(0) != nil || sl.NodeByRank(sl.length+1) != nil {
		return fmt.Errorf("NodeByRank accepted an out-of-range rank")
	}
	return nil
}
