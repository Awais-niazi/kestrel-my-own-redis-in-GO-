package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kestrel/config"
	"kestrel/persist"
)

// durableServer starts a server that persists into dir.
func durableServer(t *testing.T, dir string, tweaks ...func(*config.Config)) *testServer {
	t.Helper()
	// dir is not modifiable at runtime, so it arrives the way it does in
	// production: as a startup flag.
	all := append([]func(*config.Config){func(c *config.Config) {
		must(t, c.LoadFlags([]string{
			"--appendonly", "yes", "--dir", dir, "--appendfsync", "everysec",
		}))
	}}, tweaks...)
	return startServer(t, all...)
}

// stop shuts a server down and waits for it, so the next one can open the
// same files.
func stop(t *testing.T, ts *testServer) {
	t.Helper()
	ts.Shutdown(true)
	if err := ts.wait(t); err != nil {
		t.Fatal(err)
	}
}

// TestDataSurvivesARestart is the whole point of M3.
func TestDataSurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	first := durableServer(t, dir)
	c := first.connect(t)
	c.do("SET", "greeting", "hello")
	c.do("SET", "counter", "0")
	for i := 0; i < 5; i++ {
		c.do("INCR", "counter")
	}
	c.do("RPUSH", "queue", "a", "b", "c")
	c.do("HSET", "profile", "name", "ana", "city", "lisbon")
	c.do("SADD", "tags", "x", "y")
	c.do("ZADD", "board", "10", "ana", "20", "bo")
	c.do("SET", "temporary", "v", "EX", "9999")
	c.do("SELECT", "3")
	c.do("SET", "other-db", "yes")
	stop(t, first)

	second := durableServer(t, dir)
	d := second.connect(t)
	for _, tc := range []struct {
		want string
		args []string
	}{
		{"hello", []string{"GET", "greeting"}},
		{"5", []string{"GET", "counter"}},
		{"3", []string{"LLEN", "queue"}},
		{"a", []string{"LINDEX", "queue", "0"}},
		{"ana", []string{"HGET", "profile", "name"}},
		{"2", []string{"SCARD", "tags"}},
		{"20", []string{"ZSCORE", "board", "bo"}},
	} {
		if got := text(d.do(tc.args...)); got != tc.want {
			t.Errorf("%v = %q after restart, want %q", tc.args, got, tc.want)
		}
	}
	if ttl := text(d.do("TTL", "temporary")); ttl == "-1" || ttl == "-2" {
		t.Errorf("TTL survived as %q; the expiry should still be set", ttl)
	}
	d.do("SELECT", "3")
	if got := text(d.do("GET", "other-db")); got != "yes" {
		t.Errorf("database 3 lost its data: %q", got)
	}
}

// TestRestartIsRepeatable covers the second restart, where the log is opened,
// appended to, and replayed again.
func TestRestartIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	for round := 0; round < 3; round++ {
		ts := durableServer(t, dir)
		c := ts.connect(t)
		if got := text(c.do("GET", "rounds")); got != expectedRounds(round) {
			t.Fatalf("round %d: rounds = %q, want %q", round, got, expectedRounds(round))
		}
		c.do("INCR", "rounds")
		c.do("RPUSH", "log", fmt.Sprint(round))
		stop(t, ts)
	}

	final := durableServer(t, dir)
	c := final.connect(t)
	if got := text(c.do("GET", "rounds")); got != "3" {
		t.Errorf("rounds = %q after three restarts, want 3", got)
	}
	if got := text(c.do("LLEN", "log")); got != "3" {
		t.Errorf("list holds %q entries after three restarts, want 3", got)
	}
	if got := text(c.do("LINDEX", "log", "2")); got != "2" {
		t.Errorf("the last round's entry is %q, want \"2\"", got)
	}
}

func expectedRounds(round int) string {
	if round == 0 {
		return "<nil>"
	}
	return fmt.Sprint(round)
}

// TestDeletionsSurviveARestart is the case a naive log gets wrong: the key
// must be gone, not merely last-written.
func TestDeletionsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	first := durableServer(t, dir)
	c := first.connect(t)
	c.do("SET", "doomed", "v")
	c.do("SET", "kept", "v")
	c.do("DEL", "doomed")
	c.do("RPUSH", "l", "a", "b")
	c.do("LPOP", "l")
	stop(t, first)

	second := durableServer(t, dir)
	d := second.connect(t)
	if got := text(d.do("GET", "doomed")); got != "<nil>" {
		t.Errorf("a deleted key came back as %q", got)
	}
	if got := text(d.do("GET", "kept")); got != "v" {
		t.Errorf("kept = %q", got)
	}
	if got := text(d.do("LLEN", "l")); got != "1" {
		t.Errorf("list holds %q entries after a pop and a restart, want 1", got)
	}
	if got := text(d.do("LINDEX", "l", "0")); got != "b" {
		t.Errorf("list holds %q after a pop and a restart, want \"b\"", got)
	}
}

