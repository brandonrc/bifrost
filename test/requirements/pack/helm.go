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
func ChartDir(t testing.TB) string {
	t.Helper()
	if v := os.Getenv("PACK_CHART"); v != "" {
		return v
	}
	p := filepath.Join("..", "..", "..", "..", "bifrost-pack", "chart")
	if _, err := os.Stat(filepath.Join(p, "Chart.yaml")); err != nil {
		reason := "bifrost-pack chart not checked out (set PACK_CHART)"
		t.Log(req.Line{Kind: "skip", Req: 0, Reason: reason}.Format())
		t.Skipf("bifrost-pack chart not found at %s (set PACK_CHART): %s", p, reason)
	}
	return p
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
