package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

// Chain and UsageSource tests below are ported from
// mobula-controller/src/store.rs's #[cfg(test)] mod tests (store.rs:769-943).
// queue_assignment_resolves_first_matching_allocation is NOT ported here:
// it exercises mobula_provision::QueueAssignment, a type that belongs to
// internal/provision (Task 5) — a Go equivalent of that helper is Task
// 9/11's concern, once Provisioner exists. The full InMemoryStore CRUD
// scenario suite lives in Task 2's storetest.RunConformance, not here;
// the MemoryStore-specific tests below are a compile/sanity smoke check
// on top of that.

func strPtr(s string) *string { return &s }

func TestUsageSourceRoundTripsAndRejectsUnknown(t *testing.T) {
	for _, s := range []UsageSource{UsageSourceKueueLedger, UsageSourceObservedSpec} {
		got, err := ParseUsageSource(s.AsStr())
		if err != nil || got != s {
			t.Fatalf("ParseUsageSource(%s.AsStr()) = %v, %v", s, got, err)
		}
	}
	_, err := ParseUsageSource("bogus")
	if err == nil {
		t.Fatal("expected ParseUsageSource(\"bogus\") to fail")
	}
	if !strings.Contains(err.Error(), "bad usage source") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "bad usage source")
	}
}

func chainEvent(ts uint64, subject *string) *core.AuditEvent {
	action := "create_cluster"
	return &core.AuditEvent{
		Ts:       ts,
		Subject:  subject,
		Decision: core.AuditDecisionAllow,
		Action:   &action,
	}
}

