package command

import (
	"kestrel/resp"
)

// SAVE, BGSAVE and BGREWRITEAOF are three names for one operation here.
//
// In the reference implementation they are genuinely different: SAVE and
// BGSAVE write an RDB file, BGREWRITEAOF rewrites the append-only file, and
// the two formats live side by side. Kestrel has a single durability story
// -- a snapshot plus the log segments after it -- so a snapshot *is* the
// compaction, and there is nothing for a separate rewrite to do.
//
// They are kept as three commands rather than collapsed into one because
// existing tooling calls whichever it was written against, and an operator
// running BGREWRITEAOF to reclaim disk should get what they asked for. The
// difference is documented in docs/deviations.md rather than hidden.
func init() {
	register(&Descriptor{
		Name: "SAVE", Arity: 1, Flags: Readonly | Admin | NoMulti,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary: "Writes a snapshot and compacts the log, blocking until it " +
			"is finished.",
		Handler: cmdSave,
	})
	register(&Descriptor{
		Name: "BGSAVE", Arity: -1, Flags: Readonly | Admin | NoMulti,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "Writes a snapshot and compacts the log in the background.",
		Handler:    cmdBgSave,
	})
	register(&Descriptor{
		Name: "BGREWRITEAOF", Arity: 1, Flags: Readonly | Admin | NoMulti,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary: "Compacts the append log in the background. The same " +
			"operation as BGSAVE in this implementation.",
		Handler: cmdBgRewriteAOF,
	})
	register(&Descriptor{
		Name: "LASTSAVE", Arity: 1, Flags: Readonly | Loading | Stale | Fast,
		Categories: []string{"admin", "fast", "dangerous"},
		Summary:    "Returns the time of the last successful snapshot.",
		Handler:    cmdLastSave,
	})
}

func cmdSave(c *Ctx) resp.Value {
	if err := c.Host.Snapshot(false); err != nil {
		return saveError(err)
	}
	return resp.OK()
}

func cmdBgSave(c *Ctx) resp.Value {
	// BGSAVE SCHEDULE asks for the save to happen once any in-progress one
	// finishes. There is nothing to defer here -- the maintenance loop will
	// take the next one -- so it is accepted and reported honestly.
	if c.Len() == 2 && upper(c.Arg(1)) == "SCHEDULE" {
		if err := c.Host.Snapshot(true); err != nil {
			return resp.Simple("Background saving scheduled")
		}
		return resp.Simple("Background saving started")
	}
	if c.Len() != 1 {
		return errSyntax
	}
	if err := c.Host.Snapshot(true); err != nil {
		return saveError(err)
	}
	return resp.Simple("Background saving started")
}

func cmdBgRewriteAOF(c *Ctx) resp.Value {
	if err := c.Host.Snapshot(true); err != nil {
		return saveError(err)
	}
	return resp.Simple("Background append only file rewriting started")
}

func cmdLastSave(c *Ctx) resp.Value { return resp.Int(c.Host.LastSave()) }

// saveError turns a refusal into a wire error that says what to do about it.
func saveError(err error) resp.Value {
	return resp.Err("ERR " + err.Error())
}
