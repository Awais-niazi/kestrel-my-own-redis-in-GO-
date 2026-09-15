package engine

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The property tests in this file run random operation sequences against
// both the real collection and a naive model built from Go maps and slices,
// comparing the full contents after every step (§12).
//
// Each runs at three threshold settings: high enough that the compact
// encoding is never left, low enough that promotion happens immediately, and
// somewhere in between so the transition itself is crossed mid-sequence.
// That last one is where the interesting bugs live.

func thresholdVariants() []struct {
	name string
	t    Thresholds
} {
	tight := DefaultThresholds()
	tight.HashMaxListpackEntries, tight.HashMaxListpackValue = 1, 1
	tight.SetMaxIntsetEntries, tight.SetMaxListpackEntries, tight.SetMaxListpackValue = 1, 1, 1
	tight.ListMaxListpackSize, tight.ListMaxListpackValue = 1, 1
	tight.ZSetMaxListpackEntries, tight.ZSetMaxListpackValue = 1, 1

	mid := DefaultThresholds()
	mid.HashMaxListpackEntries = 8
	mid.SetMaxIntsetEntries, mid.SetMaxListpackEntries = 8, 8
	mid.ListMaxListpackSize = 8
	mid.ZSetMaxListpackEntries = 8

	return []struct {
		name string
		t    Thresholds
	}{
		{"compact", DefaultThresholds()},
		{"promoted", tight},
		{"crossing", mid},
	}
}

func TestHashPropertyAgainstModel(t *testing.T) {
	for _, variant := range thresholdVariants() {
		t.Run(variant.name, func(t *testing.T) {
			th := variant.t
			rng := rand.New(rand.NewSource(11))
			for round := 0; round < 40; round++ {
				h := newHash()
				model := map[string]string{}

				for step := 0; step < 150; step++ {
					field := fmt.Sprintf("f%d", rng.Intn(20))
					switch rng.Intn(4) {
					case 0, 1:
						value := fmt.Sprintf("v%d", rng.Intn(1000))
						_, existed := model[field]
						isNew := h.Set([]byte(field), []byte(value), &th)
						if isNew == existed {
							t.Fatalf("Set(%q) reported new=%v, model existed=%v",
								field, isNew, existed)
						}
						model[field] = value
					case 2:
						_, existed := model[field]
						if h.Delete([]byte(field)) != existed {
							t.Fatalf("Delete(%q) disagreed with the model", field)
						}
						delete(model, field)
					case 3:
						want, existed := model[field]
						got, ok := h.Get([]byte(field))
						if ok != existed || (existed && string(got) != want) {
							t.Fatalf("Get(%q) = %q,%v; model has %q,%v",
								field, got, ok, want, existed)
						}
					}

					if h.Len() != len(model) {
						t.Fatalf("round %d step %d: len %d, model %d",
							round, step, h.Len(), len(model))
					}
					// Every field must be readable and every listed field
					// must be in the model.
					fields := h.Fields()
					if len(fields) != len(model) {
						t.Fatalf("Fields returned %d, model has %d", len(fields), len(model))
					}
					for _, f := range fields {
						want, ok := model[string(f)]
						if !ok {
							t.Fatalf("Fields listed unknown field %q", f)
						}
						got, _ := h.Get(f)
						if string(got) != want {
							t.Fatalf("field %q is %q, model has %q", f, got, want)
						}
					}
					if pairs := h.All(); len(pairs) != len(model)*2 {
						t.Fatalf("All returned %d entries for %d fields", len(pairs), len(model))
					}
				}
			}
		})
	}
}

func TestSetPropertyAgainstModel(t *testing.T) {
	for _, variant := range thresholdVariants() {
		t.Run(variant.name, func(t *testing.T) {
			th := variant.t
			rng := rand.New(rand.NewSource(22))
			for round := 0; round < 40; round++ {
				var set *Set
				model := map[string]bool{}

				for step := 0; step < 150; step++ {
					// Mix integer and non-integer members so the intset
					// encoding is both used and abandoned.
					var member string
					if rng.Intn(2) == 0 {
						member = strconv.Itoa(rng.Intn(40))
					} else {
						member = fmt.Sprintf("m%d", rng.Intn(20))
					}
					if set == nil {
						set = newSet([]byte(member), &th)
					}

					switch rng.Intn(4) {
					case 0, 1:
						added := set.Add([]byte(member), &th)
						if added == model[member] {
							t.Fatalf("Add(%q) reported new=%v, model had %v",
								member, added, model[member])
						}
						model[member] = true
					case 2:
						removed := set.Remove([]byte(member))
						if removed != model[member] {
							t.Fatalf("Remove(%q) disagreed with the model", member)
						}
						delete(model, member)
					case 3:
						if set.Contains([]byte(member)) != model[member] {
							t.Fatalf("Contains(%q) disagreed with the model", member)
						}
					}

					if set.Len() != len(model) {
						t.Fatalf("round %d step %d: len %d, model %d",
							round, step, set.Len(), len(model))
					}
					members := set.Members()
					if len(members) != len(model) {
						t.Fatalf("Members returned %d, model has %d", len(members), len(model))
					}
					seen := map[string]bool{}
					for _, m := range members {
						if seen[string(m)] {
							t.Fatalf("Members returned %q twice", m)
						}
						seen[string(m)] = true
						if !model[string(m)] {
							t.Fatalf("Members listed unknown member %q", m)
						}
					}
				}
			}
		})
	}
}

