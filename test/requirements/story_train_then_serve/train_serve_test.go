// Story: from experiment to team endpoint (requirements 1, 2, 6).
//
// Dev-a trains on a throwaway private cluster, deletes it, and deploys the
// result as team-a's one shared endpoint: server-stamped owner, a gateway
// address, invisible to team-b. Redeploying under the same name is an
// update; a second name is 409 until the first is deleted. The audit trail
// records the deploys, the refusal and the delete.
package story_train_then_serve

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bifrost-compute/bifrost/test/requirements/fixture"
	"github.com/bifrost-compute/bifrost/test/requirements/req"
	"github.com/bifrost-compute/bifrost/test/requirements/target"
)

type auditRow struct {
	Subject  *string `json:"subject"`
	Decision string  `json:"decision"`
	Reason   *string `json:"reason"`
	Action   *string `json:"action"`
	Cluster  *string `json:"cluster"`
}

func auditRows(t *testing.T, tgt req.Target) []auditRow {
	t.Helper()
	resp, err := tgt.As("admin").API().ListAuditEventsWithResponse(context.Background(), nil)
	if err != nil || resp.StatusCode() != http.StatusOK {
		t.Fatalf("list_audit_events: err=%v status=%v", err, resp.StatusCode())
	}
	var wrapped struct {
		Items []auditRow `json:"items"`
	}
	if err := json.Unmarshal(resp.Body, &wrapped); err != nil {
		t.Fatalf("list_audit_events: unmarshal: %v", err)
	}
	return wrapped.Items
}

func hasAuditRow(rows []auditRow, decision, action, cluster string) bool {
	for _, r := range rows {
		if r.Decision != decision {
			continue
		}
		if r.Action == nil || *r.Action != action {
			continue
		}
		if r.Cluster == nil || *r.Cluster != cluster {
			continue
		}
		return true
	}
	return false
}

func TestFromExperimentToTeamEndpoint(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 1, "the model trained on a private cluster is deployed as a running service with a server-stamped owner and a gateway address")
	req.Covers(t, 2, "the team's one endpoint is invisible to team-b, redeploy-by-name updates it, and a second name is 409 until the first is gone")
	req.Covers(t, 6, "the training cluster is self-serve and disposable: the endpoint does not depend on it")
	ctx := context.Background()
	devA := fixture.Subject(t, tgt, "dev-a")

	// Train on a private cluster; throw it away before serving — the
	// endpoint must stand on its own.
	train := req.Name("train")
	fixture.MustCreate(t, tgt, "dev-a", train, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", train, "running")
	if st := fixture.Delete(t, tgt, "dev-a", train); st/100 != 2 {
		t.Fatalf("delete training cluster = %d", st)
	}
	fixture.WaitGone(t, tgt, "dev-a", train)

	// Deploy the trained model as the team's endpoint.
	name := req.Name("svc")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(name, "team-a")); st != http.StatusAccepted {
		t.Fatalf("deploy_service: status=%d body=%s, want 202", st, raw)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", name) })
	view := fixture.WaitService(t, tgt, "dev-a", name, "running")
	if owner, _ := view["owner"].(string); owner != devA {
		t.Errorf("service owner = %q, want the server-stamped deployer %q", owner, devA)
	}
	if fixture.GatewayHost(view) == "" {
		t.Errorf("running service carries no gateway_url: %v", view)
	}

	// Team-b cannot see it: get is refused and list excludes it.
	if st, _ := fixture.GetService(t, tgt, "dev-b", name); !fixture.Denied(st) {
		t.Fatalf("dev-b get team-a's service = %d, want 403/404", st)
	}
	blist, err := tgt.As("dev-b").API().ListServicesWithResponse(ctx)
	if err != nil || blist.StatusCode() != http.StatusOK {
		t.Fatalf("list_services as dev-b: err=%v status=%v", err, blist.StatusCode())
	}
	if fixture.Contains(string(blist.Body), name) {
		t.Fatalf("dev-b's list_services includes team-a's %s", name)
	}

	// Redeploy under the same name: an update, still exactly one entry.
	body := fixture.ServiceBody(name, "team-a")
	body.Spec.WorkerCpu = "2"
	if st, raw := fixture.Deploy(t, tgt, "dev-a", body); st != http.StatusAccepted {
		t.Fatalf("redeploy same name: status=%d body=%s, want 202", st, raw)
	}
	fixture.WaitService(t, tgt, "dev-a", name, "running")
	list, err := tgt.As("dev-a").API().ListServicesWithResponse(ctx)
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("list_services: err=%v status=%v", err, list.StatusCode())
	}
	var items []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(list.Body, &items); err != nil {
		t.Fatalf("list_services: unmarshal: %v", err)
	}
	var live []string
	for _, it := range items {
		if it.State != "terminating" && it.State != "terminated" {
			live = append(live, it.Name)
		}
	}
	if len(live) != 1 || live[0] != name {
		t.Fatalf("live services after redeploy = %v, want exactly [%s]", live, name)
	}

	// A second name in the same project is 409 naming the holder, and the
	// refused name does not exist.
	second := req.Name("svc2")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(second, "team-a")); st != http.StatusConflict {
		t.Fatalf("deploy second name in team-a: status=%d body=%s, want 409", st, raw)
	} else if !strings.Contains(string(raw), name) {
		t.Errorf("409 body must name the service that holds the slot (%s): %s", name, raw)
	}
	if st, _ := fixture.GetService(t, tgt, "dev-a", second); st != http.StatusNotFound {
		t.Fatalf("refused service must not exist, get = %d", st)
	}

	// Deleting the first frees the slot; the second deploy lands.
	if st := fixture.DeleteService(t, tgt, "dev-a", name); st != http.StatusAccepted {
		t.Fatalf("delete first: status=%d, want 202", st)
	}
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := fixture.GetService(t, tgt, "dev-a", name)
		if st == http.StatusNotFound {
			return true, "404"
		}
		state, _ := v["state"].(string)
		return state == "terminated" || state == "terminating", "state=" + state
	})
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(second, "team-a")); st != http.StatusAccepted {
		t.Fatalf("redeploy second after delete: status=%d body=%s, want 202", st, raw)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", second) })
	fixture.WaitService(t, tgt, "dev-a", second, "running")

	// The audit trail recorded the deploys, the refusal and the delete.
	rows := auditRows(t, tgt)
	if !hasAuditRow(rows, "allow", "deploy_service", name) {
		t.Errorf("no allow row for deploy_service %s", name)
	}
	if !hasAuditRow(rows, "deny", "deploy_service", second) {
		t.Errorf("no deny row for the refused deploy of %s", second)
	}
	if !hasAuditRow(rows, "allow", "delete_service", name) {
		t.Errorf("no allow row for delete_service %s", name)
	}
	if !hasAuditRow(rows, "allow", "deploy_service", second) {
		t.Errorf("no allow row for the eventual deploy of %s", second)
	}
}
