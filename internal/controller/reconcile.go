// Observation-first reconcile engine (ADR-0006/ADR-0007-equivalent).
//
// Level-triggered: every pass reconstructs each cluster's state from the
// provisioner (never trusts a stored phase), compares it to desired, and
// actuates the difference through an idempotency-keyed provisioner call. It
// is safe to run on a fixed resync interval and safe to re-run after a
// crash — repeating an actuation with the same desired generation is a
// no-op at the provider.
//
// Ported from mobula-controller/src/reconcile.rs (Rust reference, retired
// project — cited here only where a file:line reference is genuinely
// useful).
//
// # Lifecycle reaping (#100)
//
// [Reconciler.ReapExpired] enforces two independent lifecycle bounds on a
// running cluster:
//
//   - max-age (TtlSeconds) — reaped this long after creation regardless of
//     activity (the absolute cap); and
//   - activity-idle (IdleTimeoutSecs) — reaped once it has been idle for
//     the window, so a busy cluster survives past it while an unused one is
//     released. A pure max-age TTL kills a cluster mid-use, hence this
//     second bound.
//
// Activity signal: idleness is derived from the persisted job history
// (Store.ListJobs) — a cluster with a running/pending job is busy now, and
// a finished job counts as activity through its end time; creation is the
// floor. This is the cheapest robust signal already in the store.
//
// Limitation — interactive sessions: interactive Ray Client / Dask sessions
// submit no gateway jobs, so job history is empty for them and their
// derived activity never advances past creation. An interactive-only
// cluster therefore looks idle from birth: IdleTimeoutSecs would reap it
// even while actively used. For such sessions leave IdleTimeoutSecs unset
// (max-age-only) or rely on TtlSeconds.
//
// # Known limitation carried from Task 1 (ledgered, see
// docs/superpowers/sdd/2026-08-31-wave-1-critical-parity/task-9-report.md)
//
// specChanged (store.go) deliberately skips Engine/Owner when deciding
// whether to bump a cluster's generation — Rust parity, confirmed against
// mobula-controller/src/store.rs's spec_changed, which skips the same two
// fields. Because paramsFingerprint (below) hashes the FULL spec —
// Engine and Owner included, matching Rust's params_fingerprint, which
// does the same over the whole ClusterSpec — an edit that changes only
// Engine or Owner changes the fingerprint under the existing
// {id}/{generation} outbox key WITHOUT bumping Generation. The next
// actuating pass's BeginIntent call then finds that key already holds a
// DIFFERENT fingerprint than before the edit and returns
// IntentOutcomeParamMismatch, so reconcileOne returns
// ReconcileErrStaleIntent — every pass, from then on, permanently: the
// key never changes (Generation is stuck) and the fingerprint mismatch
// never resolves on its own. This is not merely "the edit is never
// re-actuated" — it is a permanent StaleIntent wedge on that cluster.
// Reproduced faithfully from the Rust reference (the same
// params_fingerprint/spec_changed field-set mismatch exists there, so
// this is Rust-parity, not a Go-only regression). Known, accepted
// limitation — not a bug in this file.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// IntentRetentionSecs is how long an Applied outbox row is retained before
// the run loop reaps it (ADR-0007-equivalent, #39). Kept well beyond a few
// resync intervals so crash recovery can still inspect recent intents, but
// bounded so the table can't grow one row per (cluster, generation)
// forever.
const IntentRetentionSecs uint64 = 3600

// TerminatedRetentionSecs is the default window a terminated cluster's
// tombstone row is retained before the run loop hard-deletes it (Truthful
// Console). Generous by design: an operator and the dashboard have a full
// day to see that a cluster died before its row disappears from
// GET /api/v1/clusters.
const TerminatedRetentionSecs uint64 = 24 * 3600

// BackoffBaseSecs and BackoffCeilSecs bound the exponential-backoff delay
// for a no-progress cluster (#43).
const (
	BackoffBaseSecs uint64 = 5
	BackoffCeilSecs uint64 = 300
)

// DefaultReconcileInterval is the resync interval RunReconciler/
// RunPoolReconciler use when Options.Interval is unset (<= 0).
const DefaultReconcileInterval = 30 * time.Second

// backoffSecs is the delay before re-actuating a cluster that has made no
// progress for failureCount consecutive attempts (#43): base * 2^(n-1),
// capped. Mirrors Rust's saturating arithmetic so a saturated failureCount
// can't overflow the shift or the product.
func backoffSecs(failureCount uint32) uint64 {
	shift := failureCount
	if shift > 0 {
		shift--
	}
	if shift > 20 {
		shift = 20
	}
	v := BackoffBaseSecs * (uint64(1) << shift)
	if v > BackoffCeilSecs {
		v = BackoffCeilSecs
	}
	return v
}

// --- Saturating arithmetic helpers (Go has no built-in saturating_add/sub) ---

func satAddU32(a, b uint32) uint32 {
	s := a + b
	if s < a {
		return ^uint32(0)
	}
	return s
}

func satAddU64(a, b uint64) uint64 {
	s := a + b
	if s < a {
		return ^uint64(0)
	}
	return s
}

func satSubU64(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// --- Rate limiting (ADR-0006-equivalent token bucket, #43) ---

// RateLimits is the global actuation rate limit across all clusters: a
// burst of failing clusters can't exceed the provider-call budget.
type RateLimits struct {
	// Capacity is the maximum actuations available in a burst.
	Capacity float64
	// RefillPerSec is tokens replenished per second.
	RefillPerSec float64
}

// tokenBucket is a time-based token bucket keyed on the reconcile `now`
// (unix secs), so it is deterministic in tests. Only actuating passes
// (apply) take a token.
type tokenBucket struct {
	mu           sync.Mutex
	tokens       float64
	capacity     float64
	refillPerSec float64
	last         uint64
}

func newTokenBucket(limits RateLimits, now uint64) *tokenBucket {
	return &tokenBucket{
		tokens:       limits.Capacity,
		capacity:     limits.Capacity,
		refillPerSec: limits.RefillPerSec,
		last:         now,
	}
}

func (b *tokenBucket) tryTake(now uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	elapsed := float64(satSubU64(now, b.last))
	tokens := b.tokens + elapsed*b.refillPerSec
	if tokens > b.capacity {
		tokens = b.capacity
	}
	b.tokens = tokens
	b.last = now
	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true
	}
	return false
}

