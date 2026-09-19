# Kestrel — a Redis-compatible data store in Go

**The project lives in [`kestrel/`](kestrel/). Start with
[`kestrel/README.md`](kestrel/README.md).**

Kestrel is a RESP-compatible in-memory data store written from scratch in Go.
Unmodified Redis clients — `redis-cli`, go-redis, redis-py, Jedis, ioredis —
connect to it without changes. It is not a Redis distribution and is not
affiliated with Redis Ltd.

```sh
cd kestrel
make build
./build/kestreld ./kestrel.conf --appendonly no

# in another shell
redis-cli -p 6380
```

The default port is **6380**, not 6379, so Kestrel and Redis can run side by
side.

## Where things are

| Path | What it is |
|---|---|
| [`kestrel/`](kestrel/) | The project. Its own Go module, `module kestrel`. |
| [`kestrel/README.md`](kestrel/README.md) | Status, the full command list, layout, conformance and performance. |
| [`kestrel/docs/design-notes.md`](kestrel/docs/design-notes.md) | Findings that changed the design, each with the failing case that produced it. |
| [`kestrel/docs/deviations.md`](kestrel/docs/deviations.md) | Every place Kestrel knowingly differs from Redis, and why. |
| `Leaning/` | An unrelated hello-world from when this directory was a Go scratch area. |

The repository root is still that scratch area — `module learning`, one
`hello.go` — because that is what it was before Kestrel was started here.
Kestrel is a separate module underneath it and shares nothing with it. The
root module is kept rather than deleted so the history stays honest about
where the repository came from.

## Status

M0 (skeleton), M1 (core key/value), M2 (collections), M3 (persistence) and
M4 (replication) and M5 (Pub/Sub, transactions, blocking commands) are
complete: 171 working commands, strings, lists, hashes, sets and sorted sets with adaptive
encodings, expiration, `SCAN`, RESP2 and RESP3, TLS, and a metrics endpoint.

M5 (Pub/Sub, transactions and blocking commands) is complete.

M4 (replication) is complete: a replica follows a leader over a socket, is
fed straight from the leader's log segments rather than a separate backlog,
and persists what it applies at the leader's own offsets — so a restarted
replica resumes with no transfer at all. `WAIT` and `min-replicas-to-write`
work.

M3 (persistence) is complete: **a restart keeps your data.** With
`appendonly yes` the server logs the canonical effect of every write and
rebuilds the keyspace from it on startup, TTLs included. Snapshots run on a
timer, on log growth, or on `SAVE`/`BGSAVE`/`BGREWRITEAOF`, and compaction
unlinks whole log segments rather than rewriting the log — a 251 KB log came
down to 13 KB in the round trip that verified it.

The per-milestone table is in [`kestrel/README.md`](kestrel/README.md#status).

## What this repository is for

It is a personal implementation, built to understand the problems a data
store actually has rather than to ship a Redis replacement. The parts that
make that worthwhile are the ones that are usually invisible:

- **Every write command declares how it replicates, and how far it reads.** A
  command that cannot be replayed verbatim must produce a canonical rewrite,
  and one that answers neither question panics at registration rather than
  starting a server that will diverge from its own log.
- **The interesting bugs are written down, not just fixed.**
  `docs/design-notes.md` records each one with the failing case that exposed
  it. Two of them had survived four milestones underneath passing
  differential tests: one hidden because leader and follower shared a clock,
  the other because the tests drove a single session and a lone writer
  cannot reorder against itself.
- **Deviations from Redis are deliberate and listed.** Commands that are out
  of scope return an error naming the decision that excluded them, and get
  counted, so the compatibility backlog comes from real traffic.

## Development

```sh
cd kestrel
make check       # gofmt, go vet, and the tests under the race detector
make test        # tests only
make cover       # coverage report
make bench       # benchmarks, including the allocation gate
make fuzz        # the protocol and log fuzzers
make run         # build and start on port 6380
```

Go 1.22 or newer. The only non-standard-library dependency is
`github.com/cespare/xxhash/v2`, used for shard selection.
