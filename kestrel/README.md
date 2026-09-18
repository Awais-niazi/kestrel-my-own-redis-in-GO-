# Kestrel

A RESP-compatible in-memory data store written in Go. Unmodified Redis
clients — `redis-cli`, go-redis, redis-py, Jedis, ioredis — connect to it
without changes.

This repository implements the design in `kestrel-prd.md`. It is not a Redis
distribution and is not affiliated with Redis Ltd.

## Status

**M0 (skeleton), M1 (core key/value), M2 (collections) and M3 (persistence)
are complete.** The server runs, real Redis clients talk to it, and strings,
lists, hashes, sets, sorted sets, generic keyspace commands, expiration and
durability all work.

| Milestone | Scope | State |
|---|---|---|
| M0 | RESP2 codec, TCP/TLS server, connection handling, config, fuzz harness | done |
| M1 | Strings, generic keyspace, expiration, `SCAN`, `SELECT`, multiple databases | done |
| M2 | Lists, hashes, sets, sorted sets, adaptive encodings | done |
| M3 | Append log, snapshots, recovery, compaction | done |
| M4 | Replication | in progress: links, `WAIT` and `min-replicas-to-write` done; replica persistence remains |
| M5 | Pub/Sub, transactions, blocking commands | not started |
| M6 | `maxmemory`, eviction, full metrics | partial: accounting, `INFO`, `SLOWLOG`, `/metrics` |
| M7 | RESP3, TLS, ACL, hardening | partial: RESP3 and TLS done, ACL not started |

### What durability means here

With `appendonly yes` the server writes the canonical effect of every write
to an append log in `dir`, and rebuilds the keyspace from it on startup. A
restart keeps your data, TTLs included.

```
appendfsync always     every write is on disk before it is acknowledged
appendfsync everysec   a crash loses at most a second; a process kill loses nothing
appendfsync no         the operating system decides
```

`always` is the honest option and an expensive one: it costs a disk flush per
command, which measured about 220 writes a second on the development host.
`everysec` is the default.

A log that cannot be read to its end is handled by `corrupt-log-policy`. A
torn tail -- the ordinary result of a crash during a write -- is truncated;
so is a checksum failure, unless the policy is `refuse`, in which case the
server declines to start and says why. The two are reported differently,
because truncating at a checksum failure throws away every write that
followed it.

If the log stops accepting writes, so does the server: writes are refused
with `MISCONF` and reads keep working. Acknowledging a write that will not
survive a restart is worse than an outage, because it looks like success.

The log is a sequence of segment files in `dir`:

```
kestrel-000001.log
kestrel-000002.log
```

Stream offsets run across them, so compaction is the deletion of whole files
rather than a rewrite: a snapshot names the offset below which every record
is dead, and any segment ending below it can be unlinked. Nothing is copied
and no buffer accumulates writes while it happens.

Snapshots run on `snapshot-interval`, when the log has grown past
`auto-rewrite-percentage` of its size at the last one, and on `SAVE`,
`BGSAVE` or `BGREWRITEAOF`. Each one writes the dataset, rolls the log, and
unlinks the segments it made redundant. An idle server does not snapshot
again: a pass that would change nothing still costs a shard-blocking pause.

Measured on a 5,000-write workload over 40 keys: 251,676 bytes of log before
`BGREWRITEAOF`, 13,038 bytes after, and the restart that followed rebuilt all
41 keys from the snapshot plus the one record written since.

The pause is worth knowing about. Serializing a shard holds its lock for the
whole shard, and at the default 16 shards a 200,000-key database blocks one
shard for about 6.5 ms. Raising `shards` divides that almost linearly -- 256
shards brings the same database to 279 µs -- and
[`docs/design-notes.md`](docs/design-notes.md) issue 9 has the numbers and
the alternatives.

## Quick start

```sh
make build
./build/kestreld ./kestrel.conf --appendonly no

# in another shell
./build/kestrel-cli -p 6380
# or the real thing
redis-cli -p 6380
```

```
127.0.0.1:6380> SET session:42 '{"user":"ana"}' EX 3600
OK
127.0.0.1:6380> TTL session:42
(integer) 3600
127.0.0.1:6380> INCR rate:ana
(integer) 1
127.0.0.1:6380> OBJECT ENCODING rate:ana
"int"
```

The default port is **6380**, not 6379, so Kestrel and Redis can run side by
side during migration testing.

## What works

**Connection** `PING` `ECHO` `SELECT` `SWAPDB` `AUTH` `HELLO` (RESP2 and
RESP3) `QUIT` `RESET` `CLIENT ID|GETNAME|SETNAME|INFO|NO-EVICT|HELP`

**Strings** `GET` `SET` (`NX` `XX` `GET` `KEEPTTL` `EX` `PX` `EXAT` `PXAT`)
`SETNX` `SETEX` `PSETEX` `GETSET` `GETDEL` `GETEX` `MGET` `MSET` `MSETNX`
`INCR` `DECR` `INCRBY` `DECRBY` `INCRBYFLOAT` `APPEND` `STRLEN` `GETRANGE`
`SETRANGE` `SUBSTR`

