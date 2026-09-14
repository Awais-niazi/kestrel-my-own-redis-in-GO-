package command

import (
	"fmt"
	"strings"
	"testing"
)

// sortedSet renders a set reply in a stable order, since set replies have no
// defined ordering.
func sortedSet(v string) string { return sortedFields(v) }

func TestSetCommands(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expect("3", "SADD", "s", "a", "b", "c")
	s.expect("0", "SADD", "s", "a")
	s.expect("1", "SADD", "s", "d")
	s.expect("4", "SCARD", "s")
	s.expect("0", "SCARD", "nosuchkey")
	s.expect("1", "SISMEMBER", "s", "a")
	s.expect("0", "SISMEMBER", "s", "zz")
	s.expect("0", "SISMEMBER", "nosuchkey", "a")
	s.expect("[1 0 1]", "SMISMEMBER", "s", "a", "zz", "b")
	if got := sortedSet(str(s.do("SMEMBERS", "s"))); got != "[a b c d]" {
		t.Fatalf("SMEMBERS = %s", got)
	}
	s.expect("[]", "SMEMBERS", "nosuchkey")

	s.expect("2", "SREM", "s", "a", "b", "zz")
	s.expect("2", "SCARD", "s")
	s.expect("0", "SREM", "nosuchkey", "a")

	// The key disappears with its last member.
	s.expect("2", "SREM", "s", "c", "d")
	s.expect("0", "EXISTS", "s")
}

func TestSetEncodingPromotion(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	// All-integer members start as an intset.
	s.do("SADD", "ints", "1", "2", "3")
	s.expect("intset", "OBJECT", "ENCODING", "ints")

	// A non-integer member ends that, regardless of size.
	s.do("SADD", "ints", "notanumber")
	s.expect("listpack", "OBJECT", "ENCODING", "ints")
	s.expect("4", "SCARD", "ints")
	s.expect("1", "SISMEMBER", "ints", "2")

	// A non-canonical integer is not an integer for encoding purposes.
	s.do("SADD", "padded", "007")
	s.expect("listpack", "OBJECT", "ENCODING", "padded")

	// Outgrowing the intset limit moves to a map, since it is already past
	// the listpack limit too.
	for i := 0; i < 600; i++ {
		s.do("SADD", "big", fmt.Sprint(i))
	}
	s.expect("hashtable", "OBJECT", "ENCODING", "big")
	s.expect("600", "SCARD", "big")

	// A long member promotes a small set straight past the listpack.
	s.do("SADD", "long", strings.Repeat("x", 100))
	s.expect("hashtable", "OBJECT", "ENCODING", "long")

	// Many short non-integer members cross the listpack entry limit.
	for i := 0; i < 200; i++ {
		s.do("SADD", "many", fmt.Sprintf("m%d", i))
	}
	s.expect("hashtable", "OBJECT", "ENCODING", "many")
	s.expect("200", "SCARD", "many")
}

func TestSetIntsetStaysSorted(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for _, n := range []string{"50", "3", "-20", "7", "0"} {
		s.do("SADD", "ints", n)
	}
	// An intset iterates in numeric order, which callers of SMEMBERS on an
	// integer set can and do rely on in practice.
	if got := str(s.do("SMEMBERS", "ints")); got != "[-20 0 3 7 50]" {
		t.Fatalf("SMEMBERS = %s", got)
	}
	s.do("SREM", "ints", "3")
	if got := str(s.do("SMEMBERS", "ints")); got != "[-20 0 7 50]" {
		t.Fatalf("after SREM = %s", got)
	}
}

func TestSetPopAndRandom(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SADD", "s", "a", "b", "c")

	if v := s.do("SPOP", "s"); v.Kind == kindNull() {
		t.Fatal("SPOP on a populated set returned nil")
	}
	s.expect("2", "SCARD", "s")
	if v := s.do("SPOP", "s", "5"); len(v.Elems) != 2 {
		t.Fatalf("SPOP 5 returned %d members, want the remaining 2", len(v.Elems))
	}
	s.expect("0", "EXISTS", "s")
	s.expect("<nil>", "SPOP", "s")
	if v := s.do("SPOP", "s", "3"); len(v.Elems) != 0 {
		t.Fatalf("SPOP on a missing key returned %s", str(v))
	}
	s.expectErrPrefix("ERR value is out of range", "SPOP", "s", "-1")

	s.do("SADD", "r", "a", "b", "c")
	if v := s.do("SRANDMEMBER", "r", "10"); len(v.Elems) != 3 {
		t.Fatalf("SRANDMEMBER 10 returned %d, want 3", len(v.Elems))
	}
	if v := s.do("SRANDMEMBER", "r", "-10"); len(v.Elems) != 10 {
		t.Fatalf("SRANDMEMBER -10 returned %d", len(v.Elems))
	}
	s.expect("3", "SCARD", "r") // SRANDMEMBER does not remove
	s.expect("<nil>", "SRANDMEMBER", "nosuchkey")
}

