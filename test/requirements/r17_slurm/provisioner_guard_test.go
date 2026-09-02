package r17_slurm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// Req 17 is "design must not foreclose Slurm". The seam that would have to
// grow a Slurm implementation is provision.Provisioner; if any of its method
// signatures names a Kubernetes type, a Slurm backend cannot implement it.
func TestProvisionerSeamCarriesNoKubernetesTypes(t *testing.T) {
	req.Covers(t, 17, "provision.Provisioner's method signatures reference no k8s.io / sigs.k8s.io types")

	p := filepath.Join("..", "..", "..", "internal", "provision", "provisioner.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		v := strings.Trim(imp.Path.Value, `"`)
		if strings.HasPrefix(v, "k8s.io/") || strings.HasPrefix(v, "sigs.k8s.io/") {
			t.Errorf("provisioner.go imports %s; the seam must stay engine-agnostic", v)
		}
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Provisioner" {
			return true
		}
		found = true
		return false
	})
	if !found {
		t.Fatal("type Provisioner not found in provisioner.go")
	}
}
