package command

import (
	"fmt"
	"strings"
	"testing"
)

func TestListPushPop(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expect("3", "RPUSH", "l", "a", "b", "c")
	s.expect("5", "LPUSH", "l", "z", "y")
	s.expect("[y z a b c]", "LRANGE", "l", "0", "-1")
	s.expect("5", "LLEN", "l")
	s.expect("0", "LLEN", "nosuchkey")

	s.expect("y", "LPOP", "l")
	s.expect("c", "RPOP", "l")
	s.expect("[z a b]", "LRANGE", "l", "0", "-1")
	s.expect("[z a]", "LPOP", "l", "2")
	s.expect("[b]", "RPOP", "l", "5")
	// The key goes when the last element does.
	s.expect("0", "EXISTS", "l")
	s.expect("<nil>", "LPOP", "l")
	s.expect("<nil>", "LPOP", "l", "3")

	// The X variants refuse to create.
	s.expect("0", "LPUSHX", "gone", "a")
	s.expect("0", "RPUSHX", "gone", "a")
	s.expect("0", "EXISTS", "gone")
	s.do("RPUSH", "gone", "seed")
	s.expect("2", "LPUSHX", "gone", "a")
	s.expectErrPrefix("ERR value is out of range", "LPOP", "gone", "-1")
}

func TestListRange(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("RPUSH", "l", "a", "b", "c", "d", "e")

	for _, c := range []struct {
		start, stop, want string
	}{
		{"0", "-1", "[a b c d e]"},
		{"0", "2", "[a b c]"},
		{"-3", "-1", "[c d e]"},
		{"-100", "100", "[a b c d e]"},
		{"3", "1", "[]"},
		{"10", "20", "[]"},
		{"-1", "-1", "[e]"},
	} {
		if got := str(s.do("LRANGE", "l", c.start, c.stop)); got != c.want {
			t.Errorf("LRANGE %s %s = %s, want %s", c.start, c.stop, got, c.want)
		}
	}
	s.expect("[]", "LRANGE", "nosuchkey", "0", "-1")
}

func TestListIndexAndSet(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("RPUSH", "l", "a", "b", "c")

	s.expect("a", "LINDEX", "l", "0")
	s.expect("c", "LINDEX", "l", "-1")
	s.expect("<nil>", "LINDEX", "l", "99")
	s.expect("<nil>", "LINDEX", "nosuchkey", "0")

	s.expect("OK", "LSET", "l", "1", "B")
	s.expect("[a B c]", "LRANGE", "l", "0", "-1")
	s.expect("OK", "LSET", "l", "-1", "C")
	s.expect("[a B C]", "LRANGE", "l", "0", "-1")
	s.expectErrPrefix("ERR offset is out of range", "LSET", "l", "99", "x")
	s.expectErrPrefix("ERR no such key", "LSET", "nosuchkey", "0", "x")
}

func TestListInsertRemoveTrim(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("RPUSH", "l", "a", "b", "c")

	s.expect("4", "LINSERT", "l", "BEFORE", "b", "X")
	s.expect("[a X b c]", "LRANGE", "l", "0", "-1")
	s.expect("5", "LINSERT", "l", "AFTER", "c", "Y")
	s.expect("[a X b c Y]", "LRANGE", "l", "0", "-1")
	s.expect("-1", "LINSERT", "l", "BEFORE", "nope", "Z")
	s.expect("0", "LINSERT", "nosuchkey", "BEFORE", "a", "Z")
	s.expectErrPrefix("ERR syntax error", "LINSERT", "l", "SIDEWAYS", "a", "Z")

	s.do("DEL", "r")
	s.do("RPUSH", "r", "a", "b", "a", "c", "a")
	s.expect("2", "LREM", "r", "2", "a")
	s.expect("[b c a]", "LRANGE", "r", "0", "-1")
	s.do("DEL", "r")
	s.do("RPUSH", "r", "a", "b", "a", "c", "a")
	s.expect("2", "LREM", "r", "-2", "a")
	s.expect("[a b c]", "LRANGE", "r", "0", "-1")
	s.do("DEL", "r")
	s.do("RPUSH", "r", "a", "b", "a")
	s.expect("2", "LREM", "r", "0", "a")
	s.expect("[b]", "LRANGE", "r", "0", "-1")
	s.expect("0", "LREM", "nosuchkey", "0", "a")

	s.do("DEL", "t")
	s.do("RPUSH", "t", "a", "b", "c", "d", "e")
	s.expect("OK", "LTRIM", "t", "1", "3")
	s.expect("[b c d]", "LRANGE", "t", "0", "-1")
	// A range that selects nothing empties and removes the key.
	s.expect("OK", "LTRIM", "t", "5", "10")
	s.expect("0", "EXISTS", "t")
}

