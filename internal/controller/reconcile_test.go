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

// Tests below are ported from the predecessor's controller crate, src/reconcile.rs's
// #[cfg(test)] mod tests (reconcile.rs:744-1339), plus
// queue_assignment_resolves_first_matching_allocation, ported from
// store.rs (deferred to this task per T1's store_memory_test.go doc
// comment — see reconcile.go's paramsFingerprint/
// queueAssignmentForProject doc comments). One Rust test,
// set_desired_stamps_and_clears_terminated_at, is NOT re-ported here: its
// Go equivalent (TestMemoryStoreSetDesiredAnchorsAndClearsTerminatedAt)
// already landed in store_memory_test.go as part of Task 1 — the same
// MemoryStore behavior, already covered, so porting it again here would
// only duplicate it.
//
// A handful of supplementary tests beyond the Rust file's own coverage
// are added at the bottom of this file (search "Supplementary"): the Rust
// suite exercises reconcileOne's decision *logic* directly (needsApply,
// is_expired, ...) but never drives the full Applied/Suspended/
// Terminated/Drift/rate-limit/stale-intent paths of reconcileOne
// end-to-end. Given this is flagged as the wave's most
// concurrency-sensitive code, that coverage gap is worth closing here.

// --- fixtures ---

func testClusterSpec() core.ClusterSpec {
	return core.ClusterSpec{
		Engine:     core.EngineRay,
		Name:       "c",
		Project:    "p",
		RayVersion: "2.57.0",
		Image:      "img",
		HeadCpu:    "1",
		HeadMemory: "2Gi",
	}
}

func storedFixture(ttl *uint64, createdAt uint64, observed *core.ClusterState) *StoredCluster {
	spec := testClusterSpec()
	spec.TtlSeconds = ttl
	return &StoredCluster{
		ID:                 core.ClusterId("c"),
		Spec:               spec,
		Generation:         1,
		Desired:            DesiredRunning,
		ObservedState:      observed,
		ObservedGeneration: 1,
		CreatedAt:          createdAt,
	}
}

// storedIdleFixture is storedFixture with an idle-reap window set (#100).
func storedIdleFixture(idle *uint64, createdAt uint64, observed *core.ClusterState) *StoredCluster {
	c := storedFixture(nil, createdAt, observed)
	c.Spec.IdleTimeoutSecs = idle
	return c
}

func u64p(v uint64) *uint64 { return &v }

func clusterStatep(s core.ClusterState) *core.ClusterState { return &s }

func testJob(cluster, status string, submittedAt uint64, duration *uint64) core.JobRecord {
	return core.JobRecord{
		Id:           cluster + "-job",
		Cluster:      cluster,
		Submitter:    "-",
		Status:       status,
		DurationSecs: duration,
		SubmittedAt:  submittedAt,
	}
}

// fakeProvisioner is a scriptable provision.Provisioner: Observe/Apply
// behavior is driven by optional function fields; Terminate/Suspend/
// EnsureNamespacePosture/ReapNetworkPolicies calls are recorded. The Go
// analogue of reconcile.rs's ErrProv/VanishingProv test doubles, made
// reusable across every scenario in this file via its function fields.
type fakeProvisioner struct {
	provision.BaseProvisioner

	mu           sync.Mutex
	observeCalls int
	// observeFn, when set, is called with the 0-based Observe call index.
	// Default (nil): always NotFound.
	observeFn func(callIndex int) (provision.ObservedCluster, error)

	applyKeys []string
	applyFn   func(id core.ClusterId, spec *core.ClusterSpec, generation uint64, key string, queue *provision.QueueAssignment) (provision.ApplyResponse, error)

	terminateCalls []core.ClusterId
	suspendCalls   []core.ClusterId

	ensurePostureCalls int
	ensurePostureErr   error

	reapNetpolCalls []core.ClusterId
	reapNetpolErr   error
}

var _ provision.Provisioner = (*fakeProvisioner)(nil)

func (p *fakeProvisioner) Observe(_ context.Context, id core.ClusterId) (provision.ObservedCluster, error) {
	p.mu.Lock()
	n := p.observeCalls
	p.observeCalls++
	fn := p.observeFn
	p.mu.Unlock()
	if fn != nil {
		return fn(n)
	}
	return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
}

func (p *fakeProvisioner) Apply(_ context.Context, id core.ClusterId, spec *core.ClusterSpec, generation uint64, key string, queue *provision.QueueAssignment) (provision.ApplyResponse, error) {
	p.mu.Lock()
	p.applyKeys = append(p.applyKeys, key)
	fn := p.applyFn
	p.mu.Unlock()
	if fn != nil {
		return fn(id, spec, generation, key, queue)
	}
	return provision.ApplyResponse{Generation: generation}, nil
}

func (p *fakeProvisioner) EnsureNamespacePosture(context.Context) error {
	p.mu.Lock()
	p.ensurePostureCalls++
	err := p.ensurePostureErr
	p.mu.Unlock()
	return err
}

func (p *fakeProvisioner) Terminate(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	p.terminateCalls = append(p.terminateCalls, id)
	p.mu.Unlock()
	return nil
}

func (p *fakeProvisioner) Suspend(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	p.suspendCalls = append(p.suspendCalls, id)
	p.mu.Unlock()
	return nil
}

func (p *fakeProvisioner) Resume(context.Context, core.ClusterId) error { return nil }

func (p *fakeProvisioner) List(context.Context) ([]provision.ObservedCluster, error) { return nil, nil }

func (p *fakeProvisioner) ReapNetworkPolicies(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	p.reapNetpolCalls = append(p.reapNetpolCalls, id)
	err := p.reapNetpolErr
	p.mu.Unlock()
	return err
}

// errProvisioner: Observe always fails with a backend error (reconcile.rs's ErrProv).
func errProvisioner() *fakeProvisioner {
	return &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "injected observe failure"}
		},
	}
}