func TestSetCombinations(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SADD", "a", "1", "2", "3", "4")
	s.do("SADD", "b", "3", "4", "5")
	s.do("SADD", "c", "4", "6")

	if got := sortedSet(str(s.do("SINTER", "a", "b"))); got != "[3 4]" {
		t.Errorf("SINTER = %s", got)
	}
	if got := sortedSet(str(s.do("SINTER", "a", "b", "c"))); got != "[4]" {
		t.Errorf("SINTER 3 = %s", got)
	}
	if got := str(s.do("SINTER", "a", "nosuchkey")); got != "[]" {
		t.Errorf("SINTER with a missing key = %s", got)
	}
	if got := sortedSet(str(s.do("SUNION", "a", "c"))); got != "[1 2 3 4 6]" {
		t.Errorf("SUNION = %s", got)
	}
	if got := sortedSet(str(s.do("SDIFF", "a", "b"))); got != "[1 2]" {
		t.Errorf("SDIFF = %s", got)
	}
	if got := str(s.do("SDIFF", "nosuchkey", "a")); got != "[]" {
		t.Errorf("SDIFF from a missing key = %s", got)
	}

	s.expect("2", "SINTERSTORE", "dst", "a", "b")
	if got := sortedSet(str(s.do("SMEMBERS", "dst"))); got != "[3 4]" {
		t.Errorf("SINTERSTORE result = %s", got)
	}
	s.expect("5", "SUNIONSTORE", "dst", "a", "c")
	s.expect("2", "SDIFFSTORE", "dst", "a", "b")
	// An empty result removes the destination rather than leaving an empty
	// set behind.
	s.expect("0", "SINTERSTORE", "dst", "a", "nosuchkey")
	s.expect("0", "EXISTS", "dst")

	s.expect("2", "SINTERCARD", "2", "a", "b")
	s.expect("1", "SINTERCARD", "2", "a", "b", "LIMIT", "1")
	s.expect("2", "SINTERCARD", "2", "a", "b", "LIMIT", "0")
	s.expectErrPrefix("ERR numkeys", "SINTERCARD", "0", "a")
	s.expectErrPrefix("ERR Number of keys", "SINTERCARD", "5", "a")
	s.expectErrPrefix("ERR syntax error", "SINTERCARD", "2", "a", "b", "BOGUS", "1")
}

func TestSetMove(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SADD", "src", "a", "b")
	s.do("SADD", "dst", "c")

	s.expect("1", "SMOVE", "src", "dst", "a")
	s.expect("1", "SCARD", "src")
	s.expect("2", "SCARD", "dst")
	s.expect("0", "SMOVE", "src", "dst", "zz")
	// Moving the last member removes the source key.
	s.expect("1", "SMOVE", "src", "dst", "b")
	s.expect("0", "EXISTS", "src")
	// Moving into a key that does not exist yet creates it.
	s.expect("1", "SMOVE", "dst", "fresh", "a")
	s.expect("1", "SCARD", "fresh")
	s.expect("0", "SMOVE", "nosuchkey", "dst", "a")
}

func TestSetScan(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for i := 0; i < 30; i++ {
		s.do("SADD", "s", fmt.Sprintf("m%d", i))
	}
	v := s.do("SSCAN", "s", "0")
	if string(v.Elems[0].Str) != "0" || len(v.Elems[1].Elems) != 30 {
		t.Fatalf("SSCAN = cursor %q, %d members", v.Elems[0].Str, len(v.Elems[1].Elems))
	}
	if v := s.do("SSCAN", "s", "0", "MATCH", "m1?"); len(v.Elems[1].Elems) != 10 {
		t.Fatalf("SSCAN MATCH returned %d", len(v.Elems[1].Elems))
	}
	s.expectErrPrefix("ERR syntax error", "SSCAN", "s", "0", "NOVALUES")
}

func TestSetWrongType(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "str", "v")
	for _, cmd := range [][]string{
		{"SADD", "str", "m"}, {"SREM", "str", "m"}, {"SCARD", "str"},
		{"SMEMBERS", "str"}, {"SPOP", "str"}, {"SINTER", "str"},
		{"SMOVE", "str", "d", "m"}, {"SSCAN", "str", "0"},
	} {
		s.expectErrPrefix("WRONGTYPE", cmd...)
	}
}

func TestSetMembersIsASetUnderRESP3(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SADD", "s", "a")
	if v := s.do("SMEMBERS", "s"); v.Kind == kindSet() {
		t.Fatal("RESP2 SMEMBERS should be a plain array")
	}
	s.do("HELLO", "3")
	if v := s.do("SMEMBERS", "s"); v.Kind != kindSet() {
		t.Fatalf("RESP3 SMEMBERS replied with kind %d, want a set", v.Kind)
	}
}
