package command

import "kestrel/resp"

// Commands that exist in the reference implementation but are deliberately
// not in this version get an explicit registration rather than falling
// through to "unknown command".
//
// This is the mitigation for R4 and R7: a client that hits a gap learns
// which gap it is and where it is documented, and the server counts the
// attempt so that the compatibility backlog is driven by real traffic rather
// than by guesswork.
func init() {
	unsupported := []struct {
		name  string
		arity int
		why   string
	}{
		{"EVAL", -3, "NG2, ADR-014"},
		{"EVALSHA", -3, "NG2, ADR-014"},
		{"EVAL_RO", -3, "NG2, ADR-014"},
		{"EVALSHA_RO", -3, "NG2, ADR-014"},
		{"SCRIPT", -2, "NG2, ADR-014"},
		{"FUNCTION", -2, "NG2, ADR-014"},
		{"FCALL", -3, "NG2, ADR-014"},
		{"FCALL_RO", -3, "NG2, ADR-014"},
		{"XADD", -5, "NG4"},
		{"XREAD", -4, "NG4"},
		{"XRANGE", -4, "NG4"},
		{"XLEN", 2, "NG4"},
		{"XINFO", -2, "NG4"},
		{"XGROUP", -2, "NG4"},
		{"DUMP", 2, "NG7: no RDB format compatibility"},
		{"RESTORE", -4, "NG7: no RDB format compatibility"},
		{"MIGRATE", -6, "NG7: no RDB format compatibility"},
		{"FAILOVER", -1, "NG5"},
		{"PFADD", -2, "v2: HyperLogLog"},
		{"PFCOUNT", -2, "v2: HyperLogLog"},
		{"PFMERGE", -2, "v2: HyperLogLog"},
		{"BITFIELD", -2, "v2"},
		{"LCS", -3, "v2"},
		{"LATENCY", -2, "v2: histograms are available through the metrics endpoint"},
		{"SSUBSCRIBE", -2, "requires cluster mode, NG1"},
		{"SUNSUBSCRIBE", -1, "requires cluster mode, NG1"},
		{"SPUBLISH", 3, "requires cluster mode, NG1"},
	}
	for _, u := range unsupported {
		name, why := u.name, u.why
		register(&Descriptor{
			Name: name, Arity: u.arity, Flags: Readonly | Loading | Stale,
			Categories: []string{"slow"},
			Summary:    "Not supported in this version (" + why + ").",
			Handler: func(c *Ctx) resp.Value {
				c.Host.Stats().UnsupportedCommands.Add(1)
				return errUnsupported(name, why)
			},
		})
	}

	// CLUSTER answers enough to let a cluster-aware client discover that
	// clustering is off and fall back to single-node behaviour (ADR-011).
	register(&Descriptor{
		Name: "CLUSTER", Arity: -2, Flags: Readonly | Loading | Stale,
		Categories: []string{"slow"},
		Summary:    "A container for cluster commands. Cluster mode is disabled (NG1).",
		Subcommands: map[string]*Descriptor{
			"INFO": {Arity: 2, Flags: Readonly | Loading | Stale,
				Summary: "Returns cluster state, which is always disabled.",
				Handler: func(c *Ctx) resp.Value {
					return resp.Verbatim("txt", "cluster_enabled:0\r\ncluster_state:ok\r\n"+
						"cluster_slots_assigned:0\r\ncluster_known_nodes:1\r\ncluster_size:0\r\n")
				}},
			"MYID": {Arity: 2, Flags: Readonly | Loading | Stale,
				Summary: "Returns this node's ID.",
				Handler: func(c *Ctx) resp.Value { return resp.BulkString(nodeID()) }},
			"SLOTS": {Arity: 2, Flags: Readonly | Loading | Stale,
				Summary: "Returns the slot map, which is empty.",
				Handler: func(c *Ctx) resp.Value { return resp.EmptyArray() }},
			"SHARDS": {Arity: 2, Flags: Readonly | Loading | Stale,
				Summary: "Returns the shard map, which is empty.",
				Handler: func(c *Ctx) resp.Value { return resp.EmptyArray() }},
			"COUNTKEYSINSLOT": {Arity: 3, Flags: Readonly | Loading | Stale,
				Summary: "Returns the number of keys in a slot, which is always zero.",
				Handler: func(c *Ctx) resp.Value { return resp.Int(0) }},
		},
	})
}
