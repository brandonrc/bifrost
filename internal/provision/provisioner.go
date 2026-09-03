// Package provision is the only Kubernetes-aware package in Bifrost's
// control plane: it translates provider-agnostic domain specs
// (internal/core) into typed KubeRay (ray.io/v1) and Kueue
// (kueue.x-k8s.io/v1beta2) custom resources, and defines the Provisioner
// seam the reconcile engine (internal/controller, Task 9) drives.
//
// This file is pure (no Kubernetes client, no I/O) — it mirrors
// mobula-provision/src/lib.rs. The live client that actually talks to a
// cluster (server-side apply, watches, pod/event reads) lives in
// internal/provision/live (Task 6), kept thin per the wave's coverage-gate
// exclusion.
//
// Engine shape: [ClusterSpec] already carries an Engine discriminator
// (internal/core/cluster.go); [Provisioner] takes the whole spec rather
// than engine-specific parameters, so a future multi-engine EngineRouter
// (Wave 3, mirrors mobula's router.rs) can implement this same interface
// and dispatch on spec.Engine without changing the seam.
package provision

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/brandonrc/bifrost/internal/core"
)

// ProvisionErrorKind discriminates [ProvisionError] variants — the Go
// analogue of mobula-provision's `ProvisionError` thiserror enum
// (lib.rs:35-41).
type ProvisionErrorKind int

const (
	// ProvisionErrNotFound: the backend has no resource for this cluster id.
	ProvisionErrNotFound ProvisionErrorKind = iota
	// ProvisionErrBackend: the backend rejected or failed the operation;
	// Message carries its detail.
	ProvisionErrBackend
)

// ProvisionError is a value-typed error mirroring mobula's
// `ProvisionError` (lib.rs:35-41), matching this codebase's established
// error pattern (value types with a Kind discriminant, e.g.
// core.TransitionError, core.PoolSpecError).
type ProvisionError struct {
	Kind ProvisionErrorKind
	// ClusterID is set for [ProvisionErrNotFound].
	ClusterID core.ClusterId
	// Message is set for [ProvisionErrBackend].
	Message string
}

func (e ProvisionError) Error() string {
	switch e.Kind {
	case ProvisionErrNotFound:
		return fmt.Sprintf("cluster not found: %s", e.ClusterID)
	case ProvisionErrBackend:
		return fmt.Sprintf("backend error: %s", e.Message)
	}
	return "provision error"
}

// ObservedCluster is the observed state of a cluster as reported by a
// backend (lib.rs:44-65).
type ObservedCluster struct {
	ID    core.ClusterId
	State core.ClusterState
	// ObservedGeneration is the spec generation the backing cluster
	// actually reflects, read back from the resource Bifrost stamps
	// (ADR-0006-equivalent). nil when the backend exposes no generation
	// marker.
	ObservedGeneration *uint64
	// SpecFingerprint is the owned-field fingerprint recomputed from the
	// live resource (ADR-0004-equivalent drift detection). nil when the
	// backend can't project one.
	SpecFingerprint *string
	// ApiBaseUrl is the cluster's native Ray dashboard/job API base URL,
	// reachable from the control plane. nil when unknown.
	ApiBaseUrl *string
}

// ApplyResponse is the outcome of a [Provisioner.Apply], stored in the
// transactional outbox (ADR-0007-equivalent) so a replay can return it
// without re-actuating a non-idempotent backend (lib.rs:67-77). It is
// JSON-serializable because the store persists it as opaque JSON.
type ApplyResponse struct {
	// Generation is the generation this apply actuated.
	Generation uint64 `json:"generation"`
	// ApiBaseUrl is the cluster's native Ray API base URL, if the backend
	// can name it.
	ApiBaseUrl *string `json:"api_base_url"`
}

// ObservedService is the observed state of a Serve service (lib.rs:232-239).
type ObservedService struct {
	Name  string
	State core.ClusterState
	// Url is the service's external Serve endpoint base URL, if ready.
	Url *string
	// Project is the owning project as stamped on the backing resource
	// (requirement 2: project-scoped services); "" when the backend does
	// not carry one.
	Project string
	// Generation is the Bifrost spec generation the backing resource was
	// last applied at, read back from [GenerationAnnotation]; nil when the
	// resource carries none (created before the annotation existed, or
	// by another manager). The service reconciler redeploys when it is
	// nil or behind the stored generation.
	Generation *uint64
}

// ObservedJob is the observed state of an ephemeral Ray job as read back
// from its backing resource (a KubeRay RayJob's status, on Kubernetes).
// Vocabularies are the backend's verbatim — Ray's job status and
// KubeRay's deployment status — because the controller records what it
// sees (ADR-0006-equivalent) and the API normalizes.
type ObservedJob struct {
	ID core.ClusterId
	// JobStatus is Ray's job status (PENDING | RUNNING | SUCCEEDED |
	// FAILED | STOPPED); "" until Ray reports one.
	JobStatus string
	// DeploymentStatus is KubeRay's job deployment status (Initializing |
	// Running | Complete | Failed | Suspended | ...); "" until observed.
	DeploymentStatus string
	// ClusterName is the backing RayCluster's name while it exists.
	ClusterName *string
	// DashboardURL is the backing cluster's Ray dashboard/Jobs API base,
	// reachable from the control plane, when known.
	DashboardURL *string
	// Message is the backend's last status message, when any.
	Message *string
	// StartTime/EndTime are unix seconds; nil until the job starts /
	// finishes.
	StartTime *uint64
	EndTime   *uint64
}

