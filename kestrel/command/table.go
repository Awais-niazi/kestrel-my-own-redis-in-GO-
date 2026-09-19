// Package command holds the command table, argument validation, execution,
// and effect rewriting. It sits above the engine and the protocol codec and
// below the server (ADR-017).
package command

import (
	"fmt"
	"sort"
	"strings"

	"kestrel/resp"
)

// Flag describes how the dispatcher must treat a command.
type Flag uint32

// Command flags.
const (
	// Write marks a command that may modify the keyspace. Every write
	// command must also declare how it is replicated: see Deterministic and
	// Descriptor.Rewrite.
	Write Flag = 1 << iota
	// Readonly marks a command that never modifies the keyspace, so a
	// read-only replica may serve it.
	Readonly
	// Admin marks an operator command, excluded from the default ACL.
	Admin
	// DenyOOM marks a command that may increase memory use, so it is
	// refused once maxmemory is reached under the noeviction policy.
	DenyOOM
	// NoMulti marks a command that cannot be queued inside a transaction.
	NoMulti
	// Loading marks a command that may run while the dataset is loading.
	Loading
	// Stale marks a command a replica may serve while its link is down.
	Stale
	// NoAuth marks a command allowed before authentication.
	NoAuth
	// Fast marks an O(1) command, for the COMMAND reply.
	Fast
	// Blocking marks a command that can park the client.
	Blocking
	// SubscriberOK marks a command permitted while a RESP2 client is in
	// subscriber mode. It is reported as "pubsub" by COMMAND, which is the
	// name the reference implementation gives it.
	SubscriberOK
	// NoScript is reserved for parity with the reference command table.
	NoScript
)

var flagNames = []struct {
	flag Flag
	name string
}{
	{Write, "write"}, {Readonly, "readonly"}, {Admin, "admin"},
	{DenyOOM, "denyoom"}, {NoMulti, "no-multi"}, {Loading, "loading"},
	{Stale, "stale"}, {NoAuth, "no-auth"}, {Fast, "fast"},
	{Blocking, "blocking"}, {SubscriberOK, "pubsub"}, {NoScript, "noscript"},
}

// EffectKind declares how a write command reaches the append log and the
// replication stream (ADR-008).
//
// Every write command must declare one. The declaration is checked at
// registration time and the choice is enforced again at execution time,
// because a write that reaches the log in a form that does not replay
// identically is a silent divergence between leader and replica: the worst
// class of bug this system can have (R5).
type EffectKind uint8

// Effect declarations.
const (
	// EffectNone is the correct declaration for a command that never writes.
	EffectNone EffectKind = iota
	// EffectVerbatim means the command's own arguments are already
	// deterministic and replay-safe, so they are logged unchanged.
	EffectVerbatim
	// EffectCanonical means the handler must call Ctx.Propagate with a
	// replay-safe rewrite, or Ctx.SuppressPropagation when nothing changed.
	EffectCanonical
)

// Locality declares whether a write command's effect can depend on the
// current value of a key outside the shard it writes (ADR-009, and
// docs/design-notes.md issue 1).
//
// A chunked fuzzy snapshot serializes shards one at a time, so two shards in
// the same snapshot can reflect different instants of the log. Recovery
// replays each record against the shards whose recorded offset precedes it.
// That reconstruction is sound only for effects whose result is determined
// by the arguments and by keys living in the shard being written. An effect
// that reads key A and writes key B, where the two hash to different shards,
// can be replayed against a shard state that no longer holds what it read:
// the value is silently lost or, worse, written from a value the leader
// never saw.
//
// Every write command must therefore declare its locality. The declaration
// is checked at registration, for the same reason Effect is: the failure it
// prevents is a silent divergence between leader and replica, which is the
// worst class of bug this system can have (R5). A command that has not
// answered the question does not register.
type Locality uint8

// Locality declarations.
const (
	// LocalityUnset is the zero value. It is not a legal declaration for a
	// write command.
	LocalityUnset Locality = iota
	// LocalityShardLocal means every key this command writes is written
	// from the command's own arguments, or from the current value of that
	// same key. Both a pure write (SET, MSET) and a same-key
	// read-modify-write (INCR, APPEND, LPUSH) qualify: read and write are
	// the same key, hence the same shard, hence the same snapshot instant.
	LocalityShardLocal
	// LocalityCrossShard means at least one key's new value depends on the
	// current value of a different key, which may hash to another shard.
	// The dispatcher excludes these from the snapshot window.
	LocalityCrossShard
)

