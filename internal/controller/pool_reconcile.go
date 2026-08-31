// Pool reconcile loop (ADR-0010-equivalent): the level-triggered
// counterpart of the cluster reconciler for Kueue pool objects, and
// simpler — pools have no state machine. Every pass lists desired pools +
// allocations from the store and converges the Kueue objects (Cohort /
// ResourceFlavors / ClusterQueue / LocalQueues) through a
// provision.PoolProvisioner, then records the ClusterQueue status
// observation back onto the pool row (a later task's metering loop reads
// those observations).
//
// Ported from mobula-controller/src/pool_reconcile.rs (Rust reference,
// retired project).
//
// Differences from the cluster engine, by design:
//   - No-op when unchanged: the outbox intent row for
//     "pool:{name}/{generation}:{digest}" is the record of "this desired
//     state is applied" — a matching Applied intent skips the provider
//     call (the digest covers the allocations, which don't bump the pool
//     generation, so allocation-only changes still re-apply under a fresh
//     key).
//   - Deletion is disappearance: the store hard-deletes pools, so the
//     loop tears down Kueue objects for pools it applied earlier that are
//     no longer listed. The applied-set is in-memory; a control-plane
//     restart between apply and delete leaves orphaned objects until the
//     loop runs again in the same process — bounded, and the objects are
//     bifrost.dev/pool-labeled for manual/audit cleanup.
//   - Absent Kueue = inert: when the CRDs aren't served, the loop skips
//     everything (no actuation, no observation) and pools remain
//     in-process quota only (ADR-0010 fallback).
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// PoolAction is the per-pool outcome of a reconcile pass, for
// logging/metrics/tests.
type PoolAction int

const (
	// PoolActionNoOp: desired state already applied (matching Applied
	// intent) or only observed (quarantined).
	PoolActionNoOp PoolAction = iota
	// PoolActionApplied: applied the pool's Kueue objects (create or
	// update).
	PoolActionApplied
	// PoolActionDeleted: the pool vanished from the store; its Kueue
	// objects were deleted.
	PoolActionDeleted
)

// String renders the action for logs.
func (a PoolAction) String() string {
	switch a {
	case PoolActionNoOp:
		return "no_op"
	case PoolActionApplied:
		return "applied"
	case PoolActionDeleted:
		return "deleted"
	}
	return "unknown"
}

// PoolReconcileResult is one pool's outcome from a reconcile pass — the Go
// equivalent of Rust's (String, Result<PoolAction, ReconcileError>) tuple.
type PoolReconcileResult struct {
	Name   string
	Action PoolAction
	Err    error
}

// PoolReconciler is the pool reconcile engine.
type PoolReconciler struct {
	store       Store
	provisioner provision.PoolProvisioner

	// converged is the set of pool names this process has converged
	// (applied or found already applied). Drives teardown when a pool
	// disappears from the store.
	convergedMu sync.Mutex
	converged   map[string]struct{}
}

// NewPoolReconciler returns a ready-to-use PoolReconciler.
func NewPoolReconciler(store Store, provisioner provision.PoolProvisioner) *PoolReconciler {
	return &PoolReconciler{
		store:       store,
		provisioner: provisioner,
		converged:   make(map[string]struct{}),
	}
}

// ReconcileAll reconciles every pool once. Empty when the Kueue CRDs are
// absent (nothing to actuate or observe). Errors on individual pools are
// collected, not fatal — one bad pool must not stall the loop.
func (r *PoolReconciler) ReconcileAll(ctx context.Context) []PoolReconcileResult {
	if !r.provisioner.KueuePresent(ctx) {
		return nil
	}
	pools, err := r.store.ListPools(ctx)
	if err != nil {
		return []PoolReconcileResult{{Name: "<list>", Err: wrapStoreErr(err)}}
	}
	current := make(map[string]struct{}, len(pools))
	for _, p := range pools {
		current[p.Name] = struct{}{}
	}
	out := make([]PoolReconcileResult, 0, len(pools))
	for i := range pools {
		p := &pools[i]
		action, err := r.reconcileOne(ctx, p)
		out = append(out, PoolReconcileResult{Name: p.Name, Action: action, Err: err})
	}

	// Teardown for pools that disappeared. Quarantine (ADR-0007-
	// equivalent, #41) blocks ALL actuation, deletes included.
	quarantined, err := r.store.IsQuarantined(ctx)
	switch {
	case err != nil:
		out = append(out, PoolReconcileResult{Name: "<quarantine>", Err: wrapStoreErr(err)})
	case quarantined:
		slog.Warn("control plane quarantined: pool teardowns deferred", "target", "bifrost::audit")
	default:
		r.convergedMu.Lock()
		vanished := make([]string, 0)
		for name := range r.converged {
			if _, ok := current[name]; !ok {
				vanished = append(vanished, name)
			}
		}
		r.convergedMu.Unlock()
		// Deterministic ordering (Rust's BTreeSet::difference iterates
		// sorted).
		sort.Strings(vanished)
		for _, name := range vanished {
			if err := r.provisioner.DeletePool(ctx, name); err != nil {
				out = append(out, PoolReconcileResult{Name: name, Err: wrapProvisionErr(err)})
				continue
			}
			r.convergedMu.Lock()
			delete(r.converged, name)
			r.convergedMu.Unlock()
			out = append(out, PoolReconcileResult{Name: name, Action: PoolActionDeleted})
		}
	}
	return out
}

