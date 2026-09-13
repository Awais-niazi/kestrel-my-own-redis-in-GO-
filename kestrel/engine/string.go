package engine

import (
	"math"
	"strconv"
	"strings"
)

// SetOptions carries the modifiers of the SET command.
type SetOptions struct {
	NX      bool  // only set when the key does not exist
	XX      bool  // only set when the key already exists
	Get     bool  // return the previous value
	KeepTTL bool  // retain any existing TTL
	At      int64 // absolute expiry in ms; 0 means "no TTL"
}

// SetResult reports what SET did.
type SetResult struct {
	Set       bool   // the write happened
	Old       []byte // previous value, when Get was requested
	OldExists bool   // whether a previous value existed
}

// Get returns the value of a string key.
//
// The returned slice aliases stored data and must not be modified.
func (db *DB) Get(key []byte) ([]byte, bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil || o == nil {
		return nil, false, err
	}
	return o.stringBytes(), true, nil
}

// Set writes a string value subject to opts.
func (db *DB) Set(key, val []byte, opts SetOptions) (SetResult, error) {
	if len(val) > db.ks.opts.MaxStringLength {
		return SetResult{}, ErrValueTooLarge
	}
	s := db.lockKey(key)
	defer db.unlockKey(s)

	old := db.lookup(s, key)
	var res SetResult
	if opts.Get {
		if old != nil {
			if old.Type != TypeString {
				return SetResult{}, ErrWrongType
			}
			res.Old, res.OldExists = old.stringBytes(), true
		}
	}
	if (opts.NX && old != nil) || (opts.XX && old == nil) {
		return res, nil
	}

	o := newStringObject(val)
	switch {
	case opts.At > 0:
		o.ExpireAt = opts.At
	case opts.KeepTTL && old != nil:
		o.ExpireAt = old.ExpireAt
	}
	db.store(s, key, o)
	db.touched(key)
	res.Set = true
	return res, nil
}

// GetDel atomically returns and deletes a string key.
func (db *DB) GetDel(key []byte) ([]byte, bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil || o == nil {
		return nil, false, err
	}
	val := o.stringBytes()
	db.removeLocked(s, string(key), o)
	db.touched(key)
	return val, true, nil
}

// GetEx returns a string value and optionally re-dates it. persist clears the
// TTL; at > 0 sets one; both zero leaves the TTL untouched.
func (db *DB) GetEx(key []byte, at int64, persist bool) ([]byte, bool, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil || o == nil {
		return nil, false, err
	}
	val := o.stringBytes()
	switch {
	case persist && o.ExpireAt != 0:
		db.setExpireLocked(s, string(key), o, 0)
		db.touched(key)
	case at > 0:
		if at <= db.ks.Now() {
			db.removeLocked(s, string(key), o)
		} else {
			db.setExpireLocked(s, string(key), o, at)
		}
		db.touched(key)
	}
	return val, true, nil
}

// StrLen returns the length of a string value.
func (db *DB) StrLen(key []byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil || o == nil {
		return 0, err
	}
	return int64(o.stringLen()), nil
}

// Append concatenates val onto a string key, creating it if absent, and
// returns the resulting length.
func (db *DB) Append(key, val []byte) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil {
		return 0, err
	}
	if o == nil {
		if len(val) > db.ks.opts.MaxStringLength {
			return 0, ErrValueTooLarge
		}
		n := newRawObject(copyBytes(val))
		db.store(s, key, n)
		db.touched(key)
		return int64(len(val)), nil
	}
	cur := o.stringBytes()
	if len(cur)+len(val) > db.ks.opts.MaxStringLength {
		return 0, ErrValueTooLarge
	}
	// A fresh allocation, never an in-place append: a concurrent reader may
	// still hold the old slice (see the package concurrency contract).
	next := make([]byte, len(cur)+len(val))
	copy(next, cur)
	copy(next[len(cur):], val)
	db.replaceValue(s, key, o, newRawObject(next))
	db.touched(key)
	return int64(len(next)), nil
}

// GetRange returns the substring of a string value between start and end
// inclusive, with negative offsets counting from the end.
func (db *DB) GetRange(key []byte, start, end int64) ([]byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil || o == nil {
		return nil, err
	}
	val := o.stringBytes()
	lo, hi, ok := clampRange(start, end, int64(len(val)))
	if !ok {
		return nil, nil
	}
	return val[lo : hi+1], nil
}

// SetRange overwrites part of a string value starting at offset, zero-padding
// any gap, and returns the resulting length.
func (db *DB) SetRange(key []byte, offset int64, val []byte) (int64, error) {
	if offset < 0 {
		return 0, ErrOutOfRange
	}
	if offset+int64(len(val)) > int64(db.ks.opts.MaxStringLength) {
		return 0, ErrValueTooLarge
	}
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil {
		return 0, err
	}
	var cur []byte
	if o != nil {
		cur = o.stringBytes()
	}
	if len(val) == 0 {
		return int64(len(cur)), nil
	}
	size := int64(len(cur))
	if end := offset + int64(len(val)); end > size {
		size = end
	}
	next := make([]byte, size)
	copy(next, cur)
	copy(next[offset:], val)
	if o == nil {
		db.store(s, key, newRawObject(next))
	} else {
		db.replaceValue(s, key, o, newRawObject(next))
	}
	db.touched(key)
	return size, nil
}

