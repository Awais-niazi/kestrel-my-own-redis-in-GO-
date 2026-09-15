package engine

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// zentry is the reference model: a plain sorted slice.
type zentry struct {
	score  float64
	member string
}

type zmodel []zentry

func (m zmodel) Len() int      { return len(m) }
func (m zmodel) Swap(i, j int) { m[i], m[j] = m[j], m[i] }
func (m zmodel) Less(i, j int) bool {
	if m[i].score != m[j].score {
		return m[i].score < m[j].score
	}
	return m[i].member < m[j].member
}

func (m *zmodel) insert(score float64, member string) {
	*m = append(*m, zentry{score, member})
	sort.Sort(*m)
}

func (m *zmodel) remove(member string) bool {
	for i, e := range *m {
		if e.member == member {
			*m = append((*m)[:i], (*m)[i+1:]...)
			return true
		}
	}
	return false
}

func TestSkiplistBasics(t *testing.T) {
	sl := newSkiplist()
	if sl.First() != nil || sl.Last() != nil || sl.length != 0 {
		t.Fatal("a fresh skiplist is not empty")
	}
	sl.Insert(2, []byte("b"))
	sl.Insert(1, []byte("a"))
	sl.Insert(3, []byte("c"))
	if err := sl.checkInvariants(); err != nil {
		t.Fatal(err)
	}
	if string(sl.First().member) != "a" || string(sl.Last().member) != "c" {
		t.Fatalf("ends are %q and %q", sl.First().member, sl.Last().member)
	}
	if sl.Rank(2, []byte("b")) != 2 {
		t.Fatalf("rank of b is %d", sl.Rank(2, []byte("b")))
	}
	if sl.Rank(9, []byte("nope")) != 0 {
		t.Fatal("rank of an absent member is not zero")
	}
	if !sl.Delete(2, []byte("b")) || sl.Delete(2, []byte("b")) {
		t.Fatal("Delete is wrong")
	}
	if err := sl.checkInvariants(); err != nil {
		t.Fatal(err)
	}
}

func TestSkiplistTiesBreakOnMember(t *testing.T) {
	sl := newSkiplist()
	for _, m := range []string{"delta", "alpha", "charlie", "bravo"} {
		sl.Insert(1, []byte(m))
	}
	var got []string
	for x := sl.First(); x != nil; x = x.levels[0].next {
		got = append(got, string(x.member))
	}
	want := "alpha bravo charlie delta"
	if fmt.Sprint(got) != "["+want+"]" {
		t.Fatalf("equal scores ordered as %v, want %s", got, want)
	}
	if err := sl.checkInvariants(); err != nil {
		t.Fatal(err)
	}
}

func TestSkiplistScoreRanges(t *testing.T) {
	sl := newSkiplist()
	for i := 1; i <= 5; i++ {
		sl.Insert(float64(i), fmt.Appendf(nil, "m%d", i))
	}
	cases := []struct {
		r           ScoreRange
		first, last string
	}{
		{ScoreRange{Min: 2, Max: 4}, "m2", "m4"},
		{ScoreRange{Min: 2, Max: 4, MinExcl: true}, "m3", "m4"},
		{ScoreRange{Min: 2, Max: 4, MaxExcl: true}, "m2", "m3"},
		{ScoreRange{Min: math.Inf(-1), Max: math.Inf(1)}, "m1", "m5"},
		{ScoreRange{Min: 10, Max: 20}, "", ""},
		{ScoreRange{Min: 4, Max: 2}, "", ""},
		{ScoreRange{Min: 3, Max: 3}, "m3", "m3"},
		{ScoreRange{Min: 3, Max: 3, MinExcl: true}, "", ""},
	}
	for _, c := range cases {
		first, last := sl.FirstInScoreRange(c.r), sl.LastInScoreRange(c.r)
		got := func(n *slNode) string {
			if n == nil {
				return ""
			}
			return string(n.member)
		}
		if got(first) != c.first || got(last) != c.last {
			t.Errorf("range %+v gave %q..%q, want %q..%q",
				c.r, got(first), got(last), c.first, c.last)
		}
	}
}

