package command

import (
	"strconv"
	"strings"
	"time"

	"kestrel/resp"
)

func init() {
	register(&Descriptor{
		Name: "COMMAND", Arity: -1, Flags: Readonly | Loading | Stale,
		Categories: []string{"slow", "connection"},
		Summary:    "Returns detailed information about all commands.",
		Handler:    cmdCommand,
		Subcommands: map[string]*Descriptor{
			"COUNT": {Arity: 2, Flags: Readonly | Loading | Stale,
				Summary: "Returns the number of commands.", Handler: cmdCommandCount},
			"INFO": {Arity: -2, Flags: Readonly | Loading | Stale,
				Summary: "Returns information about one or more commands.", Handler: cmdCommandInfo},
			"DOCS": {Arity: -2, Flags: Readonly | Loading | Stale,
				Summary: "Returns documentation about one or more commands.", Handler: cmdCommandDocs},
			"GETKEYS": {Arity: -3, Flags: Readonly | Loading | Stale,
				Summary: "Extracts the key names from a command.", Handler: cmdCommandGetKeys},
		},
	})
	register(&Descriptor{
		Name: "CONFIG", Arity: -2, Flags: Readonly | Admin | Loading | Stale,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "A container for server configuration commands.",
		Subcommands: map[string]*Descriptor{
			"GET": {Arity: -3, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Returns configuration parameters.", Handler: cmdConfigGet},
			"SET": {Arity: -4, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Sets configuration parameters.", Handler: cmdConfigSet},
			"RESETSTAT": {Arity: 2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Resets the server statistics.", Handler: cmdConfigResetStat},
			"REWRITE": {Arity: 2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Rewrites the configuration file.", Handler: cmdConfigRewrite},
		},
	})
	register(&Descriptor{
		Name: "INFO", Arity: -1, Flags: Readonly | Loading | Stale,
		Categories: []string{"slow", "dangerous"},
		Summary:    "Returns information and statistics about the server.",
		Handler:    cmdInfo,
	})
	register(&Descriptor{
		Name: "FLUSHDB", Arity: -1, Flags: Write, Effect: EffectCanonical,
		Locality:   LocalityShardLocal,
		Categories: []string{"keyspace", "write", "slow", "dangerous"},
		Summary:    "Removes all keys from the current database.",
		Handler:    cmdFlushDB,
	})
	register(&Descriptor{
		Name: "FLUSHALL", Arity: -1, Flags: Write, Effect: EffectCanonical,
		Locality:   LocalityShardLocal,
		Categories: []string{"keyspace", "write", "slow", "dangerous"},
		Summary:    "Removes all keys from all databases.",
		Handler:    cmdFlushAll,
	})
	register(&Descriptor{
		Name: "TIME", Arity: 1, Flags: Readonly | Fast | Loading | Stale,
		Categories: []string{"fast"},
		Summary:    "Returns the server time.",
		Handler:    cmdTime,
	})
	register(&Descriptor{
		Name: "SHUTDOWN", Arity: -1, Flags: Readonly | Admin | Loading | Stale | NoMulti,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "Synchronously saves the dataset to disk and shuts down the server.",
		Handler:    cmdShutdown,
	})
	register(&Descriptor{
		Name: "SLOWLOG", Arity: -2, Flags: Readonly | Admin | Loading | Stale,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "A container for slow log commands.",
		Subcommands: map[string]*Descriptor{
			"GET": {Arity: -2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Returns the slow log's entries.", Handler: cmdSlowlogGet},
			"LEN": {Arity: 2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Returns the number of entries in the slow log.", Handler: cmdSlowlogLen},
			"RESET": {Arity: 2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Clears all entries from the slow log.", Handler: cmdSlowlogReset},
		},
	})
	register(&Descriptor{
		Name: "MEMORY", Arity: -2, Flags: Readonly | Admin,
		Categories: []string{"admin", "slow"},
		Summary:    "A container for memory diagnostics commands.",
		Subcommands: map[string]*Descriptor{
			"USAGE": {Arity: -3, Flags: Readonly, FirstKey: 2, LastKey: 2, Step: 1,
				Summary: "Estimates the memory usage of a key.", Handler: cmdMemoryUsage},
			"DOCTOR": {Arity: 2, Flags: Readonly | Admin,
				Summary: "Outputs a memory problems report.", Handler: cmdMemoryDoctor},
		},
	})
	register(&Descriptor{
		Name: "OBJECT", Arity: -2, Flags: Readonly,
		Categories: []string{"read", "slow"},
		Summary:    "A container for object introspection commands.",
		Subcommands: map[string]*Descriptor{
			"ENCODING": {Arity: 3, Flags: Readonly, FirstKey: 2, LastKey: 2, Step: 1,
				Summary: "Returns the internal encoding of a key's value.", Handler: cmdObjectEncoding},
			"REFCOUNT": {Arity: 3, Flags: Readonly, FirstKey: 2, LastKey: 2, Step: 1,
				Summary: "Returns the reference count of a value.", Handler: cmdObjectRefcount},
			"FREQ": {Arity: 3, Flags: Readonly, FirstKey: 2, LastKey: 2, Step: 1,
				Summary: "Returns the access frequency of a key.", Handler: cmdObjectFreq},
			"IDLETIME": {Arity: 3, Flags: Readonly, FirstKey: 2, LastKey: 2, Step: 1,
				Summary: "Returns the idle time of a key.", Handler: cmdObjectIdletime},
		},
	})
	register(&Descriptor{
		Name: "DEBUG", Arity: -2, Flags: Readonly | Admin | Loading | Stale,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "A container for debugging commands.",
		Subcommands: map[string]*Descriptor{
			"SLEEP": {Arity: 3, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Blocks the server for the given number of seconds.", Handler: cmdDebugSleep},
			"JMAP": {Arity: 2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Accepted for compatibility; does nothing.", Handler: cmdDebugNoop},
			"SET-ACTIVE-EXPIRE": {Arity: 3, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Enables or disables the active expiry cycle.", Handler: cmdDebugSetActiveExpire},
			"OBJECT": {Arity: 3, Flags: Readonly | Admin, FirstKey: 2, LastKey: 2, Step: 1,
				Summary: "Returns internal details about a key.", Handler: cmdDebugObject},
			"EXPIRE-CYCLE": {Arity: 2, Flags: Readonly | Admin,
				Summary: "Runs one active expiry pass synchronously.", Handler: cmdDebugExpireCycle},
		},
	})
}

func cmdCommand(c *Ctx) resp.Value {
	all := c.Host.Commands().All()
	out := make([]resp.Value, 0, len(all))
	for _, d := range all {
		out = append(out, commandInfoValue(d))
	}
	return resp.ArrayOf(out)
}

func commandInfoValue(d *Descriptor) resp.Value {
	flags := d.FlagNames()
	flagVals := make([]resp.Value, len(flags))
	for i, f := range flags {
		flagVals[i] = resp.Simple(f)
	}
	cats := make([]resp.Value, len(d.Categories))
	for i, cat := range d.Categories {
		cats[i] = resp.Simple("@" + cat)
	}
	subs := make([]resp.Value, 0, len(d.Subcommands))
	for _, sub := range d.Subcommands {
		subs = append(subs, commandInfoValue(sub))
	}
	return resp.Array(
		resp.BulkString(strings.ToLower(d.FullName())),
		resp.Int(int64(d.Arity)),
		resp.ArrayOf(flagVals),
		resp.Int(int64(d.FirstKey)),
		resp.Int(int64(d.LastKey)),
		resp.Int(int64(d.Step)),
		resp.ArrayOf(cats),
		resp.EmptyArray(), // tips
		resp.EmptyArray(), // key specs
		resp.ArrayOf(subs),
	)
}

func cmdCommandCount(c *Ctx) resp.Value { return resp.Int(int64(c.Host.Commands().Count())) }

func cmdCommandInfo(c *Ctx) resp.Value {
	if c.Len() == 2 {
		return cmdCommand(c)
	}
	out := make([]resp.Value, 0, c.Len()-2)
	for _, name := range c.Tail(2) {
		if d, ok := c.Host.Commands().Lookup(name); ok {
			out = append(out, commandInfoValue(d))
		} else {
			out = append(out, resp.NullArray())
		}
	}
	return resp.ArrayOf(out)
}

func cmdCommandDocs(c *Ctx) resp.Value {
	names := c.Tail(2)
	var list []*Descriptor
	if len(names) == 0 {
		list = c.Host.Commands().All()
	} else {
		for _, n := range names {
			if d, ok := c.Host.Commands().Lookup(n); ok {
				list = append(list, d)
			}
		}
	}
	out := make([]resp.Value, 0, len(list)*2)
	for _, d := range list {
		out = append(out, resp.BulkString(strings.ToLower(d.Name)), commandDocValue(d))
	}
	return resp.Map(out)
}

func commandDocValue(d *Descriptor) resp.Value {
	fields := []resp.Value{
		resp.BulkString("summary"), resp.BulkString(d.Summary),
		resp.BulkString("since"), resp.BulkString(Version),
		resp.BulkString("group"), resp.BulkString(primaryGroup(d)),
	}
	if len(d.Subcommands) > 0 {
		subs := make([]resp.Value, 0, len(d.Subcommands)*2)
		for name, sub := range d.Subcommands {
			subs = append(subs, resp.BulkString(strings.ToLower(name)), commandDocValue(sub))
		}
		fields = append(fields, resp.BulkString("subcommands"), resp.Map(subs))
	}
	return resp.Map(fields)
}

func primaryGroup(d *Descriptor) string {
	for _, c := range d.Categories {
		switch c {
		case "string", "list", "hash", "set", "zset", "keyspace", "connection", "admin":
			return c
		}
	}
	return "generic"
}

func cmdCommandGetKeys(c *Ctx) resp.Value {
	d, ok := c.Host.Commands().Lookup(c.Arg(2))
	if !ok {
		return resp.Err("ERR Invalid command specified")
	}
	args := c.Tail(2)
	keys := ExtractKeys(d, args)
	if len(keys) == 0 {
		return resp.Err("ERR The command has no key arguments")
	}
	out := make([]resp.Value, len(keys))
	for i, k := range keys {
		out[i] = resp.Bulk(k)
	}
	return resp.ArrayOf(out)
}

// ExtractKeys returns the key arguments of a command, using the descriptor's
// key positions. It is used by COMMAND GETKEYS, and by the ACL and cluster
// layers as they land.
func ExtractKeys(d *Descriptor, args [][]byte) [][]byte {
	if d.FirstKey <= 0 || d.FirstKey >= len(args) {
		return nil
	}
	last := d.LastKey
	if last < 0 {
		last = len(args) + last
	}
	if last >= len(args) {
		last = len(args) - 1
	}
	step := d.Step
	if step <= 0 {
		step = 1
	}
	var keys [][]byte
	for i := d.FirstKey; i <= last; i += step {
		keys = append(keys, args[i])
	}
	return keys
}

func cmdConfigGet(c *Ctx) resp.Value {
	cfg := c.Host.Config()
	seen := make(map[string]bool)
	out := make([]resp.Value, 0, 8)
	for _, pattern := range c.Tail(2) {
		for _, kv := range cfg.Match(string(pattern)) {
			if seen[kv[0]] {
				continue
			}
			seen[kv[0]] = true
			out = append(out, resp.BulkString(kv[0]), resp.BulkString(kv[1]))
		}
	}
	return resp.Map(out)
}

func cmdConfigSet(c *Ctx) resp.Value {
	args := c.Tail(2)
	if len(args)%2 != 0 {
		return errWrongArgs("config|set")
	}
	cfg := c.Host.Config()
	// Validate every pair before applying any, so a bad value in the middle
	// of a multi-parameter CONFIG SET cannot leave a half-applied change.
	staged := cfg.Snapshot().Clone()
	for i := 0; i < len(args); i += 2 {
		if err := staged.SetParam(string(args[i]), string(args[i+1])); err != nil {
			return resp.Err("ERR CONFIG SET failed - " + err.Error())
		}
	}
	if err := staged.Validate(); err != nil {
		return resp.Err("ERR CONFIG SET failed - " + err.Error())
	}
	for i := 0; i < len(args); i += 2 {
		if err := cfg.Set(string(args[i]), string(args[i+1])); err != nil {
			return resp.Err("ERR CONFIG SET failed - " + err.Error())
		}
	}
	c.Host.ApplyRuntimeConfig()
	return resp.OK()
}

func cmdConfigResetStat(c *Ctx) resp.Value {
	c.Host.Stats().Reset()
	c.Host.Commands().ResetCommandStats()
	// The keyspace hit, miss and eviction counters are part of what INFO
	// stats reports, so they reset with everything else.
	c.Host.Keyspace().ResetStats()
	return resp.OK()
}

func cmdConfigRewrite(c *Ctx) resp.Value {
	return resp.Err("ERR CONFIG REWRITE is not supported in this version: " +
		"the configuration file is never modified by the server.")
}

func cmdInfo(c *Ctx) resp.Value {
	sections := make([]string, 0, c.Len()-1)
	for _, s := range c.Tail(1) {
		sections = append(sections, strings.ToLower(string(s)))
	}
	return resp.Verbatim("txt", c.Host.Info(sections))
}

func cmdFlushDB(c *Ctx) resp.Value {
	if reply, ok := parseFlushMode(c, 1); !ok {
		return reply
	}
	c.DB().Flush()
	c.Dirty(1)
	c.Propagate([]byte("FLUSHDB"))
	return resp.OK()
}

func cmdFlushAll(c *Ctx) resp.Value {
	if reply, ok := parseFlushMode(c, 1); !ok {
		return reply
	}
	c.Host.Keyspace().FlushAll()
	c.Dirty(1)
	c.Propagate([]byte("FLUSHALL"))
	return resp.OK()
}

// parseFlushMode accepts ASYNC and SYNC. Both are honoured as SYNC: there is
// no lazy-free thread in this version, so claiming otherwise would be a lie
// about when memory is actually released.
func parseFlushMode(c *Ctx, from int) (resp.Value, bool) {
	for i := from; i < c.Len(); i++ {
		switch strings.ToUpper(string(c.Arg(i))) {
		case "ASYNC", "SYNC":
		default:
			return errSyntax, false
		}
	}
	return resp.Value{}, true
}

func cmdTime(c *Ctx) resp.Value {
	now := time.Now()
	return resp.Array(
		resp.BulkString(strconv.FormatInt(now.Unix(), 10)),
		resp.BulkString(strconv.FormatInt(int64(now.Nanosecond()/1000), 10)),
	)
}

func cmdShutdown(c *Ctx) resp.Value {
	save := c.Host.Config().Snapshot().AppendOnly
	for _, a := range c.Tail(1) {
		switch strings.ToUpper(string(a)) {
		case "NOSAVE":
			save = false
		case "SAVE":
			save = true
		case "NOW", "FORCE", "ABORT":
			return resp.Err("ERR " + strings.ToUpper(string(a)) +
				" is not supported in this version")
		default:
			return errSyntax
		}
	}
	if err := c.Host.Shutdown(save); err != nil {
		return resp.Err("ERR Errors trying to SHUTDOWN: " + err.Error())
	}
	// A successful SHUTDOWN sends no reply: the connection simply ends.
	c.Client.CloseAfterReply = true
	return resp.None()
}

func cmdSlowlogGet(c *Ctx) resp.Value {
	count := 10
	if c.Len() == 3 {
		n, err := resp.ParseInt(c.Arg(2))
		if err != nil {
			return errNotInteger
		}
		count = int(n)
	}
	entries := c.Host.Stats().Slowlog.Entries(count)
	out := make([]resp.Value, len(entries))
	for i, e := range entries {
		args := make([]resp.Value, len(e.Args))
		for j, a := range e.Args {
			args[j] = resp.BulkString(a)
		}
		out[i] = resp.Array(
			resp.Int(e.ID),
			resp.Int(e.Time.Unix()),
			resp.Int(e.Duration.Microseconds()),
			resp.ArrayOf(args),
			resp.BulkString(e.Addr),
			resp.BulkString(""),
		)
	}
	return resp.ArrayOf(out)
}

func cmdSlowlogLen(c *Ctx) resp.Value { return resp.Int(int64(c.Host.Stats().Slowlog.Len())) }

func cmdSlowlogReset(c *Ctx) resp.Value {
	c.Host.Stats().Slowlog.Reset()
	return resp.OK()
}

func cmdMemoryUsage(c *Ctx) resp.Value {
	n, ok := c.DB().MemoryUsage(c.Arg(2))
	if !ok {
		return resp.Null()
	}
	return resp.Int(n)
}

func cmdMemoryDoctor(c *Ctx) resp.Value {
	est := c.Host.Keyspace().MemoryEstimate()
	limit := c.Host.Config().Snapshot().MaxMemory
	switch {
	case limit == 0:
		return resp.BulkString("No maxmemory is configured, so eviction will never run and " +
			"the process can be OOM-killed. Set maxmemory to 60-70% of the container limit.")
	case est*10 >= limit*9:
		return resp.BulkString("The dataset estimate is within 10% of maxmemory. Note that " +
			"the estimate is approximate (ADR-013) and can drift by around 15%.")
	default:
		return resp.BulkString("Sam, I detected a few issues in this Kestrel instance memory implants:\n\n" +
			"  * Nothing to report.")
	}
}

func cmdObjectEncoding(c *Ctx) resp.Value {
	enc, ok := c.DB().Encoding(c.Arg(2))
	if !ok {
		return errNoSuchKey
	}
	return resp.BulkString(enc.String())
}

// cmdObjectRefcount always reports 1: values are not shared between keys, so
// there is no shared-integer pool to report on.
func cmdObjectRefcount(c *Ctx) resp.Value {
	if _, ok := c.DB().Type(c.Arg(2)); !ok {
		return errNoSuchKey
	}
	return resp.Int(1)
}

func cmdObjectFreq(c *Ctx) resp.Value {
	if !strings.HasSuffix(c.Host.Config().Snapshot().MaxMemoryPolicy, "lfu") {
		return resp.Err("ERR An LFU maxmemory policy is not selected, access frequency not tracked.")
	}
	if _, ok := c.DB().Type(c.Arg(2)); !ok {
		return errNoSuchKey
	}
	return resp.Int(0)
}

func cmdObjectIdletime(c *Ctx) resp.Value {
	if _, ok := c.DB().Type(c.Arg(2)); !ok {
		return errNoSuchKey
	}
	return resp.Int(0)
}

func cmdDebugSleep(c *Ctx) resp.Value {
	secs, err := strconv.ParseFloat(string(c.Arg(2)), 64)
	if err != nil || secs < 0 {
		return errNotFloat
	}
	time.Sleep(time.Duration(secs * float64(time.Second)))
	return resp.OK()
}

func cmdDebugNoop(c *Ctx) resp.Value { return resp.OK() }

func cmdDebugSetActiveExpire(c *Ctx) resp.Value {
	on := string(c.Arg(2)) != "0"
	value := "no"
	if on {
		value = "yes"
	}
	if err := c.Host.Config().Set("active-expire", value); err != nil {
		return resp.Err("ERR " + err.Error())
	}
	return resp.OK()
}

func cmdDebugExpireCycle(c *Ctx) resp.Value {
	n := c.Host.Keyspace().ExpirePass(time.Now().Add(100 * time.Millisecond))
	return resp.Int(int64(n))
}

func cmdDebugObject(c *Ctx) resp.Value {
	enc, ok := c.DB().Encoding(c.Arg(2))
	if !ok {
		return errNoSuchKey
	}
	size, _ := c.DB().MemoryUsage(c.Arg(2))
	return resp.Simple("Value at:0x0 refcount:1 encoding:" + enc.String() +
		" serializedlength:" + strconv.FormatInt(size, 10) + " lru:0 lru_seconds_idle:0")
}
