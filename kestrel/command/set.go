package command

import (
	"strings"

	"kestrel/engine"
	"kestrel/resp"
)

var effSREM = []byte("SREM")

func init() {
	register(&Descriptor{
		Name: "SADD", Arity: -3, Flags: Write | DenyOOM | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "set", "fast"},
		Summary:    "Adds one or more members to a set.",
		Handler:    cmdSAdd,
	})
	register(&Descriptor{
		Name: "SREM", Arity: -3, Flags: Write | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "set", "fast"},
		Summary:    "Removes one or more members from a set.",
		Handler:    cmdSRem,
	})
	register(&Descriptor{
		Name: "SCARD", Arity: 2, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "set", "fast"},
		Summary:    "Returns the number of members in a set.",
		Handler:    cmdSCard,
	})
	register(&Descriptor{
		Name: "SISMEMBER", Arity: 3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "set", "fast"},
		Summary:    "Determines whether a member belongs to a set.",
		Handler:    cmdSIsMember,
	})
	register(&Descriptor{
		Name: "SMISMEMBER", Arity: -3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "set", "fast"},
		Summary:    "Determines whether multiple members belong to a set.",
		Handler:    cmdSMIsMember,
	})
	register(&Descriptor{
		Name: "SMEMBERS", Arity: 2, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "set", "slow"},
		Summary:    "Returns all members of a set.",
		Handler:    cmdSMembers,
	})
	register(&Descriptor{
		Name: "SPOP", Arity: -2, Flags: Write | Fast, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "set", "fast"},
		Summary:    "Returns and removes one or more random members from a set.",
		Handler:    cmdSPop,
	})
	register(&Descriptor{
		Name: "SRANDMEMBER", Arity: -2, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "set", "slow"},
		Summary:    "Returns one or more random members from a set without removing them.",
		Handler:    cmdSRandMember,
	})
	register(&Descriptor{
		Name: "SMOVE", Arity: 4, Flags: Write | Fast, Effect: EffectVerbatim,
		Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"write", "set", "fast"},
		Summary:    "Moves a member from one set to another.",
		Handler:    cmdSMove,
	})
	for _, r := range []struct {
		name    string
		op      engine.SetOp
		summary string
	}{
		{"SINTER", engine.SetInter, "Returns the intersection of multiple sets."},
		{"SUNION", engine.SetUnion, "Returns the union of multiple sets."},
		{"SDIFF", engine.SetDiff, "Returns the difference of multiple sets."},
	} {
		op := r.op
		register(&Descriptor{
			Name: r.name, Arity: -2, Flags: Readonly, FirstKey: 1, LastKey: -1, Step: 1,
			Categories: []string{"read", "set", "slow"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return setCombine(c, op) },
		})
		register(&Descriptor{
			Name: r.name + "STORE", Arity: -3, Flags: Write | DenyOOM, Effect: EffectVerbatim,
			Locality: LocalityCrossShard,
			FirstKey: 1, LastKey: -1, Step: 1,
			Categories: []string{"write", "set", "slow"},
			Summary:    r.summary + " Stores the result in a key.",
			Handler:    func(c *Ctx) resp.Value { return setCombineStore(c, op) },
		})
	}
	register(&Descriptor{
		Name: "SINTERCARD", Arity: -3, Flags: Readonly, FirstKey: 2, LastKey: -1, Step: 1,
		Categories: []string{"read", "set", "slow"},
		Summary:    "Returns the number of members in the intersection of multiple sets.",
		Handler:    cmdSInterCard,
	})
	register(&Descriptor{
		Name: "SSCAN", Arity: -3, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "set", "slow"},
		Summary:    "Iterates over the members of a set.",
		Handler:    cmdSScan,
	})
}

func cmdSAdd(c *Ctx) resp.Value {
	n, err := c.DB().SAdd(c.Arg(1), c.Tail(2))
	if err != nil {
		return engineError(err)
	}
	c.Dirty(int(n))
	return resp.Int(n)
}

func cmdSRem(c *Ctx) resp.Value {
	n, err := c.DB().SRem(c.Arg(1), c.Tail(2))
	if err != nil {
		return engineError(err)
	}
	c.Dirty(int(n))
	return resp.Int(n)
}

