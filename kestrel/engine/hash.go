package engine

import (
	"math"
	"math/rand"
	"strconv"
)

// Hash is a field-to-value map.
//
// Below the configured thresholds it is a listpack holding alternating field
// and value entries, which is one pointer in total. Above them it is a Go
// map, which is fast but pointer-dense. Promotion is one-way (ADR-006).
//
// Every accessor returns data that is safe to retain after the shard lock is
// released: the listpack path copies, because its buffer is rewritten in
// place by later writes, and the map path can alias, because a stored value
// is replaced rather than modified.
type Hash struct {
	lp *listpack         // pairs: field, value, field, value
	m  map[string][]byte // non-nil once promoted
}

func newHash() *Hash { return &Hash{lp: newListpack(0)} }

// Len returns the field count.
func (h *Hash) Len() int {
	if h.m != nil {
		return len(h.m)
	}
	return h.lp.Len() / 2
}

// Encoding reports the physical representation in use.
func (h *Hash) Encoding() Encoding {
	if h.m != nil {
		return EncodingHashtable
	}
	return EncodingListpack
}

// EstimatedSize reports the bytes attributable to this hash.
func (h *Hash) EstimatedSize() int64 {
	if h.m == nil {
		return h.lp.EstimatedSize()
	}
	n := int64(48)
	for k, v := range h.m {
		n += int64(mapEntryOverhead+len(k)+sliceHeaderSize) + int64(cap(v))
	}
	return n
}

// Clone returns an independent copy.
func (h *Hash) Clone() any {
	if h.m == nil {
		return &Hash{lp: h.lp.Clone()}
	}
	m := make(map[string][]byte, len(h.m))
	for k, v := range h.m {
		m[k] = copyBytes(v)
	}
	return &Hash{m: m}
}

// Get returns the value of a field.
func (h *Hash) Get(field []byte) ([]byte, bool) {
	if h.m != nil {
		v, ok := h.m[string(field)]
		return v, ok
	}
	i := h.lp.IndexOfStep(field, 2, 0)
	if i < 0 {
		return nil, false
	}
	return h.lp.CopyAt(i + 1), true
}

// Exists reports whether a field is present, without materializing a value.
func (h *Hash) Exists(field []byte) bool {
	if h.m != nil {
		_, ok := h.m[string(field)]
		return ok
	}
	return h.lp.IndexOfStep(field, 2, 0) >= 0
}

// ValueLen returns the length of a field's value, without copying it.
func (h *Hash) ValueLen(field []byte) (int, bool) {
	if h.m != nil {
		v, ok := h.m[string(field)]
		return len(v), ok
	}
	i := h.lp.IndexOfStep(field, 2, 0)
	if i < 0 {
		return 0, false
	}
	return len(h.lp.At(i + 1)), true
}

// Set writes a field and reports whether it was newly created.
func (h *Hash) Set(field, value []byte, t *Thresholds) bool {
	if h.m != nil {
		_, existed := h.m[string(field)]
		h.m[string(field)] = copyBytes(value)
		return !existed
	}
	if i := h.lp.IndexOfStep(field, 2, 0); i >= 0 {
		h.lp.SetAt(i+1, value)
		h.promoteIfNeeded(t, len(value))
		return false
	}
	h.lp.AppendMany(field, value)
	h.promoteIfNeeded(t, max(len(field), len(value)))
	return true
}

// Delete removes a field and reports whether it was present.
func (h *Hash) Delete(field []byte) bool {
	if h.m != nil {
		if _, ok := h.m[string(field)]; !ok {
			return false
		}
		delete(h.m, string(field))
		return true
	}
	i := h.lp.IndexOfStep(field, 2, 0)
	if i < 0 {
		return false
	}
	h.lp.DeleteRun(i, 2)
	return true
}

