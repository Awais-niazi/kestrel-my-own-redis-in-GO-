package engine

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// model is the naive reference implementation the listpack is compared
// against. Property tests that run the same random operation sequence
// through both, and diff the result, are where subtle collection bugs
// surface (§12).
type model [][]byte

func (m model) equal(l *listpack) error {
	if l.Len() != len(m) {
		return fmt.Errorf("length %d, model has %d", l.Len(), len(m))
	}
	for i := range m {
		got := l.At(i)
		if !bytes.Equal(got, m[i]) {
			return fmt.Errorf("entry %d is %q, model has %q", i, got, m[i])
		}
	}
	// Iteration must agree with indexed access.
	seen := 0
	var iterErr error
	l.Each(func(i int, entry []byte) bool {
		if i != seen {
			iterErr = fmt.Errorf("Each yielded index %d, expected %d", i, seen)
			return false
		}
		if !bytes.Equal(entry, m[i]) {
			iterErr = fmt.Errorf("Each entry %d is %q, model has %q", i, entry, m[i])
			return false
		}
		seen++
		return true
	})
	if iterErr != nil {
		return iterErr
	}
	if seen != len(m) {
		return fmt.Errorf("Each yielded %d entries, model has %d", seen, len(m))
	}
	return nil
}

func TestListpackBasics(t *testing.T) {
	l := newListpack(0)
	if l.Len() != 0 || l.At(0) != nil {
		t.Fatal("a fresh listpack is not empty")
	}
	l.Append([]byte("a"))
	l.Append([]byte(""))
	l.Append([]byte("ccc"))
	if l.Len() != 3 {
		t.Fatalf("len %d", l.Len())
	}
	if string(l.At(0)) != "a" || string(l.At(1)) != "" || string(l.At(2)) != "ccc" {
		t.Fatalf("got %q %q %q", l.At(0), l.At(1), l.At(2))
	}
	if l.At(-1) != nil || l.At(3) != nil {
		t.Fatal("out-of-range access returned data")
	}
	if l.IndexOf([]byte("ccc")) != 2 || l.IndexOf([]byte("nope")) != -1 {
		t.Fatal("IndexOf is wrong")
	}
	if l.MaxEntryLen() != 3 {
		t.Fatalf("MaxEntryLen %d", l.MaxEntryLen())
	}
}

func TestListpackBinarySafe(t *testing.T) {
	l := newListpack(0)
	entries := [][]byte{
		{}, {0}, {0, 0, 0}, []byte("\r\n"), []byte("with\x00null"),
		bytes.Repeat([]byte{0xff}, 300), // forces a multi-byte length header
	}
	for _, e := range entries {
		l.Append(e)
	}
	for i, want := range entries {
		if !bytes.Equal(l.At(i), want) {
			t.Fatalf("entry %d round-tripped as %q", i, l.At(i))
		}
	}
}

func TestListpackInsertSetDelete(t *testing.T) {
	l := newListpack(0)
	for _, s := range []string{"b", "d"} {
		l.Append([]byte(s))
	}
	l.InsertAt(0, []byte("a"))
	l.InsertAt(2, []byte("c"))
	l.InsertAt(99, []byte("e")) // past the end appends
	if got := join(l); got != "a b c d e" {
		t.Fatalf("after inserts: %q", got)
	}

	// Replacement must work when the new entry is shorter, longer, and the
	// same length, because each takes a different path through splice.
	l.SetAt(2, []byte("CCCCCCCC"))
	l.SetAt(0, []byte("A"))
	l.SetAt(4, []byte(""))
	if got := join(l); got != "A b CCCCCCCC d " {
		t.Fatalf("after sets: %q", got)
	}

	l.DeleteAt(0)
	l.DeleteAt(l.Len() - 1)
	if got := join(l); got != "b CCCCCCCC d" {
		t.Fatalf("after deletes: %q", got)
	}
	l.DeleteAt(99) // out of range is a no-op
	if l.Len() != 3 {
		t.Fatalf("len %d", l.Len())
	}
}

