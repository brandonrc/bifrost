package r03_rbac

import (
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// A private cluster's Jobs API is reached only through the authenticated
// gateway (direct :8265 is refused at the network — see
// TestCrossOwnerHeadPortsAreUnreachable): anonymous callers get 401,
// another project's developer 403, and the owner's project 200.
func TestHeadJobsApiOnlyThroughGateway(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "the head's Jobs API is reachable only through the gateway, authenticated and scoped to the cluster's project")
	req.NeedsCapability(t, tgt, "gateway")
	id := req.Name("gwa")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	var host string
	req.Eventually(t, tgt, func() (bool, string) {
		_, v := fixture.Get(t, tgt, "dev-a", id)
		host = fixture.GatewayHost(v)
		return host != "", "gateway_url not set"
	})
	if st, _ := fixture.GatewayRequest(t, tgt, "anon", host, "/api/jobs/"); st != http.StatusUnauthorized {
		t.Fatalf("anonymous via gateway = %d, want 401", st)
	}
	// /healthz is public on the control plane; on a cluster host nothing is.
	if st, _ := fixture.GatewayRequest(t, tgt, "anon", host, "/healthz"); st != http.StatusUnauthorized {
		t.Fatalf("anonymous /healthz on a cluster host = %d, want 401", st)
	}
	if st, _ := fixture.GatewayRequest(t, tgt, "dev-b", host, "/api/jobs/"); st != http.StatusForbidden {
		t.Fatalf("other project's developer via gateway = %d, want 403", st)
	}
	if _, ok := tgt.K8s(); ok {
		req.Eventually(t, tgt, func() (bool, string) {
			st, body := fixture.GatewayRequest(t, tgt, "dev-a", host, "/api/jobs/")
			return st == http.StatusOK, http.StatusText(st) + " " + string(body)
		})
	}
}
