package command

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"kestrel/config"
	"kestrel/resp"
)

func TestEveryWriteCommandDeclaresItsEffect(t *testing.T) {
	// Registration panics on a missing declaration, so reaching this point
	// already proves the invariant. The test pins it against a refactor that
	// weakens the check, and reports the classification for review.
	var verbatim, canonical []string
	for name, d := range builtins {
		if !d.Is(Write) {
			if d.Effect != EffectNone {
				t.Errorf("%s declares an effect but is not a write command", name)
			}
			continue
		}
		switch d.Effect {
		case EffectVerbatim:
			verbatim = append(verbatim, name)
		case EffectCanonical:
			canonical = append(canonical, name)
		default:
			t.Errorf("%s is a write command with no effect declaration", name)
		}
	}
	sort.Strings(verbatim)
	sort.Strings(canonical)
	if len(verbatim)+len(canonical) == 0 {
		t.Fatal("no write commands registered")
	}
	t.Logf("verbatim: %s", strings.Join(verbatim, " "))
	t.Logf("canonical: %s", strings.Join(canonical, " "))
}

func TestSubcommandsValidate(t *testing.T) {
	for name, d := range builtins {
		for subName, sub := range d.Subcommands {
			if sub.Handler == nil {
				t.Errorf("%s %s has no handler", name, subName)
			}
			if sub.Arity == 0 {
				t.Errorf("%s %s has no arity", name, subName)
			}
		}
	}
}

func TestConnectionCommands(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expect("PONG", "PING")
	s.expect("hello", "PING", "hello")
	s.expect("hi", "ECHO", "hi")
	s.expect("OK", "SELECT", "5")
	if s.cl.DBIndex != 5 {
		t.Fatalf("SELECT did not move the client: %d", s.cl.DBIndex)
	}
	s.expectErrPrefix("ERR DB index", "SELECT", "999")
	s.expect("1", "CLIENT", "ID")
	s.expect("OK", "CLIENT", "SETNAME", "worker-1")
	s.expect("worker-1", "CLIENT", "GETNAME")
	s.expectErrPrefix("ERR Client names", "CLIENT", "SETNAME", "has space")
	s.expect("RESET", "RESET")
	if s.cl.DBIndex != 0 || s.cl.Name != "" {
		t.Fatal("RESET did not restore the default state")
	}
}

func TestHelloNegotiatesProtocol(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	v := s.do("HELLO", "3")
	if v.Kind != resp.KindMap {
		t.Fatalf("HELLO 3 replied with kind %d, want a map", v.Kind)
	}
	if s.cl.Protocol() != resp.RESP3 {
		t.Fatalf("protocol is %d after HELLO 3", s.cl.Protocol())
	}
	// Under RESP3 a missing key is the null type, not a null bulk string.
	if got := s.do("GET", "missing"); got.Kind != resp.KindNull {
		t.Fatalf("RESP3 GET of a missing key replied with kind %d", got.Kind)
	}
	s.expectErrPrefix("NOPROTO", "HELLO", "4")
	s.expectErrPrefix("NOPROTO", "HELLO", "notanumber")

	// HELLO with no argument reports the current protocol without changing it.
	v = s.do("HELLO")
	if v.Kind != resp.KindMap || s.cl.Protocol() != resp.RESP3 {
		t.Fatal("bare HELLO changed the protocol")
	}
}

func TestAuth(t *testing.T) {
	h := newTestHost(t, func(c *config.Config) {
		if err := c.Set("requirepass", "hunter2"); err != nil {
			t.Fatal(err)
		}
	})
	s := newSession(t, h)

	s.expectErrPrefix("NOAUTH", "GET", "k")
	s.expect("PONG", "PING") // NoAuth commands still work
	s.expectErrPrefix("WRONGPASS", "AUTH", "wrong")
	s.expect("OK", "AUTH", "hunter2")
	s.expect("<nil>", "GET", "k")

	// A server with no password says so rather than silently accepting.
	h2 := newTestHost(t)
	s2 := newSession(t, h2)
	s2.expectErrPrefix("ERR Client sent AUTH", "AUTH", "anything")
}

