package command

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Stats holds the server-wide counters exposed through INFO and /metrics.
type Stats struct {
	TotalConnections    atomic.Int64
	RejectedConnections atomic.Int64
	TotalCommands       atomic.Int64
	TotalNetInput       atomic.Int64
	TotalNetOutput      atomic.Int64
	UnsupportedCommands atomic.Int64 // drives the R4 prioritization metric

	Slowlog Slowlog
}

// NewStats returns an initialized Stats.
func NewStats(slowlogMaxLen int) *Stats {
	s := &Stats{}
	s.Slowlog.SetCapacity(slowlogMaxLen)
	return s
}

// Reset clears the global counters. Per-command counters live on the Table
// and are cleared through Table.ResetCommandStats.
func (s *Stats) Reset() {
	s.TotalCommands.Store(0)
	s.TotalConnections.Store(0)
	s.RejectedConnections.Store(0)
	s.UnsupportedCommands.Store(0)
	s.TotalNetInput.Store(0)
	s.TotalNetOutput.Store(0)
}

// SlowlogEntry is one recorded slow command.
type SlowlogEntry struct {
	ID       int64
	Time     time.Time
	Duration time.Duration
	Args     []string
	Addr     string
	Name     string
}

// Slowlog is a bounded ring of the slowest recent commands (FR-8.3).
type Slowlog struct {
	mu      sync.Mutex
	entries []SlowlogEntry
	next    int64
	cap     int
}

// SetCapacity resizes the log, dropping the oldest entries if it shrinks.
func (l *Slowlog) SetCapacity(n int) {
	if n < 0 {
		n = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = n
	if len(l.entries) > n {
		l.entries = l.entries[len(l.entries)-n:]
	}
}

// Add records a slow command. Arguments are truncated the way the reference
// implementation truncates them, so a huge MSET cannot pin memory in the log.
func (l *Slowlog) Add(name, addr string, args [][]byte, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cap == 0 {
		return
	}
	const maxArgs, maxArgLen = 32, 128
	rendered := make([]string, 0, min(len(args), maxArgs+1))
	for i, a := range args {
		if i == maxArgs && len(args) > maxArgs {
			rendered = append(rendered, "... ("+itoa(len(args)-maxArgs)+" more arguments)")
			break
		}
		if len(a) > maxArgLen {
			rendered = append(rendered, string(a[:maxArgLen])+"... ("+itoa(len(a)-maxArgLen)+" more bytes)")
			continue
		}
		rendered = append(rendered, string(a))
	}
	e := SlowlogEntry{
		ID: l.next, Time: time.Now(), Duration: d,
		Args: rendered, Addr: addr, Name: name,
	}
	l.next++
	l.entries = append(l.entries, e)
	if len(l.entries) > l.cap {
		l.entries = l.entries[len(l.entries)-l.cap:]
	}
}

// Entries returns up to n entries, newest first. A negative n returns all.
func (l *Slowlog) Entries(n int) []SlowlogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]SlowlogEntry, len(l.entries))
	copy(out, l.entries)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if n >= 0 && n < len(out) {
		out = out[:n]
	}
	return out
}

// Len reports how many entries are held.
func (l *Slowlog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Reset empties the log.
func (l *Slowlog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