// chainRows chains n events from genesis, returning rows as (seq, event,
// hash) — the Go equivalent of store.rs's chain_rows test helper.
func chainRows(events []*core.AuditEvent) []ChainedAuditRow {
	prev := AuditGenesisHash
	rows := make([]ChainedAuditRow, 0, len(events))
	for i, e := range events {
		hash := AuditChainHash(prev, e)
		prev = hash
		rows = append(rows, ChainedAuditRow{Seq: uint64(i) + 1, Event: *e, ChainHash: hash})
	}
	return rows
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func TestChainHashIsDeterministicAndPrevSensitive(t *testing.T) {
	e := chainEvent(100, strPtr("alice"))
	h1 := AuditChainHash(AuditGenesisHash, e)
	if h1 != AuditChainHash(AuditGenesisHash, e) {
		t.Fatal("hash is not deterministic")
	}
	if len(h1) != 64 {
		t.Fatalf("hash length = %d, want 64 (lowercase hex sha256)", len(h1))
	}
	for i := 0; i < len(h1); i++ {
		if !isHexDigit(h1[i]) {
			t.Fatalf("hash contains non-hex-digit byte %q", h1[i])
		}
	}
	// A different predecessor yields a different hash for the same row.
	if h1 == AuditChainHash(h1, e) {
		t.Fatal("expected a different predecessor to change the hash")
	}
	// A different row under the same predecessor differs too.
	other := chainEvent(101, strPtr("alice"))
	if h1 == AuditChainHash(AuditGenesisHash, other) {
		t.Fatal("expected a different row to change the hash")
	}
}

func TestVerifyAcceptsAnIntactChainFromGenesis(t *testing.T) {
	rows := chainRows([]*core.AuditEvent{
		chainEvent(100, strPtr("alice")),
		chainEvent(200, nil),
		chainEvent(300, strPtr("bob")),
	})
	v := VerifyAuditChain(AuditGenesisHash, rows)
	if !v.OK() {
		t.Fatalf("expected chain to verify, first broken seq = %v", v.FirstBrokenSeq)
	}
	if v.EventsChecked != 3 {
		t.Fatalf("events checked = %d, want 3", v.EventsChecked)
	}
	// An empty window trivially verifies (nothing to check).
	v = VerifyAuditChain(AuditGenesisHash, nil)
	if !v.OK() || v.EventsChecked != 0 {
		t.Fatalf("empty window: OK=%v EventsChecked=%d", v.OK(), v.EventsChecked)
	}
}

func TestVerifyFlagsATamperedRowAtItsSeq(t *testing.T) {
	rows := chainRows([]*core.AuditEvent{
		chainEvent(100, strPtr("alice")),
		chainEvent(200, nil),
		chainEvent(300, strPtr("bob")),
	})
	// Tamper with the middle row's payload without fixing the chain.
	rows[1].Event.Subject = strPtr("mallory")
	v := VerifyAuditChain(AuditGenesisHash, rows)
	if v.OK() {
		t.Fatal("expected chain to break")
	}
	if v.EventsChecked != 1 {
		t.Fatalf("events checked = %d, want 1 (row 1 verified, row 2 broke)", v.EventsChecked)
	}
	if v.FirstBrokenSeq == nil || *v.FirstBrokenSeq != 2 {
		t.Fatalf("first broken seq = %v, want 2", v.FirstBrokenSeq)
	}

	// A forged hash on the last row is caught at that row.
	rows2 := chainRows([]*core.AuditEvent{chainEvent(100, nil), chainEvent(200, nil)})
	rows2[1].ChainHash = strings.Repeat("f", 64)
	v = VerifyAuditChain(AuditGenesisHash, rows2)
	if v.FirstBrokenSeq == nil || *v.FirstBrokenSeq != 2 {
		t.Fatalf("first broken seq = %v, want 2", v.FirstBrokenSeq)
	}
	if v.EventsChecked != 1 {
		t.Fatalf("events checked = %d, want 1", v.EventsChecked)
	}
}

func TestVerifyFlagsADeletedMiddleRow(t *testing.T) {
	rows := chainRows([]*core.AuditEvent{
		chainEvent(100, nil),
		chainEvent(200, nil),
		chainEvent(300, nil),
	})
	// Drop the middle row: row 3's stored hash chains from row 2's, so
	// replay from row 1 mismatches at seq 3.
	truncated := []ChainedAuditRow{rows[0], rows[2]}
	v := VerifyAuditChain(AuditGenesisHash, truncated)
	if v.FirstBrokenSeq == nil || *v.FirstBrokenSeq != 3 {
		t.Fatalf("first broken seq = %v, want 3", v.FirstBrokenSeq)
	}
}

func TestVerifyAMidTrailWindowAgainstItsHead(t *testing.T) {
	rows := chainRows([]*core.AuditEvent{
		chainEvent(100, nil),
		chainEvent(200, nil),
		chainEvent(300, nil),
	})
	// The window [2, 3] verifies against row 1's hash as head.
	v := VerifyAuditChain(rows[0].ChainHash, rows[1:])
	if !v.OK() || v.EventsChecked != 2 {
		t.Fatalf("OK=%v EventsChecked=%d, want OK EventsChecked=2", v.OK(), v.EventsChecked)
	}
	// The same window from genesis (the wrong head) breaks at seq 2.
	v = VerifyAuditChain(AuditGenesisHash, rows[1:])
	if v.FirstBrokenSeq == nil || *v.FirstBrokenSeq != 2 {
		t.Fatalf("first broken seq = %v, want 2", v.FirstBrokenSeq)
	}
}

// --- MemoryStore sanity smoke tests ---
//
// The exhaustive scenario suite is Task 2's storetest.RunConformance
// (plan Task 2, ported from mobula-controller/tests/store.rs). These
// exist to validate the port compiles and behaves on the paths most
// likely to have a translation bug: generation bumping, the
// terminated_at tombstone anchor, intent fencing, and the audit chain
// end-to-end through the Store interface (as opposed to the pure
// functions exercised directly above).

func TestMemoryStoreUpsertDesiredGenerationTracksSpecChanges(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c1")
	spec := core.ClusterSpec{Name: "c1", Project: "p", RayVersion: "2.9.0", Image: "img", HeadCpu: "1", HeadMemory: "1Gi"}

	gen, err := store.UpsertDesired(ctx, id, spec)
	if err != nil || gen != 1 {
		t.Fatalf("first upsert: gen=%d err=%v, want 1, nil", gen, err)
	}

	// Re-upserting the identical spec must not bump generation.
	gen, err = store.UpsertDesired(ctx, id, spec)
	if err != nil || gen != 1 {
		t.Fatalf("unchanged upsert: gen=%d err=%v, want 1, nil", gen, err)
	}

	changed := spec
	changed.Image = "img2"
	gen, err = store.UpsertDesired(ctx, id, changed)
	if err != nil || gen != 2 {
		t.Fatalf("changed upsert: gen=%d err=%v, want 2, nil", gen, err)
	}

	stored, err := store.Get(ctx, id)
	if err != nil || stored == nil {
		t.Fatalf("get: stored=%v err=%v", stored, err)
	}
	if stored.Desired != DesiredRunning {
		t.Fatalf("desired = %v, want DesiredRunning (default on first insert)", stored.Desired)
	}
}

func TestMemoryStoreSetDesiredAnchorsAndClearsTerminatedAt(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	id := core.ClusterId("c1")
	if _, err := store.UpsertDesired(ctx, id, core.ClusterSpec{Name: "c1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := store.SetDesired(ctx, id, DesiredTerminated); err != nil {
		t.Fatalf("set terminated: %v", err)
	}
	stored, _ := store.Get(ctx, id)
	if stored.TerminatedAt == nil {
		t.Fatal("expected TerminatedAt to be stamped")
	}
	firstStamp := *stored.TerminatedAt

	// Re-setting Terminated again must not move the stamp
	// (get_or_insert_with semantics in the Rust reference).
	if err := store.SetDesired(ctx, id, DesiredTerminated); err != nil {
		t.Fatalf("set terminated again: %v", err)
	}
	stored, _ = store.Get(ctx, id)
	if *stored.TerminatedAt != firstStamp {
		t.Fatalf("TerminatedAt moved: got %d, want %d", *stored.TerminatedAt, firstStamp)
	}

	if err := store.SetDesired(ctx, id, DesiredRunning); err != nil {
		t.Fatalf("resume: %v", err)
	}
	stored, _ = store.Get(ctx, id)
	if stored.TerminatedAt != nil {
		t.Fatal("expected TerminatedAt to clear on resume")
	}

	if err := store.SetDesired(ctx, core.ClusterId("missing"), DesiredRunning); err == nil {
		t.Fatal("expected error for missing cluster")
	}
}

func TestMemoryStoreBeginIntentFencing(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	out, err := store.BeginIntent(ctx, "c1/1", "fp1")
	if err != nil || out.Kind != IntentOutcomeProceed || out.Replay {
		t.Fatalf("fresh intent: out=%+v err=%v", out, err)
	}

	out, err = store.BeginIntent(ctx, "c1/1", "fp1")
	if err != nil || out.Kind != IntentOutcomeProceed || !out.Replay {
		t.Fatalf("replay intent: out=%+v err=%v", out, err)
	}

	out, err = store.BeginIntent(ctx, "c1/1", "fp2")
	if err != nil || out.Kind != IntentOutcomeParamMismatch {
		t.Fatalf("mismatched intent: out=%+v err=%v", out, err)
	}

	if err := store.CompleteIntent(ctx, "c1/1", `{"ok":true}`); err != nil {
		t.Fatalf("complete: %v", err)
	}
	rec, err := store.GetIntent(ctx, "c1/1")
	if err != nil || rec == nil || rec.Status != IntentStatusApplied || rec.ResponseJSON == nil {
		t.Fatalf("get after complete: rec=%+v err=%v", rec, err)
	}
}

func TestMemoryStoreRecordAuditChainsAndVerifies(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	for i := 0; i < 3; i++ {
		ts := uint64(100 * (i + 1))
		if _, err := store.RecordAudit(ctx, &core.AuditEvent{Ts: ts, Decision: core.AuditDecisionAllow}); err != nil {
			t.Fatalf("record audit %d: %v", i, err)
		}
	}

	window, err := store.AuditChain(ctx, nil, 100)
	if err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	if window.Head != AuditGenesisHash {
		t.Fatalf("head = %q, want genesis", window.Head)
	}
	if len(window.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(window.Rows))
	}
	v := VerifyAuditChain(window.Head, window.Rows)
	if !v.OK() {
		t.Fatalf("expected chain to verify, first broken seq = %v", v.FirstBrokenSeq)
	}

	rows, next, err := store.ListAudit(ctx, core.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 3 || next != nil {
		t.Fatalf("rows=%d next=%v, want 3 rows, nil cursor", len(rows), next)
	}
	if rows[0].Seq != 3 || rows[2].Seq != 1 {
		t.Fatalf("expected newest-first ordering, got seqs %d,%d,%d", rows[0].Seq, rows[1].Seq, rows[2].Seq)
	}

	limit := uint32(2)
	rows, next, err = store.ListAudit(ctx, core.AuditFilter{Limit: &limit})
	if err != nil {
		t.Fatalf("list audit limited: %v", err)
	}
	if len(rows) != 2 || next == nil || *next != 2 {
		t.Fatalf("rows=%d next=%v, want 2 rows, cursor=2", len(rows), next)
	}
}
