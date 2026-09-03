package api

import (
	"context"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
)

func minimalPoolSpec(name string) PoolSpec {
	return PoolSpec{
		Name:   name,
		Cohort: "default",
		Flavors: []FlavorSpec{
			{Name: "cpu-flavor", Resources: map[string]string{"cpu": "64", "memory": "256Gi"}},
		},
		FairSharingWeight: 1.0,
	}
}

func TestListPools_Empty(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	resp, err := s.ListPools(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), ListPoolsRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views := mustResponse[ListPools200JSONResponse](t, resp)
	if len(views) != 0 {
		t.Fatalf("got %d pools, want 0", len(views))
	}
}

func TestListPools_DeniedForViewerLackingRead(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	// A caller with no roles at all lacks Read on Target::Pool.
	_, err := s.ListPools(ctxWithIdentity(&auth.Identity{Subject: "nobody"}), ListPoolsRequestObject{})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}

func TestCreatePool_SuccessThenTotalNominal(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	spec := minimalPoolSpec("gpu-pool")
	resp, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: spec}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustResponse[CreatePool201Response](t, resp)

	got, err := s.GetPool(ctxWithIdentity(admin()), GetPoolRequestObject{Name: "gpu-pool"})
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	view := mustResponse[GetPool200JSONResponse](t, got)
	if view.TotalNominal["cpu"] != "64" {
		t.Errorf("total_nominal[cpu] = %q, want 64", view.TotalNominal["cpu"])
	}
}

func TestCreatePool_DeniedForNonAdmin(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.CreatePool(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("p")}})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}

func TestCreatePool_InvalidSpecRejected(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	bad := minimalPoolSpec("Bad Name!")
	_, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: bad}})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestCreatePool_UnparseableQuantityRejected(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	spec := minimalPoolSpec("p1")
	spec.Flavors[0].Resources["cpu"] = "not-a-quantity"
	_, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: spec}})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestCreatePool_ConflictOnDuplicateName(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	spec := minimalPoolSpec("dup")
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: spec}}); err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	_, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: spec}})
	if err == nil {
		t.Fatal("expected 409")
	}
	mustHTTPError(t, err, 409)
}

func TestGetPool_NotFound(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.GetPool(ctxWithIdentity(admin()), GetPoolRequestObject{Name: "nope"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestDeletePool_SuccessAndNotFound(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	spec := minimalPoolSpec("temp")
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: spec}}); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	resp, err := s.DeletePool(ctxWithIdentity(admin()), DeletePoolRequestObject{Name: "temp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustResponse[DeletePool202Response](t, resp)

	_, err = s.DeletePool(ctxWithIdentity(admin()), DeletePoolRequestObject{Name: "temp"})
	if err == nil {
		t.Fatal("expected 404 on re-delete")
	}
	mustHTTPError(t, err, 404)
}

func TestPutAllocation_SuccessThenListThenDelete(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("alloc-pool")}}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	body := &PutAllocation{Namespace: "team-a", Nominal: map[string]string{"cpu": "8"}}
	resp, err := s.PutAllocation(ctxWithIdentity(admin()), PutAllocationRequestObject{Name: "alloc-pool", Project: "team-a", Body: body})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustResponse[PutAllocation200Response](t, resp)

	list, err := s.ListAllocations(ctxWithIdentity(admin()), ListAllocationsRequestObject{Name: "alloc-pool"})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	views := mustResponse[ListAllocations200JSONResponse](t, list)
	if len(views) != 1 || views[0].Project != "team-a" {
		t.Fatalf("allocations = %+v", views)
	}

	del, err := s.DeleteAllocation(ctxWithIdentity(admin()), DeleteAllocationRequestObject{Name: "alloc-pool", Project: "team-a"})
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	mustResponse[DeleteAllocation202Response](t, del)

	_, err = s.DeleteAllocation(ctxWithIdentity(admin()), DeleteAllocationRequestObject{Name: "alloc-pool", Project: "team-a"})
	if err == nil {
		t.Fatal("expected 404 on re-delete")
	}
	mustHTTPError(t, err, 404)
}

