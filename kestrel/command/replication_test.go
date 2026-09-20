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
		{"INCRBYFLOAT is logged as its result, keeping the TTL a SET would clear",
			[]string{"INCRBYFLOAT", "flt", "1.5"}, []string{"SET flt 1.5 KEEPTTL"}},
		{"INCRBYFLOAT accumulates in the log too",
			[]string{"INCRBYFLOAT", "flt", "0.1"}, []string{"SET flt 1.6 KEEPTTL"}},
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

// TestCollectionEffectCanonicalization pins the effects of the collection
// writes whose arguments are not replay-safe as sent.
func TestCollectionEffectCanonicalization(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	cases := []struct {
		name  string
		setup [][]string
		cmd   []string
		want  []string
	}{
		{"HSET is deterministic", nil,
			[]string{"HSET", "h", "f", "v"}, []string{"HSET h f v"}},
		{"HSETNX becomes an unconditional HSET", nil,
			[]string{"HSETNX", "h", "n", "1"}, []string{"HSET h n 1"}},
		{"HSETNX that fails propagates nothing", nil,
			[]string{"HSETNX", "h", "n", "2"}, nil},
		{"HINCRBY is a delta on one key, so it is logged verbatim", nil,
			[]string{"HINCRBY", "h", "c", "5"}, []string{"HINCRBY h c 5"}},
		{"HINCRBYFLOAT is logged as its result", nil,
			[]string{"HINCRBYFLOAT", "h", "f2", "1.5"}, []string{"HSET h f2 1.5"}},
		{"HDEL is deterministic", nil,
			[]string{"HDEL", "h", "f"}, []string{"HDEL h f"}},
		{"HDEL of an absent field propagates nothing", nil,
			[]string{"HDEL", "h", "f"}, nil},

		{"SADD is deterministic", nil,
			[]string{"SADD", "s", "a", "b"}, []string{"SADD s a b"}},
		{"SADD of existing members propagates nothing", nil,
			[]string{"SADD", "s", "a"}, nil},
		{"SREM is deterministic", nil,
			[]string{"SREM", "s", "b"}, []string{"SREM s b"}},

		{"LPUSH is deterministic", nil,
			[]string{"RPUSH", "l", "x", "y"}, []string{"RPUSH l x y"}},
		{"LPOP is deterministic", nil,
			[]string{"LPOP", "l"}, []string{"LPOP l"}},
		{"LPOP of an empty list propagates nothing",
			[][]string{{"DEL", "gone"}}, []string{"LPOP", "gone"}, nil},

		{"ZADD is rewritten with the scores it applied", nil,
			[]string{"ZADD", "z", "1", "a"}, []string{"ZADD z 1 a"}},
		{"a conditional ZADD loses its condition", nil,
			[]string{"ZADD", "z", "GT", "9", "a"}, []string{"ZADD z 9 a"}},
		{"a ZADD the condition rejected propagates nothing", nil,
			[]string{"ZADD", "z", "GT", "1", "a"}, nil},
		{"ZINCRBY is logged as the resulting score", nil,
			[]string{"ZINCRBY", "z", "1", "a"}, []string{"ZADD z 10 a"}},
		{"ZADD INCR is logged as the resulting score", nil,
			[]string{"ZADD", "z", "INCR", "5", "a"}, []string{"ZADD z 15 a"}},
		{"ZREM is deterministic", nil,
			[]string{"ZREM", "z", "a"}, []string{"ZREM z a"}},
		{"a same-key range removal is deterministic",
			[][]string{{"ZADD", "zr", "1", "a", "2", "b"}},
			[]string{"ZREMRANGEBYSCORE", "zr", "0", "1"},
			[]string{"ZREMRANGEBYSCORE zr 0 1"}},
	}

	for _, c := range cases {
		for _, pre := range c.setup {
			s.do(pre...)
		}
		h.takeEffects()
		s.do(c.cmd...)
		got := h.takeEffects()
		rendered := make([]string, len(got))
		for i, e := range got {
			rendered[i] = e.String()
		}
		if strings.Join(rendered, " | ") != strings.Join(c.want, " | ") {
			t.Errorf("%s: %v produced %v, want %v", c.name, c.cmd, rendered, c.want)
		}
	}
}

