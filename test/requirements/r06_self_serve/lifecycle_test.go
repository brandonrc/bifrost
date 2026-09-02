// Requirement 6 — self-serve private clusters (dask-gateway UX).
//
// These tests are the Go form of the end-to-end exercise run by hand against
// grace on 2026-09-02: a project operator creates a cluster through the
// public API, it converges, Kubernetes shows exactly the objects the
// control plane promised (RayCluster, per-cluster NetworkPolicy scoped to
// the owner), another project cannot see or stop it, and delete removes
// everything. The same file runs on inproc (fake provisioner, seconds) and
// on kind/grace (real KubeRay, minutes); Kubernetes assertions are gated by
// tgt.K8s().
package r06_self_serve

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

const (
	clusterIDLabel = "bifrost.dev/cluster-id"
	ownerLabel     = "bifrost.dev/owner"
)

func TestCreateConvergesAndDeleteRemovesEverything(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "a project operator creates a private cluster, it converges to running, and delete removes the RayCluster, its pods and its NetworkPolicy")
	ctx := context.Background()
	id := req.Name("lc")

	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	st, view := fixture.Get(t, tgt, "dev-a", id)
	if st != http.StatusOK {
		t.Fatalf("get after create = %d", st)
	}
	if d, _ := fixture.State(view); d != "running" {
		t.Fatalf("desired after create = %q, want running", d)
	}
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	if k, ok := tgt.K8s(); ok {
		var rc rayv1.RayCluster
		if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &rc); err != nil {
			t.Fatalf("RayCluster %s: %v", id, err)
		}
		if rc.Labels[clusterIDLabel] != id {
			t.Errorf("RayCluster %s label = %q, want %q", clusterIDLabel, rc.Labels[clusterIDLabel], id)
		}
		owner := rc.Labels[ownerLabel]
		if owner == "" {
			t.Errorf("RayCluster carries no %s label: the per-owner NetworkPolicy has nothing to select", ownerLabel)
		}
		var np networkingv1.NetworkPolicy
		if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: "bifrost-cluster-" + id}, &np); err != nil {
			t.Fatalf("per-cluster NetworkPolicy: %v", err)
		}
		if !admitsOwner(&np, owner) {
			t.Errorf("NetworkPolicy bifrost-cluster-%s admits no pod labelled %s=%s: %+v", id, ownerLabel, owner, np.Spec.Ingress)
		}
	}

	if st := fixture.Delete(t, tgt, "dev-a", id); st/100 != 2 {
		t.Fatalf("delete own cluster = %d", st)
	}
	fixture.WaitGone(t, tgt, "dev-a", id)

	if k, ok := tgt.K8s(); ok {
		req.Eventually(t, tgt, func() (bool, string) {
			var rc rayv1.RayCluster
			if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &rc); !apierrors.IsNotFound(err) {
				return false, "RayCluster still present"
			}
			var pods corev1.PodList
			if err := k.List(ctx, &pods, ctrlclient.InNamespace(tgt.Namespace()), ctrlclient.MatchingLabels{"ray.io/cluster": id}); err != nil || len(pods.Items) > 0 {
				return false, fmt.Sprintf("%d ray pods remain", len(pods.Items))
			}
			var np networkingv1.NetworkPolicy
			if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: "bifrost-cluster-" + id}, &np); !apierrors.IsNotFound(err) {
				return false, "per-cluster NetworkPolicy still present"
			}
			return true, "all gone"
		})
	}
}

func admitsOwner(np *networkingv1.NetworkPolicy, owner string) bool {
	for _, rule := range np.Spec.Ingress {
		for _, from := range rule.From {
			if from.PodSelector != nil && from.PodSelector.MatchLabels[ownerLabel] == owner {
				return true
			}
		}
	}
	return false
}

func TestListShowsOnlyOwnProjectClusters(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "a user lists their own clusters and does not see, or fetch, another project's")
	ctx := context.Background()
	id := req.Name("vis")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")

	mine, err := tgt.As("dev-a").API().ListClustersWithResponse(ctx)
	if err != nil || mine.StatusCode() != http.StatusOK {
		t.Fatalf("dev-a list: err=%v status=%v", err, mine.StatusCode())
	}
	if !contains(fixture.IDs(mine.Body), id) {
		t.Errorf("dev-a's list omits their own cluster %s: %s", id, mine.Body)
	}
	theirs, err := tgt.As("dev-b").API().ListClustersWithResponse(ctx)
	if err != nil || theirs.StatusCode() != http.StatusOK {
		t.Fatalf("dev-b list: err=%v status=%v", err, theirs.StatusCode())
	}
	if contains(fixture.IDs(theirs.Body), id) {
		t.Errorf("dev-b's list shows dev-a's cluster %s: %s", id, theirs.Body)
	}
	if st, _ := fixture.Get(t, tgt, "dev-b", id); !fixture.Denied(st) {
		t.Errorf("dev-b get dev-a's cluster = %d, want 403 or 404", st)
	}
}