// vanishingProvisioner reports a converged cluster on the first observe of
// each reconcile pass, then NotFound on the re-observe (the cluster
// vanished mid-pass) — reconcile.rs's VanishingProv. startOdd lets a
// caller start the alternation at NotFound instead of Running.
func vanishingProvisioner(startOdd bool) *fakeProvisioner {
	p := &fakeProvisioner{}
	if startOdd {
		p.observeCalls = 1
	}
	p.observeFn = func(n int) (provision.ObservedCluster, error) {
		if n%2 == 0 {
			gen := uint64(1)
			return provision.ObservedCluster{ID: "c", State: core.ClusterStateRunning, ObservedGeneration: &gen}, nil
		}
		return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: "c"}
	}
	return p
}

// --- pure decision-logic tests (reconcile.rs:790-1013) ---

func TestIsExpiredMatrix(t *testing.T) {
	// Running past TTL -> expired.
	if !isExpired(storedFixture(u64p(60), 100, clusterStatep(core.ClusterStateRunning)), 200) {
		t.Fatal("expected expired: running past TTL")
	}
	// Within TTL -> not.
	if isExpired(storedFixture(u64p(60), 100, clusterStatep(core.ClusterStateRunning)), 130) {
		t.Fatal("expected not expired: within TTL")
	}
	// No TTL -> never.
	if isExpired(storedFixture(nil, 0, clusterStatep(core.ClusterStateRunning)), 999_999) {
		t.Fatal("expected not expired: no TTL set")
	}
	// Not observed Running yet -> don't reap mid-provision.
	if isExpired(storedFixture(u64p(1), 0, clusterStatep(core.ClusterStateProvisioning)), 999) {
		t.Fatal("expected not expired: not yet observed Running")
	}
}

func TestLastActivityDerivesFromJobHistory(t *testing.T) {
	// No jobs -> creation is the floor.
	if got := lastActivityAt(100, nil, 999); got != 100 {
		t.Fatalf("no jobs: got %d, want 100", got)
	}
	// A finished job's end (submitted + duration) beats creation.
	done := testJob("c", "SUCCEEDED", 200, u64p(50))
	if got := lastActivityAt(100, []*core.JobRecord{&done}, 999); got != 250 {
		t.Fatalf("finished job: got %d, want 250", got)
	}
	// Terminal without a duration counts at submission time.
	doneNoDur := testJob("c", "FAILED", 300, nil)
	if got := lastActivityAt(100, []*core.JobRecord{&doneNoDur}, 999); got != 300 {
		t.Fatalf("terminal no duration: got %d, want 300", got)
	}
	// The latest finished job wins.
	older := testJob("c", "STOPPED", 150, u64p(10))
	if got := lastActivityAt(100, []*core.JobRecord{&older, &done}, 999); got != 250 {
		t.Fatalf("latest wins: got %d, want 250", got)
	}
	// A still-running job -> busy *now*, regardless of other jobs' ages.
	running := testJob("c", "RUNNING", 120, nil)
	if got := lastActivityAt(100, []*core.JobRecord{&older, &running}, 999); got != 999 {
		t.Fatalf("running job: got %d, want 999", got)
	}
	// An unknown status is treated as still-active (fail-safe: keep alive).
	weird := testJob("c", "SOME_NEW_STATE", 120, nil)
	if got := lastActivityAt(100, []*core.JobRecord{&weird}, 999); got != 999 {
		t.Fatalf("unknown status: got %d, want 999", got)
	}
	// Status matching is case-insensitive.
	if !jobIsTerminal("succeeded") || !jobIsTerminal("Failed") {
		t.Fatal("expected case-insensitive terminal match")
	}
	if jobIsTerminal("running") {
		t.Fatal("expected running to be non-terminal")
	}
}

func TestIsIdleExpiredMatrix(t *testing.T) {
	// Idle window elapsed since last activity -> idle-expired.
	if !isIdleExpired(storedIdleFixture(u64p(60), 0, clusterStatep(core.ClusterStateRunning)), 100, 200) {
		t.Fatal("expected idle-expired: 100s idle >= 60")
	}
	// Within the idle window -> not.
	if isIdleExpired(storedIdleFixture(u64p(60), 0, clusterStatep(core.ClusterStateRunning)), 100, 130) {
		t.Fatal("expected not idle-expired: 30s idle < 60")
	}
	// No idle window set -> never (keeps old max-age-only behavior).
	if isIdleExpired(storedIdleFixture(nil, 0, clusterStatep(core.ClusterStateRunning)), 0, 999_999) {
		t.Fatal("expected not idle-expired: no idle window")
	}
	// Not observed Running yet -> don't idle-reap mid-provision.
	if isIdleExpired(storedIdleFixture(u64p(1), 0, clusterStatep(core.ClusterStateProvisioning)), 0, 999) {
		t.Fatal("expected not idle-expired: not yet observed Running")
	}
}

func TestNeedsApplyMatrix(t *testing.T) {
	// Nothing provisioned yet.
	if !needsApply(nil, 0, 1, false) {
		t.Fatal("nothing provisioned: expected needsApply")
	}
	// Gone but wanted.
	if !needsApply(clusterStatep(core.ClusterStateTerminated), 1, 1, false) {
		t.Fatal("terminated: expected needsApply")
	}
	// Cluster carries an older generation than desired (spec changed, not
	// yet picked up).
	if !needsApply(clusterStatep(core.ClusterStateRunning), 1, 2, false) {
		t.Fatal("generation behind: expected needsApply")
	}
	// Steady state, cluster carries the desired generation.
	if needsApply(clusterStatep(core.ClusterStateRunning), 1, 1, false) {
		t.Fatal("steady state: expected no needsApply")
	}
	// Mid-roll at the desired generation -> wait, don't re-apply/churn.
	if needsApply(clusterStatep(core.ClusterStateProvisioning), 2, 2, false) {
		t.Fatal("mid-roll: expected no needsApply")
	}
	// #47: Suspended with desired Running is repairable -> re-apply.
	if !needsApply(clusterStatep(core.ClusterStateSuspended), 1, 1, false) {
		t.Fatal("suspended queue-free: expected needsApply")
	}
	// ADR-0010: a queued cluster's Suspended is Kueue admission queueing,
	// not drift -- re-applying would fight Kueue's suspend.
	if needsApply(clusterStatep(core.ClusterStateSuspended), 1, 1, true) {
		t.Fatal("suspended queued: expected no needsApply")
	}
}

