# Documented deviations from the reference implementation

Kestrel targets *behavioural* compatibility on the commands it implements,
not file-format compatibility (§3.3, NG7). Where behaviour differs anyway,
it is listed here rather than left to be discovered.

## SCAN cursor semantics

**Deviation.** The cursor is a shard index, not a reverse-binary bucket
cursor. Each call drains whole shards until it has produced at least `COUNT`
keys, then returns the next shard index, or `0` when the iteration is done.

**Why.** Go's built-in map exposes no stable iteration order, so the
reference cursor cannot be implemented on top of it (ADR-004). Building the
custom hash table that would allow it is the pre-planned follow-up that
ADR-004 flags and Q2 asks about.

**What this means in practice.**

- *Stronger* in one respect: because each shard is drained under its own
  lock, a key present for the whole iteration is returned **exactly once**,
  where the reference guarantees only "at least once".
- *Weaker* in another: `COUNT` bounds the reply from below, not above. A
  single call can return as many keys as one shard holds, so with 16 shards
  and 1M keys a reply can carry ~62k keys. Raising `shards` lowers the
  per-call ceiling.
- A cursor is meaningful only against the same shard count. Changing
  `shards` requires a restart anyway.
- Total work across a full iteration is O(n), the same as the reference.

### HSCAN, SSCAN and ZSCAN

These return the whole collection in one call with a zero cursor. That is
what the reference implementation does for the compact encodings, and this
implementation extends it to the promoted ones for the same reason as above:
Go's map exposes no stable iteration order, so a resumable cursor over a
promoted hash or set cannot be built on it. `COUNT` is accepted and ignored.

For a listpack-encoded collection, which is the common case, the behaviour is
identical to the reference. For a very large hash or set the reply is large,
in the same way `HGETALL` on such a key is already large.

## INCRBYFLOAT rendering

**Deviation.** Results are rendered as the shortest decimal string that
round-trips exactly through a `float64`. The reference renders `%.17Lf` from
an 80-bit long double and strips trailing zeros.

**Why.** Go has no long double. Applying the reference's formatting to a
`float64` surfaces representation noise: `10.5 + 0.1` would print as
`10.59999999999999964`. The shortest round-trip form prints `10.6`, matches
what a user expects, and is exactly reversible — which is what replay safety
actually requires.

**What this means.** Values accumulate at `float64` precision rather than
long-double precision, so a long chain of increments can drift from the
reference in the 16th significant digit.

## UNLINK is synchronous

`UNLINK` behaves exactly like `DEL`. There is no lazy-free thread, so
claiming the memory is released asynchronously would be false. Noted in
Appendix A as a v2 item.

`FLUSHDB ASYNC` and `FLUSHALL ASYNC` are accepted and behave as `SYNC`, for
the same reason.

## OBJECT ENCODING names

Encoding names describe Kestrel's representations. `raw` and `int` match the
reference; the collection encodings will report `listpack`, `quicklist`,
`intset`, `hashtable` and `skiplist` when M2 lands, but they describe Go data
structures with different memory characteristics from their C counterparts.

`OBJECT REFCOUNT` always returns 1: values are never shared between keys, so
there is no shared-integer pool to report on. `OBJECT IDLETIME` and
`OBJECT FREQ` return 0 until eviction metadata is populated in M6.

## MEMORY USAGE is an estimate

Per-key memory is computed from an estimate maintained on write, not measured
(ADR-013). Expect it within roughly 15% of the true resident cost. `INFO
memory` reports `used_memory_is_estimated:1` so this is visible without
reading the documentation.

The practical consequence is that `maxmemory` should be set to 60-70% of the
container limit, not the 90% that is reasonable for a C implementation.

## SORT does not support BY and GET

`SORT key [LIMIT offset count] [ASC|DESC] [ALPHA] [STORE dst]` works.
`SORT ... BY pattern` and `SORT ... GET pattern` return an explicit
unsupported error rather than being ignored, because a client that asks for a
pattern sort and silently receives an unsorted answer is worse off than one
that gets an error. Appendix A records this as a v1 limitation.

`SORT_RO` is provided as the read-only variant.

## Set encodings follow Redis 7.2, not 7.0

A small set of non-integer members uses the listpack encoding, as the
encoding table in §6.2 of the PRD specifies. Redis 7.0 and earlier have no
listpack set and report `hashtable` for the same data. `OBJECT ENCODING`
therefore differs from a 7.0 server, and matches a 7.2 one.

## No RDB or AOF file compatibility

`DUMP`, `RESTORE` and `MIGRATE` return an explicit unsupported error.
Migration from an existing deployment is over the wire, not over files
(NG7, §3.3).

## Default port is 6380

So that Kestrel and Redis can run side by side during migration testing.

## INFO reports redis_version

`INFO server` includes `redis_version:7.2.0` alongside `kestrel_version`,
because a number of client libraries gate feature detection on it. It is the
compatibility target this implementation aims at, not a claim to be that
software.

## Not implemented at all

`EVAL` and scripting (NG2), streams (NG4), cluster mode (NG1), `FAILOVER`
(NG5), HyperLogLog, `BITFIELD`, `LCS`, `GEO*` and `LATENCY` all return an
error that names the decision and points at the documentation, rather than
the generic unknown-command error. Attempts are counted in
`unsupported_command_attempts` and exported as
`kestrel_unsupported_commands_total`, which is the metric that should drive
what gets built next (R4).
