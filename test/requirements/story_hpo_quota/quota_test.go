// Story: the tuning sweep hits the quota wall (requirements 7, 14).
//
// An HPO sweep wants to fan out, but the administrator has capped team-a
// at exactly one canonical cluster: the first create converges, the second
// is 409 with nothing persisted, team-b's ledger is untouched, the denial
// is audited under dev-a's name, the running cluster accrues usage while
// it lives, and releasing the first cluster lets the next sweep member in.
package story_hpo_quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/bifrost-compute/bifrost/pkg/client"
	"github.com/bifrost-compute/bifrost/test/requirements/fixture"
	"github.com/bifrost-compute/bifrost/test/requirements/req"
	"github.com/bifrost-compute/bifrost/test/requirements/target"
)

func TestTuningSweepHitsTheQuotaWall(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 7, "the administrator's per-project CPU quota admits exactly one sweep cluster and refuses the next with 409, persisting nothing")
	req.Covers(t, 14, "while the admitted cluster runs it accrues cpu-hours attributed to the requester in the usage report")
	ctx := context.Background()
	admin := tgt.As("admin").API()
	subjectA := fixture.Subject(t, tgt, "dev-a")

	// Snapshot the quota map, cap team-a at exactly one canonical cluster
	// (head + one CPU per worker; memory generous so cpu binds), restore on
	// exit — the policy is platform state.
	before, err := admin.GetPolicyWithResponse(ctx)
	if err != nil || before.JSON200 == nil {
		t.Fatalf("get_policy: err=%v status=%v body=%s", err, before.StatusCode(), before.Body)
	}
	quotas := fmt.Sprintf(`{"quotas":{"team-a":{"cpu":%g,"memory":1024}}}`, fixture.HeadCPUValue()+float64(fixture.WorkerReplicas()))
	var setBody client.UpdatePolicyJSONRequestBody
	_ = json.Unmarshal([]byte(quotas), &setBody)
	if r, err := admin.UpdatePolicyWithResponse(ctx, setBody); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("update_policy %s: err=%v status=%v body=%s", quotas, err, r.StatusCode(), r.Body)
	}
	t.Cleanup(func() {
		restore := before.JSON200.Quotas
		_, _ = admin.UpdatePolicyWithResponse(context.Background(), client.UpdatePolicyJSONRequestBody{Quotas: &restore})
	})

	// Sweep member #1 gets in and converges.
	first := req.Name("hpo1")
	fixture.MustCreate(t, tgt, "dev-a", first, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", first, "running")

	// Sweep member #2 hits the wall: 409, and nothing was persisted.
	second := req.Name("hpo2")
	if st, body := fixture.Create(t, tgt, "dev-a", second, "team-a", nil); st != http.StatusConflict {
		t.Fatalf("second create under quota = %d %s, want 409", st, body)
	}
	if st, _ := fixture.Get(t, tgt, "admin", second); st != http.StatusNotFound {
		t.Fatalf("a refused create must persist nothing; get = %d", st)
	}

	// The wall is team-a's own: team-b's ledger is separate.
	fixture.MustCreate(t, tgt, "dev-b", req.Name("hpob"), "team-b")

	// The denial names dev-a in the audit trail.
	rows := func() []map[string]any {
		r, err := admin.ListAuditEventsWithResponse(ctx, nil)
		if err != nil || r.StatusCode() != http.StatusOK {
			t.Fatalf("list_audit_events: err=%v status=%v", err, r.StatusCode())
		}
		var wrapped struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(r.Body, &wrapped); err != nil {
			t.Fatalf("list_audit_events: unmarshal: %v", err)
		}
		return wrapped.Items
	}()
	foundDeny := false
	for _, e := range rows {
		if e["decision"] == "deny" && e["subject"] == subjectA && e["reason"] == "quota_exceeded" {
			foundDeny = true
		}
	}
	if !foundDeny {
		t.Errorf("no deny row naming %s with reason quota_exceeded", subjectA)
	}

	// While it runs, the admitted cluster accrues usage under its requester.
	req.Eventually(t, tgt, func() (bool, string) {
		r, err := admin.UsageReportWithResponse(ctx, &client.UsageReportParams{})
		if err != nil || r.StatusCode() != http.StatusOK {
			return false, "usage report not 200"
		}
		var rep struct {
			Groups []struct {
				Project       string             `json:"project"`
				Owner         string             `json:"owner"`
				ResourceHours map[string]float64 `json:"resource_hours"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(r.Body, &rep); err != nil {
			return false, "unmarshal: " + err.Error()
		}
		for _, g := range rep.Groups {
			if g.Project == "team-a" && g.Owner == subjectA && g.ResourceHours["cpu"] > 0 {
				return true, "team-a group owned by " + subjectA + " has cpu-hours > 0"
			}
		}
		return false, "no team-a group owned by " + subjectA + " yet: " + string(r.Body)
	})

	// Releasing member #1 frees the wall; member #2 gets in.
	if st := fixture.Delete(t, tgt, "dev-a", first); st/100 != 2 {
		t.Fatalf("delete = %d", st)
	}
	fixture.WaitGone(t, tgt, "dev-a", first)
	req.Eventually(t, tgt, func() (bool, string) {
		st, b := fixture.Create(t, tgt, "dev-a", second, "team-a", nil)
		return st == http.StatusCreated, fmt.Sprintf("create after release = %d %s", st, b)
	})
}
