# Handoff — the build-out: requirements 1, 2, 4, 5, 7, 12, 14 and the contract flip (2026-09-02/03)

Follows `2026-09-02-requirements-push-handoff.md`. Plan: `docs/superpowers/plans/2026-09-02-buildout-plan.md`
(rulings D1–D11 at its top). Executed as one package per branch/PR by parallel implementer agents, merged in
dependency order by the orchestrator. The agents were repeatedly cut off by the account's spend limit; where that
happened the orchestrator finished the package from the agent's worktree (noted per PR).

## What merged, in order

| PR | Package | What it did |
|---|---|---|
| #3 | K contract-sync | **Contract direction flipped:** `internal/api/openapi.json` is the source of truth; `sync-api.yml` pushes it to bifrost-api on merge to main (SDK publish follows); `spec-sync` job removed; ADR-0006. Companion bifrost-api PR #1. Verified twice: bifrost-api main carries `chore: sync contract from bifrost@…` commits automatically. |
| #4 | A1 contract + stubs | 0.2.0 contract, 51 operations (`submit_job`, `get_job`, `delete_job`, `list_profiles`), new schemas/fields, 501 stubs behind real authorization, permission-matrix rows. |
| #5 | A2 seams | Store methods for services and RayJobs on memory/SQLite/Postgres with conformance; `UsageSample.Owner`; policy row fields; dynamic, concurrency-safe `ClusterRegistry`; `JobProvisioner`/`Registrar` seams; `--gateway-domain`, `--gateway-external-base`, `--services-per-project`. No behaviour change. |
| #6 | ci | kind lane timeout 50 → 120 min (the doubled suite no longer fit). |
| #7 | H usage-owner | Samples and the usage report carry the owner; running jobs metered; `?owner=` filter. **Row 14 built.** |
| #8 | F profiles | Profile catalog and per-project admission in the policy row; `list_profiles`; `ClusterSpec.profile` fills empty shape fields (D4); `admissionFor(project)`. **Row 7 built** (profiles by name, per-project allowlists). |
| #9 | C serve-converge | `DeployService` store-backed and project-scoped; service reconcile loop; KubeRay condition-based readiness; serve endpoints registered on the gateway. **Row 1 built** with the serve-endpoint fixture caveat. |
| #11 | D group-serving | One service per project (409; `--services-per-project` escape hatch); service rows scoped in the matrix. **Row 2 built** (API half; gateway half depends on B). |
| #12 | E serving-pool | `PoolSpec.purpose` compute/serving; `<project>-serving` LocalQueue; services admitted against the serving allocation's `nominal`; compute never reads it. **Row 4 built** (D5 caveat: Kueue admission of the RayService's cluster observed, not asserted). Rebased by hand onto D. |
| #10 | B rayjob | `POST /api/v1/jobs` → KubeRay RayJob with `ShutdownAfterJobFinishes`; job reconcile loop; **dynamic gateway registry** (every managed cluster/job/service registered under `<name>.<domain>`, authorized within its project); job-history side-write. **Row 5 built.** Rebased by hand (SPEC rows). |
| #13 | G private-storage | Catalogued Secret names (env/file) resolved at create, projected as `envFrom.secretRef` / read-only Secret volumes; metadata-only existence check; fingerprint covers stripped mounts; storetest tie-order flake fixed. **Row 12 built.** Finished from the agent's worktree by the orchestrator. |
| #14 | P pack tests | Chart-side assertions for the gateway flags and RayJob/Secret RBAC (skip without `PACK_CHART`). |
| #15 | ci grace lane | grace target declares `gateway`; the lane reaches the API through the in-cluster Service so Host-routed gateway tests work. |
| #16 | job-storage | `SubmitJob` resolves `spec.storage` through the same catalog as clusters (B/G gap); `TestJobStorageReferenceIsResolvedLikeAClusters`. |
| #17 | J rename | Every "mobula" mention outside a short allowlist renamed to bifrost; guard test `internal/legacy_identity_test.go` + `scripts/legacy-identity-sweep.sh` keep it that way. |
| #18 | grace-lane fixes | `contract.Load` falls back to `GET /api/v1/openapi.json`; job-history submitter compared via `fixture.Subject`; matrix rows calibrated (`delete_assignment` needs `?role&scope`, `list_registry`/`list_roles` admin-only). |
| #19 | kind shards | l3-kind split into 4 shards (rbac-selfserve, jobs-cleanup, serving, governance) + a report job merging `shards/*/l3.json`; per-ref concurrency; 14 stale runs cancelled. |
| #20 | kind robustness | postflight gets its own budget (`REQ_POSTFLIGHT_TIMEOUT`), `REQ_EVENTUALLY_TIMEOUT` (kind 10m/6m), quota test limit `1+workers`. |
| #21 | matrix cleanup + tester guide | `TestPermissionMatrix` reaps the cluster/service/job it creates (the leak behind every "left objects behind: [] (deadline)" cascade on grace and kind); postflight reports the last complete snapshot; `docs/testing/requirements-verification-guide.md`. |

bifrost-pack (local, no remote): main fast-forwarded to `effe718` — `gateway.*`, `services.perProject`, RBAC for
`rayjobs` and a metadata-only `secrets get`, plus the SSO/runtime-config and admission/metering values from the day
before.

## Where the requirement table stands (L2 matrix on main; L3 per lane)

Rows 1, 2, 4, 5, 7, 12, 14 moved to **built** (each with its caveats in `docs/SPEC.md`). Rows 3, 6, 8, 10, 13, 15, 17,
18 unchanged from the previous handoff. Row 16 (Dask) deliberately deferred (D9), still not-yet-built. Rows 9/11 remain
adapter-fed (extension's own tests).

## Grace

Helm rev 14: `ghcr.io/brandonrc/bifrost:sha-0f34051` with `--gateway-domain=ray.100-89-230-107.sslip.io`; service and
job loops running; the dynamic registry lists the live cluster. There is **no wildcard HTTPRoute** for the gateway
domain yet: `gateway_url` fields advertise hostnames Envoy will not route until the pack/NebariApp gains one (D1
follow-up). The L3 grace lane therefore talks to the in-cluster Service (clusterIP `10.152.183.219:8484`) and sends
Host headers directly. Redeploy to the final main sha once #13 publishes.

## Follow-ups (not done)

- **J rename sweep** of the ~369 "mobula" mentions in bifrost (mechanical; last so it does not conflict).
- Wildcard gateway host on grace (NebariApp/HTTPRoute) so `gateway_url` is externally routable; then the
  browser/`ray job submit` path through the gateway.
- `submit_job` storage resolution (G landed after B; B's handler still 400s a non-empty `storage`).
- The serve-endpoint L3 fixture (`serve-fixture` capability): a ConfigMap-mounted `app.py` via the storage catalog (D6).
- Kueue admission of RayService-owned RayClusters (D5) — observe on kind, then assert.
- nebari-operator: SPA client mappers (from the SSO handoff).
- bifrost-ui: a minimal "Submit job" affordance and project column on services (out of scope for the build-out).
- Replace `BIFROST_API_PUSH_TOKEN` with a fine-grained PAT.
