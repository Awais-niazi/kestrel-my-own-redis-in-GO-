package command

import (
	"strings"

	"kestrel/engine"
	"kestrel/resp"
)

func init() {
	for _, name := range []string{"DEL", "UNLINK"} {
		summary := "Deletes one or more keys."
		if name == "UNLINK" {
			summary = "Deletes one or more keys. Synchronous in this version; see Appendix A."
		}
		register(&Descriptor{
			Name: name, Arity: -2, Flags: Write | Fast, Effect: EffectVerbatim,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: -1, Step: 1,
			Categories: []string{"keyspace", "write", "fast"},
			Summary:    summary,
			Handler:    cmdDel,
		})
	}
	register(&Descriptor{
		Name: "EXISTS", Arity: -2, Flags: Readonly | Fast, FirstKey: 1, LastKey: -1, Step: 1,
		Categories: []string{"keyspace", "read", "fast"},
		Summary:    "Determines whether one or more keys exist.",
		Handler:    cmdExists,
	})
	register(&Descriptor{
		Name: "TOUCH", Arity: -2, Flags: Readonly | Fast, FirstKey: 1, LastKey: -1, Step: 1,
		Categories: []string{"keyspace", "read", "fast"},
		Summary:    "Returns the number of existing keys out of those specified.",
		Handler:    cmdExists,
	})
	register(&Descriptor{
		Name: "TYPE", Arity: 2, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"keyspace", "read", "fast"},
		Summary:    "Determines the type of value stored at a key.",
		Handler:    cmdType,
	})
	register(&Descriptor{
		Name: "RENAME", Arity: 3, Flags: Write, Effect: EffectVerbatim,
		Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"keyspace", "write", "slow"},
		Summary:    "Renames a key and overwrites the destination.",
		Handler:    cmdRename,
	})
	register(&Descriptor{
		Name: "RENAMENX", Arity: 3, Flags: Write | Fast, Effect: EffectCanonical,
		Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"keyspace", "write", "fast"},
		Summary:    "Renames a key only when the target key name doesn't exist.",
		Handler:    cmdRename,
	})
	register(&Descriptor{
		Name: "COPY", Arity: -3, Flags: Write | DenyOOM, Effect: EffectCanonical,
		Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"keyspace", "write", "slow"},
		Summary:    "Copies the value of a key to a new key.",
		Handler:    cmdCopy,
	})
	register(&Descriptor{
		Name: "KEYS", Arity: 2, Flags: Readonly, FirstKey: 0,
		Categories: []string{"keyspace", "read", "slow", "dangerous"},
		Summary:    "Returns all key names matching a pattern. O(n) and blocking.",
		Handler:    cmdKeys,
	})
	register(&Descriptor{
		Name: "SCAN", Arity: -2, Flags: Readonly, FirstKey: 0,
		Categories: []string{"keyspace", "read", "slow"},
		Summary:    "Iterates over the key names in the database.",
		Handler:    cmdScan,
	})
	register(&Descriptor{
		Name: "RANDOMKEY", Arity: 1, Flags: Readonly, FirstKey: 0,
		Categories: []string{"keyspace", "read", "slow"},
		Summary:    "Returns a random key name from the database.",
		Handler:    cmdRandomKey,
	})
	register(&Descriptor{
		Name: "DBSIZE", Arity: 1, Flags: Readonly | Fast, FirstKey: 0,
		Categories: []string{"keyspace", "read", "fast"},
		Summary:    "Returns the number of keys in the database.",
		Handler:    cmdDBSize,
	})

	// The four spellings of "set a TTL" differ only in unit and frame of
	// reference. They share a handler, and all four reach the log as an
	// absolute PEXPIREAT, because a relative expiry replayed later would set
	// the wrong instant (ADR-008).
	for _, e := range []struct {
		name    string
		unit    string
		summary string
	}{
		{"EXPIRE", "EX", "Sets the expiration time of a key in seconds."},
		{"PEXPIRE", "PX", "Sets the expiration time of a key in milliseconds."},
		{"EXPIREAT", "EXAT", "Sets the expiration time of a key to a Unix timestamp in seconds."},
		{"PEXPIREAT", "PXAT", "Sets the expiration time of a key to a Unix timestamp in milliseconds."},
	} {
		unit := e.unit
		register(&Descriptor{
			Name: e.name, Arity: -3, Flags: Write | Fast, Effect: EffectCanonical,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"keyspace", "write", "fast"},
			Summary:    e.summary,
			Handler:    func(c *Ctx) resp.Value { return cmdExpire(c, unit) },
		})
	}
	register(&Descriptor{
		Name: "PERSIST", Arity: 2, Flags: Write | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"keyspace", "write", "fast"},
		Summary:    "Removes the expiration time of a key.",
		Handler:    cmdPersist,
	})
	for _, e := range []struct {
		name     string
		divisor  int64
		absolute bool
		summary  string
	}{
		{"TTL", 1000, false, "Returns the expiration time in seconds of a key."},
		{"PTTL", 1, false, "Returns the expiration time in milliseconds of a key."},
		{"EXPIRETIME", 1000, true, "Returns the expiration Unix timestamp in seconds of a key."},
		{"PEXPIRETIME", 1, true, "Returns the expiration Unix timestamp in milliseconds of a key."},
	} {
		divisor, absolute := e.divisor, e.absolute
		register(&Descriptor{
			Name: e.name, Arity: 2, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"keyspace", "read", "fast"},
			Summary:    e.summary,
			Handler:    func(c *Ctx) resp.Value { return cmdTTL(c, divisor, absolute) },
		})
	}
}