// promoteIfNeeded converts to the map encoding once a threshold is crossed.
func (h *Hash) promoteIfNeeded(t *Thresholds, longest int) {
	if h.m != nil {
		return
	}
	if h.lp.Len()/2 <= t.HashMaxListpackEntries && longest <= t.HashMaxListpackValue {
		return
	}
	m := make(map[string][]byte, h.lp.Len()/2)
	var field []byte
	h.lp.Each(func(i int, e []byte) bool {
		if i%2 == 0 {
			field = copyBytes(e)
		} else {
			m[string(field)] = copyBytes(e)
		}
		return true
	})
	h.m, h.lp = m, nil
}

// Fields returns every field name.
func (h *Hash) Fields() [][]byte { return h.collect(true, false) }

// Values returns every value.
func (h *Hash) Values() [][]byte { return h.collect(false, true) }

// All returns alternating field and value entries.
func (h *Hash) All() [][]byte { return h.collect(true, true) }

func (h *Hash) collect(fields, values bool) [][]byte {
	per := 0
	if fields {
		per++
	}
	if values {
		per++
	}
	out := make([][]byte, 0, h.Len()*per)
	if h.m != nil {
		for k, v := range h.m {
			if fields {
				out = append(out, []byte(k))
			}
			if values {
				out = append(out, v)
			}
		}
		return out
	}
	h.lp.Each(func(i int, e []byte) bool {
		if (i%2 == 0 && fields) || (i%2 == 1 && values) {
			out = append(out, copyBytes(e))
		}
		return true
	})
	return out
}

// RandomFields returns count field names, with values interleaved when
// withValues is set.
//
// A negative count allows repeats and returns exactly that many; a positive
// count returns distinct fields, capped at the hash size.
func (h *Hash) RandomFields(count int, withValues bool, rng *rand.Rand) [][]byte {
	all := h.All() // field, value, field, value
	n := len(all) / 2
	if n == 0 {
		return nil
	}
	emit := func(out [][]byte, idx int) [][]byte {
		out = append(out, all[idx*2])
		if withValues {
			out = append(out, all[idx*2+1])
		}
		return out
	}
	if count < 0 {
		out := make([][]byte, 0, -count)
		for i := 0; i < -count; i++ {
			out = emit(out, rng.Intn(n))
		}
		return out
	}
	if count > n {
		count = n
	}
	perm := rng.Perm(n)[:count]
	out := make([][]byte, 0, count)
	for _, i := range perm {
		out = emit(out, i)
	}
	return out
}

// ---------------------------------------------------------------- database

// hashAt returns the hash stored at key, or nil when absent.
func (db *DB) hashAt(s *shard, key []byte) (*Object, *Hash, error) {
	o, err := db.collectionAt(s, key, TypeHash)
	if err != nil || o == nil {
		return nil, nil, err
	}
	return o, o.Value.(*Hash), nil
}

// HSet writes field/value pairs, returning how many fields were new.
func (db *DB) HSet(key []byte, pairs [][]byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, h, err := db.hashAt(s, key)
	if err != nil {
		return 0, err
	}
	if h == nil {
		h = newHash()
		o = db.newCollection(s, key, TypeHash, h)
	}
	t := db.ks.Thresholds()
	var added int64
	for i := 0; i+1 < len(pairs); i += 2 {
		if h.Set(pairs[i], pairs[i+1], t) {
			added++
		}
	}
	db.finishWrite(s, key, o, h)
	return added, nil
}

// HSetNX writes a field only when it does not already exist.
func (db *DB) HSetNX(key, field, value []byte) (bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, h, err := db.hashAt(s, key)
	if err != nil {
		return false, err
	}
	if h != nil && h.Exists(field) {
		return false, nil
	}
	if h == nil {
		h = newHash()
		o = db.newCollection(s, key, TypeHash, h)
	}
	h.Set(field, value, db.ks.Thresholds())
	db.finishWrite(s, key, o, h)
	return true, nil
}

// HGet returns the value of a field.
func (db *DB) HGet(key, field []byte) ([]byte, bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, h, err := db.hashAt(s, key)
	if err != nil || h == nil {
		return nil, false, err
	}
	v, ok := h.Get(field)
	return v, ok, nil
}