func cmdSCard(c *Ctx) resp.Value {
	n, err := c.DB().SCard(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	return resp.Int(n)
}

func cmdSIsMember(c *Ctx) resp.Value {
	ok, err := c.DB().SIsMember(c.Arg(1), c.Arg(2))
	if err != nil {
		return engineError(err)
	}
	return resp.Int(b2i(ok))
}

func cmdSMIsMember(c *Ctx) resp.Value {
	got, err := c.DB().SMIsMember(c.Arg(1), c.Tail(2))
	if err != nil {
		return engineError(err)
	}
	out := make([]resp.Value, len(got))
	for i, v := range got {
		out[i] = resp.Int(b2i(v))
	}
	return resp.ArrayOf(out)
}

func cmdSMembers(c *Ctx) resp.Value {
	members, err := c.DB().SMembers(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	return setReply(c, members)
}

// setReply renders a member list as a set under RESP3 and an array under
// RESP2, which is what the typed reply tree is for (ADR-016).
func setReply(c *Ctx, members [][]byte) resp.Value {
	out := make([]resp.Value, len(members))
	for i, m := range members {
		out[i] = resp.Bulk(m)
	}
	return resp.Set(out)
}

func cmdSPop(c *Ctx) resp.Value {
	single := c.Len() == 2
	count := 1
	if !single {
		if c.Len() != 3 {
			return errSyntax
		}
		n, err := resp.ParseInt(c.Arg(2))
		if err != nil {
			return errNotInteger
		}
		if n < 0 {
			return resp.Err("ERR value is out of range, must be positive")
		}
		count = int(n)
	}
	popped, err := c.DB().SPop(c.Arg(1), count)
	if err != nil {
		return engineError(err)
	}
	if len(popped) > 0 {
		c.Dirty(len(popped))
		// The members were chosen at random, so replaying SPOP would remove
		// different ones. The choice is pinned by logging the removal
		// (ADR-008).
		c.Propagate(append([][]byte{effSREM, c.Arg(1)}, popped...)...)
	}
	if single {
		if len(popped) == 0 {
			return resp.Null()
		}
		return resp.Bulk(popped[0])
	}
	return setReply(c, popped)
}

func cmdSRandMember(c *Ctx) resp.Value {
	if c.Len() == 2 {
		out, err := c.DB().SRandMember(c.Arg(1), 1)
		if err != nil {
			return engineError(err)
		}
		if len(out) == 0 {
			return resp.Null()
		}
		return resp.Bulk(out[0])
	}
	if c.Len() != 3 {
		return errSyntax
	}
	count, err := resp.ParseInt(c.Arg(2))
	if err != nil {
		return errNotInteger
	}
	out, engErr := c.DB().SRandMember(c.Arg(1), int(count))
	if engErr != nil {
		return engineError(engErr)
	}
	return bulkArray(out)
}

func cmdSMove(c *Ctx) resp.Value {
	moved, err := c.DB().SMove(c.Arg(1), c.Arg(2), c.Arg(3))
	if err != nil {
		return engineError(err)
	}
	if moved {
		c.Dirty(1)
	}
	return resp.Int(b2i(moved))
}

func setCombine(c *Ctx, op engine.SetOp) resp.Value {
	members, err := c.DB().SetCombine(op, c.Tail(1), 0)
	if err != nil {
		return engineError(err)
	}
	return setReply(c, members)
}

func setCombineStore(c *Ctx, op engine.SetOp) resp.Value {
	n, err := c.DB().SetCombineStore(op, c.Arg(1), c.Tail(2))
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	return resp.Int(n)
}

func cmdSInterCard(c *Ctx) resp.Value {
	numKeys, err := resp.ParseInt(c.Arg(1))
	if err != nil || numKeys <= 0 {
		return resp.Err("ERR numkeys should be greater than 0")
	}
	if int(numKeys) > c.Len()-2 {
		return resp.Err("ERR Number of keys can't be greater than number of args")
	}
	keys := c.Args[2 : 2+numKeys]
	limit := 0
	if rest := c.Args[2+numKeys:]; len(rest) > 0 {
		if len(rest) != 2 || !strings.EqualFold(string(rest[0]), "limit") {
			return errSyntax
		}
		n, err := resp.ParseInt(rest[1])
		if err != nil || n < 0 {
			return resp.Err("ERR LIMIT can't be negative")
		}
		limit = int(n)
	}
	members, engErr := c.DB().SetCombine(engine.SetInter, keys, limit)
	if engErr != nil {
		return engineError(engErr)
	}
	return resp.Int(int64(len(members)))
}

func cmdSScan(c *Ctx) resp.Value {
	_, opts, reply, ok := parseScanArgs(c, 3, false)
	if !ok {
		return reply
	}
	members, err := c.DB().SMembers(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	out := make([]resp.Value, 0, len(members))
	for _, m := range members {
		if opts.Match != nil && !engine.MatchPattern(opts.Match, m) {
			continue
		}
		out = append(out, resp.Bulk(m))
	}
	return resp.Array(resp.BulkString("0"), resp.ArrayOf(out))
}