func TestBackoffGrowsExponentiallyAndCaps(t *testing.T) {
	cases := []struct {
		failureCount uint32
		want         uint64
	}{
		{1, 5},
		{2, 10},
		{3, 20},
		{10, 300},
		{^uint32(0), 300}, // a saturated failure_count can't overflow the shift
	}
	for _, c := range cases {
		if got := backoffSecs(c.failureCount); got != c.want {
			t.Fatalf("backoffSecs(%d) = %d, want %d", c.failureCount, got, c.want)
		}
	}
}

func tombstoneFixture(observed *core.ClusterState, terminatedAt *uint64) *StoredCluster {
	c := storedFixture(nil, 0, observed)
	c.Desired = DesiredTerminated
	c.TerminatedAt = terminatedAt
	return c
}

func TestPurgeableTombstoneMatrix(t *testing.T) {
	const retention = 3600
	// Terminated, never observed, old enough -> purgeable.
	if !isPurgeableTombstone(tombstoneFixture(nil, u64p(0)), 4000, retention) {
		t.Fatal("expected purgeable: never observed, old enough")
	}
	// Terminated, observed Terminated, old enough -> purgeable.
	if !isPurgeableTombstone(tombstoneFixture(clusterStatep(core.ClusterStateTerminated), u64p(0)), 4000, retention) {
		t.Fatal("expected purgeable: observed terminated, old enough")
	}
	// Too recent -> not yet.
	if isPurgeableTombstone(tombstoneFixture(nil, u64p(1000)), 2000, retention) {
		t.Fatal("expected not purgeable: too recent")
	}
	// Still observed live (teardown in flight) -> never, even if old.
	if isPurgeableTombstone(tombstoneFixture(clusterStatep(core.ClusterStateRunning), u64p(0)), 999_999, retention) {
		t.Fatal("expected not purgeable: still observed live")
	}
	// No terminated_at stamp -> never.
	if isPurgeableTombstone(tombstoneFixture(nil, nil), 999_999, retention) {
		t.Fatal("expected not purgeable: no terminated_at stamp")
	}
	// Not terminated (desired Running) -> never.
	if isPurgeableTombstone(storedFixture(nil, 0, nil), 999_999, retention) {
		t.Fatal("expected not purgeable: desired Running")
	}
}

// --- store-integration tests (reconcile.rs:877-1141) ---

func TestReapIdlesUnusedButSparesBusyClusters(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	base := NowUnix()
	now := base + 10_000

	insertIdle := func(id string, idle uint64) {
		spec := testClusterSpec()
		spec.IdleTimeoutSecs = &idle
		cid := core.ClusterId(id)
		if _, err := store.UpsertDesired(ctx, cid, spec); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
		st := core.ClusterStateRunning
		if err := store.RecordObservation(ctx, cid, &st, 1); err != nil {
			t.Fatalf("record observation %s: %v", id, err)
		}
	}

	// idle: no jobs at all -> idle since creation (~10_000s ago) -> reaped.
	insertIdle("idle", 60)
	// busy: a currently-running job -> active now -> spared.
	insertIdle("busy", 60)
	if err := store.RecordJob(ctx, testJob("busy", "RUNNING", base, nil)); err != nil {
		t.Fatalf("record job: %v", err)
	}
	// recent: a job that finished 10s ago -> within the window -> spared.
	insertIdle("recent", 60)
	if err := store.RecordJob(ctx, testJob("recent", "SUCCEEDED", now-10, u64p(0))); err != nil {
		t.Fatalf("record job: %v", err)
	}
	// stale: a job that finished ~10_000s ago -> past the window -> reaped.
	insertIdle("stale", 60)
	if err := store.RecordJob(ctx, testJob("stale", "SUCCEEDED", base, u64p(5))); err != nil {
		t.Fatalf("record job: %v", err)
	}

	rec := NewReconciler(store, errProvisioner())
	reaped, err := rec.ReapExpired(ctx, now)
	if err != nil {
		t.Fatalf("reap expired: %v", err)
	}
	wantReaped := map[string]bool{"idle": true, "stale": true}
	if len(reaped) != len(wantReaped) {
		t.Fatalf("reaped = %v, want exactly %v", reaped, wantReaped)
	}
	for _, id := range reaped {
		if !wantReaped[id] {
			t.Fatalf("unexpected reap of %q", id)
		}
	}

	for id, want := range map[string]DesiredState{
		"idle": DesiredTerminated, "stale": DesiredTerminated,
		"busy": DesiredRunning, "recent": DesiredRunning,
	} {
		c, err := store.Get(ctx, core.ClusterId(id))
		if err != nil || c == nil {
			t.Fatalf("get %s: %v %v", id, c, err)
		}
		if c.Desired != want {
			t.Fatalf("cluster %s: desired = %v, want %v", id, c.Desired, want)
		}
	}
}

