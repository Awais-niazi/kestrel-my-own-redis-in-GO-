package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotIsDetached(t *testing.T) {
	c := Default()
	snap := c.Snapshot()
	if err := c.Set("maxmemory", "1mb"); err != nil {
		t.Fatal(err)
	}
	if snap.MaxMemory != 0 {
		t.Fatal("a snapshot taken before the change reflected it")
	}
	mutable := snap.Clone()
	mutable.RenamedCommands["DEBUG"] = ""
	if len(c.Snapshot().RenamedCommands) != 0 {
		t.Fatal("mutating a snapshot's rename map reached the live configuration")
	}
}

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true}, {"1024", 1024, true},
		{"64mb", 64 * 1000 * 1000, true}, {"64m", 64 * 1024 * 1024, true},
		{"1gb", 1000 * 1000 * 1000, true}, {"2G", 2 * 1024 * 1024 * 1024, true},
		{"512b", 512, true}, {" 5 mb ", 5 * 1000 * 1000, true},
		{"", 0, false}, {"abc", 0, false}, {"-1", 0, false},
	}
	for _, c := range cases {
		got, err := ParseMemory(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("ParseMemory(%q) = %d, %v", c.in, got, err)
		}
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kestrel.conf")
	body := `
# a comment
port 7000
maxmemory 256mb
appendfsync always
requirepass "s3 cret#hash"
protected-mode no
shards 8
rename-command flushall ""
rename-command config admin_config
save 900 1
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := loaded.Snapshot()
	if c.Port != 7000 || c.MaxMemory != 256*1000*1000 || c.AppendFsync != "always" {
		t.Fatalf("got port=%d maxmemory=%d fsync=%s", c.Port, c.MaxMemory, c.AppendFsync)
	}
	if c.RequirePass != "s3 cret#hash" {
		t.Fatalf("quoted value mangled: %q", c.RequirePass)
	}
	if c.ProtectedMode {
		t.Fatal("protected-mode not applied")
	}
	if c.RenamedCommands["FLUSHALL"] != "" {
		t.Fatalf("rename to empty: %q", c.RenamedCommands["FLUSHALL"])
	}
	if c.RenamedCommands["CONFIG"] != "ADMIN_CONFIG" {
		t.Fatalf("rename: %q", c.RenamedCommands["CONFIG"])
	}
}

func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.conf")
	os.WriteFile(path, []byte("port 7000\nloglevel warn\n"), 0o600)

	c := Default()
	if err := c.LoadFile(path); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadEnv([]string{"KESTREL_PORT=7100", "KESTREL_LOGLEVEL=error", "PATH=/bin"}); err != nil {
		t.Fatal(err)
	}
	if v := c.Snapshot(); v.Port != 7100 || v.LogLevel != "error" {
		t.Fatalf("env did not override file: %d %s", v.Port, v.LogLevel)
	}
	if err := c.LoadFlags([]string{"--port", "7200", "--pprof-enabled"}); err != nil {
		t.Fatal(err)
	}
	v := c.Snapshot()
	if v.Port != 7200 {
		t.Fatalf("flags did not override env: %d", v.Port)
	}
	if !v.PprofEnabled {
		t.Fatal("valueless flag did not set a boolean")
	}
}

func TestLoadRejectsUnknownNames(t *testing.T) {
	c := Default()
	if err := c.LoadFlags([]string{"--nope", "1"}); err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if err := c.LoadEnv([]string{"KESTREL_NOPE=1"}); err == nil {
		t.Fatal("expected an error for an unknown environment variable")
	}
}

func TestRuntimeSetRespectsMutability(t *testing.T) {
	c := Default()
	if err := c.Set("maxmemory", "100mb"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get("maxmemory"); v != "100000000" {
		t.Fatalf("got %q", v)
	}
	if err := c.Set("shards", "32"); err == nil {
		t.Fatal("CONFIG SET changed a startup-only parameter")
	}
	if err := c.Set("appendfsync", "sometimes"); err == nil {
		t.Fatal("CONFIG SET accepted an invalid enum value")
	}
	if err := c.Set("nosuchparam", "1"); err == nil {
		t.Fatal("CONFIG SET accepted an unknown parameter")
	}
}

func TestConfigMatch(t *testing.T) {
	c := Default()
	got := c.Match("maxmemory*")
	if len(got) != 3 {
		t.Fatalf("maxmemory* matched %d params: %v", len(got), got)
	}
	if len(c.Match("*")) != len(Names()) {
		t.Fatal("* did not match every parameter")
	}
	if len(c.Match("nothing-like-this")) != 0 {
		t.Fatal("unexpected match")
	}
}

func TestValidate(t *testing.T) {
	v := DefaultValues()
	v.Shards = 12
	if err := v.Validate(); err == nil {
		t.Fatal("accepted a non-power-of-two shard count")
	}
	v = DefaultValues()
	v.AdminPort = v.Port
	if err := v.Validate(); err == nil {
		t.Fatal("accepted a port collision")
	}
	v = DefaultValues()
	v.TLSPort = 6390
	if err := v.Validate(); err == nil {
		t.Fatal("accepted tls-port without certificates")
	}
}
