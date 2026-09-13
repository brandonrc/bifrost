// Story: production inference gets its own pool (requirements 4, 2).
//
// The administrator carves a serving pool out for a new production project
// and allocates the project into it: services are admitted through the
// project's `<project>-serving` queue and capped by the serving nominal,
// while compute clusters in the same project are admitted by the compute
// side and never read the serving ledger. A run-unique project keeps the
// tight caps from touching any other suite's team-a state on a shared
// target (r04's trick).
package story_serving_pool_isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bifrost-compute/bifrost/pkg/client"
	"github.com/bifrost-compute/bifrost/test/requirements/fixture"
	"github.com/bifrost-compute/bifrost/test/requirements/req"
	"github.com/bifrost-compute/bifrost/test/requirements/target"
)

// servingPool creates a serving pool with an allocation for project
// (nominal as given) and registers teardown. Both are administrator
// actions. Duplicated from r04 per the repo convention that requirement
// packages share only fixture/.
func servingPool(t *testing.T, tgt req.Target, name, project string, nominal string) {
	t.Helper()
	ctx := context.Background()
	admin := tgt.As("admin").API()
	var create client.CreatePoolJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"spec":{"name":%q,"cohort":"req-story-serving","fair_sharing_weight":1,"elastic":false,"purpose":"serving",
		"flavors":[{"name":"default","resources":{"cpu":"64","memory":"256Gi"},"node_labels":{},"taints":[]}]}}`, name)), &create)
	if r, err := admin.CreatePoolWithResponse(ctx, create); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("create serving pool: err=%v status=%v body=%s", err, r.StatusCode(), r.Body)
	}
	t.Cleanup(func() { _, _ = admin.DeletePoolWithResponse(context.Background(), name) })
	var alloc client.PutAllocationJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"namespace":%q,"nominal":%s,"borrowing_limit":{},"lending_limit":{}}`, tgt.Namespace(), nominal)), &alloc)
	if r, err := admin.PutAllocationWithResponse(ctx, name, project, alloc); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("put serving allocation: err=%v status=%v body=%s", err, r.StatusCode(), r.Body)
	}
	t.Cleanup(func() { _, _ = admin.DeleteAllocationWithResponse(context.Background(), name, project) })
}

// setQuotas PUTs the quotas section as admin and restores the snapshot on
// test end (the policy is platform state).
func setQuotas(t *testing.T, tgt req.Target, quotas string) {
	t.Helper()
	ctx := context.Background()
	admin := tgt.As("admin").API()
	before, err := admin.GetPolicyWithResponse(ctx)
	if err != nil || before.JSON200 == nil {
		t.Fatalf("get_policy: err=%v status=%v body=%s", err, before.StatusCode(), before.Body)
	}
	var body client.UpdatePolicyJSONRequestBody
	if err := json.Unmarshal([]byte(quotas), &body); err != nil {
		t.Fatal(err)
	}
	if r, err := admin.UpdatePolicyWithResponse(ctx, body); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("update_policy %s: err=%v status=%v body=%s", quotas, err, r.StatusCode(), r.Body)
	}
	t.Cleanup(func() {
		restore := before.JSON200.Quotas
		_, _ = admin.UpdatePolicyWithResponse(context.Background(), client.UpdatePolicyJSONRequestBody{Quotas: &restore})
	})
}