// TestRandomChoiceEffectsArePinned covers the commands whose result cannot be
// predicted: their effect must name the members they actually touched, not
// repeat the request.
func TestRandomChoiceEffectsArePinned(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SADD", "s", "a", "b", "c", "d")
	s.do("ZADD", "z", "1", "a", "2", "b")
	h.takeEffects()

	popped := str(s.do("SPOP", "s", "2"))
	got := h.takeEffects()
	if len(got) != 1 {
		t.Fatalf("SPOP produced %v", got)
	}
	// The effect must be an SREM naming exactly the members that were taken.
	fields := strings.Fields(got[0].String())
	if fields[0] != "SREM" || fields[1] != "s" || len(fields) != 4 {
		t.Fatalf("SPOP effect is %q, want an SREM of two members", got[0])
	}
	for _, m := range fields[2:] {
		if !strings.Contains(popped, m) {
			t.Errorf("SPOP removed %q in the log but returned %s", m, popped)
		}
	}

	h.takeEffects()
	s.do("ZPOPMIN", "z")
	got = h.takeEffects()
	if len(got) != 1 || got[0].String() != "ZREM z a" {
		t.Fatalf("ZPOPMIN effect is %v, want [ZREM z a]", got)
	}
}

// A read hides an expired key and says nothing; the DEL that records the
// death comes from the active cycle, which holds the write-ordering lock.
// See docs/design-notes.md issue 10.
func TestExpiryIsReportedByTheActiveCycleNotByAReader(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "k", "v", "PX", "50")
	h.takeEffects()
	h.advance(51)

	if got := str(s.do("GET", "k")); got != "<nil>" {
		t.Fatalf("GET served an expired key: %q", got)
	}
	if got := h.takeEffects(); len(got) != 0 {
		t.Fatalf("a read appended to the effect stream: %v", got)
	}

	h.ks.ExpirePass(farFuture())
	if got := h.takeEffects(); len(got) != 1 || got[0].String() != "DEL k" {
		t.Fatalf("active expiry produced %v, want [DEL k]", got)
	}
}

// A write reaps instead of hiding, because a verbatim effect is replayed
// against whatever the key holds and replay does not expire anything.
func TestAWriteOnAnExpiredKeyPropagatesTheDeleteFirst(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("RPUSH", "l", "a")
	s.do("PEXPIRE", "l", "50")
	h.takeEffects()
	h.advance(51)

	s.do("RPUSH", "l", "b")
	got := h.takeEffects()
	if len(got) != 2 || got[0].String() != "DEL l" || got[1].String() != "RPUSH l b" {
		t.Fatalf("the write produced %v, want [DEL l, RPUSH l b]", got)
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

	randomWorkload(t, leader, 20240914, 12000)

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
			typ := str(s.do("TYPE", k))
			fmt.Fprintf(&b, "db%d %q type=%s value=%s pttl=%s\n",
				i, k, typ, dumpValue(s, typ, k), str(s.do("PTTL", k)))
		}
	}
	return b.String()
}

// dumpValue renders a value in a form that is identical on two nodes holding
// the same data. Hash and set members have no defined order, so they are
// sorted; list and sorted set order is meaningful and is left alone.
func dumpValue(s *session, typ, key string) string {
	switch typ {
	case "string":
		return str(s.do("GET", key))
	case "list":
		return str(s.do("LRANGE", key, "0", "-1"))
	case "hash":
		return sortedPairs(str(s.do("HGETALL", key)))
	case "set":
		return sortedTokens(str(s.do("SMEMBERS", key)))
	case "zset":
		return str(s.do("ZRANGE", key, "0", "-1", "WITHSCORES"))
	default:
		return "<unknown type>"
	}
}

// sortedTokens sorts a bracketed reply so that unordered collections compare
// equal regardless of iteration order.
func sortedTokens(v string) string {
	inner := strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	if inner == "" {
		return "[]"
	}
	f := strings.Fields(inner)
	sort.Strings(f)
	return "[" + strings.Join(f, " ") + "]"
}

