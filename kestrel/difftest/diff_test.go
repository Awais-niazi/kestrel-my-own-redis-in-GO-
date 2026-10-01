// Package difftest compares Kestrel's replies against the reference
// implementation's, command for command, over a randomized stream.
//
// Everything else in this repository tests Kestrel against what Kestrel's
// author believed Redis does. This tests it against what Redis does. The two
// are not the same thing, and the gap between them is where conformance bugs
// live: the kind that every unit test passes over because the test and the
// code share an assumption.
//
// It needs a redis-server on PATH and does not run by default:
//
//	DIFFTEST=1 go test ./difftest/ -v
//
// A reply that differs is not automatically a bug. Some differences are
// documented deviations and some commands are not deterministic at all, so
// both are described explicitly below rather than left for a reader to infer
// from a failure.
package difftest

import (
	"bufio"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"kestrel/resp"
)

// ---------------------------------------------------------------- servers

type server struct {
	name string
	conn net.Conn
	rd   *resp.ReplyReader
}

func (s *server) do(args ...string) (resp.Value, error) {
	raw := make([][]byte, len(args))
	for i, a := range args {
		raw[i] = []byte(a)
	}
	if _, err := s.conn.Write(resp.EncodeCommand(nil, raw...)); err != nil {
		return resp.Value{}, err
	}
	s.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return s.rd.ReadReply()
}

func (s *server) close() { s.conn.Close() }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func connect(t *testing.T, name string, port int) *server {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			s := &server{name: name, conn: c, rd: resp.NewReplyReader(bufio.NewReader(c))}
			if v, err := s.do("PING"); err == nil && strings.EqualFold(string(v.Str), "PONG") {
				t.Cleanup(s.close)
				return s
			}
			c.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s at %s never answered PING", name, addr)
	return nil
}

// start launches a server process. Callers connect to it by port.
func start(t *testing.T, name, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	t.Cleanup(func() {
		if s := out.String(); s != "" && t.Failed() {
			t.Logf("%s said:\n%s", name, s)
		}
	})
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
}

// ---------------------------------------------------------------- compare

// normalise renders a reply as a string, flattening the differences that are
// not behavioural: RESP2 and RESP3 spell some types differently, and the two
// servers are allowed to disagree about them.
func normalise(v resp.Value) string {
	switch v.Kind {
	case resp.KindSimple, resp.KindBulk, resp.KindVerbatim:
		return "s:" + string(v.Str)
	case resp.KindError:
		// Error text beyond the code is wording, which is not behaviour.
		return "e:" + errCode(string(v.Str))
	case resp.KindInt:
		return "i:" + strconv.FormatInt(v.Int, 10)
	case resp.KindNull, resp.KindNullArray:
		return "nil"
	case resp.KindBool:
		return "i:" + map[bool]string{true: "1", false: "0"}[v.Bool]
	case resp.KindDouble:
		return "s:" + strconv.FormatFloat(v.Float, 'g', -1, 64)
	case resp.KindArray, resp.KindMap, resp.KindSet, resp.KindPush:
		parts := make([]string, len(v.Elems))
		for i, e := range v.Elems {
			parts[i] = normalise(e)
		}
		return "[" + strings.Join(parts, " ") + "]"
	default:
		return fmt.Sprintf("?%d", v.Kind)
	}
}

// errCode is the leading token of an error reply, which is the part clients
// branch on. The rest is a human-readable message that the two servers are
// not expected to word identically.
func errCode(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

// sameNumber compares two replies as numbers rather than as text.
//
// It exists for INCRBYFLOAT, which is a documented deviation: Kestrel
// accumulates at float64 precision where the reference uses long double, so
// a long chain of increments drifts in the last couple of significant
// digits. Comparing the text would report that difference on every run and
// bury anything real underneath it; comparing the numbers still catches a
// wrong answer.
func sameNumber(a, b string) bool {
	x, err1 := strconv.ParseFloat(strings.TrimPrefix(a, "s:"), 64)
	y, err2 := strconv.ParseFloat(strings.TrimPrefix(b, "s:"), 64)
	if err1 != nil || err2 != nil {
		return false
	}
	if x == y {
		return true
	}
	scale := math.Max(math.Abs(x), math.Abs(y))
	return math.Abs(x-y) <= 1e-9*math.Max(scale, 1)
}

// ttlClass reduces a TTL reply to what is comparable across two servers
// whose clocks are not the same: no such key, no expiry, or some time left.
// The remaining milliseconds are not a behaviour either one promises.
func ttlClass(s string) string {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "i:"), 10, 64)
	if err != nil {
		return s
	}
	switch {
	case n == -2:
		return "no-key"
	case n == -1:
		return "no-expiry"
	default:
		return "has-expiry"
	}
}

