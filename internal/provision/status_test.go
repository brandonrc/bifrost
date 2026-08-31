package provision

import (
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// TestZeroStatusRayCluster is the ledgered requirement (task-6-brief.md,
// review item 1): a RayCluster with a populated .Status must come back
// with the Go zero value after ZeroStatus, so the SSA apply body
// (json.Marshal(obj), see status.go's doc comment) never carries live
// operator status.
func TestZeroStatusRayCluster(t *testing.T) {
	rc := &rayv1.RayCluster{
		Status: rayv1.RayClusterStatus{
			State:               "ready", //nolint:staticcheck // SA1019: populating the deprecated field on purpose to prove ZeroStatus clears it
			ObservedGeneration:  3,
			ReadyWorkerReplicas: 2,
		},
	}
	ZeroStatus(rc)
	if !reflect.DeepEqual(rc.Status, rayv1.RayClusterStatus{}) {
		t.Fatalf("Status = %+v, want the zero value", rc.Status)
	}
}

// TestZeroStatusRayService is the ledgered requirement's RayService case.
func TestZeroStatusRayService(t *testing.T) {
	rs := &rayv1.RayService{
		Status: rayv1.RayServiceStatuses{
			ServiceStatus: "Running", //nolint:staticcheck // SA1019: populating the deprecated field on purpose to prove ZeroStatus clears it
		},
	}
	ZeroStatus(rs)
	if !reflect.DeepEqual(rs.Status, rayv1.RayServiceStatuses{}) {
		t.Fatalf("Status = %+v, want the zero value", rs.Status)
	}
}

// TestZeroStatusCohort is the ledgered requirement's Cohort case.
func TestZeroStatusCohort(t *testing.T) {
	cohort := &kueuev1beta2.Cohort{
		Status: kueuev1beta2.CohortStatus{
			FairSharing: &kueuev1beta2.FairSharingStatus{WeightedShare: 42},
		},
	}
	ZeroStatus(cohort)
	if !reflect.DeepEqual(cohort.Status, kueuev1beta2.CohortStatus{}) {
		t.Fatalf("Status = %+v, want the zero value", cohort.Status)
	}
}

// Cheap follow-through beyond the ledgered three: ClusterQueue and
// LocalQueue are also live-client apply targets (PoolProvisioner.ApplyPool)
// with the identical non-pointer-.Status risk.
func TestZeroStatusClusterQueue(t *testing.T) {
	cq := &kueuev1beta2.ClusterQueue{
		Status: kueuev1beta2.ClusterQueueStatus{AdmittedWorkloads: 5, PendingWorkloads: 1},
	}
	ZeroStatus(cq)
	if !reflect.DeepEqual(cq.Status, kueuev1beta2.ClusterQueueStatus{}) {
		t.Fatalf("Status = %+v, want the zero value", cq.Status)
	}
}

func TestZeroStatusLocalQueue(t *testing.T) {
	lq := &kueuev1beta2.LocalQueue{
		Status: kueuev1beta2.LocalQueueStatus{AdmittedWorkloads: 5, PendingWorkloads: 1},
	}
	ZeroStatus(lq)
	if !reflect.DeepEqual(lq.Status, kueuev1beta2.LocalQueueStatus{}) {
		t.Fatalf("Status = %+v, want the zero value", lq.Status)
	}
}

// A type with no .Status field at all, or a type ZeroStatus doesn't
// recognize, is a documented no-op: it must not panic.
func TestZeroStatusNoOpForUnrecognizedType(t *testing.T) {
	policy := &networkingv1.NetworkPolicy{}
	ZeroStatus(policy) // must not panic
	flavor := &kueuev1beta2.ResourceFlavor{}
	ZeroStatus(flavor) // must not panic; ResourceFlavor has no Status field
}
