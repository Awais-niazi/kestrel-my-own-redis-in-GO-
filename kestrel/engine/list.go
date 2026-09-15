package engine

// quicklist is a sequence of listpack nodes.
//
// A single listpack would make a long list O(n) to modify at the front,
// because every push would shift the whole buffer. Splitting it into bounded
// nodes keeps each shift proportional to the node size instead, while
// preserving the property that matters: a list of a million elements is a
// few thousand pointers rather than a million.
type quicklist struct {
	nodes []*listpack
	count int
	fill  int // maximum entries per node
}

func newQuicklist(fill int) *quicklist {
	if fill < 8 {
		fill = 8
	}
	return &quicklist{fill: fill}
}

func (q *quicklist) Len() int { return q.count }

func (q *quicklist) EstimatedSize() int64 {
	n := int64(sliceHeaderSize + 8*cap(q.nodes))
	for _, node := range q.nodes {
		n += node.EstimatedSize() + 8
	}
	return n
}

func (q *quicklist) Clone() *quicklist {
	c := &quicklist{count: q.count, fill: q.fill, nodes: make([]*listpack, len(q.nodes))}
	for i, node := range q.nodes {
		c.nodes[i] = node.Clone()
	}
	return c
}

func (q *quicklist) PushFront(v []byte) {
	if len(q.nodes) == 0 || q.nodes[0].Len() >= q.fill {
		q.nodes = append([]*listpack{newListpack(0)}, q.nodes...)
	}
	q.nodes[0].InsertAt(0, v)
	q.count++
}

func (q *quicklist) PushBack(v []byte) {
	last := len(q.nodes) - 1
	if last < 0 || q.nodes[last].Len() >= q.fill {
		q.nodes = append(q.nodes, newListpack(0))
		last++
	}
	q.nodes[last].Append(v)
	q.count++
}

func (q *quicklist) PopFront() ([]byte, bool) {
	if q.count == 0 {
		return nil, false
	}
	v := q.nodes[0].CopyAt(0)
	q.nodes[0].DeleteAt(0)
	q.count--
	q.dropEmpty(0)
	return v, true
}

func (q *quicklist) PopBack() ([]byte, bool) {
	if q.count == 0 {
		return nil, false
	}
	i := len(q.nodes) - 1
	node := q.nodes[i]
	v := node.CopyAt(node.Len() - 1)
	node.DeleteAt(node.Len() - 1)
	q.count--
	q.dropEmpty(i)
	return v, true
}

func (q *quicklist) dropEmpty(i int) {
	if q.nodes[i].Len() == 0 {
		q.nodes = append(q.nodes[:i], q.nodes[i+1:]...)
	}
}

// locate maps a logical index onto a node and an offset within it.
func (q *quicklist) locate(i int) (node, off int, ok bool) {
	if i < 0 || i >= q.count {
		return 0, 0, false
	}
	for n, lp := range q.nodes {
		if i < lp.Len() {
			return n, i, true
		}
		i -= lp.Len()
	}
	return 0, 0, false
}

func (q *quicklist) At(i int) ([]byte, bool) {
	n, off, ok := q.locate(i)
	if !ok {
		return nil, false
	}
	return q.nodes[n].CopyAt(off), true
}

func (q *quicklist) SetAt(i int, v []byte) bool {
	n, off, ok := q.locate(i)
	if !ok {
		return false
	}
	q.nodes[n].SetAt(off, v)
	return true
}

func (q *quicklist) DeleteAt(i int) bool {
	n, off, ok := q.locate(i)
	if !ok {
		return false
	}
	q.nodes[n].DeleteAt(off)
	q.count--
	q.dropEmpty(n)
	return true
}

// InsertAt inserts before logical index i.
func (q *quicklist) InsertAt(i int, v []byte) {
	if i <= 0 {
		q.PushFront(v)
		return
	}
	if i >= q.count {
		q.PushBack(v)
		return
	}
	n, off, _ := q.locate(i)
	q.nodes[n].InsertAt(off, v)
	q.count++
	q.splitIfLarge(n)
}