func (r *PoolReconciler) reconcileOne(ctx context.Context, pool *StoredPool) (PoolAction, error) {
	// 1. Observe and record (ADR-0006-equivalent): the ClusterQueue
	//    status is the quota ledger; persist it for the API/metering
	//    regardless of what we actuate below. A missing ClusterQueue
	//    records nothing — the last known observation stays until one
	//    exists.
	obs, err := r.provisioner.ObservePool(ctx, pool.Name)
	if err != nil {
		return 0, wrapProvisionErr(err)
	}
	if obs != nil {
		b, jerr := json.Marshal(obs)
		if jerr != nil {
			b = []byte("")
		}
		if err := r.store.RecordPoolObservation(ctx, pool.Name, string(b)); err != nil {
			return 0, wrapStoreErr(err)
		}
	}

	// 2. Quarantine: observe but never actuate (ADR-0007-equivalent,
	//    #41).
	quarantined, err := r.store.IsQuarantined(ctx)
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	if quarantined {
		slog.Warn("control plane quarantined: observing pool only, not actuating",
			"target", "bifrost::audit", "pool", pool.Name)
		return PoolActionNoOp, nil
	}

	// 3. Actuate on change. The intent key embeds the pool generation
	//    (spec changes) plus a digest of the full desired state
	//    (allocation changes don't bump the generation), so any desired
	//    change produces a fresh key and a same-state pass finds the
	//    Applied row and no-ops. The "pool:" prefix namespaces these
	//    intents from cluster intents ({id}/{generation}).
	allocs, err := r.store.ListAllocations(ctx, pool.Name)
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	fp := desiredFingerprint(&pool.Spec, allocs)
	key := poolIntentKey(pool, fp)

	existing, err := r.store.GetIntent(ctx, key)
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	if existing != nil && existing.Status == IntentStatusApplied && existing.ParamsFingerprint == fp {
		r.convergedMu.Lock()
		r.converged[pool.Name] = struct{}{}
		r.convergedMu.Unlock()
		return PoolActionNoOp, nil
	}

	outcome, err := r.store.BeginIntent(ctx, key, fp)
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	if outcome.Kind == IntentOutcomeParamMismatch {
		// A fresh key must never collide with a different fingerprint;
		// if it does, the store is corrupt or replayed — refuse.
		return 0, ReconcileError{Kind: ReconcileErrStaleIntent, Key: key}
	}
	if err := r.provisioner.ApplyPool(ctx, &pool.Spec, allocs); err != nil {
		return 0, wrapProvisionErr(err)
	}
	if err := r.store.CompleteIntent(ctx, key, "{}"); err != nil {
		return 0, wrapStoreErr(err)
	}
	r.convergedMu.Lock()
	r.converged[pool.Name] = struct{}{}
	r.convergedMu.Unlock()
	return PoolActionApplied, nil
}

// Run runs the pool control loop until ctx is done. Level-triggered on a
// fixed resync interval, like the cluster reconciler. Errors are logged
// per pass, never fatal.
func (r *PoolReconciler) Run(ctx context.Context, interval time.Duration) {
	// Log the Kueue posture once at startup; the per-client cache in
	// KueuePresent makes later checks free and keeps this accurate for
	// the process lifetime.
	if r.provisioner.KueuePresent(ctx) {
		slog.Info("pool reconcile loop started (Kueue present)", "interval_secs", interval.Seconds())
	} else {
		slog.Info("Kueue CRDs absent — pools are in-process quota only")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			for _, res := range r.ReconcileAll(ctx) {
				if res.Err != nil {
					slog.Warn("pool reconcile failed", "pool", res.Name, "error", res.Err)
				}
			}
		case <-ctx.Done():
			slog.Info("pool reconcile loop shutting down")
			return
		}
	}
}

// desiredFingerprintDoc is the canonical JSON shape used by
// desiredFingerprint: the spec plus its allocations sorted by project
// (store iteration order is not stable). Doubles as the outbox fingerprint
// and the input to the key digest.
type desiredFingerprintDoc struct {
	Spec        *core.PoolSpec        `json:"spec"`
	Allocations []core.AllocationSpec `json:"allocations"`
}

func desiredFingerprint(spec *core.PoolSpec, allocs []core.AllocationSpec) string {
	sorted := make([]core.AllocationSpec, len(allocs))
	copy(sorted, allocs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Project < sorted[j].Project })
	b, err := json.Marshal(desiredFingerprintDoc{Spec: spec, Allocations: sorted})
	if err != nil {
		return ""
	}
	return string(b)
}

// fnv1aDigest hashes s with FNV-1a: a short, stable-across-builds digest
// so the intent key stays compact (no hashing dependency needed).
func fnv1aDigest(s string) string {
	var h uint64 = 0xcbf29ce484222325
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 0x100000001b3
	}
	return fmt.Sprintf("%016x", h)
}

// poolIntentKey is the outbox key for a pool's desired state:
// "pool:{name}/{generation}:{digest}" — derived from "{pool}/{generation}",
// with the digest covering allocation changes that don't bump the
// generation.
func poolIntentKey(pool *StoredPool, fingerprint string) string {
	return fmt.Sprintf("pool:%s/%d:%s", pool.Name, pool.Generation, fnv1aDigest(fingerprint))
}

// PoolOptions configures RunPoolReconciler.
type PoolOptions struct {
	// Interval is the resync tick. DefaultReconcileInterval when <= 0.
	Interval time.Duration
}

// RunPoolReconciler constructs a PoolReconciler and runs its control loop
// until ctx is done. Returns when the loop stops; the returned error is
// always nil today but is part of the signature for future fatal-
// startup-error reporting, symmetric with RunReconciler.
func RunPoolReconciler(ctx context.Context, store Store, provisioner provision.PoolProvisioner, opts PoolOptions) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	r := NewPoolReconciler(store, provisioner)
	r.Run(ctx, interval)
	return nil
}
