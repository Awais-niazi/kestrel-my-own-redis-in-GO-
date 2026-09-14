// Package config loads and validates kestreld's configuration.
//
// Values come from three places with a fixed precedence (NFR-7):
// defaults < config file < environment (KESTREL_<NAME>) < command-line flags.
// The mutable subset is additionally reachable at runtime through CONFIG SET.
package config

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Config is the live server configuration.
//
// Values are held behind an atomic pointer and replaced wholesale on every
// change, so a reader pays one atomic load and no allocation. That matters
// because the dispatcher consults the configuration on every command, and an
// allocation there would defeat the zero-allocation read path (§11). Writers
// are serialized by the mutex and never mutate a published Values.
type Config struct {
	mu sync.Mutex
	v  atomic.Pointer[Values]
}

// Values is the configuration itself, with no lock in it, so that a snapshot
// can be copied and passed around freely. Fields are grouped as in
// Appendix B of the PRD.
type Values struct {
	// Network
	Bind         string
	Port         int
	TCPBacklog   int
	TCPKeepAlive int // seconds
	Timeout      int // seconds; 0 disables the idle timeout
	MaxClients   int

	// Security
	RequirePass    string
	ProtectedMode  bool
	TLSPort        int
	TLSCertFile    string
	TLSKeyFile     string
	TLSCACertFile  string
	TLSAuthClients bool

	// Memory
	MaxMemory                 int64
	MaxMemoryPolicy           string
	MaxMemorySamples          int
	MemoryLimitOverheadFactor float64

	// Engine
	Databases              int
	Shards                 int
	HashMaxListpackEntries int
	HashMaxListpackValue   int
	ListMaxListpackSize    int
	ListMaxListpackValue   int
	SetMaxIntsetEntries    int
	SetMaxListpackEntries  int
	SetMaxListpackValue    int
	ZsetMaxListpackEntries int
	ZsetMaxListpackValue   int

	// Protocol limits (FR-1.6)
	ProtoMaxBulkLen        int64
	ProtoMaxMultiBulkLen   int
	ProtoMaxInlineLen      int
	ClientQueryBufferLimit int64

	// Expiration
	ActiveExpire           bool
	ActiveExpireCPUPercent int
	ActiveExpireSampleSize int

	// Persistence
	Dir                   string
	AppendOnly            bool
	AppendFsync           string
	AutoRewritePercentage int
	AutoRewriteMinSize    int64
	SnapshotInterval      int // seconds; 0 disables periodic snapshots
	SnapshotBatchKeys     int
	CorruptLogPolicy      string

	// Replication
	ReplicaOf          string
	ReplicaReadOnly    bool
	ReplBacklogSize    int64
	ReplBacklogTTL     int
	MinReplicasToWrite int
	MinReplicasMaxLag  int

	// Observability
	AdminPort            int
	MetricsEnabled       bool
	PprofEnabled         bool
	SlowlogLogSlowerThan int64 // microseconds
	SlowlogMaxLen        int
	// TrackCommandLatency controls whether each command is timed. Turning
	// it off removes two clock reads per command, at the cost of
	// usec_per_call in INFO, the latency stats, and the slow log.
	TrackCommandLatency bool
	LogLevel            string
	LogFormat           string

	// Runtime
	GOGC int

	// RenamedCommands maps an original command name (upper case) to the name
	// it answers to. An empty target disables the command entirely (FR-7.4).
	RenamedCommands map[string]string

	// Source records where the configuration was loaded from, for INFO.
	Source string
}

// Default returns the built-in configuration, matching Appendix B.
func Default() *Config {
	c := &Config{}
	v := DefaultValues()
	c.v.Store(&v)
	return c
}

// DefaultValues returns the built-in configuration values.
func DefaultValues() Values {
	return Values{
		Bind:         "127.0.0.1",
		Port:         6380,
		TCPBacklog:   511,
		TCPKeepAlive: 300,
		Timeout:      0,
		MaxClients:   20000,

		RequirePass:    "",
		ProtectedMode:  true,
		TLSPort:        0,
		TLSAuthClients: false,

		MaxMemory:                 0,
		MaxMemoryPolicy:           "noeviction",
		MaxMemorySamples:          5,
		MemoryLimitOverheadFactor: 1.4,

		Databases:              16,
		Shards:                 16,
		HashMaxListpackEntries: 128,
		HashMaxListpackValue:   64,
		ListMaxListpackSize:    128,
		ListMaxListpackValue:   64,
		SetMaxIntsetEntries:    512,
		SetMaxListpackEntries:  128,
		SetMaxListpackValue:    64,
		ZsetMaxListpackEntries: 128,
		ZsetMaxListpackValue:   64,

		ProtoMaxBulkLen:        512 * 1024 * 1024,
		ProtoMaxMultiBulkLen:   1024 * 1024,
		ProtoMaxInlineLen:      64 * 1024,
		ClientQueryBufferLimit: 1024 * 1024 * 1024,

		ActiveExpire:           true,
		ActiveExpireCPUPercent: 25,
		ActiveExpireSampleSize: 20,

		Dir:                   "/var/lib/kestrel",
		AppendOnly:            true,
		AppendFsync:           "everysec",
		AutoRewritePercentage: 100,
		AutoRewriteMinSize:    64 * 1024 * 1024,
		SnapshotInterval:      900,
		SnapshotBatchKeys:     10000,
		CorruptLogPolicy:      "truncate",

		ReplicaReadOnly:    true,
		ReplBacklogSize:    64 * 1024 * 1024,
		ReplBacklogTTL:     3600,
		MinReplicasToWrite: 0,
		MinReplicasMaxLag:  10,

		AdminPort:            6381,
		MetricsEnabled:       true,
		PprofEnabled:         false,
		SlowlogLogSlowerThan: 10000,
		SlowlogMaxLen:        128,
		TrackCommandLatency:  true,
		LogLevel:             "info",
		LogFormat:            "json",

		GOGC: 200,

		RenamedCommands: map[string]string{},
		Source:          "defaults",
	}
}