// --- Errors ---

// ReconcileErrorKind discriminates ReconcileError variants — the Go
// analogue of reconcile.rs's ReconcileError thiserror enum.
type ReconcileErrorKind int

const (
	// ReconcileErrStore: a Store call failed. Source is the underlying
	// StoreError.
	ReconcileErrStore ReconcileErrorKind = iota
	// ReconcileErrProvision: a Provisioner call failed. Source is the
	// underlying provision.ProvisionError.
	ReconcileErrProvision
	// ReconcileErrStaleIntent: the outbox already holds this idempotency
	// key with a different spec fingerprint — a stale or conflicting
	// generation write (ADR-0007-equivalent). Key names the offending
	// outbox key.
	ReconcileErrStaleIntent
)

// ReconcileError is a value-typed error mirroring reconcile.rs's
// ReconcileError, matching this codebase's established error pattern
// (value types with a Kind discriminant and an Unwrap() chain to the
// wrapped source, e.g. core.PoolSpecError).
type ReconcileError struct {
	Kind ReconcileErrorKind
	// Source is the wrapped StoreError or provision.ProvisionError, set
	// for ReconcileErrStore / ReconcileErrProvision respectively.
	Source error
	// Key is the stale/conflicting outbox key, set for
	// ReconcileErrStaleIntent.
	Key string
}

func (e ReconcileError) Error() string {
	switch e.Kind {
	case ReconcileErrStore, ReconcileErrProvision:
		return e.Source.Error()
	case ReconcileErrStaleIntent:
		return fmt.Sprintf("stale/conflicting intent for key %s: spec fingerprint mismatch", e.Key)
	}
	return "reconcile error"
}

// Unwrap exposes the wrapped Store/Provision error so errors.As/errors.Is
// can reach it — mirroring Rust's thiserror #[from] fields. The
// StaleIntent variant carries no source and unwraps to nil.
func (e ReconcileError) Unwrap() error {
	switch e.Kind {
	case ReconcileErrStore, ReconcileErrProvision:
		return e.Source
	case ReconcileErrStaleIntent:
		return nil
	}
	return nil
}

func wrapStoreErr(err error) error {
	if err == nil {
		return nil
	}
	return ReconcileError{Kind: ReconcileErrStore, Source: err}
}

func wrapProvisionErr(err error) error {
	if err == nil {
		return nil
	}
	return ReconcileError{Kind: ReconcileErrProvision, Source: err}
}

func isProvisionNotFound(err error) bool {
	pe, ok := err.(provision.ProvisionError)
	return ok && pe.Kind == provision.ProvisionErrNotFound
}

// --- Per-cluster outcome ---

// Action is the per-cluster outcome of a reconcile pass, for
// logging/metrics/tests.
type Action int

const (
	// ActionNoOp: desired and observed already agree.
	ActionNoOp Action = iota
	// ActionApplied: applied desired spec (create or update).
	ActionApplied
	// ActionTerminated: requested teardown.
	ActionTerminated
	// ActionSuspended: suspended the cluster (#51): compute released,
	// spec and store state kept.
	ActionSuspended
	// ActionDrift: observed divergence that re-applying can't fix
	// (Degraded, or an out-of-band spec edit) — raised as an alarm, not
	// silently converged (ADR-0004-equivalent, #41/#47).
	ActionDrift
	// ActionBackoff: skipped actuation this pass: the cluster is inside
	// its backoff window or the global rate-limit budget is exhausted
	// (#43). Retried next tick.
	ActionBackoff
)

// String renders the action for logs.
func (a Action) String() string {
	switch a {
	case ActionNoOp:
		return "no_op"
	case ActionApplied:
		return "applied"
	case ActionTerminated:
		return "terminated"
	case ActionSuspended:
		return "suspended"
	case ActionDrift:
		return "drift"
	case ActionBackoff:
		return "backoff"
	}
	return "unknown"
}

// ReconcileResult is one cluster's outcome from a reconcile pass — the Go
// equivalent of Rust's (String, Result<Action, ReconcileError>) tuple.
// Unlike Rust's Result, which carries no Action at all in its Err variant,
// this struct's fields are always both present: when Err != nil, Action is
// always its zero value (ActionNoOp) — reconcileOne returns (0, err) on
// every error path — so it carries no information and callers must check
// Err before consuming Action, exactly as a Rust caller must match on
// Result::Err before an Ok(Action) is even reachable.
type ReconcileResult struct {
	ID     string
	Action Action
	Err    error
}

// --- Reconciler ---

// Reconciler is the cluster reconcile engine.
type Reconciler struct {
	store       Store
	provisioner provision.Provisioner
	// bucket is the global actuation token bucket (#43); nil = unlimited.
	bucket                  *tokenBucket
	terminatedRetentionSecs uint64
	// metering (requirement 14): how often Meter runs from the tick, when
	// it last ran, and which clusters were last seen running (so a cluster
	// leaving running gets one closing zero sample).
	meteringInterval time.Duration
	lastMeter        time.Time
	metered          map[core.ClusterId]bool
	// Requirement 5 seams (see Options): carried by the reconciler so the
	// RayJob loop can register the clusters it brings up with the gateway.
	// Accepted and stored; nothing reads them until that loop lands.
	registrar       Registrar
	gatewayHostname func(core.ClusterId) string
	jobProvisioner  provision.JobProvisioner
}

