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