// sortedPairs sorts a flat field/value reply by field.
func sortedPairs(v string) string {
	inner := strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	if inner == "" {
		return "[]"
	}
	f := strings.Fields(inner)
	pairs := make([]string, 0, len(f)/2)
	for i := 0; i+1 < len(f); i += 2 {
		pairs = append(pairs, f[i]+"="+f[i+1])
	}
	sort.Strings(pairs)
	return "[" + strings.Join(pairs, " ") + "]"
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

// randomWorkload drives a broad randomized write stream against a host.
//
// It is shared by the replication and the recovery convergence tests. Both
// ask the same question of the effect stream -- does replaying it rebuild
// the dataset exactly -- and they must ask it of the same writes, or a
// command whose effect is wrong will be caught by one and missed by the
// other.
func randomWorkload(t *testing.T, h *testHost, seed int64, iterations int) {
	t.Helper()
	s := newSession(t, h)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < iterations; i++ {
		if i%500 == 0 {
			// Move time forward so that TTLs actually elapse and the lazy
			// and active expiry paths both produce DELs.
			h.now.Add(rng.Int63n(40_000))
			h.ks.ExpirePass(farFuture())
		}
		k := fmt.Sprintf("k%d", rng.Intn(60))
		// Collection keys are kept in their own namespaces, because a
		// randomly typed write against a shared key would spend most of its
		// time producing WRONGTYPE errors rather than exercising effects.
		lk := fmt.Sprintf("list%d", rng.Intn(12))
		hk := fmt.Sprintf("hash%d", rng.Intn(12))
		sk := fmt.Sprintf("set%d", rng.Intn(12))
		zk := fmt.Sprintf("zset%d", rng.Intn(12))
		member := fmt.Sprintf("m%d", rng.Intn(20))
		score := itoa(rng.Intn(40) - 20)

		switch rng.Intn(46) {
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

		// Lists.
		case 18:
			s.do("RPUSH", lk, member)
		case 19:
			s.do("LPUSH", lk, member, member+"b")
		case 20:
			s.do("LPOP", lk)
		case 21:
			s.do("RPOP", lk, itoa(1+rng.Intn(3)))
		case 22:
			s.do("LSET", lk, "0", member)
		case 23:
			s.do("LREM", lk, itoa(rng.Intn(3)-1), member)
		case 24:
			s.do("LTRIM", lk, "0", itoa(rng.Intn(8)))
		case 25:
			s.do("LINSERT", lk, "BEFORE", member, member+"i")
		case 26:
			s.do("LMOVE", lk, lk+"-alt", "LEFT", "RIGHT")

		// Hashes.
		case 27:
			s.do("HSET", hk, member, itoa(i))
		case 28:
			s.do("HSETNX", hk, member, "nx")
		case 29:
			s.do("HDEL", hk, member)
		case 30:
			s.do("HINCRBY", hk, "counter", itoa(rng.Intn(5)))
		case 31:
			s.do("HINCRBYFLOAT", hk, "float", "0.25")

		// Sets.
		case 32:
			s.do("SADD", sk, member)
		case 33:
			s.do("SADD", sk, itoa(rng.Intn(30))) // integer members, for intset
		case 34:
			s.do("SREM", sk, member)
		case 35:
			s.do("SPOP", sk)
		case 36:
			s.do("SPOP", sk, itoa(1+rng.Intn(2)))
		case 37:
			s.do("SMOVE", sk, sk+"-alt", member)
		case 38:
			s.do("SINTERSTORE", sk+"-dst", sk, sk+"-alt")
		case 39:
			s.do("SUNIONSTORE", sk+"-dst", sk, sk+"-alt")

		// Sorted sets.
		case 40:
			s.do("ZADD", zk, score, member)
		case 41:
			s.do("ZADD", zk, "GT", "CH", score, member)
		case 42:
			s.do("ZINCRBY", zk, "1.5", member)
		case 43:
			s.do("ZREM", zk, member)
		case 44:
			s.do("ZPOPMIN", zk)
		case 45:
			s.do("ZREMRANGEBYSCORE", zk, "-5", "5")
		}
		// Reads on the leader drive lazy expiry, which is itself replicated.
		s.do("GET", k)
		s.do("EXISTS", lk, hk, sk, zk)
	}
	h.ks.ExpirePass(farFuture())
}
