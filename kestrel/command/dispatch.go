package command

import (
	"time"

	"kestrel/config"
	"kestrel/engine"
	"kestrel/resp"
)

// Execute resolves, validates and runs one command, writing the reply to the
// client's buffered writer.
//
// This is the dispatch layer of §6.3: it resolves the name, checks arity,
// authentication and server state, runs the handler, and hands the resulting
// effect to the propagation path.
func Execute(host Host, cl *Client, args [][]byte) {
	if len(args) == 0 {
		return
	}
	host.Stats().TotalCommands.Add(1)

	// A client inside MULTI queues almost everything. The commands that
	// control the transaction itself are the exception, and they are marked
	// NoMulti rather than listed here, so a new one cannot be added without
	// deciding which it is.
	if cl.InMulti() {
		if d, _ := host.Commands().Lookup(args[0]); d == nil || !d.Is(NoMulti) {
			cl.Out.WriteValue(queueCommand(host, cl, args))
			return
		}
	}

	d, reply := resolve(host, cl, args)
	if d == nil {
		cl.Out.WriteValue(reply)
		return
	}

	ctx := &cl.ctx
	*ctx = Ctx{Host: host, Client: cl, Cmd: d, Args: args}
	cl.setLastCommand(d)

	cfg := host.Config().Snapshot()

	if !d.Is(Write) {
		cl.Out.WriteValue(runAndPropagate(host, cl, ctx, d, args, cfg))
		return
	}

	ks := host.Keyspace()

	// Two locks, both spanning the handler and the propagation of its
	// effect, and both released before the reply is written -- that write
	// can block on a slow client, and neither a snapshot nor another write
	// should wait for one.
	//
	// The ordering lock makes the log record effects in the order they were
	// applied. Without it two writes to the same key can mutate in one
	// order and reach the log in the other, and the dataset a restart
	// rebuilds is then not the one that was running. See engine/order.go.
	//
	// The cross-shard guard keeps an effect that reads one key and writes
	// another out of a snapshot window, where the per-shard recovery filter
	// could not replay it safely. See Keyspace.SnapshotWindow.
	var locks writeLocks
	if d.Locality == LocalityCrossShard {
		locks = writeLocks{ks: ks, cross: true}
	} else {
		locks = writeLocks{ks: ks, db: cl.DBIndex, keys: ExtractKeys(d, args)}
	}
	locks.acquire()
	// A blocking command gives the locks back while it waits, through
	// Ctx.Yield, and takes them again before it returns.
	ctx.locks = &locks
	result := runAndPropagate(host, cl, ctx, d, args, cfg)
	locks.release()
	cl.Out.WriteValue(result)
}

// writeLocks is the pair of locks a write holds across its execution and the
// propagation of its effect.
//
// It is a value rather than two lines of dispatch because a blocking command
// has to release and retake exactly the same set, and getting that set wrong
// in one of the two places would be a deadlock or a silent ordering bug.
type writeLocks struct {
	ks    *engine.Keyspace
	db    int
	keys  [][]byte
	cross bool
	order engine.WriteOrder
	held  bool
}

func (w *writeLocks) acquire() {
	if w.cross {
		w.ks.BeginCrossShard()
		w.order = w.ks.OrderAllWrites()
	} else {
		w.order = w.ks.DB(w.db).OrderWrites(w.keys)
	}
	w.held = true
}

func (w *writeLocks) release() {
	if !w.held {
		return
	}
	w.order.Done()
	if w.cross {
		w.ks.EndCrossShard()
	}
	w.held = false
}

// runAndPropagate executes the handler, records its cost and hands its
// effect to the propagation path. It returns the reply rather than writing
// it, so that the caller can drop any lock it holds first.
func runAndPropagate(host Host, cl *Client, ctx *Ctx, d *Descriptor,
	args [][]byte, cfg *config.Values) resp.Value {

	// Reading the clock is not free -- on a host without a vDSO fast path it
	// can cost more than a whole GET -- so the two reads are skipped
	// entirely when latency tracking is off. Call and error counts are
	// atomic adds and are always kept.
	if !cfg.TrackCommandLatency {
		result := d.Handler(ctx)
		host.Commands().recordUntimed(d, result.IsError())
		propagate(host, cl, ctx, d, args)
		return result
	}

	start := time.Now()
	result := d.Handler(ctx)
	elapsed := time.Since(start)

	host.Commands().record(d, elapsed, result.IsError())
	if threshold := cfg.SlowlogLogSlowerThan; threshold >= 0 &&
		elapsed.Microseconds() >= threshold {
		host.Stats().Slowlog.Add(d.FullName(), cl.Addr, args, elapsed)
	}

	propagate(host, cl, ctx, d, args)
	return result
}

