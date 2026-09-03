package provision

import (
	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// ZeroStatus resets obj's `.Status` field to its Go zero value in place,
// for every concrete type internal/provision/live server-side-applies
// (RayCluster, RayService, Cohort, ClusterQueue, LocalQueue). Types with no
// `.Status` field at all (ResourceFlavor, NetworkPolicy) — and any type not
// in the switch — are left untouched (a documented no-op).
//
// Why this exists (Wave 1 Task 5 review ledger, task-5-report.md impedance
// mismatch #6): RayClusterFor/RayServiceFor/CohortFor/ClusterQueueFor/
// LocalQueueFor never set `.Status` — but it is a non-pointer struct field
// on every one of these types, so Go's `encoding/json` `omitempty` cannot
// elide it; the zero value still marshals as a populated
// `"status":{...}` object, never an absent key. controller-runtime's
// `client.Patch(ctx, obj, client.Apply, ...)` sends exactly
// `json.Marshal(obj)` as the server-side-apply body (see
// sigs.k8s.io/controller-runtime/pkg/client.applyPatch.Data) — the WHOLE
// object, status block included.
//
// That is harmless as long as the target CRD has the status subresource
// enabled: the API server ignores a `status` field sent to the main
// resource endpoint in that case. But if a CRD is ever installed WITHOUT
// the status subresource — an older/misconfigured CRD, a cluster that
// hasn't picked up a CRD upgrade yet, or a future Kueue/KubeRay type this
// package starts applying — the identical apply request writes straight
// through to the live `.status`, blanking whatever the operator/controller
// had already converged there. Bifrost would then observe its own
// erasure and read the cluster as stuck "Provisioning" forever (the
// failure mode this helper exists to make structurally impossible, not
// just conventionally avoided).
//
// Called from exactly one place: internal/provision/live's applySSA,
// the sole function in that package that invokes
// `client.Patch(ctx, obj, client.Apply, ...)`. Every apply site in
// internal/provision/live goes through applySSA (no site calls
// client.Patch directly), so this is a structural guarantee, not a
// per-call-site convention — a future apply site literally cannot forget
// to zero status, the way a first draft of this package's ResourceFlavor
// apply once did when the guard was still a per-call-site
// `provision.ZeroStatus(obj)` before each Patch (fix round 1, M1). Firing
// unconditionally, regardless of whether the in-memory object is already
// known to have a zero Status (e.g. because it came straight out of a
// pure translator), also means a future refactor that reuses a
// freshly-Get'd (and therefore really populated) object as an apply base
// can't reintroduce this bug either.
func ZeroStatus(obj client.Object) {
	switch o := obj.(type) {
	case *rayv1.RayCluster:
		o.Status = rayv1.RayClusterStatus{}
	case *rayv1.RayService:
		o.Status = rayv1.RayServiceStatuses{}
	case *rayv1.RayJob:
		o.Status = rayv1.RayJobStatus{}
	case *kueuev1beta2.Cohort:
		o.Status = kueuev1beta2.CohortStatus{}
	case *kueuev1beta2.ClusterQueue:
		o.Status = kueuev1beta2.ClusterQueueStatus{}
	case *kueuev1beta2.LocalQueue:
		o.Status = kueuev1beta2.LocalQueueStatus{}
	}
}
