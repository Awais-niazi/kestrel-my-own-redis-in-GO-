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