// NewReconciler returns a Reconciler with no global rate cap (per-cluster
// backoff still applies).
func NewReconciler(store Store, provisioner provision.Provisioner) *Reconciler {
	return &Reconciler{
		store:                   store,
		provisioner:             provisioner,
		terminatedRetentionSecs: TerminatedRetentionSecs,
		meteringInterval:        DefaultMeteringInterval,
		metered:                 map[core.ClusterId]bool{},
	}
}

// NewReconcilerWithLimits returns a Reconciler with a global actuation rate
// limit (#43).
func NewReconcilerWithLimits(store Store, provisioner provision.Provisioner, limits RateLimits) *Reconciler {
	return &Reconciler{
		store:                   store,
		provisioner:             provisioner,
		bucket:                  newTokenBucket(limits, 0),
		terminatedRetentionSecs: TerminatedRetentionSecs,
		meteringInterval:        DefaultMeteringInterval,
		metered:                 map[core.ClusterId]bool{},
	}
}

// WithTerminatedRetention overrides the terminated-tombstone retention
// window (Truthful Console). Default is TerminatedRetentionSecs. Returns r
// for chaining.
func (r *Reconciler) WithTerminatedRetention(secs uint64) *Reconciler {
	r.terminatedRetentionSecs = secs
	return r
}

// takeToken takes one actuation token, or true when unlimited. false means
// the budget is exhausted this pass — skip actuating (retry next tick).
func (r *Reconciler) takeToken(now uint64) bool {
	if r.bucket == nil {
		return true
	}
	return r.bucket.tryTake(now)
}

// ReconcileAll reconciles every known cluster once at the current
// wall-clock time.
func (r *Reconciler) ReconcileAll(ctx context.Context) []ReconcileResult {
	return r.ReconcileAllAt(ctx, NowUnix())
}

// ReconcileAllAt reconciles every known cluster once at time now (unix
// secs). now is injected so backoff/rate-limit decisions (#43) are
// deterministic in tests. Errors on individual clusters are collected, not
// fatal — one bad cluster must not stall the loop.
func (r *Reconciler) ReconcileAllAt(ctx context.Context, now uint64) []ReconcileResult {
	clusters, err := r.store.List(ctx)
	if err != nil {
		return []ReconcileResult{{ID: "<list>", Err: wrapStoreErr(err)}}
	}
	out := make([]ReconcileResult, 0, len(clusters))
	for i := range clusters {
		c := &clusters[i]
		action, err := r.reconcileOne(ctx, c, now)
		out = append(out, ReconcileResult{ID: c.ID.String(), Action: action, Err: err})
	}
	return out
}

