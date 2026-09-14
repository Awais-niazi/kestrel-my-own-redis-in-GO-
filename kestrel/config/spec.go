package config

import (
	"fmt"
	"strconv"
	"strings"
)

// spec describes one configuration parameter: how to render it, how to parse
// it, and whether CONFIG SET may change it while the server is running.
type spec struct {
	mutable bool
	get     func(*Values) string
	set     func(*Values, string) error
}

// Parameters fixed at startup are those the engine or the listeners bake in:
// shard count (ADR-003), database count, ports, and the data directory.
var specs = map[string]spec{}

func register(name string, mutable bool, s spec) {
	s.mutable = mutable
	specs[name] = s
}

// str, num, mem, boolean and enum build the accessor pairs for each kind of
// value, so that adding a parameter is one line rather than a closure pair.
func str(p func(*Values) *string) spec {
	return spec{
		get: func(c *Values) string { return *p(c) },
		set: func(c *Values, v string) error { *p(c) = v; return nil },
	}
}

func num[T ~int | ~int64](p func(*Values) *T) spec {
	return spec{
		get: func(c *Values) string { return strconv.FormatInt(int64(*p(c)), 10) },
		set: func(c *Values, v string) error {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return err
			}
			*p(c) = T(n)
			return nil
		},
	}
}

func mem(p func(*Values) *int64) spec {
	return spec{
		get: func(c *Values) string { return strconv.FormatInt(*p(c), 10) },
		set: func(c *Values, v string) error {
			n, err := ParseMemory(v)
			if err != nil {
				return err
			}
			*p(c) = n
			return nil
		},
	}
}

func boolean(p func(*Values) *bool) spec {
	return spec{
		get: func(c *Values) string {
			if *p(c) {
				return "yes"
			}
			return "no"
		},
		set: func(c *Values, v string) error {
			b, err := ParseBool(v)
			if err != nil {
				return err
			}
			*p(c) = b
			return nil
		},
	}
}

func enum(p func(*Values) *string, allowed ...string) spec {
	return spec{
		get: func(c *Values) string { return *p(c) },
		set: func(c *Values, v string) error {
			v = strings.ToLower(strings.TrimSpace(v))
			for _, a := range allowed {
				if v == a {
					*p(c) = v
					return nil
				}
			}
			return fmt.Errorf("must be one of %s", strings.Join(allowed, ", "))
		},
	}
}

func flt(p func(*Values) *float64) spec {
	return spec{
		get: func(c *Values) string { return strconv.FormatFloat(*p(c), 'g', -1, 64) },
		set: func(c *Values, v string) error {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return err
			}
			*p(c) = f
			return nil
		},
	}
}

