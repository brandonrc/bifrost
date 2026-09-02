package requirements

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const internalPrefix = `"github.com/brandonrc/bifrost/internal/`

func goFiles(t *testing.T, root string, testOnly bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		if testOnly && !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func parse(t *testing.T, p string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	return f
}

// Rule 1 (spec §1.3): nothing under test/requirements imports internal/,
// except the inproc target, which must (it calls app.New).
func TestNoInternalImportsOutsideInproc(t *testing.T) {
	for _, p := range goFiles(t, ".", false) {
		if strings.HasPrefix(filepath.ToSlash(p), "target/inproc/") {
			continue
		}
		for _, imp := range parse(t, p).Imports {
			if strings.HasPrefix(imp.Path.Value, internalPrefix) {
				t.Errorf("%s imports %s; requirement tests speak the public contract only", p, imp.Path.Value)
			}
		}
	}
}

// Rule 2: every Test* in a requirement package declares what it proves.
func TestEveryRequirementTestDeclaresCoverage(t *testing.T) {
	for _, p := range goFiles(t, ".", true) {
		slash := filepath.ToSlash(p)
		dir := strings.SplitN(slash, "/", 2)[0]
		isReqDir := dir == "contract" || dir == "pack" || (len(dir) >= 3 && dir[0] == 'r' && dir[1] >= '0' && dir[1] <= '9')
		if !isReqDir {
			continue
		}
		f := parse(t, p)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Recv != nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "req" && (sel.Sel.Name == "Covers" || sel.Sel.Name == "NotYetBuilt") {
						found = true
					}
				}
				return !found
			})
			if !found {
				t.Errorf("%s: %s has no req.Covers/req.NotYetBuilt", p, fn.Name.Name)
			}
		}
	}
}

// Rule 3 (spec §8): no bare sleeps in requirement tests; use req.Eventually.
func TestNoTimeSleepInRequirementTests(t *testing.T) {
	for _, p := range goFiles(t, ".", true) {
		if strings.HasPrefix(filepath.ToSlash(p), "req/") {
			continue // req.Eventually's own poll interval lives here
		}
		ast.Inspect(parse(t, p), func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && sel.Sel.Name == "Sleep" {
					t.Errorf("%s uses time.Sleep; use req.Eventually", p)
				}
			}
			return true
		})
	}
}
