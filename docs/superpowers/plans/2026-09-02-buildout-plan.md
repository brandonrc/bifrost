# Build-out plan — requirements 1, 2, 4, 5, 7, 12, 14 (+16 optional), contract source-of-truth flip, rename (2026-09-02)

> Produced by the planning agent from the code at main 18ca8e6; executed by parallel implementer agents, one worktree and
> one PR per package. Rulings on the plan's §6 decisions, made 2026-09-02 so work could start (the human can overturn any):
>
> - D1 hostname scheme: `<name>.<--gateway-domain>`, `--gateway-external-base` for `gateway_url`; kind uses `ray.kind.invalid` via Host headers; grace wildcard host is a later pack change.
> - D2 per-cluster Ray auth token: not in this build (NetworkPolicy is the boundary; the gateway is the authenticated path).
> - D3 serving pool: an administrator creates a pool with `purpose: serving` through the API; no auto-provisioning.
> - D4 profile expansion: `ClusterSpec.profile` fills zero-valued shape fields and refuses conflicting non-empty ones (400).
> - D5 Kueue admission of RayService-owned RayClusters: Bifrost-side serving ledger is the tested property; the queue label is stamped, Kueue queueing observed-not-required.
> - D6 serve fixture: ConfigMap-mounted `app.py` through the #12 file catalog; no egress relaxation.
> - D7 profiles/admission/storage live in the policy row, editable via `PUT /settings/policy`.
> - D8 one service per project (409), `--services-per-project` as the escape hatch.
> - D9 Dask (package I): DEFERRED. Row 16 stays not-yet-built.
> - D10 `BIFROST_API_PUSH_TOKEN`: set from the repo owner's gh token for now; replace with a fine-grained PAT (contents: write on bifrost-api).
> - D11 `deploy_service` keeps the contract's 202; the r01/r02 tests are fixed to accept it.

# Bifrost build-out plan: requirements 1, 2, 4, 5, 7 (profiles/per-group), 12, 14 (per-user), 16 (cheap), rename, pack, and the contract source-of-truth flip

Base: `bifrost` main `18ca8e6`. Today `internal/api/openapi.json` and `bifrost-api/openapi.json` are byte-identical (47 operations); L2 matrix has rows 1, 2, 4, 5, 12, 16 at `not-yet-built`.

## 0. Findings that shape the design (verified in code)

1. **Why service deploy never converges.** `internal/api/services.go` is a thin proxy: `DeployService` calls `s.ServiceProvisioner.Deploy` and answers **202**; no store row, no owner, no project check (`Authorize(..., auth.TargetService)` is global). On inproc `ServiceProvisioner` is nil, so deploy is **502** and r01/r02 can never go green on L2. On kind the tests themselves demand **201** (`test/requirements/r01_serve_from_jupyter/serve_test.go:26`, `r02_group_serving/group_test.go:25`) while the contract says 202, so they fail before waiting. Even fixed, `serve_config_v2: "applications: []"` will not reach `ServiceStatus=Running` (KubeRay readiness needs serve endpoints), and the tenant NetworkPolicy (`internal/provision/kuberay.go` TenantAllowNetworkPolicy) allows egress only to DNS and intra-cluster, so a `working_dir` fetched from GitHub cannot work. The serve port 8000 **is** already admitted from control-plane pods (`ControlPlanePodLabel`), so a control-plane Serve gateway is network-feasible. `live.ServiceClient.Get` maps the deprecated `Status.ServiceStatus` via `provision.ServiceStatusToState` and hard-codes `http://<name>-serve-svc.<ns>.svc:8000`.
2. **Gateway registry is static.** `core.ClusterRegistry{Clusters []ClusterEndpoint}` is loaded once by `cmd/bifrost/registry.go` `loadRegistry`, held by `api.GatewayState.Registry`, `api.AuthState.Registry` (`hostIsCluster`), and `api.Server.Registry` (`ListRegistry`, `dashboardBaseAndToken`). Nothing writes to it after boot. The reconciler already receives `observed.ApiBaseUrl` from `Provisioner.Observe` (`internal/controller/reconcile.go` `reconcileOne`) and the live client names it `http://<id>-head-svc.<ns>.svc:8265` (`live/client.go` `apiBaseURL`). Host matching uses `r.Host`, so on kind a test can reach a dynamic entry with a plain `Host:` header against the NodePort; no DNS is needed.
3. **Kueue naming collision risk for #4.** `provision.LocalQueueFor` names the LocalQueue `alloc.Project`; a project allocated to a compute pool and a serving pool in one namespace would collide. `controller.queueAssignmentForProject` returns the first allocation across all pools, so a compute cluster would happily land in a serving queue today.
4. **RayJob is available.** kuberay `ray-operator` v1.7 in the module cache has `rayv1.RayJob` with `Entrypoint`, `RuntimeEnvYAML`, `RayClusterSpec`, `ShutdownAfterJobFinishes`, `TTLSecondsAfterFinished`, `SubmissionMode`, and `Status.{JobStatus, JobDeploymentStatus, RayClusterName, DashboardURL, StartTime, EndTime}`; `rayv1.AddToScheme` in `live.NewScheme` already registers it. The kind manifests and the pack `rbac.yaml` grant only `rayclusters, rayservices`.
5. **Store extension pattern.** `controller.Store` (`internal/controller/store.go:616`) is implemented by `MemoryStore`, `SqliteStore`, `PostgresStore`; schemas live in `store_sqlite_schema.go` and the `CREATE TABLE` slice in `store_postgres.go:82-210`; `storetest.RunConformance` is the single scenario suite and `runNonNilEmptyConformance` must list every new List method.
6. **Predecessor design references.** mobula branch `feat/pod-shaping`, commit `714b6a9` ("pod shaping — env, volumes, service account and placement (#66)"): `crates/mobula-core/src/podspec.rs` (`PodOverrides{env, mounts: Vec<String>, service_account, placement}` names-only; `ResolvedPodShape` server-computed), `crates/mobula-policy/src/podshape.rs` (`PodShapeCatalog` in the policy row, edited via `PUT /settings/policy`, validated as a unit, never retroactive). Its commit message explicitly excludes secret refs in the spec and points at catalog-based delivery — that is the shape #12 follows. `crates/mobula-provision/src/router.rs` (`EngineRouter`, id→engine cache) and `dask.rs` (`kubernetes.dask.org/v1 DaskCluster`) are the #16 reference. mobula's `.github/workflows/sync-api.yml` is the template for the sync-direction flip.
7. **Test-framework constraints every package inherits.** `test/requirements/guards_test.go`: no `internal/` imports outside `target/inproc`, every `Test*` calls `req.Covers`/`req.NotYetBuilt`, no `time.Sleep`. `contract.TestEveryNonPublicOperationRequiresAToken` and `TestEveryRequiredFieldIsEnforced` walk **every** operation in `internal/api/openapi.json`; `r03_rbac.TestPermissionMatrix` fails on any non-public operation missing from `permissions.yaml` (`allow`/`deny`/`scoped` per admin/operator/dev-a/dev-b) and needs a body builder in its `bodies` map plus a `destructiveIfAllowed` entry for new mutating ops. Cluster-target postflight (`target/cluster/k8s.go` `postflight`) sweeps RayClusters, pods and NetworkPolicies by run prefix; it does not yet know RayJobs or RayServices.

---

## 1. Work packages

Naming: branches `feat/<pkg>` where `<pkg>` is the short id below.

### A. `contract-seams` — the one contract change, stubs, and the store/type seams (bifrost, serialized, merges first)

Two PRs from one branch lineage so review stays tractable: **A1** (contract + stubs + tests-that-walk-the-contract), then **A2** (store + domain types + config seams, branched from A1). Nothing downstream starts until A2 merges.

**Goal.** Add every new operation, schema and field to `internal/api/openapi.json` at once; regenerate `internal/api/zz_generated_api.go` and `pkg/client/zz_generated_client.go`; add 501 stubs that still run authorization; add `permissions.yaml` rows; add all new `Store` methods (with real CRUD implementations in the three backends and conformance scenarios, but no reconciler/handler behaviour), new `core` types, `provision` interfaces, `app.Config`/`controller.Options` fields and `serve` flags that are parsed but inert.