func TestListPos(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("RPUSH", "l", "a", "b", "c", "b", "b", "d")

	s.expect("1", "LPOS", "l", "b")
	s.expect("<nil>", "LPOS", "l", "zz")
	s.expect("3", "LPOS", "l", "b", "RANK", "2")
	s.expect("4", "LPOS", "l", "b", "RANK", "-1")
	s.expect("3", "LPOS", "l", "b", "RANK", "-2")
	s.expect("[1 3 4]", "LPOS", "l", "b", "COUNT", "0")
	s.expect("[1 3]", "LPOS", "l", "b", "COUNT", "2")
	s.expect("[3 4]", "LPOS", "l", "b", "RANK", "2", "COUNT", "0")
	s.expect("[]", "LPOS", "l", "zz", "COUNT", "0")
	// MAXLEN bounds how far the scan looks.
	s.expect("[1]", "LPOS", "l", "b", "COUNT", "0", "MAXLEN", "3")
	s.expectErrPrefix("ERR RANK can't be zero", "LPOS", "l", "b", "RANK", "0")
	s.expectErrPrefix("ERR COUNT can't be negative", "LPOS", "l", "b", "COUNT", "-1")
	s.expect("<nil>", "LPOS", "nosuchkey", "a")
}

func TestListMove(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("RPUSH", "src", "a", "b", "c")
	s.do("RPUSH", "dst", "x")

	s.expect("c", "RPOPLPUSH", "src", "dst")
	s.expect("[a b]", "LRANGE", "src", "0", "-1")
	s.expect("[c x]", "LRANGE", "dst", "0", "-1")

	s.expect("a", "LMOVE", "src", "dst", "LEFT", "RIGHT")
	s.expect("[c x a]", "LRANGE", "dst", "0", "-1")
	s.expect("<nil>", "LMOVE", "nosuchkey", "dst", "LEFT", "LEFT")
	s.expectErrPrefix("ERR syntax error", "LMOVE", "src", "dst", "UP", "LEFT")

	// Source and destination may be the same key, which rotates the list.
	s.do("DEL", "rot")
	s.do("RPUSH", "rot", "1", "2", "3")
	s.expect("3", "RPOPLPUSH", "rot", "rot")
	s.expect("[3 1 2]", "LRANGE", "rot", "0", "-1")

	// Moving the last element removes the source key.
	s.expect("b", "LMOVE", "src", "dst", "LEFT", "LEFT")
	s.expect("0", "EXISTS", "src")
}

func TestListEncodingPromotion(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.do("RPUSH", "small", "a")
	s.expect("listpack", "OBJECT", "ENCODING", "small")

	for i := 0; i < 200; i++ {
		s.do("RPUSH", "many", fmt.Sprintf("e%d", i))
	}
	s.expect("quicklist", "OBJECT", "ENCODING", "many")
	s.expect("200", "LLEN", "many")
	s.expect("e0", "LINDEX", "many", "0")
	s.expect("e199", "LINDEX", "many", "-1")
	s.expect("e150", "LINDEX", "many", "150")

	s.do("RPUSH", "long", strings.Repeat("x", 100))
	s.expect("quicklist", "OBJECT", "ENCODING", "long")
}

// TestListLargeListIntegrity exercises the quicklist across node boundaries,
// which is where an off-by-one in the node split or the index mapping would
// show up.
func TestListLargeListIntegrity(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	const n = 1000
	for i := 0; i < n; i++ {
		s.do("RPUSH", "big", fmt.Sprintf("%d", i))
	}
	s.expect(fmt.Sprint(n), "LLEN", "big")
	for _, i := range []int{0, 1, 127, 128, 129, 255, 256, 500, n - 1} {
		s.expect(fmt.Sprint(i), "LINDEX", "big", fmt.Sprint(i))
	}
	// A range spanning several nodes must come back in order.
	got := s.do("LRANGE", "big", "120", "140")
	if len(got.Elems) != 21 {
		t.Fatalf("LRANGE across nodes returned %d elements", len(got.Elems))
	}
	for i, e := range got.Elems {
		if want := fmt.Sprint(120 + i); string(e.Str) != want {
			t.Fatalf("element %d is %q, want %q", i, e.Str, want)
		}
	}
	// Popping every element from alternating ends must drain it exactly.
	for i := 0; i < n; i++ {
		var v string
		if i%2 == 0 {
			v = str(s.do("LPOP", "big"))
		} else {
			v = str(s.do("RPOP", "big"))
		}
		if v == "<nil>" {
			t.Fatalf("list ran out after %d pops", i)
		}
	}
	s.expect("0", "EXISTS", "big")
}

func TestListWrongType(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "str", "v")
	for _, cmd := range [][]string{
		{"LPUSH", "str", "a"}, {"LPOP", "str"}, {"LLEN", "str"},
		{"LRANGE", "str", "0", "-1"}, {"LINDEX", "str", "0"},
		{"LSET", "str", "0", "x"}, {"LREM", "str", "0", "a"},
		{"LTRIM", "str", "0", "1"}, {"LPOS", "str", "a"},
		{"LMOVE", "str", "d", "LEFT", "LEFT"},
	} {
		s.expectErrPrefix("WRONGTYPE", cmd...)
	}
}