func cmdDel(c *Ctx) resp.Value {
	n := c.DB().Del(c.Tail(1))
	c.Dirty(int(n))
	return resp.Int(n)
}

func cmdExists(c *Ctx) resp.Value { return resp.Int(c.DB().Exists(c.Tail(1))) }

// typeReplies is indexed by ObjectType, with the "none" answer last, so TYPE
// never builds a reply string.
var typeReplies = [...]resp.Value{
	resp.Simple("string"), resp.Simple("list"), resp.Simple("hash"),
	resp.Simple("set"), resp.Simple("zset"), resp.Simple("none"),
}

func cmdType(c *Ctx) resp.Value {
	t, ok := c.DB().Type(c.Arg(1))
	if !ok || int(t) >= len(typeReplies)-1 {
		return typeReplies[len(typeReplies)-1]
	}
	return typeReplies[t]
}

// RENAME and COPY read one key and write another. When those keys live in
// different shards, the effect is not safe to replay against a fuzzy
// snapshot whose shards were serialized at different offsets: see issue 1 in
// docs/design-notes.md.
//
// Both declare LocalityCrossShard, so the dispatcher keeps their effects out
// of the snapshot window entirely and the operation stays safe to propagate
// as written, rather than being materialized into a pure write.
func cmdRename(c *Ctx) resp.Value {
	nx := strings.EqualFold(string(c.Name()), "renamenx")
	ok, err := c.DB().Rename(c.Arg(1), c.Arg(2), nx)
	if err != nil {
		return engineError(err)
	}
	if !nx {
		c.Dirty(1)
		return resp.OK()
	}
	if ok {
		c.Dirty(1)
		// The conditional form reaches the log unconditionally: on replay
		// the destination may already exist and the condition would then
		// evaluate differently.
		c.Propagate([]byte("RENAME"), c.Arg(1), c.Arg(2))
	}
	return resp.Int(b2i(ok))
}

func cmdCopy(c *Ctx) resp.Value {
	dest := c.DB()
	replace := false
	for i := 3; i < c.Len(); i++ {
		switch strings.ToUpper(string(c.Arg(i))) {
		case "REPLACE":
			replace = true
		case "DB":
			if i+1 >= c.Len() {
				return errSyntax
			}
			n, err := resp.ParseInt(c.Arg(i + 1))
			if err != nil {
				return errNotInteger
			}
			dest = c.Host.Keyspace().DB(int(n))
			if dest == nil {
				return errDBIndex
			}
			i++
		default:
			return errSyntax
		}
	}
	if dest == c.DB() && string(c.Arg(1)) == string(c.Arg(2)) {
		return resp.Err("ERR source and destination objects are the same")
	}
	ok := c.DB().Copy(c.Arg(1), c.Arg(2), dest, replace)
	if ok {
		c.Dirty(1)
		// Logged with REPLACE so that a replay onto a base where the
		// destination already exists produces the same state.
		args := [][]byte{[]byte("COPY"), c.Arg(1), c.Arg(2)}
		if dest != c.DB() {
			args = append(args, []byte("DB"), itob(int64(dest.Index)))
		}
		c.Propagate(append(args, []byte("REPLACE"))...)
	}
	return resp.Int(b2i(ok))
}

