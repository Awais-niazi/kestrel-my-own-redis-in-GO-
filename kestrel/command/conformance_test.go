package command

import (
	"strings"
	"testing"
)

// These are the differences the differential test found against a real
// redis-server (see ../difftest). Each one passed every unit test in this
// package before it was fixed, because the tests and the code shared an
// assumption about what the reference does. They are pinned here so the
// assumption cannot come back.

// RENAMENX to a key's own name must fail: the destination exists, and it
// being the same key does not change that. Plain RENAME to the same name
// still succeeds.
func TestRenameNXToItself(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "k", "v")

	if got := str(s.do("RENAMENX", "k", "k")); got != "0" {
		t.Errorf("RENAMENX k k = %s, want 0", got)
	}
	if got := str(s.do("RENAME", "k", "k")); got != "OK" {
		t.Errorf("RENAME k k = %s, want OK", got)
	}
	if got := str(s.do("GET", "k")); got != "v" {
		t.Errorf("the key did not survive: GET k = %s", got)
	}
	if got := str(s.do("RENAMENX", "k", "fresh")); got != "1" {
		t.Errorf("RENAMENX to an unused name = %s, want 1", got)
	}
}

// Infinite scores are spelled "inf" and "-inf", in replies and in the
// effects this server writes to its own log. Go's formatter spells them
// "+Inf" and "-Inf", which no other Redis will read back.
func TestInfiniteScoresUseTheWireSpelling(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	h.takeEffects()

	s.do("ZADD", "z", "inf", "hi")
	s.do("ZADD", "z", "-inf", "lo")

	if got := str(s.do("ZSCORE", "z", "hi")); got != "inf" {
		t.Errorf("ZSCORE of an infinite score = %q, want inf", got)
	}
	if got := str(s.do("ZRANGE", "z", "0", "-1", "WITHSCORES")); got != "[lo -inf hi inf]" {
		t.Errorf("ZRANGE WITHSCORES = %s, want [lo -inf hi inf]", got)
	}
	if got := str(s.do("ZINCRBY", "z", "0", "hi")); got != "inf" {
		t.Errorf("ZINCRBY on an infinite score = %q, want inf", got)
	}

	// The log has to be readable by the thing that replays it, and by a
	// replica, which is why the propagated text matters as much as the reply.
	for _, e := range h.takeEffects() {
		if s := e.String(); strings.Contains(s, "Inf") {
			t.Errorf("an effect was propagated as %q; scores go on the wire as inf/-inf", s)
		}
	}
}

// LPOP with a count distinguishes "no such key" from "popped nothing". A
// missing key is a null array; a list that is there and a count of zero is
// an empty one.
func TestPopWithCountZero(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("RPUSH", "l", "a", "b", "c")

	for _, cmd := range []string{"LPOP", "RPOP"} {
		if got := str(s.do(cmd, "l", "0")); got != "[]" {
			t.Errorf("%s on a present list with count 0 = %s, want an empty array", cmd, got)
		}
		if got := str(s.do(cmd, "missing", "0")); got != "<nil>" {
			t.Errorf("%s on a missing key with count 0 = %s, want a null array", cmd, got)
		}
		if got := str(s.do(cmd, "missing", "2")); got != "<nil>" {
			t.Errorf("%s on a missing key with a count = %s, want a null array", cmd, got)
		}
	}
	if got := str(s.do("LLEN", "l")); got != "3" {
		t.Errorf("a count of zero removed something: LLEN = %s", got)
	}
	// Without a count the reply is a bulk string, and a missing key is nil.
	if got := str(s.do("LPOP", "missing")); got != "<nil>" {
		t.Errorf("LPOP on a missing key = %s, want nil", got)
	}
}

// GETRANGE does not share LRANGE's index convention, in two ways that are
// both observable. A pair written negative and the wrong way round is
// rejected before the indexes are resolved, and an end still negative after
// the length is added clamps up to zero.
func TestGetRangeNegativeIndexes(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "k", "hello") // length 5

	cases := []struct{ start, end, want string }{
		{"-10", "-10", "h"},               // both resolve below zero, equal as written
		{"-10", "-6", "h"},                // start < end as written, both clamp to 0
		{"-10", "-5", "h"},                // end resolves to exactly 0
		{"-6", "-10", ""},                 // backwards as written: rejected outright
		{"-5", "-10", ""},                 // likewise
		{"-5", "-6", ""},                  // likewise
		{"0", "-10", "h"},                 // start not negative, so no early rejection
		{"0", "-6", "h"},                  // and the end clamps up to zero
		{"2", "-10", ""},                  // start past the clamped end
		{"-3", "-1", "llo"},               // the ordinary case
		{"-100", "2", "hel"},              // start clamps to the beginning
		{"2", "-100", ""},                 // end clamps to the beginning, before start
		{"0", "-1", "hello"},              // the whole string
		{"-1", "-1", "o"},                 // the last byte
		{"0", "99", "hello"},              // end past the end
		{"-9999999999999", "-1", "hello"}, // far enough to overflow a naive add
	}
	for _, c := range cases {
		if got := str(s.do("GETRANGE", "k", c.start, c.end)); got != c.want {
			t.Errorf("GETRANGE k %s %s = %q, want %q", c.start, c.end, got, c.want)
		}
	}

	// LRANGE keeps its own convention, which differs on the second point.
	s.do("RPUSH", "l", "a", "b", "c", "d", "e")
	if got := str(s.do("LRANGE", "l", "-10", "-6")); got != "[]" {
		t.Errorf("LRANGE l -10 -6 = %s, want an empty array: unlike GETRANGE, "+
			"LRANGE leaves a negative end negative", got)
	}
	if got := str(s.do("LRANGE", "l", "-3", "-1")); got != "[c d e]" {
		t.Errorf("LRANGE l -3 -1 = %s", got)
	}
}

// A sanity check that the promoted encodings did not change any of this.
func TestGetRangeOnAnIntegerEncodedValue(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "n", "1234567")
	for _, c := range []struct{ start, end, want string }{
		{"-10", "-6", "12"}, // length 7, so -10 clamps to 0 and -6 resolves to 1
		{"-3", "-1", "567"},
		{"-5", "-6", ""},
	} {
		if got := str(s.do("GETRANGE", "n", c.start, c.end)); got != c.want {
			t.Errorf("GETRANGE n %s %s = %q, want %q", c.start, c.end, got, c.want)
		}
	}
}
