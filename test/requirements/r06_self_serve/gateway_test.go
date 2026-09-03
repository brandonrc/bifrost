package r06_self_serve

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// The "remote connect" caveat of SPEC row 6: a cluster's view carries the
// gateway address at which its Jobs API is reachable from off-cluster, and
// the gateway's routing table shows the entry the reconciler registered.
func TestClusterViewCarriesGatewayAddress(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "a running cluster's view carries its gateway address, registered dynamically by the controller")
	req.NeedsCapability(t, tgt, "gateway")
	ctx := context.Background()
	id := req.Name("gwv")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	var host string
	req.Eventually(t, tgt, func() (bool, string) {
		_, v := fixture.Get(t, tgt, "dev-a", id)
		host = fixture.GatewayHost(v)
		return host != "", "gateway_url not set"
	})
	if !fixture.Contains(host, id) {
		t.Fatalf("gateway host %q does not name the cluster %s", host, id)
	}

	resp, err := tgt.As("admin").API().ListRegistryWithResponse(ctx)
	if err != nil || resp.StatusCode() != http.StatusOK {
		t.Fatalf("list registry: %v %v", err, resp.StatusCode())
	}
	var entries []struct {
		Id       string `json:"id"`
		Hostname string `json:"hostname"`
		Source   string `json:"source"`
		Target   string `json:"target"`
	}
	_ = json.Unmarshal(resp.Body, &entries)
	for _, e := range entries {
		if e.Id == id {
			if e.Hostname != host || e.Source != "dynamic" || e.Target != "jobs" {
				t.Fatalf("registry entry = %+v, want hostname %s, dynamic, jobs", e, host)
			}
			// Once the cluster is deleted the entry goes with it.
			fixture.Delete(t, tgt, "dev-a", id)
			req.Eventually(t, tgt, func() (bool, string) {
				_, v := fixture.Get(t, tgt, "dev-a", id)
				return fixture.GatewayHost(v) == "", "still registered"
			})
			return
		}
	}
	t.Fatalf("registry has no entry for %s: %s", id, resp.Body)
}
