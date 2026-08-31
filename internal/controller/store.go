// Package controller holds the Bifrost control plane's desired-state store:
// the domain types persisted by every store backend (memory now; SQLite and
// Postgres follow in Tasks 3-4), the Store interface those backends
// implement, and the audit tamper-evidence hash chain.
//
// Ported from mobula-controller/src/store.rs (Rust reference, retired
// project — cited here only where a file:line reference is genuinely
// useful; never in user-facing strings). The observed *state* of a cluster
// is reconstructed from the provisioner every reconcile (ADR-0006 in the
// Rust reference) — it is never stored as authoritative truth. The store
// itself is truth for desired state.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
)

// --- Store error ---

// StoreError is the store package's single error value type, mirroring
// Rust's single-variant `StoreError::Backend(String)` (store.rs:13-17).
// Every store backend wraps its own errors (SQL driver errors, JSON
// (de)serialization failures on JSON-text columns, …) into this type.
type StoreError struct {
	Msg string
}

// Error implements the error interface, matching the Rust reference's
// `#[error("store backend error: {0}")]` Display impl.
func (e StoreError) Error() string {
	return fmt.Sprintf("store backend error: %s", e.Msg)
}

// storeErrorf constructs a StoreError with a formatted message — the Go
// equivalent of Rust's `StoreError::Backend(format!(...))` call sites
// scattered through store.rs.
func storeErrorf(format string, args ...any) StoreError {
	return StoreError{Msg: fmt.Sprintf(format, args...)}
}

// --- Desired state (carried Wave-0 pointer: lives here, not internal/core) ---

// DesiredState is what the operator wants a cluster to be. The *observed*
// state (core.ClusterState) is reconstructed from the provisioner every
// reconcile — it is never stored as authoritative truth (store.rs:32-48).
//
// Persisted as a string column by the SQL-backed stores (Tasks 3-4), so
// adding a variant is back-compatible with old rows; an *old* binary
// reading a row written by a newer one errors rather than guessing (see
// ParseDesiredState). The Rust DesiredState type itself carries no serde
// derive — it round-trips through the SQL layer's own string mapping and
// the API layer's explicit match arms (mobula-api/src/clusters.rs:120-123)
// — so these wire strings, not a #[derive(Serialize)], are the contract
// this type must honor.
type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	// DesiredSuspended: compute released, spec kept. The reconciler drives
	// the backing cluster to `spec.suspend: true` — except for
	// queue-assigned clusters, where Kueue owns suspend and the API
	// rejects user suspend/resume with 409 (Task 11 concern).
	DesiredSuspended  DesiredState = "suspended"
	DesiredTerminated DesiredState = "terminated"
)

func (d DesiredState) isValid() bool {
	switch d {
	case DesiredRunning, DesiredSuspended, DesiredTerminated:
		return true
	}
	return false
}

// AsStr returns the wire value ("running" | "suspended" | "terminated").
func (d DesiredState) AsStr() string {
	return string(d)
}

// ParseDesiredState parses a wire value into a DesiredState, rejecting
// anything else — the Go equivalent of store_sqlite.rs's
// `desired_from_str`, shared here so every SQL-backed store (Tasks 3-4)
// gets identical strictness.
func ParseDesiredState(s string) (DesiredState, error) {
	d := DesiredState(s)
	if !d.isValid() {
		return "", storeErrorf("bad desired state %q", s)
	}
	return d, nil
}

// --- Stored cluster ---

