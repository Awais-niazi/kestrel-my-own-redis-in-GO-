package engine

import "encoding/binary"

// listpack is the flat encoding every small collection uses (ADR-006).
//
// Entries live end to end in one byte slice, each as a uvarint length
// followed by its bytes. The point is pointer count, not just bytes: a
// hundred-field hash held this way is one pointer for the garbage collector
// to scan, where a map[string][]byte is two hundred and more. GC scan cost
// tracks pointers rather than bytes, so this is what keeps small collections
// -- the shape that dominates real workloads -- off the tail-latency budget
// (§11).
//
// Every operation is O(n) in the entry count. That is deliberate: these are
// used only below the configured promotion thresholds, where n is at most a
// few hundred and a linear scan over one contiguous buffer beats chasing
// pointers through a map.
//
// A listpack is used directly by lists and sets, and as alternating pairs by
// hashes (field, value) and sorted sets (member, score).
type listpack struct {
	buf   []byte
	count int
}

func newListpack(capacity int) *listpack {
	return &listpack{buf: make([]byte, 0, capacity)}
}

// Len returns the number of entries.
func (l *listpack) Len() int { return l.count }

// ByteLen returns the encoded size, which is what the promotion thresholds
// on value length are checked against.
func (l *listpack) ByteLen() int { return len(l.buf) }

// EstimatedSize reports the bytes attributable to this listpack.
func (l *listpack) EstimatedSize() int64 { return int64(sliceHeaderSize + cap(l.buf) + 16) }

// At returns entry i without copying. The result aliases the buffer and is
// invalidated by the next mutation, so it is for use inside the engine only,
// under the shard lock. Callers outside the engine get CopyAt.
func (l *listpack) At(i int) []byte {
	off, size, hdr := l.locate(i)
	if off < 0 {
		return nil
	}
	return l.buf[off+hdr : off+hdr+size]
}

// CopyAt returns a private copy of entry i.
func (l *listpack) CopyAt(i int) []byte {
	e := l.At(i)
	if e == nil {
		return nil
	}
	return copyBytes(e)
}

// locate returns the offset of entry i, the length of its payload, and the
// size of its length header. It returns -1 when i is out of range.
func (l *listpack) locate(i int) (off, size, hdr int) {
	if i < 0 || i >= l.count {
		return -1, 0, 0
	}
	off = 0
	for n := 0; ; n++ {
		v, w := binary.Uvarint(l.buf[off:])
		if n == i {
			return off, int(v), w
		}
		off += w + int(v)
	}
}

// entryEnd returns the offset just past entry i.
func (l *listpack) entryEnd(i int) int {
	off, size, hdr := l.locate(i)
	if off < 0 {
		return len(l.buf)
	}
	return off + hdr + size
}

// Append adds an entry at the end.
func (l *listpack) Append(b []byte) {
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	l.buf = append(l.buf, hdr[:n]...)
	l.buf = append(l.buf, b...)
	l.count++
}

// AppendMany adds several entries, which is how pair-based collections keep
// a field and its value adjacent.
func (l *listpack) AppendMany(entries ...[]byte) {
	for _, e := range entries {
		l.Append(e)
	}
}

// InsertAt inserts an entry before position i. An i at or past the end
// appends.
func (l *listpack) InsertAt(i int, b []byte) {
	if i >= l.count {
		l.Append(b)
		return
	}
	off, _, _ := l.locate(i)
	if off < 0 {
		off = 0
	}
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	l.splice(off, off, hdr[:n], b)
	l.count++
}

// SetAt replaces entry i.
func (l *listpack) SetAt(i int, b []byte) {
	off, size, hdr := l.locate(i)
	if off < 0 {
		return
	}
	var h [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(h[:], uint64(len(b)))
	l.splice(off, off+hdr+size, h[:n], b)
}

// DeleteAt removes entry i.
func (l *listpack) DeleteAt(i int) {
	off, size, hdr := l.locate(i)
	if off < 0 {
		return
	}
	l.splice(off, off+hdr+size)
	l.count--
}

// DeleteRun removes n consecutive entries starting at i. Removing a run in
// one pass matters for pair collections, where a field and its value are
// deleted together.
func (l *listpack) DeleteRun(i, n int) {
	if n <= 0 || i < 0 || i >= l.count {
		return
	}
	if i+n > l.count {
		n = l.count - i
	}
	start, _, _ := l.locate(i)
	end := l.entryEnd(i + n - 1)
	l.splice(start, end)
	l.count -= n
}

// Truncate keeps only the first n entries.
func (l *listpack) Truncate(n int) {
	if n >= l.count {
		return
	}
	if n <= 0 {
		l.buf, l.count = l.buf[:0], 0
		return
	}
	l.buf = l.buf[:l.entryEnd(n-1)]
	l.count = n
}

// TrimFront drops the first n entries.
func (l *listpack) TrimFront(n int) { l.DeleteRun(0, n) }

// splice replaces buf[from:to] with the concatenation of parts.
//
// The buffer is rebuilt rather than shifted in place whenever it must grow,
// because a reader elsewhere may still hold a slice into the old array. That
// is the same immutability rule the scalar types follow, applied to a
// contiguous buffer.
func (l *listpack) splice(from, to int, parts ...[]byte) {
	added := 0
	for _, p := range parts {
		added += len(p)
	}
	removed := to - from
	need := len(l.buf) - removed + added

	if need <= cap(l.buf) && added <= removed {
		// Shrinking or same size: shift the tail left in place.
		w := from
		for _, p := range parts {
			w += copy(l.buf[w:], p)
		}
		copy(l.buf[w:], l.buf[to:])
		l.buf = l.buf[:need]
		return
	}
	next := make([]byte, 0, growCap(need))
	next = append(next, l.buf[:from]...)
	for _, p := range parts {
		next = append(next, p...)
	}
	next = append(next, l.buf[to:]...)
	l.buf = next
}

// growCap rounds an allocation up so that repeated appends do not reallocate
// on every entry.
func growCap(n int) int {
	c := 64
	for c < n {
		c *= 2
	}
	return c
}

// Each walks the entries in order, stopping early if fn returns false. The
// slice handed to fn aliases the buffer and must not be retained.
func (l *listpack) Each(fn func(i int, entry []byte) bool) {
	off := 0
	for i := 0; i < l.count; i++ {
		v, w := binary.Uvarint(l.buf[off:])
		if !fn(i, l.buf[off+w:off+w+int(v)]) {
			return
		}
		off += w + int(v)
	}
}

// IndexOf returns the position of the first entry equal to b, or -1.
func (l *listpack) IndexOf(b []byte) int {
	found := -1
	l.Each(func(i int, entry []byte) bool {
		if string(entry) == string(b) {
			found = i
			return false
		}
		return true
	})
	return found
}

// IndexOfStep searches only positions i where i%step == offset, which is how
// a pair-encoded hash finds a field without matching a value.
func (l *listpack) IndexOfStep(b []byte, step, offset int) int {
	found := -1
	l.Each(func(i int, entry []byte) bool {
		if i%step == offset && string(entry) == string(b) {
			found = i
			return false
		}
		return true
	})
	return found
}

// MaxEntryLen returns the longest payload, for threshold checks.
func (l *listpack) MaxEntryLen() int {
	max := 0
	l.Each(func(_ int, entry []byte) bool {
		if len(entry) > max {
			max = len(entry)
		}
		return true
	})
	return max
}

// Clone returns an independent copy.
func (l *listpack) Clone() *listpack {
	next := make([]byte, len(l.buf))
	copy(next, l.buf)
	return &listpack{buf: next, count: l.count}
}