// Handler executes a command and returns the reply.
type Handler func(c *Ctx) resp.Value

// Descriptor is one entry in the command table.
type Descriptor struct {
	// Name is the command name in upper case.
	Name string
	// Arity is the exact argument count including the command name, or a
	// negative number giving the minimum.
	Arity int
	// Flags controls dispatch.
	Flags Flag
	// FirstKey, LastKey and Step locate the key arguments. LastKey may be
	// negative to count back from the end; -1 means "to the last argument".
	// FirstKey of 0 means the command takes no keys.
	FirstKey, LastKey, Step int
	// Categories are the ACL categories this command belongs to.
	Categories []string
	// Summary is the one-line description used by COMMAND DOCS.
	Summary string
	// Handler runs the command. Container commands leave it nil.
	Handler Handler
	// Effect declares how a write command is replicated. It must be
	// EffectNone for a command that does not write, and something else for
	// one that does.
	Effect EffectKind
	// Locality declares whether the command's effect can read a key outside
	// the shard it writes. Every write command must set it.
	Locality Locality
	// Subcommands holds the children of a container command such as CONFIG.
	Subcommands map[string]*Descriptor

	parent   *Descriptor
	fullName string
	// lowerName is fullName folded once at registration. The ACL check
	// runs on every command, and folding it there put two allocations on
	// the path of every GET.
	lowerName string
	// index addresses this command's counters inside a Table. It is
	// assigned once at registration so that per-command accounting is an
	// array index rather than a map lookup under a mutex.
	index int
}

// FullName is the name used in errors and in COMMAND replies. It is computed
// once at registration so that the dispatch path never builds a string.
func (d *Descriptor) FullName() string { return d.fullName }

// LowerName is FullName folded to lower case, also computed once.
func (d *Descriptor) LowerName() string { return d.lowerName }

// Is reports whether every flag in f is set.
func (d *Descriptor) Is(f Flag) bool { return d.Flags&f == f }

// IsContainer reports whether the command only exists to hold subcommands.
func (d *Descriptor) IsContainer() bool { return d.Handler == nil && len(d.Subcommands) > 0 }

// builtins is the global command set, populated by the register calls in
// each command file's init.
var builtins = map[string]*Descriptor{}

// descriptorCount is the number of registered descriptors, including
// subcommands. It sizes a Table's counter array.
var descriptorCount int

// register adds a command to the global set, validating the invariants that
// keep replication honest.
//
// The check that a write command must declare either Deterministic or a
// Rewrite is the mechanical half of the mitigation for R5: a missing effect
// rewrite is a silent divergence between leader and replica, so it is made
// impossible to add a write command without answering the question.
func register(d *Descriptor) {
	d.Name = strings.ToUpper(d.Name)
	if _, dup := builtins[d.Name]; dup {
		panic("command: duplicate registration of " + d.Name)
	}
	validate(d)
	builtins[d.Name] = d
}

func validate(d *Descriptor) {
	d.index = descriptorCount
	descriptorCount++
	d.fullName = d.Name
	if d.parent != nil {
		d.fullName = d.parent.Name + "|" + d.Name
	}
	d.lowerName = strings.ToLower(d.fullName)
	where := d.fullName
	if d.Arity == 0 {
		panic("command: " + where + " has no arity")
	}
	if d.Handler == nil && len(d.Subcommands) == 0 {
		panic("command: " + where + " has no handler")
	}
	if d.Is(Write) && d.Is(Readonly) {
		panic("command: " + where + " is both write and readonly")
	}
	if d.Is(Write) && d.Effect == EffectNone {
		panic("command: " + where + " is a write command but does not declare " +
			"how it is replicated; see ADR-008")
	}
	if !d.Is(Write) && d.Effect != EffectNone {
		panic("command: " + where + " declares an Effect but is not a write command")
	}
	if d.Is(Write) && d.Locality == LocalityUnset {
		panic("command: " + where + " is a write command but does not declare " +
			"whether its effect reads keys outside the shard it writes; see " +
			"ADR-009 and docs/design-notes.md issue 1")
	}
	if !d.Is(Write) && d.Locality != LocalityUnset {
		panic("command: " + where + " declares a Locality but is not a write command")
	}
	for name, sub := range d.Subcommands {
		sub.Name = strings.ToUpper(name)
		sub.parent = d
		// A subcommand with no categories of its own belongs to its
		// parent's. Without this an ACL rule like -@admin removes CONFIG
		// but leaves CONFIG GET, which is the whole of CONFIG still
		// reachable through a name the rule appeared to cover.
		if len(sub.Categories) == 0 {
			sub.Categories = d.Categories
		}
		validate(sub)
	}
}

