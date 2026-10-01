package command

import (
	"fmt"
	"testing"
)

// The promoted encodings are the ones these tests are about. Below the
// configured thresholds a collection is a listpack and the whole thing comes
// back in one call, which is what the reference does and what the existing
// TestHashScan, TestSetScan and TestZSetScan cover.

// scanCollection drives a cursor to completion and reports every entry it
// saw along with the largest single reply and the number of calls.
func scanCollection(t *testing.T, s *session, args ...string) (entries []string, worst, calls int) {
	t.Helper()
	cursor := "0"
	for {
		call := append([]string{args[0], args[1], cursor}, args[2:]...)
		v := s.do(call...)
		if len(v.Elems) != 2 {
			t.Fatalf("%s reply shape: %s", args[0], str(v))
		}
		got := len(v.Elems[1].Elems)
		if got > worst {
			worst = got
		}
		for _, e := range v.Elems[1].Elems {
			entries = append(entries, string(e.Str))
		}
		cursor = string(v.Elems[0].Str)
		calls++
		if cursor == "0" {
			return entries, worst, calls
		}
		if calls > 100000 {
			t.Fatalf("%s did not terminate", args[0])
		}
	}
}

// TestPromotedCollectionScansArePaged is the other half of what the custom
// table was for.
//
// A promoted hash, set or sorted set used to answer the whole collection in
// one call whatever COUNT said -- a million-field hash returned a million
// fields. That is the same unbounded reply SCAN had, and it is closed the
// same way, because the promoted encodings are now the same table.
func TestPromotedCollectionScansArePaged(t *testing.T) {
	const n = 3000
	// A bucket's chain comes out whole, so a reply can overshoot by a chain.
	// It cannot overshoot by the collection.
	const count = 20
	const ceiling = count * 4

	t.Run("HSCAN", func(t *testing.T) {
		h := newTestHost(t)
		s := newSession(t, h)
		for i := 0; i < n; i++ {
			s.do("HSET", "h", fmt.Sprintf("f%d", i), fmt.Sprintf("v%d", i))
		}
		if enc := str(s.do("OBJECT", "ENCODING", "h")); enc != "hashtable" {
			t.Fatalf("the hash is %q encoded; this test needs the promoted one", enc)
		}
		// NOVALUES keeps the entries one per field so counting is simple.
		entries, worst, calls := scanCollection(t, s, "HSCAN", "h", "COUNT",
			fmt.Sprint(count), "NOVALUES")
		if worst > ceiling {
			t.Errorf("the largest reply to COUNT %d held %d fields, want at most %d",
				count, worst, ceiling)
		}
		assertCovers(t, entries, n, "f", calls)
	})

	t.Run("SSCAN", func(t *testing.T) {
		h := newTestHost(t)
		s := newSession(t, h)
		for i := 0; i < n; i++ {
			s.do("SADD", "s", fmt.Sprintf("m%d", i))
		}
		if enc := str(s.do("OBJECT", "ENCODING", "s")); enc != "hashtable" {
			t.Fatalf("the set is %q encoded; this test needs the promoted one", enc)
		}
		entries, worst, calls := scanCollection(t, s, "SSCAN", "s", "COUNT", fmt.Sprint(count))
		if worst > ceiling {
			t.Errorf("the largest reply to COUNT %d held %d members, want at most %d",
				count, worst, ceiling)
		}
		assertCovers(t, entries, n, "m", calls)
	})

	t.Run("ZSCAN", func(t *testing.T) {
		h := newTestHost(t)
		s := newSession(t, h)
		for i := 0; i < n; i++ {
			s.do("ZADD", "z", fmt.Sprint(i), fmt.Sprintf("m%d", i))
		}
		if enc := str(s.do("OBJECT", "ENCODING", "z")); enc != "skiplist" {
			t.Fatalf("the sorted set is %q encoded; this test needs the promoted one", enc)
		}
		// Entries alternate member and score, so the reply is twice as long.
		entries, worst, calls := scanCollection(t, s, "ZSCAN", "z", "COUNT", fmt.Sprint(count))
		if worst > 2*ceiling {
			t.Errorf("the largest reply to COUNT %d held %d entries, want at most %d",
				count, worst, 2*ceiling)
		}
		members := make([]string, 0, len(entries)/2)
		for i := 0; i < len(entries); i += 2 {
			members = append(members, entries[i])
		}
		assertCovers(t, members, n, "m", calls)
	})
}

// assertCovers checks that a full iteration reached every member. Duplicates
// are allowed, as the reference allows them.
func assertCovers(t *testing.T, got []string, n int, prefix string, calls int) {
	t.Helper()
	seen := make(map[string]bool, n)
	for _, e := range got {
		seen[e] = true
	}
	for i := 0; i < n; i++ {
		if k := fmt.Sprintf("%s%d", prefix, i); !seen[k] {
			t.Fatalf("a full iteration missed %q (%d calls, %d entries)", k, calls, len(got))
		}
	}
	if calls < 2 {
		t.Errorf("%d members came back in %d call(s); the scan was not paged", n, calls)
	}
}

// A small collection is still one page, which is what the reference does for
// its compact encodings and what clients of those encodings expect.
func TestSmallCollectionScansAreOnePage(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for i := 0; i < 10; i++ {
		s.do("HSET", "h", fmt.Sprintf("f%d", i), "v")
		s.do("SADD", "s", fmt.Sprintf("m%d", i))
		s.do("ZADD", "z", fmt.Sprint(i), fmt.Sprintf("m%d", i))
	}
	for _, cmd := range []string{"HSCAN", "SSCAN", "ZSCAN"} {
		key := map[string]string{"HSCAN": "h", "SSCAN": "s", "ZSCAN": "z"}[cmd]
		v := s.do(cmd, key, "0", "COUNT", "1")
		if c := string(v.Elems[0].Str); c != "0" {
			t.Errorf("%s over a listpack returned cursor %q, want 0", cmd, c)
		}
		if got := len(v.Elems[1].Elems); got == 0 {
			t.Errorf("%s over a listpack returned nothing", cmd)
		}
	}
}