// Validate checks cross-field constraints that parsing alone cannot catch.
func (c *Config) Validate() error { return c.Snapshot().Validate() }

// update applies fn to a private copy of the values and publishes the result
// only if fn succeeds, so a rejected change leaves nothing half applied.
func (c *Config) update(fn func(*Values) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.v.Load().Clone()
	if err := fn(next); err != nil {
		return err
	}
	c.v.Store(next)
	return nil
}

// Validate checks cross-field constraints on a set of values.
func (c *Values) Validate() error {
	if c.Port == 0 && c.TLSPort == 0 {
		return fmt.Errorf("both port and tls-port are 0: the server would accept no connections")
	}
	if c.Port != 0 && c.Port == c.AdminPort {
		return fmt.Errorf("port and admin-port are both %d", c.Port)
	}
	if c.Shards&(c.Shards-1) != 0 {
		return fmt.Errorf("shards must be a power of two, got %d", c.Shards)
	}
	if c.Databases < 1 {
		return fmt.Errorf("databases must be at least 1")
	}
	switch c.AppendFsync {
	case "always", "everysec", "no":
	default:
		return fmt.Errorf("appendfsync must be always, everysec or no, got %q", c.AppendFsync)
	}
	switch c.CorruptLogPolicy {
	case "truncate", "refuse":
	default:
		return fmt.Errorf("corrupt-log-policy must be truncate or refuse, got %q", c.CorruptLogPolicy)
	}
	if !validPolicies[c.MaxMemoryPolicy] {
		return fmt.Errorf("unknown maxmemory-policy %q", c.MaxMemoryPolicy)
	}
	if c.TLSPort != 0 && (c.TLSCertFile == "" || c.TLSKeyFile == "") {
		return fmt.Errorf("tls-port is set but tls-cert-file or tls-key-file is missing")
	}
	return nil
}

var validPolicies = map[string]bool{
	"noeviction": true, "allkeys-lru": true, "allkeys-lfu": true,
	"allkeys-random": true, "volatile-lru": true, "volatile-lfu": true,
	"volatile-random": true, "volatile-ttl": true,
}

// Get returns the current value of a parameter, rendered as a string.
func (c *Config) Get(name string) (string, bool) {
	s, ok := specs[strings.ToLower(name)]
	if !ok {
		return "", false
	}
	return s.get(c.Snapshot()), true
}

// Set applies a runtime configuration change.
//
// It refuses parameters that are fixed at startup, because silently
// accepting a change that does not take effect is worse than an error.
func (c *Config) Set(name, value string) error {
	key := strings.ToLower(name)
	s, ok := specs[key]
	if !ok {
		return fmt.Errorf("unknown parameter '%s'", name)
	}
	if !s.mutable {
		return fmt.Errorf("parameter '%s' is not modifiable at runtime", name)
	}
	return c.update(func(v *Values) error {
		if err := s.set(v, value); err != nil {
			return fmt.Errorf("invalid value for '%s': %w", name, err)
		}
		return nil
	})
}

// SetParam applies a runtime configuration change to a detached set of
// values. CONFIG SET uses it to validate a whole batch against a copy before
// touching the live configuration, so a bad value part-way through a
// multi-parameter change cannot leave it half applied.
func (c *Values) SetParam(name, value string) error {
	key := strings.ToLower(name)
	s, ok := specs[key]
	if !ok {
		return fmt.Errorf("unknown parameter '%s'", name)
	}
	if !s.mutable {
		return fmt.Errorf("parameter '%s' is not modifiable at runtime", name)
	}
	if err := s.set(c, value); err != nil {
		return fmt.Errorf("invalid value for '%s': %w", name, err)
	}
	return nil
}

// Match returns every parameter whose name matches a glob pattern, as a
// sorted list of name/value pairs, for CONFIG GET.
func (c *Config) Match(pattern string) [][2]string {
	v := c.Snapshot()
	out := make([][2]string, 0, 8)
	for name, s := range specs {
		if globMatch(pattern, name) {
			out = append(out, [2]string{name, s.get(v)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// Names returns every known parameter name, sorted.
func Names() []string {
	out := make([]string, 0, len(specs))
	for n := range specs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Snapshot returns the current values.
//
// The result is shared and must be treated as read-only; use Clone to get a
// copy that can be modified. Reading a field from it is safe from any
// goroutine and costs one atomic load.
func (c *Config) Snapshot() *Values { return c.v.Load() }

// Clone returns an independently modifiable copy of the values.
func (v *Values) Clone() *Values {
	cp := *v
	cp.RenamedCommands = make(map[string]string, len(v.RenamedCommands))
	for k, val := range v.RenamedCommands {
		cp.RenamedCommands[k] = val
	}
	return &cp
}

// withLock applies fn to the values. Loaders use it, because they may set
// parameters that CONFIG SET refuses.
func (c *Config) withLock(fn func(v *Values)) {
	c.update(func(v *Values) error {
		fn(v)
		return nil
	})
}
