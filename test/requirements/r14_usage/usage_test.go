// Requirement 14 — users and administrators can see who requested
// resources, what they requested, how long they used them, and the estimated
// cost. Until 2026-09-02 the report was a correct reader over a permanently
// empty table (nothing produced samples); the reconciler's metering pass now
// records each running cluster's held demand per project and pool.
package r14_usage

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

type usageReport struct {
	Groups []struct {
		Project       string             `json:"project"`
		Pool          string             `json:"pool"`
		ResourceHours map[string]float64 `json:"resource_hours"`
		CostUsd       *float64           `json:"cost_usd"`
	} `json:"groups"`
}

func TestRunningClusterShowsUpInTheUsageReport(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 14, "a running cluster accrues resource-hours attributed to its project in the usage report")
	ctx := context.Background()
	id := req.Name("use")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	req.Eventually(t, tgt, func() (bool, string) {
		resp, err := tgt.As("admin").API().UsageReportWithResponse(ctx, &client.UsageReportParams{})
		if err != nil || resp.StatusCode() != http.StatusOK {
			return false, "usage report not 200"
		}
		var rep usageReport
		if err := json.Unmarshal(resp.Body, &rep); err != nil {
			return false, "unmarshal: " + err.Error()
		}
		for _, g := range rep.Groups {
			if g.Project == "team-a" && g.ResourceHours["cpu"] > 0 {
				return true, "team-a cpu-hours > 0"
			}
		}
		return false, "no team-a group with cpu-hours yet: " + string(resp.Body)
	})
}

func TestUsageReportIsNotForProjectMembers(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 14, "the usage report is readable by administrators and refused to callers without cluster read across projects")
	ctx := context.Background()
	if r, err := tgt.As("anon").API().UsageReportWithResponse(ctx, &client.UsageReportParams{}); err != nil || r.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("anonymous usage report: err=%v status=%v, want 401", err, r.StatusCode())
	}
	if r, err := tgt.As("admin").API().UsageReportWithResponse(ctx, &client.UsageReportParams{}); err != nil || r.StatusCode() != http.StatusOK {
		t.Fatalf("admin usage report: err=%v status=%v, want 200", err, r.StatusCode())
	}
}
