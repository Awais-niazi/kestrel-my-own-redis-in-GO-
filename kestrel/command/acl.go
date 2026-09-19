package command

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	"kestrel/engine"
)

// Access control (FR-7.5).
//
// A user is a set of permissions: which commands, which keys, which
// channels. Rules are applied in the order they are given, exactly as the
// reference implementation applies them, because the order is the whole
// grammar -- "+@all -@dangerous" and "-@dangerous +@all" mean different
// things, and a user who wrote the first and got the second would have
// granted what they meant to withhold.
//
// Permissions are stored as a resolved set rather than as rules to be
// interpreted per command. Checking is then a map lookup on the dispatch
// path, and the cost of a rule that touches a hundred commands is paid once
// when it is set.

// User is one ACL identity.
type User struct {
	Name string

	mu      sync.RWMutex
	enabled bool
	nopass  bool
	// passwords holds SHA-256 hashes. A plaintext password is never
	// retained: ACL GETUSER shows the hashes, which is all an operator
	// needs to recognise one they set.
	passwords []string

	// allowed maps a command's full lower-case name to whether this user
	// may run it. Every known command is present, so a lookup that misses
	// means the command does not exist rather than that it is denied.
	allowed map[string]bool
	// rules records the command rules as applied, for ACL LIST and
	// GETUSER. Rendering the resolved set instead would produce a correct
	// but unrecognisable two-hundred-term rule string.
	rules []string

	allKeys     bool
	keyPatterns []string

	allChannels     bool
	channelPatterns []string
}

// ACL is the set of users.
type ACL struct {
	mu    sync.RWMutex
	users map[string]*User
	// commands is every command name the table knows, so that +@all and
	// category rules can resolve without consulting the table each time.
	commands []commandFacts
}

// commandFacts is what the ACL needs to know about a command.
type commandFacts struct {
	name       string
	categories []string
}

// NewACL builds a registry from a command table.
//
// The default user starts able to do anything with no password, which is
// what a server with no access control configured must behave like. Turning
// on requirepass narrows it; nothing else does.
func NewACL(t *Table) *ACL {
	a := &ACL{users: make(map[string]*User)}
	for _, d := range allCommandsOf(t) {
		a.commands = append(a.commands, commandFacts{
			name:       strings.ToLower(d.FullName()),
			categories: d.Categories,
		})
	}
	def := a.newUser("default")
	def.apply([]string{"on", "nopass", "~*", "&*", "+@all"}, a.commands)
	a.users["default"] = def
	return a
}

// allCommandsOf returns every command a table can reach, subcommands
// included.
func allCommandsOf(t *Table) []*Descriptor {
	var out []*Descriptor
	var walk func(*Descriptor)
	walk = func(d *Descriptor) {
		out = append(out, d)
		for _, sub := range d.Subcommands {
			walk(sub)
		}
	}
	for _, d := range t.all {
		walk(d)
	}
	return out
}

func (a *ACL) newUser(name string) *User {
	u := &User{Name: name, allowed: make(map[string]bool, len(a.commands))}
	for _, c := range a.commands {
		u.allowed[c.name] = false
	}
	return u
}

// User looks up a user by name.
func (a *ACL) User(name string) *User {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.users[name]
}

// Default returns the default user, which always exists.
func (a *ACL) Default() *User { return a.User("default") }

