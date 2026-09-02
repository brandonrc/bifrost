# Requirement Test Framework — P0 Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up `bifrost/test/requirements` — the traceability primitives, an in-process target running the real app wiring, the generated contract tests, the report generators, the L2 CI job — and fix defects 1 (unvalidated `required`) and 4 (nginx startup DNS) with tests that prove it.

**Architecture:** Extract the server wiring from `cmd/bifrost/serve.go` into `internal/app.New` so production and the `inproc` test target construct the app through one function. Tests speak only the generated public client (`pkg/client`) and declare the requirement they cover with `req.Covers`; `reqreport` turns `go test -json` into `docs/requirements/traceability.md`. Request validation becomes a middleware driven by the embedded OpenAPI document, so `required` is enforced once for all 47 operations.

**Tech Stack:** Go 1.26, oapi-codegen v2 (`go tool oapi-codegen`), kin-openapi v0.142.0 (`openapi3`, `openapi3filter`, `routers/gorillamux`), Helm 3, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-09-02-requirement-test-framework-design.md` — this plan implements its §10 "P0 — Core". Read §1, §2, §4, §8 before starting.

## Global Constraints

- Module path is `github.com/brandonrc/bifrost`; Go `1.26.0`, toolchain `go1.26.6`. `CGO_ENABLED=0` for builds; tests run with `-race` (needs `CGO_ENABLED=1`).
- No file under `test/requirements/` except `test/requirements/target/inproc/` may import `github.com/brandonrc/bifrost/internal/...` (spec §1.3). The guard test in Task 7 enforces it; do not add exceptions.
- Every `Test*` function under `test/requirements/contract/`, `test/requirements/pack/` and `test/requirements/r??_*/` must call `req.Covers` or `req.NotYetBuilt` as its first statement after obtaining a target.
- No `time.Sleep` in test files under `test/requirements/` (use `req.Eventually`).
- Requirement numbers are 1–18. `req.Covers` panics on anything else.
- Coverage exclusions live only in `.coverage-exclude`; `covreport` prints the list on every run.
- `inproc` must call `app.New` — the same constructor `cmd/bifrost/serve.go` calls. Task 1's guard test enforces that `serve.go` no longer builds `api.Server`/`api.NewHandler` itself.
- Commit messages: no AI-attribution footers other than the two trailer lines the session mandates (`Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk`). Never `--no-verify`.
- The fake clock (`Target.Clock()`) is **deferred to P1** by decision: 70 call sites of `time.Now()`/`NowUnix()` span three packages and the tests that need it (idle timeout, usage) are P1 tests. In P0 `Clock()` returns `nil` on every target.
- `reqreport --allow-untested` is permitted in CI **only during P0**; the P1 plan removes the flag when all 18 rows have tests.
- Grace is a playground (user, 2026-09-02). Task 11 verifies on it directly: `ssh geraci@grace`, `export KUBECONFIG=/var/snap/microk8s/current/credentials/client.config`, Helm is `microk8s helm3`.

---

## File structure (what P0 creates or touches)

| Path | Responsibility |
|---|---|
| `internal/app/app.go` | `Config`, `App`, `New`, `RunLoops` — the single wiring function |
| `internal/app/app_test.go` | New() produces a handler that answers `/healthz` and `/api/v1/version` |
| `cmd/bifrost/serve.go` | Resolves inputs (store, OIDC, live client), then calls `app.New`; keeps flags + signals |
| `cmd/bifrost/serve_guard_test.go` | Asserts serve.go does not wire handlers itself |
| `internal/api/validate.go` | OpenAPI request-validation middleware (defect 1) |
| `internal/api/validate_test.go` | Missing `id` → 400; valid body reaches handler |
| `internal/api/gen.go` | adds `//go:embed openapi.json` |
| `internal/api/clusters.go` | domain guard: empty/invalid `ClusterId` → 400 |
| `pkg/client/gen.go`, `pkg/client/zz_generated_client.go` | Generated Go client |
| `pkg/client/client_test.go` | Client round-trips against an in-process handler |
| `test/requirements/req/requirements.yaml` | The 18 rows: number, title, priority |
| `test/requirements/req/req.go` | `Covers`, `NotYetBuilt`, `NeedsCapability`, `NeedK8s`, `Eventually`, `RunID` |
| `test/requirements/req/target.go` | `Target` interface, `FakeClock` |
| `test/requirements/req/reqline.go` | REQ line format, emit + parse (shared with reqreport) |
| `test/requirements/req/req_test.go` | NotYetBuilt inversion; Covers range panic; reqline round-trip |
| `test/requirements/target/inproc/fake_provisioner.go` | Converging fake with k8s-name validation |
| `test/requirements/target/inproc/inproc.go` | Builds the app, seeds principals, implements `Target` |
| `test/requirements/target/target.go` | `Get(t)` selects by `REQ_TARGET` |
| `test/requirements/target/inproc/inproc_test.go` | create → running; empty name refused |
| `test/requirements/contract/required_test.go` | every op × every required field omitted → 400 |
| `test/requirements/contract/unauth_test.go` | every non-public op, no token → 401 |
| `test/requirements/contract/spec.go` | loads the contract for the generated tests |
| `test/requirements/r17_slurm/provisioner_guard_test.go` | Provisioner signature carries no k8s types |
| `test/requirements/guards_test.go` | import / Covers / Sleep guards |
| `test/requirements/pack/nginx_test.go`, `pack/imagetag_test.go`, `pack/helm.go` | helm-template assertions |
| `test/requirements/cmd/reqreport/main.go` (+ `report.go`, `report_test.go`, `testdata/`) | traceability generator |
| `test/requirements/cmd/covreport/main.go` (+ `cov.go`, `cov_test.go`, `testdata/`) | tiered coverage + ratchet |
| `.coverage-tiers.yaml`, `.coverage-exclude`, `.coverage-ratchet.json` | coverage policy files |
| `docs/requirements/traceability.md`, `traceability.json` | committed report |
| `.github/workflows/ci.yml` | `requirements-l2` job; covreport replaces coverage-gate.sh |
| `Makefile` | `test-l2`, `report`, `ratchet` |
| `bifrost-pack/chart/templates/ui-configmap.yaml`, `ui.yaml` | defect 4 fix |

---

### Task 1: Extract `internal/app.New` from `serve.go`

**Files:**
- Create: `internal/app/app.go`
- Create: `internal/app/app_test.go`
- Modify: `cmd/bifrost/serve.go:86-197` (the `builtServer` type and `buildServer`), `cmd/bifrost/serve.go:220-244` (`runServe`'s loop start)
- Create: `cmd/bifrost/serve_guard_test.go`

**Interfaces:**
- Produces: `app.Config{Store controller.Store; Registry *core.ClusterRegistry; Validator *auth.Validator; Local *auth.LocalAuthenticator; Provisioner provision.Provisioner; ServiceProvisioner provision.ServiceProvisioner; AllowUnauthenticated bool; ReconcileInterval time.Duration}`; `app.New(cfg Config) (*App, error)`; `type App struct{ Handler http.Handler; Store controller.Store }`; `func (a *App) RunLoops(ctx context.Context)` (blocks until ctx done; no-op without Provisioner). Task 4 depends on exactly these names.

- [ ] **Step 1: Write the failing test for `app.New`**

`internal/app/app_test.go`:
```go
package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brandonrc/bifrost/internal/controller"
)

func TestNewServesHealthzAndVersion(t *testing.T) {
	a, err := New(Config{Store: controller.NewMemoryStore(), AllowUnauthenticated: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(a.Handler)
	defer srv.Close()
	for _, p := range []string{"/healthz", "/api/v1/version"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, resp.StatusCode)
		}
	}
}

func TestNewRequiresStore(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with nil Store returned no error")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/app/ 2>&1 | head -5`
Expected: build failure — `package github.com/brandonrc/bifrost/internal/app` has no non-test Go files / `undefined: New`.

- [ ] **Step 3: Write `internal/app/app.go`**

```go
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
	server := &api.Server{
		Store:              cfg.Store,
		Registry:           cfg.Registry,
		Validator:          cfg.Validator,
		Local:              cfg.Local,
		Provisioner:        cfg.Provisioner,
		ServiceProvisioner: cfg.ServiceProvisioner,
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
		if err := controller.RunReconciler(ctx, a.Store, a.cfg.Provisioner, controller.Options{
			Interval: a.cfg.ReconcileInterval,
		}); err != nil {
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
```

- [ ] **Step 4: Run the app tests**

Run: `go test ./internal/app/ -run 'TestNew' -v 2>&1 | tail -5`
Expected: both PASS.

- [ ] **Step 5: Write the serve.go guard test (fails until serve.go is refactored)**

`cmd/bifrost/serve_guard_test.go`:
```go
package main

import (
	"os"
	"strings"
	"testing"
)

// serve.go must resolve inputs and hand them to app.New; it must not build
// the api.Server or the handler itself. If it did, the inproc requirement
// target (which also calls app.New) would test different wiring than
// production runs — the exact gap the requirement framework exists to close.
func TestServeDelegatesWiringToAppNew(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, forbidden := range []string{"api.NewHandler(", "&api.Server{", "controller.RunReconciler(", "controller.RunPoolReconciler("} {
		if strings.Contains(s, forbidden) {
			t.Errorf("serve.go contains %q; that wiring belongs in internal/app", forbidden)
		}
	}
	if !strings.Contains(s, "app.New(") {
		t.Error("serve.go does not call app.New")
	}
}
```

- [ ] **Step 6: Run the guard to see it fail**

Run: `go test ./cmd/bifrost/ -run TestServeDelegatesWiringToAppNew 2>&1 | tail -6`
Expected: FAIL listing `api.NewHandler(`, `&api.Server{`, `controller.RunReconciler(`, `controller.RunPoolReconciler(` and "does not call app.New".

- [ ] **Step 7: Refactor `serve.go`**

Replace the `builtServer` type and the tail of `buildServer` (from `var liveClient *live.Client` through the closing `return`) and the loop-start block in `runServe`. Add `"github.com/brandonrc/bifrost/internal/app"` to imports and remove the now-unused `"github.com/brandonrc/bifrost/internal/api"` import **only if** `api.CheckBindAllowed` is no longer referenced — it is still used in `runServe`, so keep the `api` import.

New `builtServer`:
```go
type builtServer struct {
	app        *app.App
	closeStore func() error
	validator  *auth.Validator
	local      *auth.LocalAuthenticator
	live       *live.Client
}
```

Tail of `buildServer` (replaces everything from `var liveClient *live.Client` to the end of the function):
```go
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
```

In `runServe`, replace the `if built.live != nil { go func() {...RunReconciler...}(); go func() {...RunPoolReconciler...}() }` block with:
```go
	go built.app.RunLoops(ctx)
```
and `srv := &http.Server{Addr: opts.Bind, Handler: built.handler}` with `Handler: built.app.Handler`.

Update `serve_test.go` references: `built.handler` → `built.app.Handler`, `built.store` → `built.app.Store`. Remove the `controller` import from serve.go if it becomes unused (it is still used for `controller.DefaultReconcileInterval` in the flag default — keep it).

- [ ] **Step 8: Build and run the whole cmd + app test set**

Run: `go build ./... && go test ./cmd/bifrost/ ./internal/app/ 2>&1 | tail -4`
Expected: `ok  github.com/brandonrc/bifrost/cmd/bifrost` and `ok ... /internal/app`.

- [ ] **Step 9: Full suite still green**

Run: `CGO_ENABLED=1 go test -race ./... 2>&1 | grep -v "^ok" | head`
Expected: no output other than possibly `no test files` lines.

- [ ] **Step 10: Commit**

```bash
git add internal/app cmd/bifrost/serve.go cmd/bifrost/serve_test.go cmd/bifrost/serve_guard_test.go
git commit -m "refactor: extract the control-plane wiring into internal/app.New

serve.go now resolves inputs (store, OIDC discovery, live k8s client) and
hands them to app.New, which builds api.Server, the handler and the
reconcile loops. The requirement framework's in-process target will call
the same New, so a test passing there exercises production wiring rather
than a look-alike. A guard test keeps serve.go from growing wiring back.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

---

### Task 2: Generate the Go client `pkg/client`

**Files:**
- Create: `pkg/client/gen.go`
- Generate: `pkg/client/zz_generated_client.go`
- Create: `pkg/client/client_test.go`
- Modify: `.github/workflows/ci.yml:36-39` (codegen drift step)

**Interfaces:**
- Produces: package `github.com/brandonrc/bifrost/pkg/client` with `NewClientWithResponses(server string, opts ...ClientOption) (*ClientWithResponses, error)`, `WithRequestEditorFn(fn RequestEditorFn) ClientOption`, and per-operation methods named from operationIds in CamelCase with `WithResponse` suffix: `VersionWithResponse`, `HealthzWithResponse`, `LoginWithResponse`, `CreateClusterWithResponse`, `ListClustersWithResponse`, `GetClusterWithResponse`, `DeleteClusterWithResponse`, `UpsertAssignmentWithResponse`, and body aliases `LoginJSONRequestBody`, `CreateClusterJSONRequestBody`, `UpsertAssignmentJSONRequestBody`. Tasks 4 and 6 use these names.

- [ ] **Step 1: Write the failing test**

`pkg/client/client_test.go`:
```go
package client_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/brandonrc/bifrost/internal/api/apitest"
	"github.com/brandonrc/bifrost/pkg/client"
)

