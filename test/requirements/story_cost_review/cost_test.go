// Story: the Friday cost review (requirements 14, 18).
//
// Come Friday the administrator reads the week's usage: both teams' running
// clusters (and team-a's finished job) have accrued cpu-hours, each row is
// attributed to its requester, the owner filter narrows to one identity,
// prices turn hours into dollars, and a project member sees only their own
// rows. The review itself is audited — including the one call dev-a was
// not allowed to make — and the chain verifies.
package story_cost_review

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bifrost-compute/bifrost/pkg/client"
	"github.com/bifrost-compute/bifrost/test/requirements/fixture"
	"github.com/bifrost-compute/bifrost/test/requirements/req"
	"github.com/bifrost-compute/bifrost/test/requirements/target"
)

type usageReport struct {
	Groups []struct {
		Project       string             `json:"project"`
		Owner         string             `json:"owner"`
		ResourceHours map[string]float64 `json:"resource_hours"`
		CostUsd       *float64           `json:"cost_usd"`
	} `json:"groups"`
}

func fetchUsage(t *testing.T, tgt req.Target, principal string, params *client.UsageReportParams) (*usageReport, string, int) {
	t.Helper()
	resp, err := tgt.As(principal).API().UsageReportWithResponse(context.Background(), params)
	if err != nil {
		t.Fatalf("usage report as %s: %v", principal, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, string(resp.Body), resp.StatusCode()
	}
	var rep usageReport
	if err := json.Unmarshal(resp.Body, &rep); err != nil {
		t.Fatalf("usage report: unmarshal: %v", err)
	}
	return &rep, string(resp.Body), resp.StatusCode()
}

func TestFridayCostReview(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 14, "the week's usage report attributes cpu-hours and cost to each project and requester, filters by owner, and scopes a project member to their own rows")
	req.Covers(t, 18, "the review session — including a denied peek at the audit trail — is itself audited and the hash chain verifies")
	ctx := context.Background()
	admin := tgt.As("admin").API()
	subjectA := fixture.Subject(t, tgt, "dev-a")

	// The policy wire format carries a price sheet (UpdatePolicy.prices in
	// internal/api/openapi.json), so the review can talk dollars. Snapshot
	// and restore: the policy is platform state.
	before, err := admin.GetPolicyWithResponse(ctx)
	if err != nil || before.JSON200 == nil {
		t.Fatalf("get_policy: err=%v status=%v body=%s", err, before.StatusCode(), before.Body)
	}
	var priceBody client.UpdatePolicyJSONRequestBody
	_ = json.Unmarshal([]byte(`{"prices":{"cpu":0.05}}`), &priceBody)
	if r, err := admin.UpdatePolicyWithResponse(ctx, priceBody); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("update_policy prices: err=%v status=%v body=%s", err, r.StatusCode(), r.Body)
	}
	t.Cleanup(func() {
		restore := client.UpdatePolicyJSONRequestBody{}
		if before.JSON200.Prices != nil {
			restore.Prices = before.JSON200.Prices
		} else {
			var none map[string]float64 // explicit null clears the sheet
			restore.Prices = &none
		}
		_, _ = admin.UpdatePolicyWithResponse(context.Background(), restore)
	})

	// The week's activity: a running cluster per team and a finished
	// team-a job.
	clusterA := req.Name("cost-a")
	fixture.MustCreate(t, tgt, "dev-a", clusterA, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", clusterA, "running")
	clusterB := req.Name("cost-b")
	fixture.MustCreate(t, tgt, "dev-b", clusterB, "team-b")
	fixture.WaitObserved(t, tgt, "dev-b", clusterB, "running")
	job := req.Name("cost-j")
	fixture.MustSubmitJob(t, tgt, "dev-a", fixture.SubmitJobBody(job, "team-a", `python -c "print('scored')"`, nil))
	fixture.WaitJob(t, tgt, "dev-a", job, "SUCCEEDED")

	// The administrator's report: both teams accruing, team-a attributed
	// to its requester, and prices turning hours into dollars.
	req.Eventually(t, tgt, func() (bool, string) {
		rep, body, _ := fetchUsage(t, tgt, "admin", &client.UsageReportParams{})
		if rep == nil {
			return false, body
		}
		teamA, teamB, attributed, priced := false, false, false, false
		for _, g := range rep.Groups {
			if g.ResourceHours["cpu"] <= 0 {
				continue
			}
			if g.Project == "team-a" {
				teamA = true
				if g.Owner == subjectA {
					attributed = true
				}
			}
			if g.Project == "team-b" {
				teamB = true
			}
			if g.CostUsd != nil && *g.CostUsd > 0 {
				priced = true
			}
		}
		return teamA && teamB && attributed && priced, body
	})

	// The owner filter narrows to one identity; a stranger owns nothing.
	req.Eventually(t, tgt, func() (bool, string) {
		rep, body, _ := fetchUsage(t, tgt, "admin", &client.UsageReportParams{Owner: &subjectA})
		if rep == nil {
			return false, body
		}
		if len(rep.Groups) == 0 {
			return false, "no groups for owner " + subjectA + " yet: " + body
		}
		for _, g := range rep.Groups {
			if g.Owner != subjectA {
				return false, "owner filter leaked a group owned by " + g.Owner + ": " + body
			}
		}
		return true, "every group is owned by " + subjectA
	})
	nobody := "nobody"
	rep, body, _ := fetchUsage(t, tgt, "admin", &client.UsageReportParams{Owner: &nobody})
	if rep == nil {
		t.Fatal(body)
	}
	if len(rep.Groups) != 0 {
		t.Fatalf("owner=nobody returned groups: %s", body)
	}

	// Scoping: anonymous is 401; a project member sees only their own rows.
	if _, _, st := fetchUsage(t, tgt, "anon", &client.UsageReportParams{}); st != http.StatusUnauthorized {
		t.Fatalf("anonymous usage report = %d, want 401", st)
	}
	repA, bodyA, st := fetchUsage(t, tgt, "dev-a", &client.UsageReportParams{})
	if repA == nil {
		t.Fatalf("dev-a usage report = %d: %s", st, bodyA)
	}
	for _, g := range repA.Groups {
		if g.Project != "team-a" && g.Owner != subjectA && g.Owner != "dev-a" {
			t.Errorf("dev-a sees usage of project %q owner %q: another tenant's rows leaked", g.Project, g.Owner)
		}
	}

	// The review is audited too: dev-a's peek at the audit trail is a 403,
	// recorded under their name, and afterwards the chain replays clean.
	if r, err := tgt.As("dev-a").API().ListAuditEventsWithResponse(ctx, nil); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Fatalf("dev-a list_audit_events: err=%v status=%v, want 403", err, r.StatusCode())
	}
	audit, err := admin.ListAuditEventsWithResponse(ctx, nil)
	if err != nil || audit.StatusCode() != http.StatusOK {
		t.Fatalf("list_audit_events: err=%v status=%v", err, audit.StatusCode())
	}
	var wrapped struct {
		Items []struct {
			Subject  *string `json:"subject"`
			Decision string  `json:"decision"`
		} `json:"items"`
	}
	if err := json.Unmarshal(audit.Body, &wrapped); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range wrapped.Items {
		if e.Decision == "deny" && e.Subject != nil && *e.Subject == subjectA {
			found = true
		}
	}
	if !found {
		t.Errorf("no deny row naming %s for the refused audit read", subjectA)
	}

	ver, err := admin.VerifyAuditTrailWithResponse(ctx, nil)
	if err != nil || ver.StatusCode() != http.StatusOK {
		t.Fatalf("verify_audit_trail: err=%v status=%v", err, ver.StatusCode())
	}
	var res struct {
		Ok          bool   `json:"ok"`
		FirstBroken *int64 `json:"first_broken_seq"`
	}
	if err := json.Unmarshal(ver.Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Ok || res.FirstBroken != nil {
		t.Fatalf("audit chain broken: %s", ver.Body)
	}
}
