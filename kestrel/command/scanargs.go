package command

import (
	"strings"

	"kestrel/resp"
)

// subScanOptions holds the arguments shared by HSCAN, SSCAN and ZSCAN.
type subScanOptions struct {
	Match    []byte
	Count    int
	NoValues bool
}

// parseScanArgs parses the cursor and options of a collection SCAN variant.
//
// These commands return the whole collection in one call with a zero cursor,
// which is what the reference implementation does for the compact encodings
// and what this implementation extends to the large ones. Go's map has no
// stable iteration order, so a resumable cursor over a promoted hash or set
// cannot be built on top of it; the custom dictionary that would allow it is
// the same follow-up ADR-004 and Q2 describe. COUNT is therefore accepted
// and ignored, and the deviation is documented.
func parseScanArgs(c *Ctx, from int, allowNoValues bool) (uint64, subScanOptions, resp.Value, bool) {
	var opts subScanOptions
	cursor, err := resp.ParseInt(c.Arg(from - 1))
	if err != nil || cursor < 0 {
		return 0, opts, resp.Err("ERR invalid cursor"), false
	}
	for i := from; i < c.Len(); i++ {
		switch strings.ToUpper(string(c.Arg(i))) {
		case "MATCH":
			if i+1 >= c.Len() {
				return 0, opts, errSyntax, false
			}
			opts.Match = c.Arg(i + 1)
			i++
		case "COUNT":
			if i+1 >= c.Len() {
				return 0, opts, errSyntax, false
			}
			n, err := resp.ParseInt(c.Arg(i + 1))
			if err != nil || n < 1 {
				return 0, opts, errSyntax, false
			}
			opts.Count = int(n)
			i++
		case "NOVALUES":
			if !allowNoValues {
				return 0, opts, errSyntax, false
			}
			opts.NoValues = true
		default:
			return 0, opts, errSyntax, false
		}
	}
	return uint64(cursor), opts, resp.Value{}, true
}

// bulkArray renders a slice of byte strings as an array reply.
func bulkArray(items [][]byte) resp.Value {
	out := make([]resp.Value, len(items))
	for i, v := range items {
		out[i] = resp.Bulk(v)
	}
	return resp.ArrayOf(out)
}

// bulkArrayWithNils renders a slice where a nil entry means "missing".
func bulkArrayWithNils(items [][]byte) resp.Value {
	out := make([]resp.Value, len(items))
	for i, v := range items {
		if v == nil {
			out[i] = resp.Null()
		} else {
			out[i] = resp.Bulk(v)
		}
	}
	return resp.ArrayOf(out)
}
