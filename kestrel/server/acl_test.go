package server

import (
	"strings"
	"testing"

	"kestrel/config"
)

func TestAclDefaultUserIsOpen(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	if got := text(c.do("ACL", "WHOAMI")); got != "default" {
		t.Errorf("ACL WHOAMI returned %q", got)
	}
	list := arr(c.do("ACL", "LIST"))
	if !strings.Contains(list, "user default on nopass") || !strings.Contains(list, "+@all") {
		t.Errorf("the default user is not open: %q", list)
	}
	if got := text(c.do("SET", "k", "v")); got != "OK" {
		t.Errorf("the default user was refused a write: %q", got)
	}
}

func TestAclSetUserAndAuth(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)

	if got := text(c.do("ACL", "SETUSER", "alice", "on", ">secret", "~cache:*",
		"+@read", "+acl|whoami")); got != "OK" {
		t.Fatalf("ACL SETUSER returned %q", got)
	}
	if got := arr(c.do("ACL", "USERS")); !strings.Contains(got, "alice") {
		t.Errorf("ACL USERS returned %q", got)
	}

	other := ts.connect(t)
	if got := text(other.do("AUTH", "alice", "wrong")); !strings.HasPrefix(got, "WRONGPASS") {
		t.Errorf("a wrong password returned %q", got)
	}
	if got := text(other.do("AUTH", "alice", "secret")); got != "OK" {
		t.Fatalf("AUTH returned %q", got)
	}
	if got := text(other.do("ACL", "WHOAMI")); got != "alice" {
		t.Errorf("ACL WHOAMI returned %q", got)
	}
}

// TestAclRefusesCommandsKeysAndChannelsSeparately checks that each of the
// three kinds of refusal says which it is.
func TestAclRefusesCommandsKeysAndChannelsSeparately(t *testing.T) {
	ts := startServer(t)
	admin := ts.connect(t)
	admin.do("SET", "cache:hit", "value")
	admin.do("SET", "secret:key", "value")
	admin.do("ACL", "SETUSER", "reader", "on", ">pw", "~cache:*", "&news:*", "+@read", "+subscribe")

	c := ts.connect(t)
	c.do("AUTH", "reader", "pw")

	if got := text(c.do("GET", "cache:hit")); got != "value" {
		t.Errorf("an allowed key was refused: %q", got)
	}
	if got := text(c.do("GET", "secret:key")); !strings.HasPrefix(got, "NOPERM") {
		t.Errorf("a key outside the pattern returned %q", got)
	} else if !strings.Contains(got, "keys") {
		t.Errorf("the refusal does not say it was about a key: %q", got)
	}
	if got := text(c.do("SET", "cache:hit", "x")); !strings.HasPrefix(got, "NOPERM") {
		t.Errorf("a command outside the category returned %q", got)
	} else if !strings.Contains(got, "'set'") {
		t.Errorf("the refusal does not name the command: %q", got)
	}
	if got := text(c.do("SUBSCRIBE", "other:chat")); !strings.HasPrefix(got, "NOPERM") {
		t.Errorf("a channel outside the pattern returned %q", got)
	} else if !strings.Contains(got, "channels") {
		t.Errorf("the refusal does not say it was about a channel: %q", got)
	}
	if got := arr(c.do("SUBSCRIBE", "news:general")); !strings.HasPrefix(got, "subscribe") {
		t.Errorf("an allowed channel was refused: %q", got)
	}
}