// splitIfLarge halves a node that has grown past the fill factor, which
// keeps per-node work bounded after a run of inserts into the middle.
func (q *quicklist) splitIfLarge(n int) {
	node := q.nodes[n]
	if node.Len() <= q.fill*2 {
		return
	}
	half := node.Len() / 2
	tail := newListpack(0)
	node.Each(func(i int, e []byte) bool {
		if i >= half {
			tail.Append(e)
		}
		return true
	})
	node.Truncate(half)
	q.nodes = append(q.nodes, nil)
	copy(q.nodes[n+2:], q.nodes[n+1:])
	q.nodes[n+1] = tail
}

// Each walks the list in order. The slice handed to fn aliases node storage
// and must not be retained.
func (q *quicklist) Each(fn func(i int, e []byte) bool) {
	idx := 0
	for _, node := range q.nodes {
		stop := false
		node.Each(func(_ int, e []byte) bool {
			if !fn(idx, e) {
				stop = true
				return false
			}
			idx++
			return true
		})
		if stop {
			return
		}
	}
}

// List is an ordered sequence of byte strings, listpack-encoded while small
// and quicklist-encoded once it grows (ADR-006).
type List struct {
	lp *listpack
	ql *quicklist
}

func newList() *List { return &List{lp: newListpack(0)} }

// Len returns the element count.
func (l *List) Len() int {
	if l.ql != nil {
		return l.ql.Len()
	}
	return l.lp.Len()
}

// Encoding reports the physical representation in use.
func (l *List) Encoding() Encoding {
	if l.ql != nil {
		return EncodingQuicklist
	}
	return EncodingListpack
}

// EstimatedSize reports the bytes attributable to this list.
func (l *List) EstimatedSize() int64 {
	if l.ql != nil {
		return l.ql.EstimatedSize()
	}
	return l.lp.EstimatedSize()
}

// Clone returns an independent copy.
func (l *List) Clone() any {
	if l.ql != nil {
		return &List{ql: l.ql.Clone()}
	}
	return &List{lp: l.lp.Clone()}
}

func (l *List) promoteIfNeeded(t *Thresholds, longest int) {
	if l.ql != nil {
		return
	}
	if l.lp.Len() <= t.ListMaxListpackSize && longest <= t.ListMaxListpackValue {
		return
	}
	q := newQuicklist(t.ListMaxListpackSize)
	l.lp.Each(func(_ int, e []byte) bool {
		q.PushBack(e)
		return true
	})
	l.ql, l.lp = q, nil
}

// Push adds an element at either end.
func (l *List) Push(v []byte, front bool, t *Thresholds) {
	if l.ql != nil {
		if front {
			l.ql.PushFront(v)
		} else {
			l.ql.PushBack(v)
		}
		return
	}
	if front {
		l.lp.InsertAt(0, v)
	} else {
		l.lp.Append(v)
	}
	l.promoteIfNeeded(t, len(v))
}

// Pop removes and returns an element from either end.
func (l *List) Pop(front bool) ([]byte, bool) {
	if l.ql != nil {
		if front {
			return l.ql.PopFront()
		}
		return l.ql.PopBack()
	}
	if l.lp.Len() == 0 {
		return nil, false
	}
	i := 0
	if !front {
		i = l.lp.Len() - 1
	}
	v := l.lp.CopyAt(i)
	l.lp.DeleteAt(i)
	return v, true
}

// At returns the element at a logical index, with negative indices counting
// from the end.
func (l *List) At(i int) ([]byte, bool) {
	if i < 0 {
		i += l.Len()
	}
	if i < 0 || i >= l.Len() {
		return nil, false
	}
	if l.ql != nil {
		return l.ql.At(i)
	}
	return l.lp.CopyAt(i), true
}

// SetAt replaces the element at an index.
func (l *List) SetAt(i int, v []byte, t *Thresholds) bool {
	if i < 0 {
		i += l.Len()
	}
	if i < 0 || i >= l.Len() {
		return false
	}
	if l.ql != nil {
		return l.ql.SetAt(i, v)
	}
	l.lp.SetAt(i, v)
	l.promoteIfNeeded(t, len(v))
	return true
}

// Each walks the list in order.
func (l *List) Each(fn func(i int, e []byte) bool) {
	if l.ql != nil {
		l.ql.Each(fn)
		return
	}
	l.lp.Each(fn)
}

