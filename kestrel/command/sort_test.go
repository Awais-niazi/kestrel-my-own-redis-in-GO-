package command

import "testing"

func TestSort(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("RPUSH", "nums", "3", "1", "10", "2")

	s.expect("[1 2 3 10]", "SORT", "nums")
	s.expect("[10 3 2 1]", "SORT", "nums", "DESC")
	s.expect("[1 2]", "SORT", "nums", "LIMIT", "0", "2")
	s.expect("[2 3]", "SORT", "nums", "LIMIT", "1", "2")
	s.expect("[]", "SORT", "nums", "LIMIT", "10", "2")
	// ALPHA sorts as text, which orders 10 before 2.
	s.expect("[1 10 2 3]", "SORT", "nums", "ALPHA")

	s.do("RPUSH", "words", "banana", "apple", "cherry")
	s.expectErrPrefix("ERR One or more scores", "SORT", "words")
	s.expect("[apple banana cherry]", "SORT", "words", "ALPHA")

	// Sets and sorted sets are valid sources.
	s.do("SADD", "set", "5", "3", "9")
	s.expect("[3 5 9]", "SORT", "set")
	s.do("ZADD", "zset", "1", "20", "2", "10")
	s.expect("[10 20]", "SORT", "zset")

	s.expect("[]", "SORT", "nosuchkey")

	s.expect("4", "SORT", "nums", "STORE", "dst")
	s.expect("[1 2 3 10]", "LRANGE", "dst", "0", "-1")
	s.expect("list", "TYPE", "dst")
	// Sorting an absent key into a destination clears it.
	s.expect("0", "SORT", "nosuchkey", "STORE", "dst")
	s.expect("0", "EXISTS", "dst")

	s.expect("[1 2 3 10]", "SORT_RO", "nums")
	s.expectErrPrefix("ERR syntax error", "SORT_RO", "nums", "STORE", "dst")

	// BY and GET are refused rather than silently ignored.
	s.expectErrPrefix("ERR 'SORT ... BY pattern' is not supported", "SORT", "nums", "BY", "w_*")
	s.expectErrPrefix("ERR 'SORT ... GET pattern' is not supported", "SORT", "nums", "GET", "w_*")
	s.expectErrPrefix("ERR syntax error", "SORT", "nums", "BOGUS")

	s.do("SET", "str", "v")
	s.expectErrPrefix("WRONGTYPE", "SORT", "str")
}