// unordered sorts an array reply's elements, for commands whose order is
// explicitly unspecified.
func unordered(s string) string {
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return s
	}
	parts := strings.Fields(s[1 : len(s)-1])
	sort.Strings(parts)
	return "[" + strings.Join(parts, " ") + "]"
}

// ---------------------------------------------------------------- commands

// A generated command and how its replies should be compared.
type mode int

const (
	exact     mode = iota // replies must match
	setwise               // replies must match as multisets
	floatwise             // replies must match as numbers, within tolerance
	ttlwise               // only the category of a TTL reply is comparable
	skipReply             // run on both, compare nothing (non-deterministic)
)

type gen struct {
	mode mode
	args []string
}

func pick(r *rand.Rand, xs ...string) string { return xs[r.Intn(len(xs))] }

func key(r *rand.Rand, prefix string, n int) string {
	return fmt.Sprintf("%s%d", prefix, r.Intn(n))
}

func val(r *rand.Rand) string {
	switch r.Intn(4) {
	case 0:
		return strconv.Itoa(r.Intn(1000) - 500) // integers, for the int encoding
	case 1:
		return strings.Repeat("x", r.Intn(80)) // long-ish, to cross thresholds
	case 2:
		return ""
	default:
		return fmt.Sprintf("v%d", r.Intn(1000))
	}
}

