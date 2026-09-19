package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"kestrel/config"
	"kestrel/resp"
)

// freePort returns a port that is free right now. There is an inherent race
// between releasing it and the server binding it, which is acceptable in a
// test and avoids hard-coding ports that collide across parallel runs.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type testServer struct {
	*Server
	port      int
	adminPort int
	done      chan error
	waitOnce  sync.Once
	waitErr   error
}

// wait blocks until Serve returns, at most once, so both a test and the
// cleanup hook can call it.
func (ts *testServer) wait(t *testing.T) error {
	t.Helper()
	ts.waitOnce.Do(func() {
		select {
		case ts.waitErr = <-ts.done:
		case <-time.After(10 * time.Second):
			ts.waitErr = fmt.Errorf("server did not shut down within 10s")
		}
	})
	return ts.waitErr
}

func startServer(t *testing.T, tweaks ...func(*config.Config)) *testServer {
	t.Helper()
	cfg := config.Default()
	port, adminPort := freePort(t), freePort(t)
	must(t, cfg.Set("timeout", "0"))
	if err := cfg.LoadFlags([]string{
		"--port", fmt.Sprint(port),
		"--admin-port", fmt.Sprint(adminPort),
		"--loglevel", "error",
		"--appendonly", "no",
	}); err != nil {
		t.Fatal(err)
	}
	for _, tweak := range tweaks {
		tweak(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{Server: srv, port: port, adminPort: adminPort, done: make(chan error, 1)}
	go func() { ts.done <- srv.Serve(context.Background()) }()

	// Wait until the server is actually serving, not merely bound.
	//
	// The listener is created before startup recovery runs, so a bare dial
	// succeeds into the kernel backlog while the server is still loading. A
	// PING that comes back is the only thing that proves the accept loop is
	// running, and everything set up before it -- the keyspace, the log --
	// is ordered before that by the goroutine that answers.
	waitReady(t, ts.addr())
	t.Cleanup(func() {
		srv.Shutdown(false)
		if err := ts.wait(t); err != nil {
			t.Error(err)
		}
	})
	return ts
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (ts *testServer) addr() string      { return fmt.Sprintf("127.0.0.1:%d", ts.port) }
func (ts *testServer) adminAddr() string { return fmt.Sprintf("127.0.0.1:%d", ts.adminPort) }

// conn is a raw protocol client, so the tests exercise the wire format and
// not just the command layer.
type conn struct {
	t  *testing.T
	nc net.Conn
	rr *resp.ReplyReader
	bw *bufio.Writer
	// wait is how long a reply may take. Blocking commands are expected to
	// exceed the default, so their tests raise it.
	wait time.Duration
}

func (ts *testServer) connect(t *testing.T) *conn {
	t.Helper()
	nc, err := net.Dial("tcp", ts.addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return &conn{t: t, nc: nc, rr: resp.NewReplyReader(nc), bw: bufio.NewWriter(nc),
		wait: 5 * time.Second}
}

func (c *conn) send(args ...string) {
	c.t.Helper()
	raw := make([][]byte, len(args))
	for i, a := range args {
		raw[i] = []byte(a)
	}
	c.bw.Write(resp.EncodeCommand(nil, raw...))
	must(c.t, c.bw.Flush())
}

func (c *conn) sendRaw(s string) {
	c.t.Helper()
	c.bw.WriteString(s)
	must(c.t, c.bw.Flush())
}

func (c *conn) reply() resp.Value {
	c.t.Helper()
	c.nc.SetReadDeadline(time.Now().Add(c.wait))
	v, err := c.rr.ReadReply()
	if err != nil {
		c.t.Fatalf("reading reply: %v", err)
	}
	return v
}

func (c *conn) do(args ...string) resp.Value {
	c.send(args...)
	return c.reply()
}

func text(v resp.Value) string {
	switch v.Kind {
	case resp.KindInt:
		return fmt.Sprint(v.Int)
	case resp.KindNull, resp.KindNullArray:
		return "<nil>"
	default:
		return string(v.Str)
	}
}

func TestServerPingAndBasicCommands(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)

	if got := text(c.do("PING")); got != "PONG" {
		t.Fatalf("PING = %q", got)
	}
	if got := text(c.do("SET", "k", "v")); got != "OK" {
		t.Fatalf("SET = %q", got)
	}
	if got := text(c.do("GET", "k")); got != "v" {
		t.Fatalf("GET = %q", got)
	}
	if got := text(c.do("GET", "missing")); got != "<nil>" {
		t.Fatalf("GET missing = %q", got)
	}
}

func TestServerPipelining(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)

	// Three commands in one write must produce three replies, and the
	// server must not need a second read to answer the first.
	c.bw.Write(resp.EncodeCommand(nil, []byte("SET"), []byte("a"), []byte("1")))
	c.bw.Write(resp.EncodeCommand(nil, []byte("INCR"), []byte("a")))
	c.bw.Write(resp.EncodeCommand(nil, []byte("GET"), []byte("a")))
	must(t, c.bw.Flush())

	if got := text(c.reply()); got != "OK" {
		t.Fatalf("reply 1 = %q", got)
	}
	if got := text(c.reply()); got != "2" {
		t.Fatalf("reply 2 = %q", got)
	}
	if got := text(c.reply()); got != "2" {
		t.Fatalf("reply 3 = %q", got)
	}
}

func TestServerDeepPipeline(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	const n = 5000
	for i := 0; i < n; i++ {
		c.bw.Write(resp.EncodeCommand(nil, []byte("INCR"), []byte("counter")))
	}
	must(t, c.bw.Flush())
	for i := 1; i <= n; i++ {
		v := c.reply()
		if v.Int != int64(i) {
			t.Fatalf("reply %d = %d", i, v.Int)
		}
	}
}

func TestServerInlineCommands(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)

	c.sendRaw("PING\r\n")
	if got := text(c.reply()); got != "PONG" {
		t.Fatalf("inline PING = %q", got)
	}
	c.sendRaw("SET inline \"hello world\"\r\n")
	if got := text(c.reply()); got != "OK" {
		t.Fatalf("inline SET = %q", got)
	}
	if got := text(c.do("GET", "inline")); got != "hello world" {
		t.Fatalf("GET = %q", got)
	}
	// A bare newline is ignored rather than answered.
	c.sendRaw("\r\nPING\r\n")
	if got := text(c.reply()); got != "PONG" {
		t.Fatalf("after a blank line: %q", got)
	}
}

func TestServerProtocolErrorClosesConnection(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)

	c.sendRaw("*1\r\n+PING\r\n")
	v := c.reply()
	if !v.IsError() || !strings.Contains(string(v.Str), "Protocol error") {
		t.Fatalf("expected a protocol error, got %q", text(v))
	}
	// The connection must be closed after a framing error: there is no way
	// to resynchronize the stream.
	c.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.nc.Read(buf); err != io.EOF {
		t.Fatalf("connection stayed open after a protocol error: %v", err)
	}
}

