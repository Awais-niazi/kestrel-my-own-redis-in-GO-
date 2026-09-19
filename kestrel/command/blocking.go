package command

import (
	"strconv"
	"sync"
	"time"

	"kestrel/engine"
	"kestrel/resp"
)

// Blocking commands (FR-5.3).
//
// A blocked client here simply waits. The reference implementation cannot do
// that -- one thread serves every client, so it parks the client, records the
// keys, and revisits them after each command -- but a connection already has
// a goroutine of its own, and letting it wait costs a goroutine that was
// going to sit in a read anyway.
//
// What that buys is that a blocking command is the non-blocking one in a
// loop: try, register interest, try again, wait. The retry after registering
// is what makes it correct -- without it a value arriving between the first
// attempt and the registration would be missed, and the client would wait
// for a push that had already happened.

// Blocked tracks clients waiting for a key to gain a value.
//
// It is notified by the engine on every write, under a shard lock, so the
// notification does nothing but a map lookup and a non-blocking send. Any
// modification wakes a waiter, including one that cannot possibly have
// helped: a spurious wake costs a retry, and filtering would mean deciding
// inside the engine what a waiter was hoping for.
type Blocked struct {
	mu      sync.Mutex
	waiters map[blockKey]map[*waiter]struct{}
}

type blockKey struct {
	db  int
	key string
}

type waiter struct {
	ready chan struct{}
	keys  []blockKey
}

// NewBlocked returns an empty registry.
func NewBlocked() *Blocked {
	return &Blocked{waiters: make(map[blockKey]map[*waiter]struct{})}
}

// KeyModified wakes everything waiting on a key.
func (b *Blocked) KeyModified(db int, key []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for w := range b.waiters[blockKey{db: db, key: string(key)}] {
		select {
		case w.ready <- struct{}{}:
		default:
			// Already signalled and not yet observed. One wake is enough:
			// the waiter re-reads the keyspace, so it will see whatever
			// arrived.
		}
	}
}

// Wait registers interest in keys and returns a channel that is signalled
// when one of them is written, along with the function that deregisters.
//
// The caller must re-check the keys after calling this and before waiting.
func (b *Blocked) Wait(db int, keys [][]byte) (<-chan struct{}, func()) {
	w := &waiter{ready: make(chan struct{}, 1), keys: make([]blockKey, 0, len(keys))}

	b.mu.Lock()
	for _, k := range keys {
		bk := blockKey{db: db, key: string(k)}
		set := b.waiters[bk]
		if set == nil {
			set = make(map[*waiter]struct{})
			b.waiters[bk] = set
		}
		set[w] = struct{}{}
		w.keys = append(w.keys, bk)
	}
	b.mu.Unlock()

	return w.ready, func() { b.remove(w) }
}

func (b *Blocked) remove(w *waiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, bk := range w.keys {
		if set := b.waiters[bk]; set != nil {
			delete(set, w)
			if len(set) == 0 {
				delete(b.waiters, bk)
			}
		}
	}
	w.keys = nil
}

// Waiting reports how many keys have a client blocked on them, for INFO and
// tests.
func (b *Blocked) Waiting() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.waiters)
}

// parseTimeout reads the seconds argument common to every blocking command.
// Zero means wait indefinitely.
func parseTimeout(arg []byte) (time.Duration, bool) {
	secs, err := strconv.ParseFloat(string(arg), 64)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

// blockUntil runs attempt until it produces a reply, a timeout elapses, or
// the server stops.
//
// attempt returns a reply and whether it succeeded. A failed attempt means
// "nothing there yet", not an error; an error is returned as a successful
// attempt carrying an error reply, because that must reach the client rather
// than leaving it blocked.
func blockUntil(c *Ctx, keys [][]byte, timeout time.Duration,
	attempt func() (resp.Value, bool)) resp.Value {

	if v, ok := attempt(); ok {
		return v
	}
	// Inside EXEC there is nobody to wait for: no other client can write
	// while the transaction holds the write locks, so blocking would be a
	// deadlock the caller could not break. The reference implementation
	// returns the empty result for the same reason.
	if c.Client.inExec {
		return resp.NullArray()
	}

	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}

	blocked := c.Host.Blocked()
	for {
		ready, cancel := blocked.Wait(c.Client.DBIndex, keys)
		// The retry happens after registering. A value arriving between the
		// previous attempt and this registration would otherwise be missed,
		// and the client would wait for a push that had already happened.
		if v, ok := attempt(); ok {
			cancel()
			return v
		}
		var timedOut bool
		c.Yield(func() {
			select {
			case <-ready:
			case <-deadline:
				timedOut = true
			case <-c.Host.Quit():
				timedOut = true
			}
		})
		cancel()
		if timedOut {
			return resp.NullArray()
		}
	}
}

