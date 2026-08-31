package controller

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// Tests below are ported from mobula-controller/src/pool_reconcile.rs's
// #[cfg(test)] mod tests (pool_reconcile.rs:236-604).

// mockPools is the Go equivalent of pool_reconcile.rs's MockPools test
// double.
type mockPools struct {
	present bool

	mu        sync.Mutex
	applies   []poolApplyCall
	deletes   []string
	observes  int
	deleteErr atomic.Bool
}

type poolApplyCall struct {
	name   string
	allocs int
}

var _ provision.PoolProvisioner = (*mockPools)(nil)

func (m *mockPools) ApplyPool(_ context.Context, spec *core.PoolSpec, allocs []core.AllocationSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applies = append(m.applies, poolApplyCall{name: spec.Name, allocs: len(allocs)})
	return nil
}

func (m *mockPools) DeletePool(_ context.Context, name string) error {
	if m.deleteErr.Load() {
		return provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "injected delete failure"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, name)
	return nil
}

func (m *mockPools) ObservePool(context.Context, string) (*provision.PoolObservation, error) {
	m.mu.Lock()
	m.observes++
	m.mu.Unlock()
	return &provision.PoolObservation{AdmittedWorkloads: 1}, nil
}

func (m *mockPools) KueuePresent(context.Context) bool { return m.present }

func (m *mockPools) applyCalls() []poolApplyCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]poolApplyCall, len(m.applies))
	copy(out, m.applies)
	return out
}

func (m *mockPools) deleteCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.deletes))
	copy(out, m.deletes)
	return out
}

func (m *mockPools) observeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.observes
}

func testPoolSpec(name string) core.PoolSpec {
	return core.PoolSpec{
		Name: name,
		Flavors: []core.FlavorSpec{{
			Name:      "cpu",
			Resources: map[string]string{"cpu": "4"},
		}},
		Cohort:            "research",
		FairSharingWeight: 1.0,
	}
}

func testAllocFixture(pool, project string) core.AllocationSpec {
	return core.AllocationSpec{Pool: pool, Project: project, Namespace: project}
}

// actionsOf unwraps a reconcile report into (name, action) pairs, failing
// the test if any entry carries an error (mirrors pool_reconcile.rs's
// `actions` helper, which unwraps and panics on Err since ReconcileError
// isn't comparable there either).
func actionsOf(t *testing.T, out []PoolReconcileResult) []PoolReconcileResult {
	t.Helper()
	for _, r := range out {
		if r.Err != nil {
			t.Fatalf("unexpected error for %s: %v", r.Name, r.Err)
		}
	}
	return out
}

func poolRig(present bool) (Store, *mockPools, *PoolReconciler) {
	store := NewMemoryStore()
	prov := &mockPools{present: present}
	rec := NewPoolReconciler(store, prov)
	return store, prov, rec
}

func TestPoolApplyOnCreateThenNoOpWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	store, prov, rec := poolRig(true)
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	if err := store.UpsertAllocation(ctx, testAllocFixture("gpu", "proj-a")); err != nil {
		t.Fatalf("upsert alloc a: %v", err)
	}
	if err := store.UpsertAllocation(ctx, testAllocFixture("gpu", "proj-b")); err != nil {
		t.Fatalf("upsert alloc b: %v", err)
	}

	out := actionsOf(t, rec.ReconcileAll(ctx))
	if len(out) != 1 || out[0].Name != "gpu" || out[0].Action != PoolActionApplied {
		t.Fatalf("out = %+v, want [gpu Applied]", out)
	}
	if calls := prov.applyCalls(); len(calls) != 1 || calls[0] != (poolApplyCall{name: "gpu", allocs: 2}) {
		t.Fatalf("apply calls = %+v, want [{gpu 2}]", calls)
	}
	// The observation is recorded onto the pool row every pass.
	p, err := store.GetPool(ctx, "gpu")
	if err != nil || p == nil || p.ObservedJSON == nil {
		t.Fatalf("pool = %+v, err=%v, want an observed_json", p, err)
	}

	// Unchanged desired state -> no second provider call.
	out = actionsOf(t, rec.ReconcileAll(ctx))
	if len(out) != 1 || out[0].Action != PoolActionNoOp {
		t.Fatalf("out = %+v, want NoOp", out)
	}
	if len(prov.applyCalls()) != 1 {
		t.Fatalf("apply calls = %d, want still 1", len(prov.applyCalls()))
	}

	// An allocation change (no generation bump) still re-applies.
	if err := store.UpsertAllocation(ctx, testAllocFixture("gpu", "proj-c")); err != nil {
		t.Fatalf("upsert alloc c: %v", err)
	}
	out = actionsOf(t, rec.ReconcileAll(ctx))
	if len(out) != 1 || out[0].Action != PoolActionApplied {
		t.Fatalf("out = %+v, want Applied", out)
	}
	if len(prov.applyCalls()) != 2 {
		t.Fatalf("apply calls = %d, want 2", len(prov.applyCalls()))
	}
}

