// Package engine holds the keyspace: shards, the value representation, the
// data structures, expiration, and memory accounting.
//
// The engine is a library first and a server second (ADR-017). It imports
// nothing from net, resp, or any layer above it, which is enforced by
// TestEngineImportGraph.
//
// # Concurrency contract
//
// Every exported operation performs its own locking. Callers never see an
// *Object, only values derived from one, because an *Object is only stable
// while its shard lock is held.
//
// Byte slices returned by read operations alias data stored in the keyspace
// and MUST NOT be modified by the caller. This is safe because the engine
// never mutates a stored value in place: every mutation installs a freshly
// allocated value. A reader holding a slice from before a concurrent write
// therefore sees a consistent older value rather than a torn one.
package engine

import "strconv"

// ObjectType is the logical type reported by TYPE.
type ObjectType uint8

// Logical value types.
const (
	TypeString ObjectType = iota
	TypeList
	TypeHash
	TypeSet
	TypeZSet
)

// String returns the name TYPE replies with.
func (t ObjectType) String() string {
	switch t {
	case TypeString:
		return "string"
	case TypeList:
		return "list"
	case TypeHash:
		return "hash"
	case TypeSet:
		return "set"
	case TypeZSet:
		return "zset"
	default:
		return "unknown"
	}
}

// Encoding is the physical representation currently in use. Encodings are
// promoted on threshold crossing and never demoted (ADR-006).
type Encoding uint8

// Physical encodings.
const (
	EncodingRaw       Encoding = iota // []byte
	EncodingInt                       // int64
	EncodingListpack                  // flat byte slab
	EncodingQuicklist                 // linked list of listpacks
	EncodingIntset                    // sorted []int64
	EncodingHashtable                 // map
	EncodingSkiplist                  // skiplist + map
)

// String returns the name OBJECT ENCODING replies with.
func (e Encoding) String() string {
	switch e {
	case EncodingRaw:
		return "raw"
	case EncodingInt:
		return "int"
	case EncodingListpack:
		return "listpack"
	case EncodingQuicklist:
		return "quicklist"
	case EncodingIntset:
		return "intset"
	case EncodingHashtable:
		return "hashtable"
	case EncodingSkiplist:
		return "skiplist"
	default:
		return "unknown"
	}
}

// Object is a stored value.
//
// ExpireAt lives on the object rather than in a separate dictionary so that
// the expiry check on the hot read path costs no extra map lookup. The shard
// still keeps an index of keys carrying a TTL, because the active expiry
// cycle needs something to sample (see expire.go).
type Object struct {
	Type     ObjectType
	Encoding Encoding
	Value    any
	ExpireAt int64  // absolute ms since epoch; 0 means no expiry
	LRU      uint32 // access clock (LRU) or frequency counter (LFU)
}

// newStringObject builds a string object, choosing the int encoding when the
// value is an integer in canonical form.
func newStringObject(val []byte) *Object {
	if n, ok := canonicalInt(val); ok {
		return &Object{Type: TypeString, Encoding: EncodingInt, Value: n}
	}
	return &Object{Type: TypeString, Encoding: EncodingRaw, Value: copyBytes(val)}
}

// newIntObject builds an int-encoded string object.
func newIntObject(n int64) *Object {
	return &Object{Type: TypeString, Encoding: EncodingInt, Value: n}
}

// newRawObject builds a raw string object from an already-private slice.
func newRawObject(b []byte) *Object {
	return &Object{Type: TypeString, Encoding: EncodingRaw, Value: b}
}

// stringBytes renders a string object's value.
//
// For raw encoding the returned slice aliases stored data and must not be
// modified. For int encoding it is freshly formatted.
func (o *Object) stringBytes() []byte {
	if o.Encoding == EncodingInt {
		return strconv.AppendInt(nil, o.Value.(int64), 10)
	}
	return o.Value.([]byte)
}

// stringLen is the length of the string value without rendering it.
func (o *Object) stringLen() int {
	if o.Encoding == EncodingInt {
		return len(strconv.AppendInt(nil, o.Value.(int64), 10))
	}
	return len(o.Value.([]byte))
}

// canonicalInt reports whether b is an int64 whose decimal rendering is
// byte-identical to b. "007" and "+7" are integers but are not canonical, so
// they keep the raw encoding and round-trip unchanged.
func canonicalInt(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 20 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, false
	}
	var tmp [20]byte
	if string(strconv.AppendInt(tmp[:0], n, 10)) != string(b) {
		return 0, false
	}
	return n, true
}

// copyBytes returns a private copy with capacity equal to length, so that a
// later append can never write into a slice another goroutine is reading.
func copyBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