func cmdKeys(c *Ctx) resp.Value {
	keys := c.DB().Keys(c.Arg(1))
	out := make([]resp.Value, len(keys))
	for i, k := range keys {
		out[i] = resp.Bulk(k)
	}
	return resp.ArrayOf(out)
}

func cmdScan(c *Ctx) resp.Value {
	cursor, err := resp.ParseInt(c.Arg(1))
	if err != nil || cursor < 0 {
		return resp.Err("ERR invalid cursor")
	}
	var opts engine.ScanOptions
	for i := 2; i < c.Len(); i++ {
		switch strings.ToUpper(string(c.Arg(i))) {
		case "MATCH":
			if i+1 >= c.Len() {
				return errSyntax
			}
			opts.Match = c.Arg(i + 1)
			i++
		case "COUNT":
			if i+1 >= c.Len() {
				return errSyntax
			}
			n, err := resp.ParseInt(c.Arg(i + 1))
			if err != nil || n < 1 {
				return errSyntax
			}
			opts.Count = int(n)
			i++
		case "TYPE":
			if i+1 >= c.Len() {
				return errSyntax
			}
			t, ok := parseType(c.Arg(i + 1))
			if !ok {
				// An unknown type matches nothing, which is what the
				// reference implementation does.
				return resp.Array(resp.BulkString("0"), resp.EmptyArray())
			}
			opts.Type, opts.TypeFilter = t, true
			i++
		default:
			return errSyntax
		}
	}
	next, keys := c.DB().Scan(uint64(cursor), opts)
	out := make([]resp.Value, len(keys))
	for i, k := range keys {
		out[i] = resp.Bulk(k)
	}
	return resp.Array(resp.Bulk(itob(int64(next))), resp.ArrayOf(out))
}

func parseType(b []byte) (engine.ObjectType, bool) {
	switch strings.ToLower(string(b)) {
	case "string":
		return engine.TypeString, true
	case "list":
		return engine.TypeList, true
	case "hash":
		return engine.TypeHash, true
	case "set":
		return engine.TypeSet, true
	case "zset":
		return engine.TypeZSet, true
	default:
		return 0, false
	}
}

func cmdRandomKey(c *Ctx) resp.Value {
	k := c.DB().RandomKey()
	if k == nil {
		return resp.Null()
	}
	return resp.Bulk(k)
}

func cmdDBSize(c *Ctx) resp.Value { return resp.Int(c.DB().Size()) }

func cmdExpire(c *Ctx, unit string) resp.Value {
	if c.Len() > 4 {
		return errWrongArgs(strings.ToLower(string(c.Name())))
	}
	var flags engine.ExpireFlags
	if c.Len() == 4 {
		switch strings.ToUpper(string(c.Arg(3))) {
		case "NX":
			flags = engine.ExpireNX
		case "XX":
			flags = engine.ExpireXX
		case "GT":
			flags = engine.ExpireGT
		case "LT":
			flags = engine.ExpireLT
		default:
			return resp.Err("ERR Unsupported option " + string(c.Arg(3)))
		}
	}
	at, reply, ok := absoluteExpiry(unit, c.Arg(2), c.Now(), strings.ToLower(string(c.Name())))
	if !ok {
		return reply
	}
	applied, deleted := c.DB().Expire(c.Arg(1), at, flags)
	if applied {
		c.Dirty(1)
		if deleted {
			c.Propagate(effDEL, c.Arg(1))
		} else {
			c.Propagate(effPEXPIREAT, c.Arg(1), itob(at))
		}
	}
	return resp.Int(b2i(applied))
}

func cmdPersist(c *Ctx) resp.Value {
	ok := c.DB().Persist(c.Arg(1))
	if ok {
		c.Dirty(1)
	}
	return resp.Int(b2i(ok))
}

func cmdTTL(c *Ctx, divisor int64, absolute bool) resp.Value {
	var v int64
	if absolute {
		v = c.DB().ExpireTime(c.Arg(1))
	} else {
		v = c.DB().PTTL(c.Arg(1))
	}
	if v < 0 {
		return resp.Int(v) // -2 no key, -1 no expiry
	}
	if divisor == 1000 {
		// Round up, so that a key with 1500 ms left reports 2 s rather than
		// appearing to expire sooner than it will.
		v = (v + 999) / 1000
	}
	return resp.Int(v)
}
