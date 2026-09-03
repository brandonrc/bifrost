package provision

import (
	"encoding/json"
	"math"
	"testing"

	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"github.com/brandonrc/bifrost/internal/core"
)

// Test parity: this file ports the predecessor's provision crate, src/kueue.rs's `tests`
// module (kueue.rs:189-364) onto the typed kueue.x-k8s.io/v1beta2 structs.

func testFlavor(name string, resources ...[2]string) core.FlavorSpec {
	m := map[string]string{}
	for _, kv := range resources {
		m[kv[0]] = kv[1]
	}
	return core.FlavorSpec{Name: name, Resources: m, NodeLabels: map[string]string{}, Taints: nil}
}

func testPool() *core.PoolSpec {
	return &core.PoolSpec{
		Name: "gpu-pool",
		Flavors: []core.FlavorSpec{
			testFlavor("a100", [2]string{"cpu", "64"}, [2]string{"memory", "256Gi"}, [2]string{"nvidia.com/gpu", "8"}),
			testFlavor("spot-cpu", [2]string{"cpu", "128"}, [2]string{"memory", "512Gi"}),
		},
		Cohort:            "research",
		FairSharingWeight: 2.0,
		Elastic:           true,
	}
}

func testAlloc() *core.AllocationSpec {
	return &core.AllocationSpec{
		Pool: "gpu-pool", Project: "proj-a", Namespace: "proj-a",
		Nominal:        map[string]string{"cpu": "16"},
		BorrowingLimit: map[string]string{"cpu": "64"},
		LendingLimit:   map[string]string{},
	}
}

// kueue.rs: resource_flavor_carries_labels_and_taints
func TestResourceFlavorCarriesLabelsAndTaints(t *testing.T) {
	f := testFlavor("a100", [2]string{"nvidia.com/gpu", "8"})
	f.NodeLabels["node.kubernetes.io/instance-type"] = "p4d.24xlarge"
	f.Taints = []core.TaintSpec{{Key: "nvidia.com/gpu", Value: "present", Effect: "NoSchedule"}}

	rf := ResourceFlavorFor("gpu-pool", &f)
	if rf.APIVersion != KueueAPIVersion || rf.Kind != "ResourceFlavor" {
		t.Fatalf("apiVersion/kind = %q/%q", rf.APIVersion, rf.Kind)
	}
	if rf.Name != "a100" {
		t.Fatalf("name = %q", rf.Name)
	}
	if rf.Labels[PoolLabel] != "gpu-pool" {
		t.Fatalf("pool label = %q", rf.Labels[PoolLabel])
	}
	if rf.Spec.NodeLabels["node.kubernetes.io/instance-type"] != "p4d.24xlarge" {
		t.Fatalf("nodeLabels = %#v", rf.Spec.NodeLabels)
	}
	if len(rf.Spec.NodeTaints) != 1 {
		t.Fatalf("nodeTaints len = %d", len(rf.Spec.NodeTaints))
	}
	nt := rf.Spec.NodeTaints[0]
	if nt.Key != "nvidia.com/gpu" || nt.Value != "present" || string(nt.Effect) != "NoSchedule" {
		t.Fatalf("taint = %#v", nt)
	}
}