// StoredCluster is a persisted cluster: desired spec + a monotonic
// Generation that bumps whenever the spec changes, plus the last
// observation. Generation vs ObservedGeneration is the drift signal (K8s
// convention).
type StoredCluster struct {
	ID   core.ClusterId
	Spec core.ClusterSpec
	// Generation bumps whenever Spec actually changes (see specChanged).
	Generation uint64
	Desired    DesiredState
	// ObservedState is nil until the reconciler has observed anything.
	ObservedState *core.ClusterState
	// ObservedGeneration is the generation the last observation reflects;
	// monotonic non-decreasing (see MemoryStore.RecordObservation).
	ObservedGeneration uint64
	// Condition is a drift/health alarm raised by the reconcile engine,
	// distinct from ObservedState. nil while the cluster converges
	// normally.
	Condition *core.DriftCondition
	// FailureCount is consecutive no-progress reconcile attempts. Resets
	// to 0 on progress; drives the exponential backoff delay.
	FailureCount uint32
	// NextAttemptAt is unix seconds before which the reconciler must not
	// re-actuate this cluster (backoff gate). 0 means "no backoff
	// pending".
	NextAttemptAt uint64
	// CreatedAt is unix seconds when the cluster was first created (for
	// TTL reaping).
	CreatedAt uint64
	// TerminatedAt is unix seconds when Desired last became
	// DesiredTerminated, or nil while the cluster is running/suspended.
	// Anchors the terminated-row retention sweep: a tombstone older than
	// the window is hard-deleted. Cleared if the cluster is resumed.
	TerminatedAt *uint64
}

// IntentKey is the idempotency/fencing key for actuating this desired
// state: derived from id + generation, so a level-triggered loop produces
// the *same* key for the *same* desired state — never a per-call UUID.
func (c StoredCluster) IntentKey() string {
	return fmt.Sprintf("%s/%d", c.ID, c.Generation)
}

// NowUnix returns the current unix time in whole seconds.
func NowUnix() uint64 {
	return uint64(time.Now().Unix())
}

// specChanged reports whether two specs differ in a way that should bump
// generation. core.ClusterSpec carries no equality method, so this
// compares the fields that drive actuation, shared by every Store
// implementation (store.rs:98-120).
func specChanged(a, b *core.ClusterSpec) bool {
	if a.Name != b.Name ||
		a.Project != b.Project ||
		a.RayVersion != b.RayVersion ||
		a.Image != b.Image ||
		a.HeadCpu != b.HeadCpu ||
		a.HeadMemory != b.HeadMemory ||
		!uint64PtrEqual(a.TtlSeconds, b.TtlSeconds) ||
		!uint64PtrEqual(a.IdleTimeoutSecs, b.IdleTimeoutSecs) ||
		len(a.WorkerGroups) != len(b.WorkerGroups) {
		return true
	}
	for i := range a.WorkerGroups {
		x, y := a.WorkerGroups[i], b.WorkerGroups[i]
		if x.Name != y.Name ||
			x.Cpu != y.Cpu ||
			x.Memory != y.Memory ||
			!stringPtrEqual(x.Gpu, y.Gpu) ||
			x.MinReplicas != y.MinReplicas ||
			x.MaxReplicas != y.MaxReplicas ||
			x.Replicas != y.Replicas {
			return true
		}
	}
	return false
}

