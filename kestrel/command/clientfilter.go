package command

import (
	"strconv"
	"strings"
	"time"

	"kestrel/resp"
)

// clientFilter selects connections for CLIENT LIST and CLIENT KILL.
//
// The same filter serves both, because the two commands ask the same
// question and differ only in what they do with the answer. Having one
// parser means LIST cannot quietly accept a filter KILL rejects, which is
// how an operator ends up killing more than they listed.
type clientFilter struct {
	byID    bool
	ids     map[uint64]struct{}
	addr    string
	laddr   string
	user    string
	kind    string
	skipMe  bool
	maxAge  time.Duration
	hasAge  bool
	hasKind bool
}

// parseClientFilter reads the filter arguments starting at from.
//
// skipMeDefault differs between the two commands: KILL spares the caller
// unless told otherwise, because killing the connection that asked is
// almost never what was meant, while LIST has no reason to hide it.
func parseClientFilter(c *Ctx, from int, listing bool) (*clientFilter, resp.Value) {
	f := &clientFilter{skipMe: !listing}
	for i := from; i < c.Len(); {
		opt := strings.ToUpper(string(c.Arg(i)))
		if i+1 >= c.Len() {
			return nil, errSyntax
		}
		val := string(c.Arg(i + 1))
		switch opt {
		case "ID":
			f.byID = true
			if f.ids == nil {
				f.ids = make(map[uint64]struct{})
			}
			// ID takes a list, so every remaining numeric argument belongs
			// to it.
			j := i + 1
			for ; j < c.Len(); j++ {
				n, err := strconv.ParseUint(string(c.Arg(j)), 10, 64)
				if err != nil {
					break
				}
				f.ids[n] = struct{}{}
			}
			if j == i+1 {
				return nil, errSyntax
			}
			i = j
			continue
		case "ADDR":
			f.addr = val
		case "LADDR":
			f.laddr = val
		case "USER":
			f.user = val
		case "TYPE":
			switch strings.ToLower(val) {
			case "normal", "master", "pubsub":
				f.kind, f.hasKind = strings.ToLower(val), true
			case "replica", "slave":
				f.kind, f.hasKind = "replica", true
			default:
				return nil, resp.Err("ERR Unknown client type '" + val + "'")
			}
		case "SKIPME":
			switch strings.ToUpper(val) {
			case "YES":
				f.skipMe = true
			case "NO":
				f.skipMe = false
			default:
				return nil, errSyntax
			}
		case "MAXAGE":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil || n < 0 {
				return nil, errSyntax
			}
			f.maxAge, f.hasAge = time.Duration(n)*time.Second, true
		default:
			return nil, errSyntax
		}
		i += 2
	}
	return f, resp.Value{}
}

// matches reports whether cl is selected. self is the client that issued the
// command, for SKIPME.
func (f *clientFilter) matches(cl, self *Client, now ...time.Time) bool {
	if f.skipMe && cl == self {
		return false
	}
	if f.byID {
		if _, ok := f.ids[cl.ID]; !ok {
			return false
		}
	}
	if f.addr != "" && cl.Addr != f.addr {
		return false
	}
	if f.laddr != "" && cl.LocalAddr != f.laddr {
		return false
	}
	if f.user != "" && cl.User != f.user {
		return false
	}
	if f.hasKind && clientKind(cl) != f.kind {
		return false
	}
	if f.hasAge {
		at := time.Now()
		if len(now) > 0 {
			at = now[0]
		}
		if at.Sub(cl.CreatedAt) < f.maxAge {
			return false
		}
	}
	return true
}

// clientKind names a connection for the TYPE filter.
//
// "slave" is accepted as an input spelling but never reported: the filter
// normalises it to "replica", so a caller asking for one gets both.
func clientKind(cl *Client) string {
	switch {
	case cl.Replica:
		return "replica"
	case cl.Subscribed():
		return "pubsub"
	default:
		return "normal"
	}
}