// TestAclRuleOrderMatters is the reason rules are applied in sequence:
// "+@all -@admin" and "-@admin +@all" mean different things, and a user who
// wrote the first and got the second would have granted what they withheld.
func TestAclRuleOrderMatters(t *testing.T) {
	ts := startServer(t)
	admin := ts.connect(t)

	admin.do("ACL", "SETUSER", "careful", "on", ">pw", "~*", "+@all", "-@admin")
	admin.do("ACL", "SETUSER", "careless", "on", ">pw", "~*", "-@admin", "+@all")

	careful := ts.connect(t)
	careful.do("AUTH", "careful", "pw")
	if got := text(careful.do("CONFIG", "GET", "maxmemory")); !strings.HasPrefix(got, "NOPERM") {
		t.Errorf("-@admin after +@all did not take effect: %q", got)
	}
	if got := text(careful.do("SET", "k", "v")); got != "OK" {
		t.Errorf("careful lost an ordinary command: %q", got)
	}

	careless := ts.connect(t)
	careless.do("AUTH", "careless", "pw")
	if got := arr(careless.do("CONFIG", "GET", "maxmemory")); strings.HasPrefix(got, "NOPERM") {
		t.Errorf("+@all after -@admin should have restored it: %q", got)
	}
}

// TestAclSetUserIsAllOrNothing: a half-applied rule list is a user with
// permissions nobody wrote down.
func TestAclSetUserIsAllOrNothing(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("ACL", "SETUSER", "bob", "on", ">pw", "~*", "+@read")

	got := text(c.do("ACL", "SETUSER", "bob", "+@write", "+notacommand"))
	if !strings.HasPrefix(got, "ERR") {
		t.Fatalf("an unknown rule returned %q", got)
	}
	// The valid rule before the bad one must not have taken effect.
	user := arr(c.do("ACL", "GETUSER", "bob"))
	if strings.Contains(user, "+@write") {
		t.Errorf("a rejected rule list was partly applied: %q", user)
	}
}

func TestAclGetUser(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("ACL", "SETUSER", "carol", "on", ">pw", "~data:*", "&feed:*", "+@read")

	got := arr(c.do("ACL", "GETUSER", "carol"))
	for _, want := range []string{"flags", "on", "passwords", "commands", "+@read",
		"keys", "~data:*", "channels", "&feed:*"} {
		if !strings.Contains(got, want) {
			t.Errorf("ACL GETUSER is missing %q:\n%s", want, got)
		}
	}
	// The plaintext password must not appear anywhere.
	if strings.Contains(got, "pw\"") || strings.Contains(got, " pw ") {
		t.Errorf("ACL GETUSER leaked a plaintext password: %s", got)
	}
	if got := arr(c.do("ACL", "GETUSER", "nobody")); got != "<nil>" {
		t.Errorf("GETUSER for an unknown user returned %q, want a nil reply", got)
	}
}

func TestAclDelUser(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("ACL", "SETUSER", "temp", "on", ">pw")
	if got := text(c.do("ACL", "DELUSER", "temp")); got != "1" {
		t.Errorf("ACL DELUSER returned %q", got)
	}
	if got := text(c.do("ACL", "DELUSER", "temp")); got != "0" {
		t.Errorf("deleting a missing user returned %q", got)
	}
	// The default user cannot go: without it a connection cannot be
	// accepted, or told why.
	if got := text(c.do("ACL", "DELUSER", "default")); !strings.HasPrefix(got, "ERR") {
		t.Errorf("deleting the default user returned %q", got)
	}
}

func TestAclCat(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	cats := arr(c.do("ACL", "CAT"))
	for _, want := range []string{"read", "write", "admin", "dangerous", "pubsub"} {
		if !strings.Contains(cats, want) {
			t.Errorf("ACL CAT is missing %q: %s", want, cats)
		}
	}
	read := arr(c.do("ACL", "CAT", "read"))
	if !strings.Contains(read, "get") {
		t.Errorf("ACL CAT read does not list get: %s", read)
	}
	if got := text(c.do("ACL", "CAT", "nonsense")); !strings.HasPrefix(got, "ERR") {
		t.Errorf("an unknown category returned %q", got)
	}
}

