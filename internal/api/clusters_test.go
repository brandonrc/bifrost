package api

import (
	"context"
	"errors"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/policy"
	"github.com/brandonrc/bifrost/internal/provision"
)

// fakeProvisioner is a minimal provision.Provisioner for T12 tests that
// need one wired but never exercise its cluster-lifecycle methods (only
// cluster_obs.go's ClusterNodes/ClusterEvents/ClusterLogs/DashboardApiBase
// matter here) — BaseProvisioner supplies those as no-ops; this type fills
// in the rest of the interface with trivial bodies.
type fakeProvisioner struct {
	provision.BaseProvisioner
}

func (fakeProvisioner) Apply(context.Context, core.ClusterId, *core.ClusterSpec, uint64, string, *provision.QueueAssignment) (provision.ApplyResponse, error) {
	return provision.ApplyResponse{}, nil
}
func (fakeProvisioner) Terminate(context.Context, core.ClusterId) error { return nil }
func (fakeProvisioner) Suspend(context.Context, core.ClusterId) error   { return nil }
func (fakeProvisioner) Resume(context.Context, core.ClusterId) error    { return nil }
func (fakeProvisioner) Observe(context.Context, core.ClusterId) (provision.ObservedCluster, error) {
	return provision.ObservedCluster{}, nil
}
func (fakeProvisioner) List(context.Context) ([]provision.ObservedCluster, error) { return nil, nil }

// fakeServiceProvisioner is a minimal provision.ServiceProvisioner for
// services_test.go and the burn-down smoke test.
type fakeServiceProvisioner struct{}

func (fakeServiceProvisioner) Deploy(context.Context, string, *core.ServiceSpec, uint64, *provision.QueueAssignment) error {
	return nil
}
func (fakeServiceProvisioner) Get(context.Context, string) (*provision.ObservedService, error) {
	return nil, nil
}
func (fakeServiceProvisioner) Delete(context.Context, string) error { return nil }
func (fakeServiceProvisioner) List(context.Context) ([]provision.ObservedService, error) {
	return nil, nil
}

// --- shared test helpers (used by clusters_test.go, registry_test.go,
// settings_test.go, access_test.go) ---

func testIdentity(subject string, roles ...auth.Role) *auth.Identity {
	return &auth.Identity{Subject: subject, Roles: roles}
}

func ctxWithIdentity(id *auth.Identity) context.Context {
	return context.WithValue(context.Background(), identityContextKey{}, id)
}

func newMemStore(t *testing.T) controller.Store {
	t.Helper()
	return controller.NewMemoryStore()
}

func mustHTTPError(t *testing.T, err error, wantStatus int) {
	t.Helper()
	var httpErr HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %#v, want an HTTPError", err)
	}
	if httpErr.Status != wantStatus {
		t.Fatalf("status = %d, want %d (err: %v)", httpErr.Status, wantStatus, httpErr)
	}
}

// mustResponse asserts resp holds a T (the strict-server response type a
// handler's success path returns) and returns it, failing the test
// otherwise. Generic so every _test.go file in this package can use one
// shared assertion instead of a bare (possibly errcheck-flagged)
// single-value type assertion.
func mustResponse[T any](t *testing.T, resp any) T {
	t.Helper()
	v, ok := resp.(T)
	if !ok {
		var zero T
		t.Fatalf("response = %#v, want %T", resp, zero)
	}
	return v
}

func minimalClusterSpec(name, project string) ClusterSpec {
	return ClusterSpec{
		Name: name, Project: project, RayVersion: "2.9.0", Image: "rayproject/ray:2.9.0",
		HeadCpu: "1", HeadMemory: "2Gi",
	}
}

// --- ListClusters / GetCluster ---

