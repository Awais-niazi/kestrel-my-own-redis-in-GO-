package command

import (
	"sort"
	"sync/atomic"
	"time"
)

// commandCounters is the per-command accounting behind INFO commandstats.
//
// It lives in an array indexed by Descriptor.index rather than in a map, so
// recording a call costs a handful of atomic adds on a slot that is not
// shared with any other command.
type commandCounters struct {
	calls     atomic.Int64
	errors    atomic.Int64
	rejected  atomic.Int64
	micros    atomic.Int64
	maxMicros atomic.Int64
	_         [24]byte // pad towards a cache line to limit false sharing
}

// CommandStat is a point-in-time reading of one command's counters.
type CommandStat struct {
	Calls     int64
	Errors    int64
	Rejected  int64
	Micros    int64
	MaxMicros int64
}

func (t *Table) record(d *Descriptor, elapsed time.Duration, failed bool) {
	c := &t.counters[d.index]
	micros := elapsed.Microseconds()
	c.calls.Add(1)
	c.micros.Add(micros)
	if failed {
		c.errors.Add(1)
	}
	// A racing update can lose a maximum here. Reporting a slightly low
	// peak is an acceptable trade for keeping the hot path lock-free.
	for {
		cur := c.maxMicros.Load()
		if micros <= cur || c.maxMicros.CompareAndSwap(cur, micros) {
			break
		}
	}
}

// recordUntimed accounts a call without a duration, for when latency
// tracking is disabled.
func (t *Table) recordUntimed(d *Descriptor, failed bool) {
	c := &t.counters[d.index]
	c.calls.Add(1)
	if failed {
		c.errors.Add(1)
	}
}

func (t *Table) recordRejected(d *Descriptor) { t.counters[d.index].rejected.Add(1) }

// CommandStats returns a snapshot keyed by command name, skipping commands
// that were never called.
func (t *Table) CommandStats() map[string]CommandStat {
	out := make(map[string]CommandStat, 32)
	var walk func(d *Descriptor)
	walk = func(d *Descriptor) {
		c := &t.counters[d.index]
		calls, rejected := c.calls.Load(), c.rejected.Load()
		if calls > 0 || rejected > 0 {
			out[d.FullName()] = CommandStat{
				Calls: calls, Errors: c.errors.Load(), Rejected: rejected,
				Micros: c.micros.Load(), MaxMicros: c.maxMicros.Load(),
			}
		}
		for _, sub := range d.Subcommands {
			walk(sub)
		}
	}
	for _, d := range t.all {
		walk(d)
	}
	return out
}

// ResetCommandStats clears every counter, for CONFIG RESETSTAT.
func (t *Table) ResetCommandStats() {
	for i := range t.counters {
		c := &t.counters[i]
		c.calls.Store(0)
		c.errors.Store(0)
		c.rejected.Store(0)
		c.micros.Store(0)
		c.maxMicros.Store(0)
	}
}

// SortedCommandStats returns the snapshot as a name-ordered slice, which is
// what INFO and the metrics endpoint both want.
func (t *Table) SortedCommandStats() ([]string, map[string]CommandStat) {
	stats := t.CommandStats()
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, stats
}
