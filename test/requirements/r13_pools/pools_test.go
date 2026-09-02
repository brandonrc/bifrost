// Requirement 13 — group capacity through shared resource pools with
// administrator-defined quotas and weights. The pool and allocation API is
// the administrator's side of it; Kueue admission (the fair-queueing half)
// is asserted on Kubernetes targets by the pool reconciler's ClusterQueue.
package r13_pools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestPoolAndAllocationLifecycle(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 13, "an administrator creates a pool with a weight and flavors, allocates a project into it, reads usage, and tears it down; non-admins are refused")
	ctx := context.Background()
	admin := tgt.As("admin").API()
	name := req.Name("pool")

	var create client.CreatePoolJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"spec":{"name":%q,"cohort":"req","fair_sharing_weight":2,"elastic":false,
		"flavors":[{"name":"default","resources":{"cpu":"16","memory":"64Gi"},"node_labels":{},"taints":[]}]}}`, name)), &create)
	if r, err := admin.CreatePoolWithResponse(ctx, create); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("create pool: err=%v status=%v body=%s", err, r.StatusCode(), r.Body)
	}
	t.Cleanup(func() {
		_, _ = admin.DeleteAllocationWithResponse(context.Background(), name, "team-a")
		_, _ = admin.DeletePoolWithResponse(context.Background(), name)
	})
	if r, err := tgt.As("dev-a").API().CreatePoolWithResponse(ctx, create); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Errorf("developer create pool: err=%v status=%v, want 403", err, r.StatusCode())
	}
	got, err := admin.GetPoolWithResponse(ctx, name)
	if err != nil || got.StatusCode() != http.StatusOK || !fixture.Contains(string(got.Body), `"fair_sharing_weight":2`) {
		t.Fatalf("get pool: err=%v status=%v body=%s", err, got.StatusCode(), got.Body)
	}
	list, err := tgt.As("dev-a").API().ListPoolsWithResponse(ctx)
	if err != nil || list.StatusCode() != http.StatusOK || !fixture.Contains(string(list.Body), name) {
		t.Fatalf("developer list pools: err=%v status=%v body=%s", err, list.StatusCode(), list.Body)
	}

	var alloc client.PutAllocationJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"namespace":%q,"nominal":{"cpu":"4","memory":"16Gi"},"borrowing_limit":{},"lending_limit":{}}`, tgt.Namespace())), &alloc)
	if r, err := admin.PutAllocationWithResponse(ctx, name, "team-a", alloc); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("put allocation: err=%v status=%v body=%s", err, r.StatusCode(), r.Body)
	}
	if r, err := tgt.As("dev-a").API().PutAllocationWithResponse(ctx, name, "team-a", alloc); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Errorf("developer put allocation: err=%v status=%v, want 403", err, r.StatusCode())
	}
	allocs, err := admin.ListAllocationsWithResponse(ctx, name)
	if err != nil || allocs.StatusCode() != http.StatusOK || !fixture.Contains(string(allocs.Body), `"team-a"`) {
		t.Fatalf("list allocations: err=%v status=%v body=%s", err, allocs.StatusCode(), allocs.Body)
	}
	usage, err := admin.PoolUsageWithResponse(ctx, name)
	if err != nil || usage.StatusCode() != http.StatusOK {
		t.Fatalf("pool usage: err=%v status=%v body=%s", err, usage.StatusCode(), usage.Body)
	}
	if r, err := admin.DeleteAllocationWithResponse(ctx, name, "team-a"); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("delete allocation: err=%v status=%v", err, r.StatusCode())
	}
	if r, err := admin.DeletePoolWithResponse(ctx, name); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("delete pool: err=%v status=%v", err, r.StatusCode())
	}
	req.Eventually(t, tgt, func() (bool, string) {
		g, err := admin.GetPoolWithResponse(ctx, name)
		return err == nil && g.StatusCode() == http.StatusNotFound, fmt.Sprintf("get after delete = %v", codeOf(g))
	})
}

func codeOf(r interface{ StatusCode() int }) any {
	if r == nil {
		return nil
	}
	return r.StatusCode()
}
