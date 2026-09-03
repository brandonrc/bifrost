// Ray Serve service API (requirement 1: deploy from Jupyter; requirement
// 2: project-scoped). A deploy writes a desired-state row
// (Store.UpsertService) and answers 202; the service reconcile loop
// (internal/controller/service_reconcile.go) owns actuation against the
// ServiceProvisioner and records what it observes, so reads here are the
// store row merged with the last observation — the same observation-first
// shape clusters have. Nothing on this path needs a ServiceProvisioner: a
// control plane without one accepts deploys that simply never converge,
// which the view reports honestly as provisioning.
//
// Permissions are against auth.TargetService (#26): deploying/updating a
// Serve app is "code", so Developer (and Admin) may write; Operator and
// Viewer are read-only. Writes are scoped to the spec's project
// (AuthorizeScoped); reads follow readScope's narrowing, so a
// project-scoped caller never learns another project's service exists
// (404, like clusters).
package api

import (
	"context"
	"net/http"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// serviceView merges a stored service with its last observation into the
// wire ServiceView. State is the observed state, except that a service
// whose desired state is terminated reads as terminating until the
// backend confirms it gone (then terminated — the tombstone the reconciler
// reaps); a row nothing has observed yet is provisioning. gateway_url is
// filled only while the reconciler has the service registered as a
// `serve` endpoint and an external base is configured. queue is the
// serving-pool package's (requirement 4) — nil until it lands.
func (s *Server) serviceView(svc *controller.StoredService) ServiceView {
	view := ServiceView{Name: svc.Name, Project: svc.Spec.Project, Owner: svc.Owner, Url: svc.ObservedURL}
	switch {
	case svc.Desired == controller.DesiredTerminated:
		view.State = core.ClusterStateTerminating.String()
		if svc.ObservedState != nil && *svc.ObservedState == core.ClusterStateTerminated {
			view.State = core.ClusterStateTerminated.String()
		}
		view.Url = nil
	case svc.ObservedState == nil:
		view.State = core.ClusterStateProvisioning.String()
	default:
		view.State = svc.ObservedState.String()
	}
	if s.Registry != nil && s.GatewayExternalBase != "" {
		if ep, ok := s.Registry.ByID(core.ClusterId(svc.Name)); ok && ep.Target == core.RegistryTargetServe {
			u := s.GatewayExternalBase + ep.Hostname
			view.GatewayUrl = &u
		}
	}
	return view
}

// ListServices lists every service the caller may read (#49 scoped RBAC +
// ADR-0009 addendum read-scoping — see readScope; the same shape as
// ListClusters, against Target::Service).
func (s *Server) ListServices(ctx context.Context, _ ListServicesRequestObject) (ListServicesResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	assignments, narrowed := readScope(ctx, s.Store, identity)
	if len(narrowed) == 0 && identity != nil {
		if !identity.Permits(auth.Read, auth.TargetService) && len(assignments) == 0 {
			if err := Authorize(ctx, s.Store, identity, auth.Read, auth.TargetService); err != nil {
				return nil, err
			}
		}
	}
	rows, err := s.Store.ListServices(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	views := make([]ServiceView, 0, len(rows))
	for i := range rows {
		svc := &rows[i]
		var visible bool
		switch {
		case len(narrowed) > 0:
			visible = containsString(narrowed, svc.Spec.Project)
		case identity != nil:
			visible = identity.PermitsScoped(auth.Read, auth.TargetService, assignments, svc.Spec.Project)
		default:
			visible = true
		}
		if visible {
			views = append(views, s.serviceView(svc))
		}
	}
	return ListServices200JSONResponse(views), nil
}

// GetService reads one service. An out-of-scope service (narrowed away by
// project-scoped assignments) 404s rather than 403s, like GetCluster.
func (s *Server) GetService(ctx context.Context, req GetServiceRequestObject) (GetServiceResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	svc, err := s.Store.GetService(ctx, req.Name)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	if svc == nil {
		return nil, notFound("no such service")
	}
	_, narrowed := readScope(ctx, s.Store, identity)
	if len(narrowed) > 0 && !containsString(narrowed, svc.Spec.Project) {
		return nil, notFound("no such service")
	}
	if err := AuthorizeScoped(ctx, s.Store, identity, auth.Read, auth.TargetService, svc.Spec.Project); err != nil {
		return nil, err
	}
	return GetService200JSONResponse(s.serviceView(svc)), nil
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
	if w.Storage != nil {
		spec.Storage = append([]string(nil), (*w.Storage)...)
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

// DeployService deploys or updates a service: Write on Target::Service in
// the spec's project (Developer/Admin). The row is upserted — a same-name
// redeploy with a changed spec bumps the generation and the reconciler
// rolls the RayService (canary or in-place per the spec); an unchanged
// spec is a no-op — and the answer is the contract's 202 (plan ruling
// D11): accepted, converging, watch get_service.
func (s *Server) DeployService(ctx context.Context, req DeployServiceRequestObject) (DeployServiceResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if req.Body == nil {
		return nil, badRequest("missing request body")
	}
	name := req.Body.Name
	if !core.IsK8sName(name) {
		return nil, badRequest("service name must be a valid Kubernetes resource name")
	}
	spec, err := serviceSpecFromWire(&req.Body.Spec)
	if err != nil {
		return nil, err
	}
	if spec.Name == "" {
		spec.Name = name
	}
	if spec.Name != name {
		return nil, badRequest("spec.name must match name")
	}
	if spec.Project == "" {
		return nil, badRequest("spec.project is required")
	}
	if err := AuthorizeScoped(ctx, s.Store, identity, auth.Write, auth.TargetService, spec.Project); err != nil {
		return nil, err
	}

	// HOOK(private-storage, package G): resolve spec.Storage against the
	// policy catalog here — `spec.StorageResolved, err = s.resolveStorage(ctx,
	// spec.Project, spec.Storage)`; 400 for unknown names or entries the
	// project may not use. Runs before admission so a refused reference
	// never consumes quota.

	// HOOK(group-serving, package D): one service per project (ruling D8).
	// List the live (desired running) rows; another name in spec.Project
	// → 409 unless s.ServicesPerProject allows more. Same name = update.

	// HOOK(serving-pool, package E): admit against the project's serving
	// allocation (policy.ServiceDemand vs the serving pool's nominal), 409
	// on over-commit, and resolve the serving QueueAssignment the
	// reconciler stamps on the RayService (view.queue).

	if identity != nil && identity.Subject != "" {
		// Owner is server-stamped from the authenticated identity; the
		// store keeps the first deployer on a same-name update.
		owner := identity.Subject
		if _, err := s.Store.UpsertService(ctx, name, spec, &owner); err != nil {
			return nil, wrapStoreErr(err)
		}
	} else if _, err := s.Store.UpsertService(ctx, name, spec, nil); err != nil {
		return nil, wrapStoreErr(err)
	}

	action := "deploy_service"
	status := uint16(http.StatusAccepted)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: identitySubject(identity),
		Decision: core.AuditDecisionAllow, Action: &action, Cluster: &name, Status: &status,
	})
	return DeployService202Response{}, nil
}

// DeleteService requests teardown: desired → terminated; the reconciler
// deletes the RayService and the row becomes a terminated tombstone. Write
// on Target::Service in the row's project. Idempotent for a row already
// terminating. An unknown or out-of-scope name is 404 for a caller who
// could not have seen it listed; a caller with global write learns 404
// only for a genuinely unknown name.
func (s *Server) DeleteService(ctx context.Context, req DeleteServiceRequestObject) (DeleteServiceResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	svc, err := s.Store.GetService(ctx, req.Name)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	if svc == nil {
		if aerr := Authorize(ctx, s.Store, identity, auth.Write, auth.TargetService); aerr != nil {
			return nil, aerr
		}
		return nil, notFound("no such service")
	}
	_, narrowed := readScope(ctx, s.Store, identity)
	if len(narrowed) > 0 && !containsString(narrowed, svc.Spec.Project) {
		return nil, notFound("no such service")
	}
	if err := AuthorizeScoped(ctx, s.Store, identity, auth.Write, auth.TargetService, svc.Spec.Project); err != nil {
		return nil, err
	}
	if svc.Desired != controller.DesiredTerminated {
		if err := s.Store.SetServiceDesired(ctx, req.Name, controller.DesiredTerminated); err != nil {
			return nil, wrapStoreErr(err)
		}
	}
	action := "delete_service"
	status := uint16(http.StatusAccepted)
	name := req.Name
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: identitySubject(identity),
		Decision: core.AuditDecisionAllow, Action: &action, Cluster: &name, Status: &status,
	})
	return DeleteService202Response{}, nil
}