// HMGet returns the values of several fields, with a nil entry per miss.
func (db *DB) HMGet(key []byte, fields [][]byte) ([][]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	out := make([][]byte, len(fields))
	_, h, err := db.hashAt(s, key)
	if err != nil || h == nil {
		return out, err
	}
	for i, f := range fields {
		if v, ok := h.Get(f); ok {
			out[i] = v
		}
	}
	return out, nil
}

// HDel removes fields, returning how many existed.
func (db *DB) HDel(key []byte, fields [][]byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, h, err := db.hashAt(s, key)
	if err != nil || h == nil {
		return 0, err
	}
	var removed int64
	for _, f := range fields {
		if h.Delete(f) {
			removed++
		}
	}
	if removed > 0 {
		db.finishWrite(s, key, o, h)
	}
	return removed, nil
}

// HLen returns the field count.
func (db *DB) HLen(key []byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, h, err := db.hashAt(s, key)
	if err != nil || h == nil {
		return 0, err
	}
	return int64(h.Len()), nil
}

// HExists reports whether a field is present.
func (db *DB) HExists(key, field []byte) (bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, h, err := db.hashAt(s, key)
	if err != nil || h == nil {
		return false, err
	}
	return h.Exists(field), nil
}

// HStrLen returns the length of a field's value, or zero when absent.
func (db *DB) HStrLen(key, field []byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, h, err := db.hashAt(s, key)
	if err != nil || h == nil {
		return 0, err
	}
	n, _ := h.ValueLen(field)
	return int64(n), nil
}

// HashPart selects what HKEYS, HVALS and HGETALL return.
type HashPart uint8

// Which parts of a hash to read.
const (
	HashFields HashPart = iota
	HashValues
	HashAll
)

// HRead returns the fields, the values, or both.
func (db *DB) HRead(key []byte, part HashPart) ([][]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, h, err := db.hashAt(s, key)
	if err != nil || h == nil {
		return nil, err
	}
	switch part {
	case HashFields:
		return h.Fields(), nil
	case HashValues:
		return h.Values(), nil
	default:
		return h.All(), nil
	}
}

// HIncrBy adds delta to a field's integer value.
func (db *DB) HIncrBy(key, field []byte, delta int64) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, h, err := db.hashAt(s, key)
	if err != nil {
		return 0, err
	}
	if h == nil {
		h = newHash()
		o = db.newCollection(s, key, TypeHash, h)
	}
	var cur int64
	if v, ok := h.Get(field); ok {
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return 0, ErrNotInteger
		}
		cur = n
	}
	if (delta > 0 && cur > math.MaxInt64-delta) || (delta < 0 && cur < math.MinInt64-delta) {
		return 0, ErrOverflow
	}
	next := cur + delta
	h.Set(field, strconv.AppendInt(nil, next, 10), db.ks.Thresholds())
	db.finishWrite(s, key, o, h)
	return next, nil
}

// HIncrByFloat adds delta to a field's numeric value, returning the result
// and its canonical rendering, which is what gets logged (ADR-008).
func (db *DB) HIncrByFloat(key, field []byte, delta float64) (float64, []byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)

	o, h, err := db.hashAt(s, key)
	if err != nil {
		return 0, nil, err
	}
	if h == nil {
		h = newHash()
		o = db.newCollection(s, key, TypeHash, h)
	}
	var cur float64
	if v, ok := h.Get(field); ok {
		f, err := parseFloat(v)
		if err != nil {
			return 0, nil, ErrNotFloat
		}
		cur = f
	}
	next := cur + delta
	if math.IsNaN(next) || math.IsInf(next, 0) {
		return 0, nil, ErrNotFloat
	}
	rendered := FormatFloat(next)
	h.Set(field, rendered, db.ks.Thresholds())
	db.finishWrite(s, key, o, h)
	return next, rendered, nil
}

// HRandField returns random field names, optionally with their values.
func (db *DB) HRandField(key []byte, count int, withValues bool) ([][]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	_, h, err := db.hashAt(s, key)
	if err != nil || h == nil {
		return nil, err
	}
	return h.RandomFields(count, withValues, rand.New(rand.NewSource(rand.Int63()))), nil
}
