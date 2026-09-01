// Ray Serve service API (Phase 4). Unlike clusters, there is no Bifrost
// desired-state store or reconcile loop here: KubeRay's RayService
// controller owns convergence and zero-downtime (canary) upgrades, so
// Bifrost is a thin authenticated CRUD proxy over the live provisioner.
//
// Permissions are against auth.TargetService (#26): deploying/updating a
// Serve app is "code", so Developer (and Admin) may write; Operator and
// Viewer are read-only. Ported from mobula-api's services.rs.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// errNoServiceBackend backs the 502 a caller gets from a mutating service
// route when s.ServiceProvisioner is nil (no service backend configured) —
// deploy/delete have no natural "unconfigured" response of their own in
// the frozen contract, so this collapses to the same 502 a live backend
// failure would produce.
var errNoServiceBackend = errors.New("no service backend configured")

// serviceView converts an ObservedService into the wire ServiceView.
func serviceView(o *provision.ObservedService) ServiceView {
	return ServiceView{Name: o.Name, State: o.State.String(), Url: o.Url}
}

// serviceProvErr converts a provisioner failure into the canonical 502
// HTTPError, after logging it server-side — services.rs's store_err
// equivalent for the service backend (a live cluster call, not a store
// read, so a failure there is a bad-gateway, not a 500).
func serviceProvErr(err error) error {
	slog.Warn("api: service provisioner error", "error", err)
	return HTTPError{Status: http.StatusBadGateway, Code: "bad_gateway", Message: "service backend error"}
}

// ListServices lists every managed service. Read on Target::Service
// (services.rs passes a nil store to authorize — a denial stays
// trace-only, matching the store-less services router).
func (s *Server) ListServices(ctx context.Context, _ ListServicesRequestObject) (ListServicesResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, nil, identity, auth.Read, auth.TargetService); err != nil {
		return nil, err
	}
	if s.ServiceProvisioner == nil {
		return ListServices200JSONResponse{}, nil
	}
	svcs, err := s.ServiceProvisioner.List(ctx)
	if err != nil {
		return nil, serviceProvErr(err)
	}
	views := make([]ServiceView, len(svcs))
	for i := range svcs {
		views[i] = serviceView(&svcs[i])
	}
	return ListServices200JSONResponse(views), nil
}

// GetService reads one service. Read on Target::Service.
func (s *Server) GetService(ctx context.Context, req GetServiceRequestObject) (GetServiceResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, nil, identity, auth.Read, auth.TargetService); err != nil {
		return nil, err
	}
	if s.ServiceProvisioner == nil {
		return nil, notFound("no such service")
	}
	svc, err := s.ServiceProvisioner.Get(ctx, req.Name)
	if err != nil {
		return nil, serviceProvErr(err)
	}
	if svc == nil {
		return nil, notFound("no such service")
	}
	return GetService200JSONResponse(serviceView(svc)), nil
}

// serviceSpecFromWire converts the generated wire ServiceSpec into
// core.ServiceSpec.
func serviceSpecFromWire(w *ServiceSpec) (core.ServiceSpec, error) {
	spec := core.ServiceSpec{
		Name: w.Name, Project: w.Project, RayVersion: w.RayVersion, Image: w.Image,
		HeadCpu: w.HeadCpu, HeadMemory: w.HeadMemory, WorkerCpu: w.WorkerCpu, WorkerMemory: w.WorkerMemory,
		WorkerReplicas: uint32NonNeg(w.WorkerReplicas), ServeConfigV2: w.ServeConfigV2,
		Upgrade: core.DefaultUpgradeStrategy,
	}
	if w.Upgrade != nil {
		switch *w.Upgrade {
		case Canary:
			spec.Upgrade = core.UpgradeStrategyCanary
		case InPlace:
			spec.Upgrade = core.UpgradeStrategyInPlace
		default:
			return core.ServiceSpec{}, badRequest("invalid upgrade strategy " + string(*w.Upgrade))
		}
	}
	return spec, nil
}

func uint32NonNeg(v int32) uint32 {
	if v < 0 {
		return 0
	}
	return uint32(v)
}

// DeployService deploys or updates a service. Write on Target::Service
// (Developer/Admin only).
func (s *Server) DeployService(ctx context.Context, req DeployServiceRequestObject) (DeployServiceResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, nil, identity, auth.Write, auth.TargetService); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, badRequest("missing request body")
	}
	spec, err := serviceSpecFromWire(&req.Body.Spec)
	if err != nil {
		return nil, err
	}
	if s.ServiceProvisioner == nil {
		return nil, serviceProvErr(errNoServiceBackend)
	}
	if err := s.ServiceProvisioner.Deploy(ctx, req.Body.Name, &spec); err != nil {
		return nil, serviceProvErr(err)
	}
	slog.Info("api: audit event",
		"decision", "allow", "subject", subjectOrDash(identity), "action", "deploy_service",
		"service", req.Body.Name, "upgrade", spec.Upgrade)
	return DeployService202Response{}, nil
}

// DeleteService tears down a service. Write on Target::Service.
func (s *Server) DeleteService(ctx context.Context, req DeleteServiceRequestObject) (DeleteServiceResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, nil, identity, auth.Write, auth.TargetService); err != nil {
		return nil, err
	}
	if s.ServiceProvisioner == nil {
		return nil, serviceProvErr(errNoServiceBackend)
	}
	if err := s.ServiceProvisioner.Delete(ctx, req.Name); err != nil {
		return nil, serviceProvErr(err)
	}
	slog.Info("api: audit event",
		"decision", "allow", "subject", subjectOrDash(identity), "action", "delete_service", "service", req.Name)
	return DeleteService202Response{}, nil
}

func subjectOrDash(id *auth.Identity) string {
	if id == nil {
		return "-"
	}
	return id.Subject
}
