// Requirement 1 — deploy models from within Jupyter. The four RayService
// operations exist in the contract; on inproc there is no service
// provisioner and on kind/grace no serving path has been proven, so this
// is NotYetBuilt until a deploy converges to a serving endpoint.
package r01_serve_from_jupyter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestDeployServiceConvergesToServing(t *testing.T) {
	tgt := target.Get(t)
	req.NotYetBuilt(t, 1, "deploying a RayService through the API converges to a serving endpoint", func(b *req.B) {
		name := req.Name("svc")
		var body client.DeployServiceJSONRequestBody
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"name":%q,"spec":{"name":%q,"project":"team-a","ray_version":"2.56.0","image":"rayproject/ray:2.56.0",
		  "serve_config_v2":"applications: []\n","head_cpu":"1","head_memory":"2Gi","worker_replicas":0,"worker_cpu":"1","worker_memory":"2Gi","upgrade":"in_place"}}`, name, name)), &body)
		resp, err := tgt.As("dev-a").API().DeployServiceWithResponse(t.Context(), body)
		if err != nil || resp.StatusCode() != http.StatusCreated {
			b.Fatalf("deploy_service: err=%v status=%v body=%s", err, resp.StatusCode(), resp.Body)
		}
		b.Cleanup(func() { _, _ = tgt.As("dev-a").API().DeleteServiceWithResponse(t.Context(), name) })
		req.Eventually(b, tgt, func() (bool, string) {
			g, err := tgt.As("dev-a").API().GetServiceWithResponse(t.Context(), name)
			if err != nil || g.StatusCode() != http.StatusOK {
				return false, "get_service not 200"
			}
			var v struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(g.Body, &v)
			return v.State == "serving" || v.State == "running", "state=" + v.State
		})
	})
}
