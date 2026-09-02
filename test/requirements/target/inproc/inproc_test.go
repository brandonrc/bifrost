package inproc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target/inproc"
)

// These are inproc's own smoke tests: they bind to the in-process target
// explicitly, whatever REQ_TARGET says. Through target.Get they ran against
// the kind cluster on the first L3 lane and provisioned a real RayCluster.
func TestInprocCreateConvergesToRunning(t *testing.T) {
	tgt := inproc.New(t)
	ctx := context.Background()
	id := req.Name("smoke")

	body := client.CreateClusterJSONRequestBody{}
	if err := json.Unmarshal([]byte(`{"id":"`+id+`","spec":{"name":"`+id+`","project":"team-a","image":"rayproject/ray:2.56.0","ray_version":"2.56.0","head_cpu":"1","head_memory":"1Gi","worker_groups":[{"name":"w","cpu":"1","memory":"1Gi","replicas":1,"min_replicas":1,"max_replicas":1}]}}`), &body); err != nil {
		t.Fatal(err)
	}

	resp, err := tgt.As("admin").API().CreateClusterWithResponse(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode(), resp.Body)
	}

	req.Eventually(t, tgt, func() (bool, string) {
		g, err := tgt.As("admin").API().GetClusterWithResponse(ctx, id)
		if err != nil || g.StatusCode() != 200 {
			return false, "get failed"
		}
		var m map[string]any
		if err := json.Unmarshal(g.Body, &m); err != nil {
			return false, "unmarshal failed: " + err.Error()
		}
		st, _ := m["observed_state"].(string)
		return st == "running", "observed_state=" + st
	})
}

func TestInprocPrincipalsAreDistinct(t *testing.T) {
	tgt := inproc.New(t)
	ctx := context.Background()
	a, _ := tgt.As("dev-a").API().ListClustersWithResponse(ctx)
	if a.StatusCode() != 200 {
		t.Fatalf("dev-a list = %d %s", a.StatusCode(), a.Body)
	}
	anon, _ := tgt.As("anon").API().ListClustersWithResponse(ctx)
	if anon.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("anon list = %d, want 401", anon.StatusCode())
	}
}
