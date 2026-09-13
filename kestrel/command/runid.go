package command

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

var (
	runIDOnce sync.Once
	runID     string
)

// RunID returns this process's 40-character random identifier. It changes on
// every restart, which is what makes it usable as a "have I been talking to
// the same server?" check for clients and for the replication handshake.
func RunID() string {
	runIDOnce.Do(func() {
		var b [20]byte
		if _, err := rand.Read(b[:]); err != nil {
			// A run ID that is not unique is a diagnostic annoyance, never a
			// correctness problem, so a failure here must not stop startup.
			copy(b[:], "kestrel-fallback-runid")
		}
		runID = hex.EncodeToString(b[:])
	})
	return runID
}

func nodeID() string { return RunID() }
