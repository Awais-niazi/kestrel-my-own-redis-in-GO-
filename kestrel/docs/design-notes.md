# Design notes and open issues

Findings from building M0 and M1 that affect later milestones. Each is stated
with the failing case, not just the concern.

---

## 1. Fuzzy snapshots are not correct for cross-key read-modify-write effects

**Severity: correctness. Resolved — mechanism landed ahead of M3.**

§7.3 argues that chunked fuzzy snapshots recover correctly because "every
effect targets a known key set, hence a known shard set, and the per-shard
filter reconstructs exactly all effects this shard had not yet seen when it
was serialized, in original order."

That argument holds for effects that only **write** the shards they touch. It
does not hold for effects that **read one key and write another** when the
two keys land in different shards.

### The failing case

Shards `A` and `B`. The snapshotter records `offset[B] = 50` and, later,
`offset[A] = 100`. At log offset 75 the leader executed:

```
RENAME src dst      # src hashes to A, dst hashes to B
```

Recovery loads the snapshot and replays from `min(offset) = 50`:

- `A`'s snapshot content reflects the log through offset 100, so `src` has
  **already** been removed by the rename.
- `B`'s snapshot content reflects the log through offset 50, so `dst` does
  **not** hold the value.
- At record 75 the filter applies the record to shards whose offset is ≤ 75.
  `B` qualifies (50 ≤ 75). The command runs — and reads `src` from `A`,
  where it no longer exists.

`RENAME` fails, `dst` is never written, and the value is silently lost. If
`src` had been re-created after offset 75 with a different value, `dst` would
instead be written with the *wrong* value: divergence rather than loss, which
is worse.

### Which commands are affected

Anything whose write depends on the current value of a key in another shard:

- **M1 (today):** `RENAME`, `RENAMENX`, `COPY`
- **M2:** `SMOVE`, `RPOPLPUSH`, `LMOVE`, `SINTERSTORE`, `SUNIONSTORE`,
  `SDIFFSTORE`, `ZUNIONSTORE`, `ZINTERSTORE`, `ZDIFFSTORE`, `ZRANGESTORE`,
  `SORT ... STORE`
- **M6+:** `BITOP`, `PFMERGE`, `GEORADIUS ... STORE`

Single-key read-modify-write commands (`INCR`, `APPEND`, `LPUSH`,
`SETRANGE`) are **not** affected: read and write are the same key, therefore
the same shard, therefore the same snapshot instant. The §7.2 claim that
`INCR` is safe to log verbatim is correct for exactly this reason, and it is
worth stating the reason explicitly, because it is what fails for the
cross-key case.

### Why the obvious fixes do not work

*Skip the effect when the read shard is ahead.* The write shard's snapshot
does not contain the effect, so skipping loses the write.

*Apply only when every touched shard has offset ≤ R.* Same problem: the write
shard still needs it.

*Materialize every such effect as a pure write* (`SET dst <value>` + `DEL
src`). Correct, and cheap for scalars. For collections it is exactly the
write amplification ADR-008 rejected when it turned down physical logging: a
`ZUNIONSTORE` over a million members would write a million members to the
log.

### Resolution

Two parts, both small, both now in place.

1. **Shards are serialized in ascending index order**, databases likewise, so
   the per-shard offsets a snapshot records are monotonically non-decreasing
   and the window is a single interval. This was already the natural
   implementation; it is now written down as an invariant on
   `Keyspace.SnapshotWindow`, because part 2 depends on it.

2. **Cross-shard read-modify-write effects are excluded from the snapshot
   window.** `Keyspace.SnapshotWindow` holds a guard for the whole
   serialization pass, and the affected commands hold it around both their
   execution and the propagation of their effect. No such effect can then
   carry an offset inside `[min(offset), max(offset)]`, so the per-shard
   filter is never asked to apply one against inconsistent shard state.

   The guard has to span propagation, not just keyspace access: it is the
   effect's *log offset* that must fall outside the window, and the offset is
   assigned when the effect is propagated.

**Correction to the original write-up.** The first version of this note had
the lock polarity backwards -- the snapshotter holding it shared and the
commands exclusively. That would have serialized every cross-shard command
against every other while permitting two concurrent snapshots, which is both
slower and unsound. The requirement is mutual exclusion between the snapshot
and the set of cross-shard commands, and nothing more: the snapshotter takes
the guard exclusively, the commands share it.

### Making the declaration mandatory

Marking the affected commands with a flag only catches the ones someone
remembers to mark, and the property cannot be inferred from the command
table: `ZUNIONSTORE` declares `FirstKey` and `LastKey` of 1, naming only its
destination, so a key-specification check would miss it.

So locality is a **required declaration**, in the same shape and for the same
reason as `Effect` (ADR-008): `Descriptor.Locality` must be
`LocalityShardLocal` or `LocalityCrossShard` on every write command, and
`register` panics on a write command that leaves it unset. A new write
command cannot be added without answering the question.

The 16 commands that answer `LocalityCrossShard` today are listed in
`crossShardCommands` in `command/locality_test.go`, which fails if the table
and the list disagree. `SORT` is in it unconditionally even though only
`SORT ... STORE` reads across shards; the declaration is static and `SORT_RO`
covers the read-only case, so the over-approximation is cheaper than making
it dynamic.

### Cost

`RENAME`, `SWAPDB` and the `*STORE` family block for the duration of a
snapshot. Those are rare in the workloads §4 describes, and the alternative
-- blocking every write -- is what ADR-009 rejected. Outside a snapshot the
guard is an uncontended `RLock`; `BenchmarkGet` is unchanged at 0
allocations. The pause should be surfaced as a metric
(`cross_shard_command_blocked_seconds`) when the snapshotter lands.

An alternative that was considered and not taken: materialize the effect as a
pure write (`SET dst <value>` + `DEL src`) below a size threshold, falling
back to the guard above it. That keeps a `RENAME` of a small string entirely
lock-free, but it puts two propagation paths in the commands most likely to
diverge. It is worth pricing again if the snapshot pause measures badly.

---

## 2. SWAPDB and per-shard snapshot offsets

**Severity: correctness. Resolved by the same mechanism as issue 1, and still evidence for Q3.**

`SWAPDB` exchanges the shard arrays of two databases. Per-shard snapshot
offsets are recorded per database *and* per shard, so a `SWAPDB` inside the
snapshot window moves a shard's data out from under the offset recorded for
it. Replaying a record that targets `db0/shard3` against what is now
`db1/shard3` is straightforwardly wrong.

`SWAPDB` is already implemented behind the global barrier write lock, but the
barrier is only held at snapshot *start*, so a `SWAPDB` part-way through a
serialization pass was still possible. It now declares `LocalityCrossShard`
and is excluded from the whole window by the issue 1 guard. The mechanism
covers both problems because they are the same problem: an effect whose
correct replay depends on shard state the snapshot recorded at a different
instant.

This is worth noting as a small win for the design: `SWAPDB` needed no
special case.

