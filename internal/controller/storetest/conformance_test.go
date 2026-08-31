package storetest_test

import (
	"testing"

	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/controller/storetest"
)

// TestConformanceSelf exercises RunConformance against NewMemoryStore
// from inside the storetest package's own test binary. Go's per-package
// coverage instrumentation only credits a package's statements to ITS
// OWN coverage percentage; a package with no test files of its own
// (storetest has none — see the package doc comment on why) reports 0%
// under `go test -coverprofile=... ./...`, even though every line in it
// runs whenever another package's test imports and calls it. Without
// this file, that 0% drags the repo-wide coverage gate
// (scripts/coverage-gate.sh) down by several points for a package that
// is, in reality, fully exercised.
//
// internal/controller/conformance_test.go is the actual wiring the
// Task 2 brief asks for ("wired to NewMemoryStore in a test in
// internal/controller"); this file exists purely so storetest's own
// coverage number is honest, not as a second acceptance gate.
func TestConformanceSelf(t *testing.T) {
	storetest.RunConformance(t, func() controller.Store { return controller.NewMemoryStore() })
}
