package command

import (
	"math"
	"strconv"
	"strings"

	"kestrel/engine"
	"kestrel/resp"
)

var (
	effZADD = []byte("ZADD")
	effZREM = []byte("ZREM")
)

func init() {
	register(&Descriptor{
		Name: "ZADD", Arity: -4, Flags: Write | DenyOOM | Fast, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "sortedset", "fast"},
		Summary:    "Adds one or more members to a sorted set, or updates their scores.",
		Handler:    cmdZAdd,
	})
	register(&Descriptor{
		Name: "ZINCRBY", Arity: 4, Flags: Write | DenyOOM | Fast, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "sortedset", "fast"},
		Summary:    "Increments the score of a member in a sorted set.",
		Handler:    cmdZIncrBy,
	})
	register(&Descriptor{
		Name: "ZREM", Arity: -3, Flags: Write | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "sortedset", "fast"},
		Summary:    "Removes one or more members from a sorted set.",
		Handler:    cmdZRem,
	})
	register(&Descriptor{
		Name: "ZSCORE", Arity: 3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "sortedset", "fast"},
		Summary:    "Returns the score of a member in a sorted set.",
		Handler:    cmdZScore,
	})
	register(&Descriptor{
		Name: "ZMSCORE", Arity: -3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "sortedset", "fast"},
		Summary:    "Returns the scores of multiple members in a sorted set.",
		Handler:    cmdZMScore,
	})
	register(&Descriptor{
		Name: "ZCARD", Arity: 2, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "sortedset", "fast"},
		Summary:    "Returns the number of members in a sorted set.",
		Handler:    cmdZCard,
	})
	register(&Descriptor{
		Name: "ZCOUNT", Arity: 4, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "sortedset", "fast"},
		Summary:    "Returns the count of members in a sorted set within a score range.",
		Handler:    cmdZCount,
	})
	register(&Descriptor{
		Name: "ZLEXCOUNT", Arity: 4, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "sortedset", "fast"},
		Summary:    "Returns the count of members in a sorted set within a lexicographical range.",
		Handler:    cmdZLexCount,
	})
	for _, r := range []struct {
		name    string
		by      engine.RangeBy
		rev     bool
		summary string
	}{
		{"ZRANGE", engine.RangeByRank, false, "Returns members in a sorted set within a range of indexes."},
		{"ZREVRANGE", engine.RangeByRank, true, "Returns members in a sorted set within a range of indexes, in reverse order."},
		{"ZRANGEBYSCORE", engine.RangeByScore, false, "Returns members in a sorted set within a range of scores."},
		{"ZREVRANGEBYSCORE", engine.RangeByScore, true, "Returns members in a sorted set within a range of scores, in reverse order."},
		{"ZRANGEBYLEX", engine.RangeByLex, false, "Returns members in a sorted set within a lexicographical range."},
		{"ZREVRANGEBYLEX", engine.RangeByLex, true, "Returns members in a sorted set within a lexicographical range, in reverse order."},
	} {
		by, rev, legacy := r.by, r.rev, r.name != "ZRANGE"
		register(&Descriptor{
			Name: r.name, Arity: -4, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"read", "sortedset", "slow"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return zrange(c, by, rev, legacy) },
		})
	}
	register(&Descriptor{
		Name: "ZRANGESTORE", Arity: -5, Flags: Write | DenyOOM, Effect: EffectVerbatim,
		Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"write", "sortedset", "slow"},
		Summary:    "Stores a range of members from a sorted set in a key.",
		Handler:    cmdZRangeStore,
	})
	for _, r := range []struct {
		name    string
		rev     bool
		summary string
	}{
		{"ZRANK", false, "Returns the index of a member in a sorted set ordered by ascending scores."},
		{"ZREVRANK", true, "Returns the index of a member in a sorted set ordered by descending scores."},
	} {
		rev := r.rev
		register(&Descriptor{
			Name: r.name, Arity: -3, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"read", "sortedset", "fast"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return zrank(c, rev) },
		})
	}
	for _, r := range []struct {
		name    string
		highest bool
		summary string
	}{
		{"ZPOPMIN", false, "Returns the lowest-scoring members after removing them."},
		{"ZPOPMAX", true, "Returns the highest-scoring members after removing them."},
	} {
		highest := r.highest
		register(&Descriptor{
			Name: r.name, Arity: -2, Flags: Write | Fast, Effect: EffectCanonical,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"write", "sortedset", "fast"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return zpop(c, highest) },
		})
	}
	for _, r := range []struct {
		name    string
		by      engine.RangeBy
		summary string
	}{
		{"ZREMRANGEBYRANK", engine.RangeByRank, "Removes members in a sorted set within a range of indexes."},
		{"ZREMRANGEBYSCORE", engine.RangeByScore, "Removes members in a sorted set within a range of scores."},
		{"ZREMRANGEBYLEX", engine.RangeByLex, "Removes members in a sorted set within a lexicographical range."},
	} {
		by := r.by
		register(&Descriptor{
			// Same-key ranges are deterministic against the same base
			// state, so these are safe to log as they arrived.
			Name: r.name, Arity: 4, Flags: Write, Effect: EffectVerbatim,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"write", "sortedset", "slow"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return zremrange(c, by) },
		})
	}
	for _, r := range []struct {
		name    string
		op      engine.SetOp
		summary string
	}{
		{"ZUNION", engine.SetUnion, "Returns the union of multiple sorted sets."},
		{"ZINTER", engine.SetInter, "Returns the intersection of multiple sorted sets."},
		{"ZDIFF", engine.SetDiff, "Returns the difference between the first and all successive sorted sets."},
	} {
		op := r.op
		register(&Descriptor{
			Name: r.name, Arity: -3, Flags: Readonly, FirstKey: 2, LastKey: -1, Step: 1,
			Categories: []string{"read", "sortedset", "slow"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return zcombine(c, op) },
		})
		register(&Descriptor{
			Name: r.name + "STORE", Arity: -4, Flags: Write | DenyOOM, Effect: EffectVerbatim,
			Locality: LocalityCrossShard,
			FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"write", "sortedset", "slow"},
			Summary:    r.summary + " Stores the result in a key.",
			Handler:    func(c *Ctx) resp.Value { return zcombineStore(c, op) },
		})
	}
	register(&Descriptor{
		Name: "ZINTERCARD", Arity: -3, Flags: Readonly, FirstKey: 2, LastKey: -1, Step: 1,
		Categories: []string{"read", "sortedset", "slow"},
		Summary:    "Returns the number of members in the intersection of multiple sorted sets.",
		Handler:    cmdZInterCard,
	})
	register(&Descriptor{
		Name: "ZRANDMEMBER", Arity: -2, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "sortedset", "slow"},
		Summary:    "Returns one or more random members from a sorted set.",
		Handler:    cmdZRandMember,
	})
	register(&Descriptor{
		Name: "ZSCAN", Arity: -3, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "sortedset", "slow"},
		Summary:    "Iterates over the members and scores of a sorted set.",
		Handler:    cmdZScan,
	})
}

