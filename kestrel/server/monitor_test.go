package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMonitorStreamsCommands(t *testing.T) {
	ts := startServer(t)
	mon := ts.connect(t)
	other := ts.connect(t)

	if got := text(mon.do("MONITOR")); got != "OK" {
		t.Fatalf("MONITOR returned %q", got)
	}
	waitFor(t, "the monitor to register", func() bool { return ts.monitors.Count() == 1 })

	other.do("SET", "key", "value")
	line := text(mon.reply())
	if !strings.Contains(line, `"SET" "key" "value"`) {
		t.Errorf("the monitor line is %q", line)
	}
	// The line begins with a timestamp, then the database and address.
	if !strings.Contains(line, "[0 127.0.0.1:") {
		t.Errorf("the line does not name the database and client: %q", line)
	}
	fields := strings.SplitN(line, " ", 2)
	if !strings.Contains(fields[0], ".") || len(strings.SplitN(fields[0], ".", 2)[1]) != 6 {
		t.Errorf("the timestamp is not seconds.microseconds: %q", fields[0])
	}
}

// TestMonitorDoesNotEchoItsOwnCommands guards against a monitor feeding
// itself, which would never stop.
func TestMonitorDoesNotEchoItsOwnCommands(t *testing.T) {
	ts := startServer(t)
	mon := ts.connect(t)
	other := ts.connect(t)
	mon.do("MONITOR")
	waitFor(t, "the monitor to register", func() bool { return ts.monitors.Count() == 1 })

	mon.send("PING")
	other.do("SET", "marker", "1")

	// The next line the monitor sees must be the other client's command,
	// not its own PING.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line := text(mon.reply())
		if strings.Contains(line, "PONG") || line == "PONG" {
			continue // its own reply, which is not a monitor line
		}
		if strings.Contains(line, `"PING"`) {
			t.Fatalf("the monitor echoed its own command: %q", line)
		}
		if strings.Contains(line, `"SET" "marker"`) {
			return
		}
	}
	t.Fatal("the other client's command never arrived")
}

// TestMonitorRedactsPasswords is the one thing a plain-text feed of every
// command must not do.
func TestMonitorRedactsPasswords(t *testing.T) {
	ts := startServer(t)
	mon := ts.connect(t)
	other := ts.connect(t)
	mon.do("MONITOR")
	waitFor(t, "the monitor to register", func() bool { return ts.monitors.Count() == 1 })

	other.do("AUTH", "hunter2")
	line := text(mon.reply())
	if strings.Contains(line, "hunter2") {
		t.Errorf("MONITOR leaked a password: %q", line)
	}
	if !strings.Contains(line, "redacted") {
		t.Errorf("the line does not show the argument was withheld: %q", line)
	}

	other.do("HELLO", "2", "AUTH", "default", "hunter3")
	line = text(mon.reply())
	if strings.Contains(line, "hunter3") {
		t.Errorf("MONITOR leaked a password from HELLO: %q", line)
	}
}

// TestMonitorSeesRefusedCommands: a command that was refused is exactly the
// kind an operator turned MONITOR on to find.
func TestMonitorSeesRefusedCommands(t *testing.T) {
	ts := startServer(t)
	mon := ts.connect(t)
	other := ts.connect(t)
	mon.do("MONITOR")
	waitFor(t, "the monitor to register", func() bool { return ts.monitors.Count() == 1 })

	other.do("NOTACOMMAND", "x")
	if line := text(mon.reply()); !strings.Contains(line, "NOTACOMMAND") {
		t.Errorf("the monitor did not see a refused command: %q", line)
	}
}

func TestMonitorEndsWithTheConnection(t *testing.T) {
	ts := startServer(t)
	mon := ts.connect(t)
	mon.do("MONITOR")
	waitFor(t, "the monitor to register", func() bool { return ts.monitors.Count() == 1 })
	mon.nc.Close()
	waitFor(t, "the monitor to deregister", func() bool { return ts.monitors.Count() == 0 })
}

