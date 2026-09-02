package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/brandonrc/bifrost/internal/api"
	"github.com/brandonrc/bifrost/internal/app"
	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision/live"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// serveOptions holds every --serve flag. Flag names/semantics are ported
// 1:1 from mobula-cli's Serve subcommand where mobula-cli had an
// equivalent (bind/registry/dev-allow-unauthenticated/
// allow-insecure-transport/reconcile-interval-secs); --store/--db replace
// mobula's implicit --db prefix-sniffing with an explicit backend choice
// (see store.go); --namespace replaces --kuberay-namespace (shorter,
// nothing else in this CLI's surface says "kuberay"); --ray-autoscaling is
// new (T15) — it exposes live.NewClient's autoscaling parameter (ADR-0007)
// that mobula-cli hardcoded to false at its one call site.
//
// Deliberately NOT ported this wave (see the task report): --demo (mock
// provisioner, no Wave 1 equivalent exists), --policy (governance
// prices/quotas file — Phase 4/Wave 3), --metering-interval-secs (the
// metering loop is an explicit Global-Constraints Wave 3 scope-out),
// --audit-log (a second JSONL sink alongside slog; the audit trail's
// durable form is already the Store via RecordAudit, T11/T12).
type serveOptions struct {
	Bind                    string
	Registry                string
	AuthConfig              string
	DevAllowUnauthenticated bool
	AllowInsecureTransport  bool
	StoreKind               string
	DB                      string
	Namespace               string
	ReconcileInterval       time.Duration
	Autoscaling             bool
	LocalAuth               bool
}

func newServeCmd() *cobra.Command {
	var opts serveOptions
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the control-plane API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.Bind, "bind", "127.0.0.1:8484", "Address to bind")
	f.StringVar(&opts.Registry, "registry", "", "JSON cluster registry for the job gateway (see registry.go for the format)")
	f.StringVar(&opts.AuthConfig, "auth-config", "", "JSON OIDC auth config (issuer, audience, role mappings). "+
		"When set, every request needs a valid Bearer JWT and non-loopback binds are permitted")
	f.BoolVar(&opts.DevAllowUnauthenticated, "dev-allow-unauthenticated", false,
		"DANGER: serve without authentication on a non-loopback address. Refused by default")
	f.BoolVar(&opts.AllowInsecureTransport, "allow-insecure-transport", false,
		"DANGER: permit auth tokens over cleartext http:// southbound (local dev only)")
	f.StringVar(&opts.StoreKind, "store", "memory", "Desired-state store backend: memory, sqlite, or postgres")
	f.StringVar(&opts.DB, "db", "", "Store DSN: a SQLite file path (--store sqlite) or a postgres:// URL (--store postgres)")
	f.StringVar(&opts.Namespace, "namespace", "",
		"Enable the cluster lifecycle controller: reconcile RayClusters in this Kubernetes namespace via KubeRay")
	f.DurationVar(&opts.ReconcileInterval, "reconcile-interval", controller.DefaultReconcileInterval, "Reconcile resync interval")
	f.BoolVar(&opts.Autoscaling, "ray-autoscaling", false,
		"New clusters default to KubeRay in-tree-autoscaler ownership of worker replicas (ADR-0007); "+
			"per-cluster Kueue-elastic pools always get it regardless of this flag")
	f.BoolVar(&opts.LocalAuth, "local-auth", false,
		"Enable local (IdP-free) username/password auth (ADR-0011); counts as configured authentication "+
			"for the fail-closed non-loopback rule")
	return cmd
}

// builtServer is buildServer's result: everything runServe needs to open
// a listener and start the reconcile loops, kept as a struct so the smoke
// test (serve_test.go) can call buildServer directly without binding a
// real socket.
type builtServer struct {
	app        *app.App
	closeStore func() error
	validator  *auth.Validator
	local      *auth.LocalAuthenticator
	// live is the KubeRay/Kueue client (T6), set only when --namespace
	// configures the lifecycle controller. It satisfies
	// provision.Provisioner, provision.PoolProvisioner, and (via
	// live.NewServiceClient) provision.ServiceProvisioner all at once —
	// the one live connection backs the reconciler, the pool reconciler,
	// and the Serve-service routes.
	live *live.Client
}

