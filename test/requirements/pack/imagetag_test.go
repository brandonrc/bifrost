package pack

import (
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// The chart's default image tag once named an image that never existed
// (AppVersion is not a published tag). It must refuse to render without one.
func TestImageTagIsRequired(t *testing.T) {
	req.Covers(t, 8, "a deployment cannot silently point at a nonexistent image")

	out, err := RenderErr(t)
	if err == nil {
		t.Fatalf("chart rendered without image.tag; it must refuse:\n%s", out)
	}
	if !strings.Contains(out, "image.tag is required") {
		t.Errorf("refusal should name image.tag; got:\n%s", out)
	}
	if _, err := RenderErr(t, "image.tag=sha-test"); err != nil {
		t.Errorf("chart failed to render WITH image.tag: %v", err)
	}
}
