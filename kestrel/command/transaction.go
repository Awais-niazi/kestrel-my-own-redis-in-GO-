package command

import (
	"sync"

	"kestrel/resp"
)

// Transactions (FR-5.2).
//
// MULTI queues commands and EXEC runs them with no other write interleaved.
// That exclusion is the whole promise, and it is bought by taking every
// shard's write-ordering lock for the duration -- the same lock the
// dispatcher takes for one command, held across all of them.
//
// It is not the reference implementation's guarantee, which is total: there,
// a single thread means nothing at all observes a half-finished
// transaction. Here a concurrent *reader* can, because readers do not take
// that lock and making them take it would put a transaction's cost on every
// GET in the server. The deviation is recorded in docs/deviations.md; what
// it preserves is the part that matters for correctness -- no interleaved
// write, so no lost update, and WATCH's compare-and-set is sound.

// Watchers tracks WATCH registrations and invalidates them.
//
// The engine calls KeyModified with a shard lock held, so this must be quick
// and must not call back into the engine. Marking a flag on each watching
// client is all it does.
type Watchers struct {
	mu   sync.RWMutex
	keys map[watchKey]map[*Client]struct{}
}

type watchKey struct {
	db  int
	key string
}

// NewWatchers returns an empty registry.
func NewWatchers() *Watchers {
	return &Watchers{keys: make(map[watchKey]map[*Client]struct{})}
}

// KeyModified invalidates every transaction watching key.
func (w *Watchers) KeyModified(db int, key []byte) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	// The lookup allocates no string: the compiler elides the conversion
	// for a map index, and this runs under a shard lock on every write.
	for c := range w.keys[watchKey{db: db, key: string(key)}] {
		c.watchBroken.Store(true)
	}
}

// Watch registers a client's interest in a key.
func (w *Watchers) Watch(c *Client, db int, key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := watchKey{db: db, key: key}
	if _, dup := c.watching[k]; dup {
		return
	}
	c.watching[k] = struct{}{}
	set := w.keys[k]
	if set == nil {
		set = make(map[*Client]struct{})
		w.keys[k] = set
	}
	set[c] = struct{}{}
}

// Unwatch drops every registration a client holds and clears its broken
// flag. It is called by UNWATCH, by EXEC and DISCARD, and when a connection
// ends, so a registration can never outlive the client that made it.
func (w *Watchers) Unwatch(c *Client) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range c.watching {
		if set := w.keys[k]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(w.keys, k)
			}
		}
		delete(c.watching, k)
	}
	c.watchBroken.Store(false)
}

// Count reports how many keys are being watched, for INFO and tests.
func (w *Watchers) Count() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.keys)
}

func init() {
	register(&Descriptor{
		Name: "MULTI", Arity: 1, Flags: Readonly | Fast | Loading | Stale | NoMulti,
		Categories: []string{"transaction", "fast"},
		Summary:    "Starts a transaction.",
		Handler:    cmdMulti,
	})
	register(&Descriptor{
		Name: "EXEC", Arity: 1, Flags: Readonly | Loading | Stale | NoMulti,
		Categories: []string{"transaction", "slow"},
		Summary:    "Runs the queued commands with no other write interleaved.",
		Handler:    cmdExec,
	})
	register(&Descriptor{
		Name: "DISCARD", Arity: 1, Flags: Readonly | Fast | Loading | Stale | NoMulti,
		Categories: []string{"transaction", "fast"},
		Summary:    "Abandons a transaction.",
		Handler:    cmdDiscard,
	})
	register(&Descriptor{
		Name: "WATCH", Arity: -2, Flags: Readonly | Fast | Loading | Stale | NoMulti,
		FirstKey:   1,
		LastKey:    -1,
		Step:       1,
		Categories: []string{"transaction", "fast"},
		Summary:    "Aborts the next transaction if any of these keys changes.",
		Handler:    cmdWatch,
	})
	register(&Descriptor{
		Name: "UNWATCH", Arity: 1, Flags: Readonly | Fast | Loading | Stale | NoMulti,
		Categories: []string{"transaction", "fast"},
		Summary:    "Forgets every watched key.",
		Handler:    cmdUnwatch,
	})
}

func cmdMulti(c *Ctx) resp.Value {
	if c.Client.InMulti() {
		return resp.Err("ERR MULTI calls can not be nested")
	}
	c.Client.beginMulti()
	return resp.OK()
}