func init() {
	// Network
	register("bind", false, str(func(c *Values) *string { return &c.Bind }))
	register("port", false, num(func(c *Values) *int { return &c.Port }))
	register("tcp-backlog", false, num(func(c *Values) *int { return &c.TCPBacklog }))
	register("tcp-keepalive", true, num(func(c *Values) *int { return &c.TCPKeepAlive }))
	register("timeout", true, num(func(c *Values) *int { return &c.Timeout }))
	register("maxclients", true, num(func(c *Values) *int { return &c.MaxClients }))

	// Security
	register("requirepass", true, str(func(c *Values) *string { return &c.RequirePass }))
	register("protected-mode", true, boolean(func(c *Values) *bool { return &c.ProtectedMode }))
	register("tls-port", false, num(func(c *Values) *int { return &c.TLSPort }))
	register("tls-cert-file", false, str(func(c *Values) *string { return &c.TLSCertFile }))
	register("tls-key-file", false, str(func(c *Values) *string { return &c.TLSKeyFile }))
	register("tls-ca-cert-file", false, str(func(c *Values) *string { return &c.TLSCACertFile }))
	register("tls-auth-clients", false, boolean(func(c *Values) *bool { return &c.TLSAuthClients }))

	// Memory
	register("maxmemory", true, mem(func(c *Values) *int64 { return &c.MaxMemory }))
	register("maxmemory-policy", true, enum(func(c *Values) *string { return &c.MaxMemoryPolicy },
		"noeviction", "allkeys-lru", "allkeys-lfu", "allkeys-random",
		"volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl"))
	register("maxmemory-samples", true, num(func(c *Values) *int { return &c.MaxMemorySamples }))
	register("memory-limit-overhead-factor", true, flt(func(c *Values) *float64 { return &c.MemoryLimitOverheadFactor }))

	// Engine
	register("databases", false, num(func(c *Values) *int { return &c.Databases }))
	register("shards", false, num(func(c *Values) *int { return &c.Shards }))
	register("hash-max-listpack-entries", true, num(func(c *Values) *int { return &c.HashMaxListpackEntries }))
	register("hash-max-listpack-value", true, num(func(c *Values) *int { return &c.HashMaxListpackValue }))
	register("list-max-listpack-size", true, num(func(c *Values) *int { return &c.ListMaxListpackSize }))
	register("set-max-intset-entries", true, num(func(c *Values) *int { return &c.SetMaxIntsetEntries }))
	register("set-max-listpack-entries", true, num(func(c *Values) *int { return &c.SetMaxListpackEntries }))
	// Appendix B lists no value limit for sets or lists, but the encoding
	// table in §6.2 specifies one for both, so they are configurable here.
	register("set-max-listpack-value", true, num(func(c *Values) *int { return &c.SetMaxListpackValue }))
	register("list-max-listpack-value", true, num(func(c *Values) *int { return &c.ListMaxListpackValue }))
	register("zset-max-listpack-entries", true, num(func(c *Values) *int { return &c.ZsetMaxListpackEntries }))
	register("zset-max-listpack-value", true, num(func(c *Values) *int { return &c.ZsetMaxListpackValue }))

	// Protocol limits
	register("proto-max-bulk-len", true, mem(func(c *Values) *int64 { return &c.ProtoMaxBulkLen }))
	register("proto-max-multibulk-len", true, num(func(c *Values) *int { return &c.ProtoMaxMultiBulkLen }))
	register("proto-max-inline-len", true, num(func(c *Values) *int { return &c.ProtoMaxInlineLen }))
	register("client-query-buffer-limit", true, mem(func(c *Values) *int64 { return &c.ClientQueryBufferLimit }))

	// Expiration
	register("active-expire", true, boolean(func(c *Values) *bool { return &c.ActiveExpire }))
	register("active-expire-cpu-percent", true, num(func(c *Values) *int { return &c.ActiveExpireCPUPercent }))
	register("active-expire-sample-size", true, num(func(c *Values) *int { return &c.ActiveExpireSampleSize }))

	// Persistence
	register("dir", false, str(func(c *Values) *string { return &c.Dir }))
	register("appendonly", true, boolean(func(c *Values) *bool { return &c.AppendOnly }))
	register("appendfsync", true, enum(func(c *Values) *string { return &c.AppendFsync }, "always", "everysec", "no"))
	register("auto-rewrite-percentage", true, num(func(c *Values) *int { return &c.AutoRewritePercentage }))
	register("auto-rewrite-min-size", true, mem(func(c *Values) *int64 { return &c.AutoRewriteMinSize }))
	register("snapshot-interval", true, num(func(c *Values) *int { return &c.SnapshotInterval }))
	register("snapshot-batch-keys", true, num(func(c *Values) *int { return &c.SnapshotBatchKeys }))
	register("corrupt-log-policy", true, enum(func(c *Values) *string { return &c.CorruptLogPolicy }, "truncate", "refuse"))

	// Replication
	register("replicaof", false, str(func(c *Values) *string { return &c.ReplicaOf }))
	register("replica-read-only", true, boolean(func(c *Values) *bool { return &c.ReplicaReadOnly }))
	register("repl-backlog-size", true, mem(func(c *Values) *int64 { return &c.ReplBacklogSize }))
	register("repl-backlog-ttl", true, num(func(c *Values) *int { return &c.ReplBacklogTTL }))
	register("min-replicas-to-write", true, num(func(c *Values) *int { return &c.MinReplicasToWrite }))
	register("min-replicas-max-lag", true, num(func(c *Values) *int { return &c.MinReplicasMaxLag }))

	// Observability
	register("admin-port", false, num(func(c *Values) *int { return &c.AdminPort }))
	register("metrics-enabled", false, boolean(func(c *Values) *bool { return &c.MetricsEnabled }))
	register("pprof-enabled", false, boolean(func(c *Values) *bool { return &c.PprofEnabled }))
	register("slowlog-log-slower-than", true, num(func(c *Values) *int64 { return &c.SlowlogLogSlowerThan }))
	register("slowlog-max-len", true, num(func(c *Values) *int { return &c.SlowlogMaxLen }))
	register("track-command-latency", true, boolean(func(c *Values) *bool { return &c.TrackCommandLatency }))
	register("loglevel", true, enum(func(c *Values) *string { return &c.LogLevel }, "debug", "info", "warn", "error"))
	register("logformat", true, enum(func(c *Values) *string { return &c.LogFormat }, "json", "text"))

	// Runtime
	register("gogc", true, num(func(c *Values) *int { return &c.GOGC }))
}

// ParseMemory parses a byte count with an optional unit suffix. Both the
// decimal (kb, mb, gb) and binary (k, m, g) conventions of the reference
// implementation are accepted.
func ParseMemory(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("empty memory value")
	}
	mult := int64(1)
	for _, suffix := range []struct {
		name string
		n    int64
	}{
		{"kb", 1000}, {"mb", 1000 * 1000}, {"gb", 1000 * 1000 * 1000},
		{"tb", 1000 * 1000 * 1000 * 1000},
		{"k", 1024}, {"m", 1024 * 1024}, {"g", 1024 * 1024 * 1024},
		{"t", 1024 * 1024 * 1024 * 1024}, {"b", 1},
	} {
		if strings.HasSuffix(t, suffix.name) {
			mult = suffix.n
			t = strings.TrimSpace(strings.TrimSuffix(t, suffix.name))
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("memory value must not be negative")
	}
	return n * mult, nil
}

// ParseBool accepts the yes/no spelling used in the config file as well as
// the usual true/false and 1/0.
func ParseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "true", "1", "on":
		return true, nil
	case "no", "false", "0", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value %q, expected yes or no", s)
	}
}

// globMatch is a minimal '*' and '?' matcher, enough for CONFIG GET patterns.
func globMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	p, t := 0, 0
	star, match := -1, 0
	for t < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == s[t] || pattern[p] == '?'):
			p++
			t++
		case p < len(pattern) && pattern[p] == '*':
			star, match = p, t
			p++
		case star >= 0:
			p = star + 1
			match++
			t = match
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