This is a concrete data point for **Q3** ("do we support multiple logical
databases?"). Multiple databases cost real complexity in the snapshot and
recovery path, and `SWAPDB` in particular buys very little. The
recommendation is to keep `SELECT 0-15` (client libraries and test suites
expect it, and it is nearly free) and to **drop `SWAPDB`**, which is the only
part that complicates M3.

---

## 3. Reading the clock is not uniformly cheap

**Severity: performance. Affects the §8.1 budget and R1.**

On the development host used here, `time.Now()` costs **158 ns** and
`time.Since` **114 ns** — this machine has no vDSO fast clock path, which is
common in VMs and containers. Two clock reads per command for latency
accounting therefore cost more than an entire `GET`, and the engine's expiry
check added a third on every TTL'd key access.

Two mitigations are in place:

- The engine can read a clock cached at millisecond resolution
  (`Options.CachedClock`, enabled by the server), which is what the reference
  implementation does for the same reason. A TTL'd `GET` went from 395 ns to
  200 ns.
- Per-command latency timing can be disabled with
  `track-command-latency no`, which skips both reads and keeps call and error
  counts.

**Implication for §8.1.** The performance targets should specify the clock
source of the measurement host, and the benchmark gate in §11 should record
it. A CI runner without a vDSO clock will report numbers that have nothing to
do with the code under test.

---

## 4. Where the remaining dispatch overhead is

Profiling M1 turned up three costs worth knowing about before M2 adds
commands on top of them:

| Cost | Was | Fix |
|---|---|---|
| Per-command stats behind a mutex and map | 20% of dispatch | Atomic counters in an array indexed by command |
| Hit/miss counters as shared atomics | contended across all cores | Per-shard counters under the shard lock |
| `resp.Simple("PONG")` converting a string per call | 15% of a `PING` | Reply values built once at init |
| Command name upper-casing before table lookup | one allocation per command | Table indexed under both cases; lookup keyed on the request bytes |
| Heap-allocated execution context per command | one allocation per command | Reused per client |

A `GET` now costs zero allocations end to end, gated by
`TestGetReadPathAllocations`. This matters more than the raw nanoseconds:
allocation rate drives GC frequency, and GC drives the p99.9 tail that R1
identifies as the largest risk in the project.

---

## 5. SCAN needs the custom dictionary sooner than ADR-004 assumes

ADR-004 treats the custom hash table as a performance follow-up. It is also
the only way to implement the reference `SCAN` cursor, because Go's map has
no stable iteration order. The shard-index cursor shipped here is a
reasonable compromise with a genuinely stronger exactly-once property, but
`COUNT` bounds the reply from below rather than above, which is an
operational hazard on a large keyspace.

**Q2 should be answered as: document the deviation for v1, and schedule the
custom dictionary for immediately after GA**, where it resolves ADR-004's
rehash-pause risk (R6), the pointer-density GC cost, and the `SCAN` deviation
in one project rather than three.

---

## 6. Effect propagation earns its place before persistence exists

The effect stream is fully built and tested at M1 even though nothing
consumes it yet, and `TestFollowerConverges` already replays 6000 randomized
writes into a second keyspace and requires byte-identical convergence.

This was worth doing early. Every write command has had to answer "how do you
replicate?" from the first one written, which is the mitigation R5 asks for,
and M3 and M4 attach to a propagation path that is already exercised rather
than introducing one late and discovering its bugs under a fsync workload.

---

## 7. What `appendfsync` actually costs, and where the append log contends

Measured on the development host (Intel i7-8665U, ext4 on NVMe), one
`SET key value` record:

| `appendfsync` | ns/op | effective ceiling | allocations |
|---|---|---|---|
| `no` | 1055 | ~950k writes/s | 0 |
| `everysec` | 1343 | ~745k writes/s | 0 |
| `always` | 4,575,000 | **~220 writes/s** | 0 |

Two things worth stating plainly.

**`always` is four orders of magnitude slower**, not a percentage slower. It
is a correct implementation of the durability contract -- an acknowledged
write is on stable storage -- and that contract costs a disk revolution or a
flash program per command, because each append is forced on its own. The
number belongs in the operator documentation next to the directive, because
"slower" does not prepare anyone for 220 writes a second.

**The log is a contention point.** `BenchmarkAppendParallel` runs 4376 ns/op
against 1343 serial: eight goroutines appending are three times *slower* per
operation than one. The `write(2)` happens under the log mutex, so
concurrency turns into queueing at a syscall.

Both have the same fix: **group commit**, which is now implemented. See
issue 24 for what it did and what it cost to get right.

### Why the log does not reuse the `resp` package

A record payload is a RESP2 array, but `persist` encodes it with twenty lines
of its own rather than importing `resp`. A connection's writer switches
dialect when a client sends `HELLO 3`, and a log whose encoding could follow
a client's protocol negotiation would be a file whose meaning depends on who
happened to be connected when it was written. The duplication is the point:
the log's dialect is fixed at RESP2 and cannot drift with the wire protocol.

### Stream offsets run across files

A record's offset is its position in the *stream*, not in the file. A rewrite
starts a new file whose header carries the offset the previous log reached,
so an offset a snapshot recorded before a compaction still names the same
point afterwards. Restarting the counter per file would invalidate every
snapshot anchor on the first rewrite, which is the moment those anchors
matter most.

---

## 8. Replay needs a third expiry mode, and the convergence test needed a skewed clock

Two findings from building recovery, one a design gap and one a real bug the
first was needed to expose.

### Loading mode

Replaying a log re-executes writes the leader performed in the past against a
clock that is now, so every TTL in the log has usually already elapsed. A log
holding

```
PEXPIREAT k <t>
APPEND k more
DEL k              # emitted when the key actually expired at t
```

replays as an `APPEND` to a key recovery has already made vanish, so the
recovered value is `more` where the leader had `originalmore`.

Replica mode is not enough. It *hides* an expired key rather than deleting
it (FR-3.4), and a hidden key is a missing key to the next write in the log:
`APPEND` still creates a fresh one. So there is a third mode,
`Keyspace.SetLoading`, in which expiration does not happen at all. Every
removal comes from the log, where the leader recorded it, and the recovered
keyspace matches the leader's at the instant the log ends. Clearing the flag
hands the remainder to the ordinary lazy and active paths.

`DB.Expire` needed the same treatment, for a less obvious reason: a TTL at or
before the present *deletes* the key. That is correct online, and wrong
during a replay where every timestamp is in the past by definition. The
leader propagated `DEL` rather than `PEXPIREAT` for the keys it really
expired, so the log already says which ones go.

This turned eight scattered `o.ExpireAt > 0 && o.ExpireAt <= now` conditions
into one `Keyspace.expiredAt`. Eight places to remember a new mode is eight
ways for a recovered keyspace to differ from the log that produced it.

### The bug: `INCRBYFLOAT` was making expiring keys permanent

`INCRBYFLOAT` cannot be logged verbatim, because float accumulation would
drift on replay, so it is canonicalized to `SET key <result>` (ADR-008). But
`INCRBYFLOAT` leaves a key's expiry alone and a plain `SET` clears it. The
log therefore asserted "this key is permanent" about a key that was not.

The consequence was not a wrong value. It was that every replica and every
restart quietly resurrected keys that should have expired, for as long as
they were still being incremented. The fix is one argument -- `KEEPTTL` --
and the audit that followed found no second instance: `SPOP`→`SREM`,
`ZINCRBY`→`ZADD`, `HINCRBYFLOAT`→`HSET`, `MSETNX`→`MSET` and the `GETEX`
family all rewrite to commands with the same expiry semantics as the
original.

### Why `TestFollowerConverges` missed it for two milestones

The convergence test runs leader and follower on the *same clock*. Both
therefore cleared the TTL, both kept the key, and the two agreed --
incorrectly, and invisibly.

The recovery test that found it does the one thing the replication test
cannot: it replays under a clock an hour ahead, which is what every real
restart does. That skew turns a shared misconception into a visible
divergence, and it found the bug on its first run.

The lesson generalizes past this one command. **A differential test whose two
sides share an input cannot see a bug in how that input is used.** The clock
was the shared input here; the same argument applies to a shared random seed,
a shared configuration snapshot, and a shared command table. Where a
production pair would differ, the test pair should differ too.

---

## 9. The snapshot holds a shard lock for the whole shard, and that is a real pause

### Why the lock cannot be released part-way through a shard

ADR-009's "chunked" means the chunks are shards. A shard is serialized under
one hold of its lock, and the log offset at that moment becomes its anchor.

It is tempting to release the lock between batches of keys inside a shard,
because `snapshot-batch-keys` sounds like it is asking for exactly that. It
is not safe. A write arriving between two batches is visible in the second
and not the first, while the anchor names a single offset for both -- so
recovery, seeing that write at an offset after the anchor, replays it on top
of a value that already contains it. For `SET` that is redundant. For `INCR`,
`APPEND` or `RPUSH` it is a wrong value.

So `snapshot-batch-keys` is reinterpreted: it caps how many collection
elements go into one rebuild command, bounding record size. It does not
bound the lock hold. This is a deviation from what the name suggests and is
recorded in `docs/deviations.md`.

### What it costs

`BenchmarkWriteSnapshot` walks 20,000 keys plus 400 collections in 7.2 ms
with 501 allocations total -- about 2.8M keys/second, and the allocations are
per shard and per collection copy rather than per key. The walker itself is
cheap.

The pause is not, and it is now measured rather than estimated.
`TestSnapshotShardPauseByShardCount` serializes a 200,000-key database and
reports the worst single shard hold:

| Shards | Worst shard hold | Whole pass |
|---|---|---|
| 16 | 6.47 ms | 55 ms |
| 64 | 875 µs | 35 ms |
| 256 | 279 µs | 35 ms |

Commands hashing to other shards run normally throughout, so this is not a
full stop -- but at 16 shards it is already past the p99.9 budget in §8.1 for
the shard it lands on, and a 10M-key dataset is fifty times larger.

Three ways out, in increasing order of cost to build:

1. **Raise the shard count.** The pause divides by it, and very nearly
   linearly: sixteen times the shards gave twenty-three times the
   improvement above, because the smaller maps are also friendlier to the
   cache. The whole pass gets faster too. This is nearly free and should be
   the first answer; ADR-003's shard count was chosen for lock contention,
   not for snapshot latency, and it turns out the two want the same thing.
   The test asserts the shape of this relationship, so it will be noticed if
   it stops holding.
2. **Copy the shard under the lock and serialize outside it.** Turns a long
   CPU hold into a short one plus a memory spike of one shard's worth of
   deep copies. Cheaper in latency, worse in peak memory, and it needs a
   real `Clone` for every collection type.
3. **Fork, and let the kernel do copy-on-write**, which is what the
   reference implementation does. It removes the pause almost entirely and
   costs a page-table copy plus unbounded memory growth under a write-heavy
   load. It is also awkward in Go, where a fork without exec is not
   supported for a multi-threaded runtime.

Option 1 is the recommendation, and it now has the measurement. Nothing here
is urgent while `snapshot-interval` defaults to 900 s, but an operator
turning that down, or running a dataset much larger than the benchmark's,
should raise `shards` at the same time.

### Why a snapshot reuses the log's framing

A snapshot is a sequence of framed records in the same format as the log:
rebuild commands, interleaved with anchors that name the offset each shard
was taken at. Anchors travel as a distinct `RecordKind` rather than as a
specially named command, so replay cannot be talked into executing one.

The payoff is that one checksum, one decoder and one fuzz target cover both
files, and recovery is one function rather than two. The cost is size and
load speed: a sorted set is stored as `ZADD` arguments where a packed binary
encoding would be smaller. For a dataset that fits in memory by definition,
the format is not the bottleneck -- and the alternative is a second binary
format with its own corruption modes to get right.

---

## 10. The log was not recording effects in the order they were applied

**Severity: correctness. Affected log recovery and replication, not only
snapshots.**

Found by the first test that ran concurrent writers against a snapshot, and
it is the most serious defect the project has turned up so far.

### The failing case

A command mutates the keyspace under a shard's data lock and releases it.
Its effect reaches the log afterwards, from `dispatch.propagate`. Nothing
kept those two steps together, so two goroutines writing the same key could
interleave like this:

```
A: SET k 1     mutates, releases the shard lock
B: SET k 2     mutates, releases the shard lock
B: appends "SET k 2"
A: appends "SET k 1"
```

The keyspace holds `2`. The log says `1`. Every replica built from that
stream holds `1`, and so does the dataset after the next restart. Nothing
reports an error at any point.

The test that found it saw an `MSET` key end as `131` on the live host and
`x` on the restored one.

### Why nothing caught it for four milestones

`TestFollowerConverges` and `TestRecoveryConverges` both drive a single
session, issuing one command at a time. A single writer cannot reorder
against itself, so both tests were asking a question the bug is invisible
to -- the same shape of blind spot as issue 8's shared clock, from a
different direction.

**Two differential tests had agreed with each other for four milestones
while the thing they were testing was wrong.** The lesson is not that these
tests are bad; it is that a convergence test only covers the concurrency it
actually creates.

### The fix

A second lock per shard, `shard.prop`, taken by the command layer before the
handler runs and released after the effect has been propagated. Two writes
to the same shard therefore enter the log in the order they were applied.
Writes to different shards still run concurrently, and reads never take it.

It is a separate lock from the shard's `mu`, and always taken first, because
it must be held for longer: `mu` covers the mutation, `prop` covers the
mutation *and* the record of it reaching the log.

The active expiry cycle takes it too. It reaps on its own goroutine and each
reap emits a `DEL`, so without it that `DEL` is free to interleave with a
write that is between mutating a key and logging it.

Commands whose key arguments do not describe the shards they touch -- the
cross-shard set, plus `FLUSHALL` and `SWAPDB` -- take every shard. They are
rare and already blocked for the duration of a snapshot, so the blunt answer
is the right one: a wrong lock set here is a silently misordered log.

### It also makes the snapshot anchor exact

The same lock closes a second hole that had not yet been found. An anchor is
supposed to mean "this shard holds exactly the effects of the records before
this offset". Reading the log's offset without `prop` could catch a write
that had already mutated the shard but had not yet been logged, so its
record would sit *after* the anchor and be replayed on top of itself. For
`SET` that is redundant; for `INCR` it is off by one.

`SnapshotShard` now takes `prop`, reads the offset, takes `mu`, and releases
`prop`. Holding `prop` means nothing is mid-flight; taking `mu` before
letting go means nothing new can land until the shard has been serialized.

### What it costs

One extra uncontended mutex per write -- `BenchmarkSet` and `BenchmarkIncr`
show no measurable change, and `BenchmarkGet` is untouched at 0 allocations
because reads do not take it. Under contention, writes to the *same shard*
now serialize across the handler and the log append.

When this was written that cost nothing, because `Log.Append` serialized
every append globally anyway. Group commit (issue 24) removed that: appends
to different shards now proceed together and batch, so `prop` is the
remaining serialization, and it is per shard rather than global.

### The gap this left on the read path, and how it was closed

A read that found an expired key reaped it and emitted a `DEL`, under the
shard's `mu` but without `prop`, because taking `prop` on every read would
put every `GET` behind a concurrent write's log append.

The window was narrow -- a key had to fall due in the instant between a
writer mutating it and logging it -- but narrow is not closed, and what came
out of it was a log that replayed to a different keyspace than the one that
wrote it. Issue 26 is how it was fixed, and what the proposed fix got wrong.

---

## 11. Compaction by deleting segments rather than rewriting the log

The append log has to stop growing, and there are two ways to do it.

**Rewrite in place**, which is what the reference implementation does. Take a
snapshot, write a fresh log holding only the records the snapshot did not
already capture, and swap it in. Writes keep arriving while the copy runs, so
they accumulate in a rewrite buffer that is flushed into the new file just
before the swap. That buffer is unbounded in principle and is where the
reference implementation's memory use surprises people.

**Segment the log and unlink whole files**, which is what Kestrel does.

A snapshot already names the offset below which every record is dead: its
first anchor. So the log is a sequence of files

```
kestrel-000001.log
kestrel-000002.log
```

and compaction is

1. commit the snapshot, atomically, as it already was;
2. **roll** -- start the next segment at the offset the stream has reached,
   which is a create and a pointer swap, moving no data;
3. unlink every segment whose range ends at or below the snapshot's first
   anchor.

Nothing is copied, no buffer accumulates writes, and a ten gigabyte log costs
an `unlink` rather than ten gigabytes of I/O. It works because stream offsets
already run across files -- the `Base` field that chunk B put in the file
header was put there for this.

### Why rolling only at snapshot time

There is no segment-size directive, and there should not be one. A segment is
started only when a snapshot has just been committed, which makes the steady
state exactly two segments: the one straddling the current snapshot's first
anchor, and the live one. The straddler is unlinked at the next snapshot, so
the log settles at roughly two snapshot intervals of writes with no knob to
tune and no way to set it wrong.

### Crash safety falls out of the ordering

The snapshot is renamed into place before any segment is unlinked. A crash
between the two leaves a good snapshot and some redundant segments, whose
records recovery skips. A crash during the snapshot leaves a temporary file,
which is discarded. There is no instant at which a record that is still
needed has been deleted.

### Damage in a segment that is not the last is always fatal

`corrupt-log-policy truncate` truncates the last segment, which is the only
one a crash can tear. Damage anywhere earlier is refused whatever the policy,
because truncating there would orphan every segment after it -- discarding
far more than an operator asked for, and silently.

A gap between segments, which an operator creates by deleting a file by hand,
is refused for the same reason: the missing records are in no file and there
is no way to tell which they were.

### The one-time migration

A data directory written by the previous build holds a single `kestrel.log`.
It is renamed to `kestrel-000001.log` on startup, once, and only when no
segments exist. It costs ten lines and it saves an operator from a server
that starts up empty and looks fine.

---

## 12. The log is the replication backlog

A replica needs "every effect from offset X onwards, then whatever comes
next". That is exactly what the append log already holds: records in order,
addressed by a stream offset that spans files and survives compaction.

So Kestrel has no separate replication backlog. There is no ring buffer to
size, nothing to keep in memory in parallel with the log, and no second copy
of the same records to keep consistent with the first.

**`repl-backlog-size` therefore becomes a statement about log retention**
rather than about a buffer, and retention is already decided by how often
snapshots run. That is a deviation and it is written down, but it is a
simplification rather than a compromise: the reference implementation's
backlog can overflow while the AOF still holds the records, which is a
partial resynchronisation refused for no reason that exists on disk.

### A partial resynchronisation is a seek

A record's position in its file is `fileHeaderSize + (offset - base)`, so
reaching an offset is an `lseek`, not a scan. `TestFollowSeeksRatherThanScans`
pins this: without it, every catch-up after an end-of-file would re-read the
segment from the start, and a live tail would be quadratic in the number of
records.

The case where a partial resynchronisation is impossible is precisely the
case where compaction has unlinked the segment holding those records, and the
log can answer that exactly -- `OldestOffset` -- rather than by guessing at
what a buffer still contains.

### A follower cannot see a half-written record

`Follower` never reads past the offset the log reports, and that offset only
advances once a whole record has been handed to the operating system. So a
torn read is impossible by construction rather than something the reader has
to detect and recover from, and damage seen while following is real damage.

This matters because the same bytes mean different things in the two paths:
recovery finding a partial record at the end of a file is an ordinary crash
artefact, while a follower finding one would be a bug.

### Waking without polling

`Log.Appended` returns a channel closed by the next append. A closed channel
is the cheapest broadcast Go has, and unlike a `sync.Cond` it composes with a
`select` on a context, so a follower can be released by a write, a
cancellation, or the log closing, without a timer anywhere.

The channel is taken *before* the offset it is waiting past is re-checked. The
other order has a race that loses a record: the follower reads the offset,
a write lands, and only then does it subscribe -- and it then sleeps with data
available.

---

## 13. A full resynchronisation is the restart path, sent over a socket

The leader answers `PSYNC` in one of two ways, and neither is a new
mechanism.

**Partial.** The replica quotes this leader's replication id and an offset
that is still retained, so the link resumes with a seek into a segment and no
transfer at all. Issue 12 covers why that is a seek rather than a scan.

**Full.** The leader sends the current snapshot file, then the log from that
snapshot's earliest anchor. That is exactly the pair of files a restart
reads, and the replica applies them with exactly the code a restart uses:
`LoadSnapshot`, then a filtered replay of the stream. The per-shard anchors
travel inside the snapshot, so the filter a replica needs arrives with the
data it filters.

A consequence worth stating: **the leader does not take a snapshot per
replica.** Pruning never removes a segment below the current snapshot's first
anchor, so the snapshot already on disk plus the log after it is always a
complete and consistent full sync. A fresh one is taken only if none has ever
been written.

### Records travel in their on-disk framing, not as RESP commands

The stream after the handshake is the framed records themselves. So the
checksum a replica verifies is the one the leader wrote, over the bytes the
leader wrote -- not one recomputed from arguments that have already been
decoded and assumed correct. The database index travels in the frame, so
there are no `SELECT` records to interleave and no per-connection database
state to keep in step.

It also means the bytes on the wire are the bytes in the file, so a replica's
log is identical to the leader's for the range they share.

### Acknowledgements travel the other way on the same connection

A replication link is a stream in one direction and a trickle of `REPLCONF
ACK` in the other. The leader reads them on a second goroutine, which is safe
because a `net.Conn` supports concurrent reads and writes, and cancels the
link when that read ends -- a replica that has gone away stops being detected
by a failed write alone, which may not happen until the next write.

`REPLCONF ACK` is the one command that must not be replied to. Its reply
would be written into a socket the replica is parsing as a record stream, and
would be read as a corrupt frame.

### The handover

`PSYNC` cannot be served by a command handler, because a handler returns a
reply and this connection stops having replies. The handler records what was
asked for on the client and returns nothing; the connection loop sees the
request, flushes, and hands the socket to the replication code without
returning to the loop. The command layer decides that a handover is wanted;
the server decides whether it can be granted.

---

## 14. The replica applies the leader's stream with the code a restart uses

A replica's full synchronisation is: receive the snapshot, write it to a
file, load it, then apply the record stream with the snapshot's anchors as a
filter. Every one of those steps is the restart path from chunk E, called
with a socket in place of a directory.

Writing the received snapshot to a file before loading it, rather than
parsing it from memory, is deliberate. A second loader would be a second
place for the shipped format and the stored format to disagree, and the
disagreement would show up as a replica that is quietly wrong rather than as
a parse error.

The anchor filter stays installed for the life of the link. It does not need
removing: once the stream passes the last anchor, every shard qualifies for
every record and the filter stops excluding anything on its own.

### What a replica does not do yet, and why that is a decision

A replica applies to its keyspace and does not write its own log, so a
replica restart is a full resynchronisation.

Persisting a replica's log needs an invariant the leader does not have. A
shipped snapshot is usable only once the stream has reached the end of the
window its anchors span: before that, different shards hold different
instants, and the offset the replica would record does not describe any of
them. A replica that crashed mid-transfer and restarted from
`snapshot + partial log` would come up with shards at inconsistent points and
an offset claiming they were all together.

The rule that makes it safe is checkable -- local state is usable only when
`log.Offset() >= snapshot.Last` -- and it is worth building deliberately
rather than discovering after a crash. Until then, a replica restart costs a
full transfer, which is correct and merely slower.

### Reconnecting is where a partial resynchronisation earns its keep

Within one process lifetime a replica knows the leader's replication id and
its own offset, so a dropped link resumes with a seek and no transfer.
`TestReplicaResumesAfterLinkLoss` asserts that the reconnect did *not* cost a
full synchronisation, because a test that only checks the data would pass
just as happily if every reconnect shipped the whole dataset.

### A note on the first replica

A leader with no snapshot takes one when its first replica connects, which
also rolls and prunes the log. That is visible in `INFO`: `repl_oldest_offset`
jumps close to `master_repl_offset`, so for a short window a replica that
drops and reconnects needs a full transfer. The reference implementation has
the same dynamic when its backlog is small relative to the write rate. It is
worth knowing before turning `snapshot-interval` down on a leader with
replicas.

---

## 15. What WAIT actually promises, and what makes it prompt

`WAIT numreplicas timeout` is easy to read as something stronger than it is,
so the promise is worth stating exactly: **at the moment it returns, this
many replicas had acknowledged an offset at least as high as the one this
leader had reached when WAIT began.**

It is not a transaction. It does not make the writes before it atomic, it
does not roll anything back if the count comes up short, and a replica that
acknowledged can still be lost a moment later. What it gives is a bound on
how much is only on one machine.

Two details that follow from being precise about it:

**The target is sampled once.** A write arriving during the wait does not
move the goal. Otherwise a leader taking writes continuously could never
satisfy a `WAIT`, because the thing being waited for would recede at the rate
it was being produced -- and the caller is asking about the writes *they* had
already made.

**A zero timeout means "do not wait", not "wait forever".** That is what the
reference implementation means by it, and it is the opposite of what the word
suggests, so it is pinned by a test rather than left to be discovered.

### Acknowledgements are sent on catch-up, not only on a timer

A replica acknowledges whenever it drains its buffer, and on a one-second
timer as a fallback for an idle link. The timer alone would put up to a
second of latency under every `WAIT` for no reason at all.

Catching up is the right signal because it is exactly what `WAIT` is waiting
for. Under a burst the buffer is rarely empty, so it costs one acknowledgement
per burst rather than one per record; under a trickle it costs one per record,
which is thirty bytes upstream for a write that has already travelled further
than that.

The reference implementation solves this by sending `REPLCONF GETACK` down
the replication stream. That is not available here: the stream carries framed
records whose sizes advance the replica's offset, so injecting a control
record would either desynchronise the offset or need a special case in the
one piece of arithmetic that must not have one.

### min-replicas-max-lag is measured from the last acknowledgement

Not from the last byte written. A leader with nothing to send would otherwise
watch every healthy replica fall out of sync the moment the write rate
dropped to zero, and start refusing writes precisely because there were none
-- which is both wrong and the hardest kind of outage to diagnose.

---

## 16. When a replica's own files can be trusted

A replica now writes the records its leader sends into its own log, unchanged
and at the leader's offsets. So its log is byte-identical to the leader's
over the range they share, and a restarted replica can quote a position its
leader recognises with no translation at all.

### The invariant

Local state is usable only when **`log.Offset() >= snapshot.Last`**.

A snapshot's shards are serialized at different instants, so the state it
describes is complete only once the log has reached the end of the window its
anchors span. Below that point different shards hold different moments and no
single offset describes any of them -- which is exactly what a replica that
crashed part-way through a transfer has on disk.

The check runs at startup on every node, not only replicas:

- **A replica** with incomplete state warns, discards it, and
  resynchronises. It has somewhere to get the truth from.
- **A leader** refuses to start. It does not, and coming up with a hole in
  the dataset while reporting success is the worst outcome available.

A snapshot whose window is a single point -- `First` equal to `Last`, which
is what a snapshot of a quiet server produces -- is self-contained, and the
check can never fire for it. That is correct, and it is why the test for this
has to load the server while it snapshots: a quiet snapshot has nothing to be
short of.

### Switching histories is ordered so no crash leaves a lie

On a full resynchronisation the replica: writes the incoming snapshot to a
temporary file and forces it; loads it, so a snapshot that cannot be read
never replaces one that can; **resets the log**; renames the snapshot into
place; records the leader's replication id.

The log is discarded *before* the snapshot is renamed. A crash between the
two leaves the old snapshot with no log, which the invariant reads as
unusable and resynchronises. The other order would leave a new snapshot
beside a log from the old history, whose offsets mean something else
entirely -- and nothing about the files would say so.

### The replica applies with the dispatcher's locking discipline

The replica's applier does not go through `Execute`, so it was taking neither
the write-ordering lock nor the cross-shard guard. On a replica that persists
and snapshots, both are needed, for exactly the reasons they are needed on a
leader:

- Its snapshots anchor each shard to a log offset, and that anchor is exact
  only if no write to the shard is between having been applied and having
  been logged. Doing the two in either order without a lock across both
  makes the anchor wrong in one direction or the other -- the shard missing
  a record the anchor says it has, or holding one the anchor says it does
  not -- and recovery then loses a write or applies it twice.
- A cross-shard effect must stay out of a snapshot window, and a replica
  takes its own snapshots, so it needs its own exclusion.

So `Replayer.ApplyStreamed` takes the same locks `Execute` does. It was
tempting to reason that a single applier goroutine cannot race with itself;
that is true and irrelevant, because the thing it races with is the
snapshotter.

### An offset without a history is worse than no offset

A restarted replica reads its offset from its log, but an offset is only
meaningful in one leader's numbering: quoted to a different leader it names a
different write. So the replication id is written to `kestrel.leader` beside
the snapshot, and a replica offers a position only when it knows whose it is.

This also caught a bug worth recording. The first version read the resume
offset from the in-memory counter when the leader answered `+CONTINUE`. On a
reconnect that is right; on a *restart* the counter is zero while the offset
actually asked for came from disk, so the replica silently restarted the
stream numbering from zero and never caught up. The fix is to resume at the
offset that was asked for, which is the only one both ends agreed on.

---

## 17. Pub/Sub: who owns the socket, and who pays for a slow reader

A subscriber's connection goroutine spends almost its whole life blocked in a
read, so something else has to write to it. Two obvious answers are both
wrong.

**The publisher writes directly.** One slow reader then holds up every
publisher in the server, because a `write` to a full socket buffer blocks.
Pub/Sub turns into a distributed liveness problem, and the client that caused
it is not the one that suffers.

**Every message is buffered per subscriber without limit.** One slow reader
then becomes an out-of-memory kill for everyone.

So each subscriber gets a **bounded queue and its own delivery goroutine**.
The publisher does a non-blocking send: if the queue is full the subscriber is
marked and disconnected, and `PUBLISH` does not count it, because the count is
the only thing `PUBLISH` tells the caller and reporting a delivery that did
not happen is worse than reporting none. This is the same trade the reference
implementation makes with its pub/sub output buffer limit.

Two goroutines then write to one connection, so the writer is behind a mutex
-- but only once a delivery goroutine exists. An ordinary connection never
takes it, which matters because it sits on the path every command takes.

The delivery goroutine is started *after* the subscribe confirmation has been
flushed. Started before, a message about a channel could overtake the
confirmation that the client had subscribed to it.

### Every message is flushed

A subscriber is waiting for news. Batching messages behind a buffer that may
never fill turns a notification system into a polling one, so each push is
flushed on its own.

### A bug the race detector found, and the contract that predicted it

`PUBLISH channel payload` hands the payload straight from the publisher's
read buffer -- which that connection reuses for its next command. A queued
message outlives the call and is written by a different goroutine, so the
first version had one connection overwriting bytes another was serialising.

The engine's concurrency contract says exactly this: arguments alias the read
buffer and are valid only for the duration of the call. The rule was written
down two milestones earlier and still took the race detector to enforce. The
payload is now copied once in `Publish`, at the boundary where it stops
belonging to the caller, rather than per subscriber.

### Why RESP2 subscribers are restricted and RESP3 ones are not

In RESP2 a message is an ordinary array, indistinguishable from a reply. A
client that sent `GET` and received an array cannot tell whether it is the
answer or news about a channel, so a RESP2 subscriber may only send commands
whose replies it can still account for.

RESP3 gives pushes their own type, so the ambiguity does not exist and
neither does the restriction. Both are one `Kind` in the writer: the
difference between the dialects is framing, not content.

---

## 18. Transactions: what EXEC excludes, and what it does not

`MULTI` queues; `EXEC` runs the queue with no other write interleaved. That
exclusion is bought by taking every shard's write-ordering lock for the
duration -- the same lock the dispatcher takes for one command, held across
all of them.

It is not the reference implementation's guarantee, which is total: a single
thread means nothing at all observes a half-finished transaction. **Here a
concurrent reader can.** Readers do not take the ordering lock, and making
them take it would put a transaction's cost on every `GET` in the server.

What the weaker guarantee still gives is the part correctness depends on: no
interleaved write, therefore no lost update, and `WATCH`'s compare-and-set is
sound. `TestCompareAndSetUnderContention` runs eight clients racing a
read-modify-write loop and checks the counter equals the number of
transactions that reported success.

Holding the global barrier instead would have given full isolation and would
also have deadlocked: handlers take the barrier's read lock through
`db.lockKey`, and a Go `RWMutex` is not reentrant. That is worth writing down
because it is the first thing anyone will reach for.

### The watched-key check is inside the lock

Checking whether a watch was broken *before* taking the ordering lock would
race with exactly the writer the check exists to detect. So `EXEC` takes the
locks first and checks second.

### A queue error poisons the transaction

A command that cannot be queued -- unknown, wrong arity -- marks the
transaction dirty and `EXEC` refuses with `EXECABORT`. Skipping it and
running the rest would be a transaction the caller did not ask for, and the
missing command would have to be inferred from the reply count.

An error discovered while *running* is different and is reported in its own
slot, with the remaining commands still run. The two cases are distinguishable
at queue time, which is why they are treated differently.

### EXEC writes an array header rather than collecting values

Some handlers write their output directly instead of returning a value -- the
subscribe confirmations do, and so does `EXEC` itself. Collecting a
transaction's replies into a slice would mean giving those a second way to
produce a reply, so `EXEC` opens an array with `WriteArrayHeader` and each
queued command writes into it exactly as it would outside a transaction.

---

## 19. A conformance bug that crashed redis-cli

`COMMAND DOCS` reported a container's children by their bare names -- `get`,
`set` -- where the reference implementation reports `config|get`,
`config|set`.

`redis-cli` builds its command hint table from those names and splits each on
the separator to recover the parent. Given a name with no separator it
**segfaults**, so any `redis-cli` session that reads commands from a pipe --
which is how most scripts drive it -- died instantly against Kestrel.

The reply was valid RESP and parsed cleanly. Every test passed. The bug was
found by piping a few commands to a real `redis-cli` while checking something
else entirely, and it had been there since M0.

Two things worth taking from it.

**A bare subcommand name is ambiguous anyway.** Several containers have a
`GET`, so `get` does not identify the command it describes. The correct
behaviour was also the better one, which is usually the case and is worth
noticing when it is.

**Protocol conformance is not RESP validity.** Differential testing against
`redis-server` compares replies, and both servers' replies were well formed;
what differed was a convention a *client* depends on. Running the real client
is a different test from comparing the two servers, and it found something
the other could not.

---

## 20. Blocking commands, and the lock a blocked client must not hold

A blocked client here simply waits. The reference implementation cannot: one
thread serves everyone, so it parks the client, records the keys it wants,
and revisits them after every command. A connection here already has a
goroutine, and letting it wait costs a goroutine that was going to sit in a
read anyway.

That makes a blocking command the non-blocking one in a loop: try, register
interest, **try again**, wait. The second attempt is what makes it correct.
Without it, a value arriving between the first attempt and the registration
is missed, and the client waits for a push that has already happened.

### The deadlock this chunk was really about

The first version deadlocked, and the failure looked exactly like a client
that had stopped responding: `BLPOP queue 0` blocked every `RPUSH queue` in
the server -- **including the one that would have woken it.**

The dispatcher holds the write-ordering lock across a write's execution and
the propagation of its effect (issue 10). A blocking command is a write, so
it was holding the lock for its keys while it waited, and the write it was
waiting for needed that same lock.

The fix is `Ctx.Yield`: the handler gives the locks back, waits, and takes
them again before returning. So the attempt that finally succeeds mutates
and propagates under them, exactly as a non-blocking write does, and the
attempts that fail hold nothing.

It is worth noticing what found this. The unit tests passed; the failure
appeared the moment a test used two connections, because with one connection
there is nobody to be deadlocked against.

### Blocking commands propagate their non-blocking form

`BLPOP` logs `LPOP`, `BLMOVE` logs `LMOVE`. Propagating the command as
written would make a replica block on its own replication stream, waiting for
a push that will only arrive as a later record it is not reading yet. ADR-008's
rule -- a command that cannot be replayed verbatim must declare a canonical
rewrite -- is what forces the question to be answered.

### Inside MULTI they do not block

Nothing can write while a transaction holds the write locks, so waiting would
be a deadlock the caller could not break. A queued blocking command returns
its empty result instead, which is what the reference implementation does.

They are queued like any other command: marking them `NoMulti` -- which the
first version did -- made them run immediately instead, in the middle of a
transaction being built.

### Fairness is not guaranteed

The reference implementation serves the longest-waiting client first. Here
several blocked goroutines are woken together and race to retry, so which one
wins is whichever the scheduler runs first. Each element still goes to
exactly one client, which is what makes a work queue work, and
`TestOnlyOneWaiterGetsEachElement` pins that. Strict FIFO would need a
handoff queue per key and is not worth it until someone needs it; the
deviation is recorded.

---

## 21. Eviction, and two sampling mistakes worth recording

`maxmemory` now evicts rather than only refusing. The policy picks victims by
sampling, not by an exact ordering: a true LRU list means two pointers per
object and a write to three cache lines on every read, to answer a question
that only has to be approximately right. The reference implementation makes
the same trade.

Measured on a real server: 20,000 keys written against a 2 MB limit left
11,304, evicted 8,696, and settled at 2,000,030 bytes -- within thirty of the
limit.

### The limit is a level, not a ceiling

Eviction runs *before* a write, so the write that follows puts the dataset
back over by its own size. A server under memory pressure therefore hovers
just above `maxmemory` rather than staying strictly below it. That is the
reference behaviour too, and it is worth stating because the obvious test --
"is it under the limit?" -- fails on a sixteen byte overshoot and looks like
a bug.

### Uniform sampling was wrong, and looked right

The first version picked a database and a shard at random for each sample.
With sixteen databases and data in one of them, fifteen of every sixteen
samples landed on an empty shard. All five samples could miss, `evictOne`
would report nothing to evict, and the server refused writes while sitting at
three times its limit.

Sampling now walks shards from a random starting point. It finds candidates
whenever any exist, still starts somewhere different each time, and allocates
nothing. Uniform independent picks are the intuitive choice and are only
correct when the data is spread the way the sampler assumes.

### The access clock is only maintained when a policy needs it

`lookup` records an access, which is a write on the read path. Guarding it
with one atomic bool means a server with no `maxmemory` set pays a load and
writes nothing, and `GET` stays at zero allocations. Only the LRU and LFU
policies set the flag; random and TTL policies do not need it and do not pay.

### One field serves both LRU and LFU

`Object.LRU` holds a coarse second counter for LRU, and for LFU packs a decay
minute with an eight-bit logarithmic counter. No policy uses both at once, so
one field does, and `Object` stays the size it was.

The counter is logarithmic -- the higher it is, the less likely an access
raises it further -- which is what lets eight bits distinguish a key read
twice from one read ten thousand times. A linear counter saturates almost
immediately and then ranks every popular key identically.

### A replica never evicts

Its leader's `DEL` arrives on the stream. Evicting independently would
diverge the two, and the replica would be missing keys the leader still
serves -- the same reasoning as expiry (FR-3.4).

---

## 22. MONITOR and CLIENT KILL

Both were smaller than expected, because the machinery already existed.

### MONITOR reuses the Pub/Sub delivery path

A monitor is a connection receiving output it did not ask for, which is what
a subscriber is. It goes on the same bounded queue and is delivered by the
same goroutine, and a monitor that cannot keep up is dropped for the same
reason a slow subscriber is.

The outbox was generalised from a Pub/Sub message to a rendered `resp.Value`,
which also removed work: a published message is now built once and handed to
every subscriber instead of being rebuilt per subscriber. A push renders
differently in RESP2 and RESP3, but each connection's writer decides that
from the one `Kind`, so one value serves all of them.

The cost when nobody is watching is one atomic load per command, which is why
the monitor count is kept apart from the set.

### Passwords are redacted

A monitor stream is a plain-text feed of everything the server does, and
therefore exactly the wrong place for a password. `AUTH` is redacted from its
first argument, and so is the `AUTH` clause of `HELLO`, which carries the
same secret in the same way and which the reference implementation also
hides.

### Monitors see refused commands

A command that was rejected is exactly what an operator turned `MONITOR` on
to find, so the feed happens before dispatch decides anything.

### Two bugs in CLIENT KILL, both about lifetime

**Disconnecting inside the walk deadlocks.** `ForEachClient` holds the client
table's lock; `Disconnect` takes it. The first version called one from inside
the other, in a function whose own comment warned against exactly that.
Victims are now collected and closed afterwards.

**Killing yourself must still deliver the reply.** `CLIENT KILL ... SKIPME
no` includes the caller. Closing its socket immediately loses the answer to
the command that asked, so the caller cannot tell whether it worked -- and
the count is the only thing `CLIENT KILL` returns. A self-kill now sets
`CloseAfterReply`, so the reply goes out and the connection closes after it.

### The socket is closed, not a flag set

A connection parked in a read, or blocked in `BLPOP`, will not notice a flag.
`CLIENT KILL` exists precisely for connections that are not behaving, so it
closes the socket and lets the connection's own goroutine discover it.

---

## 23. ACL: rules are ordered, permissions are resolved

A user is a set of permissions -- which commands, which keys, which channels.
Two decisions shape the implementation.

**Rules are applied in the order given.** `+@all -@dangerous` and
`-@dangerous +@all` mean different things, and a user who wrote the first and
got the second would have granted exactly what they meant to withhold. A test
pins both directions.

**Permissions are stored resolved, not as rules to interpret.** Each user
holds a map from command name to whether they may run it, built when the
rules are set. Checking is then a map lookup on the dispatch path, and a rule
touching a hundred commands costs once rather than per command.

`ACL SETUSER` applies to a copy and replaces the original only if every rule
is understood. A half-applied list is a user with permissions nobody wrote
down, and the operator who made a typo would be left guessing which of their
rules took effect.

### Subcommands inherit their parent's categories

`-@admin` removed `CONFIG` and left `CONFIG GET` reachable, because the
subcommands declared no categories of their own and so matched no category
rule. The whole of `CONFIG` was still available through a name the rule
appeared to cover.

A subcommand with no categories now inherits its parent's. Separately, a rule
naming a container grants or revokes its children too: a user who could run
neither `CONFIG GET` nor `CONFIG SET` has not been granted `CONFIG`.

### The ACL check cost two allocations on every GET

`CanRun` folded the command's name to lower case on each call, and the key
check extracted a command's keys before comparing them against "allow
everything". Both were on the path of every command.

The name is now folded once at registration, and a user who may touch every
key and every channel short-circuits before any key is extracted. That is the
default user on a server with no access control configured -- the
overwhelmingly common case, and the one that must cost nothing. `GET` is back
to zero allocations.

### requirepass and the default user are the same thing

An operator who sets `requirepass` expects authentication to work without
learning about ACLs, so setting it sets the default user's password, and
clearing it restores `nopass`. `CONFIG SET requirepass` moves both.

### What is not implemented

Selectors -- the `(...)` syntax for alternate permission sets -- and the
`%R~` / `%W~` read-only and write-only key patterns. Both are recorded in
`docs/deviations.md`. Passwords are SHA-256 hashed as in the reference
implementation, compared in constant time, and never stored or reported in
plaintext.

---

## 24. Group commit, and three ways it did not work

Issue 7 measured `appendfsync always` at about 220 writes a second -- a disk
flush per command -- and eight concurrent appenders running *slower* per
operation than one, because the `write(2)` happened under the log mutex.

Group commit fixes both: records are staged into a shared buffer, and one of
the waiting appenders writes the whole buffer out. N concurrent appends cost
one `write` and one `fsync` instead of N of each.

**Measured on a real server**, `appendfsync always`, 50 clients, 20,000
writes spread over 100,000 keys:

| | Before | After |
|---|---|---|
| Throughput | ~795 writes/s | **5,259 writes/s** |
| fsyncs per write | 1.00 | **0.14** (7.1 records per flush) |

An appender still returns only once the batch carrying its record has been
written, and forced under `always`. The guarantee is unchanged: an
acknowledged write is on stable storage. A batch that fails is reported as
failed to *every* caller whose record it carried, not only to whoever
happened to be doing the writing -- which is the lie group commit exists to
avoid introducing.

### It made the uncontended case three times slower

The first version staged every append and went through the condition
variable even when nothing else was in flight. `BenchmarkAppend/no` went from
1055 ns to 3668 ns and gained an allocation per call.

Two fixes. The pending and spare buffers are swapped rather than reallocated,
which removes the allocation. And an append that arrives with nothing else in
flight writes itself and skips the batching machinery entirely: **group
commit is worth what it costs only when there is a group.** That brought the
uncontended path back to 1234 ns.

### It never formed a batch at all, and every test passed

`Log.Append` held the *log's* mutex across the segment's append. Every writer
was therefore serialised before it could reach the batching code, so only one
append was ever in flight and a batch could never form.

Nothing in the test suite noticed, because every test that mattered was
checking correctness, and the code was correct -- it was simply doing no
batching. It was found by running `redis-benchmark` against a real server and
reading two numbers out of `INFO`: `aol_writes:20000, aol_fsyncs:20000`. A
ratio of exactly 1.00 is not a performance disappointment, it is a
mechanism that is not running.

The log's lock is now shared for appends and exclusive for rolling, pruning
and closing. The segment does its own ordering underneath.

**A performance feature needs a test that fails when it stops working.**
There is now one at each level: the log counts flushes against records, and
the server does the same through real connections. Both assert a ratio rather
than a rate, because a rate is a fact about the disk and the ratio is a fact
about the code.

### It deadlocked about one run in three

`drainLocked` waited on the flush condition without incrementing the waiter
count, and `flushLocked` only broadcast when that count was above zero. A
drainer could therefore sleep with nobody left to wake it.

That alone would be a hang in a background sync. What made it fatal is that
`Roll` drains while holding the log's exclusive lock, so a missed wake-up
wedged every append in the server, and shutdown with it.

The race suite found it, intermittently, in a test about snapshot scheduling
that has nothing to do with locking. Twelve targeted reruns failed to
reproduce it; it was found by reading the code for who could wait without
registering. **A conditional broadcast needs every waiter to be counted, and
a waiter added later is the one that will be forgotten.**

---

## 25. Group commit's fourth failure: it corrupted the log on a roll

Issue 24 recorded three ways group commit did not work. Here is the fourth,
found a chunk later, and the worst of them: **it wrote logs that could not be
read back.**

```
kestrel-000001.log ends at offset 77772 but kestrel-000002.log begins at 77729;
the records between them are in no file
```

The segments overlap by 43 bytes -- one record. A server that produced this
kept running and looked healthy. The damage was invisible until it was
restarted, and then it would not start at all.

### Two bugs, one decision

Both come from the same line. To make batching possible, `Log.Append` read
`l.cur` under the read lock and released it *before* appending:

```go
l.mu.RLock()
cur := l.cur
l.mu.RUnlock()
at, err := cur.Append(db, args)
```

That was a correction to a real problem -- issue 24 records how holding the
lock across the append serialised every writer so no batch could ever form --
but it was the wrong correction, because it was made against a `sync.Mutex`
and kept after the lock became a `sync.RWMutex`. A *read* lock does not need
releasing: appenders do not exclude each other under it. Releasing it bought
nothing and gave up the only thing it was doing.

**First:** `Roll` takes the write lock and closes the segment. An appender
that had already chosen that segment then appends to a closed file and its
write fails, for no reason the operator can see.

**Second, and worse:** an appender that gets its record in *after* `Roll` has
recorded where the file ends puts bytes past that recorded end. The next
segment is declared to begin at the same offset. Two files then claim the
same range, and `Segments` refuses the whole log.

### The end of a segment cannot be read before the segment stops growing

`Roll` also took the end from `l.cur.Offset()` *before* syncing:

```go
at := l.cur.Offset()   // what has reached the file
...
l.cur.Sync()           // writes what was still staged
l.segs[last].End = at  // too small by whatever was staged
```

Before group commit these were the same number, because every append reached
the file before it returned. Group commit introduced the gap between
*assigned* and *written* and this line was not revisited. `Roll` now closes
the segment first -- which drains it, and after which nothing more can be
appended -- and reads the end from the closed file.

**A number that was safe to read early is only safe while nothing buffers.**

### How it presented, and why that mattered more than the bug

The symptom was a test timing out after fifteen seconds with "server did not
answer PING". Not a corruption error -- a *hang*.

Startup binds its ports before recovery runs, deliberately, so a client
arriving during a long replay waits instead of being refused. But `Serve`
returned recovery's error without closing what it had already bound. The port
stayed open, accepted into the kernel backlog, and answered nothing. The test
harness polled that port, could not tell "still starting" from "gave up ten
seconds ago", and reported a timeout while the real error sat unread in a
channel.

That is a production failure in its own right, and a nastier one than the
corruption: **a health check that only dials would call that server healthy.**
`Serve` now unbinds everything it bound if startup does not complete, and the
test harness waits on the server's exit as well as on its port, so a startup
that fails reports the failure instead of a deadline.

**A test that cannot distinguish "not yet" from "never" will always report
the less useful of the two.**

### What found it

Nothing that was looking for it. The race detector never fired -- every
access is correctly locked; the locking is simply not held long enough. The
persistence tests rolled quiet logs and passed. It surfaced as an unrelated
snapshot test failing about one run in three under `-count=5`, and was
identified only after the harness was changed to report *why* the server was
not answering.

The test that now covers it appends from eight goroutines while a ninth rolls
the log in a loop, then requires the segments to be contiguous on disk and
every record to replay exactly once. Against the old code it fails both ways
on the first run.

**A concurrency fix needs a test that rolls under load, not a test that rolls.**

---

## 26. Lazy expiry: reads hide, writes reap, and the obvious fix was wrong

Issue 10 left one hole. A read that found an expired key reaped it and
appended a `DEL` to the effect stream, and a read holds no propagation lock,
so that `DEL` had no ordered position among concurrent writes. A log whose
records are out of order replays to a different keyspace than the one that
produced it.

Issue 10 proposed the fix in one line: **have reads hide an expired key, as
replica mode already does, and leave the deletion to the active cycle, which
holds the lock.** That is half right, and the other half would have been a
worse bug than the one it fixed.

### Why hiding everywhere breaks replay

Replay disables expiry. `Keyspace.SetLoading` makes `expiredAt` return false
for everything, because every timestamp in a log is in the past by the time
it is read, and a log that expired its own records as it replayed them would
delete the dataset it was restoring.

So nothing is hidden during replay. A write that had hidden an expired key
and said nothing about it is replayed against the key it thought was gone:

```
leader:  key l expires; LPUSH l b  ->  hides the old list, starts a new one,
                                       propagates "LPUSH l b"
replay:  nothing expires        ->  LPUSH l b appends to the old list
```

The leader holds `[b]`. The restored server holds `[a, b]`. The same
divergence reaches a replica whose clock has not yet passed the expiry.

Under the old behaviour this could not happen, because the write's own lookup
reaped the key and emitted `DEL l` ahead of the `LPUSH`. The log was
self-sufficient. Hiding on the write path would have quietly removed that.

### The split that is actually correct

A read has nothing to say and no position to say it from. A write already
holds the lock and is already appending. So:

- **Reads hide.** No reap, no `DEL`, nothing appended. The key stays until
  the active cycle takes it.
- **Writes reap.** Exactly as before, under `prop`, `DEL` first and then the
  write's own effect.

The engine says which it is by which accessor family it uses -- `peek`,
`peekRead`, `peekType` against `lookup`, `lookupType`, with `listAt`/
`listRead` and the same pairing for sets, sorted sets and hashes. Choosing
the wrong one of a pair is silent in both directions: a read that reaps
misorders the log, a write that hides desynchronises the replay.

Three methods serve both a read-only and a writing command over the same
key -- `SORT` and `SORT_RO`, `SUNION` and `SUNIONSTORE`, `ZUNION` and
`ZUNIONSTORE`. The difference is invisible inside the engine, so those take
an explicit `engine.Access`. `SORT` is a write: it replays from its own
arguments, so a replay re-reads its source and must see what the original run
saw.

### Making the classification checkable rather than careful

Fifty call sites were classified by hand, and a wrong one fails at the next
restart rather than at the call. `SetWriteOrderingAssertions` makes the
engine verify, at the point of emission, that the caller holds the shard's
propagation lock:

```go
if s.prop.TryLock() {
	s.prop.Unlock()
	panic("engine: effect emitted without the shard's propagation lock")
}
```

The check is one-directional by construction. A successful `TryLock` proves
nobody holds the lock, so there are no false positives; a failed one proves
only that *somebody* does, which under concurrency need not be this
goroutine, so a violation that races an unrelated writer is missed. It is off
in production and on for the whole of `command`'s and `server`'s test
binaries, which is where the dispatcher is the thing taking the lock.

It is deliberately *not* on for `engine`'s own tests. They call `DB` methods
directly and hold no propagation lock, so an always-on assertion would fire
on every write in the package rather than on the mistake.

### What it costs and what it gives up

Reads get cheaper: the expiry path on a read is now a comparison and a
return, with no deletion, no allocation of the key string, and no sink call.

The cost is that an expired key read but never written stays resident until
the active cycle reaches it. **`active-expire no` is now a memory-leak
setting rather than a correctness one**, and that is worth stating plainly to
anyone who sets it: with the active cycle off, expired keys are hidden
correctly and freed never.

`DBSIZE`, `KEYS`, `SCAN` and `RANDOMKEY` already filtered expired keys
without reaping them, so they needed no change -- they had been written
against the hiding rule all along.
