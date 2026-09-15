package command

import (
	"time"

	"kestrel/config"
	"kestrel/engine"
	"kestrel/resp"
)

// Host is the surface the command layer needs from the server. Defining it
// here rather than importing the server package is what keeps the dependency
// pointing one way (ADR-017).
type Host interface {
	// Keyspace returns the data store.
	Keyspace() *engine.Keyspace
	// Config returns the live configuration.
	Config() *config.Config
	// Commands returns this server's command table.
	Commands() *Table
	// Stats returns the shared server counters.
	Stats() *Stats
	// Info renders the INFO reply for the named sections.
	Info(sections []string) string
	// Propagate hands a canonical effect to the append log, the replication
	// backlog, and keyspace notifications (ADR-008).
	Propagate(db int, args ...[]byte)
	// Shutdown asks the server to stop.
	Shutdown(save bool) error
	// IsReplica reports whether this node is following a leader.
	IsReplica() bool
	// IsLoading reports whether the dataset is still being read from disk.
	IsLoading() bool
	// PersistenceError reports why durable writes are failing, or nil. A
	// non-nil value makes the dispatcher refuse writes.
	PersistenceError() error
	// StartTime is when the process began serving.
	StartTime() time.Time
	// ApplyRuntimeConfig re-reads the configuration into the subsystems
	// that cache parts of it, after a CONFIG SET.
	ApplyRuntimeConfig()
}

// Ctx carries one command execution.
type Ctx struct {
	Host   Host
	Client *Client
	Cmd    *Descriptor

	// Args holds the whole command, including its name at index 0. The
	// slices alias the connection's read buffer and are only valid for the
	// duration of the call: anything retained must be copied.
	Args [][]byte

	// dirty counts the changes the handler made. A write command that leaves
	// it at zero is not propagated, which is how SETNX-on-an-existing-key
	// avoids reaching the log.
	dirty int

	// effects holds explicit replacement effects set by a Rewrite. When it
	// is nil and dirty is non-zero, the command propagates verbatim.
	effects  [][][]byte
	suppress bool
}

// DB returns the client's currently selected database.
func (c *Ctx) DB() *engine.DB { return c.Client.DB }

// Name returns the command name as the client spelled it.
func (c *Ctx) Name() []byte { return c.Args[0] }

// Len returns the argument count, including the command name.
func (c *Ctx) Len() int { return len(c.Args) }

// Arg returns argument i, or nil when out of range.
func (c *Ctx) Arg(i int) []byte {
	if i < 0 || i >= len(c.Args) {
		return nil
	}
	return c.Args[i]
}

// Tail returns the arguments from i onwards.
func (c *Ctx) Tail(i int) [][]byte {
	if i >= len(c.Args) {
		return nil
	}
	return c.Args[i:]
}

// Dirty records that the handler changed n things.
func (c *Ctx) Dirty(n int) { c.dirty += n }

// Changed reports whether the handler recorded any change.
func (c *Ctx) Changed() bool { return c.dirty > 0 }

// Propagate replaces the command's effect with one or more explicit ones.
// Called from a Rewrite.
func (c *Ctx) Propagate(args ...[]byte) {
	// The arguments outlive the command, since the log and the replication
	// stream are written asynchronously, so they are copied here rather than
	// at each call site.
	cp := make([][]byte, len(args))
	for i, a := range args {
		b := make([]byte, len(a))
		copy(b, a)
		cp[i] = b
	}
	c.effects = append(c.effects, cp)
}

// SuppressPropagation drops the command's effect entirely.
func (c *Ctx) SuppressPropagation() { c.suppress = true }

// Now returns the current wall-clock time in milliseconds, taken from the
// engine's clock so that tests can control it.
func (c *Ctx) Now() int64 { return c.Host.Keyspace().Now() }

// Reply helpers used pervasively by handlers.

func (c *Ctx) ok() resp.Value            { return resp.OK() }
func (c *Ctx) int(n int64) resp.Value    { return resp.Int(n) }
func (c *Ctx) boolInt(b bool) resp.Value { return resp.Int(b2i(b)) }

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// bulkOrNil returns a bulk reply, or the null reply when the value is absent.
func bulkOrNil(v []byte, ok bool) resp.Value {
	if !ok {
		return resp.Null()
	}
	return resp.Bulk(v)
}
