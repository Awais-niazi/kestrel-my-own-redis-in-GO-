package command

import (
	"strings"

	"kestrel/resp"
)

func init() {
	for _, r := range []struct {
		name      string
		front     bool
		mustExist bool
		summary   string
	}{
		{"LPUSH", true, false, "Prepends one or more elements to a list, creating it if it doesn't exist."},
		{"RPUSH", false, false, "Appends one or more elements to a list, creating it if it doesn't exist."},
		{"LPUSHX", true, true, "Prepends one or more elements to a list only when the list exists."},
		{"RPUSHX", false, true, "Appends one or more elements to a list only when the list exists."},
	} {
		front, mustExist := r.front, r.mustExist
		register(&Descriptor{
			Name: r.name, Arity: -3, Flags: Write | DenyOOM | Fast, Effect: EffectVerbatim,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"write", "list", "fast"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return listPush(c, front, mustExist) },
		})
	}
	for _, r := range []struct {
		name    string
		front   bool
		summary string
	}{
		{"LPOP", true, "Returns and removes the first elements of a list."},
		{"RPOP", false, "Returns and removes the last elements of a list."},
	} {
		front := r.front
		register(&Descriptor{
			Name: r.name, Arity: -2, Flags: Write | Fast, Effect: EffectVerbatim,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"write", "list", "fast"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return listPop(c, front) },
		})
	}
	register(&Descriptor{
		Name: "LLEN", Arity: 2, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "list", "fast"},
		Summary:    "Returns the length of a list.",
		Handler:    cmdLLen,
	})
	register(&Descriptor{
		Name: "LINDEX", Arity: 3, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "list", "slow"},
		Summary:    "Returns an element from a list by its index.",
		Handler:    cmdLIndex,
	})
	register(&Descriptor{
		Name: "LSET", Arity: 4, Flags: Write | DenyOOM, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "list", "slow"},
		Summary:    "Sets the value of an element in a list by its index.",
		Handler:    cmdLSet,
	})
	register(&Descriptor{
		Name: "LRANGE", Arity: 4, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "list", "slow"},
		Summary:    "Returns a range of elements from a list.",
		Handler:    cmdLRange,
	})
	register(&Descriptor{
		Name: "LINSERT", Arity: 5, Flags: Write | DenyOOM, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "list", "slow"},
		Summary:    "Inserts an element before or after another element in a list.",
		Handler:    cmdLInsert,
	})
	register(&Descriptor{
		Name: "LREM", Arity: 4, Flags: Write, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "list", "slow"},
		Summary:    "Removes elements from a list.",
		Handler:    cmdLRem,
	})
	register(&Descriptor{
		Name: "LTRIM", Arity: 4, Flags: Write, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "list", "slow"},
		Summary:    "Removes elements from both ends a list, keeping only a range.",
		Handler:    cmdLTrim,
	})
	register(&Descriptor{
		Name: "LPOS", Arity: -3, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "list", "slow"},
		Summary:    "Returns the index of matching elements in a list.",
		Handler:    cmdLPos,
	})
	register(&Descriptor{
		Name: "LMOVE", Arity: 5, Flags: Write | DenyOOM, Effect: EffectVerbatim,
		Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"write", "list", "slow"},
		Summary:    "Moves an element from one list to another.",
		Handler:    cmdLMove,
	})
	register(&Descriptor{
		Name: "RPOPLPUSH", Arity: 3, Flags: Write | DenyOOM, Effect: EffectVerbatim,
		Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"write", "list", "slow"},
		Summary:    "Moves the last element of a list to the front of another. Deprecated in favour of LMOVE.",
		Handler:    cmdRPopLPush,
	})
}

func listPush(c *Ctx, front, mustExist bool) resp.Value {
	n, err := c.DB().LPush(c.Arg(1), c.Tail(2), front, mustExist)
	if err != nil {
		return engineError(err)
	}
	if n > 0 {
		c.Dirty(c.Len() - 2)
	}
	return resp.Int(n)
}

func listPop(c *Ctx, front bool) resp.Value {
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
			return errIndexRange
		}
		count = int(n)
	}
	out, err := c.DB().LPop(c.Arg(1), count, front)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(len(out))
	if single {
		if len(out) == 0 {
			return resp.Null()
		}
		return resp.Bulk(out[0])
	}
	if len(out) == 0 {
		return resp.NullArray()
	}
	return bulkArray(out)
}

