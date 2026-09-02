// Package target selects the req.Target for this run from REQ_TARGET:
// inproc (default) in P0; kind and grace arrive with the cluster target in P2.
package target

import (
	"os"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target/inproc"
)

// Get returns the run's target. A fresh inproc target per test keeps tests
// independent; cluster targets (P2) are shared per process by design.
func Get(t testing.TB) req.Target {
	t.Helper()
	switch v := os.Getenv("REQ_TARGET"); v {
	case "", "inproc":
		return inproc.New(t)
	default:
		t.Fatalf("REQ_TARGET=%q is not available in this build (P0 ships inproc only)", v)
		return nil
	}
}
