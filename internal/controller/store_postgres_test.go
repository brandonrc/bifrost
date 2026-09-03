package controller_test

// Backend-specific tests for PostgresStore. RunConformance (storetest) is
// the primary acceptance gate, wired below against a fresh per-invocation
// schema; the two tests after it port the scenarios storetest's package
// doc comment lists as deliberately excluded from the backend-agnostic
// conformance suite because they exercise Postgres's own
// advisory-lock-based transaction-serialization strategy, not
// Store-interface-level behavior — the ported oracle is
// mobula-controller/tests/store.rs's
// `postgres_concurrent_distinct_upserts_do_not_collapse_generation` and
// `postgres_concurrent_audit_appends_keep_one_chain` (retired Rust
// reference; cited here only for traceability).
//
// Every test in this file is gated on BIFROST_TEST_POSTGRES_URL: t.Skip
// cleanly when it is unset (no local Postgres available), never t.Fatal —
// CI wires a postgres:16 service container and sets the env var, so these
// run on every PR there; a local run needs e.g.
//
//	docker run -d --rm -p 5433:5432 -e POSTGRES_PASSWORD=postgres postgres:16
//	BIFROST_TEST_POSTGRES_URL=postgres://postgres:postgres@localhost:5433/postgres \
//	    go test ./internal/controller/...

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/controller/storetest"
	"github.com/brandonrc/bifrost/internal/core"
)

// nextPgSchema is a process-wide counter so parallel test binaries (or
// parallel subtests within one) never collide on a schema name.
var nextPgSchema atomic.Uint64

// postgresTestURL returns BIFROST_TEST_POSTGRES_URL, skipping the calling
// test cleanly when it is unset — the gate the brief requires: these tests
// must never fail a local run that has no Postgres reachable, only skip.
func postgresTestURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("BIFROST_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("BIFROST_TEST_POSTGRES_URL not set; skipping PostgresStore test " +
			"(e.g. postgres://postgres:postgres@localhost:5433/postgres)")
	}
	return url
}

// newTestPostgresStore opens a connection pool pinned (via AfterConnect)
// to a fresh schema created here and dropped in t.Cleanup, mirroring
// tests/store.rs's postgres_store() helper: per-test-invocation schema
// isolation so parallel scenarios/subtests never share rows. Each call —
// including each of RunConformance's per-section newStore() invocations —
// gets its own schema, matching the Rust reference's "fresh PostgresStore
// per #[tokio::test]" posture at the Go granularity RunConformance uses
// (one fresh Store per t.Run section).
func newTestPostgresStore(t *testing.T, url string) *controller.PostgresStore {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("conf_%d_%d", os.Getpid(), nextPgSchema.Add(1))

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse postgres url: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, "SET search_path TO "+schema)
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	store, err := controller.NewPostgresStoreFromPool(ctx, pool)
	if err != nil {
		pool.Close()
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	})
	return store
}

// TestPostgresStoreConformance is the acceptance gate for Task 4: the full
// store-conformance suite (ported from mobula-controller/tests/store.rs,
// see storetest's package doc comment) must pass against PostgresStore
// exactly as it does against MemoryStore and SqliteStore. Each subtest
// section gets its own fresh per-test schema (see newTestPostgresStore),
// mirroring the Rust reference's per-test schema isolation.
func TestPostgresStoreConformance(t *testing.T) {
	url := postgresTestURL(t)
	storetest.RunConformance(t, func() controller.Store {
		return newTestPostgresStore(t, url)
	})
}

