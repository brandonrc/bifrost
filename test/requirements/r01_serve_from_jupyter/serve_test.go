// Requirement 1 — deploy models from within Jupyter. A developer deploys a
// RayService through the API (the same four service operations the
// JupyterLab extension drives); the control plane stores it, the service
// reconciler converges it against KubeRay, and get_service reports the
// observed state and Serve URL. Deploy answers the contract's 202 (plan
// ruling D11): accepted and converging, not created.
package r01_serve_from_jupyter

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestDeployServiceConvergesToServing(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 1, "deploying a RayService through the API converges to a serving endpoint")
	name := req.Name("svc")
	st, body := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(name, "team-a"))
	if st != http.StatusAccepted {
		t.Fatalf("deploy_service: status=%d body=%s, want 202", st, body)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", name) })

	view := fixture.WaitService(t, tgt, "dev-a", name, "running")
	if view["project"] != "team-a" {
		t.Errorf("project = %v, want team-a", view["project"])
	}
	if owner, _ := view["owner"].(string); owner == "" {
		t.Errorf("owner must be server-stamped from the deploying identity, got %v", view["owner"])
	}
	if url, _ := view["url"].(string); url == "" {
		t.Errorf("a running service must report its Serve url, got %v", view["url"])
	}
}

// A same-name redeploy is an update, not a conflict: the row's generation
// bumps (asserted directly in internal/api and internal/controller unit
// tests — ServiceView carries no generation), the service stays a single
// entry in list_services, keeps its first owner, and converges to running
// again at the new spec.
func TestRedeploySameNameBumpsGeneration(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 1, "redeploying a service under the same name updates it in place")
	name := req.Name("svc-redeploy")
	body := fixture.ServiceBody(name, "team-a")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", body); st != http.StatusAccepted {
		t.Fatalf("deploy_service: status=%d body=%s, want 202", st, raw)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", name) })
	first := fixture.WaitService(t, tgt, "dev-a", name, "running")

	body.Spec.WorkerCpu = "2"
	if st, raw := fixture.Deploy(t, tgt, "dev-a", body); st != http.StatusAccepted {
		t.Fatalf("redeploy_service: status=%d body=%s, want 202", st, raw)
	}
	second := fixture.WaitService(t, tgt, "dev-a", name, "running")
	if first["owner"] != second["owner"] {
		t.Errorf("owner changed across redeploy: %v -> %v", first["owner"], second["owner"])
	}

	list, err := tgt.As("dev-a").API().ListServicesWithResponse(t.Context())
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("list_services: err=%v status=%v", err, list.StatusCode())
	}
	var items []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(list.Body, &items)
	n := 0
	for _, it := range items {
		if it.Name == name {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("list_services has %d entries named %s, want exactly 1", n, name)
	}
}

// Delete is accepted (202) and the service converges to gone: the view
// reads terminating, then terminated (a tombstone the reconciler reaps
// after the retention window) or 404.
func TestDeleteServiceConvergesToTerminated(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 1, "deleting a service tears its RayService down")
	name := req.Name("svc-del")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(name, "team-a")); st != http.StatusAccepted {
		t.Fatalf("deploy_service: status=%d body=%s, want 202", st, raw)
	}
	fixture.WaitService(t, tgt, "dev-a", name, "running")
	if st := fixture.DeleteService(t, tgt, "dev-a", name); st != http.StatusAccepted {
		t.Fatalf("delete_service: status=%d, want 202", st)
	}
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := fixture.GetService(t, tgt, "dev-a", name)
		if st == http.StatusNotFound {
			return true, "404"
		}
		state, _ := v["state"].(string)
		return state == "terminated", "state=" + state
	})
}

// The Serve endpoint answers through the gateway: GET / with the service's
// gateway host as a team-a member is 200, anonymous is 401.
//
// Gated on two capabilities. "gateway": a --gateway-domain is configured so
// the reconciler registers `<name>.<domain>` (package B adds the `serve`
// authorization branch in the gateway middleware; without it this request
// is 403). "serve-fixture": the target can route a request to a Serve app
// — inproc has no routing (.invalid resolves nowhere) and on kind the
// gateway needs the serve app fixture (plan ruling D6: a ConfigMap-mounted
// app.py through the private-storage file catalog, package G) before a
// real HTTP round trip is honest. No target declares serve-fixture yet;
// flipping it is the follow-up once G lands.
func TestServeEndpointAnswersThroughTheGateway(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 1, "the Serve endpoint is reachable through the authenticated gateway")
	req.NeedsCapability(t, tgt, "gateway")
	req.NeedsCapability(t, tgt, "serve-fixture")
	name := req.Name("svc-gw")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(name, "team-a")); st != http.StatusAccepted {
		t.Fatalf("deploy_service: status=%d body=%s, want 202", st, raw)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", name) })
	view := fixture.WaitService(t, tgt, "dev-a", name, "running")
	gatewayURL, _ := view["gateway_url"].(string)
	if gatewayURL == "" {
		t.Fatalf("running service has no gateway_url: %v", view)
	}
	host := gatewayURL[len("https://"):]
	if i := len(host); i > 0 && host[len(host)-1] == '/' {
		host = host[:len(host)-1]
	}

	do := func(principal string) int {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tgt.BaseURL()+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = host
		tgt.As(principal).Authorize(r)
		resp, err := fixture.HTTPClient(tgt).Do(r)
		if err != nil {
			t.Fatalf("GET / as %s via %s: %v", principal, host, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	if st := do("dev-a"); st != http.StatusOK {
		t.Errorf("dev-a GET / via gateway = %d, want 200", st)
	}
	if st := do("anon"); st != http.StatusUnauthorized {
		t.Errorf("anonymous GET / via gateway = %d, want 401", st)
	}
}