func (r *Reconciler) reconcileOne(ctx context.Context, c *StoredCluster, now uint64) (Action, error) {
	// Backoff gate (#43): a Running-desired cluster that has made no
	// progress is left untouched — not even observed — until its
	// next-attempt time, so a permanently-failing cluster can't hammer
	// the provider every tick.
	if c.Desired == DesiredRunning && now < c.NextAttemptAt {
		return ActionBackoff, nil
	}

	// 1. Observe: reconstruct actual state (ADR-0006-equivalent). A
	//    NotFound means nothing is provisioned yet — model that as no
	//    observed state. observedGen is the generation the cluster
	//    actually carries (read back), never the desired one (#40).
	observed, obsErr := r.provisioner.Observe(ctx, c.ID)
	var observedState *core.ClusterState
	var observedGen uint64
	var observedFP *string
	switch {
	case obsErr == nil:
		st := observed.State
		observedState = &st
		if observed.ObservedGeneration != nil {
			observedGen = *observed.ObservedGeneration
		}
		observedFP = observed.SpecFingerprint
	case isProvisionNotFound(obsErr):
		// Nothing provisioned yet; observedState stays nil.
	default:
		return 0, wrapProvisionErr(obsErr)
	}

	// Quarantine (ADR-0007 restore fence-equivalent, #41): while set,
	// observe and record but never actuate — an operator clears it after
	// reviewing a suspected stale DB restore.
	quarantined, err := r.store.IsQuarantined(ctx)
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	if quarantined {
		slog.Warn("control plane quarantined: observing only, not actuating",
			"target", "bifrost::audit", "cluster", c.ID.String())
		if err := r.store.RecordObservation(ctx, c.ID, observedState, observedGen); err != nil {
			return 0, wrapStoreErr(err)
		}
		return ActionNoOp, nil
	}

	// 2. Decide and actuate against *observed* reality. Track the
	//    drift/health condition to persist (#41/#47); every branch sets
	//    it.
	//
	// Resolve the Kueue queue assignment for the cluster's project
	// (ADR-0010-equivalent) from the store — the store is the transport
	// between cluster-create's allocation lookup and actuation, so
	// ClusterSpec's serialized form stays free of it. A queued cluster's
	// suspend is owned by Kueue, so for queued clusters Suspended is
	// admission queueing, not repairable drift (see needsApply).
	queue, err := queueAssignmentForProject(ctx, r.store, c.Spec.Project)
	if err != nil {
		return 0, err
	}

	var newCondition *core.DriftCondition
	var action Action
	switch c.Desired {
	case DesiredRunning:
		switch {
		case observedState != nil && *observedState == core.ClusterStateDegraded:
			// #47: Degraded is a runtime failure, not spec drift —
			// re-applying the unchanged spec can't heal it and would
			// hot-loop. Alarm instead (ADR-0004-equivalent), leave it
			// Degraded.
			d := core.DriftConditionDegraded
			newCondition = &d
			slog.Warn("observed Degraded while desired Running — raising alarm, not re-applying",
				"target", "bifrost::audit", "cluster", c.ID.String())
			action = ActionDrift
		case needsApply(observedState, observedGen, c.Generation, queue != nil):
			// Global actuation budget (#43): if the token bucket is
			// empty, defer this cluster to a later tick rather than
			// exceed the provider-call rate. NoOp/observe passes don't
			// take a token, so only real actuation is capped.
			if !r.takeToken(now) {
				return ActionBackoff, nil
			}
			// None/Terminated/Terminating/Suspended(#47)/generation-
			// behind → (re)apply. Transactional outbox
			// (ADR-0007-equivalent, #39): open the intent before the
			// call; a same-params re-open (replay) still actuates
			// (idempotent SSA; drift repair needs it), a
			// different-params re-use is rejected.
			key := c.IntentKey()
			fp := paramsFingerprint(&c.Spec)
			outcome, err := r.store.BeginIntent(ctx, key, fp)
			if err != nil {
				return 0, wrapStoreErr(err)
			}
			if outcome.Kind == IntentOutcomeParamMismatch {
				return 0, ReconcileError{Kind: ReconcileErrStaleIntent, Key: key}
			}
			// #56/#62: namespace security posture (default-deny
			// NetworkPolicy + PSS labels) is per-namespace, not
			// per-cluster — ensure it with each actuating apply.
			// Fail-closed: a posture error blocks the cluster apply.
			if err := r.provisioner.EnsureNamespacePosture(ctx); err != nil {
				return 0, wrapProvisionErr(err)
			}
			resp, err := r.provisioner.Apply(ctx, c.ID, &c.Spec, c.Generation, key, queue)
			if err != nil {
				return 0, wrapProvisionErr(err)
			}
			respJSON, jerr := json.Marshal(resp)
			if jerr != nil {
				respJSON = []byte("")
			}
			if err := r.store.CompleteIntent(ctx, key, string(respJSON)); err != nil {
				return 0, wrapStoreErr(err)
			}
			if outcome.Replay {
				slog.Debug("re-applied existing intent (drift/replay)", "cluster", c.ID.String(), "key", key)
			}
			newCondition = nil
			action = ActionApplied
		default:
			// Live at the desired generation: check for an out-of-band
			// edit of a Bifrost-owned field (#41). The observed
			// fingerprint is recomputed from the live resource, so a
			// divergence is real drift — alarm, don't silently NoOp.
			// Wave 1 is Ray-only (Dask provisioning is out of scope,
			// task-5-report.md); a future multi-engine EngineRouter
			// (Wave 3) will dispatch this by spec.Engine the same way
			// mobula's router.rs does.
			desiredFP := provision.OwnedSpecFingerprint(&c.Spec)
			if observedFP != nil && *observedFP != desiredFP {
				d := core.DriftConditionSpecDrift
				newCondition = &d
				slog.Warn("observed spec drift from desired — raising alarm",
					"target", "bifrost::audit", "cluster", c.ID.String())
				action = ActionDrift
			} else {
				newCondition = nil
				action = ActionNoOp
			}
		}
	case DesiredTerminated:
		newCondition = nil
		if observedState != nil && *observedState != core.ClusterStateTerminated {
			if err := r.provisioner.Terminate(ctx, c.ID); err != nil {
				return 0, wrapProvisionErr(err)
			}
			action = ActionTerminated
		} else {
			action = ActionNoOp
		}
	case DesiredSuspended:
		// #51: drive the backing cluster to spec.suspend=true. The
		// actuation is a level-triggered, idempotent provisioner call
		// like terminate above — deliberately NOT the generation-keyed
		// apply path: suspension changes no spec field, and the outbox
		// key {id}/{generation} must always map to the same actuation
		// parameters (ADR-0007-equivalent). Resume is the reverse: the
		// API flips desired back to Running and the Running arm's
		// apply (which writes suspend:false) converges it.
		newCondition = nil
		switch {
		case queue != nil:
			// Kueue owns spec.suspend for queue-assigned clusters
			// (ADR-0010-equivalent); the API rejects user
			// suspend/resume there, so this combination should never
			// occur — if it does, never fight the queue.
			action = ActionNoOp
		case observedState != nil && !inSuspendedTerminalGroup(*observedState):
			if err := r.provisioner.Suspend(ctx, c.ID); err != nil {
				return 0, wrapProvisionErr(err)
			}
			action = ActionSuspended
		default:
			// Already suspended, or nothing/gone — nothing to suspend.
			action = ActionNoOp
		}
	}

	// 3. Re-observe and persist status reconstructed from reality, not
	//    from what we intended (ADR-0006-equivalent). Record the
	//    generation the cluster reports, so convergence is observed, not
	//    self-certified.
	finalObs, finalErr := r.provisioner.Observe(ctx, c.ID)
	var finalState *core.ClusterState
	var finalGen uint64
	switch {
	case finalErr == nil:
		st := finalObs.State
		finalState = &st
		if finalObs.ObservedGeneration != nil {
			finalGen = *finalObs.ObservedGeneration
		}
	case isProvisionNotFound(finalErr):
		// finalState stays nil.
	default:
		return 0, wrapProvisionErr(finalErr)
	}
	if err := r.store.RecordObservation(ctx, c.ID, finalState, finalGen); err != nil {
		return 0, wrapStoreErr(err)
	}
	if !driftConditionEqual(newCondition, c.Condition) {
		if err := r.store.SetCondition(ctx, c.ID, newCondition); err != nil {
			return 0, wrapStoreErr(err)
		}
	}
	// Gateway routing (requirement 5's dynamic registry, applied to #6
	// clusters too): a live cluster is registered under its gateway
	// hostname, a dead or dying one deregistered. Derived from the same
	// observation just persisted, so the table rebuilds itself from the
	// first pass after a restart — nothing to persist.
	var finalURL *string
	if finalErr == nil {
		finalURL = finalObs.ApiBaseUrl
	}
	r.syncGatewayRegistration(c, finalState, finalURL)

	// 4. Backoff accounting (#43): after actuating a Running cluster, did
	//    it make progress? A cluster observed back at None/Terminated/
	//    Terminating after an apply is not coming up → bump the failure
	//    count and push out nextAttemptAt. Progress (or a converged
	//    NoOp) clears the backoff.
	var progressed *bool
	switch {
	case action == ActionApplied:
		p := finalState != nil && *finalState != core.ClusterStateTerminated && *finalState != core.ClusterStateTerminating
		progressed = &p
	case action == ActionNoOp && c.Desired == DesiredRunning:
		t := true
		progressed = &t
	}
	switch {
	case progressed != nil && !*progressed:
		failureCount := satAddU32(c.FailureCount, 1)
		nextAttemptAt := satAddU64(now, backoffSecs(failureCount))
		if err := r.store.RecordAttempt(ctx, c.ID, failureCount, nextAttemptAt); err != nil {
			return 0, wrapStoreErr(err)
		}
		slog.Warn("cluster made no progress — backing off",
			"target", "bifrost::audit", "cluster", c.ID.String(),
			"failure_count", failureCount, "retry_in_secs", backoffSecs(failureCount))
	case progressed != nil && *progressed && (c.FailureCount != 0 || c.NextAttemptAt != 0):
		if err := r.store.RecordAttempt(ctx, c.ID, 0, 0); err != nil {
			return 0, wrapStoreErr(err)
		}
	}

	return action, nil
}

