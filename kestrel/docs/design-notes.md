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

Both have the same fix, which is deliberately not in this chunk: **group
commit.** Batch the records that arrive while a write or an fsync is in
flight, and issue one `write` and one `fsync` for the batch. Under `always`
the batch amortizes the fsync across every client waiting on it, which is
where the four orders of magnitude come back; under `everysec` it collapses
the syscall queue.

It is not in this chunk because it is an optimization with a correctness
surface -- a batch that reports success to a client whose record was in a
failed write is a lie about durability -- and it should be built against a
recovery path that can prove it, which is chunk C. The numbers above are the
baseline it has to beat.

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
now serialize across the handler and the log append. That is not really new:
`Log.Append` already serializes every append globally, so the marginal loss
is the handler's own execution time.

### Remaining gap: lazy expiry on the read path

A read that finds an expired key reaps it and emits a `DEL`, under the shard's
`mu` but without `prop`, because taking `prop` on every read would put every
`GET` behind a concurrent write's log append.

The window is narrow -- a key must fall due in the instant between a writer
mutating it and logging it, and the writer's own lookup would have reaped it
on the way in -- but narrow is not closed. The fix worth pricing: have reads
*hide* an expired key, as replica mode already does, and leave the deletion
and the `DEL` to the active cycle, which holds `prop`. That makes reads
cheaper as well, and it makes `active-expire no` a memory-leak setting rather
than a correctness one, which should be stated if it is taken.

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