// TestPostgresConcurrentDistinctUpsertsDoNotCollapseGeneration ports
// store_postgres.rs's
// postgres_concurrent_distinct_upserts_do_not_collapse_generation
// (tests/store.rs:1385-1407), one of the two scenarios storetest excludes
// because it is Postgres's own advisory-lock transaction-serialization
// strategy under test, not Store-interface behavior: #42's Postgres side —
// N concurrent upserts of N PAIRWISE DISTINCT specs on the same cluster id
// must each produce their own generation bump (1 -> 1+N), never collapse
// two of them into one (two transactions reading the same pre-bump
// generation before either commits). The pg_advisory_xact_lock(hashtext($1))
// in PostgresStore.UpsertDesired (store_postgres.go) is what this test
// exercises: it serializes across connections, unlike SqliteStore's
// single-process BEGIN IMMEDIATE, so this is the property that actually
// matters for a multi-replica Bifrost deployment.
//
// N=20 with a close(start)-channel barrier (not just "launch 2 goroutines
// and wg.Wait") so every upsert actually lands in the microseconds-wide
// contention window: a 2-goroutine version can pass even with the
// advisory lock removed entirely, because the two Begin/Lock/Read calls
// rarely interleave tightly enough to race — fix-round-1 review measured
// the lockless build only collapsing at ~20-way contention. Every one of
// the N specs differs from the seed and from every other (replicas
// 2..N+1), so the final generation is deterministically 1+N regardless of
// commit order — no flake window in the assertion itself, only in whether
// the lock does its job.
func TestPostgresConcurrentDistinctUpsertsDoNotCollapseGeneration(t *testing.T) {
	url := postgresTestURL(t)
	store := newTestPostgresStore(t, url)
	ctx := context.Background()

	id := core.ClusterId("demo")
	if _, err := store.UpsertDesired(ctx, id, postgresSpecFixture(1)); err != nil {
		t.Fatalf("seed upsert (gen 1): %v", err)
	}

	const n = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := store.UpsertDesired(ctx, id, postgresSpecFixture(uint32(i+2)))
			errs[i] = err
		}(i)
	}
	close(start) // release all N goroutines at once, maximizing contention
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent upsert %d: %v", i, err)
		}
	}

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	want := uint64(1 + n)
	if got.Generation != want {
		t.Fatalf("Generation = %d, want %d (every distinct concurrent spec change must yield its own generation bump, not a collapse)", got.Generation, want)
	}
}

// TestPostgresConcurrentAuditAppendsKeepOneChain ports store_postgres.rs's
// postgres_concurrent_audit_appends_keep_one_chain (tests/store.rs:1343-
// 1364), the second scenario storetest excludes for the same reason:
// pg_advisory_xact_lock(hashtext('audit_chain')) in
// PostgresStore.RecordAudit must serialize concurrent RecordAudit calls
// ACROSS CONNECTIONS — the property an in-process mutex (SqliteStore's
// auditMu) cannot provide but a multi-replica Bifrost deployment needs. A
// regression here wouldn't fail loudly: it would silently fork the chain
// (two rows both computed from the same "newest" hash) or skip a seq. This
// test is that tripwire: seqs must come back gapless (1..N) and the whole
// window must verify.
func TestPostgresConcurrentAuditAppendsKeepOneChain(t *testing.T) {
	url := postgresTestURL(t)
	store := newTestPostgresStore(t, url)
	ctx := context.Background()

	const n = 20
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

func postgresSpecFixture(replicas uint32) core.ClusterSpec {
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

// TestPostgresMigratesUsageSamplesOwnerColumn is the Postgres twin of
// TestSqliteMigratesUsageSamplesOwnerColumn: a schema whose usage_samples
// predates the owner column gains it on NewPostgresStoreFromPool and reads
// its old rows back as unattributed.
func TestPostgresMigratesUsageSamplesOwnerColumn(t *testing.T) {
	url := postgresTestURL(t)
	ctx := context.Background()
	schema := fmt.Sprintf("conf_%d_%d", os.Getpid(), nextPgSchema.Add(1))

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse postgres url: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, "SET search_path TO "+schema)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE usage_samples (
	    ts BIGINT NOT NULL, project TEXT NOT NULL, pool TEXT NOT NULL,
	    resource TEXT NOT NULL, quantity DOUBLE PRECISION NOT NULL, source TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO usage_samples VALUES (100, 'proj-a', 'gpu', 'cpu', 4.0, 'kueue_ledger')"); err != nil {
		t.Fatal(err)
	}

	store, err := controller.NewPostgresStoreFromPool(ctx, pool)
	if err != nil {
		t.Fatalf("apply schema over legacy table: %v", err)
	}
	old, err := store.UsageSamples(ctx, nil, nil, nil, 0, ^uint64(0))
	if err != nil || len(old) != 1 || old[0].Owner != "" {
		t.Fatalf("legacy row after migration: %+v err=%v", old, err)
	}
	owner := "alice"
	if err := store.RecordUsageSamples(ctx, []controller.UsageSample{
		{Ts: 200, Project: "proj-a", Pool: "gpu", Resource: "cpu", Quantity: 1.0, Source: controller.UsageSourceKueueLedger, Owner: owner},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.UsageSamples(ctx, nil, nil, &owner, 0, ^uint64(0)); err != nil || len(got) != 1 || got[0].Ts != 200 {
		t.Fatalf("owner filter after migration: %+v err=%v", got, err)
	}
	// Re-applying is idempotent.
	if _, err := controller.NewPostgresStoreFromPool(ctx, pool); err != nil {
		t.Fatalf("re-apply schema: %v", err)
	}
}