func TestClientRoundTripsVersion(t *testing.T) {
	h, _ := apitest.NewServer()
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.VersionWithResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("version: %d %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON200 == nil {
		t.Fatal("version: JSON200 nil — client did not decode the body")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/client/ 2>&1 | head -3`
Expected: `no non-test Go files` / undefined `client`.

- [ ] **Step 3: Write `pkg/client/gen.go` and generate**

```go
// Package client is the Go client for the Bifrost REST API, generated from
// the same vendored contract (internal/api/openapi.json) the server is
// generated from, so the two cannot disagree without CI's codegen-drift step
// noticing. It is public: requirement tests use it in place of any internal
// type, exactly as an external consumer would.
package client

//go:generate go tool oapi-codegen -generate client,types -package client -o zz_generated_client.go ../../internal/api/openapi.json
```

Run: `go generate ./pkg/client/ && go build ./pkg/client/ && head -3 pkg/client/zz_generated_client.go`
Expected: file generated, builds, header says `// Package client provides primitives to interact with the openapi HTTP API.`

- [ ] **Step 4: Run the client test**

Run: `go test ./pkg/client/ -v 2>&1 | tail -3`
Expected: `--- PASS: TestClientRoundTripsVersion`.

If the depguard lint rule denies `apitest` from a `_test.go` file: it does not — the rule denies non-test files only. Confirm with `make lint`.

- [ ] **Step 5: Extend the CI drift step**

In `.github/workflows/ci.yml`, change the "api codegen drift" step to:
```yaml
      - name: api + client codegen drift (ADR-0002 spec-first contract)
        run: |
          go generate ./internal/api/... ./pkg/client/...
          git diff --exit-code -- internal/api/zz_generated_api.go pkg/client/zz_generated_client.go
```

- [ ] **Step 6: Lint**

Run: `make lint 2>&1 | tail -5`
Expected: no findings. If `zz_generated_client.go` triggers lint, add to `.golangci.yml` under `formatters`/`linters` exclusions: `paths: ["pkg/client/zz_generated_client.go"]` — mirror however `zz_generated_api.go` is already excluded (grep `.golangci.yml` for `zz_generated`).

- [ ] **Step 7: Commit**

```bash
git add pkg/client .github/workflows/ci.yml .golangci.yml
git commit -m "feat(client): generate the Go client from the vendored contract

Requirement tests will speak the public contract only, so they need a
client that is generated from the same openapi.json as the server. The
codegen-drift CI step now covers both outputs.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

---

### Task 3: The `req` package — `Covers`, `NotYetBuilt`, REQ lines, `Target`

**Files:**
- Create: `test/requirements/req/requirements.yaml`
- Create: `test/requirements/req/reqline.go`
- Create: `test/requirements/req/req.go`
- Create: `test/requirements/req/target.go`
- Create: `test/requirements/req/req_test.go`

**Interfaces:**
- Produces: `req.Covers(t testing.TB, n int, reason string)`; `req.NotYetBuilt(t *testing.T, n int, reason string, body func(t *testing.T))`; `req.NeedsCapability(t testing.TB, tgt Target, cap string)`; `req.NeedK8s(t testing.TB, tgt Target)`; `req.Eventually(t testing.TB, tgt Target, cond func() (bool, string))`; `req.RunID() string`; `req.Target` interface; `req.ParseLine(s string) (Line, bool)`; `req.Line{Kind string; Req int; Reason string; Outcome string}`; `req.Requirements() []Requirement` from the yaml; `req.EventuallyTimeout(tgt Target) time.Duration`.
- Consumes: `pkg/client.ClientWithResponses` (Task 2); `sigs.k8s.io/controller-runtime/pkg/client.Client` (already a dependency).

- [ ] **Step 1: Write `requirements.yaml`** (the 18 rows, titles verbatim from `docs/SPEC.md`)

```yaml
# The 18 rows of the Ray Software Pack requirement table (docs/SPEC.md).
# req.Covers validates against this; reqreport renders from it. One source.
- {n: 1,  priority: CRITICAL, title: "Deploy models from within Jupyter"}
- {n: 2,  priority: HIGH,     title: "Groups share models privately"}
- {n: 3,  priority: HIGH,     title: "RBAC for model serving and cluster access; direct Ray Serve/dashboard/Jobs/GCS access blocked"}
- {n: 4,  priority: CRITICAL, title: "Serving in separate resource pool from compute clusters"}
- {n: 5,  priority: CRITICAL, title: "UI runs jobs via ephemeral RayJob"}
- {n: 6,  priority: CRITICAL, title: "Self-serve private clusters (dask-gateway UX)"}
- {n: 7,  priority: CRITICAL, title: "Group admins control profiles, images, CPU/mem/GPU, max workers"}
- {n: 8,  priority: CRITICAL, title: "Automatic cleanup even after gateway failure; ownership recorded; state recovered on restart"}
- {n: 9,  priority: CRITICAL, title: "Start/stop clusters from JupyterLab (extension)"}
- {n: 10, priority: CRITICAL, title: "Use nebi environments on the cluster"}
- {n: 11, priority: CRITICAL, title: "Pass environment variables to the cluster (JupyterLab extension)"}
- {n: 12, priority: CRITICAL, title: "Private storage (e.g. S3) from the cluster"}
- {n: 13, priority: LOW,      title: "Group capacity via shared resource pools; fair queueing; admin quotas/weights"}
- {n: 14, priority: LOW,      title: "Usage visibility: who requested what, duration, estimated cost"}
- {n: 15, priority: LOW,      title: "Cluster health / pending-reasons without direct K8s access"}
- {n: 16, priority: LOW,      title: "Same UX across Ray and Dask"}
- {n: 17, priority: LOW,      title: "Same UX across Kubernetes and Slurm (design must not foreclose)"}
- {n: 18, priority: LOW,      title: "NIST security baseline operation + audit evidence"}
```

- [ ] **Step 2: Write the failing tests**

`test/requirements/req/req_test.go`:
```go
package req

import (
	"strings"
	"testing"
)

func TestRequirementsHasEighteenRows(t *testing.T) {
	rs := Requirements()
	if len(rs) != 18 {
		t.Fatalf("got %d requirements, want 18", len(rs))
	}
	for i, r := range rs {
		if r.N != i+1 {
			t.Errorf("row %d has n=%d; rows must be 1..18 in order", i, r.N)
		}
		if r.Title == "" || r.Priority == "" {
			t.Errorf("row %d missing title or priority", r.N)
		}
	}
}

func TestCoversPanicsOutOfRange(t *testing.T) {
	for _, n := range []int{0, 19, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Covers(%d) did not panic", n)
				}
			}()
			Covers(t, n, "x")
		}()
	}
}

func TestReqLineRoundTrip(t *testing.T) {
	cases := []Line{
		{Kind: "covers", Req: 6, Reason: "creates a cluster"},
		{Kind: "notyetbuilt", Req: 5, Reason: "ephemeral RayJob", Outcome: "failed"},
		{Kind: "skip", Req: 0, Reason: "needs k8s"},
	}
	for _, c := range cases {
		got, ok := ParseLine(c.Format())
		if !ok || got != c {
			t.Errorf("round trip %+v -> %+v ok=%v", c, got, ok)
		}
	}
	if _, ok := ParseLine("some unrelated log line"); ok {
		t.Error("ParseLine accepted a non-REQ line")
	}
}

// NotYetBuilt must invert: a failing body is the expected state and passes;
// a passing body means the requirement appears built and must FAIL.
func TestNotYetBuiltInverts(t *testing.T) {
	// Use subtests as the harness: run NotYetBuilt inside a subtest and
	// inspect that subtest's outcome.
	passed := t.Run("failing-body-is-ok", func(t *testing.T) {
		NotYetBuilt(t, 5, "not built yet", func(t *testing.T) {
			t.Error("the feature is missing")
		})
	})
	if !passed {
		t.Error("NotYetBuilt with a failing body should PASS the outer test")
	}
	passed = t.Run("passing-body-must-fail", func(t *testing.T) {
		// We cannot let this subtest actually fail the suite, so run
		// NotYetBuilt inside a nested subtest and assert on its result.
		inner := t.Run("nested", func(t *testing.T) {
			NotYetBuilt(t, 5, "not built yet", func(t *testing.T) {})
		})
		if inner {
			t.Error("NotYetBuilt with a passing body should FAIL")
		}
	})
	if !passed {
		t.Error("harness subtest itself failed")
	}
}

func TestNotYetBuiltPassingBodyMessage(t *testing.T) {
	var got string
	rec := &recorder{T: t, onError: func(s string) { got = s }}
	notYetBuiltImpl(rec, 5, "r", func() bool { return true })
	if !strings.Contains(got, "requirement 5 appears built") || !strings.Contains(got, "remove the NotYetBuilt marker") {
		t.Errorf("message = %q", got)
	}
}
```

And the tiny recorder used above, appended to the same test file:
```go
// recorder captures Errorf output for message assertions.
type recorder struct {
	*testing.T
	onError func(string)
}

func (r *recorder) Errorf(format string, args ...any) { r.onError(fmt.Sprintf(format, args...)) }
func (r *recorder) Logf(string, ...any)               {}
```
(Add `"fmt"` to the test file's imports.)

- [ ] **Step 3: Run to verify failure**

Run: `go test ./test/requirements/req/ 2>&1 | head -5`
Expected: undefined `Requirements`, `Covers`, `Line`, `ParseLine`, `NotYetBuilt`, `notYetBuiltImpl`.

- [ ] **Step 4: Write `reqline.go`**

```go
package req

import (
	"fmt"
	"regexp"
	"strconv"
)

// Line is one REQ log line. It is the whole contract between tests and
// reqreport: tests emit it through t.Log, `go test -json` carries it
// verbatim in "output" events, reqreport parses it. Nothing else is shared.
//
// Format:  REQ: kind=<covers|notyetbuilt|skip> req=<n> reason=<quoted> [outcome=<failed|passed>]
type Line struct {
	Kind    string
	Req     int
	Reason  string
	Outcome string
}

const linePrefix = "REQ: "

var lineRe = regexp.MustCompile(`^\s*REQ: kind=(\w+) req=(\d+) reason=("(?:[^"\\]|\\.)*")(?: outcome=(\w+))?\s*$`)

// Format renders the line. Reason is %q-quoted so it may contain spaces.
func (l Line) Format() string {
	s := fmt.Sprintf("%skind=%s req=%d reason=%q", linePrefix, l.Kind, l.Req, l.Reason)
	if l.Outcome != "" {
		s += " outcome=" + l.Outcome
	}
	return s
}

// ParseLine parses one log line; ok=false for anything that is not a REQ line.
func ParseLine(s string) (Line, bool) {
	m := lineRe.FindStringSubmatch(s)
	if m == nil {
		return Line{}, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return Line{}, false
	}
	reason, err := strconv.Unquote(m[3])
	if err != nil {
		return Line{}, false
	}
	return Line{Kind: m[1], Req: n, Reason: reason, Outcome: m[4]}, true
}
```

- [ ] **Step 5: Write `req.go`**

```go
// Package req is the requirement-test framework's vocabulary: which
// requirement a test proves (Covers), which it is waiting on (NotYetBuilt),
// what a target must have for it to run (NeedsCapability), and the Target
// seam every test speaks through. It imports nothing from internal/.
package req