// resolve finds the descriptor for a command and applies every check that
// must pass before the handler runs. It returns a nil descriptor and the
// error reply when the command must not execute.
func resolve(host Host, cl *Client, args [][]byte) (*Descriptor, resp.Value) {
	table := host.Commands()

	d, ok := table.Lookup(args[0])
	if !ok {
		if table.Disabled(args[0]) {
			return nil, errDisabledCommand(args[0])
		}
		host.Stats().UnsupportedCommands.Add(1)
		return nil, errUnknownCommand(args[0], args[1:])
	}

	// A command with subcommands dispatches on its first argument. Some,
	// such as COMMAND, also have a handler of their own for the bare form.
	if len(d.Subcommands) > 0 {
		switch {
		case len(args) >= 2:
			sub, ok := table.LookupSub(d, args[1])
			if !ok {
				table.recordRejected(d)
				return nil, errUnknownSubcommand(d.Name, args[1])
			}
			d = sub
		case d.Handler == nil:
			table.recordRejected(d)
			return nil, errWrongArgs(d.Name)
		}
	}

	if !arityOK(d, len(args)) {
		table.recordRejected(d)
		if d.parent != nil {
			return nil, errUnknownSubcommand(d.parent.Name, args[1])
		}
		return nil, errWrongArgs(d.Name)
	}

	cfg := host.Config().Snapshot()

	if cfg.RequirePass != "" && !cl.Authenticated && !d.Is(NoAuth) {
		table.recordRejected(d)
		return nil, errNoAuth
	}
	if host.IsLoading() && !d.Is(Loading) {
		table.recordRejected(d)
		return nil, errLoading
	}
	// A RESP2 subscriber can only speak a handful of commands, because the
	// connection is carrying pushes the client has no way to tell apart
	// from replies. RESP3 gives pushes their own type, so the restriction
	// exists only for RESP2 -- and lifting it there would make every reply
	// ambiguous.
	if cl.Subscribed() && cl.Protocol() == resp.RESP2 && !d.Is(SubscriberOK) {
		table.recordRejected(d)
		return nil, errSubscriberMode(d.FullName())
	}
	if d.Is(Write) {
		if host.IsReplica() && cfg.ReplicaReadOnly && !cl.Replica {
			table.recordRejected(d)
			return nil, errReadOnly
		}
		// A write that cannot reach the log must not be acknowledged.
		// Accepting it would report success for data that will not survive
		// a restart, which is worse than an outage because it looks fine.
		if err := host.PersistenceError(); err != nil {
			table.recordRejected(d)
			return nil, errMisconf(err)
		}
		// min-replicas-to-write trades availability for a bound on how much
		// an operator can lose if this leader is lost. Refusing is the whole
		// point of the directive, so it is refused loudly and with the
		// numbers in the message.
		if need := cfg.MinReplicasToWrite; need > 0 {
			if have := host.ReplicasInSync(); have < need {
				table.recordRejected(d)
				return nil, errNotEnoughReplicas(have, need)
			}
		}
		if d.Is(DenyOOM) && overMemoryLimit(host, cfg.MaxMemory, cfg.MaxMemoryPolicy) {
			table.recordRejected(d)
			return nil, errOOM
		}
	}
	return d, resp.Value{}
}

// overMemoryLimit reports whether a memory-growing write must be refused.
//
// Under a policy other than noeviction the eviction sampler is expected to
// make room, so the write proceeds; under noeviction the write is refused and
// reads keep working (FR-6.3).
func overMemoryLimit(host Host, maxMemory int64, policy string) bool {
	if maxMemory <= 0 || policy != "noeviction" {
		return false
	}
	return host.Keyspace().MemoryEstimate() >= maxMemory
}

func arityOK(d *Descriptor, n int) bool {
	if d.Arity >= 0 {
		return n == d.Arity
	}
	return n >= -d.Arity
}

// propagate hands the command's effect to the log and the replication
// stream. A write that changed nothing propagates nothing.
func propagate(host Host, cl *Client, ctx *Ctx, d *Descriptor, args [][]byte) {
	if !d.Is(Write) || ctx.suppress {
		return
	}
	if len(ctx.effects) > 0 {
		for _, e := range ctx.effects {
			host.Propagate(cl.DBIndex, e...)
		}
		return
	}
	if ctx.dirty == 0 {
		return
	}
	if d.Effect == EffectCanonical {
		// The handler reported a change but produced no replay-safe effect.
		// Logging the arguments verbatim would be the silent-divergence bug
		// ADR-008 exists to prevent, and dropping the effect would lose the
		// write, so neither fallback is acceptable.
		panic("command: " + d.FullName() + " modified the keyspace without " +
			"propagating a canonical effect; see ADR-008")
	}
	// Verbatim propagation. The arguments alias the connection's read
	// buffer, so they are copied before they leave the command's lifetime.
	cp := make([][]byte, len(args))
	for i, a := range args {
		b := make([]byte, len(a))
		copy(b, a)
		cp[i] = b
	}
	host.Propagate(cl.DBIndex, cp...)
}