func TestSkiplistLexRanges(t *testing.T) {
	sl := newSkiplist()
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		sl.Insert(0, []byte(m))
	}
	got := func(n *slNode) string {
		if n == nil {
			return ""
		}
		return string(n.member)
	}
	cases := []struct {
		r           LexRange
		first, last string
	}{
		{LexRange{MinInf: true, MaxInf: true}, "a", "e"},
		{LexRange{Min: []byte("b"), Max: []byte("d")}, "b", "d"},
		{LexRange{Min: []byte("b"), Max: []byte("d"), MinExcl: true}, "c", "d"},
		{LexRange{Min: []byte("b"), Max: []byte("d"), MaxExcl: true}, "b", "c"},
		{LexRange{Min: []byte("x"), MaxInf: true}, "", ""},
		{LexRange{MinInf: true, Max: []byte("0")}, "", ""},
	}
	for _, c := range cases {
		if got(sl.FirstInLexRange(c.r)) != c.first || got(sl.LastInLexRange(c.r)) != c.last {
			t.Errorf("lex range %+v gave %q..%q, want %q..%q", c.r,
				got(sl.FirstInLexRange(c.r)), got(sl.LastInLexRange(c.r)), c.first, c.last)
		}
	}
}

// TestSkiplistAgainstModel is the property test. Span arithmetic is the part
// of ADR-005 most likely to be subtly wrong, and a wrong span produces a
// structure that iterates correctly while reporting bad ranks, so the
// invariant check runs after every single operation.
func TestSkiplistAgainstModel(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for round := 0; round < 30; round++ {
		sl := newSkiplist()
		var m zmodel
		present := map[string]float64{}

		for step := 0; step < 400; step++ {
			switch rng.Intn(3) {
			case 0, 1:
				member := fmt.Sprintf("m%d", rng.Intn(80))
				score := float64(rng.Intn(15))
				if rng.Intn(6) == 0 {
					score = math.Inf(1) // exercise the infinite endpoints
				}
				if old, ok := present[member]; ok {
					sl.Delete(old, []byte(member))
					m.remove(member)
				}
				sl.Insert(score, []byte(member))
				m.insert(score, member)
				present[member] = score
			case 2:
				member := fmt.Sprintf("m%d", rng.Intn(80))
				old, ok := present[member]
				removed := sl.Delete(old, []byte(member))
				if removed != ok {
					t.Fatalf("round %d step %d: Delete(%q) = %v, present = %v",
						round, step, member, removed, ok)
				}
				if ok {
					m.remove(member)
					delete(present, member)
				}
			}

			if err := sl.checkInvariants(); err != nil {
				t.Fatalf("round %d step %d: %v", round, step, err)
			}
			if sl.length != len(m) {
				t.Fatalf("round %d step %d: length %d, model %d", round, step, sl.length, len(m))
			}
			// Compare the whole sequence to the model.
			i := 0
			for x := sl.First(); x != nil; x = x.levels[0].next {
				if string(x.member) != m[i].member || x.score != m[i].score {
					t.Fatalf("round %d step %d: position %d is %v/%q, model has %v/%q",
						round, step, i, x.score, x.member, m[i].score, m[i].member)
				}
				i++
			}
		}
	}
}

func TestSkiplistLargeRanks(t *testing.T) {
	sl := newSkiplist()
	const n = 5000
	for i := 0; i < n; i++ {
		sl.Insert(float64(i), fmt.Appendf(nil, "m%05d", i))
	}
	if sl.length != n {
		t.Fatalf("length %d", sl.length)
	}
	for _, i := range []int{1, 2, 1000, 2500, n - 1, n} {
		node := sl.NodeByRank(i)
		if node == nil {
			t.Fatalf("NodeByRank(%d) is nil", i)
		}
		if want := fmt.Sprintf("m%05d", i-1); string(node.member) != want {
			t.Fatalf("NodeByRank(%d) = %q, want %q", i, node.member, want)
		}
		if got := sl.Rank(node.score, node.member); got != i {
			t.Fatalf("Rank round trip for %d gave %d", i, got)
		}
	}
	if err := sl.checkInvariants(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkSkiplistInsert(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sl := newSkiplist()
		for j := 0; j < 1000; j++ {
			sl.Insert(float64(j), fmt.Appendf(nil, "m%d", j))
		}
	}
}

func BenchmarkSkiplistRank(b *testing.B) {
	sl := newSkiplist()
	for j := 0; j < 100000; j++ {
		sl.Insert(float64(j), fmt.Appendf(nil, "m%06d", j))
	}
	member := []byte("m050000")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sl.Rank(50000, member)
	}
}