func TestMaxAgeCapsABusyClusterAndIdleUnsetKeepsOldBehavior(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	base := NowUnix()
	now := base + 10_000

	// capped: over its max-age ttl AND actively running a job -- max-age
	// is the absolute cap, so it is reaped anyway. Its idle window
	// (large) has NOT elapsed, proving the reap is attributed to
	// max-age, not idle.
	cappedSpec := testClusterSpec()
	cappedSpec.TtlSeconds = u64p(60)
	cappedSpec.IdleTimeoutSecs = u64p(1_000_000)
	capped := core.ClusterId("capped")
	if _, err := store.UpsertDesired(ctx, capped, cappedSpec); err != nil {
		t.Fatalf("upsert capped: %v", err)
	}
	st := core.ClusterStateRunning
	if err := store.RecordObservation(ctx, capped, &st, 1); err != nil {
		t.Fatalf("record observation: %v", err)
	}
	if err := store.RecordJob(ctx, testJob("capped", "RUNNING", base, nil)); err != nil {
		t.Fatalf("record job: %v", err)
	}

	// plain: neither ttl nor idle set -> never reaped.
	plain := core.ClusterId("plain")
	if _, err := store.UpsertDesired(ctx, plain, testClusterSpec()); err != nil {
		t.Fatalf("upsert plain: %v", err)
	}
	if err := store.RecordObservation(ctx, plain, &st, 1); err != nil {
		t.Fatalf("record observation: %v", err)
	}

	rec := NewReconciler(store, errProvisioner())
	reaped, err := rec.ReapExpired(ctx, now)
	if err != nil {
		t.Fatalf("reap expired: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "capped" {
		t.Fatalf("reaped = %v, want [capped]", reaped)
	}
	plainRow, err := store.Get(ctx, plain)
	if err != nil || plainRow == nil {
		t.Fatalf("get plain: %v %v", plainRow, err)
	}
	if plainRow.Desired != DesiredRunning {
		t.Fatalf("plain desired = %v, want DesiredRunning", plainRow.Desired)
	}
}

func TestReapTerminatedRemovesOnlyDeadTombstones(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	// A dead tombstone: terminated and never observed (gone).
	dead := core.ClusterId("dead")
	if _, err := store.UpsertDesired(ctx, dead, testClusterSpec()); err != nil {
		t.Fatalf("upsert dead: %v", err)
	}
	if err := store.SetDesired(ctx, dead, DesiredTerminated); err != nil {
		t.Fatalf("set desired dead: %v", err)
	}

	// Terminated but still observed Running (teardown in flight): keep.
	live := core.ClusterId("live")
	if _, err := store.UpsertDesired(ctx, live, testClusterSpec()); err != nil {
		t.Fatalf("upsert live: %v", err)
	}
	if err := store.SetDesired(ctx, live, DesiredTerminated); err != nil {
		t.Fatalf("set desired live: %v", err)
	}
	st := core.ClusterStateRunning
	if err := store.RecordObservation(ctx, live, &st, 1); err != nil {
		t.Fatalf("record observation live: %v", err)
	}

	// A running cluster (not a tombstone): keep.
	run := core.ClusterId("run")
	if _, err := store.UpsertDesired(ctx, run, testClusterSpec()); err != nil {
		t.Fatalf("upsert run: %v", err)
	}

	rec := NewReconciler(store, errProvisioner()).WithTerminatedRetention(0)
	// now in the future so age (now - terminated_at) clears the (zero)
	// retention window.
	reaped, err := rec.ReapTerminated(ctx, NowUnix()+10)
	if err != nil {
		t.Fatalf("reap terminated: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "dead" {
		t.Fatalf("reaped = %v, want [dead]", reaped)
	}
	if c, _ := store.Get(ctx, dead); c != nil {
		t.Fatal("dead cluster row should be removed")
	}
	if c, _ := store.Get(ctx, live); c == nil {
		t.Fatal("live cluster row should be kept")
	}
	if c, _ := store.Get(ctx, run); c == nil {
		t.Fatal("run cluster row should be kept")
	}
}

func TestStoreListErrorIsCollectedNotFatal(t *testing.T) {
	ctx := context.Background()
	store := NewFailingStore()
	store.Fail("List")
	rec := NewReconciler(store, errProvisioner())
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 {
		t.Fatalf("out = %v, want 1 entry", out)
	}
	if out[0].ID != "<list>" {
		t.Fatalf("ID = %q, want <list>", out[0].ID)
	}
	if out[0].Err == nil {
		t.Fatal("expected an error")
	}
}

func TestObserveBackendErrorFailsTheClusterPass(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if _, err := store.UpsertDesired(ctx, core.ClusterId("c"), testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec := NewReconciler(store, errProvisioner())
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err == nil {
		t.Fatalf("out = %+v, want one erroring entry", out)
	}
	rerr, ok := out[0].Err.(ReconcileError)
	if !ok || rerr.Kind != ReconcileErrProvision {
		t.Fatalf("err = %#v, want ReconcileErrProvision", out[0].Err)
	}
}

func TestClusterVanishingMidPassRecordsNoObservation(t *testing.T) {
	// The re-observe after a NoOp decision returns NotFound: the stored
	// observation is cleared (recorded as absent), not left stale.
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec := NewReconciler(store, vanishingProvisioner(false))
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v, want one clean entry", out)
	}
	if out[0].Action != ActionNoOp {
		t.Fatalf("action = %v, want ActionNoOp", out[0].Action)
	}
	stored, err := store.Get(ctx, id)
	if err != nil || stored == nil {
		t.Fatalf("get: %v %v", stored, err)
	}
	if stored.ObservedState != nil {
		t.Fatalf("observed state should be cleared, got %v", *stored.ObservedState)
	}
	if stored.ObservedGeneration != 0 {
		t.Fatalf("observed generation = %d, want 0", stored.ObservedGeneration)
	}
}

func TestStaleRestoreCheckHandlesAbsentAndFailingClusters(t *testing.T) {
	ctx := context.Background()

	// No clusters -> no quarantine.
	store := NewMemoryStore()
	rec := NewReconciler(store, errProvisioner())
	quarantined, err := rec.DetectStaleRestore(ctx)
	if err != nil || quarantined {
		t.Fatalf("empty store: quarantined=%v err=%v", quarantined, err)
	}

	// A stored cluster whose backing resource is gone (NotFound) is
	// skipped, not fatal.
	if _, err := store.UpsertDesired(ctx, core.ClusterId("c"), testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec = NewReconciler(store, vanishingProvisioner(true)) // odd -> NotFound first
	quarantined, err = rec.DetectStaleRestore(ctx)
	if err != nil || quarantined {
		t.Fatalf("notfound-first: quarantined=%v err=%v", quarantined, err)
	}
	if q, _ := store.IsQuarantined(ctx); q {
		t.Fatal("store should not be quarantined")
	}

	// A backend error on observe propagates.
	rec = NewReconciler(store, errProvisioner())
	if _, err := rec.DetectStaleRestore(ctx); err == nil {
		t.Fatal("expected an error")
	} else if rerr, ok := err.(ReconcileError); !ok || rerr.Kind != ReconcileErrProvision {
		t.Fatalf("err = %#v, want ReconcileErrProvision", err)
	}
}

func TestRunLoopLogsPassErrorsAndStopsOnShutdown(t *testing.T) {
	// With a failing store, every tick logs reap/reconcile errors and
	// keeps going; cancellation still stops the loop promptly.
	store := NewFailingStore()
	store.Fail("List")
	store.Fail("ReapIntents")
	rec := NewReconciler(store, errProvisioner())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rec.Run(ctx, 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop should stop promptly on shutdown")
	}
}

// --- ported from store.rs (deferred to this task per T1's doc comment) ---

func TestQueueAssignmentResolvesFirstMatchingAllocation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	// No pools at all -> no assignment.
	q, err := queueAssignmentForProject(ctx, store, "p")
	if err != nil || q != nil {
		t.Fatalf("empty store: q=%v err=%v", q, err)
	}

	poolSpec := func(name string, elastic bool) core.PoolSpec {
		return core.PoolSpec{
			Name: name,
			Flavors: []core.FlavorSpec{{
				Name:      "cpu",
				Resources: map[string]string{"cpu": "4"},
			}},
			Cohort:            "research",
			FairSharingWeight: 1.0,
			Elastic:           elastic,
		}
	}
	alloc := func(pool, project string) core.AllocationSpec {
		return core.AllocationSpec{Pool: pool, Project: project, Namespace: project}
	}

	if _, err := store.UpsertPool(ctx, "gpu", poolSpec("gpu", true)); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	if err := store.UpsertAllocation(ctx, alloc("gpu", "p")); err != nil {
		t.Fatalf("upsert allocation: %v", err)
	}

	// The assignment carries the queue name (= project) and the pool's
	// elastic flag.
	q, err = queueAssignmentForProject(ctx, store, "p")
	if err != nil || q == nil {
		t.Fatalf("q=%v err=%v", q, err)
	}
	if q.QueueName != "p" {
		t.Fatalf("QueueName = %q, want p", q.QueueName)
	}
	if !q.Elastic {
		t.Fatal("expected Elastic = true")
	}

	// A project with no allocation stays queue-free.
	q, err = queueAssignmentForProject(ctx, store, "other")
	if err != nil || q != nil {
		t.Fatalf("other project: q=%v err=%v", q, err)
	}
}

// --- Supplementary: full reconcileOne actuation paths (not directly
// exercised by the Rust file's own test module) ---

func TestReconcileAppliesWhenNothingProvisioned(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	prov := &fakeProvisioner{
		observeFn: func(n int) (provision.ObservedCluster, error) {
			// Not provisioned before the apply; converged Running after
			// the apply (both the post-apply and next-pass re-observes).
			if n == 0 {
				return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
			}
			gen := uint64(1)
			return provision.ObservedCluster{ID: id, State: core.ClusterStateRunning, ObservedGeneration: &gen}, nil
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Action != ActionApplied {
		t.Fatalf("action = %v, want ActionApplied", out[0].Action)
	}
	if len(prov.applyKeys) != 1 {
		t.Fatalf("apply calls = %v, want 1", prov.applyKeys)
	}
	if prov.ensurePostureCalls != 1 {
		t.Fatalf("EnsureNamespacePosture calls = %d, want 1", prov.ensurePostureCalls)
	}
	// The outbox intent is now Applied.
	rec2, err := store.GetIntent(ctx, (&StoredCluster{ID: id, Generation: 1}).IntentKey())
	if err != nil || rec2 == nil || rec2.Status != IntentStatusApplied {
		t.Fatalf("intent = %+v, err=%v", rec2, err)
	}
}

func TestReconcileNoOpWhenConverged(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	gen := uint64(1)
	spec := testClusterSpec()
	fp := provision.OwnedSpecFingerprint(&spec)
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{ID: id, State: core.ClusterStateRunning, ObservedGeneration: &gen, SpecFingerprint: &fp}, nil
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Action != ActionNoOp {
		t.Fatalf("action = %v, want ActionNoOp", out[0].Action)
	}
	if len(prov.applyKeys) != 0 {
		t.Fatalf("expected no apply calls, got %v", prov.applyKeys)
	}
}

func TestReconcileTerminatesWhenDesiredTerminated(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetDesired(ctx, id, DesiredTerminated); err != nil {
		t.Fatalf("set desired: %v", err)
	}
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{ID: id, State: core.ClusterStateRunning}, nil
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Action != ActionTerminated {
		t.Fatalf("action = %v, want ActionTerminated", out[0].Action)
	}
	if len(prov.terminateCalls) != 1 || prov.terminateCalls[0] != id {
		t.Fatalf("terminate calls = %v", prov.terminateCalls)
	}
}

func TestReconcileSuspendsWhenDesiredSuspended(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetDesired(ctx, id, DesiredSuspended); err != nil {
		t.Fatalf("set desired: %v", err)
	}
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{ID: id, State: core.ClusterStateRunning}, nil
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Action != ActionSuspended {
		t.Fatalf("action = %v, want ActionSuspended", out[0].Action)
	}
	if len(prov.suspendCalls) != 1 || prov.suspendCalls[0] != id {
		t.Fatalf("suspend calls = %v", prov.suspendCalls)
	}
}

func TestReconcileQueuedSuspendedClusterIsNeverFought(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	spec := testClusterSpec()
	spec.Project = "p"
	if _, err := store.UpsertDesired(ctx, id, spec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetDesired(ctx, id, DesiredSuspended); err != nil {
		t.Fatalf("set desired: %v", err)
	}
	if _, err := store.UpsertPool(ctx, "gpu", core.PoolSpec{
		Name:              "gpu",
		Flavors:           []core.FlavorSpec{{Name: "cpu", Resources: map[string]string{"cpu": "4"}}},
		Cohort:            "research",
		FairSharingWeight: 1,
	}); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "gpu", Project: "p", Namespace: "p"}); err != nil {
		t.Fatalf("upsert allocation: %v", err)
	}
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{ID: id, State: core.ClusterStateSuspended}, nil
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Action != ActionNoOp {
		t.Fatalf("action = %v, want ActionNoOp (never fight the queue)", out[0].Action)
	}
	if len(prov.suspendCalls) != 0 {
		t.Fatalf("expected no suspend calls, got %v", prov.suspendCalls)
	}
}

func TestReconcileDegradedRaisesAlarmWithoutReapplying(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{ID: id, State: core.ClusterStateDegraded}, nil
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Action != ActionDrift {
		t.Fatalf("action = %v, want ActionDrift", out[0].Action)
	}
	if len(prov.applyKeys) != 0 {
		t.Fatalf("expected no apply calls while Degraded, got %v", prov.applyKeys)
	}
	c, err := store.Get(ctx, id)
	if err != nil || c == nil || c.Condition == nil || *c.Condition != core.DriftConditionDegraded {
		t.Fatalf("condition = %v, err=%v, want DriftConditionDegraded", c, err)
	}
}

func TestReconcileSpecDriftRaisesAlarm(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	gen := uint64(1)
	badFP := "not-the-real-fingerprint"
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{ID: id, State: core.ClusterStateRunning, ObservedGeneration: &gen, SpecFingerprint: &badFP}, nil
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Action != ActionDrift {
		t.Fatalf("action = %v, want ActionDrift", out[0].Action)
	}
	c, err := store.Get(ctx, id)
	if err != nil || c == nil || c.Condition == nil || *c.Condition != core.DriftConditionSpecDrift {
		t.Fatalf("condition = %v, err=%v, want DriftConditionSpecDrift", c, err)
	}
}

func TestReconcileRateLimitDefersToBackoff(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if _, err := store.UpsertDesired(ctx, core.ClusterId("a"), func() core.ClusterSpec { s := testClusterSpec(); s.Name = "a"; return s }()); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if _, err := store.UpsertDesired(ctx, core.ClusterId("b"), func() core.ClusterSpec { s := testClusterSpec(); s.Name = "b"; return s }()); err != nil {
		t.Fatalf("upsert b: %v", err)
	}
	prov := &fakeProvisioner{}
	// Capacity 1: only the first cluster's apply can proceed this pass.
	rec := NewReconcilerWithLimits(store, prov, RateLimits{Capacity: 1, RefillPerSec: 0})
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 2 {
		t.Fatalf("out = %+v, want 2 entries", out)
	}
	var applied, backoff int
	for _, res := range out {
		if res.Err != nil {
			t.Fatalf("unexpected error: %v", res.Err)
		}
		switch res.Action {
		case ActionApplied:
			applied++
		case ActionBackoff:
			backoff++
		default:
			t.Fatalf("unexpected action %v for %s", res.Action, res.ID)
		}
	}
	if applied != 1 || backoff != 1 {
		t.Fatalf("applied=%d backoff=%d, want 1 and 1", applied, backoff)
	}
	if len(prov.applyKeys) != 1 {
		t.Fatalf("apply calls = %v, want exactly 1", prov.applyKeys)
	}
}

func TestReconcileStaleIntentIsRejected(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	c, err := store.Get(ctx, id)
	if err != nil || c == nil {
		t.Fatalf("get: %v %v", c, err)
	}
	// Seed a conflicting outbox row under the same key the reconciler
	// will compute.
	if _, err := store.BeginIntent(ctx, c.IntentKey(), "a-different-fingerprint"); err != nil {
		t.Fatalf("begin intent: %v", err)
	}
	prov := &fakeProvisioner{}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 {
		t.Fatalf("out = %+v", out)
	}
	rerr, ok := out[0].Err.(ReconcileError)
	if !ok || rerr.Kind != ReconcileErrStaleIntent {
		t.Fatalf("err = %#v, want ReconcileErrStaleIntent", out[0].Err)
	}
	if len(prov.applyKeys) != 0 {
		t.Fatalf("expected no apply calls, got %v", prov.applyKeys)
	}
}

func TestReconcileQuarantineObservesButNeverActuates(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetQuarantine(ctx, true); err != nil {
		t.Fatalf("set quarantine: %v", err)
	}
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Action != ActionNoOp {
		t.Fatalf("action = %v, want ActionNoOp", out[0].Action)
	}
	if len(prov.applyKeys) != 0 {
		t.Fatalf("expected no actuation while quarantined, got %v", prov.applyKeys)
	}
	// Observation still happened exactly once (no re-observe step 3 for
	// the quarantine branch).
	if prov.observeCalls != 1 {
		t.Fatalf("observe calls = %d, want 1", prov.observeCalls)
	}
}

func TestReconcileBackoffThenClearsOnProgress(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Apply "succeeds" at the provider but the cluster stays gone after
	// the post-apply re-observe -> no progress -> backoff recorded.
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
		},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 1000)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v", out)
	}
	c, err := store.Get(ctx, id)
	if err != nil || c == nil {
		t.Fatalf("get: %v %v", c, err)
	}
	if c.FailureCount != 1 {
		t.Fatalf("failure count = %d, want 1", c.FailureCount)
	}
	if c.NextAttemptAt != 1000+backoffSecs(1) {
		t.Fatalf("next attempt at = %d, want %d", c.NextAttemptAt, 1000+backoffSecs(1))
	}

	// The backoff gate now blocks the next attempt until NextAttemptAt.
	out = rec.ReconcileAllAt(ctx, c.NextAttemptAt-1)
	if len(out) != 1 || out[0].Action != ActionBackoff {
		t.Fatalf("out = %+v, want ActionBackoff before NextAttemptAt", out)
	}

	// Once the cluster converges (observed Running at the desired
	// generation), the backoff state clears.
	gen := uint64(1)
	convergedSpec := testClusterSpec()
	fp := provision.OwnedSpecFingerprint(&convergedSpec)
	prov.observeFn = func(int) (provision.ObservedCluster, error) {
		return provision.ObservedCluster{ID: id, State: core.ClusterStateRunning, ObservedGeneration: &gen, SpecFingerprint: &fp}, nil
	}
	out = rec.ReconcileAllAt(ctx, c.NextAttemptAt)
	if len(out) != 1 || out[0].Err != nil || out[0].Action != ActionNoOp {
		t.Fatalf("out = %+v, want a clean ActionNoOp", out)
	}
	c, err = store.Get(ctx, id)
	if err != nil || c == nil {
		t.Fatalf("get: %v %v", c, err)
	}
	if c.FailureCount != 0 || c.NextAttemptAt != 0 {
		t.Fatalf("backoff not cleared: failure_count=%d next_attempt_at=%d", c.FailureCount, c.NextAttemptAt)
	}
}