func TestArityAndUnknownCommands(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expectErrPrefix("ERR wrong number of arguments for 'get'", "GET")
	s.expectErrPrefix("ERR wrong number of arguments for 'get'", "GET", "a", "b")
	s.expectErrPrefix("ERR unknown command 'NOPE'", "NOPE", "a")
	s.expectErrPrefix("ERR Unknown subcommand", "CONFIG", "NONSENSE")
	s.expectErrPrefix("ERR wrong number of arguments", "CONFIG")
	// Case insensitivity.
	s.expect("PONG", "ping")
	s.expect("PONG", "PiNg")
}

func TestRenamedAndDisabledCommands(t *testing.T) {
	h := newTestHost(t, func(c *config.Config) {
		c.LoadFlags([]string{"--rename-command", "FLUSHALL \"\"", "--rename-command", "CONFIG admin_config"})
	})
	s := newSession(t, h)

	s.expectErrPrefix("ERR unknown command 'FLUSHALL': this command has been disabled",
		"FLUSHALL")
	s.expectErrPrefix("ERR unknown command 'CONFIG'", "CONFIG", "GET", "port")
	v := s.do("admin_config", "GET", "port")
	if v.IsError() {
		t.Fatalf("renamed command failed: %s", str(v))
	}
}

func TestStringCommandMatrix(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expect("OK", "SET", "k", "v")
	s.expect("v", "GET", "k")
	s.expect("<nil>", "SET", "k", "v2", "NX")
	s.expect("v", "GET", "k")
	s.expect("OK", "SET", "k", "v2", "XX")
	s.expect("<nil>", "SET", "absent", "v", "XX")
	s.expect("v2", "SET", "k", "v3", "GET")
	s.expect("<nil>", "SET", "fresh", "v", "GET")
	s.expectErrPrefix("ERR syntax error", "SET", "k", "v", "NX", "XX")
	s.expectErrPrefix("ERR syntax error", "SET", "k", "v", "BOGUS")
	s.expectErrPrefix("ERR syntax error", "SET", "k", "v", "EX", "10", "PX", "10")
	s.expectErrPrefix("ERR syntax error", "SET", "k", "v", "KEEPTTL", "EX", "10")
	s.expectErrPrefix("ERR invalid expire time", "SET", "k", "v", "EX", "0")
	s.expectErrPrefix("ERR invalid expire time", "SET", "k", "v", "EX", "-5")
	s.expectErrPrefix("ERR value is not an integer", "SET", "k", "v", "EX", "abc")

	s.expect("1", "SETNX", "n", "1")
	s.expect("0", "SETNX", "n", "2")
	s.expect("OK", "SETEX", "e", "100", "v")
	s.expect("100", "TTL", "e")
	s.expect("OK", "PSETEX", "pe", "100000", "v")
	s.expect("100", "TTL", "pe")
	s.expectErrPrefix("ERR invalid expire time", "SETEX", "bad", "0", "v")

	s.expect("5", "APPEND", "a", "Hello")
	s.expect("11", "APPEND", "a", " World")
	s.expect("Hello World", "GET", "a")
	s.expect("11", "STRLEN", "a")
	s.expect("0", "STRLEN", "no-such-key")
	s.expect("Hello", "GETRANGE", "a", "0", "4")
	s.expect("World", "GETRANGE", "a", "-5", "-1")
	s.expect("", "GETRANGE", "a", "100", "200")
	s.expect("11", "SETRANGE", "a", "6", "Redis")
	s.expect("Hello Redis", "GET", "a")

	s.expect("1", "INCR", "c")
	s.expect("42", "INCRBY", "c", "41")
	s.expect("41", "DECR", "c")
	s.expect("1", "DECRBY", "c", "40")
	s.expectErrPrefix("ERR value is not an integer", "INCR", "a")
	s.expect("OK", "SET", "max", "9223372036854775807")
	s.expectErrPrefix("ERR increment or decrement would overflow", "INCR", "max")

	s.expect("10.5", "INCRBYFLOAT", "f", "10.5")
	s.expect("10.6", "INCRBYFLOAT", "f", "0.1")
	s.expectErrPrefix("ERR value is not a valid float", "INCRBYFLOAT", "a", "1")

	s.expect("OK", "MSET", "x", "1", "y", "2")
	s.expect("[1 2 <nil>]", "MGET", "x", "y", "zz")
	s.expectErrPrefix("ERR wrong number of arguments", "MSET", "x", "1", "y")
	s.expect("0", "MSETNX", "x", "9", "new", "1")
	s.expect("<nil>", "GET", "new")
	s.expect("1", "MSETNX", "new", "1")

	s.expect("1", "GETDEL", "new")
	s.expect("0", "EXISTS", "new")
	s.expect("1", "GETEX", "n", "EX", "100")
	s.expect("100", "TTL", "n")
	s.expect("1", "GETEX", "n", "PERSIST")
	s.expect("-1", "TTL", "n")
	s.expectErrPrefix("ERR syntax error", "GETEX", "n", "PERSIST", "EX", "10")
}

