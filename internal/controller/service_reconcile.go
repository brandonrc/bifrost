// Service reconcile loop (requirement 1: a deployed RayService converges to
// a serving endpoint). Services used to be a stateless proxy straight to
// the ServiceProvisioner; now DeployService writes a row and this loop
// owns actuation, so the API answers 202 from the store and a deploy that
// fails against Kubernetes is retried instead of lost.
//
// Shape mirrors the cluster reconciler (reconcile.go) at a smaller scale:
// observation-first — every tick reads the RayService back and records
// what it saw; actuation is derived from desired state vs observation, not
// from a stored phase. KubeRay's RayService controller owns convergence of
// the Serve app itself (and zero-downtime upgrades), so this loop only has
// to answer "is the RayService there, at the current generation?" and "is
// it serving?".
package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// ServiceAction is what one service reconcile pass did.
type ServiceAction int

const (
	// ServiceActionNone: observation recorded, nothing to actuate.
	ServiceActionNone ServiceAction = iota
	// ServiceActionDeploy: the RayService was applied (missing, or behind
	// the stored generation).
	ServiceActionDeploy
	// ServiceActionDelete: the RayService was deleted (desired terminated).
	ServiceActionDelete
	// ServiceActionReap: the terminated tombstone row was purged.
	ServiceActionReap
)

func (a ServiceAction) String() string {
	switch a {
	case ServiceActionNone:
		return "none"
	case ServiceActionDeploy:
		return "deploy"
	case ServiceActionDelete:
		return "delete"
	case ServiceActionReap:
		return "reap"
	default:
		return "unknown"
	}
}

// ServiceReconcileResult is the outcome of reconciling one service.
type ServiceReconcileResult struct {
	Name   string
	Action ServiceAction
	Err    error
}

// ServiceReconciler converges stored services onto RayServices and keeps
// the gateway registry's `serve` entries in step with what is serving.
type ServiceReconciler struct {
	store                   Store
	provisioner             provision.ServiceProvisioner
	terminatedRetentionSecs uint64
	// registrar/gatewayHostname: the dynamic gateway seam (Options). Both
	// nil = no registration.
	registrar       Registrar
	gatewayHostname func(core.ClusterId) string
	// registered remembers the endpoint last registered per service so a
	// tick only touches the registry when serving status or URL changed.
	registered map[string]core.ClusterEndpoint
}

// NewServiceReconciler returns a reconciler with the default tombstone
// retention and no gateway registration.
func NewServiceReconciler(store Store, provisioner provision.ServiceProvisioner) *ServiceReconciler {
	return &ServiceReconciler{
		store: store, provisioner: provisioner,
		terminatedRetentionSecs: TerminatedRetentionSecs,
		registered:              map[string]core.ClusterEndpoint{},
	}
}

// ReconcileAll runs one pass over every stored service.
func (r *ServiceReconciler) ReconcileAll(ctx context.Context) []ServiceReconcileResult {
	return r.ReconcileAllAt(ctx, NowUnix())
}

// ReconcileAllAt is ReconcileAll at an explicit clock, for tests.
func (r *ServiceReconciler) ReconcileAllAt(ctx context.Context, now uint64) []ServiceReconcileResult {
	services, err := r.store.ListServices(ctx)
	if err != nil {
		return []ServiceReconcileResult{{Err: wrapStoreErr(err)}}
	}
	out := make([]ServiceReconcileResult, 0, len(services))
	for i := range services {
		svc := &services[i]
		action, err := r.reconcileOne(ctx, svc, now)
		out = append(out, ServiceReconcileResult{Name: svc.Name, Action: action, Err: err})
	}
	return out
}

// reconcileOne observes, records, and actuates one service.
func (r *ServiceReconciler) reconcileOne(ctx context.Context, svc *StoredService, now uint64) (ServiceAction, error) {
	observed, err := r.provisioner.Get(ctx, svc.Name)
	if err != nil {
		if isProvisionNotFound(err) {
			observed = nil
		} else {
			return ServiceActionNone, wrapProvisionErr(err)
		}
	}

	if svc.Desired == DesiredTerminated {
		r.deregister(svc.Name)
		if observed == nil {
			// Gone: the row is a tombstone (state terminated) until the
			// retention window passes, then it is purged — the same
			// truthful-console rule clusters follow.
			if err := r.record(ctx, svc, core.ClusterStateTerminated, nil); err != nil {
				return ServiceActionNone, err
			}
			if svc.TerminatedAt != nil && now >= satAddU64(*svc.TerminatedAt, r.terminatedRetentionSecs) {
				if _, err := r.store.RemoveService(ctx, svc.Name); err != nil {
					return ServiceActionNone, wrapStoreErr(err)
				}
				return ServiceActionReap, nil
			}
			return ServiceActionNone, nil
		}
		if err := r.record(ctx, svc, core.ClusterStateTerminating, nil); err != nil {
			return ServiceActionNone, err
		}
		if err := r.provisioner.Delete(ctx, svc.Name); err != nil {
			return ServiceActionNone, wrapProvisionErr(err)
		}
		return ServiceActionDelete, nil
	}

	// Desired running (services have no suspend; any other desired state
	// is treated as running so a row is never left unreconciled).
	behind := observed == nil || observed.Generation == nil || *observed.Generation < svc.Generation
	if behind {
		// Record before actuating so a failed Deploy still leaves an
		// honest view: provisioning for a missing RayService, updating for
		// one that exists at an older generation.
		state := core.ClusterStateProvisioning
		var url *string
		if observed != nil {
			state = core.ClusterStateUpdating
			url = observed.Url
		}
		if err := r.record(ctx, svc, state, url); err != nil {
			return ServiceActionNone, err
		}
		r.deregister(svc.Name)
		// The serving-pool queue (requirement 4) is re-derived from the
		// store on every apply, like the cluster reconciler's compute
		// queue: an allocation added after the deploy is picked up by the
		// next roll, and a project with no serving allocation deploys
		// queue-free.
		queue, err := QueueAssignmentForProjectPurpose(ctx, r.store, svc.Spec.Project, core.PoolPurposeServing)
		if err != nil {
			return ServiceActionNone, err
		}
		if err := r.provisioner.Deploy(ctx, svc.Name, &svc.Spec, svc.Generation, queue); err != nil {
			return ServiceActionNone, wrapProvisionErr(err)
		}
		return ServiceActionDeploy, nil
	}

	if err := r.record(ctx, svc, observed.State, observed.Url); err != nil {
		return ServiceActionNone, err
	}
	if observed.State == core.ClusterStateRunning && observed.Url != nil {
		r.register(svc, *observed.Url)
	} else {
		r.deregister(svc.Name)
	}
	return ServiceActionNone, nil
}

