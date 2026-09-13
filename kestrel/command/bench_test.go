package command

import (
	"testing"

	"kestrel/resp"
)

// benchHost builds a host with the reply written to a discarding writer, so
// the numbers measure dispatch and the engine rather than the socket.
func benchSession(b *testing.B) *session {
	b.Helper()
	t := &testing.T{}
	h := newTestHost(t)
	s := newSession(t, h)
	s.wr = resp.NewWriter(discard{})
	s.cl.Out = s.wr
	return s
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func exec(s *session, args ...[]byte) {
	Execute(s.h, s.cl, args)
	s.wr.Flush()
}

func BenchmarkGet(b *testing.B) {
	s := benchSession(b)
	exec(s, []byte("SET"), []byte("mykey"), []byte("some value of moderate length"))
	args := [][]byte{[]byte("GET"), []byte("mykey")}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		exec(s, args...)
	}
}

func BenchmarkSet(b *testing.B) {
	s := benchSession(b)
	args := [][]byte{[]byte("SET"), []byte("mykey"), []byte("some value of moderate length")}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		exec(s, args...)
	}
}

func BenchmarkIncr(b *testing.B) {
	s := benchSession(b)
	args := [][]byte{[]byte("INCR"), []byte("counter")}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		exec(s, args...)
	}
}

func BenchmarkPing(b *testing.B) {
	s := benchSession(b)
	args := [][]byte{[]byte("PING")}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		exec(s, args...)
	}
}

// TestGetReadPathAllocations is the regression gate from §11: a GET that hits
// must not allocate on the read path. Without a gate, allocations creep back
// in one commit at a time and the tail-latency budget goes with them.
func TestGetReadPathAllocations(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)
	s.wr = resp.NewWriter(discard{})
	s.cl.Out = s.wr
	exec(s, []byte("SET"), []byte("mykey"), []byte("some value of moderate length"))

	args := [][]byte{[]byte("GET"), []byte("mykey")}
	allocs := testing.AllocsPerRun(2000, func() { exec(s, args...) })

	// The budget covers the whole dispatch path: table lookup, arity and
	// state checks, the shard lookup, and serialization. It is a ceiling to
	// notice regressions against, not a target to relax.
	const budget = 0
	if allocs > budget {
		t.Fatalf("GET allocated %.1f times per call, budget is %d", allocs, budget)
	}
	t.Logf("GET: %.2f allocations per call", allocs)
}

// BenchmarkGetNoLatencyTracking shows what the two clock reads per command
// cost, which varies by an order of magnitude across hosts.
func BenchmarkGetNoLatencyTracking(b *testing.B) {
	s := benchSession(b)
	if err := s.h.cfg.Set("track-command-latency", "no"); err != nil {
		b.Fatal(err)
	}
	exec(s, []byte("SET"), []byte("mykey"), []byte("some value of moderate length"))
	args := [][]byte{[]byte("GET"), []byte("mykey")}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		exec(s, args...)
	}
}
