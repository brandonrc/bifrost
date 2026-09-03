// Package app is the one place Bifrost's control plane is wired together:
// store + auth + provisioner -> api.Server -> http.Handler, plus the
// reconcile loops. cmd/bifrost/serve.go calls New for production;
// test/requirements/target/inproc calls the SAME New with a fake
// provisioner. That sharing is the point — an in-process requirement test
// exercises the production wiring, not a look-alike.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/brandonrc/bifrost/internal/api"
	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// Config is everything New needs. Store is required; the rest is optional
// and nil means "that subsystem is off", exactly as the serve flags mean it.
type Config struct {
	Store                controller.Store
	Registry             *core.ClusterRegistry
	Validator            *auth.Validator
	Local                *auth.LocalAuthenticator
	Provisioner          provision.Provisioner
	ServiceProvisioner   provision.ServiceProvisioner
	AllowUnauthenticated bool
	// ReconcileInterval <= 0 means controller.DefaultReconcileInterval.
	ReconcileInterval time.Duration
	// Admission is the administrator's image allowlist and worker cap
	// (requirement 7); zero value = unrestricted.
	Admission api.Admission
	// MeteringInterval is how often usage samples are recorded
	// (requirement 14); 0 = controller.DefaultMeteringInterval.
	MeteringInterval time.Duration
	// JobProvisioner backs ephemeral RayJobs (requirement 5); nil = jobs
	// are not provisioned (submit_job answers 501/502 accordingly).
	JobProvisioner provision.JobProvisioner
	// GatewayDomain is the DNS suffix dynamically registered clusters are
	// exposed under: `<name>.<GatewayDomain>` (plan ruling D1). "" disables
	// dynamic gateway registration.
	GatewayDomain string
	// GatewayExternalBase is the scheme (and optional host prefix/port)
	// clients reach the gateway through, e.g. "https://", used to build
	// the `gateway_url` views report. "" = no external URL reported.
	GatewayExternalBase string
	// ServicesPerProject caps concurrently deployed services per project
	// (plan ruling D8: one per project, 409 beyond); <= 0 means the
	// default of 1.
	ServicesPerProject int
}

// App is a wired control plane that has not yet opened a socket.
type App struct {
	Handler http.Handler
	Store   controller.Store
	cfg     Config
}

// New wires cfg into an App. It performs no I/O.
func New(cfg Config) (*App, error) {
	if cfg.Store == nil {
		return nil, errors.New("app: Config.Store is required")
	}
	if cfg.Registry == nil {
		cfg.Registry = &core.ClusterRegistry{}
	}
	if cfg.ServicesPerProject <= 0 {
		cfg.ServicesPerProject = 1
	}
	server := &api.Server{
		Store:               cfg.Store,
		Registry:            cfg.Registry,
		Validator:           cfg.Validator,
		Local:               cfg.Local,
		Provisioner:         cfg.Provisioner,
		ServiceProvisioner:  cfg.ServiceProvisioner,
		JobProvisioner:      cfg.JobProvisioner,
		Admission:           cfg.Admission,
		GatewayDomain:       cfg.GatewayDomain,
		GatewayExternalBase: cfg.GatewayExternalBase,
		ServicesPerProject:  cfg.ServicesPerProject,
	}
	handler := api.NewHandler(server, api.HandlerOptions{
		Validator:            cfg.Validator,
		Local:                cfg.Local,
		Registry:             cfg.Registry,
		Store:                cfg.Store,
		AllowUnauthenticated: cfg.AllowUnauthenticated,
	})
	return &App{Handler: handler, Store: cfg.Store, cfg: cfg}, nil
}

// RunLoops runs the reconcile loop (and the pool loop when the provisioner
// also provisions pools) until ctx is done. With no Provisioner it simply
// waits for ctx, so callers can always `go app.RunLoops(ctx)`.
func (a *App) RunLoops(ctx context.Context) {
	if a.cfg.Provisioner == nil {
		<-ctx.Done()
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		opts := controller.Options{
			Interval:         a.cfg.ReconcileInterval,
			MeteringInterval: a.cfg.MeteringInterval,
			JobProvisioner:   a.cfg.JobProvisioner,
		}
		if a.cfg.GatewayDomain != "" {
			domain := a.cfg.GatewayDomain
			opts.Registrar = a.cfg.Registry
			opts.GatewayHostname = func(id core.ClusterId) string { return string(id) + "." + domain }
		}
		if err := controller.RunReconciler(ctx, a.Store, a.cfg.Provisioner, opts); err != nil {
			slog.Error("reconcile loop exited", "error", err)
		}
	}()
	if pp, ok := a.cfg.Provisioner.(provision.PoolProvisioner); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := controller.RunPoolReconciler(ctx, a.Store, pp, controller.PoolOptions{
				Interval: a.cfg.ReconcileInterval,
			}); err != nil {
				slog.Error("pool reconcile loop exited", "error", err)
			}
		}()
	}
	wg.Wait()
}