// TestReconcileAllAtIsRaceClean exercises ReconcileAllAt concurrently from
// many goroutines against a shared Reconciler/store/rate-limited
// provisioner, mirroring this codebase's established -race discipline for
// the store layer (store_memory_fix1_test.go). This is the wave's most
// concurrency-sensitive code (token bucket, converged bookkeeping, store
// locking), so this test exists purely to keep the race detector honest.
func TestReconcileAllAtIsRaceClean(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	for i := 0; i < 10; i++ {
		id := core.ClusterId(string(rune('a' + i)))
		spec := testClusterSpec()
		spec.Name = string(id)
		if _, err := store.UpsertDesired(ctx, id, spec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			gen := uint64(1)
			return provision.ObservedCluster{State: core.ClusterStateRunning, ObservedGeneration: &gen}, nil
		},
	}
	rec := NewReconcilerWithLimits(store, prov, RateLimits{Capacity: 5, RefillPerSec: 5})

	var wg sync.WaitGroup
	var now atomic.Uint64
	now.Store(1)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				rec.ReconcileAllAt(ctx, now.Add(1))
			}
		}()
	}
	wg.Wait()
}

// --- Fix round 1 (review of commit 5e120e8) ---

// TestRunFiresFirstPassImmediately pins M1: time.NewTicker (unlike Rust's
// tokio::time::interval, whose first tick fires at creation time) waits a
// full interval before its first tick. Run must not inherit that: with a
// 1-hour interval, a pass must still happen almost immediately, or nothing
// (reap/intent-sweep/tombstone-sweep/reconcile — including resuming any
// crash-interrupted actuation) would happen for an hour after boot. The
// existing 10ms-interval tests are blind to this bug (a ticker with a 1h
// initial wait still "passes" the 60ms-sleep assertions in
// TestPoolRunLoopConvergesThenStopsOnShutdown-style tests, and this file's
// own 10ms-interval loop tests, because the interval itself is short
// enough that the bug is invisible) — this test uses a deliberately huge
// interval so only an immediate first pass can satisfy it.
func TestRunFiresFirstPassImmediately(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
		},
	}
	rec := NewReconciler(store, prov)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		rec.Run(runCtx, time.Hour)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
