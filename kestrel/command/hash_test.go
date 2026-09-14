package command

import (
	"fmt"
	"strings"
	"testing"
)

func TestHashCommands(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expect("2", "HSET", "u:1", "name", "ana", "city", "lisbon")
	s.expect("0", "HSET", "u:1", "name", "bea") // an update is not a new field
	s.expect("bea", "HGET", "u:1", "name")
	s.expect("<nil>", "HGET", "u:1", "missing")
	s.expect("<nil>", "HGET", "nosuchkey", "name")
	s.expect("2", "HLEN", "u:1")
	s.expect("1", "HEXISTS", "u:1", "name")
	s.expect("0", "HEXISTS", "u:1", "nope")
	s.expect("3", "HSTRLEN", "u:1", "name")
	s.expect("0", "HSTRLEN", "u:1", "nope")
	s.expect("[bea lisbon <nil>]", "HMGET", "u:1", "name", "city", "nope")
	s.expect("[<nil>]", "HMGET", "nosuchkey", "f")

	s.expect("1", "HSETNX", "u:1", "email", "a@b.c")
	s.expect("0", "HSETNX", "u:1", "email", "other")
	s.expect("a@b.c", "HGET", "u:1", "email")

	s.expect("OK", "HMSET", "u:2", "a", "1", "b", "2")
	s.expect("2", "HLEN", "u:2")

	s.expect("1", "HDEL", "u:1", "email", "nope")
	s.expect("2", "HLEN", "u:1")
	s.expect("0", "HDEL", "nosuchkey", "f")

	// A hash disappears when its last field goes.
	s.expect("2", "HDEL", "u:1", "name", "city")
	s.expect("0", "EXISTS", "u:1")
	s.expect("none", "TYPE", "u:1")

	s.expectErrPrefix("ERR wrong number of arguments", "HSET", "k", "field")
	s.expectErrPrefix("ERR wrong number of arguments", "HSET", "k", "a", "1", "b")
}

func TestHashReadShapes(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("HSET", "h", "a", "1", "b", "2")

	if got := sortedFields(str(s.do("HKEYS", "h"))); got != "[a b]" {
		t.Errorf("HKEYS = %s", got)
	}
	if got := sortedFields(str(s.do("HVALS", "h"))); got != "[1 2]" {
		t.Errorf("HVALS = %s", got)
	}
	all := str(s.do("HGETALL", "h"))
	if !strings.Contains(all, "a") || !strings.Contains(all, "1") || len(strings.Fields(all)) != 4 {
		t.Errorf("HGETALL = %s", all)
	}
	// Reads of a missing key are empty, not an error.
	s.expect("[]", "HKEYS", "nosuchkey")
	s.expect("[]", "HGETALL", "nosuchkey")
}

func TestHashGetAllIsAMapUnderRESP3(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("HSET", "h", "a", "1")

	if v := s.do("HGETALL", "h"); v.Kind == kindMap() || len(v.Elems) != 2 {
		t.Fatalf("RESP2 HGETALL should be a flat array, got %s", str(v))
	}
	s.do("HELLO", "3")
	v := s.do("HGETALL", "h")
	if v.Kind != kindMap() {
		t.Fatalf("RESP3 HGETALL replied with kind %d, want a map", v.Kind)
	}
}

func TestHashIncrement(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expect("1", "HINCRBY", "h", "n", "1")
	s.expect("11", "HINCRBY", "h", "n", "10")
	s.expect("1", "HINCRBY", "h", "n", "-10")
	s.expect("1", "HSET", "h", "s", "abc")
	s.expectErrPrefix("ERR value is not an integer", "HINCRBY", "h", "s", "1")
	s.do("HSET", "h", "big", "9223372036854775807")
	s.expectErrPrefix("ERR increment or decrement would overflow", "HINCRBY", "h", "big", "1")

	s.expect("10.5", "HINCRBYFLOAT", "h", "f", "10.5")
	s.expect("10.6", "HINCRBYFLOAT", "h", "f", "0.1")
	s.expectErrPrefix("ERR value is not a valid float", "HINCRBYFLOAT", "h", "s", "1")
}

