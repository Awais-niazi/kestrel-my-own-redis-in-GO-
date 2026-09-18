package command

import (
	"strings"
	"testing"
)

func TestWaitReportsAcknowledgedReplicas(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	h.replicasAcked.Store(2)

	if got := str(s.do("WAIT", "2", "100")); got != "2" {
		t.Errorf("WAIT returned %q, want 2", got)
	}
	if h.waitCalls.Load() != 1 {
		t.Error("WAIT did not reach the host")
	}
}

func TestWaitRejectsBadArguments(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	for _, args := range [][]string{
		{"WAIT", "x", "100"},
		{"WAIT", "1", "x"},
		{"WAIT", "-1", "100"},
		{"WAIT", "1", "-5"},
	} {
		if got := str(s.do(args...)); !strings.HasPrefix(got, "ERR") {
			t.Errorf("%v returned %q", args, got)
		}
	}
}

// TestWaitOnAReplicaIsRefused: a replica has nothing to wait for, and
// returning zero would look like a healthy answer.
func TestWaitOnAReplicaIsRefused(t *testing.T) {
	h := newTestHost(t)
	h.isReplica = true
	s := newSession(t, h)
	if got := str(s.do("WAIT", "1", "10")); !strings.HasPrefix(got, "ERR") {
		t.Errorf("WAIT on a replica returned %q", got)
	}
}

// TestMinReplicasToWriteRefusesWrites is the availability trade the
// directive exists to make.
func TestMinReplicasToWriteRefusesWrites(t *testing.T) {
	h := newTestHost(t)
	if err := h.cfg.Set("min-replicas-to-write", "2"); err != nil {
		t.Fatal(err)
	}
	s := newSession(t, h)

	h.replicasInSync.Store(1)
	got := str(s.do("SET", "k", "v"))
	if !strings.HasPrefix(got, "NOREPLICAS") {
		t.Fatalf("a write with too few replicas returned %q", got)
	}
	// The numbers are in the message so an operator knows how far short
	// they are without going to look.
	if !strings.Contains(got, "1 in sync") || !strings.Contains(got, "2 required") {
		t.Errorf("the refusal does not say how short it is: %q", got)
	}
	// Reads keep working: the point is to bound loss, not to stop serving.
	if got := str(s.do("GET", "k")); got != "<nil>" {
		t.Errorf("reads were refused too: %q", got)
	}

	h.replicasInSync.Store(2)
	if got := str(s.do("SET", "k", "v")); got != "OK" {
		t.Errorf("a write with enough replicas returned %q", got)
	}
}

func TestMinReplicasToWriteOffByDefault(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	h.replicasInSync.Store(0)
	if got := str(s.do("SET", "k", "v")); got != "OK" {
		t.Errorf("writes were refused with the directive unset: %q", got)
	}
}