func TestServerQuit(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	if got := text(c.do("QUIT")); got != "OK" {
		t.Fatalf("QUIT = %q", got)
	}
	c.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.nc.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection stayed open after QUIT: %v", err)
	}
}

func TestServerAuth(t *testing.T) {
	ts := startServer(t, func(c *config.Config) { c.Set("requirepass", "s3cret") })
	c := ts.connect(t)

	v := c.do("GET", "k")
	if !v.IsError() || !strings.HasPrefix(string(v.Str), "NOAUTH") {
		t.Fatalf("unauthenticated GET = %q", text(v))
	}
	if v := c.do("AUTH", "wrong"); !v.IsError() {
		t.Fatal("wrong password accepted")
	}
	if got := text(c.do("AUTH", "s3cret")); got != "OK" {
		t.Fatalf("AUTH = %q", got)
	}
	if got := text(c.do("SET", "k", "v")); got != "OK" {
		t.Fatalf("post-auth SET = %q", got)
	}
}

func TestServerMaxClients(t *testing.T) {
	ts := startServer(t, func(c *config.Config) { c.Set("maxclients", "2") })

	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		nc, err := net.Dial("tcp", ts.addr())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, nc)
		// Force the server to finish registering the connection.
		fmt.Fprint(nc, "PING\r\n")
		bufio.NewReader(nc).ReadString('\n')
	}

	nc, err := net.Dial("tcp", ts.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(nc).ReadString('\n')
	if err != nil {
		t.Fatalf("reading refusal: %v", err)
	}
	if !strings.Contains(line, "max number of clients") {
		t.Fatalf("third connection got %q", line)
	}
	if got := ts.Stats().RejectedConnections.Load(); got != 1 {
		t.Fatalf("rejected connections counted %d", got)
	}
}

func TestServerIdleTimeout(t *testing.T) {
	ts := startServer(t, func(c *config.Config) { c.Set("timeout", "1") })
	c := ts.connect(t)
	c.do("PING")

	c.nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	if _, err := c.nc.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("idle connection was not closed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("idle close took %v", elapsed)
	}
}

func TestServerBigValueRoundTrip(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	value := strings.Repeat("abcdefgh", 512*1024) // 4 MiB
	if got := text(c.do("SET", "big", value)); got != "OK" {
		t.Fatalf("SET big = %q", got)
	}
	got := c.do("GET", "big")
	if string(got.Str) != value {
		t.Fatalf("GET big returned %d bytes, want %d", len(got.Str), len(value))
	}
	if n := text(c.do("STRLEN", "big")); n != fmt.Sprint(len(value)) {
		t.Fatalf("STRLEN = %s", n)
	}
}