func TestListClusters_UnscopedReadPermitted(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	if _, err := store.UpsertDesired(ctx, "c1", core.ClusterSpec{Name: "c1", Project: "p1", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}

	resp, err := s.ListClusters(ctxWithIdentity(testIdentity("alice", auth.RoleViewer)), ListClustersRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views := mustResponse[ListClusters200JSONResponse](t, resp)
	if len(views) != 1 {
		t.Fatalf("got %d clusters, want 1", len(views))
	}
	if views[0].Id != "c1" || views[0].Project != "p1" || views[0].Desired != "running" {
		t.Errorf("unexpected view: %+v", views[0])
	}
}

func TestListClusters_NoRoleAndNoAssignmentsIsDenied(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{Store: store}
	_, err := s.ListClusters(ctxWithIdentity(testIdentity("bob")), ListClustersRequestObject{})
	mustHTTPError(t, err, 403)
}

func TestListClusters_ScopedAssignmentNarrowsVisibility(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	if _, err := store.UpsertDesired(ctx, "c-a", core.ClusterSpec{Name: "c-a", Project: "proj-a", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertDesired(ctx, "c-b", core.ClusterSpec{Name: "c-b", Project: "proj-b", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}
	// No global roles at all, but a scoped assignment for proj-a: visibility
	// narrows to exactly that project (ADR-0009 addendum), and the caller
	// does NOT get a blanket 403 despite holding no global Read.
	id := testIdentity("carol")
	id.ProjectRoles = []auth.RoleScope{{Role: auth.RoleViewer, Scope: "project:proj-a"}}

	resp, err := s.ListClusters(ctxWithIdentity(id), ListClustersRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views := mustResponse[ListClusters200JSONResponse](t, resp)
	if len(views) != 1 || views[0].Id != "c-a" {
		t.Fatalf("views = %+v, want exactly c-a", views)
	}
}

func TestGetCluster_NotFound(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{Store: store}
	_, err := s.GetCluster(ctxWithIdentity(testIdentity("alice", auth.RoleAdmin)), GetClusterRequestObject{Id: "nope"})
	mustHTTPError(t, err, 404)
}

func TestGetCluster_OutOfScopeIs404NotLeaked(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	if _, err := store.UpsertDesired(ctx, "secret", core.ClusterSpec{Name: "secret", Project: "other-proj", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}
	id := testIdentity("carol")
	id.ProjectRoles = []auth.RoleScope{{Role: auth.RoleViewer, Scope: "project:proj-a"}}
	_, err := s.GetCluster(ctxWithIdentity(id), GetClusterRequestObject{Id: "secret"})
	mustHTTPError(t, err, 404)
}

// --- CreateCluster ---

func TestCreateCluster_OperatorSucceedsDeveloperDenied(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{Store: store}
	body := CreateCluster{Id: "c1", Spec: minimalClusterSpec("c1", "proj-a")}

	if _, err := s.CreateCluster(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), CreateClusterRequestObject{Body: &body}); err == nil {
		t.Fatal("expected developer to be denied cluster create")
	} else {
		mustHTTPError(t, err, 403)
	}

	resp, err := s.CreateCluster(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), CreateClusterRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("operator create failed: %v", err)
	}
	if _, ok := resp.(CreateCluster201Response); !ok {
		t.Fatalf("response = %#v, want CreateCluster201Response", resp)
	}
	stored, err := store.Get(context.Background(), "c1")
	if err != nil || stored == nil {
		t.Fatalf("cluster not persisted: %v", err)
	}
	if stored.Spec.Owner == nil || *stored.Spec.Owner != "op" {
		t.Errorf("owner = %v, want stamped from the authenticated identity (op), never trusted from the body", stored.Spec.Owner)
	}
}

func TestCreateCluster_QuotaExceededIs409(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{
		Store: store,
		PolicySeed: PolicyConfig{
			Quotas: map[string]policy.ResourceMap{"proj-a": {"cpu": 1}},
		},
	}
	spec := minimalClusterSpec("c1", "proj-a")
	spec.HeadCpu = "2" // demand (2 cores) exceeds the 1-core quota outright.
	body := CreateCluster{Id: "c1", Spec: spec}

	_, err := s.CreateCluster(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), CreateClusterRequestObject{Body: &body})
	mustHTTPError(t, err, 409)

	if c, _ := store.Get(context.Background(), "c1"); c != nil {
		t.Error("a quota-rejected cluster must not be persisted")
	}
}

func TestCreateCluster_BudgetExceededIs409(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	// Seed windowed usage at a level that already exceeds the budget cap
	// for the whole window (carry-in from ts=0, well before any "from").
	if err := store.RecordUsageSamples(ctx, []controller.UsageSample{
		{Ts: 0, Project: "proj-a", Pool: "pool-a", Resource: "cpu", Quantity: 100, Source: controller.UsageSourceObservedSpec},
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Store: store,
		PolicySeed: PolicyConfig{
			Budgets: map[string]policy.Budget{"proj-a": {WindowSecs: 3600, Limits: map[string]float64{"cpu": 50}}},
		},
	}
	body := CreateCluster{Id: "c1", Spec: minimalClusterSpec("c1", "proj-a")}
	_, err := s.CreateCluster(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), CreateClusterRequestObject{Body: &body})
	mustHTTPError(t, err, 409)
}

func TestCreateCluster_FractionalGPURejectedInMultiTenantPool(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	if _, err := store.UpsertPool(ctx, "gpu-pool", core.PoolSpec{Name: "gpu-pool", Cohort: "c", Flavors: []core.FlavorSpec{{Name: "f", Resources: map[string]string{"nvidia.com/gpu": "8"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "gpu-pool", Project: "proj-a", Namespace: "ns-a"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "gpu-pool", Project: "proj-b", Namespace: "ns-b"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}
	spec := minimalClusterSpec("c1", "proj-a")
	gpu := "0.5"
	spec.WorkerGroups = []WorkerGroup{{Name: "w", Cpu: "1", Memory: "1Gi", Gpu: &gpu, MinReplicas: 0, MaxReplicas: 1}}
	body := CreateCluster{Id: "c1", Spec: spec}

	_, err := s.CreateCluster(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), CreateClusterRequestObject{Body: &body})
	mustHTTPError(t, err, 400)
}

// --- DeleteCluster / purge ---

func TestDeleteCluster_DefaultTerminatesThenPurgeRequiresTombstone(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	if _, err := store.UpsertDesired(ctx, "c1", core.ClusterSpec{Name: "c1", Project: "p1", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}
	id := testIdentity("op", auth.RoleOperator)

	// Live cluster: purge refused (not yet a tombstone).
	purgeTrue := true
	_, err := s.DeleteCluster(ctxWithIdentity(id), DeleteClusterRequestObject{Id: "c1", Params: DeleteClusterParams{Purge: &purgeTrue}})
	mustHTTPError(t, err, 409)

	// Default delete: terminate.
	resp, err := s.DeleteCluster(ctxWithIdentity(id), DeleteClusterRequestObject{Id: "c1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := resp.(DeleteCluster202Response); !ok {
		t.Fatalf("response = %#v, want 202", resp)
	}
	stored, _ := store.Get(ctx, "c1")
	if stored.Desired != controller.DesiredTerminated {
		t.Fatalf("desired = %v, want terminated", stored.Desired)
	}

	// Still not observed gone yet (never observed) -> IS a tombstone per
	// observedGone's nil-means-gone rule, so purge now succeeds.
	resp, err = s.DeleteCluster(ctxWithIdentity(id), DeleteClusterRequestObject{Id: "c1", Params: DeleteClusterParams{Purge: &purgeTrue}})
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if _, ok := resp.(DeleteCluster200Response); !ok {
		t.Fatalf("response = %#v, want 200", resp)
	}
	if stored, _ := store.Get(ctx, "c1"); stored != nil {
		t.Error("cluster row should be hard-deleted after purge")
	}
}

func TestDeleteCluster_NotFound(t *testing.T) {
	s := &Server{Store: controller.NewMemoryStore()}
	_, err := s.DeleteCluster(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), DeleteClusterRequestObject{Id: "nope"})
	mustHTTPError(t, err, 404)
}

// --- Suspend / Resume ---

func setObserved(t *testing.T, store controller.Store, id core.ClusterId, state core.ClusterState, gen uint64) {
	t.Helper()
	if err := store.RecordObservation(context.Background(), id, &state, gen); err != nil {
		t.Fatal(err)
	}
}

func TestSuspendCluster_LegalTransitionThenIllegalIs409(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	gen, err := store.UpsertDesired(ctx, "c1", core.ClusterSpec{Name: "c1", Project: "p1", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"})
	if err != nil {
		t.Fatal(err)
	}
	setObserved(t, store, "c1", core.ClusterStateRunning, gen)
	s := &Server{Store: store}
	id := testIdentity("op", auth.RoleOperator)

	resp, err := s.SuspendCluster(ctxWithIdentity(id), SuspendClusterRequestObject{Id: "c1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := resp.(SuspendCluster202Response); !ok {
		t.Fatalf("response = %#v, want 202", resp)
	}

	// A never-observed cluster (ObservedState nil) has no legal edge to
	// Suspending — CanTransitionTo requires an observed state to reason
	// about; a Pending/unobserved cluster can't be suspended.
	if _, err := store.UpsertDesired(ctx, "c2", core.ClusterSpec{Name: "c2", Project: "p1", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.SuspendCluster(ctxWithIdentity(id), SuspendClusterRequestObject{Id: "c2"})
	mustHTTPError(t, err, 409)
}

func TestSuspendResume_RequiresWritePermission(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	gen, err := store.UpsertDesired(ctx, "c1", core.ClusterSpec{Name: "c1", Project: "p1", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"})
	if err != nil {
		t.Fatal(err)
	}
	setObserved(t, store, "c1", core.ClusterStateRunning, gen)
	s := &Server{Store: store}

	_, err = s.SuspendCluster(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), SuspendClusterRequestObject{Id: "c1"})
	mustHTTPError(t, err, 403)

	_, err = s.ResumeCluster(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), ResumeClusterRequestObject{Id: "c1"})
	mustHTTPError(t, err, 403)
}

// A project-scoped operator may suspend and resume the clusters of their
// own project and no other's — the rule create and delete already follow.
// Found on grace 2026-09-02: the port demanded global write here.
func TestSuspendResume_ProjectScopedOperator(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	for _, c := range []struct{ id, project string }{{"c1", "p1"}, {"c2", "p2"}} {
		gen, err := store.UpsertDesired(ctx, core.ClusterId(c.id), core.ClusterSpec{Name: c.id, Project: c.project, RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"})
		if err != nil {
			t.Fatal(err)
		}
		setObserved(t, store, core.ClusterId(c.id), core.ClusterStateRunning, gen)
	}
	s := &Server{Store: store}
	op := testIdentity("dev", auth.RoleDeveloper)
	op.ProjectRoles = []auth.RoleScope{{Role: auth.RoleOperator, Scope: "project:p1"}}

	if _, err := s.SuspendCluster(ctxWithIdentity(op), SuspendClusterRequestObject{Id: "c1"}); err != nil {
		t.Fatalf("project operator suspend own project's cluster: %v", err)
	}
	got, _ := store.Get(ctx, "c1")
	if got.Desired != controller.DesiredSuspended {
		t.Fatalf("desired = %s, want suspended", got.Desired)
	}
	setObserved(t, store, "c1", core.ClusterStateSuspended, got.Generation)
	if _, err := s.ResumeCluster(ctxWithIdentity(op), ResumeClusterRequestObject{Id: "c1"}); err != nil {
		t.Fatalf("project operator resume own project's cluster: %v", err)
	}
	_, err := s.SuspendCluster(ctxWithIdentity(op), SuspendClusterRequestObject{Id: "c2"})
	mustHTTPError(t, err, 403)
	_, err = s.SuspendCluster(ctxWithIdentity(op), SuspendClusterRequestObject{Id: "nope"})
	mustHTTPError(t, err, 403)
}

func TestSuspendCluster_QueueOwnedIs409(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	gen, err := store.UpsertDesired(ctx, "c1", core.ClusterSpec{Name: "c1", Project: "proj-a", RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi"})
	if err != nil {
		t.Fatal(err)
	}
	setObserved(t, store, "c1", core.ClusterStateRunning, gen)
	if _, err := store.UpsertPool(ctx, "pool-a", core.PoolSpec{Name: "pool-a", Cohort: "c", Flavors: []core.FlavorSpec{{Name: "f", Resources: map[string]string{"cpu": "8"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "pool-a", Project: "proj-a", Namespace: "ns-a"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}
	_, err = s.SuspendCluster(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), SuspendClusterRequestObject{Id: "c1"})
	mustHTTPError(t, err, 409)
}

// --- ListJobs ---

func TestListJobs_ReadPermissionRequired(t *testing.T) {
	store := controller.NewMemoryStore()
	ctx := context.Background()
	if err := store.RecordJob(ctx, core.JobRecord{Id: "j1", Cluster: "c1", Submitter: "alice", Status: "SUCCEEDED", SubmittedAt: 100}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}

	_, err := s.ListJobs(ctxWithIdentity(testIdentity("nobody")), ListJobsRequestObject{})
	mustHTTPError(t, err, 403)

	resp, err := s.ListJobs(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), ListJobsRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views := mustResponse[ListJobs200JSONResponse](t, resp)
	if len(views) != 1 || views[0].Id != "j1" {
		t.Fatalf("views = %+v", views)
	}
}