func TestPoolDeletePropagatesToTheProvisioner(t *testing.T) {
	ctx := context.Background()
	store, prov, rec := poolRig(true)
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	rec.ReconcileAll(ctx)
	if err := store.DeletePool(ctx, "gpu"); err != nil {
		t.Fatalf("delete pool: %v", err)
	}

	out := actionsOf(t, rec.ReconcileAll(ctx))
	if len(out) != 1 || out[0].Name != "gpu" || out[0].Action != PoolActionDeleted {
		t.Fatalf("out = %+v, want [gpu Deleted]", out)
	}
	if calls := prov.deleteCalls(); len(calls) != 1 || calls[0] != "gpu" {
		t.Fatalf("delete calls = %v, want [gpu]", calls)
	}
	// Once deleted, the pool is forgotten -- no repeat teardown.
	if out := rec.ReconcileAll(ctx); len(out) != 0 {
		t.Fatalf("expected no further output, got %+v", out)
	}
	if len(prov.deleteCalls()) != 1 {
		t.Fatalf("delete calls = %d, want still 1", len(prov.deleteCalls()))
	}
}

func TestPoolQuarantineBlocksActuationButStillObserves(t *testing.T) {
	ctx := context.Background()
	store, prov, rec := poolRig(true)
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	if err := store.SetQuarantine(ctx, true); err != nil {
		t.Fatalf("set quarantine: %v", err)
	}

	out := actionsOf(t, rec.ReconcileAll(ctx))
	if len(out) != 1 || out[0].Name != "gpu" || out[0].Action != PoolActionNoOp {
		t.Fatalf("out = %+v, want [gpu NoOp]", out)
	}
	if len(prov.applyCalls()) != 0 {
		t.Fatal("no actuation while quarantined")
	}
	if prov.observeCount() != 1 {
		t.Fatalf("observe count = %d, want 1 (observation still happens)", prov.observeCount())
	}
	p, err := store.GetPool(ctx, "gpu")
	if err != nil || p == nil || p.ObservedJSON == nil {
		t.Fatalf("pool = %+v, err=%v", p, err)
	}

	// Teardown is actuation too: quarantined, a vanished pool's objects
	// are left alone (the converged set keeps the name for after the
	// quarantine lifts).
	rec.convergedMu.Lock()
	rec.converged["gpu"] = struct{}{}
	rec.convergedMu.Unlock()
	if err := store.DeletePool(ctx, "gpu"); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	out = rec.ReconcileAll(ctx)
	if len(out) != 0 {
		t.Fatalf("out = %+v, want empty (no pools listed; teardown deferred)", out)
	}
	if len(prov.deleteCalls()) != 0 {
		t.Fatal("expected no delete calls while quarantined")
	}
}

func TestPoolAbsentKueueSkipsEverything(t *testing.T) {
	ctx := context.Background()
	store, prov, rec := poolRig(false)
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}

	if out := rec.ReconcileAll(ctx); len(out) != 0 {
		t.Fatalf("out = %+v, want empty", out)
	}
	if len(prov.applyCalls()) != 0 {
		t.Fatal("expected no apply calls")
	}
	if prov.observeCount() != 0 {
		t.Fatal("expected no observe calls")
	}
	p, err := store.GetPool(ctx, "gpu")
	if err != nil || p == nil || p.ObservedJSON != nil {
		t.Fatalf("pool = %+v, err=%v, want no observed_json", p, err)
	}
}

func TestPoolStoreListErrorIsReportedNotFatal(t *testing.T) {
	ctx := context.Background()
	store := NewFailingStore()
	store.Fail("ListPools")
	prov := &mockPools{present: true}
	rec := NewPoolReconciler(store, prov)
	out := rec.ReconcileAll(ctx)
	if len(out) != 1 || out[0].Name != "<list>" || out[0].Err == nil {
		t.Fatalf("out = %+v, want a single <list> error entry", out)
	}
}

func TestPoolDeleteFailureKeepsThePoolInTheConvergedSet(t *testing.T) {
	ctx := context.Background()
	store, prov, rec := poolRig(true)
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	rec.ReconcileAll(ctx)
	if err := store.DeletePool(ctx, "gpu"); err != nil {
		t.Fatalf("delete pool: %v", err)
	}

	prov.deleteErr.Store(true)
	out := rec.ReconcileAll(ctx)
	if len(out) != 1 || out[0].Name != "gpu" || out[0].Err == nil {
		t.Fatalf("out = %+v, want a single failed gpu entry", out)
	}
	rec.convergedMu.Lock()
	_, stillThere := rec.converged["gpu"]
	rec.convergedMu.Unlock()
	if !stillThere {
		t.Fatal("a failed delete must not forget the pool")
	}

	// Retry succeeds once the backend recovers.
	prov.deleteErr.Store(false)
	out = actionsOf(t, rec.ReconcileAll(ctx))
	if len(out) != 1 || out[0].Name != "gpu" || out[0].Action != PoolActionDeleted {
		t.Fatalf("out = %+v, want [gpu Deleted]", out)
	}
}

