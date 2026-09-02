// Requirement 4 — model serving runs in its own resource pool, separate from
// notebook clusters and UI jobs, so user compute cannot starve it.
package r04_serving_pool

import (
	"net/http"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestServingHasItsOwnPool(t *testing.T) {
	tgt := target.Get(t)
	req.NotYetBuilt(t, 4, "the platform provisions a serving pool distinct from compute pools and admits RayServices to it", func(b *req.B) {
		list, err := tgt.As("admin").API().ListPoolsWithResponse(t.Context())
		if err != nil || list.StatusCode() != http.StatusOK {
			b.Fatalf("list_pools: err=%v status=%v", err, list.StatusCode())
		}
		if !strings.Contains(string(list.Body), `"serving"`) {
			b.Fatalf("no pool marked for serving: %s", list.Body)
		}
	})
}