**A1 files.**
- `internal/api/openapi.json` — additions below.
- `internal/api/zz_generated_api.go`, `pkg/client/zz_generated_client.go` — `go generate ./internal/api/... ./pkg/client/...`.
- `internal/api/stubs_seams.go` (new) — `SubmitJob`, `GetJob`, `DeleteJob`, `ListProfiles`: call `Authorize`/`AuthorizeScoped` exactly as the final handler will (see per-package "authorization rule"), then `return nil, ErrNotImplemented` (`internal/api/errors.go:54`). This keeps `TestPermissionMatrix` honest (501 is "not 401/403", so `allow` passes; `deny` and `scoped` need the real check).
- `internal/api/services.go` `serviceView` — populate the new required `ServiceView.project` with `""` for now (A2 replaces).
- `test/requirements/r03_rbac/permissions.yaml` — rows: `submit_job: {admin: allow, operator: deny, dev-a: scoped, dev-b: scoped}`, `get_job: {admin: allow, operator: allow, dev-a: scoped, dev-b: scoped}`, `delete_job: {admin: allow, operator: deny, dev-a: scoped, dev-b: scoped}`, `list_profiles: {admin: allow, operator: allow, dev-a: allow, dev-b: allow}`. Also change `deploy_service`/`get_service`/`delete_service` to `scoped` for dev-a/dev-b (this is the #2 rule; the stubbed-then-real handlers implement it in package C/D — leave as `allow` in A1 and flip in D if you prefer zero behaviour drift in A1).
- `test/requirements/r03_rbac/matrix_test.go` — `bodies["submit_job"]` builder (project team-a, entrypoint `python -c 1`, image `fixture.RayImage()`), `destructiveIfAllowed["submit_job"] = true`; `fill` maps `{id}` for `/api/v1/jobs/{id}` to a run-prefixed id (404 counts as denied for dev-b; for dev-a a 404 is "not 401/403", fine).
- `test/requirements/contract/spec.go` `FillPath` already covers `{id}`; no change.
- `docs/requirements/traceability.*` — regenerate (`make report`); the matrix does not change rows but the test lists may.

**Contract additions (exact).** All new properties optional unless stated; nullability uses the file's existing `"type": ["string","null"]` idiom; enums as `type: string, enum: [...]`.

Schemas (components):
```
RayJobSpec:            required [project, entrypoint, image]
  project string; entrypoint string; image string; ray_version string (default from image tag, may be "")
  runtime_env_yaml string ("" = none); head_cpu string; head_memory string  (default "1"/"2Gi")
  worker_groups WorkerGroup[]; profile string|null; storage string[]  (catalog names, #12)
  ttl_seconds_after_finished int32 (min 0; default 60)
SubmitJob:             required [spec]; id string|null (RFC1123, server generates "job-<8hex>" when null); spec RayJobSpec
RayJobView:            required [id, project, status, deployment_status, submitted_at]
  id, project string; owner string|null; status string (Ray: PENDING|RUNNING|SUCCEEDED|FAILED|STOPPED|"")
  deployment_status string (KubeRay: Initializing|Running|Complete|Failed|...); cluster string|null (RayCluster name)
  gateway_url string|null; message string|null; submitted_at int64; started_at/finished_at int64|null; queue string|null
ProfileSpec:           required [name, image, ray_version, head_cpu, head_memory, worker_groups]
  name; description string|null; image; ray_version; head_cpu; head_memory; worker_groups WorkerGroup[]
  max_workers int32|null; ttl_seconds int64|null; idle_timeout_secs int64|null; projects string[] ([] = every project)
AdmissionRule:         allowed_images string[]; max_workers int32   (both optional; zero = unrestricted)
StorageEntry:          required [name, secret_name, mode]
  name; secret_name; mode string enum [env, file]; mount_path string|null (file only); projects string[] ([] = every project)
PoolPurpose:           string enum [compute, serving]
```
Field additions:
```
ClusterSpec:   profile string|null; storage string[]
ServiceSpec:   storage string[]
ClusterView:   owner string|null; gateway_url string|null; queue string|null
ServiceView:   project string (REQUIRED); owner string|null; gateway_url string|null; queue string|null
PoolSpec / PoolView:  purpose PoolPurpose (absent = compute)
PolicyView:    profiles ProfileSpec[]; admission {"*"|project -> AdmissionRule}; storage StorageEntry[]
UpdatePolicy:  profiles ProfileSpec[]|null; admission object|null; storage StorageEntry[]|null   (section-replace semantics, same as prices)
UsageGroup:    owner string ("" = unattributed)
RegistryEntryView: source string enum [static, dynamic]; target string enum [jobs, serve]
```
Operations:
```
POST   /api/v1/jobs           submit_job     body SubmitJob -> 201 RayJobView | 400 | 401 | 403 | 409 (quota) | 502
GET    /api/v1/jobs/{id}      get_job        -> 200 RayJobView | 401 | 403 | 404
DELETE /api/v1/jobs/{id}      delete_job     -> 202 | 401 | 403 | 404          (stop + tear down; ?purge=true like clusters)
GET    /api/v1/profiles       list_profiles  -> 200 ProfileSpec[] (filtered to caller's projects) | 401 | 403
GET    /api/v1/usage          usage_report   + query param owner string|null
```
Bump `info.version` to `0.2.0` and add `RayJobSpec`/`ProfileSpec` to `tags`. Expect `TestEveryRequiredFieldIsEnforced` to grow from 18 to about 24 cases (floor is 15).

**A2 files (seams, no behaviour).**
- `internal/core/rayjob.go` (new): `RayJobSpec` (mirrors the schema; `Owner *string` server-stamped like `ClusterSpec.Owner`), `RayJobState` (`pending|running|succeeded|failed|stopped`), `Marshal/UnmarshalJSON` per the package's nil-slice convention.
- `internal/core/profile.go` (new): `Profile`, `AdmissionRule`, `StorageEntry`, `StorageMode`, `PoolPurpose` (+ `DefaultPoolPurpose = compute`, strict `UnmarshalJSON`).
- `internal/core/cluster.go`: `ClusterSpec.Profile *string`, `ClusterSpec.Storage []string`, `ClusterSpec.StorageResolved []ResolvedStorage `json:"storage_resolved,omitempty"`` (server-computed: `{Name, SecretName, Mode, MountPath}`; persisted, never echoed because `ClusterView` carries no spec). `ServiceSpec` gets the same two fields. `PoolSpec.Purpose`.
- `internal/core/registry.go`: `ClusterEndpoint.Project string`, `Target string` (`"jobs"` default, `"serve"`), `Source string`; `ClusterRegistry` gains `mu sync.RWMutex`, `dynamic map[ClusterId]ClusterEndpoint`, methods `Upsert(ClusterEndpoint)`, `Remove(ClusterId)`, `Snapshot() []ClusterEndpoint`; `ByHostname`/`ByID` take the read lock and consult static then dynamic. Change `MarshalJSON`/`String` to pointer receivers (a value receiver would copy the mutex; `go vet copylocks`). Callers iterating `.Clusters` (`cmd/bifrost/serve.go` boot log, `internal/api/registry.go` `ListRegistry`) switch to `Snapshot()`.
- `internal/controller/store.go`: `StoredService{Name, Spec core.ServiceSpec, Owner *string, Generation, Desired DesiredState, ObservedState *core.ClusterState, ObservedURL *string, CreatedAt, TerminatedAt *uint64}`, `StoredRayJob{ID, Spec core.RayJobSpec, Owner *string, Desired, Status string, DeploymentStatus string, ClusterName *string, DashboardURL *string, Message *string, SubmittedAt, StartedAt, FinishedAt *uint64, FailureCount, NextAttemptAt}`, `UsageSample.Owner string`, `StoredPolicy.{Profiles []core.Profile, Admission map[string]core.AdmissionRule, Storage []core.StorageEntry}`. Interface methods: `UpsertService/GetService/ListServices/SetServiceDesired/RecordServiceObservation/RemoveService`, `UpsertRayJob/GetRayJob/ListRayJobs/SetRayJobDesired/RecordRayJobObservation/RecordRayJobAttempt/RemoveRayJob`, `UsageSamples(ctx, project, pool, owner *string, from, to)` (signature change; two callers: `internal/api/usage.go`, `internal/api/clusters.go windowedConsumption`).
- `internal/controller/store_memory.go`, `store_sqlite.go`, `store_sqlite_schema.go`, `store_postgres.go`: tables `services`, `ray_jobs`; `usage_samples.owner TEXT NOT NULL DEFAULT ''` (additive `ALTER TABLE ... ADD COLUMN` guarded, both SQL backends — this is the first real migration; put it where `store_sqlite_schema.go`'s comment says migrations belong); policy JSON gains the three fields (no schema change).
- `internal/controller/storetest/conformance.go`: `runServiceConformance`, `runRayJobConformance`, usage owner filter/round-trip in `runUsageConformance`, policy fields in `runPolicyConformance`, new List methods in `runNonNilEmptyConformance`.
- `internal/provision/provisioner.go`: `JobProvisioner` interface (`ApplyJob(ctx, id core.ClusterId, spec *core.RayJobSpec, generation uint64, queue *QueueAssignment) error`, `ObserveJob(ctx, id) (ObservedJob, error)`, `DeleteJob(ctx, id) error`, `ListJobs(ctx) ([]ObservedJob, error)`), `ObservedJob{ID, JobStatus, DeploymentStatus string, ClusterName, DashboardURL, Message *string, StartTime, EndTime *uint64}`; `ObservedService.Project`. No k8s types (r17 guard).
- `internal/controller/registrar.go` (new): `type Registrar interface { Register(core.ClusterEndpoint); Deregister(core.ClusterId) }`; `*core.ClusterRegistry` satisfies it via `Upsert`/`Remove`. `controller.Options.Registrar`, `Options.GatewayHostname func(core.ClusterId) string`, `Options.JobProvisioner`.
- `internal/app/app.go`: `Config.JobProvisioner provision.JobProvisioner`, `Config.GatewayDomain string`, `Config.GatewayExternalBase string` (e.g. `https://`), `Config.ServicesPerProject int`; pass through to `controller.Options` and `api.Server` (new `Server.GatewayDomain`, `Server.GatewayExternalBase`, `Server.ServicesPerProject`, `Server.JobProvisioner`).
- `cmd/bifrost/serve.go`: flags `--gateway-domain`, `--gateway-external-base`, `--services-per-project` (default 1), wired into `app.Config`; `buildServer` sets `cfg.JobProvisioner = live.NewJobClient(c)` once package B adds it (A2 leaves nil).
- `test/requirements/target/inproc/inproc.go`: `Option`s `WithGatewayDomain(string)`; default inproc target sets `GatewayDomain: "inproc.invalid"` so `Has("gateway")` can return true on L2 (authz-only assertions).
- `test/requirements/target/cluster/targets.yaml`: capability `gateway` on kind (grace only after its overlay sets the flag); tests read `REQ_GATEWAY_DOMAIN`.
- `test/requirements/target/cluster/kind/manifests.yaml`: `--gateway-domain=ray.kind.invalid`; RBAC add `rayjobs` to the `ray.io` rule and a `secrets` rule `get` (metadata-only existence check in #12); kind workflow env `REQ_GATEWAY_DOMAIN: ray.kind.invalid`.
- `.github/workflows/l3-kind.yml` — the env var above; and `postflight` in `target/cluster/k8s.go` must also sweep `rayv1.RayJobList` and `rayv1.RayServiceList` by prefix (otherwise B and C leak silently).

**Size.** A1: S–M (1 day). A2: L (2–3 days; three store backends and conformance dominate). **Risk.** A2 is on the critical path; keep it strictly additive, `make lint` and `go test -race ./...` green, and have each downstream owner review their slice before merge. The `UsageSamples` signature change touches two handlers — do it here so B/H do not both edit `usage.go`.

### K. `contract-sync` — flip the sync direction (bifrost + bifrost-api; can run in parallel with A1, merges right after A1)

**Goal.** `internal/api/openapi.json` is authoritative; bifrost-api is a downstream publish target.

**bifrost files.**
- `.github/workflows/ci.yml`: **delete the `spec-sync` job** (lines 53–80; it checks out `brandonrc/bifrost-api@main` and `diff -u`s — after the flip every bifrost-side change would fail it until the push lands). Keep the `api + client codegen drift` step in `test` (that is the lockstep gate now).
- `.github/workflows/sync-api.yml` (new), modelled on mobula's: `on: push: branches [main], paths: [internal/api/openapi.json, .github/workflows/sync-api.yml]`, `workflow_dispatch`; steps: checkout; `git clone https://x-access-token:${BIFROST_API_PUSH_TOKEN}@github.com/brandonrc/bifrost-api out`; `cp internal/api/openapi.json out/openapi.json`; regenerate `out/openapi.yaml` (`python3 -c "import json,yaml; yaml.safe_dump(json.load(open('openapi.json')), open('openapi.yaml','w'), sort_keys=False)"` — keep a YAML companion because bifrost-api's README documents one); `git diff --quiet && exit 0`; commit `chore: sync contract from bifrost@${GITHUB_SHA::7}` as `bifrost-ci`; push to `main`. Unlike mobula, a **missing token on a main push is a hard failure** (the project's "no silent skip" rule, SPEC.md "Port governance" and bifrost-api's own posture). Add `concurrency: sync-api` so two merges cannot race the push. Direct push to main rather than a PR: bifrost-api has no branch protection and its `generate.yml` triggers on `push: main` with `paths: [openapi.json, ...]`, so a direct push is what makes "Generate & publish SDKs" run; a PR would need an auto-merge step and a second token.
- `docs/adr/0006-contract-source-of-truth.md` (new): decision, the two admissible failure modes it replaces, what "spec-first" still means (oapi-codegen strict server, handler/spec drift is a compile error), and the rule "a contract change is edited in the same PR as its handler; `go generate` output is committed; `make report` regenerated". Add a "Superseded in part by ADR-0006" line at the top of `docs/adr/0002-openapi-codegen-result.md`.
- `docs/SPEC.md` "The frozen contract" section: rewrite the first paragraph — the contract is no longer frozen; `internal/api/openapi.json` is the source, bifrost-api hosts the published copy and SDK pipeline, the `spec-sync` gate is replaced by the codegen-drift step plus `sync-api.yml`. Update "Port governance" bullet "Spec-diff check against the frozen contract" accordingly.
- `internal/api/gen.go` and `pkg/client/gen.go` doc comments (they say "vendored from bifrost-api").

**bifrost-api files.**
- `README.md`: "Generated — do not hand-edit" now means "pushed by bifrost's `sync-api.yml`; edits go to `bifrost/internal/api/openapi.json`". Remove the "frozen, 47 operations" claims and the TODO about wiring a spec-sync workflow (now done).
- `.github/workflows/validate.yml`: unchanged; it lints the pushed file on `push: main`. Optional advisory job `upstream-drift` that checks out `brandonrc/bifrost@main` and diffs, `continue-on-error: true` — surfaces a missed push without gating anything.

**Human prerequisite.** Create repo secret `BIFROST_API_PUSH_TOKEN` in `brandonrc/bifrost` with `contents: write` on `brandonrc/bifrost-api` (fine-grained PAT).

**Size.** S (half a day). **Risk.** Ordering: K must merge **before** A1 (or in the same merge window), otherwise A1's merge to main turns `spec-sync` red and blocks nothing but alarms everyone; and the first `sync-api` run after A1 must succeed or bifrost-api's SDKs stay at 47 ops while bifrost-ui and bifrost-jupyter need the new ones.

### B. `rayjob` — #5 ephemeral RayJob + dynamic gateway registry

**Goal.** `POST /api/v1/jobs` creates a RayJob whose cluster is created for the job and removed after it finishes; the job's cluster is routable through the Jobs gateway while it runs; provisioned RayClusters (from #6) become routable too.

**Files.**
- `internal/provision/rayjob.go` (new, pure): `RayJobFor(id core.ClusterId, spec *core.RayJobSpec, generation uint64, queue *QueueAssignment) (*rayv1.RayJob, error)`; builds `RayClusterSpec` from `headGroupSpec`/`workerGroupSpec` via a small `ClusterSpec` view of the job (reuse `podTemplate`, `rayProbe`, labels `ManagedByLabel`, `ClusterIDLabel`, `OwnerLabel`, `GenerationAnnotation`, `QueueLabel`); `ShutdownAfterJobFinishes: true`, `TTLSecondsAfterFinished: spec.TtlSecondsAfterFinished`, `SubmissionMode: K8sJobMode`, `Entrypoint`, `RuntimeEnvYAML`; `JobStatusToState(rayv1.RayJobStatus) core.RayJobState`. Unit tests in `rayjob_test.go` (mirror `kuberay_test.go`'s style: labels, probe, queue label, fingerprint).
- `internal/provision/live/rayjob.go` (new): `JobClient struct{*Client}` (same façade trick as `ServiceClient` because `List` collides), `NewJobClient`; `ApplyJob` = SSA via `applySSA`; `ObserveJob` = `Get` RayJob → `ObservedJob` (status fields, `DashboardURL`, `RayClusterName`); `DeleteJob` = delete RayJob (KubeRay deletes the RayCluster it owns) + `deleteClusterAllow`; `ListJobs` by `ManagedByLabel`. Also `ensureClusterAllow(ctx, id, owner)` before apply so the job's pods get the per-cluster allow (the RayCluster KubeRay creates carries the RayJob's pod-template labels, and `podTemplate` stamps `ClusterIDLabel`).
- `internal/controller/job_reconcile.go` (new): `JobReconciler{store, jobs provision.JobProvisioner, registrar Registrar, hostname func}`; `ReconcileAllAt(ctx, now)`: observe → record (`RecordRayJobObservation`); desired running and observed NotFound → `ApplyJob` (intent key `job/<id>/<gen>` through the existing outbox `BeginIntent/CompleteIntent`); while `DeploymentStatus == Running` and `DashboardURL != nil` → `Register(ClusterEndpoint{Id: id, Hostname: hostname(id), ApiBaseUrl: "http://"+DashboardURL, Project, Target: "jobs", Source: "dynamic"})`; on terminal (`Complete|Failed`) → `Deregister`, side-write `store.RecordJob(core.JobRecord{Id, Cluster: *ClusterName, Submitter: owner, Status, DurationSecs, SubmittedAt})` (this closes the "job-history side-write" follow-up in SPEC.md); desired terminated → `DeleteJob`, then tombstone with the same retention sweep as clusters (`ReapTerminated` analogue). Backoff via `RecordRayJobAttempt` mirroring `backoffSecs`. `RunJobReconciler(ctx, store, jobs, opts)` started from `app.RunLoops` when `cfg.JobProvisioner != nil`.
- `internal/controller/reconcile.go`: in `reconcileOne` after observation, when `observed.ApiBaseUrl != nil` and state is running/provisioning/degraded → `r.registrar.Register(...)` with `Target: "jobs"`, `Project: c.Spec.Project`; on terminated/gone → `Deregister`. This is what makes #6 clusters routable and lets `r03`'s "reachable via gateway" be asserted. Registry rebuilds on restart from the first observation pass — no persistence needed.
- `internal/api/middleware.go` `authorizeGatewayRequest`: resolve the endpoint (already matched by `hostIsCluster`) and use `AuthorizeScoped`-equivalent logic: `Target: "jobs"` → `auth.TargetJob` with `PermitsScoped(..., endpoint.Project)`; `Target: "serve"` → `auth.TargetService`. Static entries have `Project == ""` → global check as today.
- `internal/api/rayjobs.go` (new; replaces the A1 stubs): `SubmitJob` — `AuthorizeScoped(Write, TargetJob, spec.Project)`; id default `job-<8 hex>`; `core.IsK8sName`; profile expansion and `admissionFor(project)` (from F; until F merges call `s.Admission.Check` on the derived ClusterSpec); storage resolution (from G; until G merges reject non-empty `storage` with 400 "not configured"); owner stamp; queue via `QueueAssignmentForProjectPurpose(..., compute)`; quota admission reuses `policy.ClusterDemand` over the derived spec; `UpsertRayJob`; audit `submit_job`; 201 with `rayJobView`. `GetJob` — fetch, hide out-of-scope as 404 (same rule as `GetCluster`), `AuthorizeScoped(Read, TargetJob, project)`. `DeleteJob` — scoped Write, `SetRayJobDesired(terminated)`, `?purge=true` for tombstones. `rayJobView` fills `gateway_url = GatewayExternalBase + hostname(id)` when registered.
- `internal/api/clusters.go` `clusterView`: fill `Owner`, `Queue`, and `GatewayUrl` (from `s.Registry.ByID(id)`).
- `internal/api/registry.go` `ListRegistry`: iterate `Snapshot()`, emit `source`/`target`.
- `cmd/bifrost/serve.go`: `cfg.JobProvisioner = live.NewJobClient(c)`; `GatewayHostname = func(id) string { return string(id) + "." + opts.GatewayDomain }` when the domain is set.
- `test/requirements/target/inproc/fake_provisioner.go`: `fakeJobProvisioner` — Initializing → Running (with a fake `DashboardURL`) → Complete/`SUCCEEDED` over three observes; an entrypoint containing `exit 1` ends `Failed`/`FAILED`; `DeleteJob` removes.
- `test/requirements/fixture/fixture.go`: `SubmitJobBody`, `WaitJob(t, tgt, principal, id, wantStatus)`, `GatewayRequest(t, tgt, principal, host, path)` (raw HTTP with `Host` header).

**Store.** All A2 seams; no new methods.

**Authorization rule.** Jobs are "code": Developer/Admin write, Operator read, project-scoped (`permissions.yaml` rows in A1).

**Tests.** `test/requirements/r05_ephemeral_rayjob/rayjob_test.go`: flip `TestSubmitCreatesAnEphemeralCluster` to `req.Covers(t, 5, ...)` (assert 201 and `RayJobView.status`), add `TestJobCompletionRemovesItsCluster` (L2+L3: wait `SUCCEEDED`; on L3 the RayCluster named in `cluster` disappears within the lane budget; `ListClusters` never shows it), `TestFailedJobIsReportedAndCleanedUp` (entrypoint `python -c "import sys; sys.exit(1)"`), `TestJobAppearsInHistoryWithSubmitter` (`GET /api/v1/jobs` shows `submitter == "dev-a"`; also `req.Covers(t, 14, ...)`), `TestRunningJobIsReachableThroughTheGateway` (L3, `NeedsCapability(gateway)`: `GET /api/jobs/` with `Host: <id>.<domain>` → 200 as dev-a, 403 as dev-b, 401 anon). `r03_rbac`: add `TestHeadJobsApiOnlyThroughGateway` (L3: direct `:8265` from a probe pod refused — already `TestCrossOwnerHeadPortsAreUnreachable`; via gateway 200; L2 asserts the 401/403 half). `r06`: add `TestClusterViewCarriesGatewayAddress` (Covers 6: the "remote connect" caveat in SPEC row 6).

**Kind lane.** RayJob CRD ships with the KubeRay chart already installed; RBAC `rayjobs` (A2); postflight sweeps RayJobs (A2).

**Size.** L (4–5 days). **Risk.** KubeRay's K8sJobMode submitter needs the RayJob image to have `ray job submit` (rayproject/ray does). Run-time TTL: `TTLSecondsAfterFinished` deletes the RayCluster; the RayJob CR stays until Bifrost tombstones it. Registry entries for static clusters must never be overwritten by dynamic ones (`Upsert` refuses when a static hostname matches).

### C. `serve-converge` — #1 RayService deploy that converges

**Goal.** Deploy is store-backed and reconciled; observed state is read from KubeRay (conditions and legacy status); the Serve endpoint is reachable through the gateway with a token.

**Files.**
- `internal/api/services.go` (rewrite): `DeployService` — `AuthorizeScoped(Write, TargetService, spec.Project)`; storage resolution (G); owner stamp; `UpsertService` (generation bump on spec change: add `serviceSpecChanged` in `store.go` next to `specChanged`); one-per-project check (D); audit; keep **202** (the contract's status; fix the two tests to accept 202 — see below). `GetService`/`ListServices` read the store row, apply `readScope` narrowing (404 for out-of-scope), merge the last observation (`ObservedState`, `ObservedURL`), fill `gateway_url` from the registry (`Target: "serve"`). `DeleteService` → `SetServiceDesired(terminated)`. No `ServiceProvisioner` nil-check on the API path any more (the reconciler owns actuation).
- `internal/controller/service_reconcile.go` (new): `ServiceReconciler` — observe (`ServiceProvisioner.Get`), record, `Deploy` when desired running and (not observed or generation behind — stamp `GenerationAnnotation` on the RayService in `RayServiceFor` and read it back in `Get`), `Delete` when desired terminated, register `ClusterEndpoint{Id: name, Hostname: name+"."+domain, ApiBaseUrl: *ObservedURL, Project, Target: "serve", Source: "dynamic"}` when running, deregister otherwise. Started from `app.RunLoops` when `cfg.ServiceProvisioner != nil`.
- `internal/provision/kuberay.go`: `RayServiceFor(name, spec, generation, queue *QueueAssignment, storage []ResolvedStorage)` — stamp generation annotation, `QueueLabel` (from E), storage mounts (from G); `ServiceStatusToState` — prefer `Conditions[type=RayServiceReady].Status==True` → running, `UpgradeInProgress` → updating, else legacy `ServiceStatus` fallback (KubeRay 1.7 populates both; the deprecation comment already flags this).
- `internal/provision/live/client.go` `ServiceClient.Get/List`: fill `Project` from a `bifrost.dev/project` label stamped by `RayServiceFor` (add `ProjectLabel` constant).
- `internal/api/gateway.go`/`gateway_ws.go`: no change for HTTP proxying — a serve endpoint proxies `http://<name>-serve-svc.<ns>.svc:8000` exactly like a dashboard; the only difference is authorization (B's `authorizeGatewayRequest` `Target: "serve"`). Serve traffic has no southbound token (Ray Serve has none); `AuthToken == nil` path exists already.
- `test/requirements/target/inproc/fake_provisioner.go`: `fakeServiceProvisioner` — provisioning → running with `Url` on the second `Get`.
- `test/requirements/target/cluster/kind/manifests.yaml`: nothing new beyond A2; the serve app fixture (below).

**Tests.** `r01_serve_from_jupyter/serve_test.go`: flip `TestDeployServiceConvergesToServing` to Covers; accept **202** (contract) instead of 201; make the serve config a real app. The head has no egress, so the L3 fixture must be in-image or mounted: recommended fixture is a run-labelled ConfigMap `serve-app` with a 6-line `app.py` (`@serve.deployment` returning "ok"), mounted through the #12 catalog as a `file` entry (`mode: file`, `mount_path: /opt/serve`) with `runtime_env: {env_vars: {PYTHONPATH: /opt/serve}}` in `serve_config_v2`; if G lands later, allow egress for serving pods behind a flag (decision D6). Add `TestServeEndpointAnswersThroughTheGateway` (L3, `NeedsCapability(gateway)`: `GET /` with `Host: <name>.<domain>` → 200 "ok" as dev-a; 401 anon). `r02` shares this fixture. Also fix `fixture.WaitObserved`-style helper for services (`fixture.WaitService`).

**Kind lane.** RayService CRD present; ingress to :8000 from control-plane pods already allowed. Budget: a RayService takes 2–4 minutes on the 4-vCPU runner; `REQ_WORKER_REPLICAS=0` applies (`worker_replicas: 0`).

**Size.** M–L (3–4 days). **Risk.** RayService readiness semantics on KubeRay 1.7 (verify `RayServiceReady` appears); the test fixture needs G (file mounts) or D6.

### D. `group-serving` — #2 project-scoped services, one service per group

**Goal.** Only members of the owning project can see/call a service; a project has one shared RayService; every serve request is authenticated.

**Files.** Small, layered on C: `internal/api/services.go` — one-per-project rule in `DeployService`: if `ListServices` has a live (desired running) row for the same project with a different name → `409 conflict("project %s already has service %s; redeploy it or delete it first")`, unless `s.ServicesPerProject > 1` allows more; same name = update (generation bump → canary/in-place as KubeRay handles). `internal/api/middleware.go`: serve-host requests require Read on `TargetService` scoped to `endpoint.Project` (B introduces the field; D adds the `serve` branch if B has not). `test/requirements/r03_rbac/permissions.yaml`: `deploy_service`/`get_service`/`delete_service` → `scoped` for dev-a/dev-b; `matrix_test.go` `serviceBody` already uses team-a.

**Authorization rule.** `AuthorizeScoped(action, auth.TargetService, row.Spec.Project)`; out-of-scope reads answer 404 like clusters.

**Tests.** `r02_group_serving/group_test.go`: flip `TestNonMemberCannotReadAnotherGroupsService` to Covers (accept 202); add `TestAnonymousServeRequestIs401AndNonMemberIs403` (gateway host, L2 for 401/403 since they precede proxying; L3 for the 200), `TestSecondServiceInSameProjectIsRefusedUntilFirstIsGone` (L2: 409, delete, wait 404, redeploy 202), `TestRedeploySameNameIsAnUpdate` (L2: generation bumps, no 409).

**Size.** S–M (1–2 days). **Risk.** None beyond C.

### E. `serving-pool` — #4 serving in a separate resource pool

**Goal.** A pool is declared `purpose: serving`; services are admitted to serving pools and clusters/jobs to compute pools; compute cannot consume serving quota.

**Files.**
- `internal/core/pool.go`: `PoolSpec.Purpose` validation (`Validate`), `poolSpecEqual` in `controller/store.go` compares it.
- `internal/provision/kueue.go` `LocalQueueFor(alloc, purpose)`: name `alloc.Project` for compute (unchanged, no migration) and `alloc.Project+"-serving"` for serving; `ApplyPool`/`DeletePool` in `live/client.go` pass the purpose.
- `internal/controller/reconcile.go`: `queueAssignmentForProject` → `QueueAssignmentForProjectPurpose(ctx, store, project, core.PoolPurpose)`; the cluster reconciler and `CreateCluster`/`lifecycleCommand`/`Meter.poolFor` call it with `compute`; C's service reconciler and `DeployService` call it with `serving`. `QueueAssignment.QueueName` for serving = `<project>-serving`.
- `internal/api/pools.go`: `poolSpecFromWire`/`poolView` carry `purpose`.
- `internal/api/services.go` (C): quota admission for services against `cfg.Quotas[project]` **is not** the right ledger (that is compute); add `policy.ServiceDemand(*core.ServiceSpec)` and admit against the serving pool's allocation `nominal` (the allocation's `nominal` map is already stored; use it as the per-project serving limit — `policy.AdmitQuota(project, limitFromNominal, inUseServices, requested)`). Compute clusters never read serving allocations, which is the L2 "compute cannot consume serving quota" property.
- `internal/provision/kuberay.go` `RayServiceFor`: stamp `QueueLabel` with the serving queue (KubeRay copies RayService labels onto the RayCluster it creates; whether Kueue then admits it is decision D5 — the label is inert if not).
- `internal/api/clusters.go`/`services.go`: `queue` in views.

**Tests.** `r04_serving_pool/pool_test.go`: replace `TestServingHasItsOwnPool` body (flip to Covers): admin creates pool `purpose: serving` + allocation for team-a; `ListPools` shows `"purpose":"serving"`; dev-a deploys a service → `ServiceView.queue == "team-a-serving"`; dev-a creates a cluster → `ClusterView.queue` is null (no compute allocation) — this is the "admitted to serving, not compute" half. Add `TestComputeClusterCannotConsumeServingQuota` (L2: serving allocation nominal cpu 2; a 2-CPU service admitted; a second service 409; a compute cluster of 4 CPU still 201 because it never touches the serving ledger; a compute quota via `update_policy` still refuses compute over-commit). L3: `TestServingLocalQueueExists` (`kueue LocalQueue team-a-serving` present in the namespace).

**Kind lane.** Kueue 0.19.2 installed; LocalQueue CRUD RBAC exists.

**Size.** M (2–3 days). **Risk.** Kueue admission of RayService-owned RayClusters is unverified (D5); the plan makes the Bifrost-side ledger the tested property and leaves Kueue queueing of serving as observed-not-required.

### F. `profiles` — #7 profile catalog and per-project admission

**Goal.** Administrators define named profiles (image, head/worker shapes, max workers, project scope) users pick by name; image allowlists and worker caps become per-project.

**Files.**
- `internal/api/settings.go`: `PolicyConfig.{Profiles, Admission}` seed from flags (`--allowed-images`/`--max-workers` become the `"*"` `AdmissionRule` seed, so existing deployments and `r07`'s current tests are unchanged); `seedFromConfig`/`configFromStored`/`policyView`/`UpdatePolicy` handle `profiles`/`admission` with section-replace semantics; validate profiles as a unit at PUT (unique names, quantities parse via `policy.ClusterDemand` on a synthetic spec, `projects` non-empty strings) — mobula's "validate at the edit, not as a 403 on every create" rule.
- `internal/api/admission.go`: `func (s *Server) admissionFor(ctx, project) Admission` merges `"*"` then project (project wins; empty list = inherit). `Check` unchanged.
- `internal/api/profiles.go` (new): `ListProfiles` — `Authorize(Read, TargetCluster)`, filter by `readScope` projects (admins see all); `expandProfile(spec *core.ClusterSpec, p *core.Profile) error` — fills `Image/RayVersion/HeadCpu/HeadMemory/WorkerGroups/TtlSeconds/IdleTimeoutSecs` where the request left them zero, **400** on a non-empty conflicting field ("profile small fixes image; leave it empty or omit profile"); enforces `p.MaxWorkers`; 400 when the profile is not available to `spec.Project`.
- `internal/api/clusters.go` `CreateCluster`: after `clusterSpecFromWire`, if `spec.Profile != nil` → `expandProfile`; then the existing validation/admission order (`admissionFor(project)` instead of `s.Admission`). Same in B's `SubmitJob`.
- `cmd/bifrost/serve.go`: flags unchanged in meaning; add `--profiles` (JSON file seed, optional) mirroring how the policy seed would be loaded.
- `test/requirements/target/inproc/inproc.go`: `WithAdmission` keeps working (sets the `"*"` seed).

**Store.** Policy row fields from A2.

**Authorization rule.** `update_policy` Admin-only (unchanged); `list_profiles` Read on Cluster (Viewer+), project-filtered.

**Tests.** `r07_admin_controls/`: new `profiles_test.go`: `TestProfileSelectedByName` (admin PUT `profiles: [{name: small, projects: [team-a], image: RayImage(), ...}]`; dev-a create with `"profile":"small"` and empty shape fields → 201; `GetCluster` shows `ray_version` from the profile; dev-b → 400), `TestListProfilesShowsOnlyTheCallersProjects`, `TestProfileMaxWorkersIsEnforced`, `TestConflictingFieldWithProfileIsRefused`; `admission_test.go`: `TestPerProjectImageAllowlist` (admin PUT `admission: {"team-b": {allowed_images: ["registry.example/"]}}`; dev-b refused with `RayImage()`; dev-a fine). Policy cleanup on `t.Cleanup` as `quota_test.go` does. The existing r07 tests stay Covers. bifrost-jupyter follow-up (out of scope here): `_profiles.py` can read `GET /api/v1/profiles` instead of its config.

**Size.** M (2–3 days). **Risk.** The profile-expansion rule (D4) needs a human ruling before implementation.

### G. `private-storage` — #12 secret references reach pods, never the API

**Goal.** Administrators catalog Kubernetes Secrets as named storage entries (env or file); users reference names; pods get `envFrom.secretRef`/secret volumes; no secret value ever enters Bifrost.

**Files.**
- `internal/api/settings.go`: `storage` section in `PolicyView`/`UpdatePolicy` (validate: unique names, `mount_path` required for `file`, RFC1123 `secret_name`).
- `internal/api/storage.go` (new): `resolveStorage(ctx, project string, names []string) ([]core.ResolvedStorage, error)` — 400 for unknown names or entries not available to the project. Called from `CreateCluster`, `DeployService`, `SubmitJob`; result stored in `Spec.StorageResolved`.
- `internal/controller/store.go` `specChanged`/`serviceSpecChanged`: include `Storage`.
- `internal/provision/kuberay.go`: `podTemplate(..., storage []core.ResolvedStorage)` adds `EnvFrom: [{SecretRef: {Name}}]` for `env` and a `Volume{Secret{SecretName}}` + `VolumeMount{MountPath, ReadOnly: true}` for `file`; `RayClusterFor`/`RayServiceFor`/`RayJobFor` pass `spec.StorageResolved`; `OwnedSpecFingerprint`/`FingerprintFromRayCluster` include the projection (drift detection covers stripped mounts, as mobula's did). Unit tests in `kuberay_test.go`.
- `internal/provision/live/client.go` `Apply`: optional fail-fast — `Get` each referenced Secret as `metav1.PartialObjectMetadata` (metadata only, never `.data`) and return `ProvisionErrBackend("secret %q not found")` so the cluster surfaces a condition instead of `CreateContainerConfigError`; needs RBAC `secrets: get` (A2 added it).
- `internal/api/clusters_test.go`/`services_test.go`: assert no response body contains `secret_name` outside `PolicyView`.

**Tests.** `r12_private_storage/storage_test.go`: flip `TestClusterSpecCanReferenceStorageCredentials` to Covers (the contract now has `storage`). Add `TestStorageRefReachesPodsAsSecretRefNeverAsValue` (L3, `NeedK8s`: create a run-labelled Secret with a known random value via `K8s()`; admin PUT `storage: [{name: s3-a, secret_name: <name>, mode: env, projects: [team-a]}]`; dev-a creates with `storage: ["s3-a"]`; wait running; head pod `envFrom[0].secretRef.name == <name>`; scan every API response the test made plus `list_audit_events` for the value → absent), `TestStorageRefOutsideProjectIsRefused` (L2), `TestFileModeMountsAtPath` (L3), `TestSecretValuesNeverAppearInResponses` (L2: `PolicyView.storage` has no `value`/`data` keys; `GetCluster` carries no `storage_resolved`). Postflight already fails on leaked run-labelled objects; add Secrets to the sweep in `k8s.go`.

**Size.** M (2–3 days). **Risk.** Mount-path collisions with the image (validate `mount_path` is not `/`, `/tmp`, `/home/ray`).

### H. `usage-owner` — #14 per-user attribution

**Files.** `internal/controller/metering.go` `Meter`/`zeroSamples`: `Owner: derefOr(c.Spec.Owner, "")`; also meter running `ray_jobs` (owner = job owner, pool = compute pool) once B has merged (the `ListRayJobs` seam exists from A2, so H can meter jobs without B; the rows just stay empty until B). `internal/api/usage.go`: group key `(project, pool, owner)`, `owner` query param, `UsageGroup.Owner`; `internal/api/usage_test.go`. Metrics endpoint gains an `owner` label. `docs/SPEC.md` row 14.

**Tests.** `r14_usage/usage_test.go`: add `TestUsageReportAttributesToTheRequestingUser` (dev-a creates; a group with `owner == "dev-a"` and cpu-hours > 0), `TestUsageReportFiltersByOwner`; keep both existing Covers.

**Size.** S (1 day). **Risk.** None; depends only on A2.

### I. `dask-adapter` — #16 (LOW; plan only, build if cheap)

Cheapest honest slice: `internal/provision/dask.go` (pure, unstructured `kubernetes.dask.org/v1 DaskCluster` per mobula `dask.rs`: scheduler `:8786/:8787`, worker replicas, same labels/probes convention), `internal/provision/router.go` (`EngineRouter` implementing `Provisioner`, id→engine cache warmed by `Apply`/`List`, cold-miss probe on the DaskCluster CRD, per mobula `router.rs`), `internal/provision/live/dask.go` (unstructured client on the same `rest.Config`), `--dask` serve flag wiring `EngineRouter` as `cfg.Provisioner`. `internal/api/cluster_obs.go` already refuses jobs for `EngineDask`. Kind lane needs `helm install dask-kubernetes-operator` (adds ~1 min) and a `dask` capability in `targets.yaml`; flip `r16_dask.TestDaskClusterIsProvisioned` only with that. **Size.** L (4 days) — recommend deferring; if built, it owns disjoint files except `serve.go`/`app.go` (one flag) and can trail everything.

### J. `rename-predecessor` — mechanical wording sweep (separate PR, any time)

92 files / 369 mentions: `internal/api` 17, `internal/controller` 15, `internal/core` 11, `cmd/bifrost` 10, `internal/provision` 8, `internal/policy` 8, `internal/auth` 8, `docs/superpowers` 7, workflows 2, `docs/SPEC.md`, `docs/adr/0001`, `.golangci.yml`, `Dockerfile`, `scripts/legacy-identity-sweep.sh`, one test file. Rules: `mobula-api's clusters.rs` → `the Rust predecessor's clusters.rs`; `mobula ADR-0002` → `predecessor ADR-0002`; `mobula-api #45` → `predecessor issue #45`; `mobula's tests/store.rs` → `the predecessor's tests/store.rs`; SPEC table header "Mobula reference status" → "Predecessor reference status". **Do not** touch: `docs/superpowers/handoff/*` (dated records), real external identifiers (`realms/mobula`, `mobula-admins` Keycloak group, `localhost:32000/...` image names), `zz_generated_*.go`, `go.sum`. Read `scripts/legacy-identity-sweep.sh` first — it may already implement the sweep; extend it and add an L1 guard test (`internal/api/legacy_identity_test.go` or `test/requirements/guards_test.go`) that fails on a case-insensitive `mobula` match outside the allowlist. Regenerate nothing but `make lint`/`go test`. **Size.** S–M (1 day). Merge last-but-one (after features) to avoid conflicts, or first with a rebase burden on everyone; recommend **last**.

### P. `pack-values` — bifrost-pack (local commits only, one branch `feat/serve-flags-2026-09`)

`chart/values.yaml`: `gateway: {domain: "", externalBase: ""}`, `services: {perProject: 1}`, `dask: {enabled: false}`; `templates/deployment.yaml`: `--gateway-domain`, `--gateway-external-base`, `--services-per-project`, `--dask`; `templates/rbac.yaml`: `rayjobs` in the `ray.io` rule, `secrets` `get` (comment: metadata-only existence check), `daskclusters` when `dask.enabled`; `README.md` sections; pack tests in `bifrost/test/requirements/pack/` (`TestGatewayDomainRendersAsFlag`, `TestRBACGrantsRayJobs`) which skip without `PACK_CHART` as today. Optional NebariApp for a wildcard host is decision D1. Also mirror every flag into `test/requirements/target/cluster/kind/manifests.yaml` (A2 did the two the lane needs). **Size.** S.

---

## 2. Shared hot spots and the strategy for them

| Hot spot | Owner | Rule for everyone else |
|---|---|---|
| `internal/api/openapi.json`, both `zz_generated_*` | A1 only | never edit on a feature branch; if a field is missing, stop and ask for an A-follow-up |
| `internal/controller/store.go`, three backends, `storetest/conformance.go` | A2 only | features only *call* the seam methods |
| `internal/app/app.go`, `cmd/bifrost/serve.go` | A2 adds fields/flags; B sets `JobProvisioner`; I adds `--dask` | one-line wiring edits only, rebase before PR |
| `internal/api/clusters.go` | F (profile expansion, `admissionFor`), G (`resolveStorage` call), B (`clusterView` owner/queue/gateway_url) | each touches distinct functions/lines; merge order B → F → G; rebase |
| `internal/api/services.go` | C rewrites; D layers; E adds serving admission; G adds storage call | C merges first; D/E/G rebase onto C |
| `internal/api/settings.go` | F (profiles/admission) and G (storage) | F merges first; G rebases |
| `internal/provision/kuberay.go` | C (`RayServiceFor` signature), E (queue label), G (`podTemplate` storage) | signature change in C; E/G rebase |
| `internal/controller/reconcile.go` | B (registrar hook), E (`QueueAssignmentForProjectPurpose`) | B first |
| `internal/api/middleware.go` | B (`authorizeGatewayRequest` project/target) | D only if B has not landed |
| `test/requirements/r03_rbac/permissions.yaml`, `matrix_test.go` | A1, D | — |
| `test/requirements/target/inproc/*` | A2 options; B, C fakes | append-only files: put fakes in new files `fake_jobs.go`, `fake_services.go` |
| `test/requirements/target/cluster/k8s.go` postflight | A2 (RayJob/RayService), G (Secrets) | — |
| `docs/requirements/traceability.*` | **regenerated only by whoever merges last in each merge window**; every PR runs `make report` and commits, and rebases regenerate | never hand-edit |
| `docs/SPEC.md` | K (contract section), each feature updates only its own row; J the header | rebase conflicts are trivial (row-local) |

---

## 3. Dependencies, parallelism and merge order

```
K (contract-sync) ──┐
                    ├─► A1 (contract + stubs) ─► A2 (seams) ─► ┬─ B rayjob + dynamic registry
                    │                                          ├─ C serve-converge ─► D group-serving
                    │                                          │                    └► E serving-pool (after C for services.go)
                    │                                          ├─ F profiles
                    │                                          ├─ G private-storage (after F for settings.go; C's L3 fixture wants G)
                    │                                          ├─ H usage-owner
                    │                                          └─ I dask (optional, trails)
J rename ──────────── any time; recommended last
P pack-values ─────── after A2 (needs the flag names); local commits only
```

- **Serialized:** K → A1 → A2. K and A1 can be authored in parallel but K merges first (or same window) so `spec-sync` never goes red on main.
- **Parallel wave 1 (after A2):** B, C, F, H (disjoint files except the one-line wiring noted). P can start.
- **Parallel wave 2:** D and E after C; G after F (and C's fixture consumes G — C may merge with `TestServeEndpointAnswersThroughTheGateway` behind `NeedsCapability("serve-fixture")` until G lands, then a tiny follow-up flips it).
- **Merge order:** K, A1, A2, B, C, F, H, D, E, G, (I), P (pack, local), J.
- Each PR rebases on main before `gh pr create`, runs `make report`, and regenerates traceability; the last merger of a window resolves any traceability conflict by re-running `make report` on main.

---

## 4. Conventions for every implementer (paste into each brief)

- Branch from `main` (`18ca8e6` or later) into your own worktree; one branch per package named `feat/<pkg>` (`feat/contract-seams`, `feat/contract-sync`, `feat/rayjob`, `feat/serve-converge`, `feat/group-serving`, `feat/serving-pool`, `feat/profiles`, `feat/private-storage`, `feat/usage-owner`, `feat/dask-adapter`, `feat/rename-predecessor`; pack: `feat/serve-flags-2026-09`).
- Commit messages: imperative subject, a **why**-focused body; **no AI attribution trailers** (no `Co-Authored-By`, no "Generated with").
- `make lint` zero issues (golangci-lint v2.13.2: `exhaustive` on every state switch, `depguard`: no k8s imports in `core`/`policy`/`controller`; `apitest` only from `_test.go`).
- `CGO_ENABLED=1 go test -race ./...` green; `CGO_ENABLED=0 go build ./...` clean; coverage ratchet (`go run ./test/requirements/cmd/covreport -profile coverage.txt`) not lowered by more than 0.5 points.
- Contract edits happen **only** in package A1, in `internal/api/openapi.json`, with `go generate ./internal/api/... ./pkg/client/...` output committed; feature branches never touch the JSON or generated files. bifrost-api is never edited by hand (K's workflow pushes it).
- Requirement tests: every `Test*` under `r??_*/` calls `req.Covers` or `req.NotYetBuilt`; no `internal/` imports outside `target/inproc`; no `time.Sleep` (use `req.Eventually`); every created object is named with `req.Name(...)`; new object kinds you create must be swept by `Cleanup`/`postflight`. Flipping a marker to `req.Covers` happens in the PR that makes it pass, and only then.
- When tests change, run `make report` and commit `docs/requirements/traceability.md|json`; rebase and regenerate before opening the PR.
- New store methods get a conformance scenario in `internal/controller/storetest/conformance.go` and implementations in memory, sqlite and postgres (`BIFROST_TEST_POSTGRES_URL` locally if you can; CI has Postgres).
- Update your requirement's row in `docs/SPEC.md` (row-local edit) and add a short ADR only for a design decision (`docs/adr/000N-*.md`).
- Open the PR with `gh pr create` against `main`; **never merge**, never rewrite another agent's branch, never force-push shared branches. Pack repo has no remote: commit locally on its branch and report the commit hashes.

---

## 5. Definition of done

**Per package**
- A1: 51 operations in the contract; `go generate` clean; stubs return 501 after the correct authorization; `TestPermissionMatrix`, `TestEveryRequiredFieldIsEnforced`, `TestEveryNonPublicOperationRequiresAToken` green; L2 matrix unchanged in status.
- A2: all new `Store` methods conformance-green on three backends; `app.New` accepts the new `Config` fields; flags parse; **no behaviour change** (L2 matrix identical to A1).
- K: `spec-sync` job gone; `sync-api.yml` merged; secret provisioned; first main push after A1 lands a commit on `bifrost-api@main` and its "Generate & publish SDKs" run is green; ADR-0006 + SPEC section updated.
- B: `r05` marker flipped and all r05 tests pass on inproc and kind; `ListRegistry` shows dynamic entries; `GET /api/v1/jobs` history populated; SPEC row 5 says built.
- C: `r01` flipped; a RayService reaches `running` on kind; serve endpoint answers through the gateway on kind.
- D: `r02` flipped; permissions matrix rows scoped; one-per-project 409 tested.
- E: `r04` flipped; L2 quota separation test green; serving LocalQueue observed on kind.
- F: r07 gains the profile/per-project tests, all Covers; `GET /api/v1/profiles` project-filtered; SPEC row 7 no longer says "Profiles are not built".
- G: `r12` flipped; kind test proves `envFrom.secretRef` and value absence.
- H: r14 owner tests Covers; SPEC row 14 "per-user attribution" closed.
- I (if built): `r16` flipped on kind with the dask operator installed; else the row stays not-yet-built and SPEC says so.
- J: zero `mobula` matches outside the documented allowlist; guard test in place.
- P: chart renders every new flag; RBAC covers `rayjobs`/`secrets get`; pack tests pass with `PACK_CHART`.

**Whole build-out**
- `docs/requirements/traceability.md` on main shows rows 1, 2, 4, 5, 12 **built** (kind lane green for their L3 tests), 7 and 14 built with the new tests, 16 explicitly not-yet-built unless I shipped, 17/18 unchanged.
- `l3-kind.yml` nightly green including RayJob and RayService tests; postflight reports nothing leaked.
- bifrost-api `main` carries the new contract and has published SDKs; bifrost-ui builds against `@brandonrc/bifrost-client` with the new types (UI work itself is out of scope beyond the minimal "Submit job" button on `/jobs` and a project column on `/services`, if wanted).
- SPEC.md "The frozen contract" describes the new direction; no CI job diffs against bifrost-api.

---

## 6. Decisions that need the human

- **D1 Hostname scheme for the dynamic registry.** Proposed `<cluster-or-job-or-service-name>.<--gateway-domain>` with `--gateway-external-base` (e.g. `https://`) for the `gateway_url` fields; kind uses `ray.kind.invalid` via `Host` headers; grace needs a wildcard host on the NebariApp/HTTPRoute (pack change) — or path-based routing, which the stock `ray job submit` client cannot do (the reason routing is host-based, `core.ClusterId` doc).
- **D2 Per-cluster Ray auth token.** Not in this build: NetworkPolicy is the isolation boundary and the gateway is the only authenticated path. If wanted, a phase 2 generates a token Secret per cluster, injects `RAY_AUTH_MODE/RAY_AUTH_TOKEN` and carries the token to the registry via a new `ObservedCluster.AuthToken`; needs `secrets create`.
- **D3 Where the serving pool is declared.** Proposed: administrator creates a pool with `purpose: serving` through the API (no platform auto-provisioning; the r04 test is rewritten accordingly). Alternative: `--serving-pool` seed flag and pack value.
- **D4 Profile expansion rule.** Proposed: `ClusterSpec.profile` fills zero-valued shape fields and refuses conflicts (keeps every field required and the generated types stable). Alternative: `CreateCluster.profile` with `spec` optional (cleaner for clients, changes `Spec` to a pointer in generated code).
- **D5 Kueue admission of RayService-owned RayClusters.** Proposed: Bifrost-side serving ledger is the tested property; the `kueue.x-k8s.io/queue-name` label is stamped and Kueue queueing is observed-not-required until verified on kind.
- **D6 Serve app fixture without egress.** Proposed: ConfigMap-mounted `app.py` through the #12 file catalog. Alternative: allow egress from serving-pool pods behind a flag (weakens the tenant posture).
- **D7 Profiles/admission/storage live in the policy row** (as mobula's catalog did; editable via `PUT /settings/policy`, no restart) rather than their own tables. Confirm.
- **D8 One service per group** strictly (409) with `--services-per-project` as the escape hatch. Confirm.
- **D9 Build I (Dask) now or defer**; it costs an operator install in the kind lane.
- **D10 Secret `BIFROST_API_PUSH_TOKEN`** and whether bifrost-api should reject direct pushes from anything but the sync workflow.
- **D11 r01/r02 status code**: keep the contract's 202 for `deploy_service` and fix the tests, or change the contract to 201 in A1.

---

### Critical Files for Implementation
- `/Users/khan/openteams/bifrost/internal/api/openapi.json` (A1 — the single contract edit everything else compiles against)
- `/Users/khan/openteams/bifrost/internal/controller/store.go` (A2 — `Store` interface, `StoredService`/`StoredRayJob`/usage owner/policy fields)
- `/Users/khan/openteams/bifrost/internal/core/registry.go` (B — `ClusterRegistry` becomes concurrent and dynamic; `ClusterEndpoint.Project/Target`)
- `/Users/khan/openteams/bifrost/internal/api/services.go` (C/D/E/G — the service path rewritten from thin proxy to store-backed, project-scoped, pool-admitted)
- `/Users/khan/openteams/bifrost/.github/workflows/ci.yml` (K — remove `spec-sync`; new `.github/workflows/sync-api.yml` alongside it)

Also load-bearing: `/Users/khan/openteams/bifrost/internal/controller/reconcile.go` (registrar hook, purpose-aware queue lookup), `/Users/khan/openteams/bifrost/internal/provision/kuberay.go` (`RayServiceFor`, `podTemplate` storage, new `rayjob.go` beside it), `/Users/khan/openteams/bifrost/test/requirements/r03_rbac/permissions.yaml` (every new operation must be classified), `/Users/khan/openteams/bifrost/test/requirements/target/cluster/kind/manifests.yaml` (flags + RBAC for the kind lane).