// Names lists the users, in a stable order.
func (a *ACL) Names() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.users))
	for n := range a.users {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Users lists the users in name order.
func (a *ACL) Users() []*User {
	names := a.Names()
	out := make([]*User, 0, len(names))
	for _, n := range names {
		if u := a.User(n); u != nil {
			out = append(out, u)
		}
	}
	return out
}

// SetUser creates or updates a user by applying rules in order.
//
// The rules are applied to a copy, which replaces the original only if every
// one of them is understood. A half-applied rule list is a user with
// permissions nobody wrote down, and the operator who typed a misspelling
// would be left guessing which of their rules took effect.
func (a *ACL) SetUser(name string, rules []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	var draft *User
	if existing := a.users[name]; existing != nil {
		draft = existing.clone()
	} else {
		draft = a.newUser(name)
	}
	if err := draft.apply(rules, a.commands); err != nil {
		return err
	}
	a.users[name] = draft
	return nil
}

// DelUser removes users and returns how many went. The default user cannot
// be removed: a server with no default user cannot accept an unauthenticated
// connection, or explain why.
func (a *ACL) DelUser(names []string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, name := range names {
		if name == "default" {
			return n, fmt.Errorf("the 'default' user cannot be removed")
		}
		if _, ok := a.users[name]; ok {
			delete(a.users, name)
			n++
		}
	}
	return n, nil
}

// Categories lists the command categories, for ACL CAT.
func (a *ACL) Categories() []string {
	seen := make(map[string]struct{})
	for _, c := range a.commands {
		for _, cat := range c.categories {
			seen[cat] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CommandsIn lists the commands in a category.
func (a *ACL) CommandsIn(category string) ([]string, bool) {
	var out []string
	known := false
	for _, c := range a.commands {
		for _, cat := range c.categories {
			if cat == category {
				known = true
				out = append(out, c.name)
			}
		}
	}
	sort.Strings(out)
	return out, known
}

// SetDefaultPassword makes requirepass and the default user agree.
//
// They are two spellings of the same thing, and an operator who sets
// requirepass expects authentication to work without learning about ACLs at
// all. An empty password restores the open default.
func (a *ACL) SetDefaultPassword(password string) {
	u := a.Default()
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.passwords = nil
	if password == "" {
		u.nopass = true
		return
	}
	u.nopass = false
	u.passwords = []string{hashPassword(password)}
}

func (u *User) clone() *User {
	u.mu.RLock()
	defer u.mu.RUnlock()
	c := &User{
		Name:            u.Name,
		enabled:         u.enabled,
		nopass:          u.nopass,
		passwords:       append([]string(nil), u.passwords...),
		allowed:         make(map[string]bool, len(u.allowed)),
		rules:           append([]string(nil), u.rules...),
		allKeys:         u.allKeys,
		keyPatterns:     append([]string(nil), u.keyPatterns...),
		allChannels:     u.allChannels,
		channelPatterns: append([]string(nil), u.channelPatterns...),
	}
	for k, v := range u.allowed {
		c.allowed[k] = v
	}
	return c
}

// apply runs rules in order.
func (u *User) apply(rules []string, commands []commandFacts) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, raw := range rules {
		if err := u.applyOne(raw, commands); err != nil {
			return err
		}
	}
	return nil
}

func (u *User) applyOne(raw string, commands []commandFacts) error {
	rule := strings.ToLower(raw)
	switch {
	case rule == "on":
		u.enabled = true
		return nil
	case rule == "off":
		u.enabled = false
		return nil
	case rule == "nopass":
		u.nopass, u.passwords = true, nil
		return nil
	case rule == "resetpass":
		u.nopass, u.passwords = false, nil
		return nil
	case rule == "reset":
		u.enabled, u.nopass, u.passwords = false, false, nil
		u.setAll(false)
		u.rules = nil
		u.allKeys, u.keyPatterns = false, nil
		u.allChannels, u.channelPatterns = false, nil
		return nil
	case rule == "allkeys" || rule == "~*":
		u.allKeys, u.keyPatterns = true, nil
		return nil
	case rule == "resetkeys":
		u.allKeys, u.keyPatterns = false, nil
		return nil
	case rule == "allchannels" || rule == "&*":
		u.allChannels, u.channelPatterns = true, nil
		return nil
	case rule == "resetchannels":
		u.allChannels, u.channelPatterns = false, nil
		return nil
	case rule == "allcommands" || rule == "+@all":
		u.setAll(true)
		u.rules = []string{"+@all"}
		return nil
	case rule == "nocommands" || rule == "-@all":
		u.setAll(false)
		u.rules = []string{"-@all"}
		return nil
	}

	switch raw[0] {
	case '>':
		u.passwords = appendUnique(u.passwords, hashPassword(raw[1:]))
		u.nopass = false
		return nil
	case '<':
		u.passwords = removeString(u.passwords, hashPassword(raw[1:]))
		return nil
	case '#':
		h, err := normaliseHash(raw[1:])
		if err != nil {
			return err
		}
		u.passwords = appendUnique(u.passwords, h)
		u.nopass = false
		return nil
	case '!':
		h, err := normaliseHash(raw[1:])
		if err != nil {
			return err
		}
		u.passwords = removeString(u.passwords, h)
		return nil
	case '~':
		u.keyPatterns = appendUnique(u.keyPatterns, raw[1:])
		return nil
	case '&':
		u.channelPatterns = appendUnique(u.channelPatterns, raw[1:])
		return nil
	case '+', '-':
		return u.applyCommandRule(rule, commands)
	}
	return fmt.Errorf("Error in ACL SETUSER modifier '%s': Syntax error", raw)
}

// applyCommandRule handles +cmd, -cmd, +cmd|sub and +@category.
func (u *User) applyCommandRule(rule string, commands []commandFacts) error {
	grant := rule[0] == '+'
	name := rule[1:]
	if name == "" {
		return fmt.Errorf("Error in ACL SETUSER modifier '%s': Syntax error", rule)
	}

	if name[0] == '@' {
		category := name[1:]
		matched := false
		for _, c := range commands {
			for _, cat := range c.categories {
				if cat == category {
					u.allowed[c.name] = grant
					matched = true
				}
			}
		}
		if !matched {
			return fmt.Errorf("Error in ACL SETUSER modifier '%s': Unknown command or category name in ACL", rule)
		}
		u.rules = append(u.rules, rule)
		return nil
	}

	if _, known := u.allowed[name]; !known {
		return fmt.Errorf("Error in ACL SETUSER modifier '%s': Unknown command or category name in ACL", rule)
	}
	u.allowed[name] = grant
	// Granting a container also grants its subcommands, because a rule
	// naming CONFIG is asking about CONFIG, and a user who could run
	// neither CONFIG GET nor CONFIG SET would have been granted nothing.
	prefix := name + "|"
	for _, c := range commands {
		if strings.HasPrefix(c.name, prefix) {
			u.allowed[c.name] = grant
		}
	}
	u.rules = append(u.rules, rule)
	return nil
}

func (u *User) setAll(v bool) {
	for k := range u.allowed {
		u.allowed[k] = v
	}
}

// Enabled reports whether the user may authenticate.
func (u *User) Enabled() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.enabled
}

// NoPass reports whether the user authenticates without a password.
func (u *User) NoPass() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.nopass
}

// CheckPassword verifies a password in constant time.
//
// The comparison is constant time even though the reply says plainly whether
// it matched: what a timing difference would leak is not whether the
// password is right, which the caller already learns, but how much of it is.
func (u *User) CheckPassword(password string) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if !u.enabled {
		return false
	}
	if u.nopass {
		return true
	}
	want := hashPassword(password)
	ok := false
	for _, h := range u.passwords {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			ok = true
		}
	}
	return ok
}

