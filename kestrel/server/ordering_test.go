package server

import (
	"os"
	"testing"

	"kestrel/engine"
)

// TestMain turns on the engine's write-ordering assertion for the whole
// binary; see engine/ordering_test.go for why it is on everywhere.
func TestMain(m *testing.M) {
	engine.SetWriteOrderingAssertions(true)
	os.Exit(m.Run())
}
