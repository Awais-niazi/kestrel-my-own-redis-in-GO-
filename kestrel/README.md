# Kestrel

A RESP-compatible in-memory data store written in Go. Unmodified Redis
clients — `redis-cli`, go-redis, redis-py, Jedis, ioredis — connect to it
without changes.

This repository implements the design in `kestrel-prd.md`. It is not a Redis
distribution and is not affiliated with Redis Ltd.

## Status

**M0 (skeleton) and M1 (core key/value) are complete.** The server runs, real
Redis clients talk to it, and strings, generic keyspace commands, and
expiration all work.

| Milestone | Scope | State |
|---|---|---|
| M0 | RESP2 codec, TCP/TLS server, connection handling, config, fuzz harness | done |
| M1 | Strings, generic keyspace, expiration, `SCAN`, `SELECT`, multiple databases | done |
| M2 | Lists, hashes, sets, sorted sets, adaptive encodings | not started |
| M3 | Append log, snapshots, recovery, compaction | not started |
| M4 | Replication | not started |
| M5 | Pub/Sub, transactions, blocking commands | not started |
| M6 | `maxmemory`, eviction, full metrics | partial: accounting, `INFO`, `SLOWLOG`, `/metrics` |
| M7 | RESP3, TLS, ACL, hardening | partial: RESP3 and TLS done, ACL not started |

### Nothing is durable yet

The configuration accepts `appendonly` and `snapshot-interval` so that a
production config file loads unchanged, but **the append log and the
snapshotter do not exist yet**. Data lives in memory only and is lost on
restart. The server says so in its startup log, every time.

The effect stream those subsystems will consume is already built and tested,
so M3 attaches to a working propagation path rather than introducing one.

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

**Keyspace** `DEL` `UNLINK` `EXISTS` `TOUCH` `TYPE` `RENAME` `RENAMENX`
`COPY` `KEYS` `SCAN` `RANDOMKEY` `DBSIZE` `EXPIRE` `PEXPIRE` `EXPIREAT`
`PEXPIREAT` (each with `NX` `XX` `GT` `LT`) `TTL` `PTTL` `EXPIRETIME`
`PEXPIRETIME` `PERSIST` `FLUSHDB` `FLUSHALL`

**Server** `INFO` `CONFIG GET|SET|RESETSTAT` `COMMAND` (`COUNT` `INFO` `DOCS`
`GETKEYS`) `SLOWLOG` `MEMORY USAGE|DOCTOR` `OBJECT` `DEBUG` `TIME` `SHUTDOWN`
`CLUSTER INFO|MYID|SLOTS|SHARDS`

**Operations** protected mode, `requirepass`, TLS and mutual TLS,
`rename-command`, structured JSON logs, graceful shutdown, a separate admin
port serving `/health`, `/ready`, `/metrics`, and optional `pprof`.

Commands that exist in Redis but are deliberately out of scope — `EVAL`,
`XADD`, `PFADD`, `DUMP`, and the rest — return an explicit error naming the
decision that excluded them and are counted in
`unsupported_command_attempts`, so the compatibility backlog is driven by
real traffic rather than guesswork.

Collection types (`LPUSH`, `HSET`, `SADD`, `ZADD`, …) are M2 and currently
return "unknown command".

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

**Expiry is approximate, deliberately.** Lazy expiry on access plus a
background cycle that samples the TTL index under a CPU budget. On a replica
expired keys are hidden from reads but never deleted locally: the leader's
`DEL` is authoritative, so the two cannot diverge on their own clocks.

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
