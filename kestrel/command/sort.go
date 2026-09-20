package command

import (
	"bytes"
	"sort"
	"strings"

	"kestrel/engine"
	"kestrel/resp"
)

func init() {
	register(&Descriptor{
		Name: "SORT", Arity: -2, Flags: Write | DenyOOM, Effect: EffectVerbatim,
		Locality: LocalityCrossShard,
		FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"write", "list", "set", "sortedset", "slow", "dangerous"},
		Summary: "Sorts the elements in a list, a set, or a sorted set, " +
			"optionally storing the result. BY and GET patterns are not supported.",
		Handler: cmdSort,
	})
	register(&Descriptor{
		Name: "SORT_RO", Arity: -2, Flags: Readonly, FirstKey: 1, LastKey: 1, Step: 1,
		Categories: []string{"read", "list", "set", "sortedset", "slow", "dangerous"},
		Summary:    "Returns the sorted elements of a list, a set, or a sorted set.",
		Handler:    cmdSort,
	})
}

func cmdSort(c *Ctx) resp.Value {
	var (
		desc     bool
		alpha    bool
		store    []byte
		offset   int64
		count    int64 = -1
		hasLimit bool
	)
	readOnly := strings.EqualFold(string(c.Name()), "sort_ro")

	for i := 2; i < c.Len(); i++ {
		switch opt := strings.ToUpper(string(c.Arg(i))); opt {
		case "ASC":
			desc = false
		case "DESC":
			desc = true
		case "ALPHA":
			alpha = true
		case "LIMIT":
			if i+2 >= c.Len() {
				return errSyntax
			}
			o, err1 := resp.ParseInt(c.Arg(i + 1))
			n, err2 := resp.ParseInt(c.Arg(i + 2))
			if err1 != nil || err2 != nil {
				return errNotInteger
			}
			offset, count, hasLimit = o, n, true
			i += 2
		case "STORE":
			if readOnly || i+1 >= c.Len() {
				return errSyntax
			}
			store = c.Arg(i + 1)
			i++
		case "BY", "GET":
			// Pattern-based sorting reaches into other keys, which makes it
			// both a cross-key read and a source of surprising O(n) work.
			// It is documented as out of scope rather than silently ignored,
			// because a client that sends BY and gets an unsorted answer
			// would be much worse off than one that gets an error.
			return errUnsupported("SORT ... "+opt+" pattern",
				"Appendix A: BY and GET patterns are not supported in v1")
		default:
			return errSyntax
		}
	}

	// SORT replays from its own arguments, so it is a write and reaps an
	// expired source; SORT_RO holds no write-ordering lock and only hides one.
	access := engine.WriteAccess
	if readOnly {
		access = engine.ReadAccess
	}
	elements, ok, err := c.DB().SortSource(c.Arg(1), access)
	if err != nil {
		return engineError(err)
	}
	if !ok {
		if store != nil {
			n := c.DB().StoreList(store, nil)
			c.Dirty(1)
			return resp.Int(n)
		}
		return resp.EmptyArray()
	}

	if !alpha {
		// A numeric sort requires every element to parse, and says so
		// rather than falling back to a lexicographic order that would look
		// almost right.
		keys := make([]float64, len(elements))
		for i, e := range elements {
			f, err := engine.ParseFloat(e)
			if err != nil {
				return resp.Err("ERR One or more scores can't be converted into double")
			}
			keys[i] = f
		}
		idx := make([]int, len(elements))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			if keys[idx[a]] != keys[idx[b]] {
				return keys[idx[a]] < keys[idx[b]]
			}
			return bytes.Compare(elements[idx[a]], elements[idx[b]]) < 0
		})
		sorted := make([][]byte, len(elements))
		for i, j := range idx {
			sorted[i] = elements[j]
		}
		elements = sorted
	} else {
		sort.SliceStable(elements, func(a, b int) bool {
			return bytes.Compare(elements[a], elements[b]) < 0
		})
	}

	if desc {
		for i, j := 0, len(elements)-1; i < j; i, j = i+1, j-1 {
			elements[i], elements[j] = elements[j], elements[i]
		}
	}
	if hasLimit {
		elements = limitSlice(elements, offset, count)
	}

	if store != nil {
		n := c.DB().StoreList(store, elements)
		c.Dirty(1)
		return resp.Int(n)
	}
	return bulkArray(elements)
}

func limitSlice(in [][]byte, offset, count int64) [][]byte {
	if offset < 0 || offset >= int64(len(in)) {
		return nil
	}
	in = in[offset:]
	if count >= 0 && count < int64(len(in)) {
		in = in[:count]
	}
	return in
}