func uint64PtrEqual(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// --- Stored pool ---

// StoredPool is a persisted pool: the pool spec plus a monotonic
// Generation that bumps whenever the spec changes, plus the last Kueue
// observation recorded by the pool reconcile loop.
type StoredPool struct {
	Name       string
	Spec       core.PoolSpec
	Generation uint64
	// ObservedJSON is the last observed ClusterQueue status (opaque JSON),
	// nil until the pool reconcile loop has observed this pool. Never
	// authoritative — pools are level-triggered from the spec like
	// clusters are.
	ObservedJSON *string
	// ObservedAt is unix seconds when ObservedJSON was recorded; nil until
	// the first observation.
	ObservedAt *uint64
	// CreatedAt is unix seconds when the pool was first created.
	CreatedAt uint64
}

// poolSpecChanged reports whether two pool specs differ in a way that
// should bump generation. core.PoolSpec has no derived equality in Go, so
// this does a full structural comparison — the Go equivalent of Rust's
// derived PartialEq (store.rs:143-149).
func poolSpecChanged(a, b *core.PoolSpec) bool {
	return !poolSpecEqual(a, b)
}

func poolSpecEqual(a, b *core.PoolSpec) bool {
	if a.Name != b.Name || a.Cohort != b.Cohort || a.FairSharingWeight != b.FairSharingWeight || a.Elastic != b.Elastic {
		return false
	}
	if !gpuSharingPtrEqual(a.GpuSharing, b.GpuSharing) {
		return false
	}
	if len(a.Flavors) != len(b.Flavors) {
		return false
	}
	for i := range a.Flavors {
		if !flavorSpecEqual(&a.Flavors[i], &b.Flavors[i]) {
			return false
		}
	}
	return true
}

func gpuSharingPtrEqual(a, b *core.GpuSharing) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func flavorSpecEqual(a, b *core.FlavorSpec) bool {
	if a.Name != b.Name {
		return false
	}
	if !stringMapEqual(a.Resources, b.Resources) || !stringMapEqual(a.NodeLabels, b.NodeLabels) {
		return false
	}
	if len(a.Taints) != len(b.Taints) {
		return false
	}
	for i := range a.Taints {
		if a.Taints[i] != b.Taints[i] {
			return false
		}
	}
	return true
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// --- Usage samples (metering, Wave 3 consumer; stored types land now) ---

// UsageSource is where a usage sample came from: Kueue's flavorsUsage is a
// *reservation* ledger, not measured consumption, so Bifrost meters
// attribution itself and labels each sample's provenance.
type UsageSource string

const (
	// UsageSourceKueueLedger is Kueue's ClusterQueue/LocalQueue
	// status.flavorsUsage — reservation ledger amounts (what Kueue admits
	// against quota).
	UsageSourceKueueLedger UsageSource = "kueue_ledger"
	// UsageSourceObservedSpec is Bifrost's own estimate from desired
	// cluster specs (the min-demand baseline), used when Kueue is absent.
	UsageSourceObservedSpec UsageSource = "observed_spec"
)

func (s UsageSource) isValid() bool {
	switch s {
	case UsageSourceKueueLedger, UsageSourceObservedSpec:
		return true
	}
	return false
}

// AsStr returns the wire value.
func (s UsageSource) AsStr() string {
	return string(s)
}

// ParseUsageSource parses a wire value into a UsageSource.
func ParseUsageSource(s string) (UsageSource, error) {
	v := UsageSource(s)
	if !v.isValid() {
		return "", storeErrorf("bad usage source %q", s)
	}
	return v, nil
}

// UnmarshalJSON rejects any value other than the known UsageSource
// variants, mirroring serde's strict enum deserialization and this
// package's other strict-enum ingress guards (core.AuditDecision et al.).
func (s *UsageSource) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	v, err := ParseUsageSource(str)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// UsageSample is one point-in-time usage reading: Quantity units of
// Resource attributed to (Project, Pool) at Ts. Append-only timeseries —
// no primary key, no updates. An empty Project is the pool-level aggregate
// row (not attributable to a single project); an empty Pool means the
// project has no allocation.
type UsageSample struct {
	// Ts is unix seconds.
	Ts       uint64      `json:"ts"`
	Project  string      `json:"project"`
	Pool     string      `json:"pool"`
	Resource string      `json:"resource"`
	Quantity float64     `json:"quantity"`
	Source   UsageSource `json:"source"`
}

// --- Governance policy (settings API, Wave 3 consumer; stored types land now) ---

// StoredPolicy is the persisted governance policy: the optional price
// sheet (resource -> $/unit-hour) and per-project quota limits (project ->
// resource -> amount), stored as one JSON-text row by the SQL-backed
// stores (policy is a singleton like the quarantine flag).
type StoredPolicy struct {
	// Prices maps resource -> $/unit-hour; nil = no price sheet (no cost
	// estimates).
	Prices map[string]float64 `json:"prices"`
	// Quotas maps project -> resource -> limit. Empty = no quotas
	// enforced.
	Quotas map[string]map[string]float64 `json:"quotas"`
	// Budgets maps project -> time-windowed compute budget. Empty = no
	// budgets enforced.
	Budgets map[string]StoredBudget `json:"budgets"`
	// FromFileSeed is true while the row is the untouched --policy boot
	// seed.
	FromFileSeed bool `json:"from_file_seed"`
}

// storedPolicyAlias breaks the recursion MarshalJSON would otherwise cause
// by re-entering StoredPolicy's own MarshalJSON.
type storedPolicyAlias StoredPolicy

// MarshalJSON substitutes empty maps for nil Quotas/Budgets, mirroring
// Rust's BTreeMap::default() (via #[serde(default)]), which serde always
// writes as `{}`, never `null`. Prices stays nullable (Option<...>) — a
// true "no price sheet" is meaningfully distinct from an empty one.
func (p StoredPolicy) MarshalJSON() ([]byte, error) {
	a := storedPolicyAlias(p)
	if a.Quotas == nil {
		a.Quotas = map[string]map[string]float64{}
	}
	if a.Budgets == nil {
		a.Budgets = map[string]StoredBudget{}
	}
	return json.Marshal(a)
}

// StoredBudget is a persisted time-windowed compute budget: a trailing
// WindowSecs window and a Limits map of resource name -> resource-hours
// allowed over it.
type StoredBudget struct {
	WindowSecs uint64             `json:"window_secs"`
	Limits     map[string]float64 `json:"limits"`
}

// storedBudgetAlias breaks the recursion MarshalJSON would otherwise cause
// by re-entering StoredBudget's own MarshalJSON.
type storedBudgetAlias StoredBudget

// MarshalJSON substitutes an empty map for a nil Limits, mirroring Rust's
// BTreeMap::default() (via #[serde(default)]).
func (b StoredBudget) MarshalJSON() ([]byte, error) {
	a := storedBudgetAlias(b)
	if a.Limits == nil {
		a.Limits = map[string]float64{}
	}
	return json.Marshal(a)
}

// --- Scoped role assignments ---

// RoleAssignment is a scoped role assignment: Principal (the Identity
// subject) holds Role at Scope, where scope is "*" (global) or
// "project:<name>". Assignments are additive grants on top of the
// group-derived roles; there are no deny rules.
type RoleAssignment struct {
	Principal string `json:"principal"`
	// Role is the wire form of the auth package's Role ("viewer" |
	// "developer" | "operator" | "admin" | "auditor") — the store stays
	// free of the auth package.
	Role string `json:"role"`
	// Scope is "*" or "project:<name>".
	Scope string `json:"scope"`
	// CreatedAt is unix seconds when the assignment was first written.
	CreatedAt uint64 `json:"created_at"`
}

// --- Transactional outbox (idempotent actuation fencing) ---

// IntentStatus is the lifecycle of an outbox intent. A Pending row left
// behind by a crash between BeginIntent and CompleteIntent tells recovery
// the previous apply may not have finished; Applied means it committed and
// a response was stored.
type IntentStatus int

const (
	IntentStatusPending IntentStatus = iota
	IntentStatusApplied
)

// IntentRecord is a persisted outbox row: what we were about to actuate
// (Key), the spec fingerprint we actuated, the completion status, and the
// stored provider response (opaque JSON so the store stays decoupled from
// provider types).
type IntentRecord struct {
	Key               string
	ParamsFingerprint string
	Status            IntentStatus
	ResponseJSON      *string
	CreatedAt         uint64
	CompletedAt       *uint64
}

// IntentOutcomeKind discriminates the result of opening an intent before
// actuating.
type IntentOutcomeKind int

const (
	// IntentOutcomeProceed: safe to actuate.
	IntentOutcomeProceed IntentOutcomeKind = iota
	// IntentOutcomeParamMismatch: the key already exists with a
	// *different* fingerprint — a stale or conflicting generation write.
	// The caller must refuse to actuate.
	IntentOutcomeParamMismatch
)

// IntentOutcome is the result of BeginIntent (the ADR-0007 fence in the
// Rust reference).
type IntentOutcome struct {
	Kind IntentOutcomeKind
	// Replay is true when Kind == IntentOutcomeProceed and a
	// matching-params row already existed (a crash-recovery or drift
	// re-apply of the *same* desired state) — the caller still applies,
	// because the provider call is idempotent per key and drift repair
	// depends on re-applying. Meaningless when Kind is
	// IntentOutcomeParamMismatch.
	Replay bool
}

// --- Local-auth lockout policy ---

// LoginLockoutThreshold is the number of consecutive failed logins after
// which an account locks for LockoutSecs.
const LoginLockoutThreshold uint32 = 5

// LockoutSecs is the lockout duration in seconds (5 minutes).
const LockoutSecs uint64 = 300

// NextLoginFailureState is the pure lockout state machine, shared by every
// Store implementation's RecordLoginFailure: one more failure increments
// the counter; crossing LoginLockoutThreshold resets the counter and locks
// the account until now + LockoutSecs.
func NextLoginFailureState(failedLogins uint32, now uint64) (uint32, *uint64) {
	failed := failedLogins + 1
	if failed >= LoginLockoutThreshold {
		locked := now + LockoutSecs
		return 0, &locked
	}
	return failed, nil
}

// --- Audit tamper-evidence (hash chain) ---
//
// The audit trail is hash-chained: every appended row carries a
// chain_hash = sha256 over (previous row's chain_hash || this row's
// canonical serialization). A single chain_hash column suffices — the
// previous row's chain_hash IS the prev_hash. The genesis row chains from
// AuditGenesisHash. This is tamper-EVIDENCE, not tamper-proofing: there is
// no secret key, so an attacker with write access to the table can
// recompute the chain — but any edit, insert, or delete of a middle row
// breaks every later hash, which chain verification detects. See
// docs/adr/0004-audit-chain-format.md for the exact byte construction and
// why no migration path exists.

// AuditGenesisHash is the chain head the very first audit row (seq 1)
// chains from: 64 zero hex chars. Fixed-length like every other hash in
// the chain, so the hash input's concatenation is unambiguous without a
// separator.
const AuditGenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ChainedAuditRow is one audit row as the chain sees it: (Seq, Event,
// ChainHash). Go equivalent of Rust's `type ChainedAuditRow = (u64,
// AuditEvent, String)` tuple.
type ChainedAuditRow struct {
	Seq       uint64
	Event     core.AuditEvent
	ChainHash string
}

// AuditChainWindow is a window of the audit chain, ascending by Seq, for
// verification.
type AuditChainWindow struct {
	// Head is the hash the first row in Rows must chain from:
	// AuditGenesisHash when the window starts at the beginning of the
	// trail, else the newest row before the window's chain hash.
	Head string
	// Rows are (seq, event, chain_hash), ascending by seq.
	Rows []ChainedAuditRow
}

// AuditChainHash is the chain hash of a row: sha256 hex over (prevHash ‖
// canonical event serialization). The canonical serialization is Go's
// encoding/json over core.AuditEvent (docs/adr/0004): struct field order
// is fixed by declaration and every field marshals (pointer fields as
// explicit null), so every store implementation — and the verifier —
// produces byte-identical input. prevHash is always 64 lowercase hex
// chars (genesis included), making the concatenation unambiguous.
func AuditChainHash(prevHash string, event *core.AuditEvent) string {
	canonical, err := json.Marshal(event)
	if err != nil {
		// MarshalJSON over core.AuditEvent's plain-data alias (see
		// AuditEvent.MarshalJSON in internal/core/audit.go) cannot fail;
		// this panic mirrors Rust's `.expect("AuditEvent is plain data;
		// serialization cannot fail")`.
		panic(fmt.Sprintf("controller: AuditEvent canonical serialization failed: %v", err))
	}
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// AuditChainVerification is the result of replaying a chain window (see
// VerifyAuditChain).
type AuditChainVerification struct {
	// EventsChecked is rows that verified before the replay stopped (all
	// rows on success; the rows *before* the broken one on failure).
	EventsChecked uint64
	// FirstBrokenSeq is the seq of the first row whose stored chain hash
	// doesn't match the replay; nil when the whole window verifies.
	FirstBrokenSeq *uint64
}

// OK reports whether the whole window verified.
func (v AuditChainVerification) OK() bool {
	return v.FirstBrokenSeq == nil
}

// VerifyAuditChain replays a chain window: recomputes each row's hash from
// its predecessor (starting at head) and compares against the stored
// chain hash. Stops at the first mismatch — everything after a broken
// link is untrustworthy by construction. Pure and shared by every store
// backend and the future /api/v1/audit/verify handler (Task 12).
func VerifyAuditChain(head string, rows []ChainedAuditRow) AuditChainVerification {
	prev := head
	var checked uint64
	for _, row := range rows {
		if AuditChainHash(prev, &row.Event) != row.ChainHash {
			seq := row.Seq
			return AuditChainVerification{EventsChecked: checked, FirstBrokenSeq: &seq}
		}
		prev = row.ChainHash
		checked++
	}
	return AuditChainVerification{EventsChecked: checked, FirstBrokenSeq: nil}
}

// --- Store interface ---

// AuditRow is one (seq, event) pair returned by ListAudit — the Go
// equivalent of Rust's `(u64, AuditEvent)` tuple.
type AuditRow struct {
	Seq   uint64
	Event core.AuditEvent
}

// Store is the desired-state store (ported field-for-field and
// method-for-method from mobula-controller's `Store` trait, store.rs).
// Postgres is truth in production; SQLite serves dev; the in-memory
// implementation in this file exists so the reconcile engine and API
// handlers are testable without a database.
//
// Every method takes a context.Context first (Go's substitute for Rust's
// implicit async task cancellation) and returns (T, error), the idiomatic
// Go shape for Rust's Result<T, StoreError>.
type Store interface {
	// UpsertDesired creates or updates a cluster's desired spec. Returns
	// the (possibly bumped) generation. Generation only advances when the
	// spec actually changes.
	UpsertDesired(ctx context.Context, id core.ClusterId, spec core.ClusterSpec) (uint64, error)

	Get(ctx context.Context, id core.ClusterId) (*StoredCluster, error)
	List(ctx context.Context) ([]StoredCluster, error)

	// SetDesired flips desired state (e.g. request termination).
	SetDesired(ctx context.Context, id core.ClusterId, desired DesiredState) error

	// RemoveCluster hard-deletes a cluster row (tombstone purge). Unlike
	// SetDesired(DesiredTerminated), which only records intent, this
	// removes the row entirely. The caller is responsible for confirming
	// the cluster is already terminated/gone before removing it. Returns
	// true if a row was removed, false if none existed.
	RemoveCluster(ctx context.Context, id core.ClusterId) (bool, error)

	// RecordObservation records the reconstructed observation and the
	// generation it reflects. The stored observed generation is monotonic
	// non-decreasing: an observation reporting an *older* generation than
	// what's stored does not roll it back.
	RecordObservation(ctx context.Context, id core.ClusterId, observed *core.ClusterState, observedGeneration uint64) error

	// SetCondition sets (or clears) the drift/health condition on a
	// cluster.
	SetCondition(ctx context.Context, id core.ClusterId, condition *core.DriftCondition) error

	// IsQuarantined reports whether the control plane is quarantined: a
	// stale-restore boot check trips this, and while set the reconcile
	// engine observes but never actuates until an operator clears it.
	IsQuarantined(ctx context.Context) (bool, error)
	// SetQuarantine enters or leaves quarantine.
	SetQuarantine(ctx context.Context, quarantined bool) error

	// RecordAttempt persists a cluster's backoff state after a reconcile
	// attempt: failureCount consecutive no-progress attempts and the unix
	// time before which not to re-actuate. Both 0 clears the backoff.
	RecordAttempt(ctx context.Context, id core.ClusterId, failureCount uint32, nextAttemptAt uint64) error

	// BeginIntent opens a transactional-outbox intent to actuate key with
	// the given spec fingerprint, committing a Pending row *before* the
	// provider call. Returns IntentOutcomeProceed when the caller should
	// actuate (fresh, or a same-params re-apply), or
	// IntentOutcomeParamMismatch when the key already exists with a
	// different fingerprint (reject — stale/conflicting generation).
	BeginIntent(ctx context.Context, key, fingerprint string) (IntentOutcome, error)
	// CompleteIntent marks an opened intent Applied and stores the
	// provider responseJSON (opaque). Called after a successful provider
	// actuation.
	CompleteIntent(ctx context.Context, key, responseJSON string) error
	// GetIntent reads an outbox row (crash-recovery / audit / tests).
	GetIntent(ctx context.Context, key string) (*IntentRecord, error)
	// ReapIntents bounds outbox growth: deletes Applied rows whose
	// CompletedAt is older than appliedBefore. Returns how many were
	// removed.
	ReapIntents(ctx context.Context, appliedBefore uint64) (uint64, error)

	// RecordJob records or updates a job in the persistent history, keyed
	// by job id. Job records live independently of clusters, so they
	// survive the deletion of the cluster that ran them.
	RecordJob(ctx context.Context, job core.JobRecord) error
	// ListJobs lists job history, most recently submitted first.
	ListJobs(ctx context.Context) ([]core.JobRecord, error)

	// UpsertPool creates or updates a pool's spec. Returns the (possibly
	// bumped) generation — like clusters, generation only advances when
	// the spec actually changes.
	UpsertPool(ctx context.Context, name string, spec core.PoolSpec) (uint64, error)
	GetPool(ctx context.Context, name string) (*StoredPool, error)
	ListPools(ctx context.Context) ([]StoredPool, error)
	// DeletePool hard-deletes a pool. Errors naming the missing pool when
	// it does not exist.
	DeletePool(ctx context.Context, name string) error
	// RecordPoolObservation records the pool reconcile loop's last
	// observation of a pool's Kueue ClusterQueue status (opaque JSON).
	// Overwrites on every pass; recorded only when the observe succeeded.
	RecordPoolObservation(ctx context.Context, name, observedJSON string) error

	// UpsertAllocation creates or updates a project's allocation within a
	// pool (keyed by (pool, project)). Allocations are part of the pool's
	// desired state.
	UpsertAllocation(ctx context.Context, alloc core.AllocationSpec) error
	// ListAllocations lists the allocations of one pool.
	ListAllocations(ctx context.Context, pool string) ([]core.AllocationSpec, error)
	// DeleteAllocation deletes one allocation. Errors naming the missing
	// (pool, project) when it does not exist.
	DeleteAllocation(ctx context.Context, pool, project string) error

	// RecordUsageSamples appends usage samples. Append-only timeseries —
	// the metering loop writes, nothing updates or deletes individual
	// rows.
	RecordUsageSamples(ctx context.Context, samples []UsageSample) error
	// UsageSamples reads usage samples in [from, to] (unix seconds,
	// inclusive), ordered by ts ascending. project/pool filter when
	// non-nil.
	UsageSamples(ctx context.Context, project, pool *string, from, to uint64) ([]UsageSample, error)

	// GetPolicy reads the persisted governance policy; nil when no policy
	// row exists (never seeded, never edited).
	GetPolicy(ctx context.Context) (*StoredPolicy, error)
	// SetPolicy overwrites the governance policy row (the settings PUT
	// path).
	SetPolicy(ctx context.Context, policy *StoredPolicy) error
	// SeedPolicy inserts the --policy boot seed ONLY when no policy row
	// exists (insert-if-absent, so a concurrent edit or seeder is never
	// clobbered). Returns true when this call inserted the row. Backends
	// must implement this atomically (a single conditional INSERT), not
	// as get+set.
	SeedPolicy(ctx context.Context, policy *StoredPolicy) (bool, error)

	// RecordAudit appends an audit event. Append-only: returns the row's
	// seq, a 1-based monotonic sequence number that doubles as the
	// pagination cursor for ListAudit. Callers must treat a failure as
	// non-fatal (log and continue) — audit persistence never fails the
	// request being audited.
	RecordAudit(ctx context.Context, event *core.AuditEvent) (uint64, error)
	// ListAudit lists audit events matching filter, newest-first by seq
	// (descending). filter.Cursor selects only rows with seq < cursor;
	// the page holds at most filter.EffectiveLimit() rows. The returned
	// next cursor is non-nil (seq of the oldest row in the page) when
	// more matching rows exist beyond it, nil at the end.
	ListAudit(ctx context.Context, filter core.AuditFilter) ([]AuditRow, *uint64, error)
	// AuditChain reads the audit chain in ASCENDING seq order for
	// verification: rows with seq >= fromSeq (the whole trail when nil),
	// at most limit. The window's Head is the hash the first row must
	// chain from.
	AuditChain(ctx context.Context, fromSeq *uint64, limit uint32) (AuditChainWindow, error)

	// --- Local auth ---

	// CreateLocalUser creates a local user. The store receives the bcrypt
	// password hash, never plaintext. createdAt is stamped by the store;
	// lockout counters start cleared. Errors when the username already
	// exists.
	CreateLocalUser(ctx context.Context, username string, email *string, passwordHash string, role core.LocalRole) error
	// GetLocalUser reads a local user row (including the password hash —
	// the caller is the auth layer, which must never serialize it).
	GetLocalUser(ctx context.Context, username string) (*core.LocalUserRecord, error)
	// ListLocalUsers lists all local users, ordered by username.
	ListLocalUsers(ctx context.Context) ([]core.LocalUserRecord, error)
	// SetLocalUserPassword replaces a user's bcrypt password hash. Errors
	// naming the missing user when it does not exist.
	SetLocalUserPassword(ctx context.Context, username, passwordHash string) error
	// SetLocalUserRole changes a user's role (resolved per request, so
	// this applies to the very next authenticated call). Errors naming
	// the missing user.
	SetLocalUserRole(ctx context.Context, username string, role core.LocalRole) error
	// SetLocalUserDisabled disables or re-enables a user. Disabled users
	// cannot log in and their existing tokens stop authenticating. Errors
	// naming the missing user.
	SetLocalUserDisabled(ctx context.Context, username string, disabled bool) error
	// SetLoginLockout persists the lockout counters. Backend hook for the
	// shared RecordLoginFailure/RecordLoginSuccess implementations;
	// errors naming the missing user.
	SetLoginLockout(ctx context.Context, username string, failedLogins uint32, lockedUntil *uint64) error
	// RecordLoginFailure records a failed login: increments the counter,
	// and when it crosses LoginLockoutThreshold locks the account for
	// LockoutSecs and resets the counter. The decision lives in
	// NextLoginFailureState so every backend shares semantics.
	RecordLoginFailure(ctx context.Context, username string) error
	// RecordLoginSuccess records a successful login: clears the failure
	// counter and any lock.
	RecordLoginSuccess(ctx context.Context, username string) error

	// CreateApiToken stores an opaque API token. record carries the
	// bcrypt token hash; the plaintext is shown once at issuance and
	// never stored. Errors when the prefix collides.
	CreateApiToken(ctx context.Context, record core.ApiTokenRecord) error
	// GetApiTokenByPrefix looks a token up by its 8-char prefix
	// (including the hash — the caller is the auth layer, which must
	// never serialize it).
	GetApiTokenByPrefix(ctx context.Context, prefix string) (*core.ApiTokenRecord, error)
	// ListApiTokens lists one user's tokens, newest first. Never returns
	// hashes to the wire — callers project to ApiTokenView.
	ListApiTokens(ctx context.Context, username string) ([]core.ApiTokenRecord, error)
	// RevokeApiToken revokes a token, owner-scoped: revoking someone
	// else's token (or a nonexistent one) errors as "no such token" so
	// ownership can't be probed. Idempotent for an already-revoked own
	// token.
	RevokeApiToken(ctx context.Context, prefix, username string) error
	// TouchApiToken is a best-effort last-used-at stamp on a successful
	// token authentication. Never fails the request being authenticated.
	TouchApiToken(ctx context.Context, prefix string, now uint64) error

	// --- Scoped role assignments ---

	// UpsertRoleAssignment creates or replaces a scoped role assignment,
	// keyed by (principal, role, scope). createdAt is stamped by the
	// store on insert and preserved on re-upsert. Validation of the role
	// name and scope grammar is the API layer's job — the store is dumb
	// persistence.
	UpsertRoleAssignment(ctx context.Context, principal, role, scope string) error
	// ListRoleAssignments lists assignments, ordered by (principal,
	// scope, role). principal, when non-nil, filters to one subject —
	// the per-request authz lookup path.
	ListRoleAssignments(ctx context.Context, principal *string) ([]RoleAssignment, error)
	// DeleteRoleAssignment removes one assignment. Errors naming the
	// missing (principal, role, scope) when it does not exist.
	DeleteRoleAssignment(ctx context.Context, principal, role, scope string) error
}
