# Design notes and open issues

Findings from building M0 and M1 that affect later milestones. Each is stated
with the failing case, not just the concern.

---

## 1. Fuzzy snapshots are not correct for cross-key read-modify-write effects

**Severity: correctness. Blocks M3.**

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

### Recommended fix

Two parts, both small:

1. **Serialize shards in ascending index order** so per-shard offsets are
   monotonically non-decreasing. This is already the natural implementation
   and should be written down as an invariant, because part 2 depends on it.

2. **Exclude cross-shard read-modify-write effects from the snapshot
   window.** Give the snapshotter a lock it holds (shared) for its entire
   duration, and have the affected commands take it exclusively. No such
   effect can then have an offset inside `[min(offset), max(offset)]`, so the
   per-shard filter is never asked to apply one against inconsistent shard
   states.

   The cost is that `RENAME` and the `*STORE` family block for the duration
   of a snapshot. Those commands are rare in the workloads §4 describes, and
   the alternative — blocking every write — is what ADR-009 rejected. This
   should be measured and documented as a known pause, and surfaced as a
   metric (`cross_key_command_blocked_seconds`).

An alternative worth pricing: materialize for scalar values below a size
threshold and fall back to the barrier only above it. That keeps the common
`RENAME` of a small string entirely lock-free.

**Until M3 lands**, `RENAME`, `RENAMENX` and `COPY` propagate the operation
rather than a materialized value, matching the reference implementation. The
code carries a pointer to this note so the decision is not silently
inherited.

---

## 2. SWAPDB and per-shard snapshot offsets

**Severity: correctness. Affects M3, and is evidence for Q3.**

`SWAPDB` exchanges the shard arrays of two databases. Per-shard snapshot
offsets are recorded per database *and* per shard, so a `SWAPDB` inside the
snapshot window moves a shard's data out from under the offset recorded for
it. Replaying a record that targets `db0/shard3` against what is now
`db1/shard3` is straightforwardly wrong.

`SWAPDB` is already implemented behind the global barrier write lock, so the
fix is the same shape as issue 1: exclude it from the snapshot window
entirely. It is cheap to do, because `SWAPDB` is already a barrier operation.

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