func TestProductionInferenceGetsItsOwnPool(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 4, "a serving pool admits the project's services through its own queue and caps them by its nominal, while compute clusters never read the serving ledger and stay capped by compute quota")
	req.Covers(t, 2, "the project still has exactly one serving slot: a second name is 409 naming the holder")
	ctx := context.Background()
	admin := tgt.As("admin").API()
	project := req.Name("prod")
	// One canonical service is head 1 + worker_replicas x 1 CPU; the
	// nominal admits exactly one. Memory is generous so cpu binds.
	servingPool(t, tgt, req.Name("prodpool"), project, fmt.Sprintf(`{"cpu":"%d","memory":"1024Gi"}`, 1+fixture.WorkerReplicas()))

	// The production service is admitted through the serving queue.
	svc := req.Name("infer")
	if st, body := fixture.Deploy(t, tgt, "admin", fixture.ServiceBody(svc, project)); st != http.StatusAccepted {
		t.Fatalf("deploy_service: status=%d body=%s, want 202", st, body)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "admin", svc) })
	st, view := fixture.GetService(t, tgt, "admin", svc)
	if st != http.StatusOK {
		t.Fatalf("get_service = %d", st)
	}
	if view["queue"] != project+"-serving" {
		t.Fatalf("service queue = %v, want %s-serving (admitted to the serving pool)", view["queue"], project)
	}

	// One slot per project: a second name is 409 naming the holder.
	if st, raw := fixture.Deploy(t, tgt, "admin", fixture.ServiceBody(req.Name("infer2"), project)); st != http.StatusConflict {
		t.Fatalf("second service name in the project: status=%d body=%s, want 409", st, raw)
	} else if !strings.Contains(string(raw), svc) {
		t.Errorf("409 body must name the service that holds the slot (%s): %s", svc, raw)
	}

	// The serving nominal binds: growing the one service past it is 409,
	// audited as serving_quota_exceeded. (A same-name redeploy is an
	// update, so this probe reaches the serving ledger rather than the
	// one-per-project rule.)
	big := fixture.ServiceBody(svc, project)
	big.Spec.WorkerCpu = "8"
	if st, raw := fixture.Deploy(t, tgt, "admin", big); st != http.StatusConflict {
		t.Fatalf("redeploy past the serving nominal: status=%d body=%s, want 409", st, raw)
	}

	// The ledgers are separate: a compute cluster twice a service's size
	// in the same project is admitted and queue-free.
	cluster := req.Name("train")
	cbody := fixture.ClusterBody(cluster, project, nil)
	cbody.Spec.HeadCpu = "2"
	resp, err := admin.CreateClusterWithResponse(ctx, cbody)
	if err != nil || resp.StatusCode() != http.StatusCreated {
		t.Fatalf("compute cluster with the serving allocation exhausted: err=%v status=%v body=%s, want 201", err, resp.StatusCode(), resp.Body)
	}
	t.Cleanup(func() { fixture.Delete(t, tgt, "admin", cluster) })
	st, cview := fixture.Get(t, tgt, "admin", cluster)
	if st != http.StatusOK {
		t.Fatalf("get_cluster = %d", st)
	}
	if q, ok := cview["queue"]; ok && q != nil {
		t.Fatalf("cluster queue = %v, want null: a compute cluster must not be admitted through the serving pool", q)
	}

	// Compute has its own cap all the same: a 1-CPU policy quota refuses
	// the next cluster (restored on exit).
	setQuotas(t, tgt, fmt.Sprintf(`{"quotas":{%q:{"cpu":1,"memory":1024}}}`, project))
	if st, b := fixture.Create(t, tgt, "admin", req.Name("train2"), project, nil); st != http.StatusConflict {
		t.Fatalf("compute cluster past the compute quota: status=%d body=%s, want 409", st, b)
	}

	// The serving-quota refusal is in the audit trail by name.
	audit, err := admin.ListAuditEventsWithResponse(ctx, nil)
	if err != nil || audit.StatusCode() != http.StatusOK {
		t.Fatalf("list_audit_events: err=%v status=%v", err, audit.StatusCode())
	}
	var wrapped struct {
		Items []struct {
			Decision string  `json:"decision"`
			Reason   *string `json:"reason"`
			Action   *string `json:"action"`
			Cluster  *string `json:"cluster"`
		} `json:"items"`
	}
	if err := json.Unmarshal(audit.Body, &wrapped); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range wrapped.Items {
		if e.Decision == "deny" && e.Reason != nil && *e.Reason == "serving_quota_exceeded" &&
			e.Action != nil && *e.Action == "deploy_service" && e.Cluster != nil && *e.Cluster == svc {
			found = true
		}
	}
	if !found {
		t.Errorf("no serving_quota_exceeded deny row for the oversized redeploy of %s", svc)
	}
}
