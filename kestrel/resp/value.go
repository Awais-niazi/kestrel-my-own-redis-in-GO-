// Package resp implements the RESP wire protocol codec.
//
// Replies are built as a typed tree (Value) and serialized per connection
// according to the negotiated protocol version, so command implementations
// never know whether the client speaks RESP2 or RESP3 (ADR-016).
//
// This package has no dependency on the engine or on net.
package resp

import "strconv"

// Kind discriminates the reply types. The RESP3-only kinds degrade to a
// RESP2 equivalent at serialization time; see Writer.
type Kind uint8

const (
	KindSimple    Kind = iota // +OK
	KindError                 // -ERR ...
	KindInt                   // :123
	KindBulk                  // $3\r\nfoo
	KindArray                 // *2\r\n...
	KindNull                  // _  (RESP3) / $-1 (RESP2)
	KindNullArray             // _  (RESP3) / *-1 (RESP2)
	KindMap                   // %  (RESP3) / flattened array (RESP2)
	KindSet                   // ~  (RESP3) / array (RESP2)
	KindDouble                // ,  (RESP3) / bulk string (RESP2)
	KindBool                  // #  (RESP3) / :1 / :0 (RESP2)
	KindBigNum                // (  (RESP3) / bulk string (RESP2)
	KindVerbatim              // =  (RESP3) / bulk string (RESP2)
	KindPush                  // >  (RESP3) / array (RESP2)
	// KindNone marks the absence of a reply: the handler has already
	// written its own output, or there is deliberately nothing to send.
	// Writer ignores it.
	KindNone
)

// Value is one node of a reply tree.
type Value struct {
	Kind  Kind
	Str   []byte  // Simple, Error, Bulk, BigNum, Verbatim payload
	Int   int64   // Int
	Float float64 // Double
	Bool  bool    // Bool
	Elems []Value // Array, Map (flattened k,v,k,v), Set, Push
	Fmt   string  // Verbatim format hint, e.g. "txt"
}

// Simple returns a simple-string reply (+...).
func Simple(s string) Value { return Value{Kind: KindSimple, Str: []byte(s)} }

// OK is the canonical +OK reply.
func OK() Value { return Value{Kind: KindSimple, Str: okBytes} }

var okBytes = []byte("OK")

// Err returns an error reply. The message should already carry its error
// code prefix, e.g. "ERR ..." or "WRONGTYPE ...".
func Err(msg string) Value { return Value{Kind: KindError, Str: []byte(msg)} }

// Errf is Err with formatting done by the caller-supplied parts.
func Errf(code, msg string) Value { return Err(code + " " + msg) }

// Int returns an integer reply (:...).
func Int(n int64) Value { return Value{Kind: KindInt, Int: n} }

// Bulk returns a bulk string reply. b is retained, not copied.
func Bulk(b []byte) Value { return Value{Kind: KindBulk, Str: b} }

// BulkString returns a bulk string reply from a Go string.
func BulkString(s string) Value { return Value{Kind: KindBulk, Str: []byte(s)} }

// BulkInt returns an integer rendered as a bulk string.
func BulkInt(n int64) Value { return Value{Kind: KindBulk, Str: strconv.AppendInt(nil, n, 10)} }

// Null is the null reply, rendered as $-1 under RESP2.
func Null() Value { return Value{Kind: KindNull} }

// NullArray is the null reply, rendered as *-1 under RESP2.
func NullArray() Value { return Value{Kind: KindNullArray} }

// Array returns an array reply over elems, which is retained.
func Array(elems ...Value) Value { return Value{Kind: KindArray, Elems: elems} }

// ArrayOf wraps an existing slice as an array reply.
func ArrayOf(elems []Value) Value { return Value{Kind: KindArray, Elems: elems} }

// EmptyArray is an array reply with no elements.
func EmptyArray() Value { return Value{Kind: KindArray, Elems: nil} }

// Map returns a map reply. kv must be a flattened key,value,key,value list.
func Map(kv []Value) Value { return Value{Kind: KindMap, Elems: kv} }

// Set returns a set reply.
func Set(elems []Value) Value { return Value{Kind: KindSet, Elems: elems} }

// Push returns an out-of-band push message (pub/sub under RESP3).
func Push(elems ...Value) Value { return Value{Kind: KindPush, Elems: elems} }

// Double returns a double reply.
func Double(f float64) Value { return Value{Kind: KindDouble, Float: f} }

// Bool returns a boolean reply.
func Bool(b bool) Value { return Value{Kind: KindBool, Bool: b} }

// Verbatim returns a verbatim string with the given three-character format.
func Verbatim(format, s string) Value {
	return Value{Kind: KindVerbatim, Fmt: format, Str: []byte(s)}
}

// None is the empty reply, for handlers that write their own output.
func None() Value { return Value{Kind: KindNone} }

// IsNone reports whether v carries no reply at all.
func (v Value) IsNone() bool { return v.Kind == KindNone }

// IsError reports whether v is an error reply.
func (v Value) IsError() bool { return v.Kind == KindError }