func TestClientList(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	other := ts.connect(t)
	other.do("CLIENT", "SETNAME", "worker")
	other.do("SELECT", "3")

	list := text(c.do("CLIENT", "LIST"))
	lines := strings.Split(strings.TrimRight(list, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("CLIENT LIST returned %d lines:\n%s", len(lines), list)
	}
	if !strings.Contains(list, "name=worker") {
		t.Errorf("the named client is missing:\n%s", list)
	}
	if !strings.Contains(list, "db=3") {
		t.Errorf("the selected database is missing:\n%s", list)
	}
	for _, l := range lines {
		if !strings.Contains(l, "id=") || !strings.Contains(l, "addr=") {
			t.Errorf("a line is missing its fields: %q", l)
		}
	}
}

func TestClientListFilters(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	other := ts.connect(t)
	id := text(other.do("CLIENT", "ID"))

	got := text(c.do("CLIENT", "LIST", "ID", id))
	if strings.Count(strings.TrimRight(got, "\n"), "\n") != 0 {
		t.Errorf("filtering by one id returned more than one line:\n%s", got)
	}
	if !strings.Contains(got, "id="+id) {
		t.Errorf("the filtered line is not the one asked for:\n%s", got)
	}
	if got := text(c.do("CLIENT", "LIST", "TYPE", "pubsub")); got != "" {
		t.Errorf("TYPE pubsub matched with no subscribers:\n%s", got)
	}
	if got := text(c.do("CLIENT", "LIST", "TYPE", "nonsense")); !strings.HasPrefix(got, "ERR") {
		t.Errorf("an unknown type returned %q", got)
	}
}

func TestClientKillByAddress(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	victim := ts.connect(t)
	addr := clientAddrOf(t, victim)

	if got := text(c.do("CLIENT", "KILL", addr)); got != "OK" {
		t.Fatalf("CLIENT KILL returned %q", got)
	}
	waitFor(t, "the victim to disappear", func() bool {
		return !strings.Contains(text(c.do("CLIENT", "LIST")), "addr="+addr)
	})
	if got := text(c.do("CLIENT", "KILL", "127.0.0.1:1")); !strings.HasPrefix(got, "ERR") {
		t.Errorf("killing an unknown address returned %q", got)
	}
}

func clientAddrOf(t *testing.T, c *conn) string {
	t.Helper()
	info := text(c.do("CLIENT", "INFO"))
	for _, f := range strings.Fields(info) {
		if strings.HasPrefix(f, "addr=") {
			return strings.TrimPrefix(f, "addr=")
		}
	}
	t.Fatalf("no address in %q", info)
	return ""
}

func TestClientKillByFilter(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	victim := ts.connect(t)
	id := text(victim.do("CLIENT", "ID"))

	if got := text(c.do("CLIENT", "KILL", "ID", id)); got != "1" {
		t.Errorf("CLIENT KILL ID returned %q, want 1", got)
	}
	waitFor(t, "the victim to disappear", func() bool {
		return !strings.Contains(text(c.do("CLIENT", "LIST")), "id="+id+" ")
	})
}

// TestClientKillSparesTheCallerByDefault: killing the connection that asked
// is almost never what was meant.
func TestClientKillSparesTheCallerByDefault(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	ts.connect(t)
	// A dial returns before the server has accepted it, so the second
	// client may not be in the table yet.
	waitFor(t, "both clients to register", func() bool {
		return strings.Count(text(c.do("CLIENT", "LIST")), "\n") >= 2
	})

	killed := text(c.do("CLIENT", "KILL", "TYPE", "normal"))
	if killed == "0" {
		t.Fatal("CLIENT KILL TYPE normal killed nobody")
	}
	// The caller is still here to be told.
	if got := text(c.do("PING")); got != "PONG" {
		t.Errorf("the caller killed itself: %q", got)
	}

	// SKIPME no includes the caller, which must still receive the count
	// before its connection goes.
	other := ts.connect(t)
	waitFor(t, "the new client to register", func() bool {
		return strings.Contains(text(other.do("CLIENT", "LIST")), "addr=")
	})
	got := text(other.do("CLIENT", "KILL", "TYPE", "normal", "SKIPME", "NO"))
	if got == "0" || strings.HasPrefix(got, "ERR") {
		t.Errorf("SKIPME NO returned %q", got)
	}
}

func TestClientKillRejectsBadFilters(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	for _, args := range [][]string{
		{"CLIENT", "KILL", "ID"},
		{"CLIENT", "KILL", "ID", "notanumber"},
		{"CLIENT", "KILL", "SKIPME", "maybe"},
		{"CLIENT", "KILL", "MAXAGE", "-1"},
		{"CLIENT", "KILL", "NONSENSE", "x"},
	} {
		if got := text(c.do(args...)); !strings.HasPrefix(got, "ERR") {
			t.Errorf("%v returned %q", args, got)
		}
	}
}

func TestClientNoTouch(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	if got := text(c.do("CLIENT", "NO-TOUCH", "ON")); got != "OK" {
		t.Errorf("CLIENT NO-TOUCH ON returned %q", got)
	}
	if got := text(c.do("CLIENT", "NO-TOUCH", "MAYBE")); !strings.HasPrefix(got, "ERR") {
		t.Errorf("CLIENT NO-TOUCH MAYBE returned %q", got)
	}
}

// TestMonitorIsNotHeldUpByASlowWatcher mirrors the Pub/Sub guarantee: a
// monitor that never reads must not stall the commands it is watching.
func TestMonitorIsNotHeldUpByASlowWatcher(t *testing.T) {
	ts := startServer(t)
	mon := ts.connect(t)
	mon.do("MONITOR")
	waitFor(t, "the monitor to register", func() bool { return ts.monitors.Count() == 1 })

	c := ts.connect(t)
	start := time.Now()
	for i := 0; i < 5000; i++ {
		c.do("SET", fmt.Sprintf("k%d", i), "value")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("5000 writes took %v with a monitor that never reads", elapsed)
	}
	if got := text(c.do("PING")); got != "PONG" {
		t.Errorf("the writer is stuck: %q", got)
	}
}