waitForFirstPass:
	for {
		prov.mu.Lock()
		calls := prov.observeCalls
		prov.mu.Unlock()
		if calls > 0 {
			break waitForFirstPass
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("expected an immediate first pass (interval is 1h); got none within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop should stop promptly on shutdown")
	}
}

// TestReconcileFailsClosedWhenNamespacePostureErrors pins M2(a): a
// namespace-posture error must block the Apply call entirely (fail-closed,
// #56/#62) and must not mark the outbox intent Applied.
func TestReconcileFailsClosedWhenNamespacePostureErrors(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
		},
		ensurePostureErr: provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "posture apply failed"},
	}
	rec := NewReconciler(store, prov)
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 {
		t.Fatalf("out = %+v", out)
	}
	rerr, ok := out[0].Err.(ReconcileError)
	if !ok || rerr.Kind != ReconcileErrProvision {
		t.Fatalf("err = %#v, want ReconcileErrProvision", out[0].Err)
	}
	if len(prov.applyKeys) != 0 {
		t.Fatalf("expected Apply never called when posture fails closed, got %v", prov.applyKeys)
	}
	if prov.ensurePostureCalls != 1 {
		t.Fatalf("EnsureNamespacePosture calls = %d, want 1", prov.ensurePostureCalls)
	}
	c, err := store.Get(ctx, id)
	if err != nil || c == nil {
		t.Fatalf("get: %v %v", c, err)
	}
	rec1, err := store.GetIntent(ctx, c.IntentKey())
	if err != nil || rec1 == nil || rec1.Status != IntentStatusPending {
		t.Fatalf("intent = %+v, err=%v, want Pending (a posture failure must not complete it)", rec1, err)
	}
}

