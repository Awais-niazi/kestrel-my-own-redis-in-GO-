package command

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// allDescriptors returns every reachable command, subcommands included.
func allDescriptors(t *Table) []*Descriptor {
	var out []*Descriptor
	var walk func(d *Descriptor)
	walk = func(d *Descriptor) {
		out = append(out, d)
		for _, sub := range d.Subcommands {
			walk(sub)
		}
	}
	for _, d := range t.all {
		walk(d)
	}
	return out
}

// TestWriteCommandsDeclareLocality is the mechanical half of the mitigation
// for the snapshot gap in docs/design-notes.md issue 1: a write command that
// has not said whether its effect reads across shards cannot be added.
//
// register panics on the omission, so reaching this assertion at all means
// the table already loaded. The test exists so the invariant is stated where
// someone adding a command will read it, rather than only in a panic string.
func TestWriteCommandsDeclareLocality(t *testing.T) {
	table, err := NewTable(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range allDescriptors(table) {
		switch {
		case d.Is(Write) && d.Locality == LocalityUnset:
			t.Errorf("%s is a write command with no Locality declaration", d.FullName())
		case !d.Is(Write) && d.Locality != LocalityUnset:
			t.Errorf("%s declares a Locality but is not a write command", d.FullName())
		}
	}
}

// crossShardCommands is the audit record for issue 1: every command whose
// write can depend on the current value of a key in another shard, and which
// is therefore excluded from the snapshot window.
//
// It is written out in full rather than derived, because it cannot be
// derived. Key specifications do not identify these commands -- ZUNIONSTORE
// declares FirstKey and LastKey of 1, naming only its destination -- so the
// property is a statement about what the handler does, and the only honest
// check is that the list and the table agree.
//
// FLUSHDB and FLUSHALL are here for a slightly different reason than the
// rest. They read nothing, but they write every shard at once, so one landing
// inside a snapshot window leaves the shards serialized after it already
// flushed and the ones before it not. Recovery would then have to apply a
// flush to some shards and not others. Excluding them keeps the filter
// working on whole commands.
var crossShardCommands = []string{
	"COPY",
	"FLUSHALL",
	"FLUSHDB",
	"LMOVE",
	"MSETNX",
	"RENAME",
	"RENAMENX",
	"RPOPLPUSH",
	"SDIFFSTORE",
	"SINTERSTORE",
	"SMOVE",
	"SORT",
	"SUNIONSTORE",
	"SWAPDB",
	"ZDIFFSTORE",
	"ZINTERSTORE",
	"ZRANGESTORE",
	"ZUNIONSTORE",
}

func TestCrossShardCommandSet(t *testing.T) {
	table, err := NewTable(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range allDescriptors(table) {
		if d.Locality == LocalityCrossShard {
			got = append(got, d.FullName())
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(crossShardCommands, ",") {
		t.Errorf("cross-shard command set changed.\n got: %v\nwant: %v\n\n"+
			"If a command was added or its handler changed, update "+
			"crossShardCommands and docs/design-notes.md issue 1 together.",
			got, crossShardCommands)
	}
}

// TestSortStoreIsTreatedAsCrossShard records a deliberate over-approximation.
//
// SORT reads across shards only when STORE is given, but the declaration is
// static, so plain SORT pays the guard too. The guard is an uncontended
// RLock except while a snapshot runs, and SORT_RO exists for the read-only
// case, so the simpler declaration is worth the cost.
func TestSortStoreIsTreatedAsCrossShard(t *testing.T) {
	table, err := NewTable(nil)
	if err != nil {
		t.Fatal(err)
	}
	sortCmd, _ := table.Lookup([]byte("SORT"))
	if sortCmd.Locality != LocalityCrossShard {
		t.Error("SORT must be cross-shard: SORT ... STORE writes a key it did not read")
	}
	ro, _ := table.Lookup([]byte("SORT_RO"))
	if ro.Is(Write) || ro.Locality != LocalityUnset {
		t.Error("SORT_RO must stay a pure read, so it is never held up by a snapshot")
	}
}

// TestSnapshotWindowExcludesCrossShardCommands is the behavioural half: a
// cross-shard command waits for a snapshot in progress, and a shard-local
// one does not.
func TestSnapshotWindowExcludesCrossShardCommands(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.do("SET", "src", "v")

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ks.SnapshotWindow(func() {
			close(entered)
			<-release
		})
	}()
	<-entered

	// A shard-local write proceeds while the snapshot is being serialized.
	local := make(chan struct{})
	go func() {
		newSession(t, h).do("SET", "other", "v")
		close(local)
	}()
	select {
	case <-local:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("SET blocked during a snapshot window; only cross-shard commands should")
	}

	// A cross-shard write does not.
	cross := make(chan struct{})
	go func() {
		newSession(t, h).do("RENAME", "src", "dst")
		close(cross)
	}()
	select {
	case <-cross:
		t.Fatal("RENAME ran inside the snapshot window; its effect could land at an " +
			"offset the per-shard recovery filter cannot replay safely")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-done
	select {
	case <-cross:
	case <-time.After(2 * time.Second):
		t.Fatal("RENAME did not resume after the snapshot window closed")
	}
}

// TestSnapshotWindowWaitsForInFlightCrossShard covers the other order: a
// snapshot must not begin recording offsets while a cross-shard command is
// part-way through.
func TestSnapshotWindowWaitsForInFlightCrossShard(t *testing.T) {
	h := newTestHost(t)
	var wg sync.WaitGroup

	h.ks.BeginCrossShard()
	started := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		h.ks.SnapshotWindow(func() {})
	}()
	<-started

	select {
	case <-waitFor(&wg):
		t.Fatal("snapshot started while a cross-shard command was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	h.ks.EndCrossShard()
	select {
	case <-waitFor(&wg):
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot did not start after the cross-shard command finished")
	}
}

func waitFor(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	return done
}