// PoolObservation is a pool's quota ledger as read back from Kueue's
// ClusterQueue `.status` (lib.rs:241-265): the status *is* the ledger. All
// counts default to 0 when the ClusterQueue exists but Kueue hasn't
// populated status yet. JSON-serializable: the controller persists it as
// opaque JSON on the pool row.
type PoolObservation struct {
	AdmittedWorkloads  uint32 `json:"admitted_workloads"`
	ReservingWorkloads uint32 `json:"reserving_workloads"`
	PendingWorkloads   uint32 `json:"pending_workloads"`
	// FlavorsUsage is flavor -> resource -> quantity string, from
	// status.flavorsUsage.total (the amounts Kueue admits against, not
	// measured consumption).
	FlavorsUsage map[string]map[string]string `json:"flavors_usage"`
	// QueuesUsage is LocalQueue name -> resource -> quantity string, the
	// per-project attribution the ClusterQueue-level FlavorsUsage lacks.
	// Absent on the wire deserializes as an empty map (mirrors Rust's
	// `#[serde(default)]`), which is Go's zero-value map behavior already —
	// no custom UnmarshalJSON needed.
	QueuesUsage map[string]map[string]string `json:"queues_usage"`
}

// poolObservationAlias breaks the recursion MarshalJSON would otherwise
// cause by re-entering PoolObservation's own MarshalJSON.
type poolObservationAlias PoolObservation

// MarshalJSON substitutes an empty map for a nil FlavorsUsage or
// QueuesUsage, mirroring Rust's BTreeMap::default(), which serde always
// writes as `{}`, never `null` — the established core/policy pattern.
func (p PoolObservation) MarshalJSON() ([]byte, error) {
	a := poolObservationAlias(p)
	if a.FlavorsUsage == nil {
		a.FlavorsUsage = map[string]map[string]string{}
	}
	if a.QueuesUsage == nil {
		a.QueuesUsage = map[string]map[string]string{}
	}
	return json.Marshal(a)
}

// Provisioner manages the backing resources for managed Ray clusters — the
// Go analogue of mobula's `Provisioner` trait (lib.rs:79-230). Every
// mutating call carries an idempotency key so an HA failover mid-provision
// cannot double-provision.
//
// Rust's trait gives several methods default (no-op) bodies; Go interfaces
// cannot. Implementations that want those defaults verbatim should embed
// [BaseProvisioner], which supplies them.
type Provisioner interface {
	// Apply creates or updates the backing resources for spec at
	// generation. Idempotent per idempotencyKey: repeating a call must not
	// create duplicates. queue nominates the Kueue LocalQueue the workload
	// is admitted through (ADR-0010-equivalent); nil leaves the manifest
	// queue-free.
	Apply(ctx context.Context, id core.ClusterId, spec *core.ClusterSpec, generation uint64, idempotencyKey string, queue *QueueAssignment) (ApplyResponse, error)

	// EnsureNamespacePosture ensures the namespace-level security posture
	// (default-deny NetworkPolicy + explicit allows + Pod Security
	// Standards labels) for the namespace this backend provisions into.
	// Idempotent; must NOT overwrite a stricter existing posture
	// (check-then-apply).
	EnsureNamespacePosture(ctx context.Context) error

	// Terminate begins teardown. Idempotent; succeeds if already gone.
	Terminate(ctx context.Context, id core.ClusterId) error

	// ReapNetworkPolicies deletes the per-cluster NetworkPolicy(ies) for
	// id. Idempotent: already-gone is success.
	ReapNetworkPolicies(ctx context.Context, id core.ClusterId) error

	// Suspend releases the cluster's compute while keeping spec and state.
	// Idempotent; succeeds if already suspended.
	Suspend(ctx context.Context, id core.ClusterId) error

	// Resume resumes a suspended cluster. Idempotent.
	Resume(ctx context.Context, id core.ClusterId) error

	// Observe reads current state without mutating anything.
	Observe(ctx context.Context, id core.ClusterId) (ObservedCluster, error)

	// List returns every cluster this backend manages (field-manager
	// scoped).
	List(ctx context.Context) ([]ObservedCluster, error)

	// MetricsEndpoint returns the Ray head's Prometheus metrics endpoint
	// for id, and whether the backend can name one. Synchronous and pure.
	MetricsEndpoint(id core.ClusterId) (string, bool)

	// DashboardApiBase returns the base URL of the cluster's native Ray
	// dashboard / Job Submission API, and whether the backend can name
	// one. Synchronous and pure.
	DashboardApiBase(id core.ClusterId) (string, bool)

	// ClusterNodes returns the Kubernetes-sourced head + worker-group node
	// breakdown for id, or nil when the backend cannot produce one.
	ClusterNodes(ctx context.Context, id core.ClusterId) (*core.ClusterNodes, error)

	// ClusterEvents returns Kubernetes Events for id's objects, or nil
	// when the backend cannot produce one.
	ClusterEvents(ctx context.Context, id core.ClusterId) (*core.ClusterEvents, error)

	// ClusterLogs returns tail-capped pod logs for id. pod selects one of
	// the cluster's pods; nil tails the head pod. Returns nil when the
	// backend cannot produce one (or the requested pod is not part of the
	// cluster).
	ClusterLogs(ctx context.Context, id core.ClusterId, pod *string, tail uint32) (*core.ClusterLogs, error)
}