// CanRun reports whether the user may run a command.
func (u *User) CanRun(d *Descriptor) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.allowed[d.lowerName]
}

// Unrestricted reports that this user may touch every key and every channel,
// so a caller can skip extracting them.
//
// The default user is unrestricted on a server with no access control
// configured, which is the overwhelmingly common case and the one that must
// cost nothing: extracting a command's keys to check them against "allow
// everything" allocated a slice on the path of every GET.
func (u *User) Unrestricted() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.allKeys && u.allChannels
}

// CanAccessKey reports whether the user may touch a key.
func (u *User) CanAccessKey(key []byte) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.allKeys {
		return true
	}
	for _, p := range u.keyPatterns {
		if engine.MatchPattern([]byte(p), key) {
			return true
		}
	}
	return false
}

// CanAccessChannel reports whether the user may use a channel.
func (u *User) CanAccessChannel(channel []byte) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.allChannels {
		return true
	}
	for _, p := range u.channelPatterns {
		if engine.MatchPattern([]byte(p), channel) {
			return true
		}
	}
	return false
}

// Describe renders the user as an ACL LIST line would, without the name.
func (u *User) Describe() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	var parts []string
	if u.enabled {
		parts = append(parts, "on")
	} else {
		parts = append(parts, "off")
	}
	if u.nopass {
		parts = append(parts, "nopass")
	}
	for _, h := range u.passwords {
		parts = append(parts, "#"+h)
	}
	if u.allKeys {
		parts = append(parts, "~*")
	}
	for _, p := range u.keyPatterns {
		parts = append(parts, "~"+p)
	}
	if u.allChannels {
		parts = append(parts, "&*")
	}
	for _, p := range u.channelPatterns {
		parts = append(parts, "&"+p)
	}
	if len(u.rules) == 0 {
		parts = append(parts, "-@all")
	} else {
		parts = append(parts, u.rules...)
	}
	return strings.Join(parts, " ")
}

// Snapshot returns the pieces ACL GETUSER reports.
func (u *User) Snapshot() (flags, passwords, keys, channels []string, commands string) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.enabled {
		flags = append(flags, "on")
	} else {
		flags = append(flags, "off")
	}
	if u.nopass {
		flags = append(flags, "nopass")
	}
	if u.allKeys {
		flags = append(flags, "allkeys")
		keys = []string{"~*"}
	} else {
		for _, p := range u.keyPatterns {
			keys = append(keys, "~"+p)
		}
	}
	if u.allChannels {
		flags = append(flags, "allchannels")
		channels = []string{"&*"}
	} else {
		for _, p := range u.channelPatterns {
			channels = append(channels, "&"+p)
		}
	}
	passwords = append(passwords, u.passwords...)
	if len(u.rules) == 0 {
		commands = "-@all"
	} else {
		commands = strings.Join(u.rules, " ")
	}
	return flags, passwords, keys, channels, commands
}

func hashPassword(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:])
}

func normaliseHash(h string) (string, error) {
	h = strings.ToLower(h)
	if len(h) != 64 {
		return "", fmt.Errorf("Error in ACL SETUSER modifier '#%s': Invalid password hash provided. It must be exactly 64 characters and contain only lowercase hexadecimal characters", h)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("Error in ACL SETUSER modifier '#%s': Invalid password hash provided. It must be exactly 64 characters and contain only lowercase hexadecimal characters", h)
	}
	return h, nil
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func removeString(xs []string, v string) []string {
	out := xs[:0]
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// genPassword returns a random password of n hex characters.
func genPassword(bits int) (string, error) {
	b := make([]byte, (bits+7)/8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := hex.EncodeToString(b)
	want := (bits + 3) / 4
	if len(s) > want {
		s = s[:want]
	}
	return s, nil
}