// buildServer resolves store -> registry -> auth -> (optional) live k8s
// client, then hands them to app.New to build the api.Server and handler,
// without opening any listener — the fail-closed bind guard
// (CheckBindAllowed) and the actual http.Server both live in runServe,
// which is the only caller that needs a real socket.
func buildServer(ctx context.Context, opts serveOptions) (*builtServer, error) {
	kind, err := parseStoreKind(opts.StoreKind)
	if err != nil {
		return nil, err
	}
	store, closeStore, err := openStore(ctx, kind, opts.DB)
	if err != nil {
		return nil, err
	}
	// From here on, every early-return must close what openStore opened.
	fail := func(err error) (*builtServer, error) {
		_ = closeStore()
		return nil, err
	}

	registry := &core.ClusterRegistry{}
	if opts.Registry != "" {
		reg, err := loadRegistry(opts.Registry, opts.AllowInsecureTransport)
		if err != nil {
			return fail(err)
		}
		registry = reg
	}
	for _, c := range registry.Clusters {
		slog.Info("cluster registered", "id", c.Id, "hostname", c.Hostname)
	}
	slog.Info("registry loaded", "clusters", len(registry.Clusters))

	var validator *auth.Validator
	if opts.AuthConfig != "" {
		cfg, err := loadAuthConfig(opts.AuthConfig)
		if err != nil {
			return fail(err)
		}
		slog.Info("OIDC discovery", "issuer", cfg.Issuer, "audience", cfg.Audience)
		v, err := auth.Discover(ctx, cfg, auth.IdpClient(), opts.AllowInsecureTransport)
		if err != nil {
			return fail(err)
		}
		validator = v
	}

	var localAuth *auth.LocalAuthenticator
	if opts.LocalAuth {
		var dbPath string
		if kind == storeSqlite {
			dbPath = opts.DB
		}
		if err := bootstrapLocalAdmin(ctx, store, dbPath); err != nil {
			return fail(err)
		}
		localAuth = auth.NewLocalAuthenticator(store, 86_400, 90)
		slog.Info("local auth enabled (ADR-0011): /api/v1/auth/login")
	}

	cfg := app.Config{
		Store:                store,
		Registry:             registry,
		Validator:            validator,
		Local:                localAuth,
		AllowUnauthenticated: opts.DevAllowUnauthenticated,
		ReconcileInterval:    opts.ReconcileInterval,
	}
	var liveClient *live.Client
	if opts.Namespace != "" {
		restCfg, err := ctrlconfig.GetConfig()
		if err != nil {
			return fail(fmt.Errorf("resolving kubeconfig: %w", err))
		}
		c, err := live.NewClient(restCfg, opts.Namespace, opts.Autoscaling)
		if err != nil {
			return fail(err)
		}
		liveClient = c
		cfg.Provisioner = c
		cfg.ServiceProvisioner = live.NewServiceClient(c)
		slog.Info("cluster lifecycle controller + services enabled", "namespace", opts.Namespace)
	}

	a, err := app.New(cfg)
	if err != nil {
		return fail(err)
	}
	return &builtServer{
		app:        a,
		closeStore: closeStore,
		validator:  validator,
		local:      localAuth,
		live:       liveClient,
	}, nil
}

// bindIPFor extracts the bind address's host IP for CheckBindAllowed.
// Unparseable/absent hosts (an empty host from ":8484", or a
// SplitHostPort failure) resolve to nil, which CheckBindAllowed treats as
// NOT provably loopback — fail closed, not "assume safe".
func bindIPFor(bind string) net.IP {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// runServe is serve's production entry point: build the handler, enforce
// the bind-time fail-closed guard (CheckBindAllowed — internal/api's
// guard, called correctly so its error aborts startup before any socket
// opens), start the reconcile + pool-reconcile loops when a live
// provisioner is configured (RunReconciler runs the ADR-0007-equivalent
// stale-restore boot check itself — see reconcile.go's doc comment; no
// separate wiring is needed here), then serve until SIGINT/SIGTERM.
func runServe(ctx context.Context, opts serveOptions) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	built, err := buildServer(ctx, opts)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := built.closeStore(); cerr != nil {
			slog.Warn("store close failed", "error", cerr)
		}
	}()

	authConfigured := built.validator != nil || built.local != nil
	if err := api.CheckBindAllowed(bindIPFor(opts.Bind), authConfigured, opts.DevAllowUnauthenticated); err != nil {
		return err
	}

	go built.app.RunLoops(ctx)

	srv := &http.Server{Addr: opts.Bind, Handler: built.app.Handler}
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("bifrost serve listening", "bind", opts.Bind)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-serveErr:
		return err
	}
}
