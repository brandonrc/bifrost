package r06_self_serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bifrost-compute/bifrost/pkg/client"
	"github.com/bifrost-compute/bifrost/test/requirements/fixture"
	"github.com/bifrost-compute/bifrost/test/requirements/req"
	"github.com/bifrost-compute/bifrost/test/requirements/target"
)

// TestAutoscalerReachesTheAPIServerAndAddsAWorker is the test that did not
// exist on 2026-09-08, when the grace sim lane turned --ray-autoscaling on for
// the first time anywhere and every autoscaled cluster's sidecar died on a
// connect timeout to the API server. Bifrost's tenant posture let a head
// reach kube-dns and its own pods; the autoscaler needs to read and patch its
// RayCluster, and could not.
//
// The proof is the sidecar acting: a worker group with min_replicas 1 is
// created, and under autoscaling Bifrost never writes replicas (ADR-0007) —
// the RayCluster starts with no workers, so the only thing that can bring
// the count to one is the autoscaler talking to the API server. If the egress policy Bifrost now
// writes is wrong, or missing, or the RBAC to build it is gone, this waits
// out the convergence budget on zero and fails with the sidecar's restarts
// in hand.
func TestAutoscalerReachesTheAPIServerAndAddsAWorker(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "with --ray-autoscaling the KubeRay autoscaler reaches the API server through Bifrost's tenant posture and brings a min-1 worker group from zero to one")
	req.NeedsCapability(t, tgt, "autoscaling")
	req.NeedK8s(t, tgt)
	k, _ := tgt.K8s()
	ctx := context.Background()

	id := req.Name("as")
	ttl := fixture.TTL(tgt)
	raw := fmt.Sprintf(`{"id":%q,"spec":{"name":%q,"project":"team-a","ray_version":"2.56.0","image":%q,
		"head_cpu":%q,"head_memory":%q,"ttl_seconds":%d,
		"worker_groups":[{"name":"w","cpu":"1","memory":"2Gi","gpu":null,"min_replicas":1,"max_replicas":2,"replicas":1}]}}`,
		id, id, fixture.RayImage(), fixture.HeadCPU(), fixture.HeadMemory(), ttl)
	var body client.CreateClusterJSONRequestBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	resp, err := tgt.As("dev-a").API().CreateClusterWithResponse(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode(), resp.Body)
	}
	t.Cleanup(func() { _ = fixture.Delete(t, tgt, "admin", id) })

	// The control plane's half: the manifest hands the worker count to the
	// autoscaler, and the policy that lets the autoscaler work exists beside
	// the cluster allow.
	var rc rayv1.RayCluster
	req.Eventually(t, tgt, func() (bool, string) {
		if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &rc); err != nil {
			return false, err.Error()
		}
		return true, "raycluster present"
	})
	if rc.Spec.EnableInTreeAutoscaling == nil || !*rc.Spec.EnableInTreeAutoscaling {
		t.Fatalf("RayCluster %s: enableInTreeAutoscaling = %v, want true on an autoscaling target", id, rc.Spec.EnableInTreeAutoscaling)
	}
	var np networkingv1.NetworkPolicy
	if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: "bifrost-cluster-" + id + "-autoscaler"}, &np); err != nil {
		t.Fatalf("autoscaler egress policy: %v (the head's sidecar has no route to the API server without it)", err)
	}
	if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) == 0 || np.Spec.Egress[0].To[0].IPBlock == nil {
		t.Fatalf("autoscaler policy egress = %+v, want ipBlocks naming the API server endpoints", np.Spec.Egress)
	}

	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	// The autoscaler's half: it read the RayCluster, saw min 1 against 0
	// workers, and asked for one. Nothing else in the system writes that
	// worker into being.
	workers := func() (int, string) {
		var pods corev1.PodList
		if err := k.List(ctx, &pods, ctrlclient.InNamespace(tgt.Namespace()), ctrlclient.MatchingLabels{clusterIDLabel: id, "ray.io/node-type": "worker"}); err != nil {
			return 0, err.Error()
		}
		return len(pods.Items), fmt.Sprintf("%d worker pod(s)", len(pods.Items))
	}
	req.Eventually(t, tgt, func() (bool, string) {
		n, state := workers()
		if n >= 1 {
			return true, state
		}
		return false, state + "; " + autoscalerState(ctx, k, tgt.Namespace(), id)
	})

	// And it did so without dying on the way: a sidecar that restarted is a
	// sidecar that could not reach the API server for a while.
	if state := autoscalerState(ctx, k, tgt.Namespace(), id); state != "autoscaler: 0 restart(s)" {
		t.Errorf("the worker arrived, but the sidecar struggled: %s", state)
	}
}

// autoscalerState reports the head pod's autoscaler container restarts and
// last termination — the two facts that name this failure when it recurs.
func autoscalerState(ctx context.Context, k ctrlclient.Client, ns, id string) string {
	var pods corev1.PodList
	if err := k.List(ctx, &pods, ctrlclient.InNamespace(ns), ctrlclient.MatchingLabels{clusterIDLabel: id, "ray.io/node-type": "head"}); err != nil || len(pods.Items) == 0 {
		return "head pod not found"
	}
	for _, cs := range pods.Items[0].Status.ContainerStatuses {
		if cs.Name != "autoscaler" {
			continue
		}
		s := fmt.Sprintf("autoscaler: %d restart(s)", cs.RestartCount)
		if cs.LastTerminationState.Terminated != nil {
			s += fmt.Sprintf(", last termination %s", cs.LastTerminationState.Terminated.Reason)
		}
		return s
	}
	return "head pod has no autoscaler container"
}
