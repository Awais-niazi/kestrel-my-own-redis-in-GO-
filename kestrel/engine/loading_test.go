package engine

import (
	"testing"
	"time"
)

func loadingKeyspace(t *testing.T) *Keyspace {
	t.Helper()
	ks := New(Options{Databases: 4, Shards: 4, ActiveExpire: false})
	t.Cleanup(ks.Close)
	return ks
}

// TestLoadingKeepsExpiredKeysVisible is the property recovery depends on.
// A replay re-executes the leader's past writes against the present clock,
// so a TTL in the log has usually already elapsed.
func TestLoadingKeepsExpiredKeysVisible(t *testing.T) {
	ks := loadingKeyspace(t)
	now := int64(1_700_000_000_000)
	ks.SetClock(func() int64 { return now })
	db := ks.DB(0)

	for _, k := range []string{"k", "probe"} {
		db.Set([]byte(k), []byte("v"), SetOptions{})
		db.Expire([]byte(k), now+100, 0)
	}
	now += 1000 // the TTLs are now in the past

	// A separate key establishes the baseline, because reading an expired
	// key outside loading mode reaps it.
	if v, _, _ := db.Get([]byte("probe")); v != nil {
		t.Fatal("precondition: the key should be expired outside loading mode")
	}

	ks.SetLoading(true)
	v, ok, _ := db.Get([]byte("k"))
	if !ok || string(v) != "v" {
		t.Errorf("loading mode hid an expired key: %q, %v", v, ok)
	}

	// Clearing the flag hands the key back to the ordinary lazy path.
	ks.SetLoading(false)
	if v, _, _ := db.Get([]byte("k")); v != nil {
		t.Errorf("the key survived the end of loading: %q", v)
	}
}

// TestLoadingPreventsReplayDivergence reproduces the failure the flag
// exists for, by replaying a leader's log against a later clock.
//
// The leader set a TTL, appended to the key while it was still alive, and
// only later emitted the DEL its own expiry produced. Recovery sees all
// three at once, long after the TTL elapsed.
func TestLoadingPreventsReplayDivergence(t *testing.T) {
	const expireAt = 1_700_000_000_100

	replay := func(loading bool) (string, bool) {
		ks := loadingKeyspace(t)
		now := int64(1_700_000_099_000) // well past expireAt
		ks.SetClock(func() int64 { return now })
		ks.SetLoading(loading)
		db := ks.DB(0)

		// The log, in the order the leader wrote it.
		db.Set([]byte("k"), []byte("original"), SetOptions{})
		db.Expire([]byte("k"), expireAt, 0)
		db.Append([]byte("k"), []byte("-more"))

		v, ok, _ := db.Get([]byte("k"))
		return string(v), ok
	}

	if got, _ := replay(false); got == "original-more" {
		t.Skip("the divergence did not reproduce; the test no longer proves anything")
	} else if got != "-more" {
		t.Logf("without loading mode the replay produced %q", got)
	}

	got, ok := replay(true)
	if !ok || got != "original-more" {
		t.Errorf("replay under loading mode produced %q (present=%v), want %q",
			got, ok, "original-more")
	}
}

// TestLoadingSuspendsActiveExpiry keeps the background reaper from removing
// what the log has not yet told it to remove.
func TestLoadingSuspendsActiveExpiry(t *testing.T) {
	ks := loadingKeyspace(t)
	now := int64(1_700_000_000_000)
	ks.SetClock(func() int64 { return now })
	db := ks.DB(0)

	for _, k := range []string{"a", "b", "c"} {
		db.Set([]byte(k), []byte("v"), SetOptions{})
		db.Expire([]byte(k), now+100, 0)
	}
	now += 1000

	ks.SetLoading(true)
	if n := ks.ExpirePass(time.Now().Add(time.Second)); n != 0 {
		t.Errorf("the active pass reaped %d keys while loading", n)
	}
	if got := db.Size(); got != 3 {
		t.Errorf("%d keys survived the pass, want 3", got)
	}

	ks.SetLoading(false)
	if n := ks.ExpirePass(time.Now().Add(time.Second)); n != 3 {
		t.Errorf("the pass reaped %d keys after loading ended, want 3", n)
	}
}

// TestLoadingIsDistinctFromReplica records why replica mode could not be
// reused: it hides an expired key, and a hidden key is a missing key to the
// next write in the log.
func TestLoadingIsDistinctFromReplica(t *testing.T) {
	ks := loadingKeyspace(t)
	now := int64(1_700_000_000_000)
	ks.SetClock(func() int64 { return now })
	db := ks.DB(0)

	db.Set([]byte("k"), []byte("v"), SetOptions{})
	db.Expire([]byte("k"), now+100, 0)
	now += 1000

	ks.SetReplica(true)
	if v, _, _ := db.Get([]byte("k")); v != nil {
		t.Error("replica mode returned an expired key; it is supposed to hide it")
	}
	// Size is a logical count, so it drops the key too. That the key is
	// still physically there -- only the leader's DEL may remove it -- is
	// what the next assertion shows.
	if got := db.Size(); got != 0 {
		t.Errorf("DBSIZE counted %d, want 0: an expired key is not visible on a replica", got)
	}

	ks.SetLoading(true)
	if v, _, _ := db.Get([]byte("k")); string(v) != "v" {
		t.Error("loading mode must win over replica mode, and the key must still " +
			"be present: a replica recovers from a log too")
	}
}
