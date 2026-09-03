// Requirement 4 — model serving runs in its own resource pool, separate from
// notebook clusters and UI jobs, so user compute cannot starve it.
//
// The platform does not auto-provision a serving pool (plan ruling D3): an
// administrator declares one with `purpose: serving` and allocates projects
// into it. Services are admitted through that pool's `<project>-serving`
// LocalQueue and capped by the allocation's nominal; clusters and jobs go
// through compute pools and the policy's compute quotas, and never read the
// serving allocation (plan ruling D5: the Bifrost-side ledger is the tested
// property; Kueue's own queueing of RayService-owned RayClusters is
// observed on kind, not required).
package r04_serving_pool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// servingPool creates a serving pool named name with an allocation for
// project (nominal as given) and registers teardown. Both are administrator
// actions: pools are platform configuration.
func servingPool(t *testing.T, tgt req.Target, name, project string, nominal string) {
	t.Helper()
	ctx := context.Background()
	admin := tgt.As("admin").API()
	var create client.CreatePoolJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"spec":{"name":%q,"cohort":"req-serving","fair_sharing_weight":1,"elastic":false,"purpose":"serving",
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

func TestServingHasItsOwnPool(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 4, "an administrator declares a serving pool; services are admitted through its queue and clusters are not")
	ctx := context.Background()
	pool := req.Name("serve")
	// Generous nominal: this test is about placement, not the cap (and on
	// a shared target other suites deploy team-a services concurrently).
	servingPool(t, tgt, pool, "team-a", `{"cpu":"64","memory":"256Gi"}`)

	list, err := tgt.As("admin").API().ListPoolsWithResponse(ctx)
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("list_pools: err=%v status=%v", err, list.StatusCode())
	}
	var pools []struct {
		Name    string `json:"name"`
		Purpose string `json:"purpose"`
	}
	_ = json.Unmarshal(list.Body, &pools)
	found := false
	for _, p := range pools {
		if p.Name == pool {
			found = true
			if p.Purpose != "serving" {
				t.Fatalf("pool %s purpose = %q, want serving: %s", pool, p.Purpose, list.Body)
			}
		}
	}
	if !found {
		t.Fatalf("serving pool %s not listed: %s", pool, list.Body)
	}

	// A team-a service is admitted through the serving queue.
	svc := req.Name("svc")
	if st, body := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(svc, "team-a")); st != http.StatusAccepted {
		t.Fatalf("deploy_service: status=%d body=%s, want 202", st, body)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", svc) })
	st, view := fixture.GetService(t, tgt, "dev-a", svc)
	if st != http.StatusOK {
		t.Fatalf("get_service = %d", st)
	}
	if view["queue"] != "team-a-serving" {
		t.Fatalf("service queue = %v, want team-a-serving (admitted to the serving pool)", view["queue"])
	}

	// A team-a cluster is not: the serving allocation is not a compute
	// allocation, so the cluster stays queue-free.
	cluster := req.Name("c")
	fixture.MustCreate(t, tgt, "dev-a", cluster, "team-a")
	t.Cleanup(func() { fixture.Delete(t, tgt, "dev-a", cluster) })
	st, cview := fixture.Get(t, tgt, "dev-a", cluster)
	if st != http.StatusOK {
		t.Fatalf("get_cluster = %d", st)
	}
	if q, ok := cview["queue"]; ok && q != nil {
		t.Fatalf("cluster queue = %v, want null: a compute cluster must not be admitted through the serving pool", q)
	}
}

// The serving allocation is a ledger for services only. Its nominal caps
// the project's summed service demand (409 past it) while a compute cluster
// of any size in the same project is admitted — it never reads the serving
// allocation — and compute over-commit is still refused by the policy's
// compute quota. A run-unique project keeps the tight cap from touching
// other suites' team-a services on a shared target; the administrator has
// global write, so it needs no role assignment.
func TestComputeClusterCannotConsumeServingQuota(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 4, "a project's serving allocation caps its services but not its compute clusters, and compute quota still caps compute")
	ctx := context.Background()
	admin := tgt.As("admin").API()
	project := req.Name("sq")
	// One canonical service is head 1 + worker_replicas × 1 CPU; the
	// nominal admits exactly one. Memory is generous so cpu binds.
	servingPool(t, tgt, req.Name("sqpool"), project, fmt.Sprintf(`{"cpu":"%d","memory":"1024Gi"}`, 1+fixture.WorkerReplicas()))

	first := req.Name("s1")
	if st, body := fixture.Deploy(t, tgt, "admin", fixture.ServiceBody(first, project)); st != http.StatusAccepted {
		t.Fatalf("first service within the serving allocation: status=%d body=%s, want 202", st, body)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "admin", first) })
	if st, body := fixture.Deploy(t, tgt, "admin", fixture.ServiceBody(req.Name("s2"), project)); st != http.StatusConflict {
		t.Fatalf("second service past the serving allocation: status=%d body=%s, want 409", st, body)
	}

	// The serving ledger is exhausted; a compute cluster twice the size of
	// a service is still 201 because compute never draws on it.
	cluster := req.Name("c1")
	body := fixture.ClusterBody(cluster, project, nil)
	body.Spec.HeadCpu = "2"
	resp, err := admin.CreateClusterWithResponse(ctx, body)
	if err != nil || resp.StatusCode() != http.StatusCreated {
		t.Fatalf("compute cluster with the serving allocation exhausted: err=%v status=%v body=%s, want 201", err, resp.StatusCode(), resp.Body)
	}
	t.Cleanup(func() { fixture.Delete(t, tgt, "admin", cluster) })

	// Compute has its own cap: a policy quota of 1 CPU for the project
	// refuses another cluster (the r07 pattern; restored on exit).
	var quota client.UpdatePolicyJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"quotas":{%q:{"cpu":1,"memory":1024}}}`, project)), &quota)
	if r, err := admin.UpdatePolicyWithResponse(ctx, quota); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("update_policy quotas: err=%v status=%v body=%s", err, r.StatusCode(), r.Body)
	}
	t.Cleanup(func() {
		var restore client.UpdatePolicyJSONRequestBody
		_ = json.Unmarshal([]byte(`{"quotas":{}}`), &restore)
		_, _ = admin.UpdatePolicyWithResponse(context.Background(), restore)
	})
	if st, b := fixture.Create(t, tgt, "admin", req.Name("c2"), project, nil); st != http.StatusConflict {
		t.Fatalf("compute cluster past the compute quota: status=%d body=%s, want 409", st, b)
	}
}

// On Kubernetes the pool reconciler materializes the allocation as a Kueue
// LocalQueue named `<project>-serving` in the project's namespace —
// distinct from the compute pool's `<project>` queue, so the two never
// collide in one namespace.
func TestServingLocalQueueExists(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 4, "a serving allocation becomes a `<project>-serving` Kueue LocalQueue in the project namespace")
	req.NeedK8s(t, tgt)
	k, _ := tgt.K8s()
	servingPool(t, tgt, req.Name("lq"), "team-a", `{"cpu":"64","memory":"256Gi"}`)

	lq := &unstructured.Unstructured{}
	lq.SetGroupVersionKind(schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "LocalQueue"})
	req.Eventually(t, tgt, func() (bool, string) {
		err := k.Get(context.Background(), ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: "team-a-serving"}, lq)
		if err != nil {
			return false, fmt.Sprintf("get LocalQueue team-a-serving: %v", err)
		}
		cq, _, _ := unstructured.NestedString(lq.Object, "spec", "clusterQueue")
		return cq != "", "clusterQueue=" + cq
	})
}
