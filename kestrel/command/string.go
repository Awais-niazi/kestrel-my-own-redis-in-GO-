package command

import (
	"math"
	"strconv"
	"strings"

	"kestrel/engine"
	"kestrel/resp"
)

// Canonical effect verbs. Every rewritten write reaches the log as one of
// these, so the log is a stream of absolute, replay-safe operations.
var (
	effSET       = []byte("SET")
	effDEL       = []byte("DEL")
	effMSET      = []byte("MSET")
	effPXAT      = []byte("PXAT")
	effKEEPTTL   = []byte("KEEPTTL")
	effPEXPIREAT = []byte("PEXPIREAT")
	effPERSIST   = []byte("PERSIST")
)

// itob renders an integer as its decimal wire representation.
func itob(n int64) []byte { return strconv.AppendInt(nil, n, 10) }

// propagateSet emits the canonical form of a successful string write:
// an unconditional SET carrying an absolute expiry, if any.
func propagateSet(c *Ctx, key, val []byte, at int64, keepTTL bool) {
	switch {
	case at > 0:
		c.Propagate(effSET, key, val, effPXAT, itob(at))
	case keepTTL:
		c.Propagate(effSET, key, val, effKEEPTTL)
	default:
		c.Propagate(effSET, key, val)
	}
}

func init() {
	register(&Descriptor{
		Name: "GET", Arity: 2, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "string", "fast"},
		Summary:    "Returns the string value of a key.",
		Handler:    cmdGet,
	})
	register(&Descriptor{
		Name: "SET", Arity: -3, Flags: Write | DenyOOM, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "slow"},
		Summary:    "Sets the string value of a key, ignoring its type.",
		Handler:    cmdSet, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
	})
	register(&Descriptor{
		Name: "SETNX", Arity: 3, Flags: Write | DenyOOM | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "fast"},
		Summary:    "Sets the value of a key only when the key doesn't exist.",
		Handler:    cmdSetNX, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
	})
	register(&Descriptor{
		Name: "SETEX", Arity: 4, Flags: Write | DenyOOM, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "slow"},
		Summary:    "Sets the value and expiration time of a key.",
		Handler:    cmdSetEx, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
	})
	register(&Descriptor{
		Name: "PSETEX", Arity: 4, Flags: Write | DenyOOM, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "slow"},
		Summary:    "Sets the value and expiration in milliseconds of a key.",
		Handler:    cmdSetEx, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
	})
	register(&Descriptor{
		Name: "GETSET", Arity: 3, Flags: Write | DenyOOM | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "fast"},
		Summary:    "Returns the previous string value of a key after setting it.",
		Handler:    cmdGetSet, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
	})
	register(&Descriptor{
		Name: "GETDEL", Arity: 2, Flags: Write | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "fast"},
		Summary:    "Returns the string value of a key after deleting it.",
		Handler:    cmdGetDel, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
	})
	register(&Descriptor{
		Name: "GETEX", Arity: -2, Flags: Write | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "fast"},
		Summary:    "Returns the string value of a key after setting its expiration time.",
		Handler:    cmdGetEx, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
	})
	register(&Descriptor{
		Name: "MGET", Arity: -2, Flags: Readonly | Fast, FirstKey: 1, LastKey: -1, Step: 1,
		Categories: []string{"read", "string", "fast"},
		Summary:    "Returns the string values of one or more keys.",
		Handler:    cmdMGet,
	})
	register(&Descriptor{
		Name: "MSET", Arity: -3, Flags: Write | DenyOOM, Effect: EffectVerbatim, FirstKey: 1, LastKey: -1, Step: 2,
		Locality:   LocalityShardLocal,
		Categories: []string{"write", "string", "slow"},
		Summary:    "Sets the string values of one or more keys.",
		Handler:    cmdMSet,
	})
	register(&Descriptor{
		Name: "MSETNX", Arity: -3, Flags: Write | DenyOOM, FirstKey: 1, LastKey: -1, Step: 2,
		Categories: []string{"write", "string", "slow"},
		Summary:    "Sets the string values of one or more keys, only when none of them exist.",
		Handler:    cmdMSetNX, Effect: EffectCanonical,
		Locality: LocalityCrossShard,
	})
	for _, spec := range []struct {
		name    string
		summary string
	}{
		{"INCR", "Increments the integer value of a key by one."},
		{"DECR", "Decrements the integer value of a key by one."},
		{"INCRBY", "Increments the integer value of a key by a number."},
		{"DECRBY", "Decrements the integer value of a key by a number."},
	} {
		arity := 2
		if strings.HasSuffix(spec.name, "BY") {
			arity = 3
		}
		register(&Descriptor{
			Name: spec.name, Arity: arity,
			// INCR and friends are deltas, but a delta applied to an ordered
			// replay from a consistent base is deterministic, so they are
			// logged verbatim (ADR-008). The per-shard offset filter in the
			// recovery path is what makes "consistent base" true (§7.3).
			Flags: Write | DenyOOM | Fast, Effect: EffectVerbatim,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: 1, Step: 1,
			Categories: []string{"write", "string", "fast"},
			Summary:    spec.summary,
			Handler:    cmdIncrDecr,
		})
	}
	register(&Descriptor{
		Name: "INCRBYFLOAT", Arity: 3, Flags: Write | DenyOOM | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "fast"},
		Summary:    "Increments the floating point value of a key by a number.",
		Handler:    cmdIncrByFloat, Effect: EffectCanonical,
		Locality: LocalityShardLocal,
	})
	register(&Descriptor{
		Name: "APPEND", Arity: 3, Flags: Write | DenyOOM | Fast, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "fast"},
		Summary:    "Appends a string to the value of a key.",
		Handler:    cmdAppend,
	})
	register(&Descriptor{
		Name: "STRLEN", Arity: 2, Flags: Readonly | Fast, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "string", "fast"},
		Summary:    "Returns the length of a string value.",
		Handler:    cmdStrLen,
	})
	register(&Descriptor{
		Name: "GETRANGE", Arity: 4, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "string", "slow"},
		Summary:    "Returns a substring of the string stored at a key.",
		Handler:    cmdGetRange,
	})
	register(&Descriptor{
		Name: "SETRANGE", Arity: 4, Flags: Write | DenyOOM, Effect: EffectVerbatim,
		Locality: LocalityShardLocal,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "string", "slow"},
		Summary:    "Overwrites part of a string value at a key.",
		Handler:    cmdSetRange,
	})
	register(&Descriptor{
		Name: "SUBSTR", Arity: 4, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "string", "slow"},
		Summary:    "Returns a substring of the string stored at a key. Deprecated alias of GETRANGE.",
		Handler:    cmdGetRange,
	})
}