func TestKeyspaceCommands(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	s.expect("OK", "MSET", "a", "1", "b", "2", "c", "3")
	s.expect("3", "DBSIZE")
	s.expect("2", "EXISTS", "a", "b", "nope")
	s.expect("string", "TYPE", "a")
	s.expect("none", "TYPE", "nope")
	s.expect("2", "DEL", "a", "b", "nope")
	s.expect("1", "DBSIZE")
	s.expect("1", "UNLINK", "c")

	s.expect("OK", "SET", "src", "value")
	s.expect("OK", "RENAME", "src", "dst")
	s.expect("value", "GET", "dst")
	s.expectErrPrefix("ERR no such key", "RENAME", "gone", "x")
	s.expect("OK", "SET", "other", "o")
	s.expect("0", "RENAMENX", "dst", "other")
	s.expect("1", "RENAMENX", "dst", "renamed")

	s.expect("1", "COPY", "renamed", "copy")
	s.expect("0", "COPY", "renamed", "copy")
	s.expect("1", "COPY", "renamed", "copy", "REPLACE")
	s.expectErrPrefix("ERR source and destination", "COPY", "renamed", "renamed")

	s.expect("value", "GET", "copy")
	s.expect("2", "TOUCH", "renamed", "copy")

	keys := s.do("KEYS", "*")
	if len(keys.Elems) != 3 {
		t.Fatalf("KEYS returned %s", str(keys))
	}
	if got := s.do("KEYS", "cop*"); len(got.Elems) != 1 {
		t.Fatalf("KEYS pattern returned %s", str(got))
	}

	rk := s.do("RANDOMKEY")
	if rk.Kind == resp.KindNull {
		t.Fatal("RANDOMKEY on a non-empty database")
	}
}

func TestScanCommand(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for i := 0; i < 200; i++ {
		s.do("SET", fmt.Sprintf("key:%d", i), "v")
	}
	seen := map[string]bool{}
	cursor := "0"
	for i := 0; i < 100; i++ {
		v := s.do("SCAN", cursor, "COUNT", "30")
		if len(v.Elems) != 2 {
			t.Fatalf("SCAN reply shape: %s", str(v))
		}
		cursor = string(v.Elems[0].Str)
		for _, k := range v.Elems[1].Elems {
			if seen[string(k.Str)] {
				t.Fatalf("SCAN returned %s twice", k.Str)
			}
			seen[string(k.Str)] = true
		}
		if cursor == "0" {
			break
		}
	}
	if len(seen) != 200 {
		t.Fatalf("SCAN covered %d of 200 keys", len(seen))
	}
	s.expectErrPrefix("ERR invalid cursor", "SCAN", "notanumber")
	s.expectErrPrefix("ERR syntax error", "SCAN", "0", "BOGUS")

	// A TYPE filter that matches nothing yields an empty, terminated scan.
	v := s.do("SCAN", "0", "TYPE", "list")
	if str(v) != "[0 []]" {
		t.Fatalf("SCAN TYPE list = %s", str(v))
	}
}

