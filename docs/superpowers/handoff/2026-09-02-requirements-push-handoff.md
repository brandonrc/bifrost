# Handoff — the evening requirements push and SSO (2026-09-02)

Follows `2026-09-02-l3-lane-handoff.md`. Same branch (`feat/req-framework-p0`,
draft PR #1). Everything below is on that branch unless another repo is named.

## Requirement deltas landed today

| Req | Before | Now | Where |
|---|---|---|---|
| 6 | suspend/resume demanded **global** cluster write; the project operator who created a cluster got 403 | scoped to the cluster's project like create/delete | `internal/api/clusters.go` `lifecycleCommand`; `TestSuspendResume_ProjectScopedOperator`; r06 flipped to Covers |
| 7 | quotas/budgets only; no image or worker control | `serve --allowed-images` / `--max-workers` (`api.Admission`), 400 + audit deny; pack values `admission.*` | `internal/api/admission.go`, `r07_admin_controls`, kind lane configured; **profiles still unbuilt**, controls are platform-level not per-group |
| 8 | re-creating a deleted id → 201 zombie | terminated record revives as a fresh create in all three stores | `UpsertDesired`, conformance scenario, `TestClusterIdCanBeReusedAfterDelete` |
| 10 | KubeRay's wget probes killed any image without wget every ~10 min | explicit python probes in the pod templates; **verified on grace** with the checkmaite image (80 s to running, 0 restarts) | `internal/provision/kuberay.go` `rayProbe`; r10 flipped to Covers; existing clusters migrate on their next spec-generation bump |
| 14 | reader over an empty table | `controller.Meter` records held demand per running cluster per project/pool every `--metering-interval` (1 min); usage report shows resource-hours (+cost with a price sheet) | `internal/controller/metering.go`, `r14_usage`; per-user attribution still open |

Requirement table: `docs/SPEC.md` rows 7, 10, 14 updated; the defect-2
record marked fixed.

## SSO (user hit `localhost:8090/realms/mobula` from the dashboard)

Root cause: the UI compiled its OIDC issuer and client id in at `vite build`
time, and the API on grace ran local auth only. Fixed across three repos:

- **bifrost-ui** branch `feat/runtime-sso-config` (pushed, no PR): reads
  `/config.json` at runtime before first render; `ssoClientId()`/`issuerBase()`
  chain runtime → `VITE_*` → default; every remaining "mobula" reference
  reworded. 211 tests green. Image for grace:
  `localhost:32000/bifrost-ui:sso-2d26f63`.
- **bifrost-pack** branch `feat/sso-runtime-config` (local, no remote):
  `ui.sso.*` rendered into the UI ConfigMap as `config.json`, nginx serves it
  at `/config.json`; client id derived from the operator's SPA client
  (`<ns>-<ui-nebariapp>-spa`); `auth.local.enabled` adds `--local-auth`
  beside `--auth-config`; `admission.*` and `metering.interval` values.
  README/values notes reworded, pack test `TestDashboardRuntimeSSOConfig`.
- **grace**: helm rev 12 = OIDC (issuer
  `https://grace.possum-fujita.ts.net:8443/auth/realms/nebari`, audience
  `bifrost-bifrost-ui-spa`, roles admin ← `mobula-admins`, project operator ←
  `team-a`/`team-b`) + local auth, control plane `localhost:32000/bifrost:b498ba7`,
  UI `sso-2d26f63`. Overlay: `grace-deploy/values/bifrost-sso-values.yaml`.
- **Verified**: `grace-e2e/tests/ui/bifrost-sso.spec.ts` (3/3, pushed):
  runtime config, admin SSO → admin role, alice SSO → team-a. alice's SSO
  token created a cluster in team-a (201, owner label `alice`), was refused
  in team-b (403).

**nebari-operator gap (needs an upstream fix):** `provisionSPAClient`
creates the public PKCE client with **no protocol mappers** — tokens carry
`aud: account` and no `groups`. The device-flow client gets an audience
mapper (`audience-confidential-client`); the SPA client should get the same
plus the group-membership mapper. On grace both were added by hand to client
`bifrost-bifrost-ui-spa` via the Keycloak admin API (creds:
`keycloak/nebari-realm-admin-credentials`). Re-provisioning by the operator
does not remove them, but a fresh cluster will hit this again.

## Lanes — results on the final commit (b498ba7)

- **kind (CI run 33678290320): 52 pass, 0 fail**, 3 skips = the pack template
  tests (no chart checkout until bifrost-pack has a remote).
- **grace (image `localhost:32000/bifrost:b498ba7`): 26 pass, 0 fail**, 2 skips =
  r07 (grace's deployment has no allowlist configured). Requirements 3, 6, 8,
  10, 14, 15, 18 read **built** on the real environment, including the
  wget-less checkmaite image reaching running and the usage report showing
  resource-hours.
- L2 green on every push. Kind lane: infra fixed (Kueue webhook race →
  retry apply; head-only clusters via `REQ_WORKER_REPLICAS=0`; inproc smoke
  tests bind to inproc).
- Grace lane: `make test-l3 TARGET=grace`; passes
  `REQ_NOWGET_RAY_IMAGE=localhost:32000/checkmaite-api:2.56.0-r1`. r07 skips
  on grace (deployment not configured with an allowlist yet — set
  `admission.*` in the overlay and `REQ_ADMISSION_*` in the script to enable).

## Still open (user decisions / upstream)

- bifrost-pack GitHub org, so the kind lane installs the real chart and the
  pack branches can be pushed.
- nebari-operator: mappers on the SPA client (above).
- The `mobula` wording sweep in the bifrost Go repo (~365 mentions in ~91
  files, mostly provenance comments and ADRs) — mechanical, not done.
- `team-b-scoring` on grace still runs the old wget probes (its spec
  generation never changed); re-create it to migrate, or leave it.
- Requirements 1, 2, 4, 5 (serving, RayJob) and 12 (S3) remain unbuilt; #5
  also needs the gateway's static registry to become dynamic.
- Whole-branch review; un-draft PR #1.
