package r15_health

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// The pending-reasons half of requirement 15: a cluster that cannot be
// scheduled explains why through the API, without the user touching
// Kubernetes. A head asking for more CPU than any node has stays pending
// with a FailedScheduling event that names the shortage.
func TestUnschedulableClusterSurfacesTheReason(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 15, "a pending cluster's scheduling failure is visible through the events operation without Kubernetes access")
	req.NeedK8s(t, tgt)
	ctx := context.Background()
	id := req.Name("pend")
	body := fixture.ClusterBody(id, "team-a", nil)
	body.Spec.HeadCpu = "1000" // no node has this
	resp, err := tgt.As("dev-a").API().CreateClusterWithResponse(ctx, body)
	if err != nil || resp.StatusCode() != http.StatusCreated {
		t.Fatalf("create: err=%v status=%v body=%s", err, resp.StatusCode(), resp.Body)
	}
	req.Eventually(t, tgt, func() (bool, string) {
		ev, err := tgt.As("dev-a").API().ClusterEventsWithResponse(ctx, id)
		if err != nil || ev.StatusCode() != http.StatusOK {
			return false, "events not 200"
		}
		s := string(ev.Body)
		if strings.Contains(s, "FailedScheduling") || strings.Contains(s, "Insufficient cpu") {
			return true, "reason surfaced"
		}
		return false, "no scheduling reason yet: " + truncate(s)
	})
	st, v := fixture.Get(t, tgt, "dev-a", id)
	if st != http.StatusOK {
		t.Fatalf("get = %d", st)
	}
	if _, o := fixture.State(v); o == "running" {
		t.Fatalf("a 1000-CPU head cannot be running; observed=%s", o)
	}
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