// IncrBy adds delta to an integer string value and returns the result.
func (db *DB) IncrBy(key []byte, delta int64) (int64, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil {
		return 0, err
	}
	var cur int64
	if o != nil {
		cur, err = objectInt(o)
		if err != nil {
			return 0, err
		}
	}
	if (delta > 0 && cur > math.MaxInt64-delta) || (delta < 0 && cur < math.MinInt64-delta) {
		return 0, ErrOverflow
	}
	next := cur + delta
	if o == nil {
		db.store(s, key, newIntObject(next))
	} else {
		db.replaceValue(s, key, o, withExpiry(newIntObject(next), o.ExpireAt))
	}
	db.touched(key)
	return next, nil
}

// IncrByFloat adds delta to a numeric string value and returns the result
// along with its canonical rendering, which is what gets logged as the
// replacement SET (ADR-008).
func (db *DB) IncrByFloat(key []byte, delta float64) (float64, []byte, error) {
	s := db.lockKey(key)
	defer db.unlockKey(s)
	o, err := db.lookupType(s, key, TypeString)
	if err != nil {
		return 0, nil, err
	}
	var cur float64
	if o != nil {
		cur, err = parseFloat(o.stringBytes())
		if err != nil {
			return 0, nil, err
		}
	}
	next := cur + delta
	if math.IsNaN(next) || math.IsInf(next, 0) {
		return 0, nil, ErrNotFloat
	}
	rendered := FormatFloat(next)
	repl := newStringObject(rendered)
	if o == nil {
		db.store(s, key, repl)
	} else {
		db.replaceValue(s, key, o, withExpiry(repl, o.ExpireAt))
	}
	db.touched(key)
	return next, rendered, nil
}

// MGet returns the values of several string keys atomically. Missing keys and
// keys of the wrong type yield a nil entry, matching the reference behaviour.
func (db *DB) MGet(keys [][]byte) [][]byte {
	locked := db.lockKeys(keys)
	defer db.unlockShards(locked)

	out := make([][]byte, len(keys))
	for i, k := range keys {
		o := db.lookupRead(db.shardFor(k), k)
		if o != nil && o.Type == TypeString {
			out[i] = o.stringBytes()
		}
	}
	return out
}

// MSet writes several key/value pairs atomically. pairs must have even length.
func (db *DB) MSet(pairs [][]byte) {
	keys := everyOther(pairs)
	locked := db.lockKeys(keys)
	defer db.unlockShards(locked)

	for i := 0; i < len(pairs); i += 2 {
		k, v := pairs[i], pairs[i+1]
		db.store(db.shardFor(k), k, newStringObject(v))
		db.touched(k)
	}
}

// MSetNX writes several key/value pairs atomically, but only if none of the
// keys already exists.
func (db *DB) MSetNX(pairs [][]byte) bool {
	keys := everyOther(pairs)
	locked := db.lockKeys(keys)
	defer db.unlockShards(locked)

	for _, k := range keys {
		if db.lookup(db.shardFor(k), k) != nil {
			return false
		}
	}
	for i := 0; i < len(pairs); i += 2 {
		k, v := pairs[i], pairs[i+1]
		db.store(db.shardFor(k), k, newStringObject(v))
		db.touched(k)
	}
	return true
}

func everyOther(pairs [][]byte) [][]byte {
	keys := make([][]byte, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		keys = append(keys, pairs[i])
	}
	return keys
}

// replaceValue swaps an object's payload while preserving the memory
// accounting. The shard must be locked.
func (db *DB) replaceValue(s *shard, key []byte, old, next *Object) {
	next.ExpireAt = old.ExpireAt
	next.LRU = old.LRU
	db.store(s, key, next)
}

func withExpiry(o *Object, at int64) *Object {
	o.ExpireAt = at
	return o
}

// objectInt reads a string object as an integer.
func objectInt(o *Object) (int64, error) {
	if o.Encoding == EncodingInt {
		return o.Value.(int64), nil
	}
	b := o.Value.([]byte)
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, ErrNotInteger
	}
	return n, nil
}

func parseFloat(b []byte) (float64, error) {
	s := string(b)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return 0, ErrNotFloat
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) {
		return 0, ErrNotFloat
	}
	return f, nil
}

// ParseFloat exposes the value parser used for numeric string values.
func ParseFloat(b []byte) (float64, error) { return parseFloat(b) }

// FormatFloat renders a float the way INCRBYFLOAT does: fixed notation with
// trailing zeros removed, so that repeated increments produce stable,
// replay-safe text (ADR-008).
func FormatFloat(f float64) []byte {
	if f == math.Trunc(f) && math.Abs(f) < 1e17 {
		return strconv.AppendInt(nil, int64(f), 10)
	}
	// Fixed notation with the shortest digit string that round-trips
	// exactly. The reference implementation renders %.17Lf from a long
	// double and trims zeros; on float64 that leaks representation noise
	// ("10.59999999999999964"), whereas the shortest round-trip form is both
	// closer to what a user expects and exactly reversible, which is what
	// replay safety actually requires.
	return strconv.AppendFloat(nil, f, 'f', -1, 64)
}

// clampRange resolves the inclusive [start,end] convention shared by
// GETRANGE, LRANGE and friends. It reports false when the range is empty.
func clampRange(start, end, length int64) (int64, int64, bool) {
	if length == 0 {
		return 0, 0, false
	}
	if start < 0 {
		start += length
	}
	if end < 0 {
		end += length
	}
	if start < 0 {
		start = 0
	}
	if end >= length {
		end = length - 1
	}
	if start > end || start >= length {
		return 0, 0, false
	}
	return start, end, true
}
