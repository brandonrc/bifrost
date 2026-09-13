// Story: the nightly batch-scoring job (requirements 5, 3, 14).
//
// Team-a's nightly scoring run is an ephemeral RayJob: submit, watch it
// come up RUNNING with its own cluster behind the gateway (anonymous 401,
// another project 403), see it through to SUCCEEDED — and the cluster it
// ran on never shows up among the managed clusters. A broken entrypoint is
// FAILED with a message. The job lands in the persistent history under its
// submitter, the usage report accrues team-a's cpu-hours, and the audit
// trail records the submit, the gateway denial and the delete.
package story_batch_scoring_job

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bifrost-compute/bifrost/test/requirements/fixture"
	"github.com/bifrost-compute/bifrost/test/requirements/req"
	"github.com/bifrost-compute/bifrost/test/requirements/target"
)

const (
	scoringEntrypoint = `python -c "print('nightly scoring')"`
	failingEntrypoint = `python -c "import sys; sys.exit(1)"`
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

func hasAuditRow(rows []auditRow, decision, action, cluster, subject string) bool {
	for _, r := range rows {
		if decision != "" && r.Decision != decision {
			continue
		}
		if action != "" && (r.Action == nil || *r.Action != action) {
			continue
		}
		if cluster != "" && (r.Cluster == nil || *r.Cluster != cluster) {
			continue
		}
		if subject != "" && (r.Subject == nil || *r.Subject != subject) {
			continue
		}
		return true
	}
	return false
}

func TestNightlyBatchScoringJob(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 5, "the nightly job runs on an ephemeral RayJob: RUNNING with its own cluster, SUCCEEDED at the end, and the cluster never appears among the managed clusters")
	req.Covers(t, 3, "while it runs its Jobs API is behind the authenticated gateway: anonymous 401, another project 403")
	req.Covers(t, 14, "the job is recorded under its submitter with a duration, and its run accrues team-a cpu-hours in the usage report")
	req.NeedsCapability(t, tgt, "gateway")
	ctx := context.Background()
	id := req.Name("score")
	submitter := fixture.Subject(t, tgt, "dev-a")
	devB := fixture.Subject(t, tgt, "dev-b")

	fixture.MustSubmitJob(t, tgt, "dev-a", fixture.SubmitJobBody(id, "team-a", scoringEntrypoint, nil))
	fixture.WaitJob(t, tgt, "dev-a", id, "RUNNING")

	// While it runs, the view names its cluster and its gateway address.
	var host, cluster string
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := fixture.GetJob(t, tgt, "dev-a", id)
		if st != http.StatusOK {
			return false, "get=" + http.StatusText(st)
		}
		cluster, _ = v["cluster"].(string)
		host = fixture.GatewayHost(v)
		return cluster != "" && host != "", "cluster=" + cluster
	})
	if st, _ := fixture.GatewayRequest(t, tgt, "anon", host, "/api/jobs/"); st != http.StatusUnauthorized {
		t.Fatalf("anonymous via gateway = %d, want 401", st)
	}
	if st, _ := fixture.GatewayRequest(t, tgt, "dev-b", host, "/api/jobs/"); st != http.StatusForbidden {
		t.Fatalf("other project's developer via gateway = %d, want 403", st)
	}

	fixture.WaitJob(t, tgt, "dev-a", id, "SUCCEEDED")

	// The job's cluster is ephemeral: nothing about it is in GET /clusters.
	resp, err := tgt.As("admin").API().ListClustersWithResponse(ctx)
	if err != nil || resp.StatusCode() != http.StatusOK {
		t.Fatalf("list clusters: err=%v status=%v", err, resp.StatusCode())
	}
	for _, cid := range fixture.IDs(resp.Body) {
		if cid == id || cid == cluster {
			t.Fatalf("job cluster %s leaked into the managed cluster list", cid)
		}
	}

	// A broken entrypoint is FAILED and says why.
	fid := req.Name("score-fail")
	fixture.MustSubmitJob(t, tgt, "dev-a", fixture.SubmitJobBody(fid, "team-a", failingEntrypoint, nil))
	fview := fixture.WaitJob(t, tgt, "dev-a", fid, "FAILED")
	if msg, _ := fview["message"].(string); msg == "" {
		t.Errorf("failed job carries no message: %v", fview)
	}

	// History: the job is recorded under its submitter, with a duration.
	type record struct {
		Id           string `json:"id"`
		Submitter    string `json:"submitter"`
		Status       string `json:"status"`
		DurationSecs *int64 `json:"duration_secs"`
	}
	req.Eventually(t, tgt, func() (bool, string) {
		r, err := tgt.As("dev-a").API().ListJobsWithResponse(ctx)
		if err != nil || r.StatusCode() != http.StatusOK {
			return false, "list jobs not 200"
		}
		var recs []record
		_ = json.Unmarshal(r.Body, &recs)
		for _, rec := range recs {
			if rec.Id != id {
				continue
			}
			if rec.Submitter != submitter || rec.Status != "SUCCEEDED" || rec.DurationSecs == nil {
				t.Fatalf("history record = %+v, want submitter %q, SUCCEEDED and a duration", rec, submitter)
			}
			return true, "recorded"
		}
		return false, "job not in history yet"
	})

	// Usage: the run accrues team-a cpu-hours.
	req.Eventually(t, tgt, func() (bool, string) {
		r, err := tgt.As("admin").API().UsageReportWithResponse(ctx, nil)
		if err != nil || r.StatusCode() != http.StatusOK {
			return false, "usage report not 200"
		}
		var rep struct {
			Groups []struct {
				Project       string             `json:"project"`
				ResourceHours map[string]float64 `json:"resource_hours"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(r.Body, &rep); err != nil {
			return false, "unmarshal: " + err.Error()
		}
		for _, g := range rep.Groups {
			if g.Project == "team-a" && g.ResourceHours["cpu"] > 0 {
				return true, "team-a cpu-hours > 0"
			}
		}
		return false, "no team-a group with cpu-hours yet: " + string(r.Body)
	})

	// Delete the record, and the audit trail shows the whole session.
	if d, err := tgt.As("dev-a").API().DeleteJobWithResponse(ctx, id, nil); err != nil || d.StatusCode() != http.StatusAccepted {
		t.Fatalf("delete job as its submitter: err=%v status=%v, want 202", err, d.StatusCode())
	}
	rows := auditRows(t, tgt)
	if !hasAuditRow(rows, "allow", "submit_job", id, submitter) {
		t.Errorf("no allow row for submit_job %s by %s", id, submitter)
	}
	if !hasAuditRow(rows, "deny", "", "", devB) {
		t.Errorf("no deny row naming %s for the gateway probe", devB)
	}
	if !hasAuditRow(rows, "allow", "delete_job", id, submitter) {
		t.Errorf("no allow row for delete_job %s by %s", id, submitter)
	}
}
