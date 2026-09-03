package provision

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"github.com/brandonrc/bifrost/internal/core"
)

// Kueue backend for resource pools (ADR-0010-equivalent): translate
// Bifrost pool domain types into typed Kueue custom resources. Ported from
// mobula-provision/src/kueue.rs.
//
// This file is pure (no Kubernetes client) so the pool->Kueue mapping is
// exhaustively testable, mirroring [kuberay.go]'s approach. The object
// model: a ResourceFlavor per pool flavor, one ClusterQueue per pool joined
// to a shared Cohort for elastic borrowing, and one LocalQueue per project
// allocation.
//
// v0 queue layout: one ClusterQueue per pool. All project allocations in a
// pool point their LocalQueue at the same ClusterQueue, so borrowing
// between projects inside a pool is arbitrated by Kueue's admission fair
// sharing rather than per-project quotas. The AllocationSpec
// nominal/borrowing/lending limits are reserved for a future per-project
// ClusterQueue layout; until then they are serialized into LocalQueue
// metadata annotations (bifrost.dev/nominal etc., as JSON) so the declared
// intent is recorded on the object and a later layout migration can read
// it back.

const (
	// KueueAPIVersion is the API version for all Kueue objects Bifrost
	// manages. v1beta2 is the storage version since Kueue v0.19 and the
	// only one carrying spec.cohortName (v1beta1 spells it spec.cohort
	// and is deprecated).
	KueueAPIVersion = "kueue.x-k8s.io/v1beta2"

	// PoolLabel ties every Kueue object Bifrost creates back to its pool.
	// Stamped on the ResourceFlavors, Cohort, ClusterQueue, and
	// LocalQueues so DeletePool can find and remove a pool's objects by
	// selector after the spec is gone from the store.
	PoolLabel = "bifrost.dev/pool"

	// Annotation keys recording the reserved per-project limits on the
	// LocalQueue (see module docs above).
	NominalAnnotation        = "bifrost.dev/nominal"
	BorrowingLimitAnnotation = "bifrost.dev/borrowing-limit"
	LendingLimitAnnotation   = "bifrost.dev/lending-limit"
)

// ResourceFlavorFor builds the ResourceFlavor manifest for one pool
// flavor: node labels and taints select the hardware this flavor's quota
// applies to. pool is the owning pool's name, stamped as [PoolLabel] so
// the object is findable by selector at teardown. Ported from
// kueue.rs:52-76.
func ResourceFlavorFor(pool string, flavor *core.FlavorSpec) *kueuev1beta2.ResourceFlavor {
	taints := make([]corev1.Taint, len(flavor.Taints))
	for i, t := range flavor.Taints {
		taints[i] = corev1.Taint{Key: t.Key, Value: t.Value, Effect: corev1.TaintEffect(t.Effect)}
	}
	return &kueuev1beta2.ResourceFlavor{
		TypeMeta: metav1.TypeMeta{APIVersion: KueueAPIVersion, Kind: "ResourceFlavor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   flavor.Name,
			Labels: map[string]string{PoolLabel: pool},
		},
		Spec: kueuev1beta2.ResourceFlavorSpec{
			NodeLabels: flavor.NodeLabels,
			NodeTaints: taints,
		},
	}
}

// CohortFor builds the Cohort manifest — the shared capacity envelope
// member ClusterQueues borrow from. Empty spec: the v0 topology keeps
// quotas on the ClusterQueues, not on the cohort itself. Ported from
// kueue.rs:82-92.
func CohortFor(pool *core.PoolSpec) *kueuev1beta2.Cohort {
	return &kueuev1beta2.Cohort{
		TypeMeta: metav1.TypeMeta{APIVersion: KueueAPIVersion, Kind: "Cohort"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   pool.Cohort,
			Labels: map[string]string{PoolLabel: pool.Name},
		},
		Spec: kueuev1beta2.CohortSpec{},
	}
}

// fairSharingWeight converts a PoolSpec fair-sharing weight to Kueue's
// typed FairSharing.Weight ([resource.Quantity]). The Rust reference
// hand-rolled an int-vs-float JSON discriminator (weight_json,
// kueue.rs:94-103) so IntOrString decoding never saw a float; the typed
// v1beta2 API changed the field's Kubernetes type to resource.Quantity
// (not IntOrString) since this port's pin (Kueue v0.19), so that
// discriminator has no typed equivalent to reproduce — Quantity's decimal
// string form (e.g. "2", "0.5") already carries fractional weights
// natively. Noted as a typed-API impedance mismatch in the task report.
func fairSharingWeight(w float64) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(strconv.FormatFloat(w, 'f', -1, 64))
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("provision: invalid fair_sharing_weight %v: %w", w, err)
	}
	return q, nil
}

