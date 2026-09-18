package command

import (
	"strconv"
	"strings"

	"kestrel/resp"
)

// PSyncRequest is what a replica asked for, recorded by the PSYNC handler so
// that the server can take the connection over.
//
// The command layer cannot see a socket, and should not: it decides that a
// handover is wanted and what was asked for, and the server decides whether
// it can be granted and does the work.
type PSyncRequest struct {
	// ReplID is the replication history the replica believes it is
	// continuing, or "?" when it has none.
	ReplID string
	// Offset is the stream offset the replica has already applied. It is
	// meaningful only when ReplID matches this leader's.
	Offset uint64
	// Known reports whether the replica supplied a usable identity and
	// offset at all.
	Known bool
	// ListeningPort is what REPLCONF reported, for INFO. Zero when unknown.
	ListeningPort int
}

func init() {
	register(&Descriptor{
		Name: "REPLCONF", Arity: -1, Flags: Readonly | Admin | Loading | Stale,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "Configures a replication link.",
		Handler:    cmdReplConf,
	})
	register(&Descriptor{
		Name: "PSYNC", Arity: -1, Flags: Readonly | Admin | NoMulti | Stale,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "Starts a replication stream, taking over the connection.",
		Handler:    cmdPSync,
	})
}

// cmdReplConf accepts the options a replica announces before PSYNC.
//
// Unknown options are accepted rather than refused. A replica of a later
// version will announce capabilities this one has never heard of, and
// refusing the handshake over a word it does not need would turn a
// forward-compatible exchange into an outage.
func cmdReplConf(c *Ctx) resp.Value {
	for i := 1; i+1 < c.Len(); i += 2 {
		switch strings.ToLower(string(c.Arg(i))) {
		case "listening-port":
			n, err := resp.ParseInt(c.Arg(i + 1))
			if err != nil || n < 0 || n > 65535 {
				return resp.Err("ERR Invalid listening-port")
			}
			c.Client.ReplicaPort = int(n)
		case "ack":
			// A replica reports how far it has applied. The reply to an ACK
			// is silence: it travels on a connection that is already
			// carrying a stream in the other direction, and a reply would
			// be parsed as part of it.
			n, err := strconv.ParseUint(string(c.Arg(i+1)), 10, 64)
			if err != nil {
				return resp.None()
			}
			c.Client.ReplicaAck.Store(n)
			return resp.None()
		}
	}
	return resp.OK()
}

func cmdPSync(c *Ctx) resp.Value {
	req := &PSyncRequest{ReplID: "?", ListeningPort: c.Client.ReplicaPort}
	if c.Len() >= 3 {
		id := string(c.Arg(1))
		off, err := strconv.ParseUint(string(c.Arg(2)), 10, 64)
		if id != "?" && id != "" && err == nil {
			req.ReplID, req.Offset, req.Known = id, off, true
		}
	}
	// The reply is written by the server, which is the half that knows
	// whether a partial resynchronisation can be served.
	c.Client.PSync = req
	c.Client.Replica = true
	return resp.None()
}