// TestNoLogMeansEmptyStart checks that a fresh directory is not an error.
func TestNoLogMeansEmptyStart(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	if got := text(c.do("DBSIZE")); got != "0" {
		t.Errorf("a fresh directory started with %q keys", got)
	}
	c.do("SET", "k", "v")
	if _, err := os.Stat(filepath.Join(dir, logFileName)); err != nil {
		t.Errorf("no log file was created: %v", err)
	}
}

// TestAppendOnlyOffKeepsNothing states the one case where data really is
// lost, so that the behaviour is pinned rather than assumed.
func TestAppendOnlyOffKeepsNothing(t *testing.T) {
	dir := t.TempDir()
	first := startServer(t, func(c *config.Config) {
		must(t, c.LoadFlags([]string{"--dir", dir}))
	})
	c := first.connect(t)
	c.do("SET", "k", "v")
	stop(t, first)

	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("appendonly no wrote %v into the data directory", entries)
	}
}

// TestCorruptLogPolicyRefuse checks that a damaged log stops the server
// rather than starting it with a truncated dataset.
func TestCorruptLogPolicyRefuse(t *testing.T) {
	dir := t.TempDir()
	first := durableServer(t, dir)
	c := first.connect(t)
	for i := 0; i < 20; i++ {
		c.do("SET", fmt.Sprintf("k%d", i), "value")
	}
	stop(t, first)

	path := filepath.Join(dir, logFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	port, adminPort := freePort(t), freePort(t)
	must(t, cfg.LoadFlags([]string{
		"--port", fmt.Sprint(port), "--admin-port", fmt.Sprint(adminPort),
		"--loglevel", "error", "--appendonly", "yes", "--dir", dir,
		"--corrupt-log-policy", "refuse",
	}))
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("the server started on a corrupt log under the refuse policy")
	} else if !errors.Is(err, persist.ErrCorrupt) {
		t.Errorf("error %v does not name the corruption", err)
	}
}

// TestCorruptLogPolicyTruncate starts anyway, keeping the readable prefix.
func TestCorruptLogPolicyTruncate(t *testing.T) {
	dir := t.TempDir()
	first := durableServer(t, dir)
	c := first.connect(t)
	c.do("SET", "early", "kept")
	for i := 0; i < 20; i++ {
		c.do("SET", fmt.Sprintf("k%d", i), "value")
	}
	stop(t, first)

	path := filepath.Join(dir, logFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-10] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	second := durableServer(t, dir, func(cfg *config.Config) {
		must(t, cfg.Set("corrupt-log-policy", "truncate"))
	})
	d := second.connect(t)
	if got := text(d.do("GET", "early")); got != "kept" {
		t.Errorf("the readable prefix was lost: early = %q", got)
	}
	// The truncation must be persisted, so a further restart is clean.
	d.do("SET", "after", "truncation")
	stop(t, second)

	third := durableServer(t, dir)
	e := third.connect(t)
	if got := text(e.do("GET", "after")); got != "truncation" {
		t.Errorf("writing after a truncation did not survive: %q", got)
	}
}

// TestInfoPersistenceReportsTheLog checks the fields an operator watches.
func TestInfoPersistenceReportsTheLog(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "k", "v")

	info := text(c.do("INFO", "persistence"))
	for _, want := range []string{
		"aol_enabled:1", "aol_last_write_status:ok", "aol_dir:" + dir,
	} {
		if !strings.Contains(info, want) {
			t.Errorf("INFO persistence is missing %q:\n%s", want, info)
		}
	}
	if strings.Contains(info, "aol_current_size:0") {
		t.Error("INFO reports an empty log after a write")
	}
}

// TestWritesAreRefusedWhenTheLogFails is the behaviour that keeps a full
// disk from looking like success.
func TestWritesAreRefusedWhenTheLogFails(t *testing.T) {
	dir := t.TempDir()
	ts := durableServer(t, dir)
	c := ts.connect(t)
	c.do("SET", "before", "ok")

	// The failure is injected directly rather than through a test hook in
	// the log: this test is about what the server does once the log has
	// failed, not about how it fails.
	injected := error(errors.New("no space left on device"))
	ts.persist.failed.Store(&injected)

	if got := text(c.do("SET", "after", "no")); !strings.HasPrefix(got, "MISCONF") {
		t.Errorf("a write during a log failure returned %q, want MISCONF", got)
	}
	if got := text(c.do("GET", "before")); got != "ok" {
		t.Errorf("reads stopped working too: %q", got)
	}
	info := text(c.do("INFO", "persistence"))
	if !strings.Contains(info, "aol_last_write_status:err") {
		t.Errorf("INFO still reports a healthy log:\n%s", info)
	}
}