// ClusterQueueFor builds the pool's ClusterQueue: joined to the cohort,
// weighted for fair sharing, with one resource group covering the union of
// all flavor resource keys. Quotas land per flavor per resource as
// nominalQuota quantities (parsed from the spec's quota strings —
// parseability is validated upstream in internal/policy). Ported from
// kueue.rs:110-164.
func ClusterQueueFor(pool *core.PoolSpec) (*kueuev1beta2.ClusterQueue, error) {
	// coveredResources is the sorted union of resource keys across
	// flavors (mirrors Rust's BTreeSet<&String> union).
	coveredSet := map[string]struct{}{}
	for _, f := range pool.Flavors {
		for k := range f.Resources {
			coveredSet[k] = struct{}{}
		}
	}
	coveredNames := make([]string, 0, len(coveredSet))
	for k := range coveredSet {
		coveredNames = append(coveredNames, k)
	}
	sort.Strings(coveredNames)
	covered := make([]corev1.ResourceName, len(coveredNames))
	for i, n := range coveredNames {
		covered[i] = corev1.ResourceName(n)
	}

	flavors := make([]kueuev1beta2.FlavorQuotas, len(pool.Flavors))
	for i, f := range pool.Flavors {
		keys := sortedKeys(f.Resources)
		resources := make([]kueuev1beta2.ResourceQuota, len(keys))
		for j, name := range keys {
			q, err := resource.ParseQuantity(f.Resources[name])
			if err != nil {
				return nil, fmt.Errorf("provision: flavor %q resource %q: invalid quota %q: %w", f.Name, name, f.Resources[name], err)
			}
			resources[j] = kueuev1beta2.ResourceQuota{
				Name:         corev1.ResourceName(name),
				NominalQuota: q,
			}
		}
		flavors[i] = kueuev1beta2.FlavorQuotas{
			Name:      kueuev1beta2.ResourceFlavorReference(f.Name),
			Resources: resources,
		}
	}

	weight, err := fairSharingWeight(pool.FairSharingWeight)
	if err != nil {
		return nil, err
	}

	return &kueuev1beta2.ClusterQueue{
		TypeMeta: metav1.TypeMeta{APIVersion: KueueAPIVersion, Kind: "ClusterQueue"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   pool.Name,
			Labels: map[string]string{PoolLabel: pool.Name},
		},
		Spec: kueuev1beta2.ClusterQueueSpec{
			CohortName: kueuev1beta2.CohortReference(pool.Cohort),
			// Empty selector = all namespaces eligible. Kueue's default
			// (nil) is a NOTHING selector — no LocalQueue in any
			// namespace may use the ClusterQueue — so a pool would admit
			// zero workloads. Bifrost scopes tenancy through
			// allocations, not namespace labels.
			NamespaceSelector: &metav1.LabelSelector{},
			FairSharing:       &kueuev1beta2.FairSharing{Weight: &weight},
			ResourceGroups: []kueuev1beta2.ResourceGroup{
				{CoveredResources: covered, Flavors: flavors},
			},
		},
	}, nil
}

// localQueueLimits is the annotation payload for LocalQueueFor's
// nominal/borrowing/lending-limit annotations.
func marshalLimits(m map[string]string) string {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("provision: marshaling allocation limits: %v", err))
	}
	return string(b)
}

// LocalQueueName is the LocalQueue a project's allocation in a pool of
// the given purpose is named (#4). Compute keeps the bare project name
// (every pre-#4 LocalQueue, so no migration); serving appends "-serving"
// so a project allocated to both a compute and a serving pool in one
// namespace gets two distinct queues instead of one object fought over by
// two pools' applies. Clusters and jobs are admitted through the compute
// queue, RayServices through the serving one.
func LocalQueueName(project string, purpose core.PoolPurpose) string {
	if purpose.OrDefault() == core.PoolPurposeServing {
		return project + "-serving"
	}
	return project
}

// LocalQueueFor builds a project allocation's LocalQueue: the namespaced
// tenant handle pointing at the pool's ClusterQueue, named per
// [LocalQueueName] for the owning pool's purpose. The reserved
// nominal/borrowing/lending limits are recorded as JSON annotations (see
// module docs above) — these are opaque record-keeping, not typed Kueue
// spec fields, so they stay JSON-encoded strings exactly as in the Rust
// reference rather than becoming typed resource.Quantity values. Ported
// from kueue.rs:169-187.
func LocalQueueFor(alloc *core.AllocationSpec, purpose core.PoolPurpose) *kueuev1beta2.LocalQueue {
	return &kueuev1beta2.LocalQueue{
		TypeMeta: metav1.TypeMeta{APIVersion: KueueAPIVersion, Kind: "LocalQueue"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      LocalQueueName(alloc.Project, purpose),
			Namespace: alloc.Namespace,
			Labels:    map[string]string{PoolLabel: alloc.Pool},
			Annotations: map[string]string{
				NominalAnnotation:        marshalLimits(alloc.Nominal),
				BorrowingLimitAnnotation: marshalLimits(alloc.BorrowingLimit),
				LendingLimitAnnotation:   marshalLimits(alloc.LendingLimit),
			},
		},
		Spec: kueuev1beta2.LocalQueueSpec{
			ClusterQueue: kueuev1beta2.ClusterQueueReference(alloc.Pool),
		},
	}
}