// generate produces one command.
//
// The pools matter as much as the commands. Keys are drawn from a small
// space so collisions are frequent, and the type-specific pools overlap with
// the generic one, so a list command lands on a string key often enough to
// exercise every WRONGTYPE path rather than only the happy ones.
//
// Three kinds of command are deliberately absent. Anything whose reply is
// random and which also mutates -- SPOP -- would desynchronise the two
// keyspaces and make every later comparison meaningless. Anything that
// expires on the wall clock is used only with far-future deadlines, so
// nothing disappears mid-run. And OBJECT ENCODING is a documented deviation
// by design, since the encodings are Go data structures with different names.
func generate(r *rand.Rand) gen {
	// A shared pool, so type errors happen on purpose.
	any := func() string {
		return pick(r, key(r, "k", 30), key(r, "h", 6), key(r, "l", 6),
			key(r, "s", 6), key(r, "z", 6))
	}
	k := func() string { return key(r, "k", 30) }
	h := func() string { return key(r, "h", 6) }
	l := func() string { return key(r, "l", 6) }
	st := func() string { return key(r, "s", 6) }
	z := func() string { return key(r, "z", 6) }
	// Lexicographic ranges are defined only when every member shares a
	// score. The reference says so, and with mixed scores it walks its
	// skiplist in score order and returns whatever that happens to pass,
	// which is not a behaviour to hold anything to. These keys only ever
	// receive score 0, so the commands that need that hold it.
	lex := func() string { return key(r, "lex", 3) }
	f := func() string { return key(r, "f", 150) }
	n := func(max int) string { return strconv.Itoa(r.Intn(max)) }
	idx := func() string { return strconv.Itoa(r.Intn(21) - 10) }
	score := func() string { return pick(r, "0", "1", "-1", "2.5", "100", "-99.5", "inf", "-inf") }
	ttl := func(r *rand.Rand) string { return pick(r, "100000", "400000", "900000", "2000000") }

	cmds := []func() gen{
		// ---- strings
		func() gen { return gen{exact, []string{"SET", any(), val(r)}} },
		func() gen { return gen{exact, []string{"SET", k(), val(r), pick(r, "NX", "XX", "KEEPTTL")}} },
		func() gen { return gen{exact, []string{"SET", k(), val(r), "GET"}} },
		func() gen { return gen{exact, []string{"SETNX", any(), val(r)}} },
		func() gen { return gen{exact, []string{"GETSET", any(), val(r)}} },
		func() gen { return gen{exact, []string{"GET", any()}} },
		func() gen { return gen{exact, []string{"APPEND", any(), val(r)}} },
		func() gen { return gen{exact, []string{"STRLEN", any()}} },
		func() gen { return gen{exact, []string{pick(r, "INCR", "DECR"), any()}} },
		func() gen { return gen{exact, []string{"INCRBY", any(), n(10)}} },
		func() gen { return gen{exact, []string{"DECRBY", any(), n(10)}} },
		func() gen { return gen{exact, []string{"GETRANGE", any(), idx(), idx()}} },
		func() gen { return gen{exact, []string{"SETRANGE", k(), n(6), val(r)}} },
		func() gen { return gen{exact, []string{"GETDEL", any()}} },
		func() gen { return gen{exact, []string{"MGET", any(), any(), any()}} },
		func() gen { return gen{exact, []string{"MSET", k(), val(r), k(), val(r)}} },
		func() gen { return gen{exact, []string{"MSETNX", k(), val(r), k(), val(r)}} },
		func() gen { return gen{floatwise, []string{"INCRBYFLOAT", any(), pick(r, "1.5", "-0.25", "2")}} },

		// ---- generic
		func() gen { return gen{exact, []string{"DEL", any(), any()}} },
		func() gen { return gen{exact, []string{"UNLINK", any()}} },
		func() gen { return gen{exact, []string{"EXISTS", any(), any()}} },
		func() gen { return gen{exact, []string{"TYPE", any()}} },
		func() gen { return gen{exact, []string{"RENAME", any(), any()}} },
		func() gen { return gen{exact, []string{"RENAMENX", any(), any()}} },
		func() gen { return gen{exact, []string{"COPY", any(), any()}} },
		func() gen { return gen{exact, []string{"COPY", any(), any(), "REPLACE"}} },
		func() gen { return gen{exact, []string{"DBSIZE"}} },
		func() gen { return gen{setwise, []string{"KEYS", pick(r, "*", "k1*", "h*", "nope*", "?2")}} },
		func() gen { return gen{exact, []string{"PERSIST", any()}} },
		func() gen { return gen{ttlwise, []string{"TTL", any()}} },
		func() gen { return gen{ttlwise, []string{"PTTL", any()}} },
		// Far-future only, so nothing expires while the run is going on,
		// and widely separated, so GT and LT are decided by the magnitude.
		// Repeating one value puts the comparison on the boundary, where
		// the two servers' clocks differ by a millisecond and either answer
		// is correct -- a difference in the test, not in the server.
		func() gen { return gen{exact, []string{"EXPIRE", any(), ttl(r)}} },
		func() gen {
			return gen{exact, []string{"EXPIRE", any(), ttl(r), pick(r, "NX", "XX", "GT", "LT")}}
		},

		// ---- hashes
		func() gen { return gen{exact, []string{"HSET", any(), f(), val(r)}} },
		func() gen { return gen{exact, []string{"HSETNX", h(), f(), val(r)}} },
		func() gen { return gen{exact, []string{"HGET", any(), f()}} },
		func() gen { return gen{exact, []string{"HMGET", any(), f(), f()}} },
		func() gen { return gen{exact, []string{"HDEL", any(), f(), f()}} },
		func() gen { return gen{exact, []string{"HLEN", any()}} },
		func() gen { return gen{exact, []string{"HSTRLEN", any(), f()}} },
		func() gen { return gen{exact, []string{"HEXISTS", any(), f()}} },
		func() gen { return gen{setwise, []string{"HGETALL", any()}} },
		func() gen { return gen{setwise, []string{"HKEYS", any()}} },
		func() gen { return gen{setwise, []string{"HVALS", any()}} },
		func() gen { return gen{exact, []string{"HINCRBY", any(), f(), n(5)}} },
		func() gen { return gen{floatwise, []string{"HINCRBYFLOAT", h(), f(), "1.5"}} },

		// ---- lists
		func() gen { return gen{exact, []string{pick(r, "LPUSH", "RPUSH"), any(), val(r), val(r)}} },
		func() gen { return gen{exact, []string{pick(r, "LPUSHX", "RPUSHX"), any(), val(r)}} },
		func() gen { return gen{exact, []string{pick(r, "LPOP", "RPOP"), any()}} },
		func() gen { return gen{exact, []string{pick(r, "LPOP", "RPOP"), l(), n(4)}} },
		func() gen { return gen{exact, []string{"LRANGE", any(), idx(), idx()}} },
		func() gen { return gen{exact, []string{"LLEN", any()}} },
		func() gen { return gen{exact, []string{"LINDEX", any(), idx()}} },
		func() gen { return gen{exact, []string{"LSET", l(), idx(), val(r)}} },
		func() gen { return gen{exact, []string{"LTRIM", l(), idx(), idx()}} },
		func() gen {
			return gen{exact, []string{"LINSERT", l(), pick(r, "BEFORE", "AFTER"), val(r), val(r)}}
		},
		func() gen { return gen{exact, []string{"LREM", l(), idx(), val(r)}} },
		func() gen { return gen{exact, []string{"LPOS", any(), val(r)}} },
		func() gen {
			return gen{exact, []string{"LMOVE", l(), l(), pick(r, "LEFT", "RIGHT"), pick(r, "LEFT", "RIGHT")}}
		},
		func() gen { return gen{exact, []string{"RPOPLPUSH", l(), l()}} },

		// ---- sets
		func() gen { return gen{exact, []string{"SADD", any(), val(r), val(r)}} },
		func() gen { return gen{exact, []string{"SREM", any(), val(r)}} },
		func() gen { return gen{exact, []string{"SCARD", any()}} },
		func() gen { return gen{exact, []string{"SISMEMBER", any(), val(r)}} },
		func() gen { return gen{exact, []string{"SMISMEMBER", st(), val(r), val(r)}} },
		func() gen { return gen{setwise, []string{"SMEMBERS", any()}} },
		func() gen { return gen{exact, []string{"SMOVE", st(), st(), val(r)}} },
		func() gen { return gen{setwise, []string{pick(r, "SUNION", "SINTER", "SDIFF"), st(), st()}} },
		func() gen {
			return gen{exact, []string{pick(r, "SUNIONSTORE", "SINTERSTORE", "SDIFFSTORE"), st(), st(), st()}}
		},
		func() gen { return gen{exact, []string{"SINTERCARD", "2", st(), st()}} },

		// ---- sorted sets
		func() gen { return gen{exact, []string{"ZADD", any(), score(), val(r)}} },
		func() gen {
			return gen{exact, []string{"ZADD", z(), pick(r, "NX", "XX", "GT", "LT", "CH"), score(), val(r)}}
		},
		func() gen { return gen{exact, []string{"ZINCRBY", z(), score(), val(r)}} },
		func() gen { return gen{exact, []string{"ZCARD", any()}} },
		func() gen { return gen{exact, []string{"ZSCORE", any(), val(r)}} },
		func() gen { return gen{exact, []string{"ZMSCORE", z(), val(r), val(r)}} },
		func() gen { return gen{exact, []string{"ZREM", any(), val(r)}} },
		func() gen { return gen{exact, []string{pick(r, "ZRANK", "ZREVRANK"), any(), val(r)}} },
		func() gen { return gen{exact, []string{"ZRANGE", any(), idx(), idx()}} },
		func() gen { return gen{exact, []string{"ZRANGE", z(), idx(), idx(), "WITHSCORES"}} },
		func() gen { return gen{exact, []string{"ZREVRANGE", z(), idx(), idx()}} },
		func() gen {
			return gen{exact, []string{"ZCOUNT", z(), pick(r, "-inf", "0"), pick(r, "+inf", "10", "(5")}}
		},
		func() gen {
			return gen{exact, []string{"ZRANGEBYSCORE", z(), pick(r, "-inf", "0", "(0"), pick(r, "+inf", "10")}}
		},
		func() gen {
			return gen{exact, []string{"ZREVRANGEBYSCORE", z(), pick(r, "+inf", "10"), pick(r, "-inf", "0")}}
		},
		func() gen { return gen{exact, []string{"ZADD", lex(), "0", val(r)}} },
		func() gen {
			return gen{exact, []string{"ZRANGEBYLEX", lex(), pick(r, "-", "[a", "(a"), pick(r, "+", "[z", "(z")}}
		},
		func() gen { return gen{exact, []string{"ZLEXCOUNT", lex(), pick(r, "-", "[a"), pick(r, "+", "[z")}} },
		func() gen { return gen{exact, []string{"ZREMRANGEBYLEX", lex(), "-", pick(r, "+", "[c")}} },
		func() gen { return gen{exact, []string{pick(r, "ZPOPMIN", "ZPOPMAX"), z()}} },
		func() gen { return gen{exact, []string{"ZREMRANGEBYRANK", z(), idx(), idx()}} },
		func() gen {
			return gen{exact, []string{"ZREMRANGEBYSCORE", z(), pick(r, "-inf", "0"), pick(r, "+inf", "5")}}
		},
		func() gen {
			return gen{exact, []string{pick(r, "ZUNIONSTORE", "ZINTERSTORE"), z(), "2", z(), z()}}
		},
	}
	return cmds[r.Intn(len(cmds))]()
}

