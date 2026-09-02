// Requirement 8 — automatic cleanup even after gateway failure; ownership
// recorded; state recovered on restart. The grace run of 2026-09-02 killed
// the control plane mid-flight and deleted a RayCluster behind its back;
// both recovered. These tests pin that.
package r08_cleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

const (
	clusterIDLabel = "bifrost.dev/cluster-id"
	ownerLabel     = "bifrost.dev/owner"
)

func TestOwnershipIsRecordedOnKubernetesObjects(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 8, "the RayCluster and its pods carry the cluster id and owner, so cleanup can find them without the store")
	req.NeedK8s(t, tgt)
	ctx := context.Background()
	k, _ := tgt.K8s()
	id := req.Name("own")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	var rc rayv1.RayCluster
	if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &rc); err != nil {
		t.Fatal(err)
	}
	if rc.Labels[clusterIDLabel] != id || rc.Labels[ownerLabel] == "" {
		t.Errorf("RayCluster labels = %v, want %s=%s and a non-empty %s", rc.Labels, clusterIDLabel, id, ownerLabel)
	}
	var pods corev1.PodList
	if err := k.List(ctx, &pods, ctrlclient.InNamespace(tgt.Namespace()), ctrlclient.MatchingLabels{"ray.io/cluster": id}); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("no ray pods for a running cluster")
	}
	for _, p := range pods.Items {
		if p.Labels[clusterIDLabel] != id {
			t.Errorf("pod %s lacks %s=%s (the default-deny policy selects on it): %v", p.Name, clusterIDLabel, id, p.Labels)
		}
	}
}

func TestRecordSurvivesControlPlaneRestart(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 8, "a running cluster, its tokens and its audit chain survive a control-plane crash; observed state is rebuilt from the cluster")
	req.NeedsCapability(t, tgt, "restart")
	r, ok := tgt.(req.Restarter)
	if !ok {
		t.Fatalf("target %s declares capability restart but is not a req.Restarter", tgt.Name())
	}
	ctx := context.Background()
	id := req.Name("rs")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	if err := r.RestartControlPlane(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// The bearer issued before the restart is store-backed and still valid.
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := fixture.Get(t, tgt, "dev-a", id)
		if st != http.StatusOK {
			return false, fmt.Sprintf("get=%d", st)
		}
		d, o := fixture.State(v)
		return d == "running" && o == "running", fmt.Sprintf("desired=%s observed=%s", d, o)
	})
	ver, err := tgt.As("admin").API().VerifyAuditTrailWithResponse(ctx, nil)
	if err != nil || ver.StatusCode() != http.StatusOK {
		t.Fatalf("audit verify: err=%v status=%v", err, ver.StatusCode())
	}
	var res struct {
		Ok bool `json:"ok"`
	}
	_ = json.Unmarshal(ver.Body, &res)
	if !res.Ok {
		t.Errorf("audit chain does not verify after restart: %s", ver.Body)
	}
}

func TestDeleteAcceptedThenRestartStillReaps(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 8, "a delete accepted just before a crash is still carried out after the restart")
	req.NeedsCapability(t, tgt, "restart")
	req.NeedK8s(t, tgt)
	r, ok := tgt.(req.Restarter)
	if !ok {
		t.Fatalf("target %s declares capability restart but is not a req.Restarter", tgt.Name())
	}
	k, _ := tgt.K8s()
	ctx := context.Background()
	id := req.Name("dr")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	if st := fixture.Delete(t, tgt, "dev-a", id); st/100 != 2 {
		t.Fatalf("delete = %d", st)
	}
	if err := r.RestartControlPlane(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}
	fixture.WaitGone(t, tgt, "dev-a", id)
	req.Eventually(t, tgt, func() (bool, string) {
		var rc rayv1.RayCluster
		err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &rc)
		return apierrors.IsNotFound(err), "RayCluster still present"
	})
}

func TestOutOfBandCRDeletionIsReprovisioned(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 8, "the store is the source of truth: a RayCluster deleted behind the control plane's back is re-provisioned")
	req.NeedK8s(t, tgt)
	ctx := context.Background()
	k, _ := tgt.K8s()
	id := req.Name("oob")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	var rc rayv1.RayCluster
	if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &rc); err != nil {
		t.Fatal(err)
	}
	old := rc.UID
	if err := k.Delete(ctx, &rc); err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}
	req.Eventually(t, tgt, func() (bool, string) {
		var cur rayv1.RayCluster
		if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &cur); err != nil {
			return false, "RayCluster absent"
		}
		if cur.UID == old || cur.UID == types.UID("") {
			return false, "same RayCluster object (deletion pending)"
		}
		_, o := fixture.State(second(fixture.Get(t, tgt, "dev-a", id)))
		return o == "running", "re-created, observed=" + o
	})
}

func TestTTLReaperTerminates(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 8, "a cluster past its max age is reaped without anyone asking")
	ctx := context.Background()
	ttl := fixture.TTL(tgt)
	id := req.Name("ttl")
	if st, body := fixture.Create(t, tgt, "admin", id, "team-a", &ttl); st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}
	fixture.WaitGone(t, tgt, "admin", id)
	if k, ok := tgt.K8s(); ok {
		req.Eventually(t, tgt, func() (bool, string) {
			var rc rayv1.RayCluster
			err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &rc)
			return apierrors.IsNotFound(err), "RayCluster still present after reap"
		})
	}
}

func second(_ int, v map[string]any) map[string]any { return v }
