package command

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kestrel/config"
	"kestrel/engine"
	"kestrel/resp"
)

// testHost is a command.Host backed by a real engine and configuration, with
// the effect stream captured rather than written to disk.
type testHost struct {
	cfg   *config.Config
	ks    *engine.Keyspace
	table *Table
	stats *Stats
	start time.Time
	now   atomic.Int64

	persistErr  error
	snapshotErr error
	followErr   error

	followHost string // guarded by mu
	followPort int    // guarded by mu

	snapshots           atomic.Int64
	backgroundSnapshots atomic.Int64
	lastSave            atomic.Int64

	mu      sync.Mutex
	effects []effect
	// sink, when set, receives effects instead of recording them. The
	// convergence test uses it to feed a follower directly.
	sink func(db int, args [][]byte)
}

type effect struct {
	db   int
	args []string
}

func (e effect) String() string { return strings.Join(e.args, " ") }

// kindMap and kindNull name reply kinds in assertions without importing the
// protocol constants into every test file.
func kindMap() resp.Kind  { return resp.KindMap }
func kindNull() resp.Kind { return resp.KindNull }
func kindSet() resp.Kind  { return resp.KindSet }

// farFuture is a deadline the expiry pass will never hit, so a test pass
// runs to completion.
func farFuture() time.Time { return time.Now().Add(time.Hour) }

func newTestHost(t *testing.T, tweaks ...func(*config.Config)) *testHost {
	t.Helper()
	cfg := config.Default()
	if err := cfg.Set("slowlog-log-slower-than", "1000000000"); err != nil {
		t.Fatal(err)
	}
	for _, tweak := range tweaks {
		tweak(cfg)
	}
	snap := cfg.Snapshot()

	table, err := NewTable(snap.RenamedCommands)
	if err != nil {
		t.Fatal(err)
	}
	ks := engine.New(engine.Options{
		Databases: snap.Databases, Shards: 4,
		MaxStringLength: int(snap.ProtoMaxBulkLen),
		ActiveExpire:    false,
	})
	t.Cleanup(ks.Close)

	h := &testHost{cfg: cfg, ks: ks, table: table, stats: NewStats(128), start: time.Now()}
	h.ApplyRuntimeConfig()
	h.now.Store(1_700_000_000_000)
	ks.SetClock(func() int64 { return h.now.Load() })
	ks.SetEffectSink(hostSink{h})
	return h
}

type hostSink struct{ h *testHost }

func (s hostSink) Effect(db int, args ...[]byte) { s.h.Propagate(db, args...) }

func (h *testHost) Keyspace() *engine.Keyspace { return h.ks }
func (h *testHost) Config() *config.Config     { return h.cfg }
func (h *testHost) Commands() *Table           { return h.table }
func (h *testHost) Stats() *Stats              { return h.stats }
func (h *testHost) Shutdown(bool) error        { return nil }
func (h *testHost) IsReplica() bool            { return false }
func (h *testHost) IsLoading() bool            { return false }

// persistErr, when set, makes the host refuse writes the way a failed append
// log does.
func (h *testHost) PersistenceError() error { return h.persistErr }

// Snapshot records the request rather than writing anything: the command
// tests are about what the commands do, and the snapshotter has its own.
func (h *testHost) Snapshot(background bool) error {
	if h.snapshotErr != nil {
		return h.snapshotErr
	}
	h.snapshots.Add(1)
	if background {
		h.backgroundSnapshots.Add(1)
	}
	h.lastSave.Store(h.now.Load() / 1000)
	return nil
}

func (h *testHost) LastSave() int64 { return h.lastSave.Load() }

