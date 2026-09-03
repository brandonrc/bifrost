package controller

import (
	"context"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

func TestMeterRecordsRunningClustersAndClosesTheStep(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	r := NewReconciler(store, nil)
	spec := core.ClusterSpec{Name: "c1", Project: "team-a", RayVersion: "x", Image: "x", HeadCpu: "2", HeadMemory: "4Gi",
		WorkerGroups: []core.WorkerGroup{{Name: "w", Cpu: "1", Memory: "1Gi", MinReplicas: 2, MaxReplicas: 4, Replicas: 2}}}
	gen, err := store.UpsertDesired(ctx, "c1", spec)
	if err != nil {
		t.Fatal(err)
	}
	// Not yet running: nothing to meter.
	if n, err := r.Meter(ctx, 1000); err != nil || n != 0 {
		t.Fatalf("meter before running: n=%d err=%v", n, err)
	}
	running := core.ClusterStateRunning
	if err := store.RecordObservation(ctx, "c1", &running, gen); err != nil {
		t.Fatal(err)
	}
	n, err := r.Meter(ctx, 1000)
	if err != nil || n == 0 {
		t.Fatalf("meter running: n=%d err=%v", n, err)
	}
	samples, err := store.UsageSamples(ctx, nil, nil, nil, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	byRes := map[string]float64{}
	for _, s := range samples {
		if s.Project != "team-a" || s.Pool != "" || s.Source != UsageSourceObservedSpec || s.Ts != 1000 {
			t.Fatalf("unexpected sample %+v", s)
		}
		byRes[s.Resource] = s.Quantity
	}
	// Minimum demand: head 2 CPU + 2 workers x 1 CPU = 4.
	if byRes["cpu"] != 4 {
		t.Fatalf("cpu sample = %v, want 4 (head 2 + min 2 workers x 1): %v", byRes["cpu"], byRes)
	}
	// Leaving running records one closing zero per resource, then nothing.
	if err := store.SetDesired(ctx, "c1", DesiredTerminated); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Meter(ctx, 1600); err != nil || n == 0 {
		t.Fatalf("closing meter: n=%d err=%v", n, err)
	}
	if n, err := r.Meter(ctx, 2200); err != nil || n != 0 {
		t.Fatalf("meter after close must record nothing: n=%d err=%v", n, err)
	}
	samples, _ = store.UsageSamples(ctx, nil, nil, nil, 0, 3000)
	zeros := 0
	for _, s := range samples {
		if s.Ts == 1600 && s.Quantity == 0 {
			zeros++
		}
	}
	if zeros == 0 {
		t.Fatal("no closing zero samples recorded")
	}
}

func TestMeterAttributesSamplesToTheClusterOwner(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	r := NewReconciler(store, nil)
	owner := "dev-a"
	spec := core.ClusterSpec{Name: "c1", Project: "team-a", Owner: &owner, RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}
	gen, err := store.UpsertDesired(ctx, "c1", spec)
	if err != nil {
		t.Fatal(err)
	}
	running := core.ClusterStateRunning
	if err := store.RecordObservation(ctx, "c1", &running, gen); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Meter(ctx, 1000); err != nil || n == 0 {
		t.Fatalf("meter: n=%d err=%v", n, err)
	}
	samples, err := store.UsageSamples(ctx, nil, nil, &owner, 0, 2000)
	if err != nil || len(samples) == 0 {
		t.Fatalf("owner-filtered samples: %v (%d)", err, len(samples))
	}
	for _, s := range samples {
		if s.Owner != "dev-a" {
			t.Fatalf("sample owner = %q, want dev-a: %+v", s.Owner, s)
		}
	}
	// Ownerless clusters stay unattributed rather than failing to meter.
	if _, err := store.UpsertDesired(ctx, "c2", core.ClusterSpec{Name: "c2", Project: "team-b", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordObservation(ctx, "c2", &running, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Meter(ctx, 1100); err != nil {
		t.Fatal(err)
	}
	empty := ""
	unattributed, _ := store.UsageSamples(ctx, nil, nil, &empty, 0, 2000)
	if len(unattributed) == 0 {
		t.Fatal("ownerless cluster recorded no unattributed samples")
	}
	for _, s := range unattributed {
		if s.Project != "team-b" {
			t.Fatalf("unattributed sample from the wrong cluster: %+v", s)
		}
	}
}

func TestMeterRecordsRunningJobsAndClosesTheStep(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	r := NewReconciler(store, nil)
	owner := "dev-b"
	spec := core.RayJobSpec{Project: "team-a", Entrypoint: "python x.py", Image: "x", HeadCpu: "2", HeadMemory: "4Gi",
		WorkerGroups: []core.WorkerGroup{{Name: "w", Cpu: "1", Memory: "1Gi", MinReplicas: 1, MaxReplicas: 3, Replicas: 1}}}
	if err := store.UpsertRayJob(ctx, "j1", spec, &owner); err != nil {
		t.Fatal(err)
	}
	// A cluster with the same id must not share the job's bookkeeping.
	if _, err := store.UpsertDesired(ctx, "j1", core.ClusterSpec{Name: "j1", Project: "team-a", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	// Initializing: nothing held yet.
	if err := store.RecordRayJobObservation(ctx, "j1", RayJobObservation{DeploymentStatus: "Initializing"}); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Meter(ctx, 1000); err != nil || n != 0 {
		t.Fatalf("meter before running: n=%d err=%v", n, err)
	}
	if err := store.RecordRayJobObservation(ctx, "j1", RayJobObservation{DeploymentStatus: "Running", Status: "RUNNING"}); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Meter(ctx, 1000); err != nil || n == 0 {
		t.Fatalf("meter running job: n=%d err=%v", n, err)
	}
	samples, err := store.UsageSamples(ctx, nil, nil, nil, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	byRes := map[string]float64{}
	for _, s := range samples {
		if s.Project != "team-a" || s.Owner != "dev-b" || s.Source != UsageSourceObservedSpec {
			t.Fatalf("unexpected sample %+v", s)
		}
		byRes[s.Resource] = s.Quantity
	}
	// Head 2 CPU + 1 min worker x 1 CPU = 3.
	if byRes["cpu"] != 3 {
		t.Fatalf("cpu sample = %v, want 3: %v", byRes["cpu"], byRes)
	}
	// Finishing closes the step with one zero per resource, then silence.
	if err := store.RecordRayJobObservation(ctx, "j1", RayJobObservation{DeploymentStatus: "Complete", Status: "SUCCEEDED"}); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Meter(ctx, 1600); err != nil || n == 0 {
		t.Fatalf("closing meter: n=%d err=%v", n, err)
	}
	if n, err := r.Meter(ctx, 2200); err != nil || n != 0 {
		t.Fatalf("meter after close must record nothing: n=%d err=%v", n, err)
	}
	samples, _ = store.UsageSamples(ctx, nil, nil, &owner, 0, 3000)
	zeros := 0
	for _, s := range samples {
		if s.Ts == 1600 && s.Quantity == 0 {
			zeros++
		}
	}
	if zeros == 0 {
		t.Fatal("no closing zero samples recorded for the job")
	}
}
