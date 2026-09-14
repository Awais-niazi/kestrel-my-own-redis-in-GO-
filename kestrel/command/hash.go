package command

import (
	"strings"

	"kestrel/engine"
	"kestrel/resp"
)

var effHSET = []byte("HSET")

func init() {
	register(&Descriptor{
		Name: "HSET", Arity: -4, Flags: Write | DenyOOM | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "hash", "fast"},
		Summary:    "Creates or modifies the value of a field in a hash.",
		Handler:    cmdHSet,
	})
	register(&Descriptor{
		Name: "HMSET", Arity: -4, Flags: Write | DenyOOM | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "hash", "fast"},
		Summary:    "Sets the values of multiple fields. Deprecated alias of HSET.",
		Handler:    cmdHSet,
	})
	register(&Descriptor{
		Name: "HSETNX", Arity: 4, Flags: Write | DenyOOM | Fast, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "hash", "fast"},
		Summary:    "Sets the value of a field in a hash only when the field doesn't exist.",
		Handler:    cmdHSetNX,
	})
	register(&Descriptor{
		Name: "HGET", Arity: 3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "hash", "fast"},
		Summary:    "Returns the value of a field in a hash.",
		Handler:    cmdHGet,
	})
	register(&Descriptor{
		Name: "HMGET", Arity: -3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "hash", "fast"},
		Summary:    "Returns the values of multiple fields in a hash.",
		Handler:    cmdHMGet,
	})
	register(&Descriptor{
		Name: "HDEL", Arity: -3, Flags: Write | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "hash", "fast"},
		Summary:    "Deletes one or more fields and their values from a hash.",
		Handler:    cmdHDel,
	})
	register(&Descriptor{
		Name: "HLEN", Arity: 2, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "hash", "fast"},
		Summary:    "Returns the number of fields in a hash.",
		Handler:    cmdHLen,
	})
	register(&Descriptor{
		Name: "HEXISTS", Arity: 3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "hash", "fast"},
		Summary:    "Determines whether a field exists in a hash.",
		Handler:    cmdHExists,
	})
	register(&Descriptor{
		Name: "HSTRLEN", Arity: 3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "hash", "fast"},
		Summary:    "Returns the length of the value of a field.",
		Handler:    cmdHStrLen,
	})
	for _, r := range []struct {
		name    string
		part    engine.HashPart
		summary string
	}{
		{"HKEYS", engine.HashFields, "Returns all fields in a hash."},
		{"HVALS", engine.HashValues, "Returns all values in a hash."},
		{"HGETALL", engine.HashAll, "Returns all fields and values in a hash."},
	} {
		part, isGetAll := r.part, r.name == "HGETALL"
		register(&Descriptor{
			Name: r.name, Arity: 2, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"read", "hash", "slow"},
			Summary:    r.summary,
			Handler: func(c *Ctx) resp.Value {
				return hashRead(c, part, isGetAll)
			},
		})
	}
	register(&Descriptor{
		Name: "HINCRBY", Arity: 4, Flags: Write | DenyOOM | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "hash", "fast"},
		Summary:    "Increments the integer value of a field by a number.",
		Handler:    cmdHIncrBy,
	})
	register(&Descriptor{
		Name: "HINCRBYFLOAT", Arity: 4, Flags: Write | DenyOOM | Fast, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "hash", "fast"},
		Summary:    "Increments the floating point value of a field by a number.",
		Handler:    cmdHIncrByFloat,
	})
	register(&Descriptor{
		Name: "HRANDFIELD", Arity: -2, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "hash", "slow"},
		Summary:    "Returns one or more random fields from a hash.",
		Handler:    cmdHRandField,
	})
	register(&Descriptor{
		Name: "HSCAN", Arity: -3, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "hash", "slow"},
		Summary:    "Iterates over the fields and values of a hash.",
		Handler:    cmdHScan,
	})
}

func cmdHSet(c *Ctx) resp.Value {
	pairs := c.Tail(2)
	if len(pairs) == 0 || len(pairs)%2 != 0 {
		return errWrongArgs(strings.ToLower(string(c.Name())))
	}
	added, err := c.DB().HSet(c.Arg(1), pairs)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(len(pairs) / 2)
	// HMSET is an alias that replies +OK instead of the new-field count.
	if strings.EqualFold(string(c.Name()), "hmset") {
		return resp.OK()
	}
	return resp.Int(added)
}

func cmdHSetNX(c *Ctx) resp.Value {
	ok, err := c.DB().HSetNX(c.Arg(1), c.Arg(2), c.Arg(3))
	if err != nil {
		return engineError(err)
	}
	if ok {
		c.Dirty(1)
		// The conditional form reaches the log unconditionally: on replay
		// the field may already exist and the condition would then evaluate
		// differently.
		c.Propagate(effHSET, c.Arg(1), c.Arg(2), c.Arg(3))
	}
	return resp.Int(b2i(ok))
}

func cmdHGet(c *Ctx) resp.Value {
	v, ok, err := c.DB().HGet(c.Arg(1), c.Arg(2))
	if err != nil {
		return engineError(err)
	}
	return bulkOrNil(v, ok)
}