func TestPutAllocation_PathBodyMismatchRejected(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("mismatch-pool")}}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	otherProject := "not-team-a"
	body := &PutAllocation{Project: &otherProject, Namespace: "ns", Nominal: map[string]string{"cpu": "1"}}
	_, err := s.PutAllocation(ctxWithIdentity(admin()), PutAllocationRequestObject{Name: "mismatch-pool", Project: "team-a", Body: body})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestPutAllocation_NoSuchPool(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	body := &PutAllocation{Namespace: "ns", Nominal: map[string]string{"cpu": "1"}}
	_, err := s.PutAllocation(ctxWithIdentity(admin()), PutAllocationRequestObject{Name: "ghost", Project: "team-a", Body: body})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

// TestPutAllocation_GpuTenantIsolation ports pools.rs's put_allocation GPU
// tenant-isolation branch (#58): a pool resolving to time-slice sharing
// must not admit a second project's allocation.
func TestPutAllocation_GpuTenantIsolation(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	timeSlice := TimeSlice
	spec := minimalPoolSpec("gpu-shared")
	spec.GpuSharing = &timeSlice
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: spec}}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	first := &PutAllocation{Namespace: "ns-a", Nominal: map[string]string{"cpu": "1"}}
	if _, err := s.PutAllocation(ctxWithIdentity(admin()), PutAllocationRequestObject{Name: "gpu-shared", Project: "proj-a", Body: first}); err != nil {
		t.Fatalf("first allocation should succeed: %v", err)
	}
	second := &PutAllocation{Namespace: "ns-b", Nominal: map[string]string{"cpu": "1"}}
	_, err := s.PutAllocation(ctxWithIdentity(admin()), PutAllocationRequestObject{Name: "gpu-shared", Project: "proj-b", Body: second})
	if err == nil {
		t.Fatal("expected GPU tenant-isolation 400")
	}
	mustHTTPError(t, err, 400)
}

func TestPoolUsage_NoObservationYet(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("usage-pool")}}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	resp, err := s.PoolUsage(ctxWithIdentity(admin()), PoolUsageRequestObject{Name: "usage-pool"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	view := mustResponse[PoolUsage200JSONResponse](t, resp)
	if view.SampledAt != nil {
		t.Errorf("sampled_at = %v, want nil (unobserved)", view.SampledAt)
	}
	if u, ok := view.Utilization["cpu"]; !ok || u.Nominal != 64 || u.Allocated != 0 {
		t.Errorf("utilization[cpu] = %+v, want nominal=64 allocated=0", u)
	}
}

func TestPoolUsage_NotFound(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.PoolUsage(ctxWithIdentity(admin()), PoolUsageRequestObject{Name: "ghost"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestPoolUsage_WithObservation(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("observed-pool")}}); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	obsJSON := `{"admitted_workloads":1,"reserving_workloads":1,"pending_workloads":0,` +
		`"flavors_usage":{"cpu-flavor":{"cpu":"32"}},"queues_usage":{"team-a":{"cpu":"32"}}}`
	if err := s.Store.RecordPoolObservation(context.Background(), "observed-pool", obsJSON); err != nil {
		t.Fatalf("record observation failed: %v", err)
	}
	resp, err := s.PoolUsage(ctxWithIdentity(admin()), PoolUsageRequestObject{Name: "observed-pool"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	view := mustResponse[PoolUsage200JSONResponse](t, resp)
	if view.SampledAt == nil {
		t.Fatal("sampled_at should be set once observed")
	}
	if u := view.Utilization["cpu"]; u.Allocated != 32 || u.Nominal != 64 || u.Pct != 50 {
		t.Errorf("utilization[cpu] = %+v, want allocated=32 nominal=64 pct=50", u)
	}
	if view.Projects["team-a"]["cpu"] != 32 {
		t.Errorf("projects = %+v", view.Projects)
	}
}

func TestListAllocations_DeniedWithoutRead(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.ListAllocations(ctxWithIdentity(&auth.Identity{Subject: "nobody"}), ListAllocationsRequestObject{Name: "any"})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}

func TestGpuSharingFromWire_RejectsUnknownValue(t *testing.T) {
	bogus := GpuSharing("bogus")
	if _, err := gpuSharingFromWire(&bogus); err == nil {
		t.Fatal("expected an error for an unknown gpu_sharing value")
	}
}

func TestFormatQuantity_IntegralVsFractional(t *testing.T) {
	if got := formatQuantity(128); got != "128" {
		t.Errorf("formatQuantity(128) = %q, want 128", got)
	}
	if got := formatQuantity(0.5); got != "0.5" {
		t.Errorf("formatQuantity(0.5) = %q, want 0.5", got)
	}
}

// #4: purpose round-trips through the wire: absent reads as compute, a
// serving pool lists as serving, and an unknown spelling is 400.
func TestPoolPurpose_WireRoundTrip(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("compute-pool")}}); err != nil {
		t.Fatalf("create compute pool: %v", err)
	}
	serving := minimalPoolSpec("serve-pool")
	purpose := Serving
	serving.Purpose = &purpose
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: serving}}); err != nil {
		t.Fatalf("create serving pool: %v", err)
	}
	bad := minimalPoolSpec("bad-pool")
	badPurpose := PoolPurpose("inference")
	bad.Purpose = &badPurpose
	_, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: bad}})
	if err == nil {
		t.Fatal("unknown purpose must be refused")
	}
	mustHTTPError(t, err, 400)

	list, err := s.ListPools(ctxWithIdentity(admin()), ListPoolsRequestObject{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]string{}
	for _, v := range mustResponse[ListPools200JSONResponse](t, list) {
		if v.Purpose == nil {
			t.Fatalf("pool %s: purpose must always be written", v.Name)
		}
		got[v.Name] = string(*v.Purpose)
	}
	if got["compute-pool"] != "compute" || got["serve-pool"] != "serving" || len(got) != 2 {
		t.Fatalf("purposes = %v", got)
	}
}