// Range returns the elements between start and stop inclusive, with negative
// indices counting from the end.
func (l *List) Range(start, stop int64) [][]byte {
	lo, hi, ok := clampRange(start, stop, int64(l.Len()))
	if !ok {
		return nil
	}
	out := make([][]byte, 0, hi-lo+1)
	l.Each(func(i int, e []byte) bool {
		if int64(i) > hi {
			return false
		}
		if int64(i) >= lo {
			out = append(out, copyBytes(e))
		}
		return true
	})
	return out
}

// Insert places an element before or after the first occurrence of pivot. It
// returns the new length, or -1 when the pivot is absent.
func (l *List) Insert(pivot, v []byte, before bool, t *Thresholds) int {
	idx := -1
	l.Each(func(i int, e []byte) bool {
		if string(e) == string(pivot) {
			idx = i
			return false
		}
		return true
	})
	if idx < 0 {
		return -1
	}
	if !before {
		idx++
	}
	if l.ql != nil {
		l.ql.InsertAt(idx, v)
	} else {
		l.lp.InsertAt(idx, v)
		l.promoteIfNeeded(t, len(v))
	}
	return l.Len()
}

// Remove deletes occurrences of v. A positive count removes that many from
// the head, a negative count from the tail, and zero removes all.
func (l *List) Remove(count int, v []byte) int {
	var matches []int
	l.Each(func(i int, e []byte) bool {
		if string(e) == string(v) {
			matches = append(matches, i)
		}
		return true
	})
	if len(matches) == 0 {
		return 0
	}
	switch {
	case count > 0 && count < len(matches):
		matches = matches[:count]
	case count < 0:
		if n := -count; n < len(matches) {
			matches = matches[len(matches)-n:]
		}
	}
	// Deleting from the back keeps the earlier indices valid.
	for i := len(matches) - 1; i >= 0; i-- {
		l.deleteAt(matches[i])
	}
	return len(matches)
}

func (l *List) deleteAt(i int) {
	if l.ql != nil {
		l.ql.DeleteAt(i)
		return
	}
	l.lp.DeleteAt(i)
}

// Trim keeps only the elements between start and stop inclusive.
func (l *List) Trim(start, stop int64) {
	lo, hi, ok := clampRange(start, stop, int64(l.Len()))
	if !ok {
		l.clear()
		return
	}
	for i := l.Len() - 1; int64(i) > hi; i-- {
		l.deleteAt(i)
	}
	for i := int64(0); i < lo; i++ {
		l.deleteAt(0)
	}
}

func (l *List) clear() {
	if l.ql != nil {
		l.ql.nodes, l.ql.count = nil, 0
		return
	}
	l.lp.Truncate(0)
}

// Pos returns the indices of up to count occurrences of v, searching from
// the head when rank is positive and from the tail when it is negative.
// maxlen, when positive, bounds how many elements are examined.
func (l *List) Pos(v []byte, rank, count, maxlen int) []int {
	var all []int
	examined := 0
	l.Each(func(i int, e []byte) bool {
		examined++
		if string(e) == string(v) {
			all = append(all, i)
		}
		return maxlen <= 0 || examined < maxlen
	})
	if rank < 0 {
		// Reverse so that rank counts back from the tail.
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
		rank = -rank
	}
	if rank < 1 {
		rank = 1
	}
	if rank-1 >= len(all) {
		return nil
	}
	all = all[rank-1:]
	if count > 0 && count < len(all) {
		all = all[:count]
	}
	return all
}

// ---------------------------------------------------------------- database

func (db *DB) listAt(s *shard, key []byte) (*Object, *List, error) {
	o, err := db.collectionAt(s, key, TypeList)
	if err != nil || o == nil {
		return nil, nil, err
	}
	return o, o.Value.(*List), nil
}

// LPush adds elements at one end, returning the new length.
//
// When mustExist is set the list is not created, which is what the LPUSHX
// and RPUSHX variants need; a missing key then returns zero.
func (db *DB) LPush(key []byte, values [][]byte, front, mustExist bool) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, l, err := db.listAt(s, key)
	if err != nil {
		return 0, err
	}
	if l == nil {
		if mustExist {
			return 0, nil
		}
		l = newList()
		o = db.newCollection(s, key, TypeList, l)
	}
	t := db.ks.Thresholds()
	for _, v := range values {
		l.Push(v, front, t)
	}
	db.finishWrite(s, key, o, l)
	return int64(l.Len()), nil
}

