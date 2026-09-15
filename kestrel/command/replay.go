package command

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"kestrel/engine"
	"kestrel/resp"
)

// Replayer applies logged effects to a host's keyspace.
//
// It is how recovery and, from M4, a replica link turn a stream of records
// back into state. It deliberately does not go through Execute: a logged
// effect has already been validated, authenticated and rate-limited once, on
// the leader, and running those checks again would let a configuration
// change between then and now silently drop a record. What it does keep is
// arity and the write check, because those failing means the record is not
// what it claims to be.
//
// A Replayer is not safe for concurrent use. Records are ordered, so
// applying them from more than one goroutine would not be meaningful.
type Replayer struct {
	host  Host
	cl    *Client
	table *Table

	// anchorOf, when set, reconciles the log against a fuzzy snapshot. See
	// Filter.
	anchorOf func(db, shard int) uint64
	split    [][]byte // reused when a record has to be applied to some keys only
}

// NewReplayer returns a Replayer that writes into host's keyspace.
//
// The caller is responsible for putting the keyspace into loading mode
// (engine.Keyspace.SetLoading) for the duration, and for making sure nothing
// is installed that would propagate the replayed effects back into the log
// they came from.
func NewReplayer(host Host) *Replayer {
	// Replies go nowhere. Handlers that write their own output reach for
	// the writer rather than returning a value, so it has to be real.
	out := resp.NewWriter(io.Discard)
	cl := NewClient(0, "replay", "replay", out, host.Keyspace().DB(0), true)
	cl.Replica = true
	return &Replayer{host: host, cl: cl, table: host.Commands()}
}

// Filter makes the Replayer reconcile records against a fuzzy snapshot.
//
// anchorOf returns the log offset a shard's snapshot contents correspond to,
// so the shard already holds every record before that offset and needs every
// record from it onwards. Without a filter every record is applied whole,
// which is right when replaying a log into an empty keyspace.
//
// Passing nil clears the filter.
func (r *Replayer) Filter(anchorOf func(db, shard int) uint64) *Replayer {
	r.anchorOf = anchorOf
	return r
}

// Apply executes one logged effect against database db.
//
// offset is the record's position in the log stream, and matters only when a
// filter is installed.
//
// An error means the record did not replay cleanly, which is a divergence
// between this keyspace and the one that wrote the log. Recovery stops
// there: a dataset that is silently not what the log says is worse than a
// server that refuses to start and explains why.
func (r *Replayer) Apply(db int, offset uint64, args [][]byte) error {
	if len(args) == 0 {
		return fmt.Errorf("replay: empty record")
	}
	d, ok := r.table.Lookup(args[0])
	if !ok {
		return fmt.Errorf("replay: unknown command %q", args[0])
	}
	if len(d.Subcommands) > 0 && len(args) >= 2 {
		if sub, ok := r.table.LookupSub(d, args[1]); ok {
			d = sub
		}
	}
	if !d.Is(Write) {
		// Only writes are propagated, so a read in the log means the file
		// was not written by this system, or was written by a version whose
		// command table disagrees with this one.
		return fmt.Errorf("replay: %s is not a write command", d.FullName())
	}
	if !arityOK(d, len(args)) {
		return fmt.Errorf("replay: %s got %d arguments", d.FullName(), len(args))
	}
	if !r.cl.Select(r.host.Keyspace(), db) {
		return fmt.Errorf("replay: no database %d", db)
	}

	if r.anchorOf != nil {
		apply, err := r.narrow(d, db, offset, args)
		if err != nil || apply == nil {
			return err
		}
		args = apply
	}
	return r.run(d, args)
}

func (r *Replayer) run(d *Descriptor, args [][]byte) error {
	ctx := &r.cl.ctx
	*ctx = Ctx{Host: r.host, Client: r.cl, Cmd: d, Args: args}
	reply := d.Handler(ctx)
	if reply.IsError() {
		return fmt.Errorf("replay: %s: %s", d.FullName(), reply.Str)
	}
	return nil
}

// narrow returns the arguments to apply for a record, given how far each
// shard had been snapshotted. It returns nil when the record must be skipped
// entirely.
//
// A shard whose anchor is at or before this record does not have it yet and
// must be given it. A shard whose anchor is after it already absorbed it,
// and applying it again would be wrong for anything that is not idempotent:
// a second INCR or RPUSH is a different value, not a redundant one.
func (r *Replayer) narrow(d *Descriptor, db int, offset uint64, args [][]byte) ([][]byte, error) {
	keys := ExtractKeys(d, args)
	if len(keys) == 0 {
		// A write with no keys touches the whole database -- FLUSHDB,
		// FLUSHALL, SWAPDB. All three are cross-shard, so the snapshot
		// window excluded them and this record cannot be inside it: it
		// applies everywhere or not at all, and "not at all" is impossible
		// because recovery starts at the first anchor.
		return args, nil
	}

	engineDB := r.host.Keyspace().DB(db)
	if engineDB == nil {
		return nil, fmt.Errorf("replay: no database %d", db)
	}
	wanted, count := 0, 0
	for _, k := range keys {
		count++
		if offset >= r.anchorOf(db, engineDB.ShardIndexOf(k)) {
			wanted++
		}
	}
	switch wanted {
	case count:
		return args, nil
	case 0:
		return nil, nil
	}
	return r.splitKeys(d, args, engineDB, db, offset)
}

// splitKeys rebuilds a record from only the key groups whose shards still
// need it.
//
// This can only happen for a shard-local command with keys in more than one
// shard, inside the snapshot window. Every cross-shard command is kept out
// of that window (docs/design-notes.md issue 1), so what reaches here is
// DEL, UNLINK and MSET: commands whose arguments after the name are groups
// of Step, each beginning with its key, and whose keys are independent of
// one another.
//
// Anything else is refused rather than guessed at. A command that needs
// splitting and does not fit this shape is a correctness problem that must
// be looked at, not one to paper over with a partial application.
func (r *Replayer) splitKeys(d *Descriptor, args [][]byte, engineDB *engine.DB,
	db int, offset uint64) ([][]byte, error) {

	step := d.Step
	if step <= 0 {
		step = 1
	}
	if d.FirstKey != 1 || d.LastKey != -1 || (len(args)-1)%step != 0 {
		return nil, fmt.Errorf("replay: %s spans shards that were snapshotted at "+
			"different offsets and cannot be split by its key specification "+
			"(first=%d last=%d step=%d); see docs/design-notes.md issue 1",
			d.FullName(), d.FirstKey, d.LastKey, step)
	}

	r.split = append(r.split[:0], args[0])
	for i := 1; i < len(args); i += step {
		if offset >= r.anchorOf(db, engineDB.ShardIndexOf(args[i])) {
			r.split = append(r.split, args[i:i+step]...)
		}
	}
	if len(r.split) == 1 {
		return nil, nil
	}
	return r.split, nil
}

// Describe renders a record for an error message or a log line, truncating
// arguments that are too long to be worth printing in full.
func Describe(args [][]byte) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		s := string(a)
		if len(s) > 40 {
			s = fmt.Sprintf("%s...(%d bytes)", s[:40], len(a))
		}
		// Keys and values are binary safe, and an error message that emits
		// a raw NUL or a newline is one that mangles the log it lands in.
		if strconv.Quote(s) != `"`+s+`"` {
			s = strconv.Quote(s)
		}
		b.WriteString(s)
	}
	return b.String()
}
