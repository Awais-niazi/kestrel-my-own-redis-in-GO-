package command

import (
	"strconv"
	"strings"

	"kestrel/resp"
)

// Version identifies the build. It is reported by INFO, HELLO and the
// COMMAND reply.
const (
	Version    = "0.1.0"
	Name       = "Kestrel"
	Codename   = "kestreld"
	RESPCompat = "7.2.0" // the reference version whose behaviour is targeted
)

func init() {
	register(&Descriptor{
		Name: "PING", Arity: -1, Flags: Readonly | Fast | NoAuth | Loading | Stale | SubscriberOK,
		Categories: []string{"fast", "connection"},
		Summary:    "Returns the server's liveliness response.",
		Handler:    cmdPing,
	})
	register(&Descriptor{
		Name: "ECHO", Arity: 2, Flags: Readonly | Fast | Loading | Stale,
		Categories: []string{"fast", "connection"},
		Summary:    "Returns the given string.",
		Handler:    cmdEcho,
	})
	register(&Descriptor{
		Name: "SELECT", Arity: 2, Flags: Readonly | Fast | Loading | Stale,
		Categories: []string{"fast", "connection"},
		Summary:    "Changes the selected database.",
		Handler:    cmdSelect,
	})
	register(&Descriptor{
		Name: "SWAPDB", Arity: 3, Flags: Write | Fast, Effect: EffectVerbatim,
		Locality:   LocalityCrossShard,
		Categories: []string{"keyspace", "dangerous"},
		Summary:    "Swaps two databases.",
		Handler:    cmdSwapDB,
	})
	register(&Descriptor{
		Name: "AUTH", Arity: -2, Flags: Readonly | Fast | NoAuth | Loading | Stale,
		Categories: []string{"fast", "connection"},
		Summary:    "Authenticates the connection.",
		Handler:    cmdAuth,
	})
	register(&Descriptor{
		Name: "HELLO", Arity: -1, Flags: Readonly | Fast | NoAuth | Loading | Stale,
		Categories: []string{"fast", "connection"},
		Summary:    "Handshakes with the server and optionally switches protocol.",
		Handler:    cmdHello,
	})
	register(&Descriptor{
		Name: "QUIT", Arity: -1, Flags: Readonly | Fast | NoAuth | Loading | Stale,
		Categories: []string{"fast", "connection"},
		Summary:    "Closes the connection.",
		Handler:    cmdQuit,
	})
	register(&Descriptor{
		Name: "RESET", Arity: 1, Flags: Readonly | Fast | NoAuth | Loading | Stale,
		Categories: []string{"fast", "connection"},
		Summary:    "Resets the connection to its default state.",
		Handler:    cmdReset,
	})
	register(&Descriptor{
		Name: "CLIENT", Arity: -2, Flags: Readonly | Loading | Stale,
		Categories: []string{"slow", "connection"},
		Summary:    "A container for client connection commands.",
		Subcommands: map[string]*Descriptor{
			"ID": {Arity: 2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Returns the connection ID.", Handler: cmdClientID},
			"GETNAME": {Arity: 2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Returns the connection name.", Handler: cmdClientGetName},
			"SETNAME": {Arity: 3, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Sets the connection name.", Handler: cmdClientSetName},
			"INFO": {Arity: 2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Returns information about the connection.", Handler: cmdClientInfo},
			"NO-EVICT": {Arity: 3, Flags: Readonly | Fast | Admin | Loading | Stale,
				Summary: "Sets the client eviction mode.", Handler: cmdClientNoEvict},
			"LIST": {Arity: -2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Lists the connected clients.", Handler: cmdClientList},
			"KILL": {Arity: -3, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Disconnects clients matching a filter.", Handler: cmdClientKill},
			"NO-TOUCH": {Arity: 3, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Accepted for compatibility; not implemented.",
				Handler: cmdClientNoTouch},
			"HELP": {Arity: 2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Shows helpful text.", Handler: cmdClientHelp},
		},
	})
}

// pongReply is built once: resp.Simple would convert the string to bytes on
// every call, which is measurable on a command that does nothing else.
var pongReply = resp.Simple("PONG")

func cmdPing(c *Ctx) resp.Value {
	switch c.Len() {
	case 1:
		return pongReply
	case 2:
		return resp.Bulk(c.Arg(1))
	default:
		return errWrongArgs("ping")
	}
}

func cmdEcho(c *Ctx) resp.Value { return resp.Bulk(c.Arg(1)) }

func cmdSelect(c *Ctx) resp.Value {
	n, err := resp.ParseInt(c.Arg(1))
	if err != nil {
		return errNotInteger
	}
	if !c.Client.Select(c.Host.Keyspace(), int(n)) {
		return errDBIndex
	}
	return resp.OK()
}

func cmdSwapDB(c *Ctx) resp.Value {
	i, err1 := resp.ParseInt(c.Arg(1))
	j, err2 := resp.ParseInt(c.Arg(2))
	if err1 != nil || err2 != nil {
		return resp.Err("ERR invalid first DB index")
	}
	if err := c.Host.Keyspace().SwapDB(int(i), int(j)); err != nil {
		return errDBIndex
	}
	c.Dirty(1)
	return resp.OK()
}

func cmdAuth(c *Ctx) resp.Value {
	var user, pass string
	switch c.Len() {
	case 2:
		user, pass = "default", string(c.Arg(1))
	case 3:
		user, pass = string(c.Arg(1)), string(c.Arg(2))
	default:
		return errWrongArgs("auth")
	}
	return authenticate(c, user, pass)
}

func authenticate(c *Ctx, user, pass string) resp.Value {
	acl := c.Host.ACL()
	u := acl.User(user)
	// A server with no password and no ACL users has nothing to
	// authenticate against, and says so rather than failing as though the
	// password were wrong.
	if user == "default" && u != nil && u.NoPass() &&
		c.Host.Config().Snapshot().RequirePass == "" {
		return errAuthNotSet
	}
	if u == nil || !u.CheckPassword(pass) {
		return errAuthFailed
	}
	c.Client.Authenticated = true
	c.Client.User = user
	c.Client.Perms = u
	return resp.OK()
}

func cmdHello(c *Ctx) resp.Value {
	cl := c.Client
	proto := cl.Protocol()

	i := 1
	if i < c.Len() {
		n, err := resp.ParseInt(c.Arg(i))
		if err != nil {
			return resp.Err("NOPROTO unsupported protocol version")
		}
		if n != resp.RESP2 && n != resp.RESP3 {
			return resp.Err("NOPROTO unsupported protocol version")
		}
		proto = int(n)
		i++
	}
	for i < c.Len() {
		switch strings.ToUpper(string(c.Arg(i))) {
		case "AUTH":
			if i+2 >= c.Len() {
				return errSyntax
			}
			if r := authenticate(c, string(c.Arg(i+1)), string(c.Arg(i+2))); r.IsError() {
				return r
			}
			i += 3
		case "SETNAME":
			if i+1 >= c.Len() {
				return errSyntax
			}
			cl.Name = string(c.Arg(i + 1))
			i += 2
		default:
			return resp.Err("ERR Protocol error, got '" + string(c.Arg(i)) +
				"' as HELLO argument")
		}
	}

	cfg := c.Host.Config().Snapshot()
	if cfg.RequirePass != "" && !cl.Authenticated {
		return errNoAuth
	}

	// The protocol switch takes effect from this reply onwards, so the
	// handshake itself is already encoded in the new dialect.
	cl.Out.SetProtocol(proto)

	role := "master"
	if c.Host.IsReplica() {
		role = "replica"
	}
	return resp.Map([]resp.Value{
		resp.BulkString("server"), resp.BulkString(strings.ToLower(Name)),
		resp.BulkString("version"), resp.BulkString(Version),
		resp.BulkString("proto"), resp.Int(int64(proto)),
		resp.BulkString("id"), resp.Int(int64(cl.ID)),
		resp.BulkString("mode"), resp.BulkString("standalone"),
		resp.BulkString("role"), resp.BulkString(role),
		resp.BulkString("modules"), resp.EmptyArray(),
	})
}

func cmdQuit(c *Ctx) resp.Value {
	c.Client.CloseAfterReply = true
	return resp.OK()
}

var resetReply = resp.Simple("RESET")

func cmdReset(c *Ctx) resp.Value {
	cl := c.Client
	cl.Select(c.Host.Keyspace(), 0)
	cl.Name = ""
	cl.Out.SetProtocol(resp.RESP2)
	if c.Host.Config().Snapshot().RequirePass != "" {
		cl.Authenticated = false
	}
	return resetReply
}

func cmdClientID(c *Ctx) resp.Value { return resp.Int(int64(c.Client.ID)) }

func cmdClientGetName(c *Ctx) resp.Value {
	if c.Client.Name == "" {
		return resp.Null()
	}
	return resp.BulkString(c.Client.Name)
}

func cmdClientSetName(c *Ctx) resp.Value {
	name := string(c.Arg(2))
	if strings.ContainsAny(name, " \n\r") {
		return resp.Err("ERR Client names cannot contain spaces, newlines or special characters.")
	}
	c.Client.Name = name
	return resp.OK()
}

func cmdClientInfo(c *Ctx) resp.Value { return resp.BulkString(clientInfoLineOf(c.Client)) }

func clientInfoLineOf(cl *Client) string {
	var b strings.Builder
	b.Grow(160)
	writeField(&b, "id", strconv.FormatUint(cl.ID, 10))
	writeField(&b, "addr", cl.Addr)
	writeField(&b, "laddr", cl.LocalAddr)
	writeField(&b, "name", cl.Name)
	writeField(&b, "db", strconv.Itoa(cl.DBIndex))
	writeField(&b, "resp", strconv.Itoa(cl.Protocol()))
	writeField(&b, "cmd", strings.ToLower(cl.LastCommand()))
	writeField(&b, "user", cl.User)
	return b.String()
}

// writeField appends one "name=value" field, separated from any preceding
// field by a space.
//
// The parts go into the builder one at a time rather than being joined with
// '+' first: concatenating before the call allocates a throwaway string per
// field, which is the cost a Builder exists to avoid. CLIENT LIST reuses
// this when it lands in M6, once per connection per call.
func writeField(b *strings.Builder, name, value string) {
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(name)
	b.WriteByte('=')
	b.WriteString(value)
}

func cmdClientNoEvict(c *Ctx) resp.Value {
	switch strings.ToUpper(string(c.Arg(2))) {
	case "ON", "OFF":
		// Client eviction is not implemented; the option is accepted so
		// that clients which set it unconditionally keep working.
		return resp.OK()
	default:
		return errSyntax
	}
}

func cmdClientNoTouch(c *Ctx) resp.Value {
	switch strings.ToUpper(string(c.Arg(2))) {
	case "ON", "OFF":
		// The access clock is only maintained under an LRU or LFU policy,
		// and a client that asks not to touch it is asking for something
		// that may not be happening at all. Accepting keeps a client that
		// sets it unconditionally working.
		return resp.OK()
	default:
		return errSyntax
	}
}

// cmdClientList renders one line per connection, in the same format as
// CLIENT INFO.
func cmdClientList(c *Ctx) resp.Value {
	filter, reply := parseClientFilter(c, 2, true)
	if filter == nil {
		return reply
	}
	var b strings.Builder
	c.Host.ForEachClient(func(cl *Client) {
		if !filter.matches(cl, c.Client) {
			return
		}
		b.WriteString(clientInfoLineOf(cl))
		b.WriteByte('\n')
	})
	return resp.BulkString(b.String())
}

// cmdClientKill disconnects clients, by address in the old form or by filter
// in the new one.
//
// The two forms return different things -- +OK or the number killed -- which
// is a wart of the reference implementation and is reproduced rather than
// tidied, because tooling written against either form depends on it.
func cmdClientKill(c *Ctx) resp.Value {
	if c.Len() == 3 {
		// Victims are collected before any is closed, here as in the
		// filter form below. Disconnecting inside the walk would take the
		// client table's lock while the walk still holds it.
		addr := string(c.Arg(2))
		var victims []uint64
		c.Host.ForEachClient(func(cl *Client) {
			if cl.Addr == addr {
				victims = append(victims, cl.ID)
			}
		})
		killed := killClients(c, victims)
		if killed == 0 {
			return resp.Err("ERR No such client address in client list")
		}
		return resp.OK()
	}

	filter, reply := parseClientFilter(c, 2, false)
	if filter == nil {
		return reply
	}
	// The victims are collected before any is closed. Disconnecting inside
	// the walk would mutate the client table while it is being iterated.
	var victims []uint64
	c.Host.ForEachClient(func(cl *Client) {
		if filter.matches(cl, c.Client) {
			victims = append(victims, cl.ID)
		}
	})
	return resp.Int(int64(killClients(c, victims)))
}

// killClients disconnects the given clients and returns how many went.
//
// A client that kills itself is closed after its reply rather than during
// it. Closing the socket immediately loses the answer to the command that
// asked, so the caller cannot tell whether it worked -- and the count it was
// waiting for is the only thing CLIENT KILL returns.
func killClients(c *Ctx, victims []uint64) int {
	killed := 0
	for _, id := range victims {
		if id == c.Client.ID {
			c.Client.CloseAfterReply = true
			killed++
			continue
		}
		if c.Host.Disconnect(id) {
			killed++
		}
	}
	return killed
}

func cmdClientHelp(c *Ctx) resp.Value {
	lines := []string{
		"CLIENT <subcommand>",
		"ID                    -- Return the client ID.",
		"GETNAME               -- Return the current connection name.",
		"SETNAME <name>        -- Assign the name to the current connection.",
		"INFO                  -- Return information about the current client.",
		"LIST [filters]        -- List the connected clients.",
		"KILL <addr> | [filters] -- Disconnect clients.",
		"NO-EVICT (ON|OFF)     -- Accepted for compatibility; not implemented.",
		"NO-TOUCH (ON|OFF)     -- Accepted for compatibility; not implemented.",
	}
	out := make([]resp.Value, len(lines))
	for i, l := range lines {
		out[i] = resp.Simple(l)
	}
	return resp.ArrayOf(out)
}
