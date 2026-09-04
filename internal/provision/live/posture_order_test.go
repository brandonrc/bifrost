package live

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// recordingClient is the smallest client the posture path needs: List
// (no policies yet), Get (the namespace, already restricted so no PSS
// patch follows) and Patch, which records every applied object in order.
type recordingClient struct {
	client.Client
	applied []client.Object
}

func (r *recordingClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return nil
}

func (r *recordingClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if ns, ok := obj.(*corev1.Namespace); ok {
		ns.Name = key.Name
		ns.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}
		return nil
	}
	return nil
}

func (r *recordingClient) Patch(_ context.Context, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
	r.applied = append(r.applied, obj)
	return nil
}

func policyNames(objs []client.Object) []string {
	var names []string
	for _, o := range objs {
		if np, ok := o.(*networkingv1.NetworkPolicy); ok {
			names = append(names, np.Name)
		}
	}
	return names
}

// The namespace posture must land before a job's or a service's own
// resources: without it the per-cluster allow is the only policy selecting
// the head and KubeRay's operator is dropped (defect 2026-09-04).
func TestJobAndServiceAppliesEnsureNamespacePostureFirst(t *testing.T) {
	spec := &core.RayJobSpec{Project: "team-a", Entrypoint: "python -c 1", Image: "rayproject/ray:2.56.0",
		HeadCpu: "1", HeadMemory: "2Gi"}
	rec := &recordingClient{}
	c := &Client{c: rec, namespace: "tenants"}
	if err := NewJobClient(c).ApplyJob(context.Background(), core.ClusterId("j1"), spec, 1, nil); err != nil {
		t.Fatalf("ApplyJob: %v", err)
	}
	want := []string{provision.DefaultDenyPolicyName, provision.TenantAllowPolicyName, provision.ClusterAllowPolicyName("j1")}
	if got := policyNames(rec.applied); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("ApplyJob applied policies %v, want %v before the RayJob", got, want)
	}
	if len(rec.applied) != 4 {
		t.Fatalf("ApplyJob applied %d objects, want 3 policies + 1 RayJob", len(rec.applied))
	}

	svc := &core.ServiceSpec{Project: "team-a", Image: "rayproject/ray:2.56.0", RayVersion: "2.56.0",
		ServeConfigV2: "applications: []\n", HeadCpu: "1", HeadMemory: "2Gi", WorkerReplicas: 0, WorkerCpu: "1", WorkerMemory: "2Gi", Upgrade: core.UpgradeStrategyInPlace}
	rec = &recordingClient{}
	c = &Client{c: rec, namespace: "tenants"}
	if err := NewServiceClient(c).Deploy(context.Background(), "s1", svc, 1, nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	want = []string{provision.DefaultDenyPolicyName, provision.TenantAllowPolicyName, provision.ClusterAllowPolicyName("s1")}
	if got := policyNames(rec.applied); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("Deploy applied policies %v, want %v before the RayService", got, want)
	}
}
