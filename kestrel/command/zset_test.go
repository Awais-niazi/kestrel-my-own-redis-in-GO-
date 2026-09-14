package command

import (
	"fmt"
	"strings"
	"testing"
)

func TestZSetAddAndScore(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expect("2", "ZADD", "z", "1", "a", "2", "b")
	s.expect("0", "ZADD", "z", "3", "a") // an update is not an add
	s.expect("3", "ZSCORE", "z", "a")
	s.expect("<nil>", "ZSCORE", "z", "nope")
	s.expect("<nil>", "ZSCORE", "nosuchkey", "a")
	s.expect("2", "ZCARD", "z")
	s.expect("0", "ZCARD", "nosuchkey")
	s.expect("[3 2 <nil>]", "ZMSCORE", "z", "a", "b", "nope")

	// CH counts updates as well as additions.
	s.expect("1", "ZADD", "z", "CH", "9", "a")
	s.expect("0", "ZADD", "z", "CH", "9", "a") // no change at all
	s.expect("2", "ZADD", "z", "CH", "1", "a", "5", "c")

	// Scores accept the infinity spellings.
	s.expect("1", "ZADD", "z", "inf", "hi")
	s.expect("inf", "ZSCORE", "z", "hi")
	s.expect("1", "ZADD", "z", "-inf", "lo")
	s.expect("-inf", "ZSCORE", "z", "lo")
	s.expectErrPrefix("ERR min or max is not a float", "ZADD", "z", "notanumber", "x")
	s.expectErrPrefix("ERR wrong number of arguments", "ZADD", "z", "1")
	s.expectErrPrefix("ERR syntax error", "ZADD", "z", "1", "a", "2")
}

func TestZSetAddFlags(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("ZADD", "z", "5", "a")

	s.expect("0", "ZADD", "z", "NX", "9", "a")
	s.expect("5", "ZSCORE", "z", "a")
	s.expect("1", "ZADD", "z", "NX", "1", "fresh")
	s.expect("0", "ZADD", "z", "XX", "9", "absent")
	s.expect("0", "EXISTS", "absent")
	s.expect("0", "ZADD", "z", "XX", "CH", "5", "a") // same score, no change

	s.expect("0", "ZADD", "z", "GT", "CH", "1", "a") // lower is refused
	s.expect("5", "ZSCORE", "z", "a")
	s.expect("1", "ZADD", "z", "GT", "CH", "9", "a")
	s.expect("9", "ZSCORE", "z", "a")
	s.expect("0", "ZADD", "z", "LT", "CH", "20", "a")
	s.expect("1", "ZADD", "z", "LT", "CH", "2", "a")
	s.expect("2", "ZSCORE", "z", "a")

	s.expectErrPrefix("ERR XX and NX options at the same time", "ZADD", "z", "NX", "XX", "1", "a")
	s.expectErrPrefix("ERR GT, LT, and/or NX", "ZADD", "z", "GT", "LT", "1", "a")
	s.expectErrPrefix("ERR GT, LT, and/or NX", "ZADD", "z", "NX", "GT", "1", "a")

	// INCR turns ZADD into ZINCRBY and returns the resulting score.
	s.expect("12", "ZADD", "z", "INCR", "10", "a")
	s.expect("<nil>", "ZADD", "z", "NX", "INCR", "1", "a")
	s.expect("<nil>", "ZADD", "z", "XX", "INCR", "1", "brandnew")
	s.expectErrPrefix("ERR INCR option supports a single", "ZADD", "z", "INCR", "1", "a", "2", "b")
}

func TestZSetIncrBy(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.expect("5", "ZINCRBY", "z", "5", "a")
	s.expect("7.5", "ZINCRBY", "z", "2.5", "a")
	s.expect("-2.5", "ZINCRBY", "z", "-10", "a")
	s.expect("inf", "ZINCRBY", "z", "inf", "a")
	s.expectErrPrefix("ERR resulting score is not a number", "ZINCRBY", "z", "-inf", "a")
}