func TestListpackDeleteRunAndTruncate(t *testing.T) {
	build := func() *listpack {
		l := newListpack(0)
		for i := 0; i < 10; i++ {
			l.Append(fmt.Appendf(nil, "e%d", i))
		}
		return l
	}
	l := build()
	l.DeleteRun(2, 3)
	if got := join(l); got != "e0 e1 e5 e6 e7 e8 e9" {
		t.Fatalf("DeleteRun: %q", got)
	}
	l = build()
	l.DeleteRun(7, 100) // a run past the end clamps
	if got := join(l); got != "e0 e1 e2 e3 e4 e5 e6" {
		t.Fatalf("clamped DeleteRun: %q", got)
	}
	l = build()
	l.TrimFront(8)
	if got := join(l); got != "e8 e9" {
		t.Fatalf("TrimFront: %q", got)
	}
	l = build()
	l.Truncate(3)
	if got := join(l); got != "e0 e1 e2" {
		t.Fatalf("Truncate: %q", got)
	}
	l.Truncate(0)
	if l.Len() != 0 || l.ByteLen() != 0 {
		t.Fatalf("Truncate(0) left %d entries, %d bytes", l.Len(), l.ByteLen())
	}
}

func TestListpackIndexOfStep(t *testing.T) {
	// A pair-encoded hash: field, value, field, value.
	l := newListpack(0)
	l.AppendMany([]byte("name"), []byte("ana"), []byte("city"), []byte("name"))
	// Searching field positions must not match the value that happens to
	// equal a field name.
	if got := l.IndexOfStep([]byte("name"), 2, 0); got != 0 {
		t.Fatalf("field search returned %d", got)
	}
	if got := l.IndexOfStep([]byte("ana"), 2, 0); got != -1 {
		t.Fatalf("a value was matched as a field: %d", got)
	}
	if got := l.IndexOfStep([]byte("ana"), 2, 1); got != 1 {
		t.Fatalf("value search returned %d", got)
	}
}

func TestListpackClone(t *testing.T) {
	l := newListpack(0)
	l.AppendMany([]byte("a"), []byte("b"))
	c := l.Clone()
	l.SetAt(0, []byte("changed"))
	l.Append([]byte("extra"))
	if string(c.At(0)) != "a" || c.Len() != 2 {
		t.Fatalf("the clone tracked the original: %q len=%d", c.At(0), c.Len())
	}
}

// TestListpackAgainstModel is the property test: random operation sequences
// against a naive slice model, compared after every step.
func TestListpackAgainstModel(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		l := newListpack(0)
		var m model
		for step := 0; step < 200; step++ {
			switch rng.Intn(7) {
			case 0:
				v := randEntry(rng)
				l.Append(v)
				m = append(m, v)
			case 1:
				if len(m) == 0 {
					continue
				}
				i, v := rng.Intn(len(m)+1), randEntry(rng)
				l.InsertAt(i, v)
				if i >= len(m) {
					m = append(m, v)
				} else {
					m = append(m[:i], append(model{v}, m[i:]...)...)
				}
			case 2:
				if len(m) == 0 {
					continue
				}
				i, v := rng.Intn(len(m)), randEntry(rng)
				l.SetAt(i, v)
				m[i] = v
			case 3:
				if len(m) == 0 {
					continue
				}
				i := rng.Intn(len(m))
				l.DeleteAt(i)
				m = append(m[:i], m[i+1:]...)
			case 4:
				if len(m) == 0 {
					continue
				}
				i, n := rng.Intn(len(m)), rng.Intn(5)
				l.DeleteRun(i, n)
				end := i + n
				if end > len(m) {
					end = len(m)
				}
				m = append(m[:i], m[end:]...)
			case 5:
				n := rng.Intn(len(m) + 1)
				l.Truncate(n)
				m = m[:n]
			case 6:
				n := rng.Intn(3)
				l.TrimFront(n)
				if n > len(m) {
					n = len(m)
				}
				m = m[n:]
			}
			if err := l.equalCheck(m); err != nil {
				t.Fatalf("round %d step %d: %v", round, step, err)
			}
		}
	}
}

// equalCheck is a method so the failure message reads naturally.
func (l *listpack) equalCheck(m model) error { return m.equal(l) }

func randEntry(rng *rand.Rand) []byte {
	n := rng.Intn(12)
	if rng.Intn(20) == 0 {
		n = 200 + rng.Intn(100) // occasionally cross the varint header boundary
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + rng.Intn(26))
	}
	return b
}

func join(l *listpack) string {
	var out []byte
	l.Each(func(i int, e []byte) bool {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, e...)
		return true
	})
	return string(out)
}

func BenchmarkListpackAppend(b *testing.B) {
	v := []byte("a moderate value")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := newListpack(0)
		for j := 0; j < 128; j++ {
			l.Append(v)
		}
	}
}

func BenchmarkListpackLookup(b *testing.B) {
	l := newListpack(0)
	for j := 0; j < 128; j++ {
		l.Append(fmt.Appendf(nil, "member-%d", j))
	}
	needle := []byte("member-127")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.IndexOf(needle)
	}
}