// Table is one server's view of the command set, with any configured
// renames applied.
type Table struct {
	byName map[string]*Descriptor // lookup name -> descriptor
	all    []*Descriptor          // every reachable command, sorted by name
	// counters is indexed by Descriptor.index. Keeping accounting here
	// rather than in a shared map means a command execution costs a few
	// atomic adds and no lock (§11).
	counters []commandCounters
	// disabled records commands renamed to the empty string, so that the
	// dispatcher can say why they are gone rather than pretending they never
	// existed.
	disabled map[string]bool
}

// NewTable builds a command table, applying rename-command directives
// (FR-7.4). A rename to the empty string disables the command.
func NewTable(renames map[string]string) (*Table, error) {
	t := &Table{
		byName:   make(map[string]*Descriptor, len(builtins)*2),
		disabled: make(map[string]bool),
		counters: make([]commandCounters, descriptorCount),
	}
	for name, d := range builtins {
		target, renamed := renames[name]
		switch {
		case !renamed:
			t.add(name, d)
		case target == "":
			t.disabled[name] = true
			t.disabled[strings.ToLower(name)] = true
		default:
			if _, clash := t.byName[target]; clash {
				return nil, fmt.Errorf("rename-command %s %s: target name is already in use", name, target)
			}
			t.add(target, d)
		}
		t.all = append(t.all, d)
	}
	sort.Slice(t.all, func(i, j int) bool { return t.all[i].Name < t.all[j].Name })
	return t, nil
}

// add indexes a command under both its upper- and lower-case spellings, so
// that the overwhelmingly common cases resolve with a map lookup keyed
// directly on the request bytes and no string allocation.
func (t *Table) add(name string, d *Descriptor) {
	t.byName[name] = d
	if lower := strings.ToLower(name); lower != name {
		t.byName[lower] = d
	}
}

// Lookup resolves a command name, which the caller need not have upper-cased.
//
// The first lookup is keyed on the argument bytes directly, which the
// compiler performs without allocating a string. Only a mixed-case name
// falls through to the allocating path.
func (t *Table) Lookup(name []byte) (*Descriptor, bool) {
	if d, ok := t.byName[string(name)]; ok {
		return d, true
	}
	d, ok := t.byName[upper(name)]
	return d, ok
}

// LookupSub resolves a subcommand of a container command.
func (t *Table) LookupSub(d *Descriptor, name []byte) (*Descriptor, bool) {
	if d.Subcommands == nil {
		return nil, false
	}
	sub, ok := d.Subcommands[upper(name)]
	return sub, ok
}

// Disabled reports whether a command was removed by configuration.
func (t *Table) Disabled(name []byte) bool {
	if t.disabled[string(name)] {
		return true
	}
	return t.disabled[upper(name)]
}

// All returns every command, sorted by name.
func (t *Table) All() []*Descriptor { return t.all }

// Count returns the number of top-level commands.
func (t *Table) Count() int { return len(t.all) }

// upper upper-cases an ASCII command name without allocating for names that
// are already upper case.
func upper(b []byte) string {
	needs := false
	for _, c := range b {
		if c >= 'a' && c <= 'z' {
			needs = true
			break
		}
	}
	if !needs {
		return string(b)
	}
	buf := make([]byte, len(b))
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		buf[i] = c
	}
	return string(buf)
}

// FlagNames renders a descriptor's flags for the COMMAND reply.
func (d *Descriptor) FlagNames() []string {
	out := make([]string, 0, 4)
	for _, f := range flagNames {
		if d.Flags&f.flag != 0 {
			out = append(out, f.name)
		}
	}
	return out
}