// syncGatewayRegistration keeps the gateway registry in step with one
// cluster's observed state: registered (Target jobs, project-scoped) while
// desired running and observed in a routable state with a known API base
// URL; deregistered otherwise. A registration refused by the registry (a
// static hostname collision) is logged, never fatal — routing is a
// convenience layered on the cluster, not a condition of it.
func (r *Reconciler) syncGatewayRegistration(c *StoredCluster, state *core.ClusterState, apiBaseURL *string) {
	if r.registrar == nil || r.gatewayHostname == nil {
		return
	}
	if c.Desired == DesiredRunning && state != nil && apiBaseURL != nil && clusterIsRoutable(*state) {
		err := r.registrar.Register(core.ClusterEndpoint{
			Id:         c.ID,
			Hostname:   r.gatewayHostname(c.ID),
			ApiBaseUrl: *apiBaseURL,
			Project:    c.Spec.Project,
			Target:     core.RegistryTargetJobs,
			Source:     core.RegistrySourceDynamic,
		})
		if err != nil {
			slog.Warn("cluster could not be registered with the gateway",
				"target", "bifrost::audit", "cluster", c.ID.String(), "error", err)
		}
		return
	}
	r.registrar.Deregister(c.ID)
}

// clusterIsRoutable reports whether a cluster in state s has (or is about
// to have) a head worth routing to: coming up, running, degraded or
// rolling. Suspended and terminating clusters have no head.
func clusterIsRoutable(s core.ClusterState) bool {
	switch s {
	case core.ClusterStateProvisioning, core.ClusterStateRunning, core.ClusterStateDegraded, core.ClusterStateUpdating:
		return true
	case core.ClusterStatePending, core.ClusterStateSuspending, core.ClusterStateSuspended,
		core.ClusterStateTerminating, core.ClusterStateTerminated:
		return false
	}
	return false
}

// inSuspendedTerminalGroup reports whether s is one of the states that
// mean "nothing to suspend" (already suspended, or gone).
func inSuspendedTerminalGroup(s core.ClusterState) bool {
	switch s {
	case core.ClusterStateSuspended, core.ClusterStateTerminated, core.ClusterStateTerminating:
		return true
	default: // membership test; not an exhaustive state guard
		return false
	}
}

func driftConditionEqual(a, b *core.DriftCondition) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// --- Lifecycle reaping (#100) ---

// observedGone reports whether a cluster's backing resource is gone (or
// was never observed), so the row is safe to treat as a dead tombstone
// rather than a live cluster (Truthful Console). Shared by the retention
// sweep and (in a later task) the API purge guard.
func observedGone(observed *core.ClusterState) bool {
	return observed == nil || *observed == core.ClusterStateTerminated
}

// ObservedGone is observedGone's exported form, for Wave 1 T11's cluster
// API purge guard (DELETE .../{id}?purge=true), ported from
// mobula-controller's reconcile.rs observed_gone.
func ObservedGone(observed *core.ClusterState) bool {
	return observedGone(observed)
}

// isPurgeableTombstone reports whether a terminated cluster row is a
// purgeable tombstone: desired=Terminated, observed gone, and its
// TerminatedAt stamp is older than the retention window.
func isPurgeableTombstone(c *StoredCluster, now, retentionSecs uint64) bool {
	return c.Desired == DesiredTerminated &&
		observedGone(c.ObservedState) &&
		c.TerminatedAt != nil && satSubU64(now, *c.TerminatedAt) >= retentionSecs
}

// isExpired reports whether a running cluster has a max-age TTL and its
// age exceeds it (the absolute cap — reaped regardless of activity).
func isExpired(c *StoredCluster, now uint64) bool {
	return c.Desired == DesiredRunning &&
		c.ObservedState != nil && *c.ObservedState == core.ClusterStateRunning &&
		c.Spec.TtlSeconds != nil && satSubU64(now, c.CreatedAt) >= *c.Spec.TtlSeconds
}