import (
	_ "embed"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed requirements.yaml
var requirementsYAML []byte

// Requirement is one row of the 18-row table.
type Requirement struct {
	N        int    `yaml:"n"`
	Priority string `yaml:"priority"`
	Title    string `yaml:"title"`
}

var (
	reqsOnce sync.Once
	reqs     []Requirement
)

// Requirements returns the 18 rows, in order.
func Requirements() []Requirement {
	reqsOnce.Do(func() {
		if err := yaml.Unmarshal(requirementsYAML, &reqs); err != nil {
			panic("req: requirements.yaml: " + err.Error())
		}
	})
	return reqs
}

func mustValid(n int) {
	if n < 1 || n > len(Requirements()) {
		panic(fmt.Sprintf("req: requirement %d is out of range 1..%d", n, len(Requirements())))
	}
}

// Covers declares that the calling test proves requirement n. Call it first.
// A test may Covers two requirements when one scenario genuinely proves
// both; give each its own reason.
func Covers(t testing.TB, n int, reason string) {
	t.Helper()
	mustValid(n)
	t.Log(Line{Kind: "covers", Req: n, Reason: reason}.Format())
}

// NotYetBuilt runs body as a subtest and INVERTS its result: a failing body
// is the expected state for an unbuilt requirement and the outer test
// passes; a passing body means the requirement appears built, and the outer
// test fails until a human removes the marker in the same PR that built it.
func NotYetBuilt(t *testing.T, n int, reason string, body func(t *testing.T)) {
	t.Helper()
	mustValid(n)
	bodyPassed := t.Run("not-yet-built", func(st *testing.T) {
		body(st)
	})
	notYetBuiltImpl(t, n, reason, func() bool { return bodyPassed })
}

type errorLogger interface {
	Errorf(string, ...any)
	Logf(string, ...any)
}

func notYetBuiltImpl(t errorLogger, n int, reason string, bodyPassed func() bool) {
	if bodyPassed() {
		t.Logf("%s", Line{Kind: "notyetbuilt", Req: n, Reason: reason, Outcome: "passed"}.Format())
		t.Errorf("requirement %d appears built: the NotYetBuilt body passed. Remove the NotYetBuilt marker in the same PR that made this pass (reason was: %s)", n, reason)
		return
	}
	t.Logf("%s", Line{Kind: "notyetbuilt", Req: n, Reason: reason, Outcome: "failed"}.Format())
}

// NeedsCapability skips unless the target declares cap. The skip is
// recorded as a REQ line so the report can say WHY a column is partial.
func NeedsCapability(t testing.TB, tgt Target, cap string) {
	t.Helper()
	if !tgt.Has(cap) {
		reason := "target " + tgt.Name() + " lacks capability " + cap
		t.Log(Line{Kind: "skip", Req: 0, Reason: reason}.Format())
		t.Skip(reason)
	}
}

// NeedK8s skips on targets without Kubernetes (inproc).
func NeedK8s(t testing.TB, tgt Target) {
	t.Helper()
	if _, ok := tgt.K8s(); !ok {
		reason := "target " + tgt.Name() + " has no Kubernetes"
		t.Log(Line{Kind: "skip", Req: 0, Reason: reason}.Format())
		t.Skip(reason)
	}
}

// EventuallyTimeout is the per-lane convergence budget (spec §8).
func EventuallyTimeout(tgt Target) time.Duration {
	if _, ok := tgt.K8s(); ok {
		return 5 * time.Minute
	}
	return 5 * time.Second
}

// Eventually polls cond until it reports true or the lane timeout elapses.
// cond returns a human-readable state for the failure message. This is the
// only sanctioned way to wait; time.Sleep is forbidden under test/requirements.
func Eventually(t testing.TB, tgt Target, cond func() (ok bool, state string)) {
	t.Helper()
	deadline := time.Now().Add(EventuallyTimeout(tgt))
	var last string
	for time.Now().Before(deadline) {
		ok, state := cond()
		if ok {
			return
		}
		last = state
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s; last state: %s", EventuallyTimeout(tgt), last)
}

var (
	runIDOnce sync.Once
	runID     string
)

// RunID is a short per-process id used as the cluster-name prefix so every
// object a run creates can be found and removed (spec §1.4).
func RunID() string {
	runIDOnce.Do(func() {
		if v := os.Getenv("REQ_RUN_ID"); v != "" {
			runID = v
			return
		}
		runID = fmt.Sprintf("t%x", time.Now().UnixNano()%0xffffff)
	})
	return runID
}

// Name returns a cluster id carrying the run prefix: "t<run>-<short>".
func Name(short string) string { return RunID() + "-" + short }
```

- [ ] **Step 6: Write `target.go`**

```go
package req

import (
	"context"
	"net/http"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/pkg/client"
)

// FakeClock is controllable time. nil on every target in P0 (spec's
// Global Constraints: deferred to P1).
type FakeClock interface {
	Advance(d time.Duration)
	Now() time.Time
}

// Target is the seam every requirement test speaks through. Two
// implementations: inproc (L2) and cluster (L3, P2). Tests never learn which.
type Target interface {
	// Name is the REQ_TARGET value: inproc, kind, grace.
	Name() string
	// API is a client authenticated as the current principal.
	API() *client.ClientWithResponses
	// As returns a Target bound to another seeded principal:
	// admin, operator, dev-a (project team-a), dev-b (project team-b), anon.
	As(principal string) Target
	// K8s is a controller-runtime client scoped to Namespace(); ok=false on inproc.
	K8s() (c ctrlclient.Client, ok bool)
	Namespace() string
	Clock() FakeClock
	// Has reports a declared capability: keycloak, artifact-keeper, gateway, calico, consumers.
	Has(capability string) bool
	// BaseURL is the API's root (no trailing slash), for raw-HTTP tests such
	// as the generated contract tests that must send malformed bodies the
	// typed client cannot express.
	BaseURL() string
	// Authorize sets the current principal's bearer header on r (no-op for anon).
	Authorize(r *http.Request)
	// Cleanup deletes every cluster whose id carries RunID(). Registered by
	// target.Get via t.Cleanup; exposed so a test can force it early.
	Cleanup(ctx context.Context) error
}
```

- [ ] **Step 7: Add yaml dependency and run**

Run: `go get gopkg.in/yaml.v3@v3.0.1 && go mod tidy && go test ./test/requirements/req/ -v 2>&1 | tail -12`
Expected: all five tests PASS. If `TestNotYetBuiltInverts` reports the nested subtest passed, `NotYetBuilt` is not calling `t.Errorf` on the outer `t` — check that `notYetBuiltImpl` receives the outer `t`, not `st`.

- [ ] **Step 8: Commit**

```bash
git add test/requirements/req go.mod go.sum
git commit -m "feat(req): the requirement-test vocabulary — Covers, NotYetBuilt, REQ lines, Target

Covers logs a REQ line that go test -json carries verbatim; reqreport will
parse it. NotYetBuilt inverts a subtest so unbuilt requirements ship with
tests now and a passing one fails loudly until a human removes the marker.
The Target seam imports nothing from internal/.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

---

### Task 4: The `inproc` target and `target.Get`

**Files:**
- Create: `test/requirements/target/inproc/fake_provisioner.go`
- Create: `test/requirements/target/inproc/inproc.go`
- Create: `test/requirements/target/target.go`
- Create: `test/requirements/target/inproc/inproc_test.go`

**Interfaces:**
- Consumes: `app.New`, `app.Config`, `App.RunLoops` (Task 1); `client.NewClientWithResponses`, `client.WithRequestEditorFn`, `LoginWithResponse`, `CreateClusterWithResponse`, `GetClusterWithResponse`, `ListClustersWithResponse`, `DeleteClusterWithResponse`, `UpsertAssignmentWithResponse` (Task 2); `req.Target`, `req.RunID`, `req.Name` (Task 3).
- Produces: `target.Get(t testing.TB) req.Target` — selects by `REQ_TARGET` (default `inproc`); registers cleanup. `inproc.New(t testing.TB) req.Target`.

- [ ] **Step 1: Write the failing test**

`test/requirements/target/inproc/inproc_test.go`:
```go
package inproc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestInprocCreateConvergesToRunning(t *testing.T) {
	tgt := target.Get(t)
	ctx := context.Background()
	id := req.Name("smoke")

	body := client.CreateClusterJSONRequestBody{}
	json.Unmarshal([]byte(`{"id":"`+id+`","spec":{"name":"`+id+`","project":"team-a","image":"rayproject/ray:2.56.0","ray_version":"2.56.0","head_cpu":"1","head_memory":"1Gi","worker_groups":[{"name":"w","cpu":"1","memory":"1Gi","replicas":1,"min_replicas":1,"max_replicas":1}]}}`), &body)

	resp, err := tgt.As("admin").API().CreateClusterWithResponse(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode(), resp.Body)
	}

	req.Eventually(t, tgt, func() (bool, string) {
		g, err := tgt.As("admin").API().GetClusterWithResponse(ctx, id)
		if err != nil || g.StatusCode() != 200 {
			return false, "get failed"
		}
		var m map[string]any
		json.Unmarshal(g.Body, &m)
		st, _ := m["observed_state"].(string)
		return st == "running", "observed_state=" + st
	})
}

func TestInprocPrincipalsAreDistinct(t *testing.T) {
	tgt := target.Get(t)
	ctx := context.Background()
	a, _ := tgt.As("dev-a").API().ListClustersWithResponse(ctx, nil)
	if a.StatusCode() != 200 {
		t.Fatalf("dev-a list = %d %s", a.StatusCode(), a.Body)
	}
	anon, _ := tgt.As("anon").API().ListClustersWithResponse(ctx, nil)
	if anon.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("anon list = %d, want 401", anon.StatusCode())
	}
}
```
If `ListClustersWithResponse` takes no params argument in the generated client (no query parameters on that operation), drop the `nil`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./test/requirements/target/... 2>&1 | head -3`
Expected: undefined package `target` / `inproc`.

- [ ] **Step 3: Write `fake_provisioner.go`**

```go
package inproc

import (
	"context"
	"sync"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// fakeProvisioner is the Kubernetes edge, faked. It converges one step per
// Observe (provisioning -> running) and validates names the way a real API
// server would, so the empty-id record that wedged Grace on 2026-09-02
// fails here on the first reconcile tick.
type fakeProvisioner struct {
	provision.BaseProvisioner
	mu       sync.Mutex
	clusters map[core.ClusterId]*fakeCluster
}

type fakeCluster struct {
	generation uint64
	observes   int
	suspended  bool
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{clusters: map[core.ClusterId]*fakeCluster{}}
}

var _ provision.Provisioner = (*fakeProvisioner)(nil)

func (p *fakeProvisioner) Apply(_ context.Context, id core.ClusterId, _ *core.ClusterSpec, generation uint64, _ string, _ *provision.QueueAssignment) (provision.ApplyResponse, error) {
	if !core.IsK8sName(string(id)) {
		return provision.ApplyResponse{}, provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "resource name may not be empty or invalid: " + string(id)}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clusters[id]
	if !ok {
		c = &fakeCluster{}
		p.clusters[id] = c
	}
	c.generation = generation
	url := "http://" + string(id) + "-head-svc:8265"
	return provision.ApplyResponse{Generation: generation, ApiBaseUrl: &url}, nil
}

func (p *fakeProvisioner) Observe(_ context.Context, id core.ClusterId) (provision.ObservedCluster, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clusters[id]
	if !ok {
		return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
	}
	c.observes++
	state := core.ClusterStateRunning
	if c.observes == 1 {
		state = core.ClusterStateProvisioning
	}
	if c.suspended {
		state = core.ClusterStateSuspended
	}
	gen := c.generation
	url := "http://" + string(id) + "-head-svc:8265"
	return provision.ObservedCluster{ID: id, State: state, ObservedGeneration: &gen, ApiBaseUrl: &url}, nil
}

func (p *fakeProvisioner) List(_ context.Context) ([]provision.ObservedCluster, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provision.ObservedCluster, 0, len(p.clusters))
	for id, c := range p.clusters {
		gen := c.generation
		out = append(out, provision.ObservedCluster{ID: id, State: core.ClusterStateRunning, ObservedGeneration: &gen})
	}
	return out, nil
}

func (p *fakeProvisioner) Terminate(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.clusters, id)
	return nil
}

func (p *fakeProvisioner) Suspend(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clusters[id]; ok {
		c.suspended = true
	}
	return nil
}

func (p *fakeProvisioner) Resume(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clusters[id]; ok {
		c.suspended = false
	}
	return nil
}

func (p *fakeProvisioner) DashboardApiBase(id core.ClusterId) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.clusters[id]; !ok {
		return "", false
	}
	return "http://" + string(id) + "-head-svc:8265", true
}