func TestZSetRanges(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("ZADD", "z", "1", "a", "2", "b", "3", "c", "4", "d")

	s.expect("[a b c d]", "ZRANGE", "z", "0", "-1")
	s.expect("[b c]", "ZRANGE", "z", "1", "2")
	s.expect("[d]", "ZRANGE", "z", "-1", "-1")
	s.expect("[]", "ZRANGE", "z", "5", "10")
	s.expect("[d c b a]", "ZREVRANGE", "z", "0", "-1")
	s.expect("[d c b a]", "ZRANGE", "z", "0", "-1", "REV")
	s.expect("[a 1 b 2 c 3 d 4]", "ZRANGE", "z", "0", "-1", "WITHSCORES")

	s.expect("[b c]", "ZRANGEBYSCORE", "z", "2", "3")
	s.expect("[c]", "ZRANGEBYSCORE", "z", "(2", "3")
	s.expect("[b]", "ZRANGEBYSCORE", "z", "2", "(3")
	s.expect("[a b c d]", "ZRANGEBYSCORE", "z", "-inf", "+inf")
	s.expect("[c b]", "ZREVRANGEBYSCORE", "z", "3", "2")
	s.expect("[b c]", "ZRANGE", "z", "2", "3", "BYSCORE")
	s.expect("[c b]", "ZRANGE", "z", "3", "2", "BYSCORE", "REV")
	s.expect("[b]", "ZRANGEBYSCORE", "z", "-inf", "+inf", "LIMIT", "1", "1")
	s.expect("[b c d]", "ZRANGEBYSCORE", "z", "-inf", "+inf", "LIMIT", "1", "-1")
	s.expectErrPrefix("ERR min or max is not a float", "ZRANGEBYSCORE", "z", "bad", "3")
	s.expectErrPrefix("ERR syntax error, LIMIT", "ZRANGE", "z", "0", "-1", "LIMIT", "0", "1")

	s.expect("2", "ZCOUNT", "z", "2", "3")
	s.expect("4", "ZCOUNT", "z", "-inf", "+inf")
	s.expect("1", "ZCOUNT", "z", "(2", "3")
	s.expect("0", "ZCOUNT", "nosuchkey", "-inf", "+inf")
}

func TestZSetLexRanges(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	// Lexicographic ranges are only meaningful with equal scores.
	s.do("ZADD", "z", "0", "a", "0", "b", "0", "c", "0", "d")

	s.expect("[a b c d]", "ZRANGEBYLEX", "z", "-", "+")
	s.expect("[b c]", "ZRANGEBYLEX", "z", "[b", "[c")
	s.expect("[c]", "ZRANGEBYLEX", "z", "(b", "[c")
	s.expect("[b]", "ZRANGEBYLEX", "z", "[b", "(c")
	s.expect("[d c b a]", "ZREVRANGEBYLEX", "z", "+", "-")
	s.expect("[a b c d]", "ZRANGE", "z", "-", "+", "BYLEX")
	s.expect("[b c]", "ZRANGEBYLEX", "z", "-", "+", "LIMIT", "1", "2")
	s.expect("4", "ZLEXCOUNT", "z", "-", "+")
	s.expect("2", "ZLEXCOUNT", "z", "[b", "[c")
	s.expectErrPrefix("ERR min or max not valid", "ZRANGEBYLEX", "z", "b", "c")
	s.expectErrPrefix("ERR syntax error", "ZRANGEBYLEX", "z", "-", "+", "WITHSCORES")
}

func TestZSetRank(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("ZADD", "z", "1", "a", "2", "b", "3", "c")

	s.expect("0", "ZRANK", "z", "a")
	s.expect("2", "ZRANK", "z", "c")
	s.expect("<nil>", "ZRANK", "z", "nope")
	s.expect("<nil>", "ZRANK", "nosuchkey", "a")
	s.expect("2", "ZREVRANK", "z", "a")
	s.expect("0", "ZREVRANK", "z", "c")
	s.expect("[0 1]", "ZRANK", "z", "a", "WITHSCORE")
	s.expect("<nil>", "ZRANK", "z", "nope", "WITHSCORE")
	s.expectErrPrefix("ERR syntax error", "ZRANK", "z", "a", "BOGUS")
}

