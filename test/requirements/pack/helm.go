// Package pack renders the bifrost-pack Helm chart and asserts on the
// output. The chart is the deployable unit; a template-level assertion is
// the cheapest place to pin behaviour that only showed up on a real node
// (defect 4: nginx exiting at boot because DNS was not up yet).
package pack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// ChartDir resolves the chart: $PACK_CHART, else a sibling checkout.
// ChartDir is PACK_CHART, explicitly. There is no relative default: a
// developer whose checkout happens to sit beside bifrost-pack would run the
// chart tests and regenerate a traceability matrix CI (which has no chart)
// cannot reproduce — exactly what broke PR #2's L2 job. The matrix must not
// depend on disk layout.
func ChartDir(t testing.TB) string {
	t.Helper()
	if v := os.Getenv("PACK_CHART"); v != "" {
		if _, err := os.Stat(filepath.Join(v, "Chart.yaml")); err != nil {
			t.Fatalf("PACK_CHART=%s has no Chart.yaml: %v", v, err)
		}
		return v
	}
	reason := "bifrost-pack chart not checked out (set PACK_CHART)"
	t.Log(req.Line{Kind: "skip", Req: 0, Reason: reason}.Format())
	t.Skip(reason)
	return ""
}

func helm(t testing.TB, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		reason := "helm not installed"
		t.Log(req.Line{Kind: "skip", Req: 0, Reason: reason}.Format())
		t.Skip(reason)
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Render templates the chart with --set values and returns the manifests.
func Render(t testing.TB, set ...string) string {
	t.Helper()
	args := []string{"template", "t", ChartDir(t), "--set", "nebariApp.api.enabled=false"}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	out, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// RenderErr templates and returns the error (nil if it rendered).
func RenderErr(t testing.TB, set ...string) (string, error) {
	t.Helper()
	args := []string{"template", "t", ChartDir(t), "--set", "nebariApp.api.enabled=false"}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	return helm(t, args...)
}

func mustContain(t testing.TB, hay, needle, why string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Errorf("rendered chart lacks %q — %s", needle, why)
	}
}

func mustNotContain(t testing.TB, hay, needle, why string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Errorf("rendered chart contains %q — %s", needle, why)
	}
}

// indexAfter returns the index just past the first occurrence of needle, or -1.
func indexAfter(s, needle string) int {
	i := strings.Index(s, needle)
	if i < 0 {
		return -1
	}
	return i + len(needle)
}

// ruleAfter returns the rendered text from idx to the next rule or block
// boundary ("- apiGroups" or "---"), i.e. the rest of one RBAC rule.
func ruleAfter(s string, idx int) string {
	rest := s[idx:]
	end := len(rest)
	for _, stop := range []string{"- apiGroups", "\n---"} {
		if j := strings.Index(rest, stop); j >= 0 && j < end {
			end = j
		}
	}
	return rest[:end]
}

func containsVerb(rule, verb string) bool { return strings.Contains(rule, verb) }
