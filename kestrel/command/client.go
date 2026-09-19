package command

import (
	"sync/atomic"
	"time"

	"kestrel/engine"
	"kestrel/resp"
)

// Client is the per-connection state the command layer needs. The server
// owns the socket; this is everything above it.
//
// A Client is touched only by its own connection goroutine, except for the
// atomically accessed fields, which exist so that CLIENT LIST and the idle
// reaper can observe a client they do not own.
type Client struct {
	ID        uint64
	Addr      string
	LocalAddr string
	CreatedAt time.Time

	// Name is settable through CLIENT SETNAME.
	Name string

	// DBIndex and DB are the SELECTed database.
	DBIndex int
	DB      *engine.DB

	// Authenticated records whether AUTH has succeeded, or whether no
	// password is configured.
	Authenticated bool
	User          string

	// Out is the connection's reply writer. The negotiated protocol version
	// lives on it, since serialization is where the version matters.
	Out *resp.Writer

	// CloseAfterReply asks the connection loop to hang up once the pending
	// reply has been flushed, which is how QUIT and protocol errors end.
	CloseAfterReply bool

	// ReplicaPort is the port a replica announced with REPLCONF
	// listening-port, so that INFO can name it as an address rather than as
	// the ephemeral port its outbound connection happens to use.
	ReplicaPort int
	// ReplicaAck is the stream offset a replica has reported applying. It is
	// written by the connection's own goroutine and read by WAIT from
	// another, so it is atomic.
	ReplicaAck atomic.Uint64
	// PSync, when set by the PSYNC handler, asks the server to take this
	// connection out of the command loop and feed it the replication
	// stream.
	PSync *PSyncRequest

	// channels and patterns are this client's subscriptions. They live here
	// rather than in the registry so that unsubscribing everything is a
	// walk of the client's own sets, but they are only ever touched under
	// the registry's lock: one place decides what a client is subscribed
	// to, rather than two that have to agree.
	channels map[string]struct{}
	patterns map[string]struct{}
	// subCount mirrors their total so the dispatcher can ask "is this a
	// subscriber" on every command without taking the registry's lock.
	subCount atomic.Int64
	// outbox carries messages to the connection's delivery goroutine.
	outbox chan Message
	// overflowed records that the outbox filled, which means this client is
	// too slow to keep.
	overflowed atomic.Bool

	// Replica marks a connection that has been promoted to a replication
	// link and must no longer be treated as a normal client.
	Replica bool

	lastCommand atomic.Pointer[Descriptor]
	lastActive  atomic.Int64 // unix milliseconds

	// ctx is reused across commands. A Ctx never outlives the call that
	// uses it, and a client is driven by exactly one goroutine, so reusing
	// it keeps the dispatch path off the heap (§11).
	ctx Ctx
}

// NewClient returns a client bound to database 0 of ks.
func NewClient(id uint64, addr, localAddr string, out *resp.Writer, db *engine.DB, authenticated bool) *Client {
	c := &Client{
		ID:            id,
		Addr:          addr,
		LocalAddr:     localAddr,
		CreatedAt:     time.Now(),
		DB:            db,
		Out:           out,
		Authenticated: authenticated,
		User:          "default",
		channels:      make(map[string]struct{}),
		patterns:      make(map[string]struct{}),
		outbox:        make(chan Message, outboxDepth),
	}
	c.Touch(time.Now())
	return c
}

// Protocol returns the negotiated RESP version.
func (c *Client) Protocol() int { return c.Out.Protocol() }

// Touch records activity, for the idle timeout and CLIENT LIST.
func (c *Client) Touch(t time.Time) { c.lastActive.Store(t.UnixMilli()) }

// IdleSeconds reports how long the client has been idle.
func (c *Client) IdleSeconds(now time.Time) int64 {
	return (now.UnixMilli() - c.lastActive.Load()) / 1000
}

// LastCommand returns the name of the most recent command, for CLIENT LIST.
func (c *Client) LastCommand() string {
	if d := c.lastCommand.Load(); d != nil {
		return d.FullName()
	}
	return ""
}

func (c *Client) setLastCommand(d *Descriptor) { c.lastCommand.Store(d) }

// Select moves the client to another database.
func (c *Client) Select(ks *engine.Keyspace, index int) bool {
	db := ks.DB(index)
	if db == nil {
		return false
	}
	c.DBIndex, c.DB = index, db
	return true
}