// TestReconcilePendingIntentReplaysAndCompletesAfterApplyFailure pins
// M2(b): a failure between BeginIntent and a successful Apply leaves a
// Pending outbox row. The next pass's BeginIntent for the same key/
// fingerprint finds it (IntentOutcomeProceed{Replay: true}) and the
// reconciler re-actuates (idempotent SSA) rather than refusing — and this
// time completes it.
func TestReconcilePendingIntentReplaysAndCompletesAfterApplyFailure(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var failApply atomic.Bool
	failApply.Store(true)
	prov := &fakeProvisioner{
		observeFn: func(int) (provision.ObservedCluster, error) {
			return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
		},
		applyFn: func(_ core.ClusterId, _ *core.ClusterSpec, generation uint64, _ string, _ *provision.QueueAssignment) (provision.ApplyResponse, error) {
			if failApply.Load() {
				return provision.ApplyResponse{}, provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "injected apply failure"}
			}
			return provision.ApplyResponse{Generation: generation}, nil
		},
	}
	rec := NewReconciler(store, prov)

	// Pass 1: BeginIntent opens a Pending row, Apply fails. The error must
	// surface and the intent must stay Pending (not Applied).
	out := rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err == nil {
		t.Fatalf("out = %+v, want an apply error", out)
	}
	c, err := store.Get(ctx, id)
	if err != nil || c == nil {
		t.Fatalf("get: %v %v", c, err)
	}
	key := c.IntentKey()
	rec1, err := store.GetIntent(ctx, key)
	if err != nil || rec1 == nil || rec1.Status != IntentStatusPending {
		t.Fatalf("intent after failed apply = %+v, err=%v, want Pending", rec1, err)
	}
	if len(prov.applyKeys) != 1 {
		t.Fatalf("apply calls = %v, want exactly 1", prov.applyKeys)
	}

	// Pass 2: the backend recovers. The same key/fingerprint replays and
	// completes.
	failApply.Store(false)
	out = rec.ReconcileAllAt(ctx, 0)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("out = %+v, want a clean pass", out)
	}
	if out[0].Action != ActionApplied {
		t.Fatalf("action = %v, want ActionApplied", out[0].Action)
	}
	if len(prov.applyKeys) != 2 {
		t.Fatalf("apply calls = %v, want exactly 2 (the replay)", prov.applyKeys)
	}
	rec2, err := store.GetIntent(ctx, key)
	if err != nil || rec2 == nil || rec2.Status != IntentStatusApplied {
		t.Fatalf("intent after replay = %+v, err=%v, want Applied", rec2, err)
	}
}

