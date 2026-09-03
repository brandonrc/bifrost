package r07_admin_controls

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

// Quotas are the CPU/memory half of requirement 7: an administrator sets a
// per-project limit through the policy API and a create that would exceed
// it is refused with 409. The policy is platform state, so the test restores
// it on exit; a project name with the run id keeps other runs unaffected.
func TestProjectQuotaRefusesOverCommit(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 7, "an administrator's per-project CPU quota refuses a create that would exceed it with 409")
	ctx := context.Background()
	admin := tgt.As("admin").API()

	// One canonical cluster holds head 1 + worker 1 = 2 CPU of max demand;
	// a quota of 3 admits one and refuses the second.
	project := "team-a"
	set := func(quotas string) {
		var body client.UpdatePolicyJSONRequestBody
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"quotas":%s}`, quotas)), &body)
		r, err := admin.UpdatePolicyWithResponse(ctx, body)
		if err != nil || r.StatusCode()/100 != 2 {
			t.Fatalf("update_policy %s: err=%v status=%v body=%s", quotas, err, r.StatusCode(), r.Body)
		}
	}
	// A limit map must name every resource a spec requests: a resource absent
	// from the limit reads as zero and refuses everything. cpu admits exactly
	// one canonical cluster; memory is generous so cpu is the binding one.
	// Exactly one canonical cluster's cpu (head 1 + one per worker) fits; a
	// second of any size exceeds it. On head-only lanes (REQ_WORKER_REPLICAS=0)
	// that is a limit of 1, not 3 — the old "+1" let two head-only clusters in.
	set(fmt.Sprintf(`{%q:{"cpu":%d,"memory":1024}}`, project, 1+fixture.WorkerReplicas()))
	t.Cleanup(func() {
		var body client.UpdatePolicyJSONRequestBody
		_ = json.Unmarshal([]byte(`{"quotas":{}}`), &body)
		_, _ = admin.UpdatePolicyWithResponse(context.Background(), body)
	})

	first := req.Name("q1")
	fixture.MustCreate(t, tgt, "dev-a", first, project)
	st, body := fixture.Create(t, tgt, "dev-a", req.Name("q2"), project, nil)
	if st != http.StatusConflict {
		t.Fatalf("second create under quota = %d %s, want 409", st, body)
	}
	// Releasing the first frees the quota.
	if st := fixture.Delete(t, tgt, "dev-a", first); st/100 != 2 {
		t.Fatalf("delete = %d", st)
	}
	fixture.WaitGone(t, tgt, "dev-a", first)
	req.Eventually(t, tgt, func() (bool, string) {
		st, b := fixture.Create(t, tgt, "dev-a", req.Name("q3"), project, nil)
		return st == http.StatusCreated, fmt.Sprintf("create after release = %d %s", st, b)
	})
}