func TestListPropertyAgainstModel(t *testing.T) {
	for _, variant := range thresholdVariants() {
		t.Run(variant.name, func(t *testing.T) {
			th := variant.t
			rng := rand.New(rand.NewSource(33))
			for round := 0; round < 40; round++ {
				l := newList()
				var model []string

				for step := 0; step < 150; step++ {
					value := fmt.Sprintf("e%d", rng.Intn(15))
					switch rng.Intn(7) {
					case 0:
						l.Push([]byte(value), true, &th)
						model = append([]string{value}, model...)
					case 1:
						l.Push([]byte(value), false, &th)
						model = append(model, value)
					case 2:
						got, ok := l.Pop(true)
						if ok != (len(model) > 0) {
							t.Fatalf("Pop(front) reported %v with model length %d", ok, len(model))
						}
						if ok {
							if string(got) != model[0] {
								t.Fatalf("Pop(front) = %q, model has %q", got, model[0])
							}
							model = model[1:]
						}
					case 3:
						got, ok := l.Pop(false)
						if ok {
							if string(got) != model[len(model)-1] {
								t.Fatalf("Pop(back) = %q, model has %q", got, model[len(model)-1])
							}
							model = model[:len(model)-1]
						}
					case 4:
						if len(model) == 0 {
							continue
						}
						i := rng.Intn(len(model))
						if !l.SetAt(i, []byte(value), &th) {
							t.Fatalf("SetAt(%d) failed with model length %d", i, len(model))
						}
						model[i] = value
					case 5:
						count := rng.Intn(5) - 2
						before := len(model)
						removed := l.Remove(count, []byte(value))
						model = modelRemove(model, count, value)
						if removed != before-len(model) {
							t.Fatalf("Remove(%d, %q) reported %d removals, model dropped %d",
								count, value, removed, before-len(model))
						}
					case 6:
						if len(model) == 0 {
							continue
						}
						start, stop := rng.Intn(len(model)), rng.Intn(len(model))
						l.Trim(int64(start), int64(stop))
						model = modelTrim(model, start, stop)
					}

					if l.Len() != len(model) {
						t.Fatalf("round %d step %d: len %d, model %d",
							round, step, l.Len(), len(model))
					}
					got := l.Range(0, -1)
					if len(got) != len(model) {
						t.Fatalf("Range returned %d, model has %d", len(got), len(model))
					}
					for i := range model {
						if string(got[i]) != model[i] {
							t.Fatalf("position %d is %q, model has %q", i, got[i], model[i])
						}
						at, ok := l.At(i)
						if !ok || string(at) != model[i] {
							t.Fatalf("At(%d) = %q,%v; model has %q", i, at, ok, model[i])
						}
					}
				}
			}
		})
	}
}

func modelRemove(model []string, count int, value string) []string {
	var idx []int
	for i, v := range model {
		if v == value {
			idx = append(idx, i)
		}
	}
	switch {
	case count > 0 && count < len(idx):
		idx = idx[:count]
	case count < 0:
		if n := -count; n < len(idx) {
			idx = idx[len(idx)-n:]
		}
	}
	drop := map[int]bool{}
	for _, i := range idx {
		drop[i] = true
	}
	out := model[:0:0]
	for i, v := range model {
		if !drop[i] {
			out = append(out, v)
		}
	}
	return out
}

func modelTrim(model []string, start, stop int) []string {
	if start > stop || start >= len(model) {
		return nil
	}
	if stop >= len(model) {
		stop = len(model) - 1
	}
	out := make([]string, stop-start+1)
	copy(out, model[start:stop+1])
	return out
}