func TestHashRandField(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("HSET", "h", "a", "1", "b", "2", "c", "3")

	if v := s.do("HRANDFIELD", "h"); v.Kind == kindNull() {
		t.Fatal("HRANDFIELD on a populated hash returned nil")
	}
	s.expect("<nil>", "HRANDFIELD", "empty")
	if v := s.do("HRANDFIELD", "h", "2"); len(v.Elems) != 2 {
		t.Fatalf("HRANDFIELD count 2 returned %d", len(v.Elems))
	}
	// A positive count is capped at the hash size; a negative one repeats.
	if v := s.do("HRANDFIELD", "h", "10"); len(v.Elems) != 3 {
		t.Fatalf("HRANDFIELD count 10 returned %d, want 3", len(v.Elems))
	}
	if v := s.do("HRANDFIELD", "h", "-10"); len(v.Elems) != 10 {
		t.Fatalf("HRANDFIELD count -10 returned %d", len(v.Elems))
	}
	if v := s.do("HRANDFIELD", "h", "2", "WITHVALUES"); len(v.Elems) != 4 {
		t.Fatalf("RESP2 WITHVALUES returned %d elements, want a flat 4", len(v.Elems))
	}
	s.do("HELLO", "3")
	v := s.do("HRANDFIELD", "h", "2", "WITHVALUES")
	if len(v.Elems) != 2 || len(v.Elems[0].Elems) != 2 {
		t.Fatalf("RESP3 WITHVALUES = %s, want two pairs", str(v))
	}
	s.expectErrPrefix("ERR syntax error", "HRANDFIELD", "h", "2", "BOGUS")
}

func TestHashScan(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for i := 0; i < 50; i++ {
		s.do("HSET", "h", fmt.Sprintf("f%d", i), fmt.Sprintf("v%d", i))
	}
	v := s.do("HSCAN", "h", "0")
	if string(v.Elems[0].Str) != "0" {
		t.Fatalf("HSCAN cursor = %q", v.Elems[0].Str)
	}
	if len(v.Elems[1].Elems) != 100 {
		t.Fatalf("HSCAN returned %d entries, want 100", len(v.Elems[1].Elems))
	}
	if v := s.do("HSCAN", "h", "0", "NOVALUES"); len(v.Elems[1].Elems) != 50 {
		t.Fatalf("HSCAN NOVALUES returned %d", len(v.Elems[1].Elems))
	}
	if v := s.do("HSCAN", "h", "0", "MATCH", "f1?"); len(v.Elems[1].Elems) != 20 {
		t.Fatalf("HSCAN MATCH returned %d entries, want 20", len(v.Elems[1].Elems))
	}
	s.expectErrPrefix("ERR invalid cursor", "HSCAN", "h", "abc")
	s.expectErrPrefix("ERR syntax error", "HSCAN", "h", "0", "BOGUS")
}

func TestHashEncodingPromotion(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.do("HSET", "small", "a", "1")
	s.expect("listpack", "OBJECT", "ENCODING", "small")

	// Crossing the entry threshold promotes.
	for i := 0; i < 200; i++ {
		s.do("HSET", "many", fmt.Sprintf("f%d", i), "v")
	}
	s.expect("hashtable", "OBJECT", "ENCODING", "many")
	s.expect("200", "HLEN", "many")

	// So does a single oversized value.
	s.do("HSET", "big", "f", strings.Repeat("x", 100))
	s.expect("hashtable", "OBJECT", "ENCODING", "big")

	// Promotion is one way: shrinking back does not demote.
	for i := 0; i < 199; i++ {
		s.do("HDEL", "many", fmt.Sprintf("f%d", i))
	}
	s.expect("1", "HLEN", "many")
	s.expect("hashtable", "OBJECT", "ENCODING", "many")
}

func TestHashWrongType(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "str", "v")
	for _, cmd := range [][]string{
		{"HSET", "str", "f", "v"}, {"HGET", "str", "f"}, {"HDEL", "str", "f"},
		{"HLEN", "str"}, {"HGETALL", "str"}, {"HINCRBY", "str", "f", "1"},
		{"HRANDFIELD", "str"}, {"HSCAN", "str", "0"},
	} {
		s.expectErrPrefix("WRONGTYPE", cmd...)
	}
	// And the reverse: a string command against a hash.
	s.do("HSET", "h", "f", "v")
	s.expectErrPrefix("WRONGTYPE", "GET", "h")
	s.expectErrPrefix("WRONGTYPE", "INCR", "h")
	s.expectErrPrefix("WRONGTYPE", "APPEND", "h", "x")
}

func sortedFields(s string) string {
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if inner == "" {
		return "[]"
	}
	f := strings.Fields(inner)
	for i := 0; i < len(f); i++ {
		for j := i + 1; j < len(f); j++ {
			if f[j] < f[i] {
				f[i], f[j] = f[j], f[i]
			}
		}
	}
	return "[" + strings.Join(f, " ") + "]"
}
