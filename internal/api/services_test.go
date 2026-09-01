package api

import (
	"context"
	"errors"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// stubServiceProvisioner is a minimal provision.ServiceProvisioner with
// scriptable responses, for exercising every services.go status/decision
// branch (Rust's services.rs has no unit test module of its own — it's a
// thin proxy — so this ports the handler branches directly rather than
// specific named test cases).
type stubServiceProvisioner struct {
	services   []provision.ObservedService
	get        *provision.ObservedService
	deployErr  error
	deleteErr  error
	listErr    error
	getErr     error
	deployedAs *core.ServiceSpec
}

func (p *stubServiceProvisioner) Deploy(_ context.Context, name string, spec *core.ServiceSpec) error {
	if p.deployErr != nil {
		return p.deployErr
	}
	p.deployedAs = spec
	_ = name
	return nil
}
func (p *stubServiceProvisioner) Get(context.Context, string) (*provision.ObservedService, error) {
	return p.get, p.getErr
}
func (p *stubServiceProvisioner) Delete(context.Context, string) error { return p.deleteErr }
func (p *stubServiceProvisioner) List(context.Context) ([]provision.ObservedService, error) {
	return p.services, p.listErr
}

func TestListServices_NilProvisionerAnswersEmpty(t *testing.T) {
	s := &Server{}
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
	s := &Server{}
	_, err := s.ListServices(ctxWithIdentity(&auth.Identity{Subject: "nobody"}), ListServicesRequestObject{})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}

func TestListServices_ReturnsProvisionerView(t *testing.T) {
	url := "https://svc.example/"
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{services: []provision.ObservedService{
		{Name: "svc-a", State: core.ClusterStateRunning, Url: &url},
	}}}
	resp, err := s.ListServices(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), ListServicesRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views := mustResponse[ListServices200JSONResponse](t, resp)
	if len(views) != 1 || views[0].Name != "svc-a" || views[0].State != "running" || views[0].Url == nil {
		t.Fatalf("views = %+v", views)
	}
}

func TestListServices_ProvisionerErrorIsBadGateway(t *testing.T) {
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{listErr: errors.New("boom")}}
	_, err := s.ListServices(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), ListServicesRequestObject{})
	if err == nil {
		t.Fatal("expected 502")
	}
	mustHTTPError(t, err, 502)
}

func TestGetService_NotFoundWithNilProvisioner(t *testing.T) {
	s := &Server{}
	_, err := s.GetService(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), GetServiceRequestObject{Name: "x"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestGetService_NotFoundWhenProvisionerHasNone(t *testing.T) {
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{get: nil}}
	_, err := s.GetService(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), GetServiceRequestObject{Name: "x"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestGetService_Found(t *testing.T) {
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{get: &provision.ObservedService{Name: "svc-a", State: core.ClusterStateProvisioning}}}
	resp, err := s.GetService(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), GetServiceRequestObject{Name: "svc-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	view := mustResponse[GetService200JSONResponse](t, resp)
	if view.Name != "svc-a" || view.State != "provisioning" {
		t.Errorf("view = %+v", view)
	}
}

func TestDeployService_MissingBodyRejected(t *testing.T) {
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{}}
	_, err := s.DeployService(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), DeployServiceRequestObject{})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestDeployService_DeniedForViewer(t *testing.T) {
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{}}
	body := &DeployService{Name: "svc-a", Spec: minimalServiceSpec()}
	_, err := s.DeployService(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), DeployServiceRequestObject{Body: body})
	if err == nil {
		t.Fatal("expected 403")
	}
	mustHTTPError(t, err, 403)
}

func TestDeployService_SuccessDefaultsToCanary(t *testing.T) {
	prov := &stubServiceProvisioner{}
	s := &Server{ServiceProvisioner: prov}
	body := &DeployService{Name: "svc-a", Spec: minimalServiceSpec()}
	resp, err := s.DeployService(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), DeployServiceRequestObject{Body: body})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustResponse[DeployService202Response](t, resp)
	if prov.deployedAs == nil || prov.deployedAs.Upgrade != core.UpgradeStrategyCanary {
		t.Errorf("deployed spec upgrade = %+v, want canary default", prov.deployedAs)
	}
}

func TestDeployService_ProvisionerErrorIsBadGateway(t *testing.T) {
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{deployErr: errors.New("boom")}}
	body := &DeployService{Name: "svc-a", Spec: minimalServiceSpec()}
	_, err := s.DeployService(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), DeployServiceRequestObject{Body: body})
	if err == nil {
		t.Fatal("expected 502")
	}
	mustHTTPError(t, err, 502)
}

func TestDeleteService_Success(t *testing.T) {
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{}}
	resp, err := s.DeleteService(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), DeleteServiceRequestObject{Name: "svc-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustResponse[DeleteService202Response](t, resp)
}

func TestDeleteService_DeniedForViewer(t *testing.T) {
	s := &Server{ServiceProvisioner: &stubServiceProvisioner{}}
	_, err := s.DeleteService(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), DeleteServiceRequestObject{Name: "svc-a"})
	if err == nil {
		t.Fatal("expected 403")
	}
	mustHTTPError(t, err, 403)
}

func minimalServiceSpec() ServiceSpec {
	return ServiceSpec{
		Name: "svc-a", Project: "proj", RayVersion: "2.9.0", Image: "rayproject/ray:2.9.0",
		HeadCpu: "1", HeadMemory: "2Gi", WorkerCpu: "1", WorkerMemory: "2Gi",
		WorkerReplicas: 1, ServeConfigV2: "applications: []",
	}
}
