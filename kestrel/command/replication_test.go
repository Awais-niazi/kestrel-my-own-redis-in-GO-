package command

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// TestEffectCanonicalization pins the exact effect each write produces.
//
// These assertions are the readable half of the ADR-008 mitigation: the
// registration check makes it impossible to forget that a write needs an
// effect, and this makes it visible when the effect is wrong.
func TestEffectCanonicalization(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	now := h.ks.Now()
	abs := func(ms int64) string { return itoa(int(now + ms)) }

	cases := []struct {
		name string
		cmd  []string
		want []string // empty means "nothing must be propagated"
	}{
		{"plain SET", []string{"SET", "k", "v"}, []string{"SET k v"}},
		{"relative expiry becomes absolute",
			[]string{"SET", "k", "v", "EX", "100"}, []string{"SET k v PXAT " + abs(100_000)}},
		{"PX becomes absolute",
			[]string{"SET", "k", "v", "PX", "1500"}, []string{"SET k v PXAT " + abs(1500)}},
		{"EXAT is already absolute",
			[]string{"SET", "k", "v", "EXAT", "2000000000"}, []string{"SET k v PXAT 2000000000000"}},
		{"KEEPTTL is preserved",
			[]string{"SET", "k", "v2", "KEEPTTL"}, []string{"SET k v2 KEEPTTL"}},
		{"conditional SET that applies loses its condition",
			[]string{"SET", "fresh", "v", "NX"}, []string{"SET fresh v"}},
		{"conditional SET that does not apply propagates nothing",
			[]string{"SET", "fresh", "other", "NX"}, nil},
		{"SETNX becomes an unconditional SET",
			[]string{"SETNX", "n", "1"}, []string{"SET n 1"}},
		{"SETNX that fails propagates nothing", []string{"SETNX", "n", "2"}, nil},
		{"SETEX becomes SET with an absolute expiry",
			[]string{"SETEX", "e", "10", "v"}, []string{"SET e v PXAT " + abs(10_000)}},
		{"PSETEX becomes SET with an absolute expiry",
			[]string{"PSETEX", "pe", "10000", "v"}, []string{"SET pe v PXAT " + abs(10_000)}},
		{"GETSET becomes SET", []string{"GETSET", "k", "v3"}, []string{"SET k v3"}},
		{"GETDEL becomes DEL", []string{"GETDEL", "k"}, []string{"DEL k"}},
		{"GETDEL of a missing key propagates nothing", []string{"GETDEL", "k"}, nil},
		{"MSET is already deterministic",
			[]string{"MSET", "a", "1", "b", "2"}, []string{"MSET a 1 b 2"}},
		{"MSETNX becomes MSET",
			[]string{"MSETNX", "c", "3", "d", "4"}, []string{"MSET c 3 d 4"}},
		{"MSETNX that fails propagates nothing", []string{"MSETNX", "c", "9"}, nil},
		{"INCR is logged verbatim", []string{"INCR", "ctr"}, []string{"INCR ctr"}},
		{"INCRBY is logged verbatim", []string{"INCRBY", "ctr", "5"}, []string{"INCRBY ctr 5"}},
		{"APPEND is logged verbatim", []string{"APPEND", "app", "xy"}, []string{"APPEND app xy"}},
		{"SETRANGE is logged verbatim",
			[]string{"SETRANGE", "app", "1", "z"}, []string{"SETRANGE app 1 z"}},
		{"INCRBYFLOAT is logged as its result",
			[]string{"INCRBYFLOAT", "flt", "1.5"}, []string{"SET flt 1.5"}},
		{"INCRBYFLOAT accumulates in the log too",
			[]string{"INCRBYFLOAT", "flt", "0.1"}, []string{"SET flt 1.6"}},
		{"EXPIRE becomes PEXPIREAT",
			[]string{"EXPIRE", "a", "60"}, []string{"PEXPIREAT a " + abs(60_000)}},
		{"PEXPIRE becomes PEXPIREAT",
			[]string{"PEXPIRE", "a", "1000"}, []string{"PEXPIREAT a " + abs(1000)}},
		{"EXPIREAT in the past becomes DEL",
			[]string{"EXPIREAT", "a", "1"}, []string{"DEL a"}},
		{"EXPIRE on a missing key propagates nothing",
			[]string{"EXPIRE", "gone", "60"}, nil},
		{"PERSIST is logged verbatim", []string{"SET", "p", "v", "EX", "50"},
			[]string{"SET p v PXAT " + abs(50_000)}},
		{"PERSIST", []string{"PERSIST", "p"}, []string{"PERSIST p"}},
		{"PERSIST with no TTL propagates nothing", []string{"PERSIST", "p"}, nil},
		{"GETEX with an expiry becomes PEXPIREAT",
			[]string{"GETEX", "b", "EX", "30"}, []string{"PEXPIREAT b " + abs(30_000)}},
		{"GETEX PERSIST", []string{"GETEX", "b", "PERSIST"}, []string{"PERSIST b"}},
		{"GETEX with no options propagates nothing", []string{"GETEX", "b"}, nil},
		{"DEL is logged verbatim", []string{"DEL", "b"}, []string{"DEL b"}},
		{"DEL of a missing key propagates nothing", []string{"DEL", "b"}, nil},
		{"RENAME is logged verbatim",
			[]string{"RENAME", "n", "n2"}, []string{"RENAME n n2"}},
		{"RENAMENX becomes RENAME",
			[]string{"RENAMENX", "n2", "n3"}, []string{"RENAME n2 n3"}},
		{"COPY gains REPLACE",
			[]string{"COPY", "n3", "n4"}, []string{"COPY n3 n4 REPLACE"}},
		{"COPY that fails propagates nothing", []string{"COPY", "n3", "n4"}, nil},
		{"reads propagate nothing", []string{"GET", "n3"}, nil},
		{"failed writes propagate nothing", []string{"INCR", "app"}, nil},
	}

	for _, c := range cases {
		h.takeEffects()
		s.do(c.cmd...)
		got := h.takeEffects()
		rendered := make([]string, len(got))
		for i, e := range got {
			rendered[i] = e.String()
		}
		if strings.Join(rendered, " | ") != strings.Join(c.want, " | ") {
			t.Errorf("%s: %v produced %v, want %v",
				c.name, c.cmd, rendered, c.want)
		}
	}
}