func init() {
	for _, r := range []struct {
		name    string
		front   bool
		summary string
	}{
		{"BLPOP", true, "Removes the first element of a list, waiting for one to arrive."},
		{"BRPOP", false, "Removes the last element of a list, waiting for one to arrive."},
	} {
		front := r.front
		register(&Descriptor{
			Name: r.name, Arity: -3,
			Flags:    Write | Blocking,
			Effect:   EffectCanonical,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: -2, Step: 1,
			Categories: []string{"write", "list", "slow", "blocking"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return blockingPop(c, front) },
		})
	}
	register(&Descriptor{
		Name: "BLMOVE", Arity: 6, Flags: Write | DenyOOM | Blocking,
		Effect: EffectCanonical, Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"write", "list", "slow", "blocking"},
		Summary:    "Moves an element between lists, waiting for one to arrive.",
		Handler:    cmdBLMove,
	})
	register(&Descriptor{
		Name: "BRPOPLPUSH", Arity: 4, Flags: Write | DenyOOM | Blocking,
		Effect: EffectCanonical, Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 2, Step: 1,
		Categories: []string{"write", "list", "slow", "blocking"},
		Summary: "Moves the last element of a list to the front of another, " +
			"waiting for one to arrive. Deprecated in favour of BLMOVE.",
		Handler: cmdBRPopLPush,
	})
	for _, r := range []struct {
		name    string
		highest bool
		summary string
	}{
		{"BZPOPMIN", false, "Removes the lowest-scoring member, waiting for one to arrive."},
		{"BZPOPMAX", true, "Removes the highest-scoring member, waiting for one to arrive."},
	} {
		highest := r.highest
		register(&Descriptor{
			Name: r.name, Arity: -3,
			Flags:    Write | Blocking,
			Effect:   EffectCanonical,
			Locality: LocalityShardLocal,
			FirstKey: 1, LastKey: -2, Step: 1,
			Categories: []string{"write", "sortedset", "slow", "blocking"},
			Summary:    r.summary,
			Handler:    func(c *Ctx) resp.Value { return blockingZPop(c, highest) },
		})
	}
}

var (
	effLPop    = []byte("LPOP")
	effRPop    = []byte("RPOP")
	effZPopMin = []byte("ZPOPMIN")
	effZPopMax = []byte("ZPOPMAX")
	effLMove   = []byte("LMOVE")
)

// blockingPop implements BLPOP and BRPOP.
//
// The keys are tried in the order given, which is what makes the argument
// order meaningful: a client listing a priority queue first expects it
// served first when both have work.
func blockingPop(c *Ctx, front bool) resp.Value {
	keys := c.Args[1 : c.Len()-1]
	timeout, ok := parseTimeout(c.Arg(c.Len() - 1))
	if !ok {
		return errTimeout
	}
	verb := effRPop
	if front {
		verb = effLPop
	}

	return blockUntil(c, keys, timeout, func() (resp.Value, bool) {
		for _, key := range keys {
			vals, err := c.DB().LPop(key, 1, front)
			if err != nil {
				return engineError(err), true
			}
			if len(vals) == 0 {
				continue
			}
			c.Dirty(1)
			// The effect is the non-blocking pop. Propagating the command
			// as written would make a replica block on its own stream.
			c.Propagate(verb, key)
			return resp.ArrayOf([]resp.Value{resp.Bulk(key), resp.Bulk(vals[0])}), true
		}
		return resp.Value{}, false
	})
}

func blockingZPop(c *Ctx, highest bool) resp.Value {
	keys := c.Args[1 : c.Len()-1]
	timeout, ok := parseTimeout(c.Arg(c.Len() - 1))
	if !ok {
		return errTimeout
	}
	verb := effZPopMin
	if highest {
		verb = effZPopMax
	}

	return blockUntil(c, keys, timeout, func() (resp.Value, bool) {
		for _, key := range keys {
			members, err := c.DB().ZPop(key, 1, highest)
			if err != nil {
				return engineError(err), true
			}
			if len(members) == 0 {
				continue
			}
			c.Dirty(1)
			c.Propagate(verb, key)
			return resp.ArrayOf([]resp.Value{
				resp.Bulk(key),
				resp.Bulk(members[0].Member),
				resp.Bulk(engine.FormatFloat(members[0].Score)),
			}), true
		}
		return resp.Value{}, false
	})
}

func cmdBLMove(c *Ctx) resp.Value {
	srcFront, ok1 := parseSide(c.Arg(3))
	dstFront, ok2 := parseSide(c.Arg(4))
	if !ok1 || !ok2 {
		return errSyntax
	}
	timeout, ok := parseTimeout(c.Arg(5))
	if !ok {
		return errTimeout
	}
	return blockingMove(c, c.Arg(1), c.Arg(2), srcFront, dstFront, timeout)
}

func cmdBRPopLPush(c *Ctx) resp.Value {
	timeout, ok := parseTimeout(c.Arg(3))
	if !ok {
		return errTimeout
	}
	return blockingMove(c, c.Arg(1), c.Arg(2), false, true, timeout)
}

func blockingMove(c *Ctx, src, dst []byte, srcFront, dstFront bool,
	timeout time.Duration) resp.Value {

	return blockUntil(c, [][]byte{src}, timeout, func() (resp.Value, bool) {
		v, err := c.DB().LMove(src, dst, srcFront, dstFront)
		if err != nil {
			return engineError(err), true
		}
		if v == nil {
			return resp.Value{}, false
		}
		c.Dirty(1)
		c.Propagate(effLMove, src, dst, sideName(srcFront), sideName(dstFront))
		return resp.Bulk(v), true
	})
}

func sideName(front bool) []byte {
	if front {
		return []byte("LEFT")
	}
	return []byte("RIGHT")
}