func (p *fakeProvisioner) ClusterNodes(context.Context, core.ClusterId) (*core.ClusterNodes, error) {
	return &core.ClusterNodes{}, nil
}
func (p *fakeProvisioner) ClusterEvents(context.Context, core.ClusterId) (*core.ClusterEvents, error) {
	return &core.ClusterEvents{}, nil
}
func (p *fakeProvisioner) ClusterLogs(context.Context, core.ClusterId, *string, uint32) (*core.ClusterLogs, error) {
	return &core.ClusterLogs{}, nil
}
```
Check the exact state constant names with `grep -n "ClusterState[A-Z][a-zA-Z]* *ClusterState" internal/core/cluster.go` and adjust `ClusterStateRunning`/`Provisioning`/`Suspended` to match. Check `core.ClusterNodes`/`ClusterEvents`/`ClusterLogs` are struct types (`grep -n "^type Cluster\(Nodes\|Events\|Logs\) struct" internal/core/*.go`); if any is a slice type, return an empty value of that type instead.

- [ ] **Step 4: Write `inproc.go`**

```go
// Package inproc is the L2 target: the real control plane — app.New, real
// API server, real auth, real reconciler, real (memory) store — with only
// the Kubernetes edge faked. It is the ONE package under test/requirements
// allowed to import internal/ (guards_test.go enforces this).
package inproc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/internal/app"
	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/req"
)

type principal struct {
	role    core.LocalRole
	project string // "" = no project assignment
}

// Seeded principals, identical on every target (spec §1.1).
var principals = map[string]principal{
	"admin":    {role: core.LocalRoleAdmin},
	"operator": {role: core.LocalRoleOperator},
	"dev-a":    {role: core.LocalRoleDeveloper, project: "team-a"},
	"dev-b":    {role: core.LocalRoleDeveloper, project: "team-b"},
}

func password(name string) string { return "pw-" + name + "-0123456789" }

type target struct {
	srv       *httptest.Server
	store     controller.Store
	principal string
	tokens    *sync.Map // principal -> bearer token
	cancel    context.CancelFunc
}

// New builds the in-process target and starts its reconcile loop. Callers
// (target.Get) register Cleanup and srv.Close via t.Cleanup.
func New(t testing.TB) req.Target {
	t.Helper()
	store := controller.NewMemoryStore()
	ctx := context.Background()
	for name, p := range principals {
		hash, err := auth.HashPassword(password(name))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateLocalUser(ctx, name, nil, hash, p.role); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	a, err := app.New(app.Config{
		Store:             store,
		Local:             auth.NewLocalAuthenticator(store, 86_400, 90),
		Provisioner:       newFakeProvisioner(),
		ReconcileInterval: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	go a.RunLoops(loopCtx)
	srv := httptest.NewServer(a.Handler)

	tg := &target{srv: srv, store: store, principal: "admin", tokens: &sync.Map{}, cancel: cancel}
	// Project-scoped assignments are made through the API as admin, so the
	// seeding itself exercises the real access path.
	for name, p := range principals {
		if p.project == "" {
			continue
		}
		body := client.UpsertAssignmentJSONRequestBody{}
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"role":"developer","scope":"project:%s"}`, p.project)), &body)
		resp, err := tg.API().UpsertAssignmentWithResponse(ctx, name, body)
		if err != nil || resp.StatusCode()/100 != 2 {
			t.Fatalf("assign %s: err=%v status=%v body=%s", name, err, statusOf(resp), bodyOf(resp))
		}
	}
	t.Cleanup(func() {
		_ = tg.Cleanup(context.Background())
		cancel()
		srv.Close()
	})
	return tg
}

func statusOf(r *client.UpsertAssignmentResponse) any {
	if r == nil {
		return nil
	}
	return r.StatusCode()
}
func bodyOf(r *client.UpsertAssignmentResponse) string {
	if r == nil {
		return ""
	}
	return string(r.Body)
}

func (tg *target) Name() string      { return "inproc" }
func (tg *target) Namespace() string { return "inproc" }
func (tg *target) Clock() req.FakeClock {
	return nil // deferred to P1 (plan Global Constraints)
}
func (tg *target) K8s() (ctrlclient.Client, bool) { return nil, false }
func (tg *target) Has(capability string) bool     { return false }
func (tg *target) BaseURL() string                { return tg.srv.URL }
func (tg *target) Authorize(r *http.Request) {
	if tok := tg.token(context.Background()); tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
}

func (tg *target) As(p string) req.Target {
	if _, ok := principals[p]; !ok && p != "anon" {
		panic("inproc: unknown principal " + p)
	}
	cp := *tg
	cp.principal = p
	return &cp
}

func (tg *target) token(ctx context.Context) string {
	if tg.principal == "anon" {
		return ""
	}
	if v, ok := tg.tokens.Load(tg.principal); ok {
		return v.(string)
	}
	c, _ := client.NewClientWithResponses(tg.srv.URL)
	body := client.LoginJSONRequestBody{}
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q}`, tg.principal, password(tg.principal))), &body)
	resp, err := c.LoginWithResponse(ctx, body)
	if err != nil || resp.StatusCode() != http.StatusOK {
		panic(fmt.Sprintf("inproc: login %s failed: %v %s", tg.principal, err, bodyStr(resp)))
	}
	var m struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(resp.Body, &m)
	tg.tokens.Store(tg.principal, m.Token)
	return m.Token
}

func bodyStr(r *client.LoginResponse) string {
	if r == nil {
		return ""
	}
	return string(r.Body)
}

func (tg *target) API() *client.ClientWithResponses {
	tok := tg.token(context.Background())
	c, err := client.NewClientWithResponses(tg.srv.URL, client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return nil
	}))
	if err != nil {
		panic(err)
	}
	return c
}

// Cleanup deletes every cluster whose id carries the run prefix, as admin.
func (tg *target) Cleanup(ctx context.Context) error {
	api := tg.As("admin").API()
	list, err := api.ListClustersWithResponse(ctx)
	if err != nil {
		return err
	}
	var items []struct {
		Id string `json:"id"`
	}
	_ = json.Unmarshal(list.Body, &items)
	prefix := req.RunID() + "-"
	for _, it := range items {
		if strings.HasPrefix(it.Id, prefix) {
			if _, err := api.DeleteClusterWithResponse(ctx, it.Id); err != nil {
				return err
			}
		}
	}
	return nil
}
```
If the generated `ListClustersWithResponse` has a params argument, pass `nil`. If `UpsertAssignmentWithResponse`'s principal parameter type is not `string`, convert accordingly (check `grep -n "func (c \*ClientWithResponses) UpsertAssignmentWithResponse" pkg/client/zz_generated_client.go`).

- [ ] **Step 5: Write `target/target.go`**

```go
// Package target selects the req.Target for this run from REQ_TARGET:
// inproc (default) in P0; kind and grace arrive with the cluster target in P2.
package target

import (
	"os"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target/inproc"
)

// Get returns the run's target. A fresh inproc target per test keeps tests
// independent; cluster targets (P2) are shared per process by design.
func Get(t testing.TB) req.Target {
	t.Helper()
	switch v := os.Getenv("REQ_TARGET"); v {
	case "", "inproc":
		return inproc.New(t)
	default:
		t.Fatalf("REQ_TARGET=%q is not available in this build (P0 ships inproc only)", v)
		return nil
	}
}
```

- [ ] **Step 6: Run**

Run: `CGO_ENABLED=1 go test -race ./test/requirements/target/... -v 2>&1 | tail -8`
Expected: both PASS. Common failures and their meaning:
- 403 on create as admin → the local admin role does not grant `Write` on `TargetCluster`; check `auth` role grants and use `operator` for creates if that is the design (then also update the test).
- `observed_state` never `running` → the reconcile loop is not running: confirm `go a.RunLoops(loopCtx)` and that `Config.Provisioner` is non-nil.
- 400 on assignment → the scope grammar differs; check `UpsertAssignment` schema description: `"*"` or `"project:<name>"`.

- [ ] **Step 7: Commit**

```bash
git add test/requirements/target
git commit -m "feat(target): in-process requirement target on the real app wiring

inproc calls app.New with the memory store, local auth and a fake
Kubernetes edge that converges one step per observe and refuses invalid
names. Principals admin/operator/dev-a/dev-b/anon are seeded identically
to what the cluster target will seed. target.Get selects by REQ_TARGET.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

---

### Task 5: Fix defect 1 — request validation middleware + domain guard

**Files:**
- Modify: `internal/api/gen.go` (add embed)
- Create: `internal/api/validate.go`
- Create: `internal/api/validate_test.go`
- Modify: `internal/api/server.go:199` (wrap the contract routes)
- Modify: `internal/api/clusters.go:340` (domain guard on `body.Id`)

**Interfaces:**
- Produces: `api.ContractDocument() *openapi3.T` (loaded once from the embedded spec, `Servers` cleared); `api.ValidateRequests(next http.Handler) http.Handler`.

- [ ] **Step 1: Write the failing tests**

`internal/api/validate_test.go`:
```go
package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/api"
	"github.com/brandonrc/bifrost/internal/api/apitest"
	"github.com/brandonrc/bifrost/internal/controller"
)

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The body that returned 201 on Grace on 2026-09-02 and wedged the
// reconciler with an empty id. It has no `id` and no `spec`.
func TestCreateClusterWithoutRequiredFieldsIs400(t *testing.T) {
	s := api.NewServer()
	s.Store = controller.NewMemoryStore()
	h := apitest.NewHandler(s)
	rec := post(t, h, "/api/v1/clusters", `{"name":"x","engine":"ray","workers":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var e api.ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if !strings.Contains(e.Message, `"id"`) && !strings.Contains(e.Message, "id") {
		t.Errorf("400 body should name the missing field; got %q", e.Message)
	}
	// Nothing must have been persisted.
	clusters, err := s.Store.ListClusters(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Fatalf("a rejected create persisted %d cluster(s)", len(clusters))
	}
}

// Validation must not eat the body: a valid request still reaches the handler.
func TestValidRequestBodyReachesHandler(t *testing.T) {
	s := api.NewServer()
	s.Store = controller.NewMemoryStore()
	h := apitest.NewHandler(s)
	rec := post(t, h, "/api/v1/clusters", `{"id":"ok-1","spec":{"name":"ok-1","project":"p","image":"i","ray_version":"2.56.0","head_cpu":"1","head_memory":"1Gi","worker_groups":[]}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// Non-contract paths are not the validator's business: /docs and the spec
// itself must still be served, not 404'd by a router that does not know them.
func TestValidatorPassesThroughNonContractPaths(t *testing.T) {
	h, _ := apitest.NewServer()
	for _, p := range []string{api.SpecPath, "/docs"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusBadRequest {
			t.Errorf("GET %s = %d; the validator must pass non-contract paths through", p, rec.Code)
		}
	}
}

// The domain guard is independent of the middleware: an id that passes the
// schema (a string) but is not a valid Kubernetes name is still refused.
func TestCreateClusterInvalidK8sNameIs400(t *testing.T) {
	s := api.NewServer()
	s.Store = controller.NewMemoryStore()
	h := apitest.NewHandler(s)
	rec := post(t, h, "/api/v1/clusters", `{"id":"Not_Valid!","spec":{"name":"x","project":"p","image":"i","ray_version":"2.56.0","head_cpu":"1","head_memory":"1Gi","worker_groups":[]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
```
`ListClusters` on `controller.Store` — confirm the method name with `grep -n "ListClusters(ctx" internal/controller/store.go`; if it takes a filter argument, pass the zero value.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/api/ -run 'TestCreateClusterWithoutRequiredFieldsIs400|TestCreateClusterInvalidK8sNameIs400|TestValidatorPassesThroughNonContractPaths|TestValidRequestBodyReachesHandler' -v 2>&1 | grep -E "^(---|\s+validate_test)" `
Expected: `TestCreateClusterWithoutRequiredFieldsIs400` FAILs with `status = 201, want 400` (this is defect 1 reproduced); `TestCreateClusterInvalidK8sNameIs400` FAILs with 201; the other two PASS (nothing wraps yet).

- [ ] **Step 3: Embed the spec**

In `internal/api/gen.go` add:
```go
import _ "embed"

//go:embed openapi.json
var contractJSON []byte
```
(Keep the existing `//go:generate` line.)

- [ ] **Step 4: Write `validate.go`**

```go
package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

var (
	contractOnce sync.Once
	contractDoc  *openapi3.T
	contractRt   routers.Router
)

// ContractDocument is the embedded frozen contract, loaded once. Servers is
// cleared so route matching ignores host.
func ContractDocument() *openapi3.T {
	loadContract()
	return contractDoc
}

func loadContract() {
	contractOnce.Do(func() {
		doc, err := openapi3.NewLoader().LoadFromData(contractJSON)
		if err != nil {
			panic("api: embedded openapi.json does not load: " + err.Error())
		}
		doc.Servers = nil
		rt, err := gorillamux.NewRouter(doc)
		if err != nil {
			panic("api: contract router: " + err.Error())
		}
		contractDoc, contractRt = doc, rt
	})
}

// ValidateRequests enforces the contract — `required`, types, enums, path
// and query parameters — for every operation the contract defines, in one
// place, before any handler runs. Paths the contract does not define (the
// spec document itself, /docs, gateway hosts) pass through untouched: the
// mux behind us owns their 404s.
//
// This exists because on 2026-09-02 a create with no `id` and no `spec`
// returned 201 and persisted an empty-id record that no route could delete:
// Go's encoding/json zero-fills missing fields, and `required` was enforced
// only where a handler happened to hand-check.
func ValidateRequests(next http.Handler) http.Handler {
	loadContract()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, params, err := contractRt.FindRoute(r)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		input := &openapi3filter.RequestValidationInput{
			Request:    r,
			PathParams: params,
			Route:      route,
			Options: &openapi3filter.Options{
				// Auth is RequireAuth's job, one layer out; do not re-check here.
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
		}
		if err := openapi3filter.ValidateRequest(context.Background(), input); err != nil {
			WriteError(w, r, HTTPError{Status: http.StatusBadRequest, Code: "bad_request", Message: err.Error()})
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 5: Wrap the contract routes in `NewHandler`**

In `internal/api/server.go`, immediately after `h := HandlerWithOptions(strict, StdHTTPServerOptions{BaseRouter: mux})` add:
```go
	// Contract validation sits directly on the routes: inside auth (so a
	// missing token is 401, not 400) and inside the gateway (a cluster
	// host is never a contract path).
	h = ValidateRequests(h)
```

- [ ] **Step 6: Add the domain guard in `CreateCluster`**

In `internal/api/clusters.go`, replace
```go
	id := core.ClusterId(body.Id)
```
with
```go
	if !core.IsK8sName(body.Id) {
		return nil, badRequest("id must be a valid Kubernetes name (RFC 1123 label): " + body.Id)
	}
	id := core.ClusterId(body.Id)
```

- [ ] **Step 7: Dependencies and run**

Run: `go get github.com/getkin/kin-openapi@v0.142.0 && go mod tidy && go test ./internal/api/ -run 'Validate|CreateCluster' -v 2>&1 | grep -E "^--- "`
Expected: all four new tests PASS, and every pre-existing `TestCreateCluster_*` still PASSes. If `TestCreateCluster_*` tests now fail with 400, their fixture bodies omit a required field — that is the validator doing its job; fix the fixture to include the contract's required fields, do not loosen the validator.

- [ ] **Step 8: Full API package + race**

Run: `CGO_ENABLED=1 go test -race ./internal/api/... 2>&1 | tail -3`
Expected: `ok`. Pay attention to `middleware_probes_test.go`'s public-allowlist test: `/docs` and the spec path must still not 401/404.

- [ ] **Step 9: Verify against the inproc target too**

Run: `CGO_ENABLED=1 go test -race ./test/requirements/target/... 2>&1 | tail -2`
Expected: still `ok` — the valid body in `inproc_test.go` passes validation.

- [ ] **Step 10: Commit**

```bash
git add internal/api/gen.go internal/api/validate.go internal/api/validate_test.go internal/api/server.go internal/api/clusters.go go.mod go.sum
git commit -m "fix(api): enforce the contract's required fields for every operation

On Grace a POST /clusters with no id and no spec returned 201, persisted an
empty-id record, wedged a reconcile retry loop forever and left a row no
route could delete (docs/defects/2026-09-02-required-fields-unenforced.md).
Nothing validated requests: kin-openapi was codegen-only, and encoding/json
zero-fills.

ValidateRequests now runs the embedded contract through openapi3filter on
every path the contract defines, inside auth and inside the gateway, and
passes non-contract paths through untouched. CreateCluster also refuses
ids that are not RFC 1123 names, independently of the middleware.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

---

### Task 6: Generated contract tests (`required` → 400; unauth → 401)

**Files:**
- Create: `test/requirements/contract/spec.go`
- Create: `test/requirements/contract/required_test.go`
- Create: `test/requirements/contract/unauth_test.go`

**Interfaces:**
- Consumes: `target.Get`, `req.Covers`, `req.Target.API()`; the contract file at `../../../internal/api/openapi.json` (read as a file — not an import).
- Produces: `contract.Load(t) *openapi3.T`; `contract.Operations(doc) []Op` with `Op{Method, Path, ID string; Body *openapi3.SchemaRef; Public bool}`.

- [ ] **Step 1: Write `spec.go`**

```go
// Package contract holds the two generated tests: every operation, every
// required field omitted -> 400; every non-public operation, no token ->
// 401. Two tests, all 47 operations, no per-endpoint memory required.
package contract

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// Public is the auth middleware's exact allowlist (internal/api's
// middleware_probes_test.go, "the Rust seven"). Anything else needs a token.
var Public = map[string]bool{
	"GET /healthz":               true,
	"GET /api/v1/version":        true,
	"GET /api/v1/auth/providers": true,
	"POST /api/v1/auth/login":    true,
}

// Op is one contract operation with what the tests need to drive it.
type Op struct {
	Method, Path, ID string
	Body             *openapi3.SchemaRef // nil when the op takes no JSON body
	Public           bool
}

// Load reads the vendored contract from the repo — the same file the server
// embeds — so these tests can never drift from what the server enforces.
func Load(t testing.TB) *openapi3.T {
	t.Helper()
	p := filepath.Join("..", "..", "..", "internal", "api", "openapi.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read contract %s: %v", p, err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return doc
}

// Operations lists every operation, sorted for stable subtest names.
func Operations(doc *openapi3.T) []Op {
	var out []Op
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			o := Op{Method: strings.ToUpper(method), Path: path, ID: op.OperationID}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				if mt := op.RequestBody.Value.Content.Get("application/json"); mt != nil {
					o.Body = mt.Schema
				}
			}
			o.Public = Public[o.Method+" "+o.Path]
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method+out[i].Path < out[j].Method+out[j].Path })
	return out
}

// FillPath replaces {params} with a plausible value so routing succeeds.
func FillPath(p string) string {
	for _, seg := range []string{"{id}", "{name}", "{principal}", "{username}", "{prefix}", "{project}"} {
		p = strings.ReplaceAll(p, seg, "x1")
	}
	return p
}

// Do sends a raw request through the target's authenticated client base URL.
func Do(t testing.TB, base string, editor func(*http.Request), method, path, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, base+path, nil)
	} else {
		r, err = http.NewRequest(method, base+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		t.Fatal(err)
	}
	if editor != nil {
		editor(r)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
```

These tests use `Target.BaseURL()` and `Target.Authorize(*http.Request)` (defined in Task 3, implemented by `inproc` in Task 4) because they must send bodies the typed client cannot express.

- [ ] **Step 2: Write `required_test.go`**

```go
package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// dummy builds a value satisfying the schema's TYPE with every required
// property present, recursively. It is not a semantically valid object —
// it only needs to be shaped so that omitting one required field is the
// sole reason the request can be rejected for "missing".
func dummy(s *openapi3.SchemaRef) any {
	if s == nil || s.Value == nil {
		return "x"
	}
	v := s.Value
	if len(v.Enum) > 0 {
		return v.Enum[0]
	}
	switch {
	case v.Type.Is("object") || len(v.Properties) > 0:
		m := map[string]any{}
		for _, name := range v.Required {
			m[name] = dummy(v.Properties[name])
		}
		return m
	case v.Type.Is("array"):
		return []any{}
	case v.Type.Is("integer"), v.Type.Is("number"):
		if v.Min != nil {
			return *v.Min
		}
		return 1
	case v.Type.Is("boolean"):
		return true
	default:
		return "x"
	}
}

func TestEveryRequiredFieldIsEnforced(t *testing.T) {
	tgt := target.Get(t).As("admin")
	req.Covers(t, 3, "the contract's required fields are enforced before any handler runs")
	req.Covers(t, 18, "input validation is uniform across the API surface (NIST baseline)")

	doc := Load(t)
	checked := 0
	for _, op := range Operations(doc) {
		if op.Body == nil || op.Body.Value == nil || len(op.Body.Value.Required) == 0 {
			continue
		}
		full, _ := dummy(op.Body).(map[string]any)
		for _, field := range op.Body.Value.Required {
			field := field
			t.Run(fmt.Sprintf("%s_%s_omit_%s", op.Method, op.ID, field), func(t *testing.T) {
				partial := map[string]any{}
				for k, v := range full {
					if k != field {
						partial[k] = v
					}
				}
				body, _ := json.Marshal(partial)
				resp := Do(t, tgt.BaseURL(), tgt.Authorize, op.Method, FillPath(op.Path), string(body))
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("%s %s without %q = %d, want 400; body=%s", op.Method, op.Path, field, resp.StatusCode, raw)
				}
				if !strings.Contains(string(raw), field) {
					t.Errorf("400 body should name the missing field %q; got %s", field, raw)
				}
			})
			checked++
		}
	}
	if checked < 20 {
		t.Fatalf("only %d required-field cases generated; the contract has ~26 — Operations() or dummy() lost some", checked)
	}
}
```

- [ ] **Step 3: Write `unauth_test.go`**

```go
package contract

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestEveryNonPublicOperationRequiresAToken(t *testing.T) {
	tgt := target.Get(t).As("anon")
	req.Covers(t, 3, "deny-by-default: every non-public operation answers 401 without a token")

	doc := Load(t)
	seen := 0
	for _, op := range Operations(doc) {
		if op.Public {
			continue
		}
		op := op
		t.Run(fmt.Sprintf("%s_%s", op.Method, op.ID), func(t *testing.T) {
			body := ""
			if op.Body != nil {
				body = "{}"
			}
			resp := Do(t, tgt.BaseURL(), nil, op.Method, FillPath(op.Path), body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s with no token = %d, want 401", op.Method, op.Path, resp.StatusCode)
			}
		})
		seen++
	}
	if seen < 40 {
		t.Fatalf("only %d non-public operations exercised; the contract has 47 total", seen)
	}
}
```

- [ ] **Step 4: Run**

Run: `CGO_ENABLED=1 go test -race ./test/requirements/contract/ -v 2>&1 | grep -E "^(--- |ok|FAIL|\s+---)" | head -60`
Expected: every subtest PASS. Interpret failures:
- A required-field subtest getting **401** → the op is public in the server but not in `Public`; fix `Public`, not the assertion.
- A required-field subtest getting **404** → `FillPath` produced a path the router rejects (e.g. a `{prefix}` with a format constraint) — extend `FillPath` for that segment.
- A required-field subtest getting **201/200** → the validator is not covering that op. That is a real finding: do not skip it; check `ValidateRequests` placement.
- An unauth subtest getting **400** → auth ran after validation; the wrap order in `NewHandler` is wrong (validation must be *inside* `RequireAuth`).

- [ ] **Step 5: Commit**

```bash
git add test/requirements/contract test/requirements/req/target.go test/requirements/target/inproc/inproc.go
git commit -m "test(contract): every required field -> 400 and every non-public op -> 401, generated

Two tests walk the vendored contract: for each operation with a body,
omit each required field in turn and expect 400 naming it; for each
non-public operation, send no token and expect 401. 47 operations, no
per-endpoint memory. The first would have caught the 2026-09-02 empty-id
defect on the day the endpoint was written.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

---

### Task 7: AST guards and the req 17 test

**Files:**
- Create: `test/requirements/guards_test.go`
- Create: `test/requirements/r17_slurm/provisioner_guard_test.go`

**Interfaces:** none produced; these are enforcement.

- [ ] **Step 1: Write `guards_test.go`** (fails if any rule is currently violated — expected to pass on a clean tree, so also write a negative fixture check)

```go
package requirements

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const internalPrefix = `"github.com/brandonrc/bifrost/internal/`

func goFiles(t *testing.T, root string, testOnly bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		if testOnly && !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func parse(t *testing.T, p string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	return f
}

// Rule 1 (spec §1.3): nothing under test/requirements imports internal/,
// except the inproc target, which must (it calls app.New).
func TestNoInternalImportsOutsideInproc(t *testing.T) {
	for _, p := range goFiles(t, ".", false) {
		if strings.HasPrefix(filepath.ToSlash(p), "target/inproc/") {
			continue
		}
		for _, imp := range parse(t, p).Imports {
			if strings.HasPrefix(imp.Path.Value, internalPrefix) {
				t.Errorf("%s imports %s; requirement tests speak the public contract only", p, imp.Path.Value)
			}
		}
	}
}

// Rule 2: every Test* in a requirement package declares what it proves.
func TestEveryRequirementTestDeclaresCoverage(t *testing.T) {
	for _, p := range goFiles(t, ".", true) {
		slash := filepath.ToSlash(p)
		dir := strings.SplitN(slash, "/", 2)[0]
		if !(dir == "contract" || dir == "pack" || (len(dir) >= 3 && dir[0] == 'r' && dir[1] >= '0' && dir[1] <= '9')) {
			continue
		}
		f := parse(t, p)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Recv != nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "req" && (sel.Sel.Name == "Covers" || sel.Sel.Name == "NotYetBuilt") {
						found = true
					}
				}
				return !found
			})
			if !found {
				t.Errorf("%s: %s has no req.Covers/req.NotYetBuilt", p, fn.Name.Name)
			}
		}
	}
}

// Rule 3 (spec §8): no bare sleeps in requirement tests; use req.Eventually.
func TestNoTimeSleepInRequirementTests(t *testing.T) {
	for _, p := range goFiles(t, ".", true) {
		if strings.HasPrefix(filepath.ToSlash(p), "req/") {
			continue // req.Eventually's own poll interval lives here
		}
		ast.Inspect(parse(t, p), func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && sel.Sel.Name == "Sleep" {
					t.Errorf("%s uses time.Sleep; use req.Eventually", p)
				}
			}
			return true
		})
	}
}
```

- [ ] **Step 2: Write the req 17 test**

`test/requirements/r17_slurm/provisioner_guard_test.go`:
```go
package r17_slurm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// Req 17 is "design must not foreclose Slurm". The seam that would have to
// grow a Slurm implementation is provision.Provisioner; if any of its method
// signatures names a Kubernetes type, a Slurm backend cannot implement it.
func TestProvisionerSeamCarriesNoKubernetesTypes(t *testing.T) {
	req.Covers(t, 17, "provision.Provisioner's method signatures reference no k8s.io / sigs.k8s.io types")

	p := filepath.Join("..", "..", "..", "internal", "provision", "provisioner.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		v := strings.Trim(imp.Path.Value, `"`)
		if strings.HasPrefix(v, "k8s.io/") || strings.HasPrefix(v, "sigs.k8s.io/") {
			t.Errorf("provisioner.go imports %s; the seam must stay engine-agnostic", v)
		}
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Provisioner" {
			return true
		}
		found = true
		return false
	})
	if !found {
		t.Fatal("type Provisioner not found in provisioner.go")
	}
}
```
(If `provisioner.go` imports no k8s packages, none of its signatures can name k8s types — the import check is the complete check; the type-presence check guards against the file being renamed out from under the test.)

- [ ] **Step 3: Run**

Run: `go test ./test/requirements/ ./test/requirements/r17_slurm/ -v 2>&1 | grep -E "^(--- |ok|FAIL)"`
Expected: all PASS. If Rule 2 flags `inproc_test.go`: it lives under `target/`, which the rule excludes by design (it is a target self-test, not a requirement test) — confirm the dir filter; do not add `Covers` there.

- [ ] **Step 4: Commit**

```bash
git add test/requirements/guards_test.go test/requirements/r17_slurm
git commit -m "test: guards for the requirement tree, and the req 17 non-foreclosure test

Three AST rules: no internal/ imports outside target/inproc; every Test in
a requirement package declares Covers or NotYetBuilt; no time.Sleep. The
req 17 test asserts provision.Provisioner's file imports nothing from
k8s.io, which is what keeps a Slurm backend implementable.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

---

### Task 8: `reqreport` — `go test -json` → traceability matrix

**Files:**
- Create: `test/requirements/cmd/reqreport/report.go`
- Create: `test/requirements/cmd/reqreport/main.go`
- Create: `test/requirements/cmd/reqreport/report_test.go`
- Create: `test/requirements/cmd/reqreport/testdata/l2.json` (fixture)
- Create: `test/requirements/cmd/reqreport/testdata/expected.md` (golden)

**Interfaces:**
- Produces: CLI `reqreport -in <file>[,<file>...] -lane l2|l3 -out <dir> [-allow-untested]`; writes `<dir>/traceability.md` and `<dir>/traceability.json`. Exit 1 if any requirement has zero tests (unless `-allow-untested`), or if any `notyetbuilt ... outcome=passed` line exists.
- Consumes: `req.Requirements()`, `req.ParseLine`.

- [ ] **Step 1: Write the fixture** — a minimal `go test -json` stream (one event per line). Save as `testdata/l2.json`:

```json
{"Action":"run","Test":"TestA"}
{"Action":"output","Test":"TestA","Output":"    REQ: kind=covers req=3 reason=\"rbac matrix\"\n"}
{"Action":"pass","Test":"TestA","Elapsed":0.1}
{"Action":"run","Test":"TestB"}
{"Action":"output","Test":"TestB","Output":"    REQ: kind=covers req=3 reason=\"unauth is 401\"\n"}
{"Action":"fail","Test":"TestB","Elapsed":0.1}
{"Action":"run","Test":"TestC"}
{"Action":"output","Test":"TestC","Output":"    REQ: kind=notyetbuilt req=5 reason=\"ephemeral RayJob\" outcome=failed\n"}
{"Action":"pass","Test":"TestC","Elapsed":0.1}
{"Action":"run","Test":"TestD"}
{"Action":"output","Test":"TestD","Output":"    REQ: kind=skip req=0 reason=\"target inproc has no Kubernetes\"\n"}
{"Action":"output","Test":"TestD","Output":"    REQ: kind=covers req=6 reason=\"netpol isolates owners\"\n"}
{"Action":"skip","Test":"TestD","Elapsed":0}
```

- [ ] **Step 2: Write the failing test**

`report_test.go`:
```go
package main

import (
	"os"
	"strings"
	"testing"
)

func TestBuildFromFixture(t *testing.T) {
	rep, err := Build([]string{"testdata/l2.json"}, "l2")
	if err != nil {
		t.Fatal(err)
	}
	r3 := rep.Rows[2]
	if r3.N != 3 || r3.Tests != 2 || r3.Passed != 1 || r3.Failed != 1 {
		t.Errorf("row 3 = %+v", r3)
	}
	r5 := rep.Rows[4]
	if r5.NotYetBuilt != 1 || r5.Status != "not-yet-built" {
		t.Errorf("row 5 = %+v", r5)
	}
	r6 := rep.Rows[5]
	if r6.Skipped != 1 || len(r6.SkipReasons) != 1 {
		t.Errorf("row 6 = %+v", r6)
	}
	if len(rep.Untested()) != 15 {
		t.Errorf("untested = %d, want 15", len(rep.Untested()))
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	rep, err := Build([]string{"testdata/l2.json"}, "l2")
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Markdown()
	want, err := os.ReadFile("testdata/expected.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(string(want)) {
		t.Errorf("markdown differs from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestMarkerDriftIsAnError(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/drift.json"
	os.WriteFile(p, []byte(`{"Action":"run","Test":"T"}
{"Action":"output","Test":"T","Output":"REQ: kind=notyetbuilt req=5 reason=\"x\" outcome=passed\n"}
{"Action":"fail","Test":"T"}
`), 0o644)
	rep, err := Build([]string{p}, "l2")
	if err != nil {
		t.Fatal(err)
	}
	if errs := rep.Problems(true); len(errs) == 0 || !strings.Contains(errs[0], "appears built") {
		t.Errorf("Problems = %v; want a marker-drift error", errs)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./test/requirements/cmd/reqreport/ 2>&1 | head -3`
Expected: undefined `Build`.

- [ ] **Step 4: Write `report.go`**

```go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

type event struct {
	Action string  `json:"Action"`
	Test   string  `json:"Test"`
	Output string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// TestResult is one top-level or sub test with its REQ lines and outcome.
type TestResult struct {
	Name    string
	Outcome string // pass|fail|skip
	Lines   []req.Line
}

// Row is one requirement's aggregate for one lane.
type Row struct {
	N           int      `json:"n"`
	Title       string   `json:"title"`
	Priority    string   `json:"priority"`
	Tests       int      `json:"tests"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	Skipped     int      `json:"skipped"`
	NotYetBuilt int      `json:"not_yet_built"`
	Status      string   `json:"status"` // built|partial|not-yet-built|failing|untested
	TestNames   []string `json:"test_names"`
	SkipReasons []string `json:"skip_reasons"`
}

type Report struct {
	Lane  string `json:"lane"`
	Rows  []Row  `json:"rows"`
	drift []string
}

// Build parses go test -json streams and aggregates per requirement.
func Build(files []string, lane string) (*Report, error) {
	results := map[string]*TestResult{}
	order := []string{}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var e event
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Test == "" {
				continue
			}
			r, ok := results[e.Test]
			if !ok {
				r = &TestResult{Name: e.Test}
				results[e.Test] = r
				order = append(order, e.Test)
			}
			switch e.Action {
			case "output":
				if l, ok := req.ParseLine(strings.TrimSpace(e.Output)); ok {
					r.Lines = append(r.Lines, l)
				}
			case "pass", "fail", "skip":
				r.Outcome = e.Action
			}
		}
		fh.Close()
	}

	rep := &Report{Lane: lane}
	for _, rq := range req.Requirements() {
		rep.Rows = append(rep.Rows, Row{N: rq.N, Title: rq.Title, Priority: rq.Priority})
	}
	for _, name := range order {
		r := results[name]
		for _, l := range r.Lines {
			switch l.Kind {
			case "covers", "notyetbuilt":
				row := &rep.Rows[l.Req-1]
				row.Tests++
				row.TestNames = append(row.TestNames, name)
				if l.Kind == "notyetbuilt" {
					row.NotYetBuilt++
					if l.Outcome == "passed" {
						rep.drift = append(rep.drift, fmt.Sprintf("requirement %d appears built: %s passed under a NotYetBuilt marker (%s)", l.Req, name, l.Reason))
					}
					continue
				}
				switch r.Outcome {
				case "pass":
					row.Passed++
				case "fail":
					row.Failed++
				case "skip":
					row.Skipped++
				}
			}
		}
		// A skip line attaches its reason to every requirement the test covers.
		for _, l := range r.Lines {
			if l.Kind != "skip" {
				continue
			}
			for _, c := range r.Lines {
				if c.Kind == "covers" {
					rep.Rows[c.Req-1].SkipReasons = append(rep.Rows[c.Req-1].SkipReasons, l.Reason)
				}
			}
		}
	}
	for i := range rep.Rows {
		rep.Rows[i].Status = status(rep.Rows[i])
		sort.Strings(rep.Rows[i].TestNames)
	}
	return rep, nil
}

// status is DERIVED, never typed (spec §2.4).
func status(r Row) string {
	switch {
	case r.Tests == 0:
		return "untested"
	case r.Failed > 0:
		return "failing"
	case r.NotYetBuilt == r.Tests:
		return "not-yet-built"
	case r.NotYetBuilt > 0:
		return "partial"
	default:
		return "built"
	}
}

func (r *Report) Untested() []int {
	var out []int
	for _, row := range r.Rows {
		if row.Tests == 0 {
			out = append(out, row.N)
		}
	}
	return out
}

// Problems returns the conditions that must fail the run.
func (r *Report) Problems(allowUntested bool) []string {
	out := append([]string{}, r.drift...)
	if !allowUntested {
		if u := r.Untested(); len(u) > 0 {
			out = append(out, fmt.Sprintf("requirements with zero tests: %v (18 rows means 18 populated rows)", u))
		}
	}
	return out
}

func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Requirement traceability (%s)\n\n", strings.ToUpper(r.Lane))
	b.WriteString("Generated by `test/requirements/cmd/reqreport`. Do not edit; CI regenerates and diffs.\n\n")
	b.WriteString("| # | Requirement | Priority | Tests | Pass | Fail | Skip | Not yet built | Status |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "| %d | %s | %s | %d | %d | %d | %d | %d | **%s** |\n",
			row.N, row.Title, row.Priority, row.Tests, row.Passed, row.Failed, row.Skipped, row.NotYetBuilt, row.Status)
	}
	b.WriteString("\n## Per-requirement detail\n")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "\n### %d. %s\n", row.N, row.Title)
		if len(row.TestNames) == 0 {
			b.WriteString("- _no tests_\n")
		}
		for _, n := range row.TestNames {
			fmt.Fprintf(&b, "- `%s`\n", n)
		}
		for _, s := range row.SkipReasons {
			fmt.Fprintf(&b, "- skipped: %s\n", s)
		}
	}
	return b.String()
}
```

- [ ] **Step 5: Write `main.go`**

```go
// reqreport turns `go test -json` output (and, from P3, JUnit XML) into
// docs/requirements/traceability.md + .json. Status is derived from REQ
// lines and outcomes; it is never typed by hand.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	in := flag.String("in", "", "comma-separated go test -json files")
	lane := flag.String("lane", "l2", "lane label: l2|l3")
	out := flag.String("out", "docs/requirements", "output directory")
	allowUntested := flag.Bool("allow-untested", false, "P0 only: do not fail on requirements with zero tests")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "reqreport: -in is required")
		os.Exit(2)
	}
	rep, err := Build(strings.Split(*in, ","), *lane)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(*out, "traceability.md"), []byte(rep.Markdown()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}
	js, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "traceability.json"), append(js, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}
	for _, row := range rep.Rows {
		fmt.Printf("req %2d  %-13s tests=%d pass=%d fail=%d nyb=%d\n", row.N, row.Status, row.Tests, row.Passed, row.Failed, row.NotYetBuilt)
	}
	if probs := rep.Problems(*allowUntested); len(probs) > 0 {
		for _, p := range probs {
			fmt.Fprintln(os.Stderr, "reqreport: PROBLEM:", p)
		}
		os.Exit(1)
	}
}
```

- [ ] **Step 6: Generate the golden, then run the tests**

Run: `cd test/requirements/cmd/reqreport && go run . -in testdata/l2.json -lane l2 -out /tmp/rr -allow-untested >/dev/null; cp /tmp/rr/traceability.md testdata/expected.md; cd - >/dev/null && go test ./test/requirements/cmd/reqreport/ -v 2>&1 | grep -E "^(--- |ok|FAIL)"`
Expected: three PASS. **Read `testdata/expected.md` before committing it** — row 3 must show Tests=2 Pass=1 Fail=1 Status **failing**; row 5 Status **not-yet-built**; row 6 Tests=1 Skip=1 with the skip reason; all others **untested**. A golden you did not read is not a test.

- [ ] **Step 7: Commit**

```bash
git add test/requirements/cmd/reqreport
git commit -m "feat(reqreport): derive the traceability matrix from go test -json

Parses REQ lines out of test output, aggregates per requirement, derives
status (built / partial / not-yet-built / failing / untested) and renders
markdown + json. A NotYetBuilt marker on a passing test and any untested
row are failures of the report, so the committed matrix cannot claim what
no test proved.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

---

### Task 9: `covreport` — tiered coverage with a ratchet

**Files:**
- Create: `.coverage-tiers.yaml`, `.coverage-exclude`, `.coverage-ratchet.json`
- Create: `test/requirements/cmd/covreport/cov.go`, `main.go`, `cov_test.go`, `testdata/profile.txt`
- Modify: `.github/workflows/ci.yml` (replace `./scripts/coverage-gate.sh`)
- Modify: `Makefile`
- Delete: `scripts/coverage-gate.sh`

**Interfaces:**
- Produces: CLI `covreport -profile coverage.txt [-update]`; exit 1 if any tier is below `ratchet - 0.5`.

- [ ] **Step 1: Policy files**

`.coverage-tiers.yaml`:
```yaml
# Spec §4. Package path prefixes (module-relative). A file belongs to the
# first tier whose prefix matches. Files matching .coverage-exclude are
# removed before tiering.
tiers:
  - name: tier1
    floor: 95.0
    packages: [internal/auth, internal/policy, internal/core]
  - name: tier2
    floor: 90.0
    packages: [internal/api, internal/controller, internal/provision, internal/app]
```
`.coverage-exclude` (one substring per line — the current gate's list plus the generated client):
```
/cmd/
internal/provision/live
internal/controller/storetest
internal/api/zz_generated_api.go
pkg/client/zz_generated_client.go
test/requirements/
```
`.coverage-ratchet.json` — **seeded from measured numbers in Step 6**, placeholder for now:
```json
{"tier1": 0.0, "tier2": 0.0}
```

- [ ] **Step 2: Fixture and failing test**

`testdata/profile.txt`:
```
mode: atomic
github.com/brandonrc/bifrost/internal/core/a.go:1.1,2.2 2 1
github.com/brandonrc/bifrost/internal/core/a.go:3.1,4.2 2 0
github.com/brandonrc/bifrost/internal/api/b.go:1.1,2.2 4 3
github.com/brandonrc/bifrost/internal/api/zz_generated_api.go:1.1,2.2 100 0
github.com/brandonrc/bifrost/cmd/bifrost/main.go:1.1,2.2 10 0
```
`cov_test.go`:
```go
package main

import "testing"

func TestTierPercentages(t *testing.T) {
	policy := Policy{
		Tiers:   []Tier{{Name: "tier1", Packages: []string{"internal/core"}}, {Name: "tier2", Packages: []string{"internal/api"}}},
		Exclude: []string{"/cmd/", "zz_generated_api.go"},
	}
	got, err := Compute("testdata/profile.txt", policy)
	if err != nil {
		t.Fatal(err)
	}
	if got["tier1"] != 50.0 { // 2 of 4 statements covered
		t.Errorf("tier1 = %v, want 50", got["tier1"])
	}
	if got["tier2"] != 100.0 { // generated file excluded; 4 of 4
		t.Errorf("tier2 = %v, want 100", got["tier2"])
	}
}

func TestRatchetTolerance(t *testing.T) {
	cases := []struct {
		have, ratchet float64
		ok            bool
	}{{90, 90, true}, {89.6, 90, true}, {89.4, 90, false}, {95, 90, true}}
	for _, c := range cases {
		if got := WithinRatchet(c.have, c.ratchet); got != c.ok {
			t.Errorf("WithinRatchet(%v,%v)=%v want %v", c.have, c.ratchet, got, c.ok)
		}
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./test/requirements/cmd/covreport/ 2>&1 | head -3`
Expected: undefined `Policy`, `Compute`, `WithinRatchet`.

- [ ] **Step 4: Write `cov.go`**

```go
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Tier struct {
	Name     string   `yaml:"name"`
	Floor    float64  `yaml:"floor"`
	Packages []string `yaml:"packages"`
}

type Policy struct {
	Tiers   []Tier `yaml:"tiers"`
	Exclude []string
}

const modulePrefix = "github.com/brandonrc/bifrost/"

// Compute returns covered-statement percentage per tier from a coverprofile.
// Lines: file:startLine.col,endLine.col numStatements count
func Compute(profile string, p Policy) (map[string]float64, error) {
	fh, err := os.Open(profile)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	total := map[string]int{}
	covered := map[string]int{}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		fileEnd := strings.Index(line, ":")
		if fileEnd < 0 {
			continue
		}
		file := strings.TrimPrefix(line[:fileEnd], modulePrefix)
		if excluded(file, p.Exclude) {
			continue
		}
		tier := tierFor(file, p.Tiers)
		if tier == "" {
			continue
		}
		fields := strings.Fields(line[fileEnd+1:])
		if len(fields) != 3 {
			continue
		}
		n, _ := strconv.Atoi(fields[1])
		c, _ := strconv.Atoi(fields[2])
		total[tier] += n
		if c > 0 {
			covered[tier] += n
		}
	}
	out := map[string]float64{}
	for _, t := range p.Tiers {
		if total[t.Name] == 0 {
			out[t.Name] = 0
			continue
		}
		out[t.Name] = 100 * float64(covered[t.Name]) / float64(total[t.Name])
	}
	return out, sc.Err()
}

func excluded(file string, patterns []string) bool {
	for _, pat := range patterns {
		if pat != "" && strings.Contains("/"+file, pat) {
			return true
		}
	}
	return false
}

func tierFor(file string, tiers []Tier) string {
	for _, t := range tiers {
		for _, pkg := range t.Packages {
			if strings.HasPrefix(file, pkg+"/") {
				return t.Name
			}
		}
	}
	return ""
}

// WithinRatchet allows a drop of at most 0.5 points (spec §4).
func WithinRatchet(have, ratchet float64) bool { return have >= ratchet-0.5 }

func fmtPct(v float64) string { return fmt.Sprintf("%.1f%%", v) }
```

- [ ] **Step 5: Write `main.go`**

```go
// covreport computes risk-tiered coverage from a Go coverprofile and gates
// it against .coverage-ratchet.json: a tier may drop at most 0.5 points;
// -update raises the ratchet to the measured value. It prints the exclusion
// list on every run so "excluded" is never invisible (spec §4).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "covreport:", err)
		os.Exit(1)
	}
}

func main() {
	profile := flag.String("profile", "coverage.txt", "coverprofile path")
	tiersPath := flag.String("tiers", ".coverage-tiers.yaml", "tier policy")
	excludePath := flag.String("exclude", ".coverage-exclude", "exclusion substrings, one per line")
	ratchetPath := flag.String("ratchet", ".coverage-ratchet.json", "ratchet file")
	update := flag.Bool("update", false, "raise the ratchet to measured values")
	flag.Parse()

	var p Policy
	tb, err := os.ReadFile(*tiersPath)
	must(err)
	must(yaml.Unmarshal(tb, &p))
	eb, err := os.ReadFile(*excludePath)
	must(err)
	for _, l := range strings.Split(string(eb), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			p.Exclude = append(p.Exclude, l)
		}
	}
	ratchet := map[string]float64{}
	if rb, err := os.ReadFile(*ratchetPath); err == nil {
		must(json.Unmarshal(rb, &ratchet))
	}

	got, err := Compute(*profile, p)
	must(err)

	fmt.Println("exclusions:", strings.Join(p.Exclude, " "))
	failed := false
	for _, t := range p.Tiers {
		have := got[t.Name]
		r := ratchet[t.Name]
		ok := WithinRatchet(have, r)
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed = true
		}
		fmt.Printf("%s %-6s %s (ratchet %s, spec floor %s)\n", mark, t.Name, fmtPct(have), fmtPct(r), fmtPct(t.Floor))
		if *update && have > r {
			ratchet[t.Name] = float64(int(have*10)) / 10
		}
	}
	if *update {
		js, _ := json.MarshalIndent(ratchet, "", "  ")
		must(os.WriteFile(*ratchetPath, append(js, '\n'), 0o644))
		fmt.Println("ratchet updated:", *ratchetPath)
	}
	if failed {
		os.Exit(1)
	}
}
```

- [ ] **Step 6: Run tests, measure real numbers, seed the ratchet**

Run: `go test ./test/requirements/cmd/covreport/ 2>&1 | tail -1 && CGO_ENABLED=1 go test -race -count=1 -covermode=atomic -coverprofile=coverage.txt ./... >/dev/null 2>&1; go run ./test/requirements/cmd/covreport -profile coverage.txt -update`
Expected: `ok` for the unit tests; then two tier lines with real percentages and `ratchet updated`. **Record the two numbers in the commit message.** Without Postgres locally, tier2 will read lower than CI (the SPEC notes 76% vs 83.3%); that is fine — CI's first run with `-update` is not possible (it cannot commit), so the ratchet is seeded locally and CI checks against it with the 0.5 tolerance. If CI's number is *higher* than local, nothing fails.

- [ ] **Step 7: Wire CI and Makefile; delete the old gate**

In `.github/workflows/ci.yml` replace `- run: ./scripts/coverage-gate.sh` with:
```yaml
      - name: tiered coverage ratchet (spec §4)
        run: go run ./test/requirements/cmd/covreport -profile coverage.txt
```
and remove the `COVERAGE_THRESHOLD: "80"` env line. Delete `scripts/coverage-gate.sh`.

In `Makefile` add:
```makefile
.PHONY: cover-tiers ratchet
cover-tiers: cover
	go run ./test/requirements/cmd/covreport -profile coverage.txt
ratchet: cover
	go run ./test/requirements/cmd/covreport -profile coverage.txt -update
```

- [ ] **Step 8: Verify the gate fails when it should**

Run: `cp .coverage-ratchet.json /tmp/r.json && python3 -c "import json;d=json.load(open('.coverage-ratchet.json'));d['tier1']=99.9;json.dump(d,open('.coverage-ratchet.json','w'))" && go run ./test/requirements/cmd/covreport -profile coverage.txt; echo "exit=$?"; cp /tmp/r.json .coverage-ratchet.json`
Expected: `FAIL tier1 ...` and `exit=1`. Then the ratchet is restored.

- [ ] **Step 9: Commit**

```bash
git add .coverage-tiers.yaml .coverage-exclude .coverage-ratchet.json test/requirements/cmd/covreport .github/workflows/ci.yml Makefile
git rm scripts/coverage-gate.sh
git commit -m "feat(covreport): risk-tiered coverage with a ratchet replaces the flat 80% gate

Tier 1 (auth, policy, core) targets 95%; tier 2 (api, controller,
provision, app) targets 90%. Each tier may drop at most 0.5 points from
its ratchet; make ratchet raises it. Exclusions live in one file and are
printed on every run. Seeded from a local measurement of
tier1=<N>% tier2=<M>%.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```
(Replace `<N>`/`<M>` with the measured values from Step 6.)

---

### Task 10: L2 CI job, Makefile targets, and the first committed matrix

**Files:**
- Modify: `.github/workflows/ci.yml` (new job `requirements-l2`)
- Modify: `Makefile`
- Create: `docs/requirements/traceability.md`, `docs/requirements/traceability.json` (generated)
- Modify: `docs/SPEC.md` (one line pointing at the matrix — full column replacement is P4)

- [ ] **Step 1: Makefile targets**

```makefile
.PHONY: test-l2 report
# L2: the requirement suite against the in-process target.
test-l2:
	CGO_ENABLED=1 REQ_TARGET=inproc go test -race -count=1 -json ./test/requirements/... > l2.json || (go run ./test/requirements/cmd/reqreport -in l2.json -lane l2 -out docs/requirements -allow-untested; exit 1)
	go run ./test/requirements/cmd/reqreport -in l2.json -lane l2 -out docs/requirements -allow-untested
report: test-l2
```

- [ ] **Step 2: Run it locally and read the matrix**

Run: `make test-l2 2>&1 | tail -22 && sed -n '1,25p' docs/requirements/traceability.md`
Expected: reqreport prints 18 rows; rows 3, 17 and 18 show tests; row 3 status **built** (both contract tests pass on L2), 17 **built**, 18 **built**; all others **untested** (allowed in P0). `docs/requirements/traceability.md` exists.

- [ ] **Step 3: CI job**

Append to `.github/workflows/ci.yml` `jobs:`:
```yaml
  requirements-l2:
    # Spec §3: the requirement suite against the in-process target, on every
    # push, merge-blocking. Regenerates the traceability matrix and fails if
    # the committed one differs, so docs/requirements/ is always the truth
    # of the last run. -allow-untested is P0-only; the P1 plan removes it.
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - name: Checkout bifrost-pack (for test/requirements/pack)
        uses: actions/checkout@v4
        with:
          repository: brandonrc/bifrost-pack
          ref: main
          path: ../bifrost-pack
      - uses: azure/setup-helm@v4
      - name: L2 requirement suite
        run: CGO_ENABLED=1 REQ_TARGET=inproc go test -race -count=1 -json ./test/requirements/... > l2.json
      - name: Regenerate traceability
        if: always()
        run: go run ./test/requirements/cmd/reqreport -in l2.json -lane l2 -out docs/requirements -allow-untested
      - name: Committed matrix must match
        run: git diff --exit-code -- docs/requirements/
```
Note the checkout path `../bifrost-pack` is relative to the workspace, which is what Task 11's `pack/helm.go` resolves to (`../../../../bifrost-pack/chart` from `test/requirements/pack/`). Verify `actions/checkout` accepts a parent-relative path; if it refuses, check out to `bifrost-pack` inside the workspace and set `PACK_CHART=$GITHUB_WORKSPACE/bifrost-pack/chart` as an env on the L2 step — `pack/helm.go` honours `PACK_CHART`.

- [ ] **Step 4: Point SPEC at the matrix**

In `docs/SPEC.md`, directly under the `## Requirements (Ray Software Pack — Desired User Experience)` heading, add:
```markdown
> **Status is now derived, not typed.** The authoritative per-requirement
> status is `docs/requirements/traceability.md`, regenerated by CI from the
> requirement test suite (`test/requirements/`). The status column below is
> retained for history during P0–P3 and is replaced by links in P4.
```

- [ ] **Step 5: Commit**

```bash
git add Makefile .github/workflows/ci.yml docs/requirements docs/SPEC.md
git commit -m "ci: run the L2 requirement suite on every push and commit the matrix

make test-l2 runs test/requirements against the in-process target and
regenerates docs/requirements/traceability.md; CI fails if the committed
matrix differs. Rows 3, 17 and 18 are the first with tests; the rest are
untested and say so — -allow-untested is P0-only.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

- [ ] **Step 6: Push and watch CI**

Run: `git push origin HEAD && gh run watch --exit-status $(gh run list --limit 1 --json databaseId -q '.[0].databaseId')`
Expected: all jobs green including `requirements-l2`. If `requirements-l2` fails on the pack tests with "chart not found", Task 11 has not landed yet — that is expected ordering only if Task 11 is done first; otherwise Task 11's `helm.go` skips with a reason when the chart is absent (see Task 11 Step 2).

---

### Task 11: Fix defect 4 (nginx startup DNS) and the two pack template tests

**Files (bifrost-pack repo):**
- Modify: `chart/templates/ui-configmap.yaml`
- Modify: `chart/templates/ui.yaml:53-70` (env, mounts, volumes)

**Files (bifrost repo):**
- Create: `test/requirements/pack/helm.go`
- Create: `test/requirements/pack/nginx_test.go`
- Create: `test/requirements/pack/imagetag_test.go`

**Interfaces:**
- Produces: `pack.Render(t, setValues ...string) (manifests string)`; `pack.RenderErr(t, setValues ...string) error`; env `PACK_CHART` overrides the chart location.

- [ ] **Step 1: Write the failing pack tests (bifrost repo)**

`test/requirements/pack/helm.go`:
```go
// Package pack renders the bifrost-pack Helm chart and asserts on the
// output. The chart is the deployable unit; a template-level assertion is
// the cheapest place to pin behaviour that only showed up on a real node
// (defect 4: nginx exiting at boot because DNS was not up yet).
package pack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ChartDir resolves the chart: $PACK_CHART, else a sibling checkout.
func ChartDir(t testing.TB) string {
	t.Helper()
	if v := os.Getenv("PACK_CHART"); v != "" {
		return v
	}
	p := filepath.Join("..", "..", "..", "..", "bifrost-pack", "chart")
	if _, err := os.Stat(filepath.Join(p, "Chart.yaml")); err != nil {
		t.Skipf("bifrost-pack chart not found at %s (set PACK_CHART); REQ: kind=skip req=0 reason=%q", p, "bifrost-pack chart not checked out")
	}
	return p
}

func helm(t testing.TB, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Render templates the chart with --set values and returns the manifests.
func Render(t testing.TB, set ...string) string {
	t.Helper()
	args := []string{"template", "t", ChartDir(t), "--set", "nebariApp.api.enabled=false"}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	out, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// RenderErr templates and returns the error (nil if it rendered).
func RenderErr(t testing.TB, set ...string) (string, error) {
	t.Helper()
	args := []string{"template", "t", ChartDir(t), "--set", "nebariApp.api.enabled=false"}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	return helm(t, args...)
}

func mustContain(t testing.TB, hay, needle, why string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Errorf("rendered chart lacks %q — %s", needle, why)
	}
}

func mustNotContain(t testing.TB, hay, needle, why string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Errorf("rendered chart contains %q — %s", needle, why)
	}
}
```

`nginx_test.go`:
```go
package pack

import (
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// Defect 4 (docs/defects, 2026-09-02): the dashboard's nginx resolved
// `proxy_pass http://bifrost:8484` once at startup and exited when CoreDNS
// was not yet up, taking the UI down on every node reboot. The fix defers
// resolution to request time: a `resolver` directive plus a variable
// upstream. The nginx image renders /etc/nginx/templates/*.template with
// envsubst at boot and exports NGINX_LOCAL_RESOLVERS from /etc/resolv.conf,
// so the chart ships a template, not a finished conf.
func TestDashboardNginxResolvesUpstreamAtRequestTime(t *testing.T) {
	req.Covers(t, 6, "the self-serve dashboard survives a node reboot: nginx does not need DNS to start")
	req.Covers(t, 8, "deployment recovers after infrastructure failure without manual intervention")

	out := Render(t, "image.tag=sha-test", "ui.enabled=true")

	mustContain(t, out, "resolver ${NGINX_LOCAL_RESOLVERS}", "nginx must use a runtime resolver so upstream names are re-resolved")
	mustContain(t, out, "set $bifrost_upstream", "the upstream must be a variable so nginx resolves it per request, not at boot")
	mustContain(t, out, "proxy_pass $bifrost_upstream", "proxy_pass must reference the variable")
	mustNotContain(t, out, "proxy_pass http://", "a literal proxy_pass host is resolved once at startup and kills nginx if DNS is down")
	mustContain(t, out, "default.conf.template", "the conf must ship as a template so the image's envsubst renders it at boot")
	mustContain(t, out, "/etc/nginx/templates", "the template must be mounted where the image's entrypoint looks")
	mustContain(t, out, "NGINX_ENVSUBST_FILTER", "envsubst must be restricted to NGINX_* or it will clobber nginx's own $variables")
}
```

`imagetag_test.go`:
```go
package pack

import (
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// The chart's default image tag once named an image that never existed
// (AppVersion is not a published tag). It must refuse to render without one.
func TestImageTagIsRequired(t *testing.T) {
	req.Covers(t, 8, "a deployment cannot silently point at a nonexistent image")

	out, err := RenderErr(t)
	if err == nil {
		t.Fatalf("chart rendered without image.tag; it must refuse:\n%s", out)
	}
	if !strings.Contains(out, "image.tag is required") {
		t.Errorf("refusal should name image.tag; got:\n%s", out)
	}
	if _, err := RenderErr(t, "image.tag=sha-test"); err != nil {
		t.Errorf("chart failed to render WITH image.tag: %v", err)
	}
}
```

- [ ] **Step 2: Run to see the nginx test fail and the tag test pass**

Run: `go test ./test/requirements/pack/ -v 2>&1 | grep -E "^(--- |\s+nginx_test|\s+imagetag_test|ok|FAIL)"`
Expected: `TestImageTagIsRequired` PASS (already fixed in the chart); `TestDashboardNginxResolvesUpstreamAtRequestTime` FAIL on every `mustContain` and on `proxy_pass http://` being present.

- [ ] **Step 3: Rewrite `ui-configmap.yaml` (bifrost-pack repo)**

Replace the `data:` block entirely:
```yaml
data:
  # Shipped as a TEMPLATE, not a finished conf. The nginx image's entrypoint
  # (20-envsubst-on-templates.sh) renders /etc/nginx/templates/*.template
  # into /etc/nginx/conf.d/ at boot, and 15-local-resolvers.envsh exports
  # NGINX_LOCAL_RESOLVERS from the pod's /etc/resolv.conf. That gives nginx
  # the cluster DNS address without the chart hardcoding one.
  #
  # Why: nginx resolves a literal `proxy_pass http://name:port` ONCE, at
  # startup, and exits with "host not found in upstream" if DNS is not yet
  # answering. On a node reboot CoreDNS is rarely first; the dashboard
  # crash-looped on Grace for exactly this reason (2026-09-02). A `resolver`
  # plus a variable upstream defers resolution to request time.
  #
  # NGINX_ENVSUBST_FILTER on the Deployment restricts substitution to
  # NGINX_* so nginx's own $host / $http_upgrade etc. survive envsubst.
  default.conf.template: |
    server {
        listen 8080;
        server_name _;
        root /usr/share/nginx/html;
        index index.html;

        resolver ${NGINX_LOCAL_RESOLVERS} valid=10s ipv6=off;
        set $bifrost_upstream http://{{ include "bifrost-pack.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.service.port }};

        location /api/ {
            proxy_pass $bifrost_upstream;
            proxy_set_header Host $host;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection "upgrade";
            proxy_read_timeout 3600s;
        }
        location = /healthz {
            proxy_pass $bifrost_upstream;
        }
        location /docs {
            proxy_pass $bifrost_upstream;
            proxy_set_header Host $host;
        }
        location / {
            try_files $uri $uri/ /index.html;
        }
    }
```
**A behavioural note for the implementer:** with a variable in `proxy_pass` and no URI part, nginx forwards the original request URI unchanged — the same behaviour as the previous literal form without a trailing path. Do not append `$request_uri`; that would double-encode.

- [ ] **Step 4: Update `ui.yaml` (bifrost-pack repo)**

Replace the container's `volumeMounts` and the pod `volumes` blocks, and add `env`:
```yaml
          env:
            # Restrict envsubst to our variables: nginx's own $host,
            # $http_upgrade, $uri... must not be substituted away.
            - name: NGINX_ENVSUBST_FILTER
              value: "^NGINX_"
          volumeMounts:
            - name: nginx-template
              mountPath: /etc/nginx/templates/default.conf.template
              subPath: default.conf.template
              readOnly: true
            # conf.d must be writable for the entrypoint to render into it.
            - name: nginx-confd
              mountPath: /etc/nginx/conf.d
```
and
```yaml
      volumes:
        - name: nginx-template
          configMap:
            name: {{ include "bifrost-pack.ui.fullname" . }}
        - name: nginx-confd
          emptyDir: {}
```

- [ ] **Step 5: Lint the chart and run the pack tests**

Run: `helm lint /Users/khan/openteams/bifrost-pack/chart --set image.tag=x --set ui.enabled=true 2>&1 | tail -2 && cd /Users/khan/openteams/bifrost && go test ./test/requirements/pack/ -v 2>&1 | grep -E "^(--- |ok|FAIL)"`
Expected: lint clean; both tests PASS.

- [ ] **Step 6: Commit the chart (bifrost-pack repo)**

```bash
cd /Users/khan/openteams/bifrost-pack
git add chart/templates/ui-configmap.yaml chart/templates/ui.yaml
git commit -m "fix(ui): resolve the API upstream at request time, not at nginx startup

nginx resolves a literal proxy_pass host once at boot and exits with
\"host not found in upstream\" if DNS is not answering yet. After a node
reboot CoreDNS is rarely first, so the dashboard crash-looped on Grace
until backoff happened to land after DNS was up (2026-09-02).

The conf now ships as /etc/nginx/templates/default.conf.template; the
image's entrypoint renders it with envsubst and exports
NGINX_LOCAL_RESOLVERS from /etc/resolv.conf, so a resolver directive and
a variable upstream defer resolution to request time. NGINX_ENVSUBST_FILTER
keeps envsubst away from nginx's own variables; conf.d becomes an emptyDir
so the render has somewhere to land.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
```

- [ ] **Step 7: Prove it on grace — upgrade, then start nginx with DNS down**

This is the test that only a real node can run. Grace is a playground; CoreDNS is scaled to zero for roughly 20 seconds.

```bash
ssh geraci@grace 'bash -s' <<'REMOTE'
export KUBECONFIG=/var/snap/microk8s/current/credentials/client.config
set -e
cd /tmp && rm -rf bifrost-pack && git clone -q https://github.com/brandonrc/bifrost-pack.git 2>/dev/null || true
REMOTE
# If the pack has no remote yet, copy the chart instead:
rsync -a --delete /Users/khan/openteams/bifrost-pack/chart/ geraci@grace:/tmp/bifrost-pack-chart/
ssh geraci@grace 'bash -s' <<'REMOTE'
export KUBECONFIG=/var/snap/microk8s/current/credentials/client.config
set -e
TAG=$(kubectl -n bifrost get deploy bifrost -o jsonpath='{.spec.template.spec.containers[0].image}' | sed 's/.*://')
microk8s helm3 upgrade bifrost /tmp/bifrost-pack-chart -n bifrost --reuse-values --set image.tag="$TAG" --wait --timeout 3m
echo "=== rendered conf inside the pod ==="
POD=$(kubectl -n bifrost get pod -l app.kubernetes.io/name=bifrost-pack-ui -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || kubectl -n bifrost get pod -o name | grep ui | head -1 | cut -d/ -f2)
kubectl -n bifrost exec "$POD" -- sh -c 'grep -E "resolver|proxy_pass|set \$bifrost" /etc/nginx/conf.d/default.conf'
echo "=== DNS DOWN: delete the ui pod and check it starts anyway ==="
kubectl -n kube-system scale deploy coredns --replicas=0
kubectl -n bifrost delete pod "$POD" --wait=false
sleep 15
kubectl -n bifrost get pods | grep ui
kubectl -n bifrost logs -l app.kubernetes.io/name=bifrost-pack-ui --tail=5 2>/dev/null | grep -i "emerg\|host not found" && echo "FAIL: nginx still needs DNS at boot" || echo "OK: nginx started without DNS"
kubectl -n kube-system scale deploy coredns --replicas=1
kubectl -n kube-system rollout status deploy coredns --timeout=60s
sleep 5
echo "=== DNS BACK: dashboard proxies to the API ==="
kubectl -n bifrost run curl-$RANDOM --rm -i --restart=Never --image=curlimages/curl:8.10.1 --quiet -- sh -c 'curl -s -o /dev/null -w "ui=%{http_code} " http://bifrost-ui/; curl -s -o /dev/null -w "ui->api healthz=%{http_code}\n" http://bifrost-ui/healthz' 2>&1 | tail -1
REMOTE
```
Expected: the rendered conf shows `resolver 10.152.183.10 valid=10s ipv6=off;` (grace's kube-dns), `set $bifrost_upstream http://bifrost.bifrost.svc.cluster.local:8484;`, `proxy_pass $bifrost_upstream;`. With CoreDNS at 0 the new UI pod reaches `Running` with **no** `host not found` in its logs — that line is the whole point. After CoreDNS returns: `ui=200 ui->api healthz=200`.

If the pod label selector in the script does not match, use `kubectl -n bifrost get pods` to find the UI pod name and substitute.

- [ ] **Step 8: Commit the pack tests (bifrost repo) and push both repos**

```bash
cd /Users/khan/openteams/bifrost
git add test/requirements/pack
git commit -m "test(pack): the dashboard must not need DNS to start; image.tag must be required

Two helm-template assertions against bifrost-pack. The nginx one pins
defect 4's fix (resolver + variable upstream, shipped as a template the
image renders at boot); the image.tag one pins an earlier defect where
the chart's default named an image that never existed.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01LGo24EU9EmVHMGhnDEi3zk"
git push origin HEAD
```

---

## Self-review against the spec

**Spec coverage (P0 items from §10):**
- `internal/app.New` extraction → Task 1 ✔
- `pkg/client` generation → Task 2 ✔
- `req` package → Task 3 ✔ (Covers, NotYetBuilt, NeedsCapability, NeedK8s, Eventually, RunID, Target)
- `inproc` target → Task 4 ✔ (seeded principals; fake converges; refuses invalid names)
- `reqreport` / `covreport` → Tasks 8, 9 ✔
- L2 CI job → Task 10 ✔ (`-allow-untested` flagged as P0-only in three places)
- Ratchet from current numbers → Task 9 Step 6 ✔
- Two generated contract tests → Task 6 ✔
- Pack tests → Task 11 ✔ (two of three; the kind-install test needs the cluster target and is **P2**, as the spec's own lane table implies for an L3 test)
- Defect 1 test + fix → Task 5 ✔ (middleware) + Task 6 (generated regression)
- Defect 4 test + fix → Task 11 ✔ including proof on grace with DNS down
- Guards §1.3 → Task 7 ✔; req 17 test → Task 7 ✔
- §1.4 cleanup by run prefix → Task 4 (`Cleanup`, `req.Name`) ✔
- §2.2 marker semantics → Task 3 + reqreport drift check in Task 8 ✔
- §2.4 derived status; untested is a hard failure → Task 8 ✔ (`-allow-untested` P0-only)
- §4 exclusions in one file, printed → Task 9 ✔
- §8 `Eventually` per-lane timeout; no bare sleep; Covers range panic → Tasks 3, 7 ✔

**Deliberately not in P0** (each named in the plan, not silently dropped): fake clock (P1), `permissions.yaml` matrix (P1), cluster target / kind lane / `TestCNIEnforces` (P2), JUnit ingestion (P3), mutation & soak (P4), SPEC status column replacement (P4).

**Type consistency:** `req.Target` declares `BaseURL()` and `Authorize(*http.Request)` in Task 3; `inproc` implements them in Task 4; Task 6 consumes them. `app.Config.ReconcileInterval` is `time.Duration` in Task 1 and used as such in Task 4. `client.*JSONRequestBody` aliases are used consistently (Tasks 2, 4). `req.Line.Format()`/`ParseLine` are the only shared format between Task 3 and Task 8.

**Placeholder scan:** the only `<N>`/`<M>` are in Task 9's commit template, explicitly to be replaced by measured values in Step 6.
