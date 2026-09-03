package controller_test

// Backend-specific tests for SqliteStore. RunConformance (storetest) is
// the primary acceptance gate, wired below; the two tests after it port
// the scenarios storetest's package doc comment lists as deliberately
// excluded from the backend-agnostic conformance suite because they
// exercise SQLite's own transaction-serialization strategy and
// close+reopen durability, not Store-interface-level behavior — the
// ported oracle is the predecessor's controller crate, tests/store.rs's
// `sqlite_persists_across_reopen` and
// `concurrent_distinct_upserts_do_not_collapse_generation` (retired Rust
// reference; cited here only for traceability).

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/controller/storetest"
	"github.com/brandonrc/bifrost/internal/core"
)

func newTestSqliteStore(t *testing.T, path string) *controller.SqliteStore {
	t.Helper()
	store, err := controller.NewSqliteStore(context.Background(), path)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close sqlite store: %v", err)
		}
	})
	return store
}

// TestSqliteStoreConformance is the acceptance gate for Task 3: the full
// store-conformance suite (ported from the predecessor's controller crate, tests/store.rs,
// see storetest's package doc comment) must pass against SqliteStore
// exactly as it does against MemoryStore. Each subtest section gets its
// own fresh temp-file database, mirroring the Rust reference's fresh
// SqliteStore per #[tokio::test].
func TestSqliteStoreConformance(t *testing.T) {
	i := 0
	storetest.RunConformance(t, func() controller.Store {
		i++
		dir := t.TempDir()
		return newTestSqliteStore(t, filepath.Join(dir, "conformance.db"))
	})
}

// TestSqlitePersistsAcrossReopen ports store_sqlite.rs's
// sqlite_persists_across_reopen (tests/store.rs:1171-1195): desired state,
// observations, and the audit chain written before a close must be
// readable after reopening the same database file — the point of a
// file-backed store that an in-memory backend cannot verify at all.
func TestSqlitePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	id := core.ClusterId("demo")

	func() {
		store, err := controller.NewSqliteStore(ctx, path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = store.Close() }()

		if _, err := store.UpsertDesired(ctx, id, reopenSpecFixture(2)); err != nil {
			t.Fatalf("upsert desired: %v", err)
		}
		running := core.ClusterStateRunning
		if err := store.RecordObservation(ctx, id, &running, 1); err != nil {
			t.Fatalf("record observation: %v", err)
		}
		if _, err := store.RecordAudit(ctx, &core.AuditEvent{Ts: 100, Decision: core.AuditDecisionAllow}); err != nil {
			t.Fatalf("record audit: %v", err)
		}
	}()

	// Reopen: desired state, the observation, and the audit chain all
	// survive (ADR-0004: durable).
	store, err := controller.NewSqliteStore(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store.Close() }()

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get after reopen: %v %v", got, err)
	}
	if got.Spec.WorkerGroups[0].Replicas != 2 {
		t.Fatalf("Replicas = %d, want 2", got.Spec.WorkerGroups[0].Replicas)
	}
	if got.ObservedState == nil || *got.ObservedState != core.ClusterStateRunning {
		t.Fatalf("ObservedState = %v, want Running", got.ObservedState)
	}

	window, err := store.AuditChain(ctx, nil, 100)
	if err != nil {
		t.Fatalf("audit chain after reopen: %v", err)
	}
	if len(window.Rows) != 1 {
		t.Fatalf("len(window.Rows) = %d, want 1", len(window.Rows))
	}
	if v := controller.VerifyAuditChain(window.Head, window.Rows); !v.OK() {
		t.Fatalf("chain does not verify after reopen: %+v", v)
	}
}

// TestSqliteConcurrentDistinctUpsertsDoNotCollapseGeneration ports
// store_sqlite.rs's concurrent_distinct_upserts_do_not_collapse_generation
// (tests/store.rs:1138-1169), one of the two scenarios storetest excludes
// because it is SQLite's own transaction-serialization strategy under
// test, not Store-interface behavior: #42 — two concurrent upserts of
// DIFFERENT specs on the same cluster id must produce two distinct
// generation bumps (1 -> 3), never collapse into one (both transactions
// reading the pre-bump generation under a lazily-upgraded lock) and never
// leak SQLITE_BUSY to the caller. sqliteDSN's _txlock=immediate +
// _busy_timeout=5000 (store_sqlite.go) is what this test exercises: BEGIN
// IMMEDIATE takes the write lock at transaction start, so the second
// upsert blocks for the first to commit instead of racing it.
func TestSqliteConcurrentDistinctUpsertsDoNotCollapseGeneration(t *testing.T) {
	ctx := context.Background()
	store := newTestSqliteStore(t, filepath.Join(t.TempDir(), "race.db"))

	id := core.ClusterId("demo")
	if _, err := store.UpsertDesired(ctx, id, reopenSpecFixture(1)); err != nil {
		t.Fatalf("seed upsert (gen 1): %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	replicas := []uint32{2, 5}
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.UpsertDesired(ctx, id, reopenSpecFixture(replicas[i]))
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent upsert %d: %v (SQLITE_BUSY or another error must not leak to the caller)", i, err)
		}
	}

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Generation != 3 {
		t.Fatalf("Generation = %d, want 3 (two distinct concurrent spec changes must yield two generation bumps, not a collapse)", got.Generation)
	}
}