func TestServerBulkLimitRefused(t *testing.T) {
	ts := startServer(t, func(c *config.Config) { c.Set("proto-max-bulk-len", "1024") })
	c := ts.connect(t)
	c.sendRaw("*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$100000\r\n")
	v := c.reply()
	if !v.IsError() || !strings.Contains(string(v.Str), "bulk length") {
		t.Fatalf("oversized bulk got %q", text(v))
	}
}

func TestServerConcurrentClients(t *testing.T) {
	ts := startServer(t)
	const clients, ops = 16, 200
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		go func(id int) {
			nc, err := net.Dial("tcp", ts.addr())
			if err != nil {
				errs <- err
				return
			}
			defer nc.Close()
			rr := resp.NewReplyReader(nc)
			bw := bufio.NewWriter(nc)
			for j := 0; j < ops; j++ {
				key := fmt.Sprintf("client:%d:%d", id, j%10)
				bw.Write(resp.EncodeCommand(nil, []byte("SET"), []byte(key), []byte("v")))
				bw.Write(resp.EncodeCommand(nil, []byte("GET"), []byte(key)))
				bw.Write(resp.EncodeCommand(nil, []byte("INCR"), []byte("shared")))
				if err := bw.Flush(); err != nil {
					errs <- err
					return
				}
				for k := 0; k < 3; k++ {
					if _, err := rr.ReadReply(); err != nil {
						errs <- err
						return
					}
				}
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < clients; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	c := ts.connect(t)
	if got := text(c.do("GET", "shared")); got != fmt.Sprint(clients*ops) {
		t.Fatalf("shared counter = %s, want %d", got, clients*ops)
	}
}

func TestServerProtectedMode(t *testing.T) {
	// Bind to a non-loopback address so the check has something to refuse.
	// If the host has no such address, there is nothing to test.
	addr := nonLoopbackAddr()
	if addr == "" {
		t.Skip("no non-loopback address available")
	}
	ts := startServer(t, func(c *config.Config) {
		c.LoadFlags([]string{"--bind", "0.0.0.0", "--protected-mode", "yes"})
	})
	nc, err := net.Dial("tcp", net.JoinHostPort(addr, fmt.Sprint(ts.port)))
	if err != nil {
		t.Skipf("cannot reach the server over %s: %v", addr, err)
	}
	defer nc.Close()
	nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(nc).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "-DENIED") {
		t.Fatalf("non-loopback connection got %q", line)
	}
}

func nonLoopbackAddr() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

func TestAdminEndpoints(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("SET", "k", "v")

	base := "http://" + ts.adminAddr()
	for _, path := range []string{"/health", "/ready", "/metrics"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		if path == "/metrics" {
			for _, want := range []string{
				"kestrel_up 1", "kestrel_commands_total", "kestrel_db_keys{db=\"0\"}",
				"kestrel_command_calls_total{command=\"SET\"}",
			} {
				if !strings.Contains(string(body), want) &&
					!strings.Contains(string(body), strings.ToLower(want)) {
					t.Errorf("/metrics is missing %q", want)
				}
			}
		}
	}
	// pprof is off unless explicitly enabled (FR-8.5).
	r, err := http.Get(base + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("pprof was reachable with pprof-enabled no: %d", r.StatusCode)
	}
}

func TestGracefulShutdownDrains(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("SET", "k", "v")

	start := time.Now()
	if err := ts.Shutdown(true); err != nil {
		t.Fatal(err)
	}
	if err := ts.wait(t); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown took %v", elapsed)
	}
	if _, err := net.DialTimeout("tcp", ts.addr(), time.Second); err == nil {
		t.Fatal("the listener is still accepting after shutdown")
	}
}

func TestInfoSections(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("SET", "k", "v")

	full := string(c.do("INFO").Str)
	for _, want := range []string{
		"# Server", "kestrel_version:", "run_id:", "# Clients", "connected_clients:",
		"# Memory", "used_memory:", "# Persistence", "# Stats", "keyspace_hits:",
		"# Replication", "role:master", "# CPU", "# Keyspace", "db0:keys=1",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("INFO is missing %q", want)
		}
	}
	if strings.Contains(full, "# Commandstats") {
		t.Error("commandstats appeared in the default INFO")
	}
	if !strings.Contains(string(c.do("INFO", "commandstats").Str), "cmdstat_set") {
		t.Error("INFO commandstats is missing cmdstat_set")
	}
	if !strings.Contains(string(c.do("INFO", "all").Str), "# Latencystats") {
		t.Error("INFO all is missing latencystats")
	}
}

func waitReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if pingOK(addr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server at %s did not answer PING within 15s", addr)
}

func pingOK(addr string) bool {
	nc, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := nc.Write(resp.EncodeCommand(nil, []byte("PING"))); err != nil {
		return false
	}
	v, err := resp.NewReplyReader(nc).ReadReply()
	return err == nil && strings.EqualFold(string(v.Str), "PONG")
}