// ---------------------------------------------------------------- the test

func TestRepliesMatchTheReference(t *testing.T) {
	if os.Getenv("DIFFTEST") == "" {
		t.Skip("set DIFFTEST=1 to run; needs redis-server on PATH")
	}
	redisBin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skipf("no redis-server on PATH: %v", err)
	}

	dir := t.TempDir()
	kestrelBin := filepath.Join(dir, "kestreld")
	build := exec.Command("go", "build", "-o", kestrelBin, "./cmd/kestreld")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building kestreld: %v\n%s", err, out)
	}

	refPort, ourPort, adminPort := freePort(t), freePort(t), freePort(t)
	start(t, "redis", redisBin,
		"--port", strconv.Itoa(refPort), "--bind", "127.0.0.1",
		"--save", "", "--appendonly", "no", "--logfile", filepath.Join(dir, "redis.log"))
	start(t, "kestrel", kestrelBin,
		"--port", strconv.Itoa(ourPort), "--admin-port", strconv.Itoa(adminPort),
		"--dir", dir, "--appendonly", "no", "--loglevel", "error")

	ref := connect(t, "redis", refPort)
	ours := connect(t, "kestrel", ourPort)

	seed := time.Now().UnixNano()
	if s := os.Getenv("DIFFTEST_SEED"); s != "" {
		seed, _ = strconv.ParseInt(s, 10, 64)
	}
	t.Logf("seed %d (set DIFFTEST_SEED to reproduce)", seed)
	r := rand.New(rand.NewSource(seed))

	rounds := 20000
	if s := os.Getenv("DIFFTEST_ROUNDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			rounds = n
		}
	}

	type divergence struct {
		cmd       string
		got, want string
	}
	var found []divergence
	seen := map[string]bool{}

	for i := 0; i < rounds; i++ {
		g := generate(r)
		line := strings.Join(g.args, " ")

		want, err1 := ref.do(g.args...)
		got, err2 := ours.do(g.args...)
		if err1 != nil || err2 != nil {
			t.Fatalf("round %d: transport failure on %q: redis=%v kestrel=%v",
				i, line, err1, err2)
		}
		if g.mode == skipReply {
			continue
		}
		a, b := normalise(want), normalise(got)
		switch g.mode {
		case setwise:
			a, b = unordered(a), unordered(b)
		case floatwise:
			if sameNumber(a, b) {
				continue
			}
		case ttlwise:
			a, b = ttlClass(a), ttlClass(b)
		}
		if a == b {
			continue
		}
		// Report each distinct shape once; a divergence that recurs
		// thousands of times is one bug, not thousands.
		shape := g.args[0] + "|" + a + "|" + b
		if seen[shape] {
			continue
		}
		seen[shape] = true
		found = append(found, divergence{line, b, a})
		if len(found) > 40 {
			break
		}
	}

	// The two keyspaces must also have ended up the same.
	refKeys, _ := ref.do("KEYS", "*")
	ourKeys, _ := ours.do("KEYS", "*")
	if a, b := unordered(normalise(refKeys)), unordered(normalise(ourKeys)); a != b {
		t.Errorf("the keyspaces diverged after %d commands\n  redis:   %.400s\n  kestrel: %.400s",
			rounds, a, b)
	}

	for _, d := range found {
		t.Errorf("%s\n  redis:   %s\n  kestrel: %s", d.cmd, d.want, d.got)
	}
	if len(found) == 0 {
		t.Logf("%d commands, no divergence", rounds)
	}
}
