package command

import (
	"time"

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

	d, reply := resolve(host, cl, args)
	if d == nil {
		cl.Out.WriteValue(reply)
		return
	}

	ctx := &cl.ctx
	*ctx = Ctx{Host: host, Client: cl, Cmd: d, Args: args}
	cl.setLastCommand(d)

	// Reading the clock is not free -- on a host without a vDSO fast path it
	// can cost more than a whole GET -- so the two reads are skipped
	// entirely when latency tracking is off. Call and error counts are
	// atomic adds and are always kept.
	cfg := host.Config().Snapshot()
	if !cfg.TrackCommandLatency {
		result := d.Handler(ctx)
		host.Commands().recordUntimed(d, result.IsError())
		propagate(host, cl, ctx, d, args)
		cl.Out.WriteValue(result)
		return
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
	cl.Out.WriteValue(result)
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
	if d.Is(Write) {
		if host.IsReplica() && cfg.ReplicaReadOnly && !cl.Replica {
			table.recordRejected(d)
			return nil, errReadOnly
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