**Lists** `LPUSH` `RPUSH` `LPUSHX` `RPUSHX` `LPOP` `RPOP` `LRANGE` `LLEN`
`LINDEX` `LSET` `LINSERT` `LREM` `LTRIM` `LPOS` `LMOVE` `RPOPLPUSH`

**Hashes** `HSET` `HSETNX` `HGET` `HMGET` `HMSET` `HDEL` `HLEN` `HKEYS`
`HVALS` `HGETALL` `HEXISTS` `HINCRBY` `HINCRBYFLOAT` `HSTRLEN` `HRANDFIELD`
`HSCAN`

**Sets** `SADD` `SREM` `SMEMBERS` `SISMEMBER` `SMISMEMBER` `SCARD` `SPOP`
`SRANDMEMBER` `SMOVE` `SINTER` `SUNION` `SDIFF` `SINTERSTORE` `SUNIONSTORE`
`SDIFFSTORE` `SINTERCARD` `SSCAN`

**Sorted sets** `ZADD` (`NX` `XX` `GT` `LT` `CH` `INCR`) `ZREM` `ZSCORE`
`ZMSCORE` `ZINCRBY` `ZCARD` `ZCOUNT` `ZLEXCOUNT` `ZRANGE` (`BYSCORE` `BYLEX`
`REV` `LIMIT` `WITHSCORES`) `ZRANGEBYSCORE` `ZRANGEBYLEX` `ZREVRANGE`
`ZREVRANGEBYSCORE` `ZREVRANGEBYLEX` `ZRANGESTORE` `ZRANK` `ZREVRANK`
`ZPOPMIN` `ZPOPMAX` `ZREMRANGEBYRANK` `ZREMRANGEBYSCORE` `ZREMRANGEBYLEX`
`ZUNION` `ZINTER` `ZDIFF` `ZUNIONSTORE` `ZINTERSTORE` `ZDIFFSTORE`
`ZINTERCARD` `ZRANDMEMBER` `ZSCAN`

**Generic** `SORT` `SORT_RO` (without `BY` and `GET` patterns)

**Keyspace** `DEL` `UNLINK` `EXISTS` `TOUCH` `TYPE` `RENAME` `RENAMENX`
`COPY` `KEYS` `SCAN` `RANDOMKEY` `DBSIZE` `EXPIRE` `PEXPIRE` `EXPIREAT`
`PEXPIREAT` (each with `NX` `XX` `GT` `LT`) `TTL` `PTTL` `EXPIRETIME`
`PEXPIRETIME` `PERSIST` `FLUSHDB` `FLUSHALL`

**Server** `INFO` `CONFIG GET|SET|RESETSTAT` `COMMAND` (`COUNT` `INFO` `DOCS`
`GETKEYS`) `SLOWLOG` `MEMORY USAGE|DOCTOR` `OBJECT` `DEBUG` `TIME` `SHUTDOWN`
`CLUSTER INFO|MYID|SLOTS|SHARDS`

**Persistence** `SAVE` `BGSAVE` `BGREWRITEAOF` `LASTSAVE` — all three saves
are one operation here, which `docs/deviations.md` explains

**Replication** `REPLICAOF` `SLAVEOF` `REPLCONF` `PSYNC` `WAIT` — a replica
is fed straight from the leader's log segments, so a partial
resynchronisation is a seek and there is no separate backlog to size

**Operations** protected mode, `requirepass`, TLS and mutual TLS,
`rename-command`, structured JSON logs, graceful shutdown, a separate admin
port serving `/health`, `/ready`, `/metrics`, and optional `pprof`.

Commands that exist in Redis but are deliberately out of scope — `EVAL`,
`XADD`, `PFADD`, `DUMP`, and the rest — return an explicit error naming the
decision that excluded them and are counted in
`unsupported_command_attempts`, so the compatibility backlog is driven by
real traffic rather than guesswork.

The blocking variants (`BLPOP`, `BZPOPMIN`, `BLMOVE`, `LMPOP`) are M5, along
with pub/sub and transactions.

See [docs/deviations.md](docs/deviations.md) for the places where behaviour
intentionally differs from the reference implementation.

## Layout

The package boundaries are the ones in ADR-017, and the layering is enforced
by a test rather than by convention.

```
resp/      RESP2 and RESP3 codec              no engine, no net
engine/    keyspace, shards, expiry, memory   no net, no resp, no config
command/   command table, dispatch, effects   engine + resp + config
config/    parsing, validation, CONFIG SET
server/    listeners, connections, INFO, admin port
cmd/kestreld, cmd/kestrel-cli
```

`engine` is usable as a library on its own: create a `Keyspace`, call `Get`
and `Set`, and no socket is involved. `TestEngineImportGraph` fails the build
if anything above it leaks downward.