// LPop removes up to count elements from one end.
func (db *DB) LPop(key []byte, count int, front bool) ([][]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, l, err := db.listAt(s, key)
	if err != nil || l == nil {
		return nil, err
	}
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		v, ok := l.Pop(front)
		if !ok {
			break
		}
		out = append(out, v)
	}
	if len(out) > 0 {
		db.finishWrite(s, key, o, l)
	}
	return out, nil
}

// LLen returns the element count.
func (db *DB) LLen(key []byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, l, err := db.listAt(s, key)
	if err != nil || l == nil {
		return 0, err
	}
	return int64(l.Len()), nil
}

// LIndex returns the element at an index.
func (db *DB) LIndex(key []byte, i int64) ([]byte, bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, l, err := db.listAt(s, key)
	if err != nil || l == nil {
		return nil, false, err
	}
	v, ok := l.At(int(i))
	return v, ok, nil
}

// LSet replaces the element at an index. It reports ErrNoSuchKey when the
// key is absent and ErrOutOfRange when the index is.
func (db *DB) LSet(key []byte, i int64, v []byte) error {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, l, err := db.listAt(s, key)
	if err != nil {
		return err
	}
	if l == nil {
		return ErrNoSuchKey
	}
	if !l.SetAt(int(i), v, db.ks.Thresholds()) {
		return ErrOutOfRange
	}
	db.finishWrite(s, key, o, l)
	return nil
}

// LRange returns a range of elements.
func (db *DB) LRange(key []byte, start, stop int64) ([][]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, l, err := db.listAt(s, key)
	if err != nil || l == nil {
		return nil, err
	}
	return l.Range(start, stop), nil
}

// LInsert places an element next to a pivot, returning the new length, zero
// when the key is absent, or -1 when the pivot is not found.
func (db *DB) LInsert(key, pivot, v []byte, before bool) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, l, err := db.listAt(s, key)
	if err != nil || l == nil {
		return 0, err
	}
	n := l.Insert(pivot, v, before, db.ks.Thresholds())
	if n < 0 {
		return -1, nil
	}
	db.finishWrite(s, key, o, l)
	return int64(n), nil
}

// LRem removes occurrences of an element.
func (db *DB) LRem(key []byte, count int, v []byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, l, err := db.listAt(s, key)
	if err != nil || l == nil {
		return 0, err
	}
	n := l.Remove(count, v)
	if n > 0 {
		db.finishWrite(s, key, o, l)
	}
	return int64(n), nil
}

// LTrim keeps only a range of elements.
func (db *DB) LTrim(key []byte, start, stop int64) error {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, l, err := db.listAt(s, key)
	if err != nil || l == nil {
		return err
	}
	l.Trim(start, stop)
	db.finishWrite(s, key, o, l)
	return nil
}

// LPos returns the indices of occurrences of an element.
func (db *DB) LPos(key, v []byte, rank, count, maxlen int) ([]int, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, l, err := db.listAt(s, key)
	if err != nil || l == nil {
		return nil, err
	}
	return l.Pos(v, rank, count, maxlen), nil
}

// LMove atomically pops from one list and pushes onto another, returning the
// moved element. Source and destination may be the same key, which rotates.
func (db *DB) LMove(src, dst []byte, srcFront, dstFront bool) ([]byte, error) {
	locked := db.lockKeys([][]byte{src, dst})
	defer db.unlockShards(locked)

	ss, ds := db.shardFor(src), db.shardFor(dst)
	so, err := db.lookupType(ss, src, TypeList)
	if err != nil || so == nil {
		return nil, err
	}
	source := so.Value.(*List)
	t := db.ks.Thresholds()

	if string(src) == string(dst) {
		v, ok := source.Pop(srcFront)
		if !ok {
			return nil, nil
		}
		source.Push(v, dstFront, t)
		db.finishWrite(ss, src, so, source)
		return v, nil
	}

	do, err := db.lookupType(ds, dst, TypeList)
	if err != nil {
		return nil, err
	}
	v, ok := source.Pop(srcFront)
	if !ok {
		return nil, nil
	}
	if do == nil {
		target := newList()
		target.Push(v, dstFront, t)
		db.newCollection(ds, dst, TypeList, target)
		db.touched(dst)
	} else {
		target := do.Value.(*List)
		target.Push(v, dstFront, t)
		db.finishWrite(ds, dst, do, target)
	}
	db.finishWrite(ss, src, so, source)
	return v, nil
}