// TestSqliteConcurrentWritersNoBusyLeak goes beyond the single-cluster
// race above, closer to the brief's "exercise real concurrent writers"
// framing: N goroutines upsert N DISTINCT clusters concurrently, each
// changing its spec twice. SQLite serializes all writers at the
// whole-database level (not per-row), so this contends every goroutine
// against every other one; the busy-timeout/BEGIN-IMMEDIATE posture must
// absorb that without any goroutine observing SQLITE_BUSY or another
// driver error, and every cluster must land at generation 2 (not 1 — a
// silently dropped second write would be a correctness bug, not just a
// leaked error).
func TestSqliteConcurrentWritersNoBusyLeak(t *testing.T) {
	ctx := context.Background()
	store := newTestSqliteStore(t, filepath.Join(t.TempDir(), "concurrent-writers.db"))

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := core.ClusterId(clusterIdForIndex(i))
			if _, err := store.UpsertDesired(ctx, id, reopenSpecFixture(1)); err != nil {
				errs[i] = err
				return
			}
			_, err := store.UpsertDesired(ctx, id, reopenSpecFixture(2))
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v (SQLITE_BUSY or another error must not leak to the caller)", i, err)
		}
	}

	for i := range n {
		id := core.ClusterId(clusterIdForIndex(i))
		got, err := store.Get(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("get %s: %v %v", id, got, err)
		}
		if got.Generation != 2 {
			t.Fatalf("%s: Generation = %d, want 2", id, got.Generation)
		}
	}
}

// TestSqliteConcurrentAuditAppendsKeepOneChain exercises auditMu directly
// (store_sqlite.go's audit_lock equivalent): N concurrent RecordAudit
// calls must never interleave their read-newest-hash/compute/insert
// sequence — a regression there wouldn't fail loudly, it would silently
// fork the chain (two rows both computed from the same "newest" hash) or
// skip a seq, which VerifyAuditChain would only catch if something later
// happened to check it. This test is that tripwire: seqs must come back
// gapless (1..N, in some order) and the whole window must verify.
func TestSqliteConcurrentAuditAppendsKeepOneChain(t *testing.T) {
	ctx := context.Background()
	store := newTestSqliteStore(t, filepath.Join(t.TempDir(), "audit-race.db"))

	const n = 40
	var wg sync.WaitGroup
	seqs := make([]uint64, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seq, err := store.RecordAudit(ctx, &core.AuditEvent{
				Ts: uint64(i), Decision: core.AuditDecisionAllow,
			})
			seqs[i] = seq
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("record audit %d: %v", i, err)
		}
	}

	seen := make(map[uint64]bool, n)
	for _, seq := range seqs {
		if seen[seq] {
			t.Fatalf("duplicate seq %d among concurrent RecordAudit calls (chain fork)", seq)
		}
		seen[seq] = true
	}
	for want := uint64(1); want <= n; want++ {
		if !seen[want] {
			t.Fatalf("seq %d missing: seqs must be gapless 1..%d, got %v", want, n, seqs)
		}
	}

	window, err := store.AuditChain(ctx, nil, n+1)
	if err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	if len(window.Rows) != n {
		t.Fatalf("len(window.Rows) = %d, want %d", len(window.Rows), n)
	}
	if v := controller.VerifyAuditChain(window.Head, window.Rows); !v.OK() {
		t.Fatalf("chain does not verify after concurrent appends: %+v", v)
	}
}

func clusterIdForIndex(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return "writer-" + string(letters[i%len(letters)]) + string(letters[(i/len(letters))%len(letters)])
}

func reopenSpecFixture(replicas uint32) core.ClusterSpec {
	return core.ClusterSpec{
		Engine:     core.DefaultEngine,
		Name:       "demo",
		Project:    "demo",
		RayVersion: "2.57.0",
		Image:      "rayproject/ray:2.57.0",
		HeadCpu:    "1",
		HeadMemory: "2Gi",
		WorkerGroups: []core.WorkerGroup{{
			Name:        "cpu",
			Cpu:         "1",
			Memory:      "2Gi",
			MinReplicas: 0,
			MaxReplicas: 4,
			Replicas:    replicas,
		}},
	}
}

// TestSqliteMigratesUsageSamplesOwnerColumn: the first real additive
// migration (requirement 14). A database created before usage_samples had
// an owner column must open, gain the column, read its old rows back as
// unattributed (”), and accept owned samples afterwards.
func TestSqliteMigratesUsageSamplesOwnerColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `
		CREATE TABLE usage_samples (
		    ts       INTEGER NOT NULL,
		    project  TEXT NOT NULL,
		    pool     TEXT NOT NULL,
		    resource TEXT NOT NULL,
		    quantity REAL NOT NULL,
		    source   TEXT NOT NULL
		);
		INSERT INTO usage_samples VALUES (100, 'proj-a', 'gpu', 'cpu', 4.0, 'kueue_ledger');
	`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store := newTestSqliteStore(t, path)
	old, err := store.UsageSamples(ctx, nil, nil, nil, 0, ^uint64(0))
	if err != nil || len(old) != 1 || old[0].Owner != "" || old[0].Quantity != 4.0 {
		t.Fatalf("legacy row after migration: %+v err=%v", old, err)
	}
	if err := store.RecordUsageSamples(ctx, []controller.UsageSample{
		{Ts: 200, Project: "proj-a", Pool: "gpu", Resource: "cpu", Quantity: 1.0, Source: controller.UsageSourceKueueLedger, Owner: "alice"},
	}); err != nil {
		t.Fatalf("record owned sample: %v", err)
	}
	owner := "alice"
	alice, err := store.UsageSamples(ctx, nil, nil, &owner, 0, ^uint64(0))
	if err != nil || len(alice) != 1 || alice[0].Ts != 200 {
		t.Fatalf("owner filter after migration: %+v err=%v", alice, err)
	}

	// Reopening an already-migrated database must be a no-op, not a
	// duplicate-column error.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := controller.NewSqliteStore(ctx, path)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	_ = again.Close()
}