// jobIsTerminal reports whether a Ray job status means the job is
// finished. Compared case-insensitively and kept as a set here rather than
// in the wire type so a Ray status rename degrades to "treat as
// still-active" (fail-safe: an unknown status keeps the cluster alive)
// rather than mis-reaping.
func jobIsTerminal(status string) bool {
	switch strings.ToUpper(status) {
	case "SUCCEEDED", "FAILED", "STOPPED":
		return true
	default: // membership test; not an exhaustive state guard
		return false
	}
}

// lastActivityAt is the cluster's last-activity unix time, derived from
// its job history.
//
// createdAt is the floor: a freshly created cluster that has run no jobs
// is "active as of creation", so its idle window starts at birth (never
// epoch). A non-terminal (PENDING/RUNNING) job means the cluster is busy
// right now, so this returns now — such a cluster is never idle.
// Otherwise each finished job counts as activity through its end
// (submittedAt + durationSecs, falling back to submittedAt when the
// duration is unknown), and the latest such end wins.
//
// jobs must already be filtered to this cluster's records.
func lastActivityAt(createdAt uint64, jobs []*core.JobRecord, now uint64) uint64 {
	last := createdAt
	for _, j := range jobs {
		if !jobIsTerminal(j.Status) {
			// Busy now — cannot be idle regardless of the other jobs'
			// ages.
			return now
		}
		dur := uint64(0)
		if j.DurationSecs != nil {
			dur = *j.DurationSecs
		}
		end := satAddU64(j.SubmittedAt, dur)
		if end > last {
			last = end
		}
	}
	return last
}

// isIdleExpired reports whether a running cluster has an IdleTimeoutSecs
// window and its time since last activity exceeds it (#100). Independent
// of the max-age cap; whichever bound fires first reaps the cluster.
func isIdleExpired(c *StoredCluster, lastActivity, now uint64) bool {
	return c.Desired == DesiredRunning &&
		c.ObservedState != nil && *c.ObservedState == core.ClusterStateRunning &&
		c.Spec.IdleTimeoutSecs != nil && satSubU64(now, lastActivity) >= *c.Spec.IdleTimeoutSecs
}

