package api

import (
	"context"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// The service handlers are store-backed (requirement 1): every test here
// runs against a real MemoryStore with no ServiceProvisioner at all — the
// API path must not depend on one (the reconciler owns actuation).
func newServiceServer(t *testing.T) *Server {
	t.Helper()
	return &Server{Store: newMemStore(t)}
}

func minimalServiceSpec() ServiceSpec {
	return ServiceSpec{
		Name: "svc-a", Project: "proj", RayVersion: "2.9.0", Image: "rayproject/ray:2.9.0",
		HeadCpu: "1", HeadMemory: "2Gi", WorkerCpu: "1", WorkerMemory: "2Gi",
		WorkerReplicas: 1, ServeConfigV2: "applications: []",
	}
}

func deployAs(t *testing.T, s *Server, id *auth.Identity, name string, spec ServiceSpec) error {
	t.Helper()
	body := &DeployService{Name: name, Spec: spec}
	resp, err := s.DeployService(ctxWithIdentity(id), DeployServiceRequestObject{Body: body})
	if err == nil {
		mustResponse[DeployService202Response](t, resp)
	}
	return err
}

func TestListServices_EmptyStoreAnswersEmpty(t *testing.T) {
	s := newServiceServer(t)
	resp, err := s.ListServices(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), ListServicesRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views := mustResponse[ListServices200JSONResponse](t, resp)
	if len(views) != 0 {
		t.Fatalf("got %d services, want 0", len(views))
	}
}

func TestListServices_DeniedWithoutRead(t *testing.T) {
	s := newServiceServer(t)
	_, err := s.ListServices(ctxWithIdentity(&auth.Identity{Subject: "nobody"}), ListServicesRequestObject{})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}

// A deploy is a 202 that writes the row: the view is provisioning (nothing
// observed yet), carries the project and the server-stamped owner, and
// the row's spec defaults the upgrade strategy to canary.
func TestDeployService_WritesRowAndAnswers202(t *testing.T) {
	s := newServiceServer(t)
	if err := deployAs(t, s, testIdentity("dev", auth.RoleDeveloper), "svc-a", minimalServiceSpec()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	resp, err := s.GetService(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), GetServiceRequestObject{Name: "svc-a"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	view := mustResponse[GetService200JSONResponse](t, resp)
	if view.Name != "svc-a" || view.Project != "proj" || view.State != "provisioning" || view.Owner == nil || *view.Owner != "dev" {
		t.Fatalf("view = %+v", view)
	}
	if view.Url != nil || view.GatewayUrl != nil || view.Queue != nil {
		t.Fatalf("nothing observed yet, view must carry no url/gateway_url/queue: %+v", view)
	}
	row, _ := s.Store.GetService(context.Background(), "svc-a")
	if row == nil || row.Spec.Upgrade != core.UpgradeStrategyCanary || row.Generation != 1 {
		t.Fatalf("row = %+v, want canary default at generation 1", row)
	}
}

// A same-name redeploy is an update: an unchanged spec keeps the
// generation, a changed one bumps it; the first deployer stays the owner.
func TestDeployService_SameNameUpdatesGeneration(t *testing.T) {
	s := newServiceServer(t)
	dev := testIdentity("dev", auth.RoleDeveloper)
	if err := deployAs(t, s, dev, "svc-a", minimalServiceSpec()); err != nil {
		t.Fatal(err)
	}
	if err := deployAs(t, s, testIdentity("other", auth.RoleDeveloper), "svc-a", minimalServiceSpec()); err != nil {
		t.Fatal(err)
	}
	row, _ := s.Store.GetService(context.Background(), "svc-a")
	if row.Generation != 1 || row.Owner == nil || *row.Owner != "dev" {
		t.Fatalf("unchanged redeploy: gen=%d owner=%v, want 1/dev", row.Generation, row.Owner)
	}
	changed := minimalServiceSpec()
	changed.WorkerReplicas = 3
	if err := deployAs(t, s, dev, "svc-a", changed); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Store.GetService(context.Background(), "svc-a")
	if row.Generation != 2 || row.Spec.WorkerReplicas != 3 {
		t.Fatalf("changed redeploy: gen=%d replicas=%d, want 2/3", row.Generation, row.Spec.WorkerReplicas)
	}
}

// The view merges the reconciler's observation and, once the gateway has
// the service registered as a serve target, the external gateway URL.
func TestGetService_MergesObservationAndGatewayURL(t *testing.T) {
	s := newServiceServer(t)
	s.Registry = &core.ClusterRegistry{}
	s.GatewayExternalBase = "https://"
	dev := testIdentity("dev", auth.RoleDeveloper)
	if err := deployAs(t, s, dev, "svc-a", minimalServiceSpec()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running := core.ClusterStateRunning
	url := "http://svc-a-serve-svc.ns.svc:8000"
	if err := s.Store.RecordServiceObservation(ctx, "svc-a", &running, &url); err != nil {
		t.Fatal(err)
	}
	if err := s.Registry.Register(core.ClusterEndpoint{Id: "svc-a", Hostname: "svc-a.ray.test", ApiBaseUrl: url, Target: core.RegistryTargetServe}); err != nil {
		t.Fatal(err)
	}
	resp, err := s.GetService(ctxWithIdentity(dev), GetServiceRequestObject{Name: "svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	view := mustResponse[GetService200JSONResponse](t, resp)
	if view.State != "running" || view.Url == nil || *view.Url != url {
		t.Fatalf("view = %+v", view)
	}
	if view.GatewayUrl == nil || *view.GatewayUrl != "https://svc-a.ray.test" {
		t.Fatalf("gateway_url = %v, want https://svc-a.ray.test", view.GatewayUrl)
	}
	// A jobs-target entry under the same id is not a serve endpoint.
	s.Registry.Deregister("svc-a")
	if err := s.Registry.Register(core.ClusterEndpoint{Id: "svc-a", Hostname: "svc-a.ray.test", ApiBaseUrl: url}); err != nil {
		t.Fatal(err)
	}
	resp, _ = s.GetService(ctxWithIdentity(dev), GetServiceRequestObject{Name: "svc-a"})
	if view := mustResponse[GetService200JSONResponse](t, resp); view.GatewayUrl != nil {
		t.Fatalf("a jobs entry must not produce a serve gateway_url: %+v", view)
	}
}

func TestGetService_NotFound(t *testing.T) {
	s := newServiceServer(t)
	_, err := s.GetService(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), GetServiceRequestObject{Name: "x"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

// Requirement 2 read side: a project-scoped caller is narrowed to their
// projects — another project's service is 404 on get and absent from the
// list, exactly as clusters behave.
func TestServices_ProjectScopedReadsAreNarrowed(t *testing.T) {
	s := newServiceServer(t)
	admin := testIdentity("admin", auth.RoleAdmin)
	specA := minimalServiceSpec()
	specA.Project = "team-a"
	specB := minimalServiceSpec()
	specB.Name = "svc-b"
	specB.Project = "team-b"
	if err := deployAs(t, s, admin, "svc-a", specA); err != nil {
		t.Fatal(err)
	}
	if err := deployAs(t, s, admin, "svc-b", specB); err != nil {
		t.Fatal(err)
	}
	devB := &auth.Identity{Subject: "dev-b", Roles: []auth.Role{auth.RoleDeveloper},
		ProjectRoles: []auth.RoleScope{{Role: auth.RoleOperator, Scope: "project:team-b"}}}

	_, err := s.GetService(ctxWithIdentity(devB), GetServiceRequestObject{Name: "svc-a"})
	if err == nil {
		t.Fatal("expected 404 for another project's service")
	}
	mustHTTPError(t, err, 404)
	if _, err := s.GetService(ctxWithIdentity(devB), GetServiceRequestObject{Name: "svc-b"}); err != nil {
		t.Fatalf("own project's service: %v", err)
	}
	resp, err := s.ListServices(ctxWithIdentity(devB), ListServicesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	views := mustResponse[ListServices200JSONResponse](t, resp)
	if len(views) != 1 || views[0].Name != "svc-b" {
		t.Fatalf("narrowed list = %+v, want only svc-b", views)
	}
	all, _ := s.ListServices(ctxWithIdentity(admin), ListServicesRequestObject{})
	if views := mustResponse[ListServices200JSONResponse](t, all); len(views) != 2 {
		t.Fatalf("admin list = %+v, want both", views)
	}
}

func TestDeployService_MissingBodyRejected(t *testing.T) {
	s := newServiceServer(t)
	_, err := s.DeployService(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), DeployServiceRequestObject{})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestDeployService_ValidatesNameAndProject(t *testing.T) {
	s := newServiceServer(t)
	dev := testIdentity("dev", auth.RoleDeveloper)
	if err := deployAs(t, s, dev, "Not_A_K8s_Name", minimalServiceSpec()); err == nil {
		t.Fatal("expected 400 for an invalid name")
	} else {
		mustHTTPError(t, err, 400)
	}
	mismatch := minimalServiceSpec()
	mismatch.Name = "other"
	if err := deployAs(t, s, dev, "svc-a", mismatch); err == nil {
		t.Fatal("expected 400 for spec.name mismatch")
	} else {
		mustHTTPError(t, err, 400)
	}
	noProject := minimalServiceSpec()
	noProject.Project = ""
	if err := deployAs(t, s, dev, "svc-a", noProject); err == nil {
		t.Fatal("expected 400 for a missing project")
	} else {
		mustHTTPError(t, err, 400)
	}
	unnamed := minimalServiceSpec()
	unnamed.Name = ""
	if err := deployAs(t, s, dev, "svc-a", unnamed); err != nil {
		t.Fatalf("an empty spec.name takes the body name: %v", err)
	}
	if rows, _ := s.Store.ListServices(context.Background()); len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly the one valid deploy", rows)
	}
}

func TestDeployService_DeniedForViewer(t *testing.T) {
	s := newServiceServer(t)
	err := deployAs(t, s, testIdentity("v", auth.RoleViewer), "svc-a", minimalServiceSpec())
	if err == nil {
		t.Fatal("expected 403")
	}
	mustHTTPError(t, err, 403)
	if rows, _ := s.Store.ListServices(context.Background()); len(rows) != 0 {
		t.Fatalf("a denied deploy must not write a row: %+v", rows)
	}
}

// A project-scoped operator grant is enough to deploy into that project
// (AuthorizeScoped), and not into another.
func TestDeployService_ScopedGrantCoversOwnProjectOnly(t *testing.T) {
	s := newServiceServer(t)
	scoped := &auth.Identity{Subject: "op-a", Roles: []auth.Role{auth.RoleViewer},
		ProjectRoles: []auth.RoleScope{{Role: auth.RoleDeveloper, Scope: "project:team-a"}}}
	specA := minimalServiceSpec()
	specA.Project = "team-a"
	if err := deployAs(t, s, scoped, "svc-a", specA); err != nil {
		t.Fatalf("own project: %v", err)
	}
	specB := minimalServiceSpec()
	specB.Project = "team-b"
	if err := deployAs(t, s, scoped, "svc-a", specB); err == nil {
		t.Fatal("expected 403 for another project")
	} else {
		mustHTTPError(t, err, 403)
	}
}

// Delete flips desired to terminated (202); the view reads terminating
// until the reconciler confirms the RayService gone, then terminated. A
// second delete is idempotent.
func TestDeleteService_MarksTerminated(t *testing.T) {
	s := newServiceServer(t)
	dev := testIdentity("dev", auth.RoleDeveloper)
	if err := deployAs(t, s, dev, "svc-a", minimalServiceSpec()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		resp, err := s.DeleteService(ctxWithIdentity(dev), DeleteServiceRequestObject{Name: "svc-a"})
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		mustResponse[DeleteService202Response](t, resp)
	}
	get, _ := s.GetService(ctxWithIdentity(dev), GetServiceRequestObject{Name: "svc-a"})
	if view := mustResponse[GetService200JSONResponse](t, get); view.State != "terminating" {
		t.Fatalf("state after delete = %q, want terminating", view.State)
	}
	terminated := core.ClusterStateTerminated
	_ = s.Store.RecordServiceObservation(context.Background(), "svc-a", &terminated, nil)
	get, _ = s.GetService(ctxWithIdentity(dev), GetServiceRequestObject{Name: "svc-a"})
	if view := mustResponse[GetService200JSONResponse](t, get); view.State != "terminated" || view.Url != nil {
		t.Fatalf("tombstone view = %+v, want terminated with no url", view)
	}
}

func TestDeleteService_UnknownIs404ForWriter(t *testing.T) {
	s := newServiceServer(t)
	_, err := s.DeleteService(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), DeleteServiceRequestObject{Name: "ghost"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestDeleteService_DeniedForViewer(t *testing.T) {
	s := newServiceServer(t)
	if err := deployAs(t, s, testIdentity("dev", auth.RoleDeveloper), "svc-a", minimalServiceSpec()); err != nil {
		t.Fatal(err)
	}
	_, err := s.DeleteService(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), DeleteServiceRequestObject{Name: "svc-a"})
	if err == nil {
		t.Fatal("expected 403")
	}
	mustHTTPError(t, err, 403)
	// And a viewer must not learn whether an unknown name exists either.
	_, err = s.DeleteService(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), DeleteServiceRequestObject{Name: "ghost"})
	mustHTTPError(t, err, 403)
}

// Requirement 2 write side: a project-narrowed caller deleting another
// project's service gets 404 (it was never visible to them).
func TestDeleteService_OutOfScopeIs404(t *testing.T) {
	s := newServiceServer(t)
	specA := minimalServiceSpec()
	specA.Project = "team-a"
	if err := deployAs(t, s, testIdentity("admin", auth.RoleAdmin), "svc-a", specA); err != nil {
		t.Fatal(err)
	}
	devB := &auth.Identity{Subject: "dev-b", Roles: []auth.Role{auth.RoleDeveloper},
		ProjectRoles: []auth.RoleScope{{Role: auth.RoleOperator, Scope: "project:team-b"}}}
	_, err := s.DeleteService(ctxWithIdentity(devB), DeleteServiceRequestObject{Name: "svc-a"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
	row, _ := s.Store.GetService(context.Background(), "svc-a")
	if row.Desired != controller.DesiredRunning {
		t.Fatalf("out-of-scope delete must not flip desired: %+v", row)
	}
}

// Dev mode (no identity): everything is permitted and the owner is nil.
func TestDeployService_NoIdentityStampsNoOwner(t *testing.T) {
	s := newServiceServer(t)
	body := &DeployService{Name: "svc-a", Spec: minimalServiceSpec()}
	if _, err := s.DeployService(context.Background(), DeployServiceRequestObject{Body: body}); err != nil {
		t.Fatal(err)
	}
	row, _ := s.Store.GetService(context.Background(), "svc-a")
	if row == nil || row.Owner != nil {
		t.Fatalf("row = %+v, want no owner", row)
	}
}

// One service per project (requirement 2, ruling D8): a second name in the
// same project is 409 and audited as a deny; a same-name redeploy and
// another project's service are unaffected.
func TestDeployService_SecondNameInProjectIs409(t *testing.T) {
	s := newServiceServer(t)
	dev := testIdentity("dev", auth.RoleDeveloper)
	if err := deployAs(t, s, dev, "svc-a", minimalServiceSpec()); err != nil {
		t.Fatal(err)
	}
	second := minimalServiceSpec()
	second.Name = "svc-b"
	err := deployAs(t, s, dev, "svc-b", second)
	if err == nil {
		t.Fatal("second service in the same project must be refused")
	}
	mustHTTPError(t, err, 409)
	if row, _ := s.Store.GetService(context.Background(), "svc-b"); row != nil {
		t.Fatalf("refused deploy must write no row, got %+v", row)
	}
	rows, _, aerr := s.Store.ListAudit(context.Background(), core.AuditFilter{})
	if aerr != nil {
		t.Fatal(aerr)
	}
	var denied bool
	for _, row := range rows {
		ev := row.Event
		if ev.Decision == core.AuditDecisionDeny && ev.Reason != nil && *ev.Reason == "service_limit" &&
			ev.Action != nil && *ev.Action == "deploy_service" && ev.Cluster != nil && *ev.Cluster == "svc-b" &&
			ev.Status != nil && *ev.Status == 409 {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("no service_limit deny in the audit trail: %+v", rows)
	}

	// Same name is an update, another project is unaffected.
	if err := deployAs(t, s, dev, "svc-a", minimalServiceSpec()); err != nil {
		t.Fatalf("same-name redeploy: %v", err)
	}
	other := minimalServiceSpec()
	other.Name, other.Project = "svc-c", "other"
	if err := deployAs(t, s, dev, "svc-c", other); err != nil {
		t.Fatalf("another project's first service: %v", err)
	}
}

// Deleting the first service (desired terminated) frees the project's slot
// at once — no wait for the tombstone — and --services-per-project raises
// the cap.
func TestDeployService_LimitFreesOnDeleteAndHonoursCap(t *testing.T) {
	s := newServiceServer(t)
	dev := testIdentity("dev", auth.RoleDeveloper)
	if err := deployAs(t, s, dev, "svc-a", minimalServiceSpec()); err != nil {
		t.Fatal(err)
	}
	second := minimalServiceSpec()
	second.Name = "svc-b"
	if _, err := s.DeleteService(ctxWithIdentity(dev), DeleteServiceRequestObject{Name: "svc-a"}); err != nil {
		t.Fatal(err)
	}
	if err := deployAs(t, s, dev, "svc-b", second); err != nil {
		t.Fatalf("deploy after delete: %v", err)
	}

	third := minimalServiceSpec()
	third.Name = "svc-c"
	if err := deployAs(t, s, dev, "svc-c", third); err == nil {
		t.Fatal("cap 1: third name must be refused while svc-b is live")
	}
	s.ServicesPerProject = 2
	if err := deployAs(t, s, dev, "svc-c", third); err != nil {
		t.Fatalf("cap 2 must admit a second live service: %v", err)
	}
	fourth := minimalServiceSpec()
	fourth.Name = "svc-d"
	err := deployAs(t, s, dev, "svc-d", fourth)
	if err == nil {
		t.Fatal("cap 2: a third live service must be refused")
	}
	mustHTTPError(t, err, 409)
}

// A global Developer role plus a project-scoped binding is narrowed to the
// bound projects for deploy too (requirement 2): the seeded dev-b of the
// requirement lane is exactly this shape and must not deploy into team-a.
func TestDeployService_GlobalRoleNarrowedByScopedBinding(t *testing.T) {
	s := newServiceServer(t)
	devB := &auth.Identity{Subject: "dev-b", Roles: []auth.Role{auth.RoleDeveloper},
		ProjectRoles: []auth.RoleScope{{Role: auth.RoleOperator, Scope: "project:team-b"}}}
	specA := minimalServiceSpec()
	specA.Project = "team-a"
	err := deployAs(t, s, devB, "svc-a", specA)
	if err == nil {
		t.Fatal("a team-b developer must not deploy into team-a")
	}
	mustHTTPError(t, err, 403)
	specB := minimalServiceSpec()
	specB.Project = "team-b"
	if err := deployAs(t, s, devB, "svc-a", specB); err != nil {
		t.Fatalf("own project: %v", err)
	}
}
