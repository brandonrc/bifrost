package controller_test

import (
	"testing"

	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/controller/storetest"
)

// TestMemoryStoreConformance is the acceptance gate for the SQLite and
// Postgres backends (Tasks 3-4): it runs the full store-conformance
// suite (ported from mobula-controller/tests/store.rs, see
// storetest's package doc comment) against NewMemoryStore. Every
// backend must pass storetest.RunConformance the same way.
func TestMemoryStoreConformance(t *testing.T) {
	storetest.RunConformance(t, func() controller.Store { return controller.NewMemoryStore() })
}