func TestExpireCommands(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	now := h.ks.Now()

	s.expect("OK", "SET", "k", "v")
	s.expect("-1", "TTL", "k")
	s.expect("-2", "TTL", "gone")
	s.expect("1", "EXPIRE", "k", "100")
	s.expect("100", "TTL", "k")
	s.expect(itoa(int(now+100_000)), "PEXPIRETIME", "k")
	s.expect(itoa(int((now+100_000)/1000)), "EXPIRETIME", "k")
	s.expect("0", "EXPIRE", "k", "200", "NX")
	s.expect("1", "EXPIRE", "k", "200", "XX")
	s.expect("0", "EXPIRE", "k", "100", "GT")
	s.expect("1", "EXPIRE", "k", "100", "LT")
	s.expectErrPrefix("ERR Unsupported option", "EXPIRE", "k", "100", "ZZ")
	s.expect("1", "PERSIST", "k")
	s.expect("0", "PERSIST", "k")

	// An expiry in the past deletes the key.
	s.expect("1", "EXPIREAT", "k", "1")
	s.expect("0", "EXISTS", "k")

	// A key whose TTL passes disappears from reads and from DBSIZE.
	s.expect("OK", "SET", "t", "v", "PX", "50")
	h.advance(51)
	s.expect("<nil>", "GET", "t")
	s.expect("0", "DBSIZE")
}

func TestServerCommands(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	if v := s.do("INFO"); !strings.Contains(string(v.Str), "kestrel_version") {
		t.Fatalf("INFO = %q", v.Str)
	}
	if v := s.do("COMMAND", "COUNT"); v.Int < 40 {
		t.Fatalf("COMMAND COUNT = %d", v.Int)
	}
	if v := s.do("COMMAND", "INFO", "get"); len(v.Elems) != 1 || len(v.Elems[0].Elems) != 10 {
		t.Fatalf("COMMAND INFO shape: %s", str(v))
	}
	if v := s.do("COMMAND", "GETKEYS", "MSET", "a", "1", "b", "2"); str(v) != "[a b]" {
		t.Fatalf("COMMAND GETKEYS = %s", str(v))
	}
	if v := s.do("COMMAND", "GETKEYS", "PING"); !v.IsError() {
		t.Fatalf("COMMAND GETKEYS PING = %s", str(v))
	}

	if v := s.do("CONFIG", "GET", "maxmemory"); str(v) != "[maxmemory 0]" {
		t.Fatalf("CONFIG GET = %s", str(v))
	}
	s.expect("OK", "CONFIG", "SET", "maxmemory", "100mb")
	if v := s.do("CONFIG", "GET", "maxmemory"); str(v) != "[maxmemory 100000000]" {
		t.Fatalf("CONFIG GET after SET = %s", str(v))
	}
	s.expectErrPrefix("ERR CONFIG SET failed", "CONFIG", "SET", "shards", "32")
	s.expectErrPrefix("ERR CONFIG SET failed", "CONFIG", "SET", "appendfsync", "sometimes")
	// A batch with a bad member applies none of it.
	s.expectErrPrefix("ERR CONFIG SET failed", "CONFIG", "SET", "maxmemory", "1mb", "appendfsync", "bogus")
	if v := s.do("CONFIG", "GET", "maxmemory"); str(v) != "[maxmemory 100000000]" {
		t.Fatalf("a rejected batch was partially applied: %s", str(v))
	}
	s.expect("OK", "CONFIG", "RESETSTAT")

	if v := s.do("TIME"); len(v.Elems) != 2 {
		t.Fatalf("TIME = %s", str(v))
	}
	s.expect("OK", "SET", "k", "v")
	if v := s.do("MEMORY", "USAGE", "k"); v.Int <= 0 {
		t.Fatalf("MEMORY USAGE = %s", str(v))
	}
	s.expect("<nil>", "MEMORY", "USAGE", "nope")
	s.expect("raw", "OBJECT", "ENCODING", "k")
	s.expect("OK", "SET", "n", "42")
	s.expect("int", "OBJECT", "ENCODING", "n")
	s.expectErrPrefix("ERR no such key", "OBJECT", "ENCODING", "nope")
	s.expect("OK", "FLUSHDB")
	s.expect("0", "DBSIZE")
	s.expectErrPrefix("ERR syntax error", "FLUSHDB", "MAYBE")
}