func TestZSetPopAndRemove(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("ZADD", "z", "1", "a", "2", "b", "3", "c")

	s.expect("[a 1]", "ZPOPMIN", "z")
	s.expect("[c 3]", "ZPOPMAX", "z")
	s.expect("1", "ZCARD", "z")
	s.expect("[b 2]", "ZPOPMIN", "z", "5")
	s.expect("0", "EXISTS", "z")
	s.expect("[]", "ZPOPMIN", "z")

	s.do("ZADD", "r", "1", "a", "2", "b", "3", "c", "4", "d")
	s.expect("2", "ZREM", "r", "a", "b", "nope")
	s.expect("2", "ZCARD", "r")
	s.expect("0", "ZREM", "nosuchkey", "a")

	s.do("DEL", "rr")
	s.do("ZADD", "rr", "1", "a", "2", "b", "3", "c", "4", "d")
	s.expect("2", "ZREMRANGEBYRANK", "rr", "0", "1")
	s.expect("[c d]", "ZRANGE", "rr", "0", "-1")
	s.expect("1", "ZREMRANGEBYSCORE", "rr", "3", "3")
	s.expect("[d]", "ZRANGE", "rr", "0", "-1")

	s.do("DEL", "lx")
	s.do("ZADD", "lx", "0", "a", "0", "b", "0", "c")
	s.expect("2", "ZREMRANGEBYLEX", "lx", "[a", "[b")
	s.expect("[c]", "ZRANGE", "lx", "0", "-1")
	// Removing everything removes the key.
	s.expect("1", "ZREMRANGEBYLEX", "lx", "-", "+")
	s.expect("0", "EXISTS", "lx")
}

func TestZSetCombinations(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("ZADD", "a", "1", "x", "2", "y")
	s.do("ZADD", "b", "10", "y", "20", "z")

	// Results come back in score order, with ties broken by member.
	s.expect("[x 1 y 12 z 20]", "ZUNION", "2", "a", "b", "WITHSCORES")
	s.expect("[y 12]", "ZINTER", "2", "a", "b", "WITHSCORES")
	s.expect("[x 1]", "ZDIFF", "2", "a", "b", "WITHSCORES")
	s.expect("[y 2]", "ZINTER", "2", "a", "b", "WEIGHTS", "1", "0", "WITHSCORES")
	s.expect("[y 2]", "ZINTER", "2", "a", "b", "AGGREGATE", "MIN", "WITHSCORES")
	s.expect("[y 10]", "ZINTER", "2", "a", "b", "AGGREGATE", "MAX", "WITHSCORES")

	s.expect("3", "ZUNIONSTORE", "dst", "2", "a", "b")
	s.expect("[x 1 y 12 z 20]", "ZRANGE", "dst", "0", "-1", "WITHSCORES")
	s.expect("1", "ZINTERSTORE", "dst", "2", "a", "b")
	s.expect("1", "ZDIFFSTORE", "dst", "2", "a", "b")
	// An empty result removes the destination.
	s.expect("0", "ZINTERSTORE", "dst", "2", "a", "nosuchkey")
	s.expect("0", "EXISTS", "dst")

	s.expect("1", "ZINTERCARD", "2", "a", "b")
	s.expect("1", "ZINTERCARD", "2", "a", "b", "LIMIT", "1")

	// A plain set counts as a sorted set whose members all score 1.
	s.do("SADD", "plain", "y", "w")
	s.expect("3", "ZUNIONSTORE", "u", "2", "a", "plain")
	s.expect("[w 1 x 1 y 3]", "ZRANGE", "u", "0", "-1", "WITHSCORES")

	s.expectErrPrefix("ERR at least 1 input key", "ZUNION", "0", "a")
	s.expectErrPrefix("ERR syntax error", "ZUNION", "5", "a")
}

func TestZSetRangeStore(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("ZADD", "src", "1", "a", "2", "b", "3", "c")

	s.expect("2", "ZRANGESTORE", "dst", "src", "0", "1")
	s.expect("[a 1 b 2]", "ZRANGE", "dst", "0", "-1", "WITHSCORES")
	s.expect("2", "ZRANGESTORE", "dst", "src", "2", "3", "BYSCORE")
	s.expect("[b c]", "ZRANGE", "dst", "0", "-1")
	s.expect("0", "ZRANGESTORE", "dst", "nosuchkey", "0", "-1")
	s.expect("0", "EXISTS", "dst")
}

func TestZSetRandMember(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("ZADD", "z", "1", "a", "2", "b", "3", "c")

	if v := s.do("ZRANDMEMBER", "z"); v.Kind == kindNull() {
		t.Fatal("ZRANDMEMBER on a populated set returned nil")
	}
	s.expect("<nil>", "ZRANDMEMBER", "nosuchkey")
	if v := s.do("ZRANDMEMBER", "z", "10"); len(v.Elems) != 3 {
		t.Fatalf("ZRANDMEMBER 10 returned %d, want 3", len(v.Elems))
	}
	if v := s.do("ZRANDMEMBER", "z", "-10"); len(v.Elems) != 10 {
		t.Fatalf("ZRANDMEMBER -10 returned %d", len(v.Elems))
	}
	if v := s.do("ZRANDMEMBER", "z", "2", "WITHSCORES"); len(v.Elems) != 4 {
		t.Fatalf("WITHSCORES returned %d elements", len(v.Elems))
	}
	s.expectErrPrefix("ERR syntax error", "ZRANDMEMBER", "z", "2", "BOGUS")
}

