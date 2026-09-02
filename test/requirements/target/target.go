// Package target selects the req.Target for this run from REQ_TARGET:
// inproc (default), or kind / grace — the two configurations of the L3
// cluster target.
package target

import (
	"os"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target/cluster"
	"github.com/brandonrc/bifrost/test/requirements/target/inproc"
)

// Get returns the run's target. A fresh inproc target per test keeps tests
// independent; cluster targets are shared per process by design (one
// deployment, one seeding, one preflight).
func Get(t testing.TB) req.Target {
	t.Helper()
	switch v := os.Getenv("REQ_TARGET"); v {
	case "", "inproc":
		return inproc.New(t)
	case "kind", "grace":
		return cluster.New(t, v)
	default:
		t.Fatalf("REQ_TARGET=%q is not a known target (inproc, kind, grace)", v)
		return nil
	}
}