func TestStopAnothersClusterIsRefused(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "a user cannot stop or suspend a cluster in another project, and the attempt changes nothing")
	ctx := context.Background()
	id := req.Name("stop")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")

	if st := fixture.Delete(t, tgt, "dev-b", id); !fixture.Denied(st) {
		t.Errorf("dev-b delete dev-a's cluster = %d, want 403 or 404", st)
	}
	sus, err := tgt.As("dev-b").API().SuspendClusterWithResponse(ctx, id)
	if err != nil || !fixture.Denied(sus.StatusCode()) {
		t.Errorf("dev-b suspend dev-a's cluster: err=%v status=%v, want 403 or 404", err, sus.StatusCode())
	}
	st, view := fixture.Get(t, tgt, "dev-a", id)
	if st != http.StatusOK {
		t.Fatalf("owner get after denied ops = %d", st)
	}
	if d, _ := fixture.State(view); d != "running" {
		t.Errorf("desired after denied ops = %q, want running (a denied request must not change state)", d)
	}
}

func TestTwoClustersHaveDistinctHeadServices(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "private clusters share no head: each has its own head Service and head pod")
	req.NeedK8s(t, tgt)
	ctx := context.Background()
	k, _ := tgt.K8s()
	ids := []string{req.Name("h1"), req.Name("h2")}
	for _, id := range ids {
		fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	}
	for _, id := range ids {
		fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		var svc corev1.Service
		name := id + "-head-svc"
		if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: name}, &svc); err != nil {
			t.Fatalf("head service %s: %v", name, err)
		}
		if seen[svc.Name] {
			t.Fatalf("two clusters resolved to the same head service %s", svc.Name)
		}
		seen[svc.Name] = true
		var heads corev1.PodList
		if err := k.List(ctx, &heads, ctrlclient.InNamespace(tgt.Namespace()),
			ctrlclient.MatchingLabels{"ray.io/cluster": id, "ray.io/node-type": "head"}); err != nil {
			t.Fatal(err)
		}
		if len(heads.Items) != 1 {
			t.Errorf("cluster %s has %d head pods, want exactly 1", id, len(heads.Items))
		}
	}
}

func TestSuspendResumeByGlobalOperator(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "suspend releases compute and resume brings the cluster back, for a caller with global cluster write")
	ctx := context.Background()
	id := req.Name("sr")
	fixture.MustCreate(t, tgt, "admin", id, "team-a")
	fixture.WaitObserved(t, tgt, "admin", id, "running")

	sus, err := tgt.As("admin").API().SuspendClusterWithResponse(ctx, id)
	if err != nil || sus.StatusCode() != http.StatusAccepted {
		t.Fatalf("suspend: err=%v status=%v body=%s", err, sus.StatusCode(), sus.Body)
	}
	fixture.WaitObserved(t, tgt, "admin", id, "suspended")
	if k, ok := tgt.K8s(); ok {
		req.Eventually(t, tgt, func() (bool, string) {
			var pods corev1.PodList
			if err := k.List(ctx, &pods, ctrlclient.InNamespace(tgt.Namespace()), ctrlclient.MatchingLabels{"ray.io/cluster": id}); err != nil {
				return false, err.Error()
			}
			return len(pods.Items) == 0, fmt.Sprintf("%d ray pods still running while suspended", len(pods.Items))
		})
	}
	res, err := tgt.As("admin").API().ResumeClusterWithResponse(ctx, id)
	if err != nil || res.StatusCode() != http.StatusAccepted {
		t.Fatalf("resume: err=%v status=%v body=%s", err, res.StatusCode(), res.Body)
	}
	fixture.WaitObserved(t, tgt, "admin", id, "running")
}

// Found on grace 2026-09-02 and fixed the same day: the project operator who
// may create and delete their cluster was refused suspend/resume, because
// lifecycleCommand demanded GLOBAL cluster write (ported verbatim from
// clusters.rs) where create and delete were project-scoped. bifrost-jupyter
// shows those buttons to exactly this user.
func TestSuspendResumeByProjectOperator(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "the project operator who owns a cluster can suspend and resume it, like create and delete")
	ctx := context.Background()
	id := req.Name("psr")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	sus, err := tgt.As("dev-a").API().SuspendClusterWithResponse(ctx, id)
	if err != nil || sus.StatusCode() != http.StatusAccepted {
		t.Fatalf("project operator suspend own cluster: err=%v status=%v body=%s", err, sus.StatusCode(), sus.Body)
	}
	fixture.WaitObserved(t, tgt, "dev-a", id, "suspended")
	res, err := tgt.As("dev-a").API().ResumeClusterWithResponse(ctx, id)
	if err != nil || res.StatusCode() != http.StatusAccepted {
		t.Fatalf("project operator resume own cluster: err=%v status=%v body=%s", err, res.StatusCode(), res.Body)
	}
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