func TestZSetScan(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for i := 0; i < 20; i++ {
		s.do("ZADD", "z", fmt.Sprint(i), fmt.Sprintf("m%d", i))
	}
	v := s.do("ZSCAN", "z", "0")
	if string(v.Elems[0].Str) != "0" || len(v.Elems[1].Elems) != 40 {
		t.Fatalf("ZSCAN returned cursor %q and %d entries", v.Elems[0].Str, len(v.Elems[1].Elems))
	}
	if v := s.do("ZSCAN", "z", "0", "MATCH", "m1?"); len(v.Elems[1].Elems) != 20 {
		t.Fatalf("ZSCAN MATCH returned %d", len(v.Elems[1].Elems))
	}
}

func TestZSetEncodingPromotion(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.do("ZADD", "small", "1", "a")
	s.expect("listpack", "OBJECT", "ENCODING", "small")

	for i := 0; i < 200; i++ {
		s.do("ZADD", "many", fmt.Sprint(i), fmt.Sprintf("m%03d", i))
	}
	s.expect("skiplist", "OBJECT", "ENCODING", "many")
	s.expect("200", "ZCARD", "many")
	s.expect("0", "ZRANK", "many", "m000")
	s.expect("199", "ZRANK", "many", "m199")
	s.expect("[m100 m101]", "ZRANGE", "many", "100", "101")

	s.do("ZADD", "long", "1", strings.Repeat("x", 100))
	s.expect("skiplist", "OBJECT", "ENCODING", "long")
}

// TestZSetOrderingAcrossEncodings runs the same sequence through both
// encodings and requires identical answers, which is the check that keeps
// the listpack and skiplist paths from drifting apart.
func TestZSetOrderingAcrossEncodings(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	// The second set is forced past the threshold by a long member, which is
	// then removed. A companion member keeps the key alive, since emptying a
	// collection deletes it. What is left is a small set that kept the
	// skiplist encoding, because promotion is one-way.
	s.do("ZADD", "big", "0", "keeper", "0", strings.Repeat("p", 200))
	s.do("ZREM", "big", strings.Repeat("p", 200))
	s.expect("skiplist", "OBJECT", "ENCODING", "big")
	s.do("ZADD", "small", "0", "keeper")

	for _, entry := range [][2]string{
		{"3", "c"}, {"1", "a"}, {"2", "b"}, {"1", "aa"}, {"-5", "neg"}, {"inf", "top"},
	} {
		s.do("ZADD", "small", entry[0], entry[1])
		s.do("ZADD", "big", entry[0], entry[1])
	}
	s.expect("listpack", "OBJECT", "ENCODING", "small")
	s.expect("skiplist", "OBJECT", "ENCODING", "big")

	for _, q := range [][]string{
		{"ZRANGE", "%s", "0", "-1", "WITHSCORES"},
		{"ZRANGE", "%s", "0", "-1", "REV"},
		{"ZRANGEBYSCORE", "%s", "-inf", "+inf"},
		{"ZRANGEBYSCORE", "%s", "1", "3"},
		{"ZRANK", "%s", "b"},
		{"ZREVRANK", "%s", "b"},
		{"ZCOUNT", "%s", "1", "3"},
		{"ZCARD", "%s"},
	} {
		a := make([]string, len(q))
		b := make([]string, len(q))
		for i, part := range q {
			a[i] = strings.ReplaceAll(part, "%s", "small")
			b[i] = strings.ReplaceAll(part, "%s", "big")
		}
		if got, want := str(s.do(b...)), str(s.do(a...)); got != want {
			t.Errorf("%v: skiplist gave %s, listpack gave %s", q, got, want)
		}
	}
}

func TestZSetWrongType(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "str", "v")
	for _, cmd := range [][]string{
		{"ZADD", "str", "1", "a"}, {"ZSCORE", "str", "a"}, {"ZCARD", "str"},
		{"ZRANGE", "str", "0", "-1"}, {"ZREM", "str", "a"}, {"ZRANK", "str", "a"},
		{"ZPOPMIN", "str"}, {"ZINCRBY", "str", "1", "a"}, {"ZSCAN", "str", "0"},
	} {
		s.expectErrPrefix("WRONGTYPE", cmd...)
	}
}