func cmdLLen(c *Ctx) resp.Value {
	n, err := c.DB().LLen(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	return resp.Int(n)
}

func cmdLIndex(c *Ctx) resp.Value {
	i, err := resp.ParseInt(c.Arg(2))
	if err != nil {
		return errNotInteger
	}
	v, ok, engErr := c.DB().LIndex(c.Arg(1), i)
	if engErr != nil {
		return engineError(engErr)
	}
	return bulkOrNil(v, ok)
}

func cmdLSet(c *Ctx) resp.Value {
	i, err := resp.ParseInt(c.Arg(2))
	if err != nil {
		return errNotInteger
	}
	if err := c.DB().LSet(c.Arg(1), i, c.Arg(3)); err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	return resp.OK()
}

func cmdLRange(c *Ctx) resp.Value {
	start, err1 := resp.ParseInt(c.Arg(2))
	stop, err2 := resp.ParseInt(c.Arg(3))
	if err1 != nil || err2 != nil {
		return errNotInteger
	}
	out, err := c.DB().LRange(c.Arg(1), start, stop)
	if err != nil {
		return engineError(err)
	}
	return bulkArray(out)
}

func cmdLInsert(c *Ctx) resp.Value {
	var before bool
	switch strings.ToUpper(string(c.Arg(2))) {
	case "BEFORE":
		before = true
	case "AFTER":
	default:
		return errSyntax
	}
	n, err := c.DB().LInsert(c.Arg(1), c.Arg(3), c.Arg(4), before)
	if err != nil {
		return engineError(err)
	}
	if n > 0 {
		c.Dirty(1)
	}
	return resp.Int(n)
}

func cmdLRem(c *Ctx) resp.Value {
	count, err := resp.ParseInt(c.Arg(2))
	if err != nil {
		return errNotInteger
	}
	n, engErr := c.DB().LRem(c.Arg(1), int(count), c.Arg(3))
	if engErr != nil {
		return engineError(engErr)
	}
	c.Dirty(int(n))
	return resp.Int(n)
}

func cmdLTrim(c *Ctx) resp.Value {
	start, err1 := resp.ParseInt(c.Arg(2))
	stop, err2 := resp.ParseInt(c.Arg(3))
	if err1 != nil || err2 != nil {
		return errNotInteger
	}
	if err := c.DB().LTrim(c.Arg(1), start, stop); err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	return resp.OK()
}

func cmdLPos(c *Ctx) resp.Value {
	rank, maxlen := 1, 0
	count := -1 // -1 means "no COUNT given": reply with a single index
	for i := 3; i < c.Len(); i++ {
		if i+1 >= c.Len() {
			return errSyntax
		}
		n, err := resp.ParseInt(c.Arg(i + 1))
		if err != nil {
			return errNotInteger
		}
		switch strings.ToUpper(string(c.Arg(i))) {
		case "RANK":
			if n == 0 {
				return resp.Err("ERR RANK can't be zero. Use 1 to start searching " +
					"from the first match, 2 from the second, and so forth.")
			}
			rank = int(n)
		case "COUNT":
			if n < 0 {
				return resp.Err("ERR COUNT can't be negative")
			}
			count = int(n)
		case "MAXLEN":
			if n < 0 {
				return resp.Err("ERR MAXLEN can't be negative")
			}
			maxlen = int(n)
		default:
			return errSyntax
		}
		i++
	}
	// COUNT 0 means "every match".
	want := count
	if count == 0 {
		want = 0
	} else if count < 0 {
		want = 1
	}
	found, err := c.DB().LPos(c.Arg(1), c.Arg(2), rank, want, maxlen)
	if err != nil {
		return engineError(err)
	}
	if count < 0 {
		if len(found) == 0 {
			return resp.Null()
		}
		return resp.Int(int64(found[0]))
	}
	out := make([]resp.Value, len(found))
	for i, idx := range found {
		out[i] = resp.Int(int64(idx))
	}
	return resp.ArrayOf(out)
}

func cmdLMove(c *Ctx) resp.Value {
	srcFront, ok1 := parseSide(c.Arg(3))
	dstFront, ok2 := parseSide(c.Arg(4))
	if !ok1 || !ok2 {
		return errSyntax
	}
	return listMove(c, c.Arg(1), c.Arg(2), srcFront, dstFront)
}

func cmdRPopLPush(c *Ctx) resp.Value {
	return listMove(c, c.Arg(1), c.Arg(2), false, true)
}

func listMove(c *Ctx, src, dst []byte, srcFront, dstFront bool) resp.Value {
	v, err := c.DB().LMove(src, dst, srcFront, dstFront)
	if err != nil {
		return engineError(err)
	}
	if v == nil {
		return resp.Null()
	}
	c.Dirty(1)
	return resp.Bulk(v)
}

func parseSide(b []byte) (front bool, ok bool) {
	switch strings.ToUpper(string(b)) {
	case "LEFT":
		return true, true
	case "RIGHT":
		return false, true
	default:
		return false, false
	}
}