func TestPoolQuarantineCheckErrorIsReported(t *testing.T) {
	ctx := context.Background()
	store := NewFailingStore()
	store.Fail("IsQuarantined")
	prov := &mockPools{present: true}
	rec := NewPoolReconciler(store, prov)
	out := rec.ReconcileAll(ctx)
	if len(out) != 1 || out[0].Name != "<quarantine>" || out[0].Err == nil {
		t.Fatalf("out = %+v, want a single <quarantine> error entry", out)
	}
}

func TestPoolObservationRecordErrorFailsThePoolPass(t *testing.T) {
	// Observing succeeds but persisting the observation fails -> the
	// pool's pass errors before any actuation.
	ctx := context.Background()
	store := NewFailingStore()
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	store.Fail("RecordPoolObservation")
	prov := &mockPools{present: true}
	rec := NewPoolReconciler(store, prov)
	out := rec.ReconcileAll(ctx)
	if len(out) != 1 || out[0].Name != "gpu" || out[0].Err == nil {
		t.Fatalf("out = %+v, want a single failed gpu entry", out)
	}
	if len(prov.applyCalls()) != 0 {
		t.Fatal("no actuation when the observation couldn't be recorded")
	}
}

func TestPoolStaleIntentFingerprintIsRejected(t *testing.T) {
	// An outbox row for this pool's key with a DIFFERENT fingerprint
	// (store corrupt or replayed) must refuse actuation, mirroring the
	// cluster reconciler's #39 fence.
	ctx := context.Background()
	store, prov, rec := poolRig(true)
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	stored, err := store.GetPool(ctx, "gpu")
	if err != nil || stored == nil {
		t.Fatalf("get pool: %v %v", stored, err)
	}
	fp := desiredFingerprint(&stored.Spec, nil)
	key := poolIntentKey(stored, fp)
	if _, err := store.BeginIntent(ctx, key, "a-different-fingerprint"); err != nil {
		t.Fatalf("begin intent: %v", err)
	}

	out := rec.ReconcileAll(ctx)
	if len(out) != 1 {
		t.Fatalf("out = %+v", out)
	}
	rerr, ok := out[0].Err.(ReconcileError)
	if !ok || rerr.Kind != ReconcileErrStaleIntent {
		t.Fatalf("err = %#v, want ReconcileErrStaleIntent", out[0].Err)
	}
	if len(prov.applyCalls()) != 0 {
		t.Fatal("expected no apply calls")
	}
}

func TestPoolRunLoopConvergesThenStopsOnShutdown(t *testing.T) {
	ctx := context.Background()
	store, prov, rec := poolRig(true)
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		rec.Run(runCtx, 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	if calls := prov.applyCalls(); len(calls) != 1 || calls[0] != (poolApplyCall{name: "gpu", allocs: 0}) {
		t.Fatalf("apply calls = %+v, want exactly one {gpu 0}", calls)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop should stop promptly on shutdown")
	}
}

func TestPoolRunLoopLogsAbsentKueuePosture(t *testing.T) {
	// The Kueue-absent startup branch: the loop runs inert and stops.
	ctx := context.Background()
	store, prov, rec := poolRig(false)
	if _, err := store.UpsertPool(ctx, "gpu", testPoolSpec("gpu")); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		rec.Run(runCtx, 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	if len(prov.applyCalls()) != 0 {
		t.Fatal("expected no apply calls while Kueue is absent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop should stop promptly on shutdown")
	}
}

// TestPoolReconcileAllIsRaceClean exercises ReconcileAll concurrently
// against a shared PoolReconciler, keeping the race detector honest on
// the converged-set bookkeeping (the pool engine's only shared mutable
// state beyond the store itself).
func TestPoolReconcileAllIsRaceClean(t *testing.T) {
	ctx := context.Background()
	store, _, rec := poolRig(true)
	for i := 0; i < 5; i++ {
		name := string(rune('a' + i))
		if _, err := store.UpsertPool(ctx, name, testPoolSpec(name)); err != nil {
			t.Fatalf("upsert pool %s: %v", name, err)
		}
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				rec.ReconcileAll(ctx)
			}
		}()
	}
	wg.Wait()
}