func cmdDiscard(c *Ctx) resp.Value {
	if !c.Client.InMulti() {
		return resp.Err("ERR DISCARD without MULTI")
	}
	c.Client.endMulti()
	c.Host.Watchers().Unwatch(c.Client)
	return resp.OK()
}

func cmdWatch(c *Ctx) resp.Value {
	if c.Client.InMulti() {
		// Watching from inside a transaction cannot do anything useful: the
		// keys would be checked at an EXEC that has already begun.
		return resp.Err("ERR WATCH inside MULTI is not allowed")
	}
	w := c.Host.Watchers()
	for i := 1; i < c.Len(); i++ {
		w.Watch(c.Client, c.Client.DBIndex, string(c.Arg(i)))
	}
	return resp.OK()
}

func cmdUnwatch(c *Ctx) resp.Value {
	c.Host.Watchers().Unwatch(c.Client)
	return resp.OK()
}

// cmdExec runs the queued commands.
//
// The replies are written directly rather than collected into a value,
// because a queued command may itself be one that writes its own output.
func cmdExec(c *Ctx) resp.Value {
	cl := c.Client
	if !cl.InMulti() {
		return resp.Err("ERR EXEC without MULTI")
	}
	queued := cl.takeQueued()
	aborted := cl.multiAborted
	cl.endMulti()

	if aborted {
		c.Host.Watchers().Unwatch(cl)
		return resp.Err("EXECABORT Transaction discarded because of previous errors.")
	}

	ks := c.Host.Keyspace()

	// Every shard's ordering lock is held for the whole transaction, so no
	// other write can land between two of its commands. The watched keys
	// are checked inside it too: a check outside would be a race with
	// exactly the writer it exists to detect.
	order := ks.OrderAllWrites()
	if cl.watchBroken.Load() {
		order.Done()
		c.Host.Watchers().Unwatch(cl)
		// A nil array, not an empty one. An empty array would say the
		// transaction ran and did nothing.
		return resp.NullArray()
	}

	cl.Out.WriteArrayHeader(len(queued))
	cl.inExec = true
	for _, args := range queued {
		runQueued(c.Host, cl, args)
	}
	cl.inExec = false
	order.Done()

	c.Host.Watchers().Unwatch(cl)
	return resp.None()
}

// runQueued executes one command of a transaction, with the ordering locks
// already held by EXEC.
func runQueued(host Host, cl *Client, args [][]byte) {
	d, reply := resolve(host, cl, args)
	if d == nil {
		cl.Out.WriteValue(reply)
		return
	}
	ctx := &cl.ctx
	*ctx = Ctx{Host: host, Client: cl, Cmd: d, Args: args}

	result := d.Handler(ctx)
	host.Commands().recordUntimed(d, result.IsError())
	propagate(host, cl, ctx, d, args)
	cl.Out.WriteValue(result)
}

// queueCommand records a command for EXEC, or refuses it.
//
// A command that cannot be queued poisons the transaction rather than being
// skipped. Running the rest would be a transaction the caller did not ask
// for, and one whose missing command they would have to infer from the reply
// count.
func queueCommand(host Host, cl *Client, args [][]byte) resp.Value {
	d, reply := resolve(host, cl, args)
	if d == nil {
		cl.multiAborted = true
		return reply
	}
	if d.Is(NoMulti) {
		cl.multiAborted = true
		return resp.Err("ERR " + d.FullName() + " is not allowed in MULTI")
	}
	cl.queue = append(cl.queue, copyArgs(args))
	return queuedReply
}

var queuedReply = resp.Simple("QUEUED")

// copyArgs detaches a command from the connection's read buffer, which is
// reused as soon as the next command arrives.
func copyArgs(args [][]byte) [][]byte {
	out := make([][]byte, len(args))
	for i, a := range args {
		out[i] = append([]byte(nil), a...)
	}
	return out
}

// InMulti reports whether the client is queueing commands.
func (c *Client) InMulti() bool { return c.inMulti }

func (c *Client) beginMulti() {
	c.inMulti = true
	c.multiAborted = false
	c.queue = c.queue[:0]
}

func (c *Client) endMulti() {
	c.inMulti = false
	c.multiAborted = false
	c.queue = nil
}

// takeQueued returns the queued commands and clears the queue.
func (c *Client) takeQueued() [][][]byte {
	q := c.queue
	c.queue = nil
	return q
}

// QueuedCount reports how many commands are waiting, for tests and INFO.
func (c *Client) QueuedCount() int { return len(c.queue) }
