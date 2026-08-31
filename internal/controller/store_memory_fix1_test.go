package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

// Fix round 1 (review of commit 23055b9): tests for the M1/L1/L3 fixes in
// clone.go and store_memory.go. See clone.go's package comment for the
// aliasing bug these guard against.

func TestMemoryStoreGetReturnsIndependentCopy(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c1")
	gpu := "1"
	ttl := uint64(100)
	spec := core.ClusterSpec{
		Name:         "c1",
		WorkerGroups: []core.WorkerGroup{{Name: "wg", Gpu: &gpu}},
		TtlSeconds:   &ttl,
	}
	if _, err := store.UpsertDesired(ctx, id, spec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	// Mutate the returned copy's container/pointer fields.
	got.Spec.WorkerGroups[0].Name = "mutated"
	*got.Spec.TtlSeconds = 999
	now := NowUnix()
	got.TerminatedAt = &now

	got2, err := store.Get(ctx, id)
	if err != nil || got2 == nil {
		t.Fatalf("get2: %v %v", got2, err)
	}
	if got2.Spec.WorkerGroups[0].Name != "wg" {
		t.Fatalf("WorkerGroups[0].Name = %q, want unaffected \"wg\"", got2.Spec.WorkerGroups[0].Name)
	}
	if *got2.Spec.TtlSeconds != 100 {
		t.Fatalf("TtlSeconds = %d, want unaffected 100", *got2.Spec.TtlSeconds)
	}
	if got2.TerminatedAt != nil {
		t.Fatal("TerminatedAt leaked from a mutated first Get() copy")
	}
}

func TestMemoryStoreUpsertDesiredCopiesCallerSpec(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c1")
	gpu := "1"
	ttl := uint64(100)
	spec := core.ClusterSpec{
		Name:         "c1",
		WorkerGroups: []core.WorkerGroup{{Name: "wg", Gpu: &gpu}},
		TtlSeconds:   &ttl,
	}
	if _, err := store.UpsertDesired(ctx, id, spec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Mutate the caller's own spec (and its pointee memory) after the
	// call returns.
	spec.WorkerGroups[0].Name = "mutated-by-caller"
	gpu = "mutated-gpu"
	ttl = 999

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Spec.WorkerGroups[0].Name != "wg" {
		t.Fatalf("WorkerGroups[0].Name = %q, want unaffected \"wg\"", got.Spec.WorkerGroups[0].Name)
	}
	if *got.Spec.WorkerGroups[0].Gpu != "1" {
		t.Fatalf("Gpu = %q, want unaffected \"1\"", *got.Spec.WorkerGroups[0].Gpu)
	}
	if *got.Spec.TtlSeconds != 100 {
		t.Fatalf("TtlSeconds = %d, want unaffected 100", *got.Spec.TtlSeconds)
	}
}

func TestMemoryStoreRecordAuditCopiesCallerEventAndChainStaysClean(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	subject := "alice"
	event := &core.AuditEvent{
		Ts:           100,
		Decision:     core.AuditDecisionAllow,
		Subject:      &subject,
		GrantedRoles: []string{"viewer"},
	}
	if _, err := store.RecordAudit(ctx, event); err != nil {
		t.Fatalf("record audit: %v", err)
	}

	// Mutate the caller's event (and its backing memory) after the call
	// returns. This must not corrupt the stored row: its ChainHash was
	// computed once, at RecordAudit time, over the value as it was then.
	subject = "mallory"
	event.GrantedRoles[0] = "admin"
	event.Ts = 999

	window, err := store.AuditChain(ctx, nil, 10)
	if err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	if len(window.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(window.Rows))
	}
	// The sharpest consequence named in review: a spurious tamper report
	// caused purely by the caller mutating its own event after the call,
	// not by any real tamper of the stored chain.
	v := VerifyAuditChain(window.Head, window.Rows)
	if !v.OK() {
		t.Fatalf("expected chain to verify after caller-side mutation, first broken seq = %v", v.FirstBrokenSeq)
	}

	if window.Rows[0].Event.Subject == nil || *window.Rows[0].Event.Subject != "alice" {
		t.Fatalf("stored Subject = %v, want unaffected \"alice\"", window.Rows[0].Event.Subject)
	}
	if len(window.Rows[0].Event.GrantedRoles) == 0 || window.Rows[0].Event.GrantedRoles[0] != "viewer" {
		t.Fatalf("stored GrantedRoles = %v, want unaffected [\"viewer\"]", window.Rows[0].Event.GrantedRoles)
	}
	if window.Rows[0].Event.Ts != 100 {
		t.Fatalf("stored Ts = %d, want unaffected 100", window.Rows[0].Event.Ts)
	}

	// ListAudit takes the same egress path; pin it too.
	rows, _, err := store.ListAudit(ctx, core.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 1 || rows[0].Event.Subject == nil || *rows[0].Event.Subject != "alice" {
		t.Fatalf("ListAudit rows = %+v, want one row with unaffected Subject \"alice\"", rows)
	}
}

// TestMemoryStoreConcurrentGetIsRaceClean exercises Get() from many
// goroutines, each mutating only the copy it individually received. Before
// the fix (Get returning a shallow copy whose WorkerGroups slice aliased
// the same backing array as the stored StoredCluster, shared by every
// other Get() caller too), this test would fail under -race: every
// goroutine's "own" copy secretly shared memory with every other
// goroutine's copy and with the mutex-guarded store itself. After the fix,
// each Get() call clones independently, so concurrent Get()+mutate is
// race-clean by construction — no goroutine ever touches another
// goroutine's or the store's memory.
func TestMemoryStoreConcurrentGetIsRaceClean(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c1")
	gpu := "1"
	spec := core.ClusterSpec{Name: "c1", WorkerGroups: []core.WorkerGroup{{Name: "wg", Gpu: &gpu}}}
	if _, err := store.UpsertDesired(ctx, id, spec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			got, err := store.Get(ctx, id)
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			if got == nil {
				t.Error("get: unexpected nil")
				return
			}
			// Mutate this goroutine's own returned copy only.
			got.Spec.WorkerGroups[0].Name = "goroutine-local"
			*got.Spec.WorkerGroups[0].Gpu = "goroutine-local-gpu"
		}()
	}
	wg.Wait()

	// No goroutine's mutation of its own copy may have reached the store.
	final, err := store.Get(ctx, id)
	if err != nil || final == nil {
		t.Fatalf("final get: %v %v", final, err)
	}
	if final.Spec.WorkerGroups[0].Name != "wg" {
		t.Fatalf("Name = %q, want unaffected \"wg\"", final.Spec.WorkerGroups[0].Name)
	}
	if *final.Spec.WorkerGroups[0].Gpu != "1" {
		t.Fatalf("Gpu = %q, want unaffected \"1\"", *final.Spec.WorkerGroups[0].Gpu)
	}
}

// TestMemoryStoreListMethodsReturnNonNilEmpty pins L1: every List*-shaped
// method (plus UsageSamples and AuditChain.Rows) returns a non-nil empty
// slice on a store with nothing in it, matching Rust's Vec::default()
// semantics (always `[]`, never `null`, on the wire) and this package's
// four other List* methods that already did this correctly (List,
// ListJobs, ListPools, ListLocalUsers).
func TestMemoryStoreListMethodsReturnNonNilEmpty(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	if v, err := store.List(ctx); err != nil || v == nil {
		t.Fatalf("List: %v %v", v, err)
	}
	if v, err := store.ListJobs(ctx); err != nil || v == nil {
		t.Fatalf("ListJobs: %v %v", v, err)
	}
	if v, err := store.ListPools(ctx); err != nil || v == nil {
		t.Fatalf("ListPools: %v %v", v, err)
	}
	if v, err := store.ListAllocations(ctx, "pool"); err != nil || v == nil {
		t.Fatalf("ListAllocations: %v %v", v, err)
	}
	if v, err := store.UsageSamples(ctx, nil, nil, 0, 1<<62); err != nil || v == nil {
		t.Fatalf("UsageSamples: %v %v", v, err)
	}
	if rows, next, err := store.ListAudit(ctx, core.AuditFilter{}); err != nil || rows == nil || next != nil {
		t.Fatalf("ListAudit: rows=%v next=%v err=%v", rows, next, err)
	}
	if window, err := store.AuditChain(ctx, nil, 10); err != nil || window.Rows == nil {
		t.Fatalf("AuditChain.Rows: %v %v", window.Rows, err)
	}
	if v, err := store.ListLocalUsers(ctx); err != nil || v == nil {
		t.Fatalf("ListLocalUsers: %v %v", v, err)
	}
	if v, err := store.ListApiTokens(ctx, "someone"); err != nil || v == nil {
		t.Fatalf("ListApiTokens: %v %v", v, err)
	}
	if v, err := store.ListRoleAssignments(ctx, nil); err != nil || v == nil {
		t.Fatalf("ListRoleAssignments: %v %v", v, err)
	}
}

func TestMemoryStoreSetDesiredRejectsInvalidState(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c1")
	if _, err := store.UpsertDesired(ctx, id, core.ClusterSpec{Name: "c1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	err := store.SetDesired(ctx, id, DesiredState("bogus"))
	if err == nil {
		t.Fatal("expected an error for an invalid DesiredState value")
	}
	if !strings.Contains(err.Error(), "bad desired state") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "bad desired state")
	}

	// The cluster's desired state must be untouched by the rejected call.
	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Desired != DesiredRunning {
		t.Fatalf("Desired = %v, want unaffected DesiredRunning", got.Desired)
	}
}