// kueue.rs: cohort_is_named_and_empty
func TestCohortIsNamedAndEmpty(t *testing.T) {
	c := CohortFor(testPool())
	if c.APIVersion != KueueAPIVersion || c.Kind != "Cohort" {
		t.Fatalf("apiVersion/kind = %q/%q", c.APIVersion, c.Kind)
	}
	if c.Name != "research" {
		t.Fatalf("name = %q", c.Name)
	}
	if c.Labels[PoolLabel] != "gpu-pool" {
		t.Fatalf("pool label = %q", c.Labels[PoolLabel])
	}
	b, err := json.Marshal(c.Spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "{}" {
		t.Fatalf("spec must marshal empty, got %s", b)
	}
}

// Status-block guard (fix round 1, MEDIUM 2): CohortFor must never
// populate .status — it is server-owned. Unlike RayCluster/RayService
// (whose status structs carry resource.Quantity fields that round-trip
// with non-nil-but-zero internal state, see TestRayClusterForNeverPopulatesStatus's
// doc comment), CohortStatus is a single pointer field, so a plain
// zero-value comparison is exact here.
func TestCohortForNeverPopulatesStatus(t *testing.T) {
	c := CohortFor(testPool())
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded kueuev1beta2.Cohort
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Status.FairSharing != nil {
		t.Fatalf("status.fairSharing = %+v, want nil", decoded.Status.FairSharing)
	}
}

// kueue.rs: cluster_queue_propagates_cohort_and_weight
func TestClusterQueuePropagatesCohortAndWeight(t *testing.T) {
	cq, err := ClusterQueueFor(testPool())
	if err != nil {
		t.Fatalf("ClusterQueueFor: %v", err)
	}
	if cq.Kind != "ClusterQueue" {
		t.Fatalf("kind = %q", cq.Kind)
	}
	if cq.Name != "gpu-pool" {
		t.Fatalf("name = %q", cq.Name)
	}
	if cq.Labels[PoolLabel] != "gpu-pool" {
		t.Fatalf("pool label = %q", cq.Labels[PoolLabel])
	}
	if string(cq.Spec.CohortName) != "research" {
		t.Fatalf("cohortName = %q", cq.Spec.CohortName)
	}
	if cq.Spec.FairSharing == nil || cq.Spec.FairSharing.Weight == nil || cq.Spec.FairSharing.Weight.String() != "2" {
		t.Fatalf("fairSharing.weight = %v", cq.Spec.FairSharing)
	}
	// All namespaces eligible: Kueue's nil default admits NOTHING.
	if cq.Spec.NamespaceSelector == nil {
		t.Fatalf("namespaceSelector must be non-nil (all-namespaces), not nil (nothing)")
	}
	if len(cq.Spec.NamespaceSelector.MatchLabels) != 0 || len(cq.Spec.NamespaceSelector.MatchExpressions) != 0 {
		t.Fatalf("namespaceSelector must be empty (all namespaces), got %#v", cq.Spec.NamespaceSelector)
	}
}

// kueue.rs: covered_resources_is_sorted_union_across_flavors
func TestCoveredResourcesIsSortedUnionAcrossFlavors(t *testing.T) {
	cq, err := ClusterQueueFor(testPool())
	if err != nil {
		t.Fatalf("ClusterQueueFor: %v", err)
	}
	got := cq.Spec.ResourceGroups[0].CoveredResources
	want := []string{"cpu", "memory", "nvidia.com/gpu"}
	if len(got) != len(want) {
		t.Fatalf("coveredResources = %v, want %v", got, want)
	}
	for i, w := range want {
		if string(got[i]) != w {
			t.Fatalf("coveredResources[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// kueue.rs: quota_lands_under_the_right_flavor_and_resource
func TestQuotaLandsUnderTheRightFlavorAndResource(t *testing.T) {
	cq, err := ClusterQueueFor(testPool())
	if err != nil {
		t.Fatalf("ClusterQueueFor: %v", err)
	}
	flavors := cq.Spec.ResourceGroups[0].Flavors
	if len(flavors) != 2 {
		t.Fatalf("flavors len = %d", len(flavors))
	}
	a100 := flavors[0]
	if string(a100.Name) != "a100" {
		t.Fatalf("flavors[0].name = %q", a100.Name)
	}
	if len(a100.Resources) != 3 {
		t.Fatalf("a100 resources len = %d", len(a100.Resources))
	}
	wantA100 := map[string]string{"cpu": "64", "memory": "256Gi", "nvidia.com/gpu": "8"}
	for i, name := range []string{"cpu", "memory", "nvidia.com/gpu"} {
		r := a100.Resources[i]
		if string(r.Name) != name {
			t.Fatalf("a100.resources[%d].name = %q, want %q", i, r.Name, name)
		}
		if r.NominalQuota.String() != wantA100[name] {
			t.Fatalf("a100.resources[%d].nominalQuota = %q, want %q", i, r.NominalQuota.String(), wantA100[name])
		}
	}
	spot := flavors[1]
	if string(spot.Name) != "spot-cpu" {
		t.Fatalf("flavors[1].name = %q", spot.Name)
	}
	if len(spot.Resources) != 2 {
		t.Fatalf("spot-cpu declares no GPU quota, resources len = %d", len(spot.Resources))
	}
}

// kueue.rs: local_queue_points_at_pool_in_project_namespace
func TestLocalQueuePointsAtPoolInProjectNamespace(t *testing.T) {
	lq := LocalQueueFor(testAlloc(), core.PoolPurposeCompute)
	if lq.APIVersion != KueueAPIVersion || lq.Kind != "LocalQueue" {
		t.Fatalf("apiVersion/kind = %q/%q", lq.APIVersion, lq.Kind)
	}
	if lq.Name != "proj-a" {
		t.Fatalf("name = %q", lq.Name)
	}
	if lq.Namespace != "proj-a" {
		t.Fatalf("namespace = %q", lq.Namespace)
	}
	if string(lq.Spec.ClusterQueue) != "gpu-pool" {
		t.Fatalf("clusterQueue = %q", lq.Spec.ClusterQueue)
	}
	if lq.Labels[PoolLabel] != "gpu-pool" {
		t.Fatalf("pool label = %q", lq.Labels[PoolLabel])
	}
}

// kueue.rs: local_queue_annotations_record_allocation_limits
func TestLocalQueueAnnotationsRecordAllocationLimits(t *testing.T) {
	lq := LocalQueueFor(testAlloc(), core.PoolPurposeCompute)
	var nominal map[string]string
	if err := json.Unmarshal([]byte(lq.Annotations[NominalAnnotation]), &nominal); err != nil {
		t.Fatalf("unmarshal nominal: %v", err)
	}
	if nominal["cpu"] != "16" {
		t.Fatalf("nominal = %#v", nominal)
	}
	var borrowing map[string]string
	if err := json.Unmarshal([]byte(lq.Annotations[BorrowingLimitAnnotation]), &borrowing); err != nil {
		t.Fatalf("unmarshal borrowing: %v", err)
	}
	if borrowing["cpu"] != "64" {
		t.Fatalf("borrowing = %#v", borrowing)
	}
	var lending map[string]string
	if err := json.Unmarshal([]byte(lq.Annotations[LendingLimitAnnotation]), &lending); err != nil {
		t.Fatalf("unmarshal lending: %v", err)
	}
	if len(lending) != 0 {
		t.Fatalf("lending = %#v", lending)
	}
}

// kueue.rs: constants_match_kueue_conventions
func TestConstantsMatchKueueConventions(t *testing.T) {
	if QueueLabel != "kueue.x-k8s.io/queue-name" {
		t.Fatalf("QueueLabel = %q", QueueLabel)
	}
	if ElasticJobAnnotation != "kueue.x-k8s.io/elastic-job" {
		t.Fatalf("ElasticJobAnnotation = %q", ElasticJobAnnotation)
	}
}

// kueue.rs: fractional_fair_sharing_weight_stays_a_json_float — ported as:
// a fractional weight must survive as a fractional Quantity, not be
// truncated to an integer. (The typed v1beta2 API represents
// fairSharing.weight as resource.Quantity, not the IntOrString the Rust
// reference's weight_json discriminator targeted — see fairSharingWeight's
// doc comment for the impedance-mismatch note.)
func TestFractionalFairSharingWeightStaysAFraction(t *testing.T) {
	p := testPool()
	p.FairSharingWeight = 0.5
	cq, err := ClusterQueueFor(p)
	if err != nil {
		t.Fatalf("ClusterQueueFor: %v", err)
	}
	if got := cq.Spec.FairSharing.Weight.AsApproximateFloat64(); got != 0.5 {
		t.Fatalf("weight = %v, want 0.5", got)
	}
}

// Additional coverage beyond the Rust suite: an unparseable quota string
// must surface as an error from the typed constructor rather than being
// passed through silently (the typed-API impedance mismatch noted in
// task-5-report.md).
func TestClusterQueueForRejectsUnparseableQuota(t *testing.T) {
	p := testPool()
	p.Flavors[0].Resources["cpu"] = "not-a-quantity"
	if _, err := ClusterQueueFor(p); err == nil {
		t.Fatalf("expected an error for an unparseable quota string")
	}
}

// Error-path coverage (fix round 1, MEDIUM 1): a non-finite
// fair_sharing_weight (NaN/Inf) formats to a string ("NaN"/"+Inf")
// resource.ParseQuantity rejects, and that rejection must surface as an
// error from ClusterQueueFor rather than panicking or silently zeroing
// the weight.
func TestClusterQueueForRejectsNonFiniteFairSharingWeight(t *testing.T) {
	p := testPool()
	p.FairSharingWeight = math.NaN()
	if _, err := ClusterQueueFor(p); err == nil {
		t.Fatalf("expected an error for a non-finite fair_sharing_weight")
	}
}

// #4: a serving pool's LocalQueue is `<project>-serving` so a project
// allocated to both a compute and a serving pool in one namespace gets two
// queues; compute keeps the bare project name (no migration).
func TestLocalQueueNameByPurpose(t *testing.T) {
	if got := LocalQueueName("proj-a", core.PoolPurposeCompute); got != "proj-a" {
		t.Fatalf("compute name = %q", got)
	}
	if got := LocalQueueName("proj-a", ""); got != "proj-a" {
		t.Fatalf("zero-value purpose name = %q, want compute's", got)
	}
	if got := LocalQueueName("proj-a", core.PoolPurposeServing); got != "proj-a-serving" {
		t.Fatalf("serving name = %q", got)
	}
	lq := LocalQueueFor(testAlloc(), core.PoolPurposeServing)
	if lq.Name != "proj-a-serving" || lq.Namespace != "proj-a" || string(lq.Spec.ClusterQueue) != "gpu-pool" {
		t.Fatalf("serving LocalQueue = %s/%s -> %s", lq.Namespace, lq.Name, lq.Spec.ClusterQueue)
	}
}