func TestZSetPropertyAgainstModel(t *testing.T) {
	for _, variant := range thresholdVariants() {
		t.Run(variant.name, func(t *testing.T) {
			th := variant.t
			rng := rand.New(rand.NewSource(44))
			for round := 0; round < 30; round++ {
				z := newZSet()
				model := map[string]float64{}

				for step := 0; step < 150; step++ {
					member := fmt.Sprintf("m%d", rng.Intn(20))
					switch rng.Intn(4) {
					case 0, 1:
						score := float64(rng.Intn(20) - 10)
						_, existed := model[member]
						added := z.Add([]byte(member), score, &th)
						if added == existed {
							t.Fatalf("Add(%q) reported new=%v, model existed=%v",
								member, added, existed)
						}
						model[member] = score
					case 2:
						_, existed := model[member]
						if z.Remove([]byte(member)) != existed {
							t.Fatalf("Remove(%q) disagreed with the model", member)
						}
						delete(model, member)
					case 3:
						want, existed := model[member]
						got, ok := z.Score([]byte(member))
						if ok != existed || (existed && got != want) {
							t.Fatalf("Score(%q) = %v,%v; model has %v,%v",
								member, got, ok, want, existed)
						}
					}

					if err := z.checkInvariant(); err != nil {
						t.Fatalf("round %d step %d: %v", round, step, err)
					}
					if z.Len() != len(model) {
						t.Fatalf("round %d step %d: len %d, model %d",
							round, step, z.Len(), len(model))
					}

					// The iteration order must match the model sorted by
					// score and then by member, and ranks must agree.
					want := sortedModel(model)
					got := z.All()
					if len(got) != len(want) {
						t.Fatalf("All returned %d, model has %d", len(got), len(want))
					}
					for i := range want {
						if string(got[i].Member) != want[i].member || got[i].Score != want[i].score {
							t.Fatalf("position %d is %v/%q, model has %v/%q",
								i, got[i].Score, got[i].Member, want[i].score, want[i].member)
						}
						rank, ok := z.Rank([]byte(want[i].member), false)
						if !ok || rank != i {
							t.Fatalf("Rank(%q) = %d,%v; want %d", want[i].member, rank, ok, i)
						}
						rev, _ := z.Rank([]byte(want[i].member), true)
						if rev != len(want)-1-i {
							t.Fatalf("reverse rank of %q is %d, want %d",
								want[i].member, rev, len(want)-1-i)
						}
					}
				}
			}
		})
	}
}

func sortedModel(m map[string]float64) []zentry {
	out := make([]zentry, 0, len(m))
	for member, score := range m {
		out = append(out, zentry{score: score, member: member})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].member < out[j].member
	})
	return out
}

// TestCollectionCloneIsIndependent guards COPY: a cloned collection must not
// share storage with its original in any encoding.
func TestCollectionCloneIsIndependent(t *testing.T) {
	th := DefaultThresholds()
	for _, tight := range []bool{false, true} {
		if tight {
			th.HashMaxListpackEntries, th.SetMaxListpackEntries = 1, 1
			th.ListMaxListpackSize, th.ZSetMaxListpackEntries = 1, 1
			th.SetMaxIntsetEntries = 1
		}

		h := newHash()
		h.Set([]byte("f"), []byte("original"), &th)
		hc := h.Clone().(*Hash)
		h.Set([]byte("f"), []byte("changed"), &th)
		if v, _ := hc.Get([]byte("f")); string(v) != "original" {
			t.Errorf("tight=%v: hash clone tracked the original: %q", tight, v)
		}

		s := newSet([]byte("a"), &th)
		s.Add([]byte("a"), &th)
		sc := s.Clone().(*Set)
		s.Add([]byte("b"), &th)
		if sc.Len() != 1 || sc.Contains([]byte("b")) {
			t.Errorf("tight=%v: set clone tracked the original", tight)
		}

		l := newList()
		l.Push([]byte("a"), false, &th)
		lc := l.Clone().(*List)
		l.Push([]byte("b"), false, &th)
		if lc.Len() != 1 {
			t.Errorf("tight=%v: list clone tracked the original", tight)
		}

		z := newZSet()
		z.Add([]byte("a"), 1, &th)
		zc := z.Clone().(*ZSet)
		z.Add([]byte("b"), 2, &th)
		if zc.Len() != 1 {
			t.Errorf("tight=%v: sorted set clone tracked the original", tight)
		}
	}
}

// TestCollectionReadsSurviveLaterWrites is the concurrency contract from the
// package documentation: a slice handed to a caller must not be rewritten by
// a later mutation, because the caller holds it after the shard lock is
// released.
func TestCollectionReadsSurviveLaterWrites(t *testing.T) {
	th := DefaultThresholds()

	h := newHash()
	h.Set([]byte("f"), []byte("first"), &th)
	held, _ := h.Get([]byte("f"))
	for i := 0; i < 50; i++ {
		h.Set(fmt.Appendf(nil, "pad%d", i), []byte(strings.Repeat("x", 20)), &th)
	}
	h.Set([]byte("f"), []byte("second"), &th)
	if string(held) != "first" {
		t.Errorf("a hash value handed out earlier now reads %q", held)
	}

	l := newList()
	l.Push([]byte("head"), false, &th)
	front := l.Range(0, 0)
	for i := 0; i < 50; i++ {
		l.Push(fmt.Appendf(nil, "e%d", i), false, &th)
	}
	if string(front[0]) != "head" {
		t.Errorf("a list element handed out earlier now reads %q", front[0])
	}

	s := newSet([]byte("alpha"), &th)
	s.Add([]byte("alpha"), &th)
	members := s.Members()
	for i := 0; i < 50; i++ {
		s.Add(fmt.Appendf(nil, "m%d", i), &th)
	}
	if string(members[0]) != "alpha" {
		t.Errorf("a set member handed out earlier now reads %q", members[0])
	}
}