func cmdHMGet(c *Ctx) resp.Value {
	vals, err := c.DB().HMGet(c.Arg(1), c.Tail(2))
	if err != nil {
		return engineError(err)
	}
	return bulkArrayWithNils(vals)
}

func cmdHDel(c *Ctx) resp.Value {
	n, err := c.DB().HDel(c.Arg(1), c.Tail(2))
	if err != nil {
		return engineError(err)
	}
	c.Dirty(int(n))
	return resp.Int(n)
}

func cmdHLen(c *Ctx) resp.Value {
	n, err := c.DB().HLen(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	return resp.Int(n)
}

func cmdHExists(c *Ctx) resp.Value {
	ok, err := c.DB().HExists(c.Arg(1), c.Arg(2))
	if err != nil {
		return engineError(err)
	}
	return resp.Int(b2i(ok))
}

func cmdHStrLen(c *Ctx) resp.Value {
	n, err := c.DB().HStrLen(c.Arg(1), c.Arg(2))
	if err != nil {
		return engineError(err)
	}
	return resp.Int(n)
}

// hashRead serves HKEYS, HVALS and HGETALL.
//
// HGETALL is one of the commands whose reply shape genuinely differs between
// protocols (ADR-016): a map under RESP3, a flat array under RESP2. Building
// it as a map and letting the writer flatten it for RESP2 means the handler
// does not need to know which protocol the client speaks.
func hashRead(c *Ctx, part engine.HashPart, asMap bool) resp.Value {
	out, err := c.DB().HRead(c.Arg(1), part)
	if err != nil {
		return engineError(err)
	}
	vals := make([]resp.Value, len(out))
	for i, v := range out {
		vals[i] = resp.Bulk(v)
	}
	if asMap {
		return resp.Map(vals)
	}
	return resp.ArrayOf(vals)
}

func cmdHIncrBy(c *Ctx) resp.Value {
	delta, err := resp.ParseInt(c.Arg(3))
	if err != nil {
		return errNotInteger
	}
	n, err := c.DB().HIncrBy(c.Arg(1), c.Arg(2), delta)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	return resp.Int(n)
}

func cmdHIncrByFloat(c *Ctx) resp.Value {
	delta, err := engine.ParseFloat(c.Arg(3))
	if err != nil {
		return errNotFloat
	}
	_, rendered, err := c.DB().HIncrByFloat(c.Arg(1), c.Arg(2), delta)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	// The computed result is logged, not the increment, so replay cannot
	// drift (ADR-008).
	c.Propagate(effHSET, c.Arg(1), c.Arg(2), rendered)
	return resp.Bulk(rendered)
}

func cmdHRandField(c *Ctx) resp.Value {
	if c.Len() > 4 {
		return errSyntax
	}
	// With no count, the reply is a single field rather than an array.
	if c.Len() == 2 {
		out, err := c.DB().HRandField(c.Arg(1), 1, false)
		if err != nil {
			return engineError(err)
		}
		if len(out) == 0 {
			return resp.Null()
		}
		return resp.Bulk(out[0])
	}
	count, err := resp.ParseInt(c.Arg(2))
	if err != nil {
		return errNotInteger
	}
	withValues := false
	if c.Len() == 4 {
		if !strings.EqualFold(string(c.Arg(3)), "withvalues") {
			return errSyntax
		}
		withValues = true
	}
	out, err := c.DB().HRandField(c.Arg(1), int(count), withValues)
	if err != nil {
		return engineError(err)
	}
	if !withValues {
		return bulkArray(out)
	}
	return pairedArray(c, out)
}

// pairedArray renders field/value pairs the way each protocol expects: a
// flat array under RESP2, an array of two-element arrays under RESP3. This
// is the other shape ADR-016 calls out as needing testing twice.
func pairedArray(c *Ctx, flat [][]byte) resp.Value {
	if c.Client.Protocol() < resp.RESP3 {
		return bulkArray(flat)
	}
	out := make([]resp.Value, 0, len(flat)/2)
	for i := 0; i+1 < len(flat); i += 2 {
		out = append(out, resp.Array(resp.Bulk(flat[i]), resp.Bulk(flat[i+1])))
	}
	return resp.ArrayOf(out)
}

func cmdHScan(c *Ctx) resp.Value {
	cursor, opts, reply, ok := parseScanArgs(c, 3, true)
	if !ok {
		return reply
	}
	_ = cursor
	out, err := c.DB().HRead(c.Arg(1), engine.HashAll)
	if err != nil {
		return engineError(err)
	}
	entries := make([]resp.Value, 0, len(out))
	for i := 0; i+1 < len(out); i += 2 {
		if opts.Match != nil && !engine.MatchPattern(opts.Match, out[i]) {
			continue
		}
		entries = append(entries, resp.Bulk(out[i]))
		if !opts.NoValues {
			entries = append(entries, resp.Bulk(out[i+1]))
		}
	}
	return resp.Array(resp.BulkString("0"), resp.ArrayOf(entries))
}