// ReapExpired enforces the two lifecycle bounds (#100): a running cluster
// is flipped to desired=Terminated — and torn down by the next reconcile
// pass — when either max-age or activity-idle fires. Max-age is checked
// first, so a cluster that is both over its age cap and idle is
// attributed to max-age. Returns the ids reaped.
func (r *Reconciler) ReapExpired(ctx context.Context, now uint64) ([]string, error) {
	clusters, err := r.store.List(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	jobs, err := r.store.ListJobs(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	jobsByCluster := make(map[string][]*core.JobRecord, len(jobs))
	for i := range jobs {
		j := &jobs[i]
		jobsByCluster[j.Cluster] = append(jobsByCluster[j.Cluster], j)
	}

	reaped := make([]string, 0)
	for i := range clusters {
		c := &clusters[i]
		cjobs := jobsByCluster[c.ID.String()]
		lastActivity := lastActivityAt(c.CreatedAt, cjobs, now)

		var reason string
		switch {
		case isExpired(c, now):
			reason = "max_age"
		case isIdleExpired(c, lastActivity, now):
			reason = "idle"
		default:
			continue
		}

		if err := r.store.SetDesired(ctx, c.ID, DesiredTerminated); err != nil {
			return nil, wrapStoreErr(err)
		}
		switch reason {
		case "max_age":
			ttl := uint64(0)
			if c.Spec.TtlSeconds != nil {
				ttl = *c.Spec.TtlSeconds
			}
			slog.Info("cluster reaped (max-age TTL)",
				"target", "bifrost::audit", "cluster", c.ID.String(), "reason", "max_age",
				"ttl", ttl, "age", satSubU64(now, c.CreatedAt))
		case "idle":
			idle := uint64(0)
			if c.Spec.IdleTimeoutSecs != nil {
				idle = *c.Spec.IdleTimeoutSecs
			}
			slog.Info("cluster reaped (activity-idle)",
				"target", "bifrost::audit", "cluster", c.ID.String(), "reason", "idle",
				"idle_timeout", idle, "idle_for", satSubU64(now, lastActivity))
		}
		reaped = append(reaped, c.ID.String())
	}
	return reaped, nil
}

// ReapTerminated is the tombstone retention sweep (Truthful Console):
// hard-deletes cluster rows that have been desired=Terminated and observed
// gone for longer than the retention window, so a reaped cluster stops
// lingering forever in GET /api/v1/clusters. A row still tearing down
// (observed_state still a live state) is left alone. Returns the ids
// removed.
func (r *Reconciler) ReapTerminated(ctx context.Context, now uint64) ([]string, error) {
	clusters, err := r.store.List(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	removed := make([]string, 0)
	for i := range clusters {
		c := &clusters[i]
		if !isPurgeableTombstone(c, now, r.terminatedRetentionSecs) {
			continue
		}
		// #122: reap any per-cluster NetworkPolicy that outlived the CR
		// before dropping the row. Idempotent (already-gone = ok). If it
		// errors, leave the row so the next pass retries rather than
		// purging the last record that this netpol is owed a reap.
		if err := r.provisioner.ReapNetworkPolicies(ctx, c.ID); err != nil {
			slog.Warn("failed to reap per-cluster NetworkPolicy on tombstone purge; deferring row removal",
				"target", "bifrost::audit", "cluster", c.ID.String(), "error", err)
			continue
		}
		ok, err := r.store.RemoveCluster(ctx, c.ID)
		if err != nil {
			return nil, wrapStoreErr(err)
		}
		if ok {
			var age uint64
			if c.TerminatedAt != nil {
				age = satSubU64(now, *c.TerminatedAt)
			}
			slog.Info("terminated cluster row reaped (retention window elapsed)",
				"target", "bifrost::audit", "cluster", c.ID.String(), "age", age)
			removed = append(removed, c.ID.String())
		}
	}
	return removed, nil
}

// DetectStaleRestore is the boot check for a stale DB restore (ADR-0007
// restore quarantine-equivalent, #41): if any backing cluster reports a
// generation newer than what the store holds, the store was restored
// behind reality — actuating would stomp a newer cluster with older
// desired state. Quarantine and alarm instead; an operator clears it after
// review. Returns whether it quarantined. Call this once before running
// Reconciler.Run.
func (r *Reconciler) DetectStaleRestore(ctx context.Context) (bool, error) {
	clusters, err := r.store.List(ctx)
	if err != nil {
		return false, wrapStoreErr(err)
	}
	for i := range clusters {
		c := &clusters[i]
		obs, err := r.provisioner.Observe(ctx, c.ID)
		switch {
		case err == nil:
			if obs.ObservedGeneration != nil && *obs.ObservedGeneration > c.Generation {
				slog.Error("stale DB restore detected (backing cluster is newer than the store) — quarantining",
					"target", "bifrost::audit", "cluster", c.ID.String(),
					"stored_generation", c.Generation, "observed_generation", *obs.ObservedGeneration)
				if err := r.store.SetQuarantine(ctx, true); err != nil {
					return false, wrapStoreErr(err)
				}
				return true, nil
			}
		case isProvisionNotFound(err):
			// Not provisioned yet: nothing to compare.
		default:
			return false, wrapProvisionErr(err)
		}
	}
	return false, nil
}

// Run runs the control loop until ctx is done: each tick reaps expired
// clusters then reconciles all. Level-triggered with a fixed resync
// interval (ADR-0006-equivalent) — an edge-trigger/watch is only an
// optimization that can be added later. Errors are logged per pass, never
// fatal, so one bad tick doesn't stop the loop.
//
// The first pass runs immediately, before the first interval elapses —
// mirroring Rust's tokio::time::interval, whose first tick fires at
// creation time rather than after a full period. Without this a fresh
// process would sit idle for a whole interval (30s by default) before its
// first reap/intent-sweep/tombstone-sweep/reconcile pass, which also
// delays resuming any crash-interrupted actuation via the outbox intent
// replay path (see the Pending-intent-replays test in reconcile_test.go).
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	slog.Info("reconcile loop started", "interval_secs", interval.Seconds())
	tick := func() {
		now := NowUnix()
		if _, err := r.ReapExpired(ctx, now); err != nil {
			slog.Warn("reap pass failed", "error", err)
		}
		// Bound outbox growth (ADR-0007-equivalent, #39): drop
		// Applied intents older than the retention window.
		cutoff := satSubU64(now, IntentRetentionSecs)
		if n, err := r.store.ReapIntents(ctx, cutoff); err != nil {
			slog.Warn("intent reap pass failed", "error", err)
		} else if n > 0 {
			slog.Debug("outbox intents reaped", "reaped", n)
		}
		// Truthful Console: drop terminated tombstone rows older
		// than the retention window.
		if ids, err := r.ReapTerminated(ctx, now); err != nil {
			slog.Warn("terminated-row reap pass failed", "error", err)
		} else if len(ids) > 0 {
			slog.Info("terminated cluster rows reaped", "reaped", len(ids))
		}
		for _, res := range r.ReconcileAll(ctx) {
			if res.Err != nil {
				slog.Warn("reconcile failed", "cluster", res.ID, "error", res.Err)
			}
		}
		if r.meteringInterval > 0 && time.Since(r.lastMeter) >= r.meteringInterval {
			r.lastMeter = time.Now()
			if n, err := r.Meter(ctx, NowUnix()); err != nil {
				slog.Warn("metering pass failed", "error", err)
			} else if n > 0 {
				slog.Debug("usage samples recorded", "samples", n)
			}
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	tick()
	for {
		select {
		case <-ticker.C:
			tick()
		case <-ctx.Done():
			slog.Info("reconcile loop shutting down")
			return
		}
	}
}

// --- Store-derived helpers ported from the Rust store layer (ledgered in
// Task 1, landed here because this reconcile engine is their only
// consumer) ---

// paramsFingerprint is a canonical fingerprint of the actuation-relevant
// spec, used by the outbox to detect a conflicting re-use of an intent key
// (ADR-0007-equivalent: stale-generation writes must be rejected). Two
// specs that produce the same generation must produce the same
// fingerprint; a {id}/{generation} key that reappears with a different
// fingerprint is a restore/rollback anomaly. core.ClusterSpec's fields are
// all actuation-relevant, so its JSON serialization is a stable
// fingerprint. Ported from store.rs's params_fingerprint.
func paramsFingerprint(spec *core.ClusterSpec) string {
	b, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(b)
}

// queueAssignmentForProject resolves the Kueue queue a project's clusters
// and jobs are admitted through: the project's allocation in a compute
// pool. See QueueAssignmentForProjectPurpose.
func queueAssignmentForProject(ctx context.Context, store Store, project string) (*provision.QueueAssignment, error) {
	return QueueAssignmentForProjectPurpose(ctx, store, project, core.PoolPurposeCompute)
}

// QueueAssignmentForProjectPurpose resolves the Kueue queue a project's
// workloads of one kind are admitted through (ADR-0010-equivalent,
// requirement 4): the first allocation matching project across the pools
// of the given purpose, carrying the pool's Elastic flag and named per
// provision.LocalQueueName (`<project>` for compute, `<project>-serving`
// for serving). nil = the project has no allocation in a pool of that
// purpose and its workloads stay queue-free. Pools of the other purpose
// are never consulted — a compute cluster cannot land in (or draw on) a
// serving pool, which is the property requirement 4 asks for. Derived from
// the store at apply time (the store is truth), so the assignment never
// travels inside a spec's serialized form — the reconcilers and the
// create/deploy APIs all resolve it through this helper. Ported from
// store.rs's queue_assignment_for_project, split by purpose.
func QueueAssignmentForProjectPurpose(ctx context.Context, store Store, project string, purpose core.PoolPurpose) (*provision.QueueAssignment, error) {
	pools, err := store.ListPools(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	purpose = purpose.OrDefault()
	for _, pool := range pools {
		if pool.Spec.Purpose.OrDefault() != purpose {
			continue
		}
		allocs, err := store.ListAllocations(ctx, pool.Name)
		if err != nil {
			return nil, wrapStoreErr(err)
		}
		for _, alloc := range allocs {
			if alloc.Project == project {
				return &provision.QueueAssignment{
					QueueName: provision.LocalQueueName(alloc.Project, purpose),
					Elastic:   pool.Spec.Elastic,
				}, nil
			}
		}
	}
	return nil, nil
}

// QueueAssignmentForProject is the compute-pool lookup (clusters and
// jobs): QueueAssignmentForProjectPurpose with core.PoolPurposeCompute.
// Used by the cluster API (create-time queue-assignment audit row and the
// suspend/resume queue-owned-suspend 409 guard) to resolve the same
// project -> Kueue-queue mapping the reconciler uses.
func QueueAssignmentForProject(ctx context.Context, store Store, project string) (*provision.QueueAssignment, error) {
	return queueAssignmentForProject(ctx, store, project)
}

// needsApply reports whether an apply is needed: when nothing is
// provisioned, when the backing cluster is gone/terminated but we still
// want it, or when the generation the cluster actually carries
// (observedGeneration, read back — #40) is behind the desired one (spec
// changed and the cluster hasn't picked it up yet). Re-applying an
// in-flight roll (same generation, still Provisioning) is not needed: the
// cluster already carries the desired generation, so we wait and
// re-observe rather than churn the provider.
//
// queued (the cluster has a Kueue queue assignment, ADR-0010-equivalent)
// changes one case: Suspended is then Kueue holding an unadmitted workload
// pod-less — Kueue owns spec.suspend for queued clusters — so re-applying
// would fight the queue. Leave it; admission unsuspends it. For queue-free
// clusters Suspended stays repairable drift (#47).
func needsApply(observed *core.ClusterState, observedGeneration, desiredGeneration uint64, queued bool) bool {
	if observed == nil {
		return true
	}
	switch *observed {
	case core.ClusterStateTerminated, core.ClusterStateTerminating:
		return true
	case core.ClusterStateSuspended:
		// #47: a Suspended cluster whose desired state is Running is
		// repairable drift — re-apply resumes it. Not so for queued
		// clusters: Kueue legitimately holds them Suspended.
		return !queued
	default: // membership test over the remaining live states; not an exhaustive state guard
		return observedGeneration < desiredGeneration
	}
}

// --- RunReconciler: the production entry point ---

// Options configures RunReconciler.
type Options struct {
	// Interval is the resync tick. DefaultReconcileInterval when <= 0.
	Interval time.Duration
	// Limits is the global actuation rate limit (#43); nil = unlimited.
	Limits *RateLimits
	// TerminatedRetentionSecs overrides the tombstone retention window;
	// nil = TerminatedRetentionSecs.
	TerminatedRetentionSecs *uint64
	// MeteringInterval is how often usage samples are recorded
	// (requirement 14). DefaultMeteringInterval when 0; negative disables.
	MeteringInterval time.Duration
	// Registrar receives gateway registrations for clusters the reconciler
	// brings up and tears down (requirement 5: ephemeral RayJobs reachable
	// through the gateway). nil = no dynamic registration.
	Registrar Registrar
	// GatewayHostname names the gateway hostname a cluster is registered
	// under (plan ruling D1: `<name>.<--gateway-domain>`). nil = no
	// dynamic registration.
	GatewayHostname func(core.ClusterId) string
	// JobProvisioner backs the ephemeral-job reconcile loop (requirement
	// 5). nil = jobs are not reconciled.
	JobProvisioner provision.JobProvisioner
}

// newReconcilerFromOptions applies every Options field onto a fresh
// Reconciler — RunReconciler's construction step, separated so tests can
// check the wiring without running the loop.
func newReconcilerFromOptions(store Store, provisioner provision.Provisioner, opts Options) *Reconciler {
	var r *Reconciler
	if opts.Limits != nil {
		r = NewReconcilerWithLimits(store, provisioner, *opts.Limits)
	} else {
		r = NewReconciler(store, provisioner)
	}
	if opts.TerminatedRetentionSecs != nil {
		r.WithTerminatedRetention(*opts.TerminatedRetentionSecs)
	}
	if opts.MeteringInterval != 0 {
		r.meteringInterval = opts.MeteringInterval
	}
	r.registrar = opts.Registrar
	r.gatewayHostname = opts.GatewayHostname
	r.jobProvisioner = opts.JobProvisioner
	return r
}

// RunReconciler constructs a Reconciler from store/provisioner/opts, runs
// the ADR-0007-equivalent stale-restore boot check once (logging its
// outcome; a failed check is non-fatal, mirroring the Rust CLI's boot
// wiring), then runs the control loop until ctx is done. Returns when the
// loop stops; the returned error is always nil today (ctx cancellation is
// a normal shutdown, not a failure) but is part of the signature for
// future fatal-startup-error reporting.
func RunReconciler(ctx context.Context, store Store, provisioner provision.Provisioner, opts Options) error {
	r := newReconcilerFromOptions(store, provisioner, opts)

	switch quarantined, err := r.DetectStaleRestore(ctx); {
	case err != nil:
		slog.Warn("stale-restore boot check failed", "error", err)
	case quarantined:
		slog.Error("started QUARANTINED after detecting a stale DB restore; not actuating until cleared")
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	r.Run(ctx, interval)
	return nil
}
