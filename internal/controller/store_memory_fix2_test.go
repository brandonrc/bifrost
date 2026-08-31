package controller

import (
	"context"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

// Fix round 2 (same class as fix round 1, review of commit 338cc34):
// RecordJob/ListJobs and GetIntent shared the store/caller aliasing bug
// M1 fixed elsewhere, plus three more ingress sites found while auditing
// the rest of the Store surface for the same class: CreateLocalUser
// (Email), SetLoginLockout (LockedUntil), CreateApiToken (LastUsedAt via
// the record parameter). See clone.go's cloneJobRecord/cloneIntentRecord
// and the CompleteIntent doc comment (reviewed, found already safe).

func TestMemoryStoreRecordJobCopiesCallerJob(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	dur := uint64(30)
	job := core.JobRecord{
		Id:           "j1",
		Cluster:      "c1",
		Submitter:    "alice",
		Status:       "RUNNING",
		DurationSecs: &dur,
		SubmittedAt:  100,
	}
	if err := store.RecordJob(ctx, job); err != nil {
		t.Fatalf("record job: %v", err)
	}

	// Mutate the caller's own job (and its pointee) after the call
	// returns.
	dur = 999
	job.Status = "mutated"

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].Status != "RUNNING" {
		t.Fatalf("Status = %q, want unaffected \"RUNNING\"", jobs[0].Status)
	}
	if jobs[0].DurationSecs == nil || *jobs[0].DurationSecs != 30 {
		t.Fatalf("DurationSecs = %v, want unaffected 30", jobs[0].DurationSecs)
	}
}

func TestMemoryStoreListJobsReturnsIndependentCopy(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	dur := uint64(30)
	if err := store.RecordJob(ctx, core.JobRecord{Id: "j1", DurationSecs: &dur, SubmittedAt: 100}); err != nil {
		t.Fatalf("record job: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list jobs: %v %v", jobs, err)
	}
	// Mutate the returned copy's pointee.
	*jobs[0].DurationSecs = 999

	jobs2, err := store.ListJobs(ctx)
	if err != nil || len(jobs2) != 1 {
		t.Fatalf("list jobs 2: %v %v", jobs2, err)
	}
	if *jobs2[0].DurationSecs != 30 {
		t.Fatalf("DurationSecs = %d, want unaffected 30", *jobs2[0].DurationSecs)
	}
}

func TestMemoryStoreGetIntentReturnsIndependentCopy(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if _, err := store.BeginIntent(ctx, "k1", "fp1"); err != nil {
		t.Fatalf("begin intent: %v", err)
	}
	if err := store.CompleteIntent(ctx, "k1", `{"ok":true}`); err != nil {
		t.Fatalf("complete intent: %v", err)
	}

	got, err := store.GetIntent(ctx, "k1")
	if err != nil || got == nil {
		t.Fatalf("get intent: %v %v", got, err)
	}
	if got.ResponseJSON == nil || got.CompletedAt == nil {
		t.Fatalf("expected ResponseJSON/CompletedAt to be set, got %+v", got)
	}
	originalCompletedAt := *got.CompletedAt

	// Mutate the returned copy's pointees.
	*got.ResponseJSON = "mutated"
	*got.CompletedAt = originalCompletedAt + 1000

	got2, err := store.GetIntent(ctx, "k1")
	if err != nil || got2 == nil {
		t.Fatalf("get intent 2: %v %v", got2, err)
	}
	if got2.ResponseJSON == nil || *got2.ResponseJSON != `{"ok":true}` {
		t.Fatalf("ResponseJSON = %v, want unaffected %q", got2.ResponseJSON, `{"ok":true}`)
	}
	if got2.CompletedAt == nil || *got2.CompletedAt != originalCompletedAt {
		t.Fatalf("CompletedAt = %v, want unaffected %d", got2.CompletedAt, originalCompletedAt)
	}
}

func TestMemoryStoreCreateLocalUserCopiesCallerEmail(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	email := "alice@example.com"
	if err := store.CreateLocalUser(ctx, "alice", &email, "hash", core.LocalRoleViewer); err != nil {
		t.Fatalf("create local user: %v", err)
	}
	email = "mutated@example.com"

	got, err := store.GetLocalUser(ctx, "alice")
	if err != nil || got == nil {
		t.Fatalf("get local user: %v %v", got, err)
	}
	if got.Email == nil || *got.Email != "alice@example.com" {
		t.Fatalf("Email = %v, want unaffected \"alice@example.com\"", got.Email)
	}
}

func TestMemoryStoreSetLoginLockoutCopiesCallerPointer(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.CreateLocalUser(ctx, "alice", nil, "hash", core.LocalRoleViewer); err != nil {
		t.Fatalf("create local user: %v", err)
	}
	locked := uint64(500)
	if err := store.SetLoginLockout(ctx, "alice", 5, &locked); err != nil {
		t.Fatalf("set login lockout: %v", err)
	}
	locked = 999999

	got, err := store.GetLocalUser(ctx, "alice")
	if err != nil || got == nil {
		t.Fatalf("get local user: %v %v", got, err)
	}
	if got.LockedUntil == nil || *got.LockedUntil != 500 {
		t.Fatalf("LockedUntil = %v, want unaffected 500", got.LockedUntil)
	}
}

func TestMemoryStoreCreateApiTokenCopiesCallerRecord(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	lastUsed := uint64(100)
	record := core.ApiTokenRecord{Prefix: "abcd1234", Username: "alice", LastUsedAt: &lastUsed}
	if err := store.CreateApiToken(ctx, record); err != nil {
		t.Fatalf("create api token: %v", err)
	}
	lastUsed = 999

	got, err := store.GetApiTokenByPrefix(ctx, "abcd1234")
	if err != nil || got == nil {
		t.Fatalf("get api token: %v %v", got, err)
	}
	if got.LastUsedAt == nil || *got.LastUsedAt != 100 {
		t.Fatalf("LastUsedAt = %v, want unaffected 100", got.LastUsedAt)
	}
}
