package command

import (
	"fmt"
	"io"
	"strconv"
	"strings"

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

// Apply executes one logged effect against database db.
//
// An error means the record did not replay cleanly, which is a divergence
// between this keyspace and the one that wrote the log. Recovery stops
// there: a dataset that is silently not what the log says is worse than a
// server that refuses to start and explains why.
func (r *Replayer) Apply(db int, args [][]byte) error {
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

	ctx := &r.cl.ctx
	*ctx = Ctx{Host: r.host, Client: r.cl, Cmd: d, Args: args}
	reply := d.Handler(ctx)
	if reply.IsError() {
		return fmt.Errorf("replay: %s: %s", d.FullName(), reply.Str)
	}
	return nil
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