func TestUnsupportedCommandsAreLegible(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	for _, cmd := range [][]string{
		{"EVAL", "return 1", "0"},
		{"XADD", "s", "*", "f", "v"},
		{"PFADD", "hll", "a"},
		{"DUMP", "k"},
	} {
		v := s.do(cmd...)
		if !v.IsError() || !strings.Contains(string(v.Str), "not supported in this version") {
			t.Errorf("%v = %q, want an explicit unsupported error", cmd, str(v))
		}
	}
	if got := h.stats.UnsupportedCommands.Load(); got != 4 {
		t.Fatalf("unsupported attempts counted %d, want 4", got)
	}
	if v := s.do("CLUSTER", "INFO"); !strings.Contains(string(v.Str), "cluster_enabled:0") {
		t.Fatalf("CLUSTER INFO = %q", str(v))
	}
}

func TestSlowlogRecordsSlowCommands(t *testing.T) {
	h := newTestHost(t, func(c *config.Config) { c.Set("slowlog-log-slower-than", "0") })
	s := newSession(t, h)
	s.do("SET", "k", "v")
	if n := h.stats.Slowlog.Len(); n == 0 {
		t.Fatal("nothing recorded with a zero threshold")
	}
	if v := s.do("SLOWLOG", "LEN"); v.Int == 0 {
		t.Fatal("SLOWLOG LEN reported nothing")
	}
	entries := s.do("SLOWLOG", "GET", "10")
	if len(entries.Elems) == 0 || len(entries.Elems[0].Elems) != 6 {
		t.Fatalf("SLOWLOG GET shape: %s", str(entries))
	}
	s.expect("OK", "SLOWLOG", "RESET")
	// RESET itself is slow enough to be logged at this threshold, so the log
	// holds exactly that one entry afterwards.
	after := s.do("SLOWLOG", "GET", "10")
	for _, e := range after.Elems {
		if name := string(e.Elems[3].Elems[0].Str); !strings.EqualFold(name, "SLOWLOG") {
			t.Fatalf("SLOWLOG RESET left a %s entry behind", name)
		}
	}
}

func TestOOMRefusesWritesUnderNoeviction(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.expect("OK", "SET", "k", strings.Repeat("v", 1024))
	// Set the ceiling below what is already stored.
	s.expect("OK", "CONFIG", "SET", "maxmemory", "1")
	s.expectErrPrefix("OOM", "SET", "k2", "v")
	// Reads keep working (FR-6.3).
	if v := s.do("GET", "k"); v.IsError() {
		t.Fatalf("a read was refused under OOM: %s", str(v))
	}
	// DEL is a write but not DenyOOM, so it must still work: refusing it
	// would leave no way out of the condition.
	s.expect("1", "DEL", "k")
	s.expect("OK", "CONFIG", "SET", "maxmemory", "0")
}

func TestCommandStatsAreRecorded(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for i := 0; i < 5; i++ {
		s.do("PING")
	}
	s.do("GET") // arity failure: rejected, not called
	stats := h.table.CommandStats()
	if stats["PING"].Calls != 5 {
		t.Fatalf("PING calls = %d", stats["PING"].Calls)
	}
	if stats["GET"].Rejected != 1 {
		t.Fatalf("GET rejected = %d", stats["GET"].Rejected)
	}
}

func TestKeysCanBeBinary(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	key := "bin\x00ary\r\nkey"
	s.expect("OK", "SET", key, "v")
	s.expect("v", "GET", key)
	s.expect("1", "EXISTS", key)
	if got := s.do("KEYS", "*"); len(got.Elems) != 1 || string(got.Elems[0].Str) != key {
		t.Fatalf("KEYS = %q", str(got))
	}
}

func TestRandomizedTraffic(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 2000; i++ {
		k := fmt.Sprintf("k%d", rng.Intn(40))
		switch rng.Intn(10) {
		case 0:
			s.do("SET", k, fmt.Sprintf("v%d", i))
		case 1:
			s.do("GET", k)
		case 2:
			s.do("INCR", k)
		case 3:
			s.do("APPEND", k, "x")
		case 4:
			s.do("EXPIRE", k, "100")
		case 5:
			s.do("DEL", k)
		case 6:
			s.do("SETRANGE", k, "2", "zz")
		case 7:
			s.do("INCRBYFLOAT", k, "1.5")
		case 8:
			s.do("GETEX", k, "PERSIST")
		case 9:
			s.do("COPY", k, k+"-copy", "REPLACE")
		}
	}
}