// parseScore parses a score, accepting the infinity spellings.
func parseScore(b []byte) (float64, bool) {
	switch strings.ToLower(string(b)) {
	case "inf", "+inf", "infinity", "+infinity":
		return math.Inf(1), true
	case "-inf", "-infinity":
		return math.Inf(-1), true
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// parseScoreBound parses one endpoint of a score range. A leading '(' makes
// it exclusive.
func parseScoreBound(b []byte) (value float64, exclusive bool, ok bool) {
	if len(b) > 0 && b[0] == '(' {
		v, ok := parseScore(b[1:])
		return v, true, ok
	}
	v, ok := parseScore(b)
	return v, false, ok
}

func parseScoreRange(minB, maxB []byte) (engine.ScoreRange, bool) {
	lo, loExcl, ok1 := parseScoreBound(minB)
	hi, hiExcl, ok2 := parseScoreBound(maxB)
	if !ok1 || !ok2 {
		return engine.ScoreRange{}, false
	}
	return engine.ScoreRange{Min: lo, Max: hi, MinExcl: loExcl, MaxExcl: hiExcl}, true
}

// parseLexBound parses one endpoint of a lexicographic range: '-' and '+'
// are the infinite ends, '[' is inclusive and '(' is exclusive.
func parseLexBound(b []byte) (value []byte, exclusive, infinite, ok bool) {
	if len(b) == 1 {
		switch b[0] {
		case '-', '+':
			return nil, false, true, true
		}
	}
	if len(b) == 0 {
		return nil, false, false, false
	}
	switch b[0] {
	case '[':
		return b[1:], false, false, true
	case '(':
		return b[1:], true, false, true
	default:
		return nil, false, false, false
	}
}

func parseLexRange(minB, maxB []byte) (engine.LexRange, bool) {
	lo, loExcl, loInf, ok1 := parseLexBound(minB)
	hi, hiExcl, hiInf, ok2 := parseLexBound(maxB)
	if !ok1 || !ok2 {
		return engine.LexRange{}, false
	}
	// '-' is only meaningful as a minimum and '+' only as a maximum, but
	// both spellings resolve to "unbounded on this side", which is how the
	// reference implementation treats them too.
	return engine.LexRange{
		Min: lo, Max: hi,
		MinExcl: loExcl, MaxExcl: hiExcl,
		MinInf: loInf, MaxInf: hiInf,
	}, true
}

var errMinMaxFloat = resp.Err("ERR min or max is not a float")
var errMinMaxLex = resp.Err("ERR min or max not valid string range item")

func cmdZAdd(c *Ctx) resp.Value {
	var flags engine.ZAddFlags
	i := 2
	for ; i < c.Len(); i++ {
		switch strings.ToUpper(string(c.Arg(i))) {
		case "NX":
			flags |= engine.ZAddNX
		case "XX":
			flags |= engine.ZAddXX
		case "GT":
			flags |= engine.ZAddGT
		case "LT":
			flags |= engine.ZAddLT
		case "CH":
			flags |= engine.ZAddCH
		case "INCR":
			flags |= engine.ZAddINCR
		default:
			goto scores
		}
	}
scores:
	rest := c.Args[i:]
	if len(rest) == 0 || len(rest)%2 != 0 {
		return errSyntax
	}
	// NX with XX gets its own message, because that is the pair users hit
	// most often and the reference implementation distinguishes it.
	if flags&engine.ZAddNX != 0 && flags&engine.ZAddXX != 0 {
		return resp.Err("ERR XX and NX options at the same time are not compatible")
	}
	if flags&engine.ZAddGT != 0 && flags&engine.ZAddLT != 0 ||
		flags&engine.ZAddNX != 0 && flags&(engine.ZAddGT|engine.ZAddLT) != 0 {
		return resp.Err("ERR GT, LT, and/or NX options at the same time are not compatible")
	}
	if flags&engine.ZAddINCR != 0 && len(rest) != 2 {
		return resp.Err("ERR INCR option supports a single increment-element pair")
	}

	members := make([]engine.ZMember, 0, len(rest)/2)
	for j := 0; j+1 < len(rest); j += 2 {
		score, ok := parseScore(rest[j])
		if !ok {
			return errMinMaxFloat
		}
		members = append(members, engine.ZMember{Member: rest[j+1], Score: score})
	}

	res, err := c.DB().ZAdd(c.Arg(1), members, flags)
	if err != nil {
		if err == engine.ErrNaN {
			return resp.Err("ERR resulting score is not a number (NaN)")
		}
		return engineError(err)
	}
	if len(res.Applied) > 0 {
		c.Dirty(len(res.Applied))
		// Every conditional and relative form collapses into a plain ZADD
		// carrying the scores that were actually written (ADR-008).
		args := make([][]byte, 0, 2+len(res.Applied)*2)
		args = append(args, effZADD, c.Arg(1))
		for _, m := range res.Applied {
			args = append(args, engine.FormatFloat(m.Score), m.Member)
		}
		c.Propagate(args...)
	}
	if flags&engine.ZAddINCR != 0 {
		if len(res.Applied) == 0 {
			return resp.Null()
		}
		return scoreReply(res.Applied[0].Score)
	}
	if flags&engine.ZAddCH != 0 {
		return resp.Int(res.Changed)
	}
	return resp.Int(res.Added)
}

// scoreReply renders a score.
//
// It is a double, which the writer emits as ",<value>" under RESP3 and as a
// bulk string under RESP2, so the handler never has to ask which protocol
// the client negotiated (ADR-016).
func scoreReply(score float64) resp.Value { return resp.Double(score) }

func cmdZIncrBy(c *Ctx) resp.Value {
	delta, ok := parseScore(c.Arg(2))
	if !ok {
		return errMinMaxFloat
	}
	res, err := c.DB().ZAdd(c.Arg(1),
		[]engine.ZMember{{Member: c.Arg(3), Score: delta}}, engine.ZAddINCR)
	if err != nil {
		if err == engine.ErrNaN {
			return resp.Err("ERR resulting score is not a number (NaN)")
		}
		return engineError(err)
	}
	if len(res.Applied) == 0 {
		return resp.Null()
	}
	c.Dirty(1)
	c.Propagate(effZADD, c.Arg(1), engine.FormatFloat(res.Applied[0].Score), c.Arg(3))
	return scoreReply(res.Applied[0].Score)
}

func cmdZRem(c *Ctx) resp.Value {
	n, err := c.DB().ZRem(c.Arg(1), c.Tail(2))
	if err != nil {
		return engineError(err)
	}
	c.Dirty(int(n))
	return resp.Int(n)
}

func cmdZScore(c *Ctx) resp.Value {
	score, ok, err := c.DB().ZScore(c.Arg(1), c.Arg(2))
	if err != nil {
		return engineError(err)
	}
	if !ok {
		return resp.Null()
	}
	return scoreReply(score)
}

func cmdZMScore(c *Ctx) resp.Value {
	scores, found, err := c.DB().ZMScore(c.Arg(1), c.Tail(2))
	if err != nil {
		return engineError(err)
	}
	out := make([]resp.Value, len(scores))
	for i := range scores {
		if !found[i] {
			out[i] = resp.Null()
		} else {
			out[i] = scoreReply(scores[i])
		}
	}
	return resp.ArrayOf(out)
}

func cmdZCard(c *Ctx) resp.Value {
	n, err := c.DB().ZCard(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	return resp.Int(n)
}

func cmdZCount(c *Ctx) resp.Value {
	r, ok := parseScoreRange(c.Arg(2), c.Arg(3))
	if !ok {
		return errMinMaxFloat
	}
	n, err := c.DB().ZCount(c.Arg(1), r)
	if err != nil {
		return engineError(err)
	}
	return resp.Int(n)
}

func cmdZLexCount(c *Ctx) resp.Value {
	r, ok := parseLexRange(c.Arg(2), c.Arg(3))
	if !ok {
		return errMinMaxLex
	}
	n, err := c.DB().ZLexCount(c.Arg(1), r)
	if err != nil {
		return engineError(err)
	}
	return resp.Int(n)
}

// zrangeArgs parses the shared body of every ZRANGE variant.
//
// The modern ZRANGE takes BYSCORE, BYLEX and REV as options; the older
// ZRANGEBYSCORE, ZREVRANGE and friends encode the same choices in the
// command name. Parsing both into one spec means there is a single range
// implementation rather than six.
func zrangeArgs(c *Ctx, by engine.RangeBy, reverse, legacy bool, from int) (engine.ZRangeSpec, resp.Value, bool, bool) {
	spec := engine.ZRangeSpec{By: by, Reverse: reverse, Count: -1}
	withScores := false
	limitSeen := false

	for i := from + 2; i < c.Len(); i++ {
		switch opt := strings.ToUpper(string(c.Arg(i))); opt {
		case "WITHSCORES":
			if by == engine.RangeByLex {
				return spec, errSyntax, false, false
			}
			withScores = true
		case "BYSCORE", "BYLEX", "REV":
			if legacy {
				return spec, errSyntax, false, false
			}
			switch opt {
			case "BYSCORE":
				spec.By = engine.RangeByScore
			case "BYLEX":
				spec.By = engine.RangeByLex
			default:
				spec.Reverse = true
			}
		case "LIMIT":
			if i+2 >= c.Len() {
				return spec, errSyntax, false, false
			}
			offset, err1 := resp.ParseInt(c.Arg(i + 1))
			count, err2 := resp.ParseInt(c.Arg(i + 2))
			if err1 != nil || err2 != nil {
				return spec, errNotInteger, false, false
			}
			spec.Offset, spec.Count, limitSeen = offset, count, true
			i += 2
		default:
			return spec, errSyntax, false, false
		}
	}
	if limitSeen && spec.By == engine.RangeByRank {
		return spec, resp.Err("ERR syntax error, LIMIT is only supported in " +
			"combination with either BYSCORE or BYLEX"), false, false
	}

	// The two endpoint arguments are interpreted according to the selector,
	// and a reversed range takes them in the opposite order.
	lo, hi := c.Arg(from), c.Arg(from+1)
	switch spec.By {
	case engine.RangeByScore:
		if spec.Reverse {
			lo, hi = hi, lo
		}
		r, ok := parseScoreRange(lo, hi)
		if !ok {
			return spec, errMinMaxFloat, false, false
		}
		spec.Score = r
	case engine.RangeByLex:
		if spec.Reverse {
			lo, hi = hi, lo
		}
		r, ok := parseLexRange(lo, hi)
		if !ok {
			return spec, errMinMaxLex, false, false
		}
		spec.Lex = r
	default:
		start, err1 := resp.ParseInt(lo)
		stop, err2 := resp.ParseInt(hi)
		if err1 != nil || err2 != nil {
			return spec, errNotInteger, false, false
		}
		spec.Start, spec.Stop = start, stop
	}
	return spec, resp.Value{}, withScores, true
}

func zrange(c *Ctx, by engine.RangeBy, reverse, legacy bool) resp.Value {
	spec, reply, withScores, ok := zrangeArgs(c, by, reverse, legacy, 2)
	if !ok {
		return reply
	}
	members, err := c.DB().ZRange(c.Arg(1), spec)
	if err != nil {
		return engineError(err)
	}
	return memberReply(c, members, withScores)
}

// memberReply renders a member list, with scores interleaved under RESP2 and
// as pairs under RESP3 (ADR-016).
func memberReply(c *Ctx, members []engine.ZMember, withScores bool) resp.Value {
	if !withScores {
		out := make([]resp.Value, len(members))
		for i, m := range members {
			out[i] = resp.Bulk(m.Member)
		}
		return resp.ArrayOf(out)
	}
	if c.Client.Protocol() >= resp.RESP3 {
		out := make([]resp.Value, len(members))
		for i, m := range members {
			out[i] = resp.Array(resp.Bulk(m.Member), resp.Double(m.Score))
		}
		return resp.ArrayOf(out)
	}
	out := make([]resp.Value, 0, len(members)*2)
	for _, m := range members {
		out = append(out, resp.Bulk(m.Member), resp.Bulk(engine.FormatFloat(m.Score)))
	}
	return resp.ArrayOf(out)
}

func cmdZRangeStore(c *Ctx) resp.Value {
	spec, reply, _, ok := zrangeArgs(c, engine.RangeByRank, false, false, 3)
	if !ok {
		return reply
	}
	n, err := c.DB().ZRangeStore(c.Arg(1), c.Arg(2), spec)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	return resp.Int(n)
}

func zrank(c *Ctx, reverse bool) resp.Value {
	withScore := false
	if c.Len() == 4 {
		if !strings.EqualFold(string(c.Arg(3)), "withscore") {
			return errSyntax
		}
		withScore = true
	} else if c.Len() != 3 {
		return errSyntax
	}
	rank, ok, err := c.DB().ZRank(c.Arg(1), c.Arg(2), reverse)
	if err != nil {
		return engineError(err)
	}
	if !ok {
		if withScore {
			return resp.NullArray()
		}
		return resp.Null()
	}
	if !withScore {
		return resp.Int(int64(rank))
	}
	score, _, _ := c.DB().ZScore(c.Arg(1), c.Arg(2))
	return resp.Array(resp.Int(int64(rank)), scoreReply(score))
}

func zpop(c *Ctx, highest bool) resp.Value {
	count, explicit := 1, false
	if c.Len() == 3 {
		n, err := resp.ParseInt(c.Arg(2))
		if err != nil {
			return errNotInteger
		}
		if n < 0 {
			return errIndexRange
		}
		count, explicit = int(n), true
	} else if c.Len() != 2 {
		return errSyntax
	}
	popped, err := c.DB().ZPop(c.Arg(1), count, highest)
	if err != nil {
		return engineError(err)
	}
	if len(popped) > 0 {
		c.Dirty(len(popped))
		propagateZRem(c, popped)
	}
	if !explicit {
		if len(popped) == 0 {
			return resp.EmptyArray()
		}
		return resp.Array(resp.Bulk(popped[0].Member), scoreReply(popped[0].Score))
	}
	return memberReply(c, popped, true)
}

// propagateZRem records the exact members a pop or range removal took out.
func propagateZRem(c *Ctx, members []engine.ZMember) {
	args := make([][]byte, 0, 2+len(members))
	args = append(args, effZREM, c.Arg(1))
	for _, m := range members {
		args = append(args, m.Member)
	}
	c.Propagate(args...)
}

func zremrange(c *Ctx, by engine.RangeBy) resp.Value {
	spec, reply, _, ok := zrangeArgs(c, by, false, true, 2)
	if !ok {
		return reply
	}
	n, err := c.DB().ZRemRange(c.Arg(1), spec)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(int(n))
	return resp.Int(n)
}

// zsetCombineArgs parses numkeys, the key list, WEIGHTS and AGGREGATE.
func zsetCombineArgs(c *Ctx, from int) (keys [][]byte, weights []float64, agg engine.Aggregate, limit int, reply resp.Value, ok bool) {
	numKeys, err := resp.ParseInt(c.Arg(from))
	if err != nil || numKeys <= 0 {
		return nil, nil, 0, 0, resp.Err("ERR at least 1 input key is needed for the command"), false
	}
	if int(numKeys) > c.Len()-from-1 {
		return nil, nil, 0, 0, errSyntax, false
	}
	keys = c.Args[from+1 : from+1+int(numKeys)]
	for i := from + 1 + int(numKeys); i < c.Len(); i++ {
		switch strings.ToUpper(string(c.Arg(i))) {
		case "WEIGHTS":
			if i+int(numKeys) >= c.Len() {
				return nil, nil, 0, 0, errSyntax, false
			}
			weights = make([]float64, 0, numKeys)
			for j := 0; j < int(numKeys); j++ {
				w, ok := parseScore(c.Arg(i + 1 + j))
				if !ok {
					return nil, nil, 0, 0, resp.Err("ERR weight value is not a float"), false
				}
				weights = append(weights, w)
			}
			i += int(numKeys)
		case "AGGREGATE":
			if i+1 >= c.Len() {
				return nil, nil, 0, 0, errSyntax, false
			}
			switch strings.ToUpper(string(c.Arg(i + 1))) {
			case "SUM":
				agg = engine.AggregateSum
			case "MIN":
				agg = engine.AggregateMin
			case "MAX":
				agg = engine.AggregateMax
			default:
				return nil, nil, 0, 0, errSyntax, false
			}
			i++
		case "LIMIT":
			if i+1 >= c.Len() {
				return nil, nil, 0, 0, errSyntax, false
			}
			n, err := resp.ParseInt(c.Arg(i + 1))
			if err != nil || n < 0 {
				return nil, nil, 0, 0, resp.Err("ERR LIMIT can't be negative"), false
			}
			limit = int(n)
			i++
		case "WITHSCORES":
			// Handled by the caller; accepted here so the loop does not
			// reject it.
		default:
			return nil, nil, 0, 0, errSyntax, false
		}
	}
	return keys, weights, agg, limit, resp.Value{}, true
}

func zcombine(c *Ctx, op engine.SetOp) resp.Value {
	keys, weights, agg, _, reply, ok := zsetCombineArgs(c, 1)
	if !ok {
		return reply
	}
	withScores := false
	for _, a := range c.Tail(1) {
		if strings.EqualFold(string(a), "withscores") {
			withScores = true
		}
	}
	members, err := c.DB().ZCombine(op, keys, weights, agg, 0)
	if err != nil {
		return engineError(err)
	}
	return memberReply(c, members, withScores)
}

func zcombineStore(c *Ctx, op engine.SetOp) resp.Value {
	keys, weights, agg, _, reply, ok := zsetCombineArgs(c, 2)
	if !ok {
		return reply
	}
	n, err := c.DB().ZCombineStore(op, c.Arg(1), keys, weights, agg)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	return resp.Int(n)
}

func cmdZInterCard(c *Ctx) resp.Value {
	keys, _, _, limit, reply, ok := zsetCombineArgs(c, 1)
	if !ok {
		return reply
	}
	members, err := c.DB().ZCombine(engine.SetInter, keys, nil, engine.AggregateSum, limit)
	if err != nil {
		return engineError(err)
	}
	return resp.Int(int64(len(members)))
}

func cmdZRandMember(c *Ctx) resp.Value {
	if c.Len() == 2 {
		out, err := c.DB().ZRandMember(c.Arg(1), 1)
		if err != nil {
			return engineError(err)
		}
		if len(out) == 0 {
			return resp.Null()
		}
		return resp.Bulk(out[0].Member)
	}
	if c.Len() > 4 {
		return errSyntax
	}
	count, err := resp.ParseInt(c.Arg(2))
	if err != nil {
		return errNotInteger
	}
	withScores := false
	if c.Len() == 4 {
		if !strings.EqualFold(string(c.Arg(3)), "withscores") {
			return errSyntax
		}
		withScores = true
	}
	out, engErr := c.DB().ZRandMember(c.Arg(1), int(count))
	if engErr != nil {
		return engineError(engErr)
	}
	return memberReply(c, out, withScores)
}

func cmdZScan(c *Ctx) resp.Value {
	_, opts, reply, ok := parseScanArgs(c, 3, false)
	if !ok {
		return reply
	}
	members, err := c.DB().ZRange(c.Arg(1),
		engine.ZRangeSpec{By: engine.RangeByRank, Start: 0, Stop: -1, Count: -1})
	if err != nil {
		return engineError(err)
	}
	out := make([]resp.Value, 0, len(members)*2)
	for _, m := range members {
		if opts.Match != nil && !engine.MatchPattern(opts.Match, m.Member) {
			continue
		}
		out = append(out, resp.Bulk(m.Member), resp.Bulk(engine.FormatFloat(m.Score)))
	}
	return resp.Array(resp.BulkString("0"), resp.ArrayOf(out))
}
