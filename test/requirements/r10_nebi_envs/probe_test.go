// Requirement 10 — nebi environments on the cluster. The first thing a
// custom environment image hits is defect 2: the provisioner sets no probes,
// so KubeRay's defaults apply, and those shell out to `wget`. An image
// without it — the checkmaite Ray image on grace, any slim environment —
// runs Ray perfectly and is killed by the liveness probe every ~10 minutes
// (docs/defects/2026-09-02-health-probes-assume-wget.md).
package r10_nebi_envs

import (
	"net/http"
	"os"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// REQ_NOWGET_RAY_IMAGE names a Ray image that ships neither wget nor curl
// (on grace: localhost:32000/checkmaite-api:2.56.0-r1). Without it the body
// skips with a recorded reason; the public rayproject/ray images all carry
// wget, so there is no portable stand-in.
func TestImageWithoutWgetReachesRunning(t *testing.T) {
	tgt := target.Get(t)
	req.NotYetBuilt(t, 10, "KubeRay's default probes shell out to wget; an image without it never reports ready and is restarted by the liveness probe", func(b *req.B) {
		req.NeedK8s(b, tgt)
		image := os.Getenv("REQ_NOWGET_RAY_IMAGE")
		if image == "" {
			b.Skip("REQ_NOWGET_RAY_IMAGE not set: no Ray image without wget available on this target")
		}
		id := req.Name("nowget")
		resp, err := tgt.As("admin").API().CreateClusterWithResponse(t.Context(), fixture.ClusterBodyWithImage(id, "team-a", image, nil))
		if err != nil || resp.StatusCode() != http.StatusCreated {
			b.Fatalf("create: err=%v status=%v body=%s", err, resp.StatusCode(), resp.Body)
		}
		fixture.WaitObserved(b, tgt, "admin", id, "running")
	})
}