// TestQuicklistInsertAndSplit covers the paths a promoted list only reaches
// through LINSERT: inserting into the middle of a node, and the node split
// that follows once a node outgrows the fill factor. A wrong split silently
// corrupts ordering rather than failing, so the whole list is compared to a
// model after every insert.
func TestQuicklistInsertAndSplit(t *testing.T) {
	th := DefaultThresholds()
	th.ListMaxListpackSize = 8 // promote early, and split often

	l := newList()
	var model []string
	for i := 0; i < 40; i++ {
		v := fmt.Sprintf("base%02d", i)
		l.Push([]byte(v), false, &th)
		model = append(model, v)
	}
	if l.Encoding() != EncodingQuicklist {
		t.Fatalf("list did not promote: %v", l.Encoding())
	}

	rng := rand.New(rand.NewSource(7))
	for step := 0; step < 300; step++ {
		pivot := model[rng.Intn(len(model))]
		value := fmt.Sprintf("ins%03d", step)
		before := rng.Intn(2) == 0

		got := l.Insert([]byte(pivot), []byte(value), before, &th)
		at := indexOfString(model, pivot)
		if !before {
			at++
		}
		model = append(model[:at], append([]string{value}, model[at:]...)...)

		if got != len(model) {
			t.Fatalf("step %d: Insert returned %d, model length is %d", step, got, len(model))
		}
		if err := compareList(l, model); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
	}

	// Inserting against an absent pivot must change nothing.
	if got := l.Insert([]byte("nosuchpivot"), []byte("x"), true, &th); got != -1 {
		t.Fatalf("insert with a missing pivot returned %d", got)
	}
	if err := compareList(l, model); err != nil {
		t.Fatal(err)
	}

	// Draining from both ends must still yield exactly the model.
	for len(model) > 0 {
		if len(model)%2 == 0 {
			v, _ := l.Pop(true)
			if string(v) != model[0] {
				t.Fatalf("front pop gave %q, model has %q", v, model[0])
			}
			model = model[1:]
		} else {
			v, _ := l.Pop(false)
			if string(v) != model[len(model)-1] {
				t.Fatalf("back pop gave %q, model has %q", v, model[len(model)-1])
			}
			model = model[:len(model)-1]
		}
	}
	if l.Len() != 0 {
		t.Fatalf("list has %d elements after draining", l.Len())
	}
}

// TestQuicklistCloneIsIndependent covers COPY of a promoted list, where the
// clone has to duplicate every node rather than share the node slice.
func TestQuicklistCloneIsIndependent(t *testing.T) {
	th := DefaultThresholds()
	th.ListMaxListpackSize = 4

	l := newList()
	for i := 0; i < 30; i++ {
		l.Push(fmt.Appendf(nil, "e%02d", i), false, &th)
	}
	if l.Encoding() != EncodingQuicklist {
		t.Fatal("list did not promote")
	}
	clone := l.Clone().(*List)

	// Mutate the original in every way that touches node storage.
	l.Push([]byte("appended"), false, &th)
	l.Push([]byte("prepended"), true, &th)
	l.SetAt(5, []byte("overwritten"), &th)
	l.Insert([]byte("e10"), []byte("inserted"), true, &th)
	l.Pop(true)

	if clone.Len() != 30 {
		t.Fatalf("clone length is %d, want 30", clone.Len())
	}
	for i := 0; i < 30; i++ {
		want := fmt.Sprintf("e%02d", i)
		got, ok := clone.At(i)
		if !ok || string(got) != want {
			t.Fatalf("clone position %d is %q, want %q", i, got, want)
		}
	}
}

func indexOfString(model []string, want string) int {
	for i, v := range model {
		if v == want {
			return i
		}
	}
	return -1
}

func compareList(l *List, model []string) error {
	if l.Len() != len(model) {
		return fmt.Errorf("length %d, model has %d", l.Len(), len(model))
	}
	got := l.Range(0, -1)
	for i := range model {
		if string(got[i]) != model[i] {
			return fmt.Errorf("position %d is %q, model has %q", i, got[i], model[i])
		}
		at, ok := l.At(i)
		if !ok || string(at) != model[i] {
			return fmt.Errorf("At(%d) = %q,%v; model has %q", i, at, ok, model[i])
		}
	}
	return nil
}
