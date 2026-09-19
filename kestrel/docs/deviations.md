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

## `snapshot-batch-keys` bounds record size, not the lock hold

The name suggests the snapshotter processes a shard in batches of keys,
releasing the shard lock between them. It does not, and cannot: a write
arriving between two batches would be visible in one and not the other while
both are covered by a single anchor offset, which makes recovery replay that
write on top of a value that already contains it.

The directive instead caps how many collection elements go into one rebuild
command, which bounds the size of a single record. The reasoning, and what
the resulting pause actually costs, are in `docs/design-notes.md` issue 9.

## Connections wait rather than receiving `-LOADING` during startup recovery

The reference implementation binds its port, accepts connections, and replies
`-LOADING` to most commands while it reads its dataset from disk.

Kestrel binds its listeners before recovery but does not accept from them
until recovery has finished, so a client that connects during a replay waits
in the kernel's backlog instead. The port is reserved either way, so nothing
is refused, and the admin server's `/ready` endpoint reports the truth
throughout. A client with a connect timeout shorter than the replay will time
out where Redis would have given it an error to retry on.

The `LOADING` machinery exists in the command table and is used by the
`loading` flag, so switching to the reference behaviour is a change of
ordering in `Serve` rather than new code. It is written down here because the
difference is visible to a client, not because it is hard to change.

## `SAVE`, `BGSAVE` and `BGREWRITEAOF` are three names for one operation

In the reference implementation these are genuinely different. `SAVE` and
`BGSAVE` write an RDB file; `BGREWRITEAOF` rewrites the append-only file; the
two formats coexist and an operator chooses between them.

Kestrel has one durability story -- a snapshot, plus the log segments written
after it -- so a snapshot *is* the compaction. All three commands take a
snapshot, roll the log and unlink the segments the snapshot made redundant.
`SAVE` blocks until it is done; the other two return immediately.

They are kept as three commands rather than collapsed into one because
tooling calls whichever it was written against, and an operator running
`BGREWRITEAOF` to reclaim disk gets exactly that. `BGSAVE SCHEDULE` is
accepted and reports honestly: there is nothing to defer, because the
maintenance loop takes the next snapshot anyway.

`LASTSAVE` reports the time of the last successful snapshot, and is seeded at
startup rather than left at zero, so that a server which has not yet
snapshotted does not look like one whose save failed in 1970.

## `repl-backlog-size` describes log retention, not a buffer

The reference implementation keeps a fixed-size in-memory ring of recent
commands for partial resynchronisation, sized by `repl-backlog-size`, and
separate from the append-only file.

Kestrel serves a replica straight from the log segments, which already hold
every effect in order and addressed by stream offset. There is no second
buffer to size, so `repl-backlog-size` is accepted and reported but does not
allocate anything: how far back a replica can resynchronise is decided by how
much log is retained, which follows from `snapshot-interval` and the
auto-rewrite directives.

The practical difference is in Kestrel's favour. A reference backlog can
overflow and force a full resynchronisation while the records the replica
wants are still on disk in the AOF. Here, a partial resynchronisation is
refused only when the segment holding those records has actually been
unlinked.

## A transaction excludes other writes, but readers can see it part-way

In the reference implementation a single thread means nothing observes a
half-finished `MULTI`/`EXEC`. Kestrel executes a transaction while holding
every shard's write-ordering lock, so no other *write* can interleave -- but
a concurrent reader is not excluded, and may see some of a transaction's
commands applied and not others.

Making readers take that lock would put a transaction's cost on every `GET`
in the server, which is the wrong trade for a feature most workloads use
rarely. What the current guarantee preserves is the part correctness rests
on: no interleaved write, so no lost update, and `WATCH`'s compare-and-set is
sound under contention.

`docs/design-notes.md` issue 18 has the reasoning, including why holding the
global barrier instead would deadlock.