// TestReapTerminatedDefersRowRemovalWhenNetpolReapFails pins M2(c): a
// failed per-cluster NetworkPolicy reap must defer the tombstone row
// removal (leave it for the next pass to retry) rather than dropping the
// last record the netpol reap is owed.
func TestReapTerminatedDefersRowRemovalWhenNetpolReapFails(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c")
	if _, err := store.UpsertDesired(ctx, id, testClusterSpec()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetDesired(ctx, id, DesiredTerminated); err != nil {
		t.Fatalf("set desired: %v", err)
	}
	prov := &fakeProvisioner{
		reapNetpolErr: provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "injected netpol reap failure"},
	}
	rec := NewReconciler(store, prov).WithTerminatedRetention(0)

	removed, err := rec.ReapTerminated(ctx, NowUnix()+10)
	if err != nil {
		t.Fatalf("reap terminated: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none (netpol reap failed, row removal deferred)", removed)
	}
	if len(prov.reapNetpolCalls) != 1 || prov.reapNetpolCalls[0] != id {
		t.Fatalf("reap netpol calls = %v, want [%s]", prov.reapNetpolCalls, id)
	}
	if c, err := store.Get(ctx, id); err != nil || c == nil {
		t.Fatalf("cluster row should still exist (deferred, not purged): %v %v", c, err)
	}

	// Once the backend recovers, the deferred row is purged.
	prov.reapNetpolErr = nil
	removed, err = rec.ReapTerminated(ctx, NowUnix()+10)
	if err != nil {
		t.Fatalf("reap terminated: %v", err)
	}
	if len(removed) != 1 || removed[0] != id.String() {
		t.Fatalf("removed = %v, want [%s]", removed, id)
	}
	if c, _ := store.Get(ctx, id); c != nil {
		t.Fatal("cluster row should be purged once the netpol reap succeeds")
	}
}

// Requirement 4: the queue lookup is split by pool purpose. A compute
// cluster never lands in a serving pool's queue (even when the serving
// allocation is the only one the project has), and a service's queue is
// the serving pool's `<project>-serving` LocalQueue.
func TestQueueAssignmentIsSplitByPoolPurpose(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	pool := func(name string, purpose core.PoolPurpose) core.PoolSpec {
		return core.PoolSpec{
			Name:              name,
			Flavors:           []core.FlavorSpec{{Name: "cpu", Resources: map[string]string{"cpu": "4"}}},
			Cohort:            "research",
			FairSharingWeight: 1.0,
			Purpose:           purpose,
		}
	}
	if _, err := store.UpsertPool(ctx, "serve", pool("serve", core.PoolPurposeServing)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "serve", Project: "p", Namespace: "p"}); err != nil {
		t.Fatal(err)
	}

	// Only a serving allocation: clusters stay queue-free, services are
	// admitted through p-serving.
	if q, err := queueAssignmentForProject(ctx, store, "p"); err != nil || q != nil {
		t.Fatalf("compute lookup with only a serving allocation: q=%v err=%v, want nil", q, err)
	}
	q, err := QueueAssignmentForProjectPurpose(ctx, store, "p", core.PoolPurposeServing)
	if err != nil || q == nil || q.QueueName != "p-serving" {
		t.Fatalf("serving lookup: q=%+v err=%v, want p-serving", q, err)
	}

	// Add a compute allocation: each purpose resolves its own pool.
	if _, err := store.UpsertPool(ctx, "cpu", pool("cpu", "")); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "cpu", Project: "p", Namespace: "p"}); err != nil {
		t.Fatal(err)
	}
	q, err = queueAssignmentForProject(ctx, store, "p")
	if err != nil || q == nil || q.QueueName != "p" {
		t.Fatalf("compute lookup: q=%+v err=%v, want p", q, err)
	}
	q, err = QueueAssignmentForProjectPurpose(ctx, store, "p", core.PoolPurposeServing)
	if err != nil || q == nil || q.QueueName != "p-serving" {
		t.Fatalf("serving lookup after compute pool added: q=%+v err=%v", q, err)
	}
}