func TestAclGenPass(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	p := text(c.do("ACL", "GENPASS"))
	if len(p) != 64 {
		t.Errorf("ACL GENPASS returned %d characters, want 64", len(p))
	}
	if second := text(c.do("ACL", "GENPASS")); second == p {
		t.Error("ACL GENPASS returned the same password twice")
	}
	if got := text(c.do("ACL", "GENPASS", "0")); !strings.HasPrefix(got, "ERR") {
		t.Errorf("GENPASS 0 returned %q", got)
	}
}

// TestRequirepassAndDefaultUserAgree: they are two spellings of the same
// thing, and an operator who sets one expects it to work without learning
// about the other.
func TestRequirepassAndDefaultUserAgree(t *testing.T) {
	ts := startServer(t, func(c *config.Config) {
		must(t, c.Set("requirepass", "startup-secret"))
	})
	c := ts.connect(t)
	if got := text(c.do("GET", "k")); !strings.HasPrefix(got, "NOAUTH") {
		t.Errorf("an unauthenticated read returned %q", got)
	}
	if got := text(c.do("AUTH", "wrong")); !strings.HasPrefix(got, "WRONGPASS") {
		t.Errorf("a wrong password returned %q", got)
	}
	if got := text(c.do("AUTH", "startup-secret")); got != "OK" {
		t.Fatalf("AUTH with requirepass returned %q", got)
	}
	if got := text(c.do("SET", "k", "v")); got != "OK" {
		t.Errorf("an authenticated write returned %q", got)
	}

	// Changing requirepass at runtime moves the default user with it.
	must(t, ts.cfg.Set("requirepass", "new-secret"))
	ts.ApplyRuntimeConfig()
	fresh := ts.connect(t)
	if got := text(fresh.do("AUTH", "startup-secret")); !strings.HasPrefix(got, "WRONGPASS") {
		t.Errorf("the old password still works: %q", got)
	}
	if got := text(fresh.do("AUTH", "new-secret")); got != "OK" {
		t.Errorf("the new password does not work: %q", got)
	}
}

// TestAclOffUserCannotAuthenticate covers the enabled flag.
func TestAclOffUserCannotAuthenticate(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("ACL", "SETUSER", "suspended", "off", ">pw", "~*", "+@all")

	other := ts.connect(t)
	if got := text(other.do("AUTH", "suspended", "pw")); !strings.HasPrefix(got, "WRONGPASS") {
		t.Errorf("a disabled user authenticated: %q", got)
	}
}

// TestAclResetClearsEverything.
func TestAclResetClearsEverything(t *testing.T) {
	ts := startServer(t)
	c := ts.connect(t)
	c.do("ACL", "SETUSER", "dave", "on", ">pw", "~*", "&*", "+@all")
	c.do("ACL", "SETUSER", "dave", "reset")
	got := arr(c.do("ACL", "GETUSER", "dave"))
	if strings.Contains(got, "+@all") || strings.Contains(got, "allkeys") {
		t.Errorf("reset left permissions behind: %s", got)
	}
	if !strings.Contains(got, "off") {
		t.Errorf("reset left the user enabled: %s", got)
	}
}

// TestAclGrantingAContainerGrantsItsSubcommands: a rule naming CONFIG is
// asking about CONFIG, and a user who could run neither GET nor SET would
// have been granted nothing.
func TestAclGrantingAContainerGrantsItsSubcommands(t *testing.T) {
	ts := startServer(t)
	admin := ts.connect(t)
	admin.do("ACL", "SETUSER", "ops", "on", ">pw", "~*", "-@all", "+config")

	c := ts.connect(t)
	c.do("AUTH", "ops", "pw")
	if got := arr(c.do("CONFIG", "GET", "maxmemory")); strings.HasPrefix(got, "NOPERM") {
		t.Errorf("+config did not grant CONFIG GET: %q", got)
	}
	if got := text(c.do("GET", "k")); !strings.HasPrefix(got, "NOPERM") {
		t.Errorf("-@all did not withhold GET: %q", got)
	}
}