func TestLazyExpiryPropagatesDelete(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "k", "v", "PX", "50")
	h.takeEffects()
	h.advance(51)
	s.do("GET", "k")
	got := h.takeEffects()
	if len(got) != 1 || got[0].String() != "DEL k" {
		t.Fatalf("lazy expiry produced %v, want [DEL k]", got)
	}
}

// TestFollowerConverges is the differential replication test from §12: a
// randomized write stream runs against a leader, its effect stream is applied
// to a follower, and the two must end up holding exactly the same data.
//
// This is what catches a wrong effect rewrite, which is the failure mode
// ADR-008 and R5 are about. It is also why the effect path exists from M1
// rather than being introduced with persistence in M3.
func TestFollowerConverges(t *testing.T) {
	leader := newTestHost(t)
	follower := newTestHost(t)

	// Both nodes read the same wall clock, so an absolute expiry means the
	// same instant on each.
	follower.now.Store(leader.now.Load())
	follower.ks.SetClock(func() int64 { return leader.now.Load() })
	// The follower must never expire on its own clock: it waits for the
	// leader's DEL (FR-3.4).
	follower.ks.SetReplica(true)

	followerSession := newSession(t, follower)
	followerSession.cl.Replica = true
	// Effects applied to the follower must not cascade into a second stream.
	follower.mu.Lock()
	follower.sink = func(db int, args [][]byte) {}
	follower.mu.Unlock()

	var applied int
	leader.mu.Lock()
	leader.sink = func(db int, args [][]byte) {
		applied++
		if !followerSession.cl.Select(follower.ks, db) {
			t.Errorf("replaying into database %d failed", db)
			return
		}
		Execute(follower, followerSession.cl, args)
		followerSession.wr.Flush()
		followerSession.out.Reset()
	}
	leader.mu.Unlock()

	s := newSession(t, leader)
	rng := rand.New(rand.NewSource(20240914))
	for i := 0; i < 6000; i++ {
		if i%500 == 0 {
			// Move time forward so that TTLs actually elapse and the lazy
			// and active expiry paths both produce DELs.
			leader.now.Add(rng.Int63n(40_000))
			leader.ks.ExpirePass(farFuture())
		}
		k := fmt.Sprintf("k%d", rng.Intn(60))
		switch rng.Intn(18) {
		case 0:
			s.do("SET", k, fmt.Sprintf("v%d", i))
		case 1:
			s.do("SET", k, "x", "EX", itoa(1+rng.Intn(30)))
		case 2:
			s.do("SET", k, "y", "NX")
		case 3:
			s.do("SET", k, "z", "XX", "KEEPTTL")
		case 4:
			s.do("SETNX", k, "n")
		case 5:
			s.do("SETEX", k, itoa(1+rng.Intn(20)), "e")
		case 6:
			s.do("GETSET", k, "gs")
		case 7:
			s.do("GETDEL", k)
		case 8:
			s.do("INCR", k)
		case 9:
			s.do("INCRBYFLOAT", k, "0.1")
		case 10:
			s.do("APPEND", k, "a")
		case 11:
			s.do("SETRANGE", k, itoa(rng.Intn(4)), "QQ")
		case 12:
			s.do("EXPIRE", k, itoa(1+rng.Intn(25)))
		case 13:
			s.do("PERSIST", k)
		case 14:
			s.do("DEL", k)
		case 15:
			s.do("MSET", k, "m", k+"-pair", "m2")
		case 16:
			s.do("GETEX", k, "EX", "5")
		case 17:
			s.do("RENAME", k, k+"-renamed")
		}
		// Reads on the leader drive lazy expiry, which is itself replicated.
		s.do("GET", k)
	}
	leader.ks.ExpirePass(farFuture())

	if applied == 0 {
		t.Fatal("no effects were replicated")
	}
	t.Logf("replicated %d effects", applied)

	want := dump(t, leader)
	got := dump(t, follower)
	if want != got {
		t.Errorf("leader and follower diverged at %s", firstDiff(want, got))
	}
}

// dump renders the whole visible dataset of a host as text, for comparison.
func dump(t *testing.T, h *testHost) string {
	t.Helper()
	s := newSession(t, h)
	var b strings.Builder
	for i := 0; i < h.ks.NumDatabases(); i++ {
		if !s.cl.Select(h.ks, i) {
			t.Fatal("select failed")
		}
		keys := s.do("KEYS", "*")
		names := make([]string, 0, len(keys.Elems))
		for _, k := range keys.Elems {
			names = append(names, string(k.Str))
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(&b, "db%d %q type=%s value=%q pttl=%s\n",
				i, k, str(s.do("TYPE", k)), str(s.do("GET", k)), str(s.do("PTTL", k)))
		}
	}
	return b.String()
}

func firstDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) || i < len(lb); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			return fmt.Sprintf("line %d:\n  leader:   %s\n  follower: %s", i+1, x, y)
		}
	}
	return "(identical)"
}
