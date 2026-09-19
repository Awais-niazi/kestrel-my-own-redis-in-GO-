package command

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kestrel/resp"
)

// MONITOR streams every command the server executes to the clients watching.
//
// It travels the same path as a Pub/Sub message: a bounded queue and the
// connection's delivery goroutine. From the connection's side they are the
// same thing -- output it did not ask for -- and a monitor that cannot keep
// up is dropped for the same reason a slow subscriber is.
//
// The cost when nobody is watching is one atomic load per command, which is
// why the count is kept separately from the list.

// Monitors is the set of clients watching the command stream.
type Monitors struct {
	mu    sync.RWMutex
	set   map[*Client]struct{}
	count atomic.Int64
}

// NewMonitors returns an empty set.
func NewMonitors() *Monitors { return &Monitors{set: make(map[*Client]struct{})} }

// Add starts feeding a client.
func (m *Monitors) Add(c *Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.set[c]; dup {
		return
	}
	m.set[c] = struct{}{}
	c.monitoring.Store(true)
	m.count.Store(int64(len(m.set)))
}

// Remove stops feeding a client. It is called by RESET and when a connection
// ends, so a monitor cannot outlive its socket.
func (m *Monitors) Remove(c *Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.set[c]; !ok {
		return
	}
	delete(m.set, c)
	c.monitoring.Store(false)
	m.count.Store(int64(len(m.set)))
}

// Count reports how many clients are watching.
func (m *Monitors) Count() int { return int(m.count.Load()) }

// Feed renders a command and sends it to every monitor.
//
// The line is built once, not per monitor. Rendering it is the expensive
// part -- quoting every argument -- and it is identical for all of them.
func (m *Monitors) Feed(cl *Client, args [][]byte) {
	if m.count.Load() == 0 {
		return
	}
	line := resp.Simple(monitorLine(cl, args, time.Now()))

	m.mu.RLock()
	defer m.mu.RUnlock()
	for c := range m.set {
		// A monitor watching its own commands would feed itself forever.
		if c == cl {
			continue
		}
		c.Push(line)
	}
}

// monitorLine renders one command in the form the reference implementation
// uses, which existing tooling parses.
func monitorLine(cl *Client, args [][]byte, now time.Time) string {
	var b strings.Builder
	b.Grow(64 + len(args)*12)

	b.WriteString(strconv.FormatInt(now.Unix(), 10))
	b.WriteByte('.')
	// Six digits of microseconds, zero padded, because a monitor line is
	// parsed positionally and a short field shifts everything after it.
	micros := strconv.FormatInt(int64(now.Nanosecond()/1000), 10)
	for i := len(micros); i < 6; i++ {
		b.WriteByte('0')
	}
	b.WriteString(micros)

	b.WriteString(" [")
	b.WriteString(strconv.Itoa(cl.DBIndex))
	b.WriteByte(' ')
	b.WriteString(cl.Addr)
	b.WriteString("] ")

	redact := redactedFrom(args)
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		if redact >= 0 && i >= redact {
			b.WriteString(`"(redacted)"`)
			continue
		}
		b.WriteString(strconv.Quote(string(a)))
	}
	return b.String()
}

// redactedFrom returns the argument index from which a command's arguments
// must not be shown, or -1.
//
// A monitor stream is a plain-text feed of everything the server does, and
// it is exactly the wrong place for a password. The reference implementation
// hides AUTH for this reason; HELLO carries the same secret in the same way
// and gets the same treatment.
func redactedFrom(args [][]byte) int {
	if len(args) == 0 {
		return -1
	}
	switch strings.ToUpper(string(args[0])) {
	case "AUTH":
		return 1
	case "HELLO":
		for i := 1; i < len(args); i++ {
			if strings.EqualFold(string(args[i]), "AUTH") {
				return i + 1
			}
		}
	}
	return -1
}

func init() {
	register(&Descriptor{
		Name: "MONITOR", Arity: 1, Flags: Readonly | Admin | Loading | Stale | NoMulti,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "Streams every command the server executes.",
		Handler:    cmdMonitor,
	})
}

func cmdMonitor(c *Ctx) resp.Value {
	c.Host.Monitors().Add(c.Client)
	return resp.OK()
}