// record persists an observation, skipping the write when nothing changed.
func (r *ServiceReconciler) record(ctx context.Context, svc *StoredService, state core.ClusterState, url *string) error {
	if svc.ObservedState != nil && *svc.ObservedState == state && ptrStringEqual(svc.ObservedURL, url) {
		return nil
	}
	if err := r.store.RecordServiceObservation(ctx, svc.Name, &state, url); err != nil {
		return wrapStoreErr(err)
	}
	return nil
}

func ptrStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// register publishes the service's Serve endpoint to the gateway under
// `<name>.<domain>` (Target serve, Source dynamic). No-op without the seam.
func (r *ServiceReconciler) register(svc *StoredService, url string) {
	if r.registrar == nil || r.gatewayHostname == nil {
		return
	}
	ep := core.ClusterEndpoint{
		Id:         core.ClusterId(svc.Name),
		Hostname:   r.gatewayHostname(core.ClusterId(svc.Name)),
		ApiBaseUrl: url,
		Project:    svc.Spec.Project,
		Target:     core.RegistryTargetServe,
		Source:     core.RegistrySourceDynamic,
	}
	if prev, ok := r.registered[svc.Name]; ok && prev == ep {
		return
	}
	if err := r.registrar.Register(ep); err != nil {
		slog.Warn("service gateway registration refused", "service", svc.Name, "hostname", ep.Hostname, "error", err)
		return
	}
	r.registered[svc.Name] = ep
	slog.Info("service registered with gateway", "service", svc.Name, "hostname", ep.Hostname)
}

func (r *ServiceReconciler) deregister(name string) {
	if r.registrar == nil {
		return
	}
	if _, ok := r.registered[name]; !ok {
		return
	}
	r.registrar.Deregister(core.ClusterId(name))
	delete(r.registered, name)
	slog.Info("service deregistered from gateway", "service", name)
}

// Run is the control loop: one pass immediately, then one per interval,
// until ctx is done.
func (r *ServiceReconciler) Run(ctx context.Context, interval time.Duration) {
	slog.Info("service reconcile loop started", "interval_secs", interval.Seconds())
	tick := func() {
		for _, res := range r.ReconcileAll(ctx) {
			if res.Err != nil {
				slog.Warn("service reconcile failed", "service", res.Name, "error", res.Err)
			}
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	tick()
	for {
		select {
		case <-ticker.C:
			tick()
		case <-ctx.Done():
			slog.Info("service reconcile loop shutting down")
			return
		}
	}
}

// ServiceOptions configures RunServiceReconciler.
type ServiceOptions struct {
	// Interval is the resync tick. DefaultReconcileInterval when <= 0.
	Interval time.Duration
	// TerminatedRetentionSecs overrides the tombstone retention window;
	// nil = TerminatedRetentionSecs.
	TerminatedRetentionSecs *uint64
	// Registrar and GatewayHostname are the dynamic gateway seam (same
	// semantics as Options): both non-nil = a running service is
	// registered as a `serve` endpoint at GatewayHostname(name).
	Registrar       Registrar
	GatewayHostname func(core.ClusterId) string
}

func newServiceReconcilerFromOptions(store Store, provisioner provision.ServiceProvisioner, opts ServiceOptions) *ServiceReconciler {
	r := NewServiceReconciler(store, provisioner)
	if opts.TerminatedRetentionSecs != nil {
		r.terminatedRetentionSecs = *opts.TerminatedRetentionSecs
	}
	r.registrar = opts.Registrar
	r.gatewayHostname = opts.GatewayHostname
	return r
}

// RunServiceReconciler constructs a ServiceReconciler from opts and runs it
// until ctx is done. The returned error is always nil today (ctx
// cancellation is a normal shutdown), symmetric with RunReconciler.
func RunServiceReconciler(ctx context.Context, store Store, provisioner provision.ServiceProvisioner, opts ServiceOptions) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	newServiceReconcilerFromOptions(store, provisioner, opts).Run(ctx, interval)
	return nil
}
