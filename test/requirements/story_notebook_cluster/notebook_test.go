// Story: Maya's first cluster (requirements 6, 3, 18).
//
// Maya (dev-a, project team-a) self-serves a private cluster: it converges,
// no one outside her project can see or touch it, its Jobs API sits behind
// the authenticated gateway (anonymous 401, another project 403), she can
// suspend and resume it, and when she deletes it everything goes away. The
// whole session — allows and denials alike — is in the audit trail and the
// hash chain replays clean.
package story_notebook_cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bifrost-compute/bifrost/test/requirements/fixture"
	"github.com/bifrost-compute/bifrost/test/requirements/req"
	"github.com/bifrost-compute/bifrost/test/requirements/target"
)

// auditRow is the slice of an audit event this story asserts on.
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

// hasAuditRow reports whether a row matches; empty fields are wildcards.
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

func verifyAuditChain(t *testing.T, tgt req.Target) {
	t.Helper()
	ver, err := tgt.As("admin").API().VerifyAuditTrailWithResponse(context.Background(), nil)
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

func TestMayasFirstCluster(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "Maya creates a private cluster, it converges, she suspends/resumes/deletes it, and another project can neither see nor touch it")
	req.Covers(t, 3, "her cluster's Jobs API is reachable only through the authenticated gateway: anonymous 401, another project 403")
	req.Covers(t, 18, "create, the cross-project denial, suspend, resume and delete each leave an audit row and the hash chain verifies")
	req.NeedsCapability(t, tgt, "gateway")
	ctx := context.Background()
	id := req.Name("maya")
	devA := fixture.Subject(t, tgt, "dev-a")
	devB := fixture.Subject(t, tgt, "dev-b")

	// Create and converge.
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	// Tenant isolation: dev-b cannot fetch it and never sees it listed.
	if st, _ := fixture.Get(t, tgt, "dev-b", id); !fixture.Denied(st) {
		t.Fatalf("dev-b get dev-a's cluster = %d, want 403 or 404", st)
	}
	list, err := tgt.As("dev-b").API().ListClustersWithResponse(ctx)
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("dev-b list: err=%v status=%v", err, list.StatusCode())
	}
	for _, cid := range fixture.IDs(list.Body) {
		if cid == id {
			t.Fatalf("dev-b's list shows dev-a's cluster %s", id)
		}
	}

	// The cluster's Jobs API is behind the gateway (r03's pattern: on
	// inproc only the pre-routing denials are assertable).
	var host string
	req.Eventually(t, tgt, func() (bool, string) {
		_, v := fixture.Get(t, tgt, "dev-a", id)
		host = fixture.GatewayHost(v)
		return host != "", "gateway_url not set"
	})
	if st, _ := fixture.GatewayRequest(t, tgt, "anon", host, "/api/jobs/"); st != http.StatusUnauthorized {
		t.Fatalf("anonymous via gateway = %d, want 401", st)
	}
	if st, _ := fixture.GatewayRequest(t, tgt, "dev-b", host, "/api/jobs/"); st != http.StatusForbidden {
		t.Fatalf("other project's developer via gateway = %d, want 403", st)
	}

	// Suspend and resume, by the owner.
	sus, err := tgt.As("dev-a").API().SuspendClusterWithResponse(ctx, id)
	if err != nil || sus.StatusCode() != http.StatusAccepted {
		t.Fatalf("suspend: err=%v status=%v body=%s", err, sus.StatusCode(), sus.Body)
	}
	fixture.WaitObserved(t, tgt, "dev-a", id, "suspended")
	res, err := tgt.As("dev-a").API().ResumeClusterWithResponse(ctx, id)
	if err != nil || res.StatusCode() != http.StatusAccepted {
		t.Fatalf("resume: err=%v status=%v body=%s", err, res.StatusCode(), res.Body)
	}
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	// dev-b can neither suspend nor delete it, and the attempts change
	// nothing in the owner's view.
	if s, err := tgt.As("dev-b").API().SuspendClusterWithResponse(ctx, id); err != nil || !fixture.Denied(s.StatusCode()) {
		t.Fatalf("dev-b suspend dev-a's cluster: err=%v status=%v, want 403 or 404", err, s.StatusCode())
	}
	if st := fixture.Delete(t, tgt, "dev-b", id); !fixture.Denied(st) {
		t.Fatalf("dev-b delete dev-a's cluster = %d, want 403 or 404", st)
	}
	st, view := fixture.Get(t, tgt, "dev-a", id)
	if st != http.StatusOK {
		t.Fatalf("owner get after denied ops = %d", st)
	}
	if d, _ := fixture.State(view); d != "running" {
		t.Fatalf("desired after denied ops = %q, want running (a denied request must not change state)", d)
	}

	// Delete by the owner; the cluster goes away.
	if st := fixture.Delete(t, tgt, "dev-a", id); st/100 != 2 {
		t.Fatalf("delete own cluster = %d", st)
	}
	fixture.WaitGone(t, tgt, "dev-a", id)

	// The audit trail tells the whole story.
	rows := auditRows(t, tgt)
	for _, action := range []string{"create_cluster", "suspend_cluster", "resume_cluster", "delete_cluster"} {
		if !hasAuditRow(rows, "allow", action, id, devA) {
			t.Errorf("no allow row for %s on %s by %s", action, id, devA)
		}
	}
	if !hasAuditRow(rows, "deny", "", "", devB) {
		t.Errorf("no deny row naming %s for the cross-project probes", devB)
	}
	verifyAuditChain(t, tgt)
}