// Follow records what REPLICAOF asked for. The replication machinery has its
// own tests against a real socket; these are about the command.
func (h *testHost) Follow(host string, port int) error {
	if h.followErr != nil {
		return h.followErr
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.followHost, h.followPort = host, port
	return nil
}
func (h *testHost) StartTime() time.Time { return h.start }

func (h *testHost) ApplyRuntimeConfig() {
	snap := h.cfg.Snapshot()
	h.stats.Slowlog.SetCapacity(snap.SlowlogMaxLen)
	h.ks.SetThresholds(engine.Thresholds{
		HashMaxListpackEntries: snap.HashMaxListpackEntries,
		HashMaxListpackValue:   snap.HashMaxListpackValue,
		ListMaxListpackSize:    snap.ListMaxListpackSize,
		ListMaxListpackValue:   snap.ListMaxListpackValue,
		SetMaxIntsetEntries:    snap.SetMaxIntsetEntries,
		SetMaxListpackEntries:  snap.SetMaxListpackEntries,
		SetMaxListpackValue:    snap.SetMaxListpackValue,
		ZSetMaxListpackEntries: snap.ZsetMaxListpackEntries,
		ZSetMaxListpackValue:   snap.ZsetMaxListpackValue,
	})
}
func (h *testHost) Info(sections []string) string {
	return "# Server\r\nkestrel_version:" + Version + "\r\n"
}

func (h *testHost) Propagate(db int, args ...[]byte) {
	h.mu.Lock()
	sink := h.sink
	h.mu.Unlock()
	if sink != nil {
		cp := make([][]byte, len(args))
		for i, a := range args {
			cp[i] = append([]byte(nil), a...)
		}
		sink(db, cp)
		return
	}
	rendered := make([]string, len(args))
	for i, a := range args {
		rendered[i] = string(a)
	}
	h.mu.Lock()
	h.effects = append(h.effects, effect{db: db, args: rendered})
	h.mu.Unlock()
}

func (h *testHost) takeEffects() []effect {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.effects
	h.effects = nil
	return out
}

func (h *testHost) advance(ms int64) { h.now.Add(ms) }

// session drives commands against a host as one client would.
type session struct {
	t    *testing.T
	h    *testHost
	cl   *Client
	out  *bytes.Buffer
	wr   *resp.Writer
	rr   *resp.ReplyReader
	read int
}

func newSession(t *testing.T, h *testHost) *session {
	t.Helper()
	buf := &bytes.Buffer{}
	w := resp.NewWriter(buf)
	snap := h.cfg.Snapshot()
	cl := NewClient(1, "127.0.0.1:1234", "127.0.0.1:6380", w, h.ks.DB(0), snap.RequirePass == "")
	return &session{t: t, h: h, cl: cl, out: buf, wr: w, rr: resp.NewReplyReader(buf)}
}

// do runs a command and returns the decoded reply.
func (s *session) do(args ...string) resp.Value {
	s.t.Helper()
	raw := make([][]byte, len(args))
	for i, a := range args {
		raw[i] = []byte(a)
	}
	Execute(s.h, s.cl, raw)
	if err := s.wr.Flush(); err != nil {
		s.t.Fatal(err)
	}
	v, err := s.rr.ReadReply()
	if err != nil {
		s.t.Fatalf("%v: decoding reply: %v", args, err)
	}
	return v
}

// str renders a reply the way a test assertion wants to read it.
func str(v resp.Value) string {
	switch v.Kind {
	case resp.KindSimple, resp.KindError, resp.KindBulk, resp.KindVerbatim:
		return string(v.Str)
	case resp.KindInt:
		return itoa(int(v.Int))
	case resp.KindNull, resp.KindNullArray:
		return "<nil>"
	case resp.KindBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case resp.KindArray, resp.KindSet, resp.KindPush, resp.KindMap:
		parts := make([]string, len(v.Elems))
		for i, e := range v.Elems {
			parts[i] = str(e)
		}
		return "[" + strings.Join(parts, " ") + "]"
	default:
		return "<none>"
	}
}

func (s *session) expect(want string, args ...string) {
	s.t.Helper()
	if got := str(s.do(args...)); got != want {
		s.t.Errorf("%s = %q, want %q", strings.Join(args, " "), got, want)
	}
}

func (s *session) expectErrPrefix(prefix string, args ...string) {
	s.t.Helper()
	v := s.do(args...)
	if !v.IsError() || !strings.HasPrefix(string(v.Str), prefix) {
		s.t.Errorf("%s = %q, want an error starting with %q",
			strings.Join(args, " "), str(v), prefix)
	}
}