## Design notes worth knowing

**Every write declares how it replicates.** A command marked `Write` must
also declare `EffectVerbatim` (its arguments already replay safely) or
`EffectCanonical` (the handler emits a rewritten, replay-safe effect).
Registration panics without one, and a command that changes data without
producing an effect panics at execution. `SET k v EX 100` reaches the log as
`SET k v PXAT <absolute>`; `INCRBYFLOAT` reaches it as the computed result;
`SETNX` loses its condition. `TestFollowerConverges` runs 6000 randomized
writes through a leader, replays the captured effect stream into a second
keyspace, and requires the two to be byte-identical.

**Reads do not allocate.** Command arguments are sub-slices of the
connection's read buffer, the command table is keyed so that lookups avoid a
string conversion, the execution context is reused per client, and constant
replies are built once. `TestGetReadPathAllocations` fails the build if a
`GET` allocates at all.

**Stored values are immutable.** Every mutation installs a fresh value rather
than editing one in place, which is what makes it safe to hand a caller a
slice that points into the keyspace.

**Collections carry two encodings each.** Small ones live in a listpack: a
single contiguous buffer with length-prefixed entries, so a hundred-field
hash is one pointer for the garbage collector rather than two hundred. Above
the configured thresholds a hash becomes a map, a list becomes a quicklist of
bounded listpack nodes, a set becomes an intset or a map, and a sorted set
becomes a skiplist paired with a member map. Promotion is one-way, because a
workload oscillating around a threshold would otherwise rebuild the
collection on every operation.

**Expiry is approximate, deliberately.** Lazy expiry on access plus a
background cycle that samples the TTL index under a CPU budget. On a replica
expired keys are hidden from reads but never deleted locally: the leader's
`DEL` is authoritative, so the two cannot diverge on their own clocks.

## Conformance

Beyond the unit and property tests, the collection commands are checked
against a real `redis-server` on the same machine: an identical stream of 154
commands runs against both and every reply is compared. Two differences
remain, both because the reference available here is 7.0.15 and the target is
7.2:

- `OBJECT ENCODING` reports `listpack` for a small non-integer set, which is
  7.2 behaviour and what §6.2 of the PRD specifies. 7.0 has no listpack set
  and reports `hashtable`.
- `ZRANK ... WITHSCORE` exists here and was added upstream in 7.2.

## Performance

Measured on an 8-core development box with `redis-benchmark`, against
`redis-server` 7.x on the same machine and in the same session. The client
saturates a core well before either server does, so these are ratios, not
ceilings.

| Workload | Kestrel | Redis | Ratio |
|---|---|---|---|
| `GET`, 50 clients, no pipelining | 19.8k ops/s | 26.3k ops/s | 75% |
| `SET`, 50 clients, no pipelining | 19.0k ops/s | 25.0k ops/s | 76% |
| `GET`, 50 clients, pipeline depth 16 | 229k ops/s | 276k ops/s | 83% |

The PRD budgets 40-60% of Redis (§8.1, NG6). Engine-level microbenchmarks:

```
BenchmarkEngineGet                     217 ns/op    0 B/op   0 allocs/op
BenchmarkEngineGetWithTTL              395 ns/op    0 B/op   0 allocs/op
BenchmarkEngineGetWithTTLCachedClock   200 ns/op    0 B/op   0 allocs/op
BenchmarkEngineSet                     629 ns/op  107 B/op   4 allocs/op
```

Two findings from profiling worth carrying forward:

- `time.Now()` costs **158 ns** on this host, which lacks a vDSO fast clock.
  That made per-command latency timing more expensive than the commands
  themselves. The engine now reads a clock cached at millisecond resolution
  (`CachedClock`, what the reference implementation does for the same
  reason), which halved a TTL'd `GET`, and command timing can be turned off
  with `track-command-latency no` for hosts with the same problem.
- Per-command accounting behind a shared mutex and map cost 20% of dispatch.
  It is now an atomic slot indexed by command, and the engine's hit, miss and
  expiry counters live on the shards instead of on shared atomics.

## Development

```sh
make check       # gofmt, go vet, and the full suite under -race
make test        # tests without the race detector
make bench       # all benchmarks
make fuzz        # 60s of protocol fuzzing (FUZZTIME=10m to extend)
make fuzz-hour   # the M0 exit criterion
make cover
make release     # static binaries for linux/amd64, linux/arm64, darwin/arm64
```

`make check` is what CI should run on every merge. The suite must be race
clean; that is NFR-5 and it is not negotiable.

## Dependencies

`cespare/xxhash/v2` for shard hashing, per ADR-015. Nothing else outside the
standard library. `stretchr/testify` is not used; the tests use the standard
library. The Prometheus client library is approved by ADR-015 for the metrics
endpoint and will be adopted in M6, when there are histograms to justify it;
until then `/metrics` writes the exposition format directly.