func cmdGet(c *Ctx) resp.Value {
	v, ok, err := c.DB().Get(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	return bulkOrNil(v, ok)
}

// setArgs is the parsed form of SET's option list.
type setArgs struct {
	opts   engine.SetOptions
	hasTTL bool
}

func parseSetOptions(c *Ctx, from int) (setArgs, resp.Value, bool) {
	var out setArgs
	seenExpire := false
	for i := from; i < c.Len(); i++ {
		switch strings.ToUpper(string(c.Arg(i))) {
		case "NX":
			if out.opts.XX {
				return out, errSyntax, false
			}
			out.opts.NX = true
		case "XX":
			if out.opts.NX {
				return out, errSyntax, false
			}
			out.opts.XX = true
		case "GET":
			out.opts.Get = true
		case "KEEPTTL":
			if seenExpire {
				return out, errSyntax, false
			}
			out.opts.KeepTTL = true
		case "EX", "PX", "EXAT", "PXAT":
			if seenExpire || out.opts.KeepTTL || i+1 >= c.Len() {
				return out, errSyntax, false
			}
			at, reply, ok := absoluteExpiry(string(c.Arg(i)), c.Arg(i+1), c.Now(), "set")
			if !ok {
				return out, reply, false
			}
			out.opts.At, out.hasTTL, seenExpire = at, true, true
			i++
		default:
			return out, errSyntax, false
		}
	}
	return out, resp.Value{}, true
}

// absoluteExpiry converts one of the four expiry spellings into an absolute
// millisecond timestamp. Doing the conversion here, at the edge, is what lets
// every downstream layer treat expiry as an absolute instant, which is the
// precondition for replay safety (ADR-008).
func absoluteExpiry(unit string, raw []byte, nowMS int64, cmd string) (int64, resp.Value, bool) {
	n, err := resp.ParseInt(raw)
	if err != nil {
		return 0, errNotInteger, false
	}
	invalid := resp.Err("ERR invalid expire time in '" + cmd + "' command")
	switch unit {
	case "EX", "PX":
		if n <= 0 {
			return 0, invalid, false
		}
	}
	var at int64
	switch unit {
	case "EX", "EXAT":
		if n > math.MaxInt64/1000 || n < math.MinInt64/1000 {
			return 0, invalid, false
		}
		at = n * 1000
	default:
		at = n
	}
	switch unit {
	case "EX", "PX":
		if at > math.MaxInt64-nowMS {
			return 0, invalid, false
		}
		at += nowMS
	}
	if at <= 0 {
		// An absolute expiry at or before the epoch would be
		// indistinguishable from "no expiry", so it is rejected rather than
		// silently making the key permanent.
		return 0, invalid, false
	}
	return at, resp.Value{}, true
}

func cmdSet(c *Ctx) resp.Value {
	args, reply, ok := parseSetOptions(c, 3)
	if !ok {
		return reply
	}
	res, err := c.DB().Set(c.Arg(1), c.Arg(2), args.opts)
	if err != nil {
		return engineError(err)
	}
	if res.Set {
		c.Dirty(1)
		propagateSet(c, c.Arg(1), c.Arg(2), args.opts.At, args.opts.KeepTTL)
	}
	if args.opts.Get {
		if !res.Set && !res.OldExists {
			return resp.Null()
		}
		return bulkOrNil(res.Old, res.OldExists)
	}
	if !res.Set {
		return resp.Null()
	}
	return resp.OK()
}

func cmdSetNX(c *Ctx) resp.Value {
	res, err := c.DB().Set(c.Arg(1), c.Arg(2), engine.SetOptions{NX: true})
	if err != nil {
		return engineError(err)
	}
	if res.Set {
		c.Dirty(1)
		propagateSet(c, c.Arg(1), c.Arg(2), 0, false)
	}
	return resp.Int(b2i(res.Set))
}

func cmdSetEx(c *Ctx) resp.Value {
	unit := "EX"
	if strings.EqualFold(string(c.Name()), "psetex") {
		unit = "PX"
	}
	at, reply, ok := absoluteExpiry(unit, c.Arg(2), c.Now(), strings.ToLower(string(c.Name())))
	if !ok {
		return reply
	}
	if _, err := c.DB().Set(c.Arg(1), c.Arg(3), engine.SetOptions{At: at}); err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	propagateSet(c, c.Arg(1), c.Arg(3), at, false)
	return resp.OK()
}

func cmdGetSet(c *Ctx) resp.Value {
	res, err := c.DB().Set(c.Arg(1), c.Arg(2), engine.SetOptions{Get: true})
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	propagateSet(c, c.Arg(1), c.Arg(2), 0, false)
	return bulkOrNil(res.Old, res.OldExists)
}

func cmdGetDel(c *Ctx) resp.Value {
	v, ok, err := c.DB().GetDel(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	if ok {
		c.Dirty(1)
		c.Propagate(effDEL, c.Arg(1))
	}
	return bulkOrNil(v, ok)
}

func cmdGetEx(c *Ctx) resp.Value {
	var (
		at      int64
		persist bool
	)
	for i := 2; i < c.Len(); i++ {
		switch opt := strings.ToUpper(string(c.Arg(i))); opt {
		case "PERSIST":
			if at != 0 {
				return errSyntax
			}
			persist = true
		case "EX", "PX", "EXAT", "PXAT":
			if persist || at != 0 || i+1 >= c.Len() {
				return errSyntax
			}
			var (
				reply resp.Value
				ok    bool
			)
			at, reply, ok = absoluteExpiry(opt, c.Arg(i+1), c.Now(), "getex")
			if !ok {
				return reply
			}
			i++
		default:
			return errSyntax
		}
	}
	v, ok, err := c.DB().GetEx(c.Arg(1), at, persist)
	if err != nil {
		return engineError(err)
	}
	if ok && (persist || at != 0) {
		c.Dirty(1)
		switch {
		case persist:
			c.Propagate(effPERSIST, c.Arg(1))
		case at <= c.Now():
			// The engine reaped the key rather than re-dating it, so the
			// replica must be told to delete it, not to expire it later.
			c.Propagate(effDEL, c.Arg(1))
		default:
			c.Propagate(effPEXPIREAT, c.Arg(1), itob(at))
		}
	}
	return bulkOrNil(v, ok)
}

func cmdMGet(c *Ctx) resp.Value {
	return bulkArrayWithNils(c.DB().MGet(c.Tail(1)))
}

func cmdMSet(c *Ctx) resp.Value {
	if c.Len()%2 != 1 {
		return errWrongArgs("mset")
	}
	c.DB().MSet(c.Tail(1))
	c.Dirty(c.Len() / 2)
	return resp.OK()
}

func cmdMSetNX(c *Ctx) resp.Value {
	if c.Len()%2 != 1 {
		return errWrongArgs("msetnx")
	}
	ok := c.DB().MSetNX(c.Tail(1))
	if ok {
		c.Dirty(c.Len() / 2)
		// MSETNX is conditional, and on replay the condition may evaluate
		// differently against a partially recovered base, so it reaches the
		// log as the unconditional write it turned out to be.
		c.Propagate(append([][]byte{effMSET}, c.Tail(1)...)...)
	}
	return resp.Int(b2i(ok))
}

func cmdIncrDecr(c *Ctx) resp.Value {
	var delta int64 = 1
	name := strings.ToUpper(string(c.Name()))
	if strings.HasSuffix(name, "BY") {
		n, err := resp.ParseInt(c.Arg(2))
		if err != nil {
			return errNotInteger
		}
		delta = n
	}
	if strings.HasPrefix(name, "DECR") {
		if delta == math.MinInt64 {
			return errOverflow
		}
		delta = -delta
	}
	n, err := c.DB().IncrBy(c.Arg(1), delta)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	return resp.Int(n)
}

func cmdIncrByFloat(c *Ctx) resp.Value {
	delta, err := engine.ParseFloat(c.Arg(2))
	if err != nil {
		return errNotFloat
	}
	_, rendered, err := c.DB().IncrByFloat(c.Arg(1), delta)
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	// Float accumulation must not drift on replay, so the computed result is
	// logged rather than the increment (ADR-008).
	//
	// KEEPTTL is not optional here. INCRBYFLOAT leaves a key's expiry alone,
	// but a plain SET clears it, so logging one as the other would quietly
	// make an expiring key permanent on every replica and on every restart.
	c.Propagate(effSET, c.Arg(1), rendered, effKEEPTTL)
	return resp.Bulk(rendered)
}

func cmdAppend(c *Ctx) resp.Value {
	n, err := c.DB().Append(c.Arg(1), c.Arg(2))
	if err != nil {
		return engineError(err)
	}
	c.Dirty(1)
	return resp.Int(n)
}

func cmdStrLen(c *Ctx) resp.Value {
	n, err := c.DB().StrLen(c.Arg(1))
	if err != nil {
		return engineError(err)
	}
	return resp.Int(n)
}

func cmdGetRange(c *Ctx) resp.Value {
	start, err1 := resp.ParseInt(c.Arg(2))
	end, err2 := resp.ParseInt(c.Arg(3))
	if err1 != nil || err2 != nil {
		return errNotInteger
	}
	v, err := c.DB().GetRange(c.Arg(1), start, end)
	if err != nil {
		return engineError(err)
	}
	if v == nil {
		return resp.BulkString("")
	}
	return resp.Bulk(v)
}

func cmdSetRange(c *Ctx) resp.Value {
	offset, err := resp.ParseInt(c.Arg(2))
	if err != nil {
		return errNotInteger
	}
	n, err := c.DB().SetRange(c.Arg(1), offset, c.Arg(3))
	if err != nil {
		return engineError(err)
	}
	if len(c.Arg(3)) > 0 {
		c.Dirty(1)
	}
	return resp.Int(n)
}