// BaseProvisioner supplies the no-op default bodies Rust's `Provisioner`
// trait gives several methods (lib.rs:109-111, 135-138, 168-171, 180-183,
// 192-198, 206-212, 221-229). Embed it in a concrete Provisioner
// implementation to inherit these defaults and override only the methods
// that need real behavior.
type BaseProvisioner struct{}

func (BaseProvisioner) EnsureNamespacePosture(context.Context) error { return nil }

func (BaseProvisioner) ReapNetworkPolicies(context.Context, core.ClusterId) error { return nil }

func (BaseProvisioner) MetricsEndpoint(core.ClusterId) (string, bool) { return "", false }

func (BaseProvisioner) DashboardApiBase(core.ClusterId) (string, bool) { return "", false }

func (BaseProvisioner) ClusterNodes(context.Context, core.ClusterId) (*core.ClusterNodes, error) {
	return nil, nil
}

func (BaseProvisioner) ClusterEvents(context.Context, core.ClusterId) (*core.ClusterEvents, error) {
	return nil, nil
}

func (BaseProvisioner) ClusterLogs(context.Context, core.ClusterId, *string, uint32) (*core.ClusterLogs, error) {
	return nil, nil
}

// PoolProvisioner manages a pool's Kueue objects (Cohort / ResourceFlavors
// / ClusterQueue / LocalQueues) — the Go analogue of mobula's
// `PoolProvisioner` trait (lib.rs:272-294).
type PoolProvisioner interface {
	// ApplyPool creates or updates all of a pool's Kueue objects
	// (server-side apply: idempotent for identical desired state).
	ApplyPool(ctx context.Context, spec *core.PoolSpec, allocs []core.AllocationSpec) error

	// DeletePool deletes every Kueue object of the named pool. Idempotent;
	// succeeds when already gone.
	DeletePool(ctx context.Context, name string) error

	// ObservePool reads the pool's quota ledger from its ClusterQueue
	// status. Returns nil when the ClusterQueue does not exist.
	ObservePool(ctx context.Context, name string) (*PoolObservation, error)

	// KueuePresent reports whether the API server serves the Kueue CRDs.
	// When false the pool reconcile loop skips actuation entirely.
	KueuePresent(ctx context.Context) bool
}

// ServiceProvisioner manages Ray Serve services (RayService CRs) — the Go
// analogue of mobula's `ServiceProvisioner` trait (lib.rs:301-311).
// Distinct from [Provisioner] because KubeRay's RayService controller owns
// convergence and zero-downtime upgrades.
type ServiceProvisioner interface {
	// Deploy deploys or updates a service (server-side apply of a
	// RayService). Idempotent; the upgrade strategy in the spec drives
	// canary vs in-place rollout. generation is the Bifrost spec
	// generation being applied; the backend stamps it on the resource so
	// Get can report it back (ObservedService.Generation).
	Deploy(ctx context.Context, name string, spec *core.ServiceSpec, generation uint64) error

	Get(ctx context.Context, name string) (*ObservedService, error)

	// Delete is an idempotent teardown; succeeds if already gone.
	Delete(ctx context.Context, name string) error

	List(ctx context.Context) ([]ObservedService, error)
}

// JobProvisioner manages ephemeral Ray jobs (RayJob CRs) — requirement 5.
// Distinct from [Provisioner] because KubeRay's RayJob controller owns the
// cluster's lifetime: it provisions the cluster, submits the entrypoint,
// and tears the cluster down after the job finishes, so Bifrost applies
// intent and observes rather than driving each step.
type JobProvisioner interface {
	// ApplyJob creates or updates the backing resources for spec under id
	// (server-side apply: idempotent for identical desired state).
	// generation is stamped on the resource for drift detection; queue
	// nominates the Kueue LocalQueue the job's cluster is admitted
	// through, nil leaves the manifest queue-free.
	ApplyJob(ctx context.Context, id core.ClusterId, spec *core.RayJobSpec, generation uint64, queue *QueueAssignment) error

	// ObserveJob reads current state without mutating anything. Returns
	// a [ProvisionErrNotFound] error when the backend has no resource for
	// id.
	ObserveJob(ctx context.Context, id core.ClusterId) (ObservedJob, error)

	// DeleteJob stops the job and tears its cluster down. Idempotent;
	// succeeds if already gone.
	DeleteJob(ctx context.Context, id core.ClusterId) error

	// ListJobs returns every job this backend manages (field-manager
	// scoped).
	ListJobs(ctx context.Context) ([]ObservedJob, error)
}
