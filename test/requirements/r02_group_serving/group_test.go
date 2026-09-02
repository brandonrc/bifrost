// Requirement 2 — groups share models privately: one RayService per group,
// every request authenticated, the caller must belong to the owning group.
package r02_group_serving

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestNonMemberCannotReadAnotherGroupsService(t *testing.T) {
	tgt := target.Get(t)
	req.NotYetBuilt(t, 2, "a service deployed by team-a is 403 to team-b and 200 to team-a", func(b *req.B) {
		name := req.Name("gsvc")
		var body client.DeployServiceJSONRequestBody
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"name":%q,"spec":{"name":%q,"project":"team-a","ray_version":"2.56.0","image":"rayproject/ray:2.56.0",
		  "serve_config_v2":"applications: []\n","head_cpu":"1","head_memory":"2Gi","worker_replicas":0,"worker_cpu":"1","worker_memory":"2Gi","upgrade":"in_place"}}`, name, name)), &body)
		resp, err := tgt.As("dev-a").API().DeployServiceWithResponse(t.Context(), body)
		if err != nil || resp.StatusCode() != http.StatusCreated {
			b.Fatalf("deploy_service: err=%v status=%v body=%s", err, resp.StatusCode(), resp.Body)
		}
		b.Cleanup(func() { _, _ = tgt.As("dev-a").API().DeleteServiceWithResponse(t.Context(), name) })
		other, err := tgt.As("dev-b").API().GetServiceWithResponse(t.Context(), name)
		if err != nil || (other.StatusCode() != http.StatusForbidden && other.StatusCode() != http.StatusNotFound) {
			b.Fatalf("dev-b get team-a's service: err=%v status=%v, want 403/404", err, other.StatusCode())
		}
		own, err := tgt.As("dev-a").API().GetServiceWithResponse(t.Context(), name)
		if err != nil || own.StatusCode() != http.StatusOK {
			b.Fatalf("dev-a get own service: err=%v status=%v", err, own.StatusCode())
		}
	})
}
