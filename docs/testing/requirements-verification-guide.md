# Requirements verification guide

A tester's guide to the eighteen Ray Software Pack requirements: what the
automated lanes already prove, how to prove each row by hand against a live
deployment (grace), and the caveats that remain with a concrete fix for each.

Status as of 2026-09-04, `main` at `37eaf6a` (contract 0.2.0, 51 operations). Grace lane `t6a99c1de`: 68 pass / 0 fail.
Kind lane run 33831466919: 74 pass / 0 fail / 7 skip.

## 1. How proof works

Every requirement test declares what it proves with a marker:

- `req.Covers(t, N, "reason")` — the test exercises requirement N and passing
  it is evidence for the row.
- `req.NotYetBuilt(t, N, "reason")` — the test pins a gap; it fails on purpose
  until the feature exists, and the report shows the row as not built.

`reqreport` turns the markers plus the test results into
`docs/requirements/traceability.md` (committed; CI fails if it drifts) and a
per-lane JSON. The lanes:

| Lane | Where it runs | What it can prove | How to run |
|------|---------------|-------------------|------------|
| L2 inproc | every PR and push, `make report` | API semantics, RBAC, validation, audit, policy, store behaviour. No Kubernetes. | `make report` (Go 1.25) |
| L3 kind | GitHub workflow `l3-kind.yml`, 4 shards + report job | everything L2 proves, plus provisioning, Calico NetworkPolicy, probes, restart recovery, gateway routing, RayJob and RayService convergence | `gh workflow run l3-kind.yml --ref main`, then download the `l3-report` artifact |
| L3 grace | your laptop drives it over ssh, tests run on grace | same as kind on the real microk8s stack with Keycloak SSO and the shared Ray cluster | `make test-l3 TARGET=grace` (needs Tailscale up and `ssh geraci@grace`) |

Tests that need a cluster call `req.NeedK8s` or `req.NeedsCapability(t, tgt,
"calico" | "probes" | "restart" | "gateway" | "serve-fixture")`; they skip on
L2 and on targets that lack the capability. `test/requirements/target/cluster/targets.yaml`
lists what each target has.

Reading a lane result: `.l3/out/*.out` are the raw `go test` streams;
`l3-grace.json` / `shards/*/l3.json` are the merged reports. `make report`
prints the matrix and the coverage ratchet.

## 2. Tester setup on grace

Prerequisites: Tailscale up on your machine, ssh access as `geraci@grace`,
`curl` and `jq`.

Hosts (TLS is signed by grace's self-signed nebari CA, so use `curl -k` or
trust the CA):

| What | URL |
|------|-----|
| Dashboard (bifrost-ui) | https://bifrost.100-89-230-107.sslip.io |
| API | https://bifrost-api.100-89-230-107.sslip.io |
| Keycloak (SSO) | https://grace.possum-fujita.ts.net:8443/auth/realms/nebari |
| Gateway host pattern | `<cluster-or-service>.ray.100-89-230-107.sslip.io` (in-cluster only today, see caveats) |

Get the local admin password (never paste it into a ticket):

```sh
ssh geraci@grace 'export KUBECONFIG=/var/snap/microk8s/current/credentials/client.config; \
  microk8s kubectl -n bifrost get secret bifrost-local-admin \
  -o jsonpath="{.data.BIFROST_LOCAL_ADMIN_PASSWORD}" | base64 -d; echo'
```

Shell helpers used throughout this guide:

```sh
export B=https://bifrost-api.100-89-230-107.sslip.io
login() { curl -sk "$B/api/v1/auth/login" -H 'content-type: application/json' \
  -d "{\"username\":\"$1\",\"password\":\"$2\"}" | jq -r .token; }
api() { local tok=$1; shift; curl -sk -H "Authorization: Bearer $tok" \
  -H 'content-type: application/json' "$@"; }
ADMIN=$(login admin "$ADMIN_PW")
```

Seed two tester identities. `alice` operates in project `team-a`, `bob` in
`team-b`. A local `developer` is read-only on clusters until it holds a
project-scoped `operator` assignment, which is the self-serve model.

```sh
for u in alice bob; do
  api $ADMIN -X POST $B/api/v1/auth/users \
    -d "{\"username\":\"$u\",\"password\":\"$TESTER_PW\",\"role\":\"developer\"}"
done
api $ADMIN -X PUT $B/api/v1/access/assignments/alice -d '{"role":"operator","scope":"project:team-a"}'
api $ADMIN -X PUT $B/api/v1/access/assignments/bob   -d '{"role":"operator","scope":"project:team-b"}'
ALICE=$(login alice "$TESTER_PW"); BOB=$(login bob "$TESTER_PW")
```

Kubernetes view for the "prove it in the cluster" steps:

```sh
ssh geraci@grace
export KUBECONFIG=/var/snap/microk8s/current/credentials/client.config
alias k='microk8s kubectl'
k -n bifrost get rayclusters,rayjobs,rayservices,networkpolicies
```

Canonical bodies. Use `rayproject/ray:2.56.0` unless a row says otherwise.

```sh
cluster_body() { # $1 id, $2 project
  cat <<EOF
{"id":"$1","spec":{"name":"$1","project":"$2","ray_version":"2.56.0",
 "image":"rayproject/ray:2.56.0","head_cpu":"1","head_memory":"2Gi",
 "worker_groups":[{"name":"w","cpu":"1","memory":"2Gi","min_replicas":0,"max_replicas":2,"replicas":1}],
 "idle_timeout_secs":900,"ttl_seconds":3600}}
EOF
}
```

Cleanup at the end of a session: delete every cluster, job and service you
created (`DELETE /api/v1/clusters/{id}` etc.) and disable your tester users
(`PUT /api/v1/auth/users/alice {"disabled":true}`). The automated lanes clean
up after themselves and refuse to touch objects they did not label.

## 3. Requirement by requirement

Each row lists: status, the automated tests that prove it (package under
`test/requirements/`, with the lane they need), the manual verification, and
the caveat plus fix. Test names are the `go test -run` names.

### R1 — Deploy models from within Jupyter (CRITICAL)

**Status:** Built (API + RayService reconciler). Jupyter surface is API-driven.

**Automated (`r01_serve_from_jupyter`):**
`TestDeployServiceConvergesToServing`, `TestRedeploySameNameBumpsGeneration`,
`TestDeleteServiceConvergesToTerminated` (all lanes),
`TestServeEndpointAnswersThroughTheGateway` (capability `serve-fixture`,
currently skipped on kind and grace).

**Manual:**
1. Deploy a Serve application as alice. The import path lives inside the Ray
   wheel, so the head needs no egress.
   ```sh
   api $ALICE -X POST $B/api/v1/services -d '{"name":"alice-pid","spec":{
     "name":"alice-pid","project":"team-a","ray_version":"2.56.0","image":"rayproject/ray:2.56.0",
     "serve_config_v2":"applications:\n  - name: pid\n    import_path: ray.serve._private.test_utils:get_pid_entrypoint\n    route_prefix: /\n",
     "head_cpu":"1","head_memory":"2Gi","worker_replicas":0,"worker_cpu":"1","worker_memory":"2Gi","upgrade":"in_place"}}'
   ```
   Expect `202` and a body with `state` of `provisioning` or `pending`.
2. Poll `api $ALICE $B/api/v1/services/alice-pid` until `state` is `serving`
   (about 60 to 120 s). `gateway_url` reads `https://alice-pid.ray.100-89-230-107.sslip.io`.
3. On grace: `k -n bifrost get rayservice alice-pid` exists and its head pod is
   `1/1 Running`.
4. Redeploy with the same name and a changed `worker_replicas`; expect `202`
   and the service view's generation to increase, not a second RayService.
5. From a pod inside the cluster (or the head itself):
   `curl -s -H "Host: alice-pid.ray.100-89-230-107.sslip.io" http://10.152.183.219:8484/` returns the replica pid.
6. `api $ALICE -X DELETE $B/api/v1/services/alice-pid` gives `202`; the
   RayService disappears and `GET` becomes `404` or `state: terminated`.

**Caveat and fix:** the gateway-side answer (step 5) is only asserted by hand
because neither target advertises `serve-fixture`; flip the capability in
`targets.yaml` for grace once step 5 is confirmed there so the lane covers it.
The JupyterLab extension has no "deploy" panel yet; deploy is API or UI.

### R2 — Groups share models privately (HIGH)

**Status:** Built for the API and gateway authorisation half.

**Automated (`r02_group_serving`):** `TestNonMemberCannotReadAnotherGroupsService`,
`TestRedeploySameNameIsAnUpdate`, `TestSecondServiceInSameProjectIsRefusedUntilFirstIsGone`
(all lanes), `TestAnonymousServeRequestIs401AndNonMemberIs403` (capability `gateway`).

**Manual:**
1. With `alice-pid` serving (R1), `api $BOB $B/api/v1/services/alice-pid`
   returns `403` (or `404` by narrowing); `api $BOB $B/api/v1/services` does
   not list it.
2. Through the gateway: no token gives `401`; bob's token gives `403`;
   alice's token gives `200`.
   ```sh
   curl -sk -o /dev/null -w '%{http_code}\n' -H 'Host: alice-pid.ray.100-89-230-107.sslip.io' http://10.152.183.219:8484/
   curl -sk -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $BOB"   -H 'Host: alice-pid.ray...' http://10.152.183.219:8484/
   curl -sk -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $ALICE" -H 'Host: alice-pid.ray...' http://10.152.183.219:8484/
   ```
3. A second service in `team-a` while the first exists returns `409` with
   `error: service_limit` (grace runs `services.perProject: 1`).

**Caveat and fix:** grace has no wildcard route for `*.ray.100-89-230-107.sslip.io`,
so the tests and this guide use the in-cluster Service IP with a `Host` header.
Add a wildcard HTTPRoute (or have the pack render one per service) so a browser
or notebook can hit the public hostname.

### R3 — RBAC for serving and cluster access; direct Ray ports blocked (HIGH)

**Status:** Built (OIDC + local auth, deny-by-default RBAC, Calico per-owner
NetworkPolicy, host-based gateway).

**Automated (`r03_rbac`, plus `pack`):** `TestPermissionMatrix` (role × 51
operations from `permissions.yaml`), `TestEveryNonPublicOperationRequiresAToken`,
`TestEveryRequiredFieldIsEnforced`, `TestDeveloperCannotCreateCluster`,
`TestCreateInAnotherProjectIsRefused`, `TestProjectOperatorCannotEscalateOrReadPlatformState`,
`TestPersonalAccessTokenLifecycle`, `TestLogoutInvalidatesTheSession`,
`TestDisabledUserCannotLogInAndWrongPasswordIs401` (all lanes);
`TestCNIEnforcesNetworkPolicy` (`probes`), `TestCrossOwnerHeadPortsAreUnreachable`
(`calico`), `TestHeadJobsApiOnlyThroughGateway` (`gateway`);
`TestDashboardRuntimeSSOConfig` (pack chart).

**Manual:**
1. Sign in to the dashboard with a Keycloak account in group `team-a`. The
   redirect must go to `grace.possum-fujita.ts.net:8443/auth/realms/nebari`,
   never to `localhost`. Sign in with a `mobula-admins` member and confirm
   admin pages appear.
2. `api $ALICE -X POST $B/api/v1/clusters -d "$(cluster_body a1 team-b)"` is `403`.
   `api $BOB -X DELETE $B/api/v1/clusters/alice-c1` is `403`.
3. `api $ALICE $B/api/v1/settings/policy` reads `200`; `PUT` is `403`.
   `api $ALICE $B/api/v1/auth/users` and `/api/v1/audit` are `403`.
4. Token lifecycle: `POST /api/v1/auth/tokens {"label":"t","expires_in_days":1}`
   returns `token` and `prefix`; the token authenticates on `/api/v1/identity`;
   after `DELETE /api/v1/auth/tokens/{prefix}` it is `401`.
5. Network: from a pod owned by bob (or any pod without alice's owner label)
   in the `bifrost` namespace, `curl -m 3 alice-c1-head-svc.bifrost.svc:8265` times out,
   and `ray.init("ray://alice-c1-head-svc.bifrost.svc:10001")` fails. From a
   pod labelled `bifrost.dev/owner=<alice's subject>` both succeed.
   `k -n bifrost get networkpolicy alice-c1` shows the owner selector.
6. Without a token, `curl -H 'Host: alice-c1.ray...' http://10.152.183.219:8484/api/jobs/`
   is `401`.

**Caveat and fix:** the Keycloak SPA client `bifrost-bifrost-ui-spa` needed its
audience and group mappers added by hand (nebari-operator provisions the SPA
client without them). File the operator fix; until then re-add the mappers
after any realm re-provision. The owner-pod path is proven with a probe pod,
not a real JupyterHub notebook, because the singleuser NetworkPolicy has no
egress to the `bifrost` namespace and KubeSpawner does not set the owner label
(data-science-pack change).

### R4 — Serving in its own resource pool (CRITICAL)

**Status:** Built (`purpose: compute | serving` on pools, serving ledger).

**Automated (`r04_serving_pool`):** `TestServingHasItsOwnPool`,
`TestComputeClusterCannotConsumeServingQuota` (all lanes),
`TestServingLocalQueueExists` (cluster lanes).

**Manual:**
1. `api $ADMIN $B/api/v1/pools` lists a pool whose `purpose` is `serving`
   and one whose `purpose` is `compute` (create them if a fresh install:
   `POST /api/v1/pools` with `"purpose":"serving"`).
2. Deploy a service (R1); `GET /api/v1/services/alice-pid` shows `queue`
   naming the serving pool's LocalQueue, and on grace
   `k -n bifrost get localqueue` includes it.
3. Create a compute cluster (R6); its `queue` is the compute pool's queue.
4. Exceed the serving allocation (set a small `nominal` cpu on the serving
   pool's `team-a` allocation, deploy a service that asks for more): `409`
   with `serving_quota_exceeded`.

**Caveat and fix:** Kueue only gates the RayCluster underneath a RayService;
the serving-pool quota is enforced by Bifrost's ledger, and Kueue's share is
shown by the LocalQueue existing. Enable Kueue's RayCluster integration for
serving-created clusters and add a test that an over-quota RayService pends
in Kueue.

### R5 — Jobs run as ephemeral RayJobs (CRITICAL)

**Status:** Built (API + RayJob reconciler + storage). Not surfaced in the UI yet.

**Automated (`r05_ephemeral_rayjob`, plus `pack`):** `TestSubmitCreatesAnEphemeralCluster`,
`TestJobCompletionRemovesItsCluster`, `TestFailedJobIsReportedAndCleanedUp`,
`TestJobAppearsInHistoryWithSubmitter`, `TestJobStorageReferenceIsResolvedLikeAClusters`
(all lanes), `TestRunningJobIsReachableThroughTheGateway` (`gateway`);
`TestGatewayDomainRendersAsFlag`, `TestRBACGrantsRayJobsAndSecretMetadataOnly` (pack).

**Manual:**
1. Submit:
   ```sh
   api $ALICE -X POST $B/api/v1/jobs -d '{"id":"alice-j1","spec":{"project":"team-a","image":"rayproject/ray:2.56.0",
     "ray_version":"2.56.0","entrypoint":"python -c \"import ray,time; ray.init(); time.sleep(20); print(42)\"",
     "head_cpu":"1","head_memory":"2Gi","worker_groups":[],"ttl_seconds_after_finished":60}}'
   ```
   Expect `201`.
2. On grace within ~30 s: `k -n bifrost get rayjob alice-j1` exists and a
   RayCluster named `alice-j1-*` appears.
3. `api $ALICE $B/api/v1/jobs/alice-j1` moves `Initializing → Running → SUCCEEDED`.
   `api $ALICE $B/api/v1/jobs` lists it with `submitter` equal to alice's subject.
4. Within the TTL after success the RayCluster and RayJob are gone.
5. Submit a job whose entrypoint is `exit 3`: status becomes `FAILED`, still
   listed, cluster still removed.
6. `api $BOB $B/api/v1/jobs/alice-j1` is `403`/`404`.

**Caveat and fix:** bifrost-ui and the JupyterLab extension have no job
submission form; add one (the contract op is `submit_job`). On the 4-vCPU
kind runner RayJob timing is tight; grace is the environment of record.

### R6 — Self-serve private clusters, dask-gateway style (CRITICAL)

**Status:** Built.

**Automated (`r06_self_serve`, plus `pack`):** `TestCreateConvergesAndDeleteRemovesEverything`,
`TestListShowsOnlyOwnProjectClusters`, `TestStopAnothersClusterIsRefused`,
`TestInvalidSpecIsRefusedWith400`, `TestIdleClusterIsReaped`,
`TestDeletedClusterTombstoneAndPurge`, `TestSuspendResumeByProjectOperator`,
`TestTwoClustersHaveDistinctHeadServices` (all lanes);
`TestSuspendResumeByGlobalOperator` (cluster), `TestOwnerNotebookPodConnectsToItsCluster`
(`probes`), `TestClusterViewCarriesGatewayAddress` (`gateway`);
`TestDashboardNginxResolvesUpstreamAtRequestTime` (pack).

**Manual:**
1. `api $ALICE -X POST $B/api/v1/clusters -d "$(cluster_body alice-c1 team-a)"` → `201`.
2. Poll `GET /api/v1/clusters/alice-c1`: `observed_state` becomes `running`
   in about 60 to 90 s; `gateway_url` is `https://alice-c1.ray.100-89-230-107.sslip.io`;
   `owner` is alice's subject.
3. `api $BOB $B/api/v1/clusters` does not include `alice-c1`.
4. Connect: from an owner-labelled pod, `ray.init("ray://alice-c1-head-svc.bifrost.svc:10001")`
   then `ray.cluster_resources()` shows one head and one worker. From the
   same pod submit `ray job submit --address http://alice-c1-head-svc.bifrost.svc:8265 -- python -c 'print(1)'`.
5. Suspend/resume as alice: `POST /api/v1/clusters/alice-c1/suspend` → `202`,
   pods scale to zero; `/resume` brings them back. Bob gets `403` on both.
6. Bad spec: `head_cpu: "lots"` or `min_replicas > max_replicas` → `400`.
7. Idle: create a cluster with `idle_timeout_secs: 120`, do not connect; after
   ~3 min `observed_state` is `terminated` and the RayCluster is gone.
8. Delete: `DELETE /api/v1/clusters/alice-c1` → `202`; RayCluster, head
   Service, NetworkPolicy and pods vanish; `GET` shows the tombstone until purge.

**Caveat and fix:** same as R3: real notebook connectivity needs the singleuser
NetworkPolicy egress and owner label; dashboard access through `gateway_url`
from a browser needs the wildcard route.

### R7 — Group admins control profiles, images, CPU/mem/GPU, max workers (CRITICAL)

**Status:** Built (profile catalog, per-project admission, quotas).

**Automated (`r07_admin_controls`):** `TestProfileSelectedByName`,
`TestConflictingFieldWithProfileIsRefused`, `TestListProfilesShowsOnlyTheCallersProjects`,
`TestDisallowedImageIsRefused`, `TestPerProjectImageAllowlist`, `TestWorkerCapIsEnforced`,
`TestProfileMaxWorkersIsEnforced`, `TestProjectQuotaRefusesOverCommit`,
`TestDeveloperCannotChangeAdmissionPolicy` (all lanes).

**Manual:**
1. Publish a profile for `team-a`:
   ```sh
   api $ADMIN -X PUT $B/api/v1/settings/policy -d '{"profiles":[{"name":"small","projects":["team-a"],
     "image":"rayproject/ray:2.56.0","ray_version":"2.56.0","head_cpu":"1","head_memory":"2Gi",
     "worker_groups":[{"name":"w","cpu":"1","memory":"2Gi","min_replicas":0,"max_replicas":2,"replicas":1}],"max_workers":2}]}'
   ```
   `api $ALICE $B/api/v1/profiles` lists `small`; bob's list does not.
2. Create with `{"id":"alice-p1","spec":{"name":"alice-p1","project":"team-a","profile":"small"}}` → `201`
   and the cluster carries the profile's image and sizes. Adding
   `"head_cpu":"4"` alongside `profile` → `400`.
3. Image allowlist: `PUT /settings/policy {"admission":{"team-a":{"allowed_images":["rayproject/"]}}}`;
   a create with `image: "docker.io/library/python:3.12"` → `400`/`403` naming the image.
4. Worker cap: `{"admission":{"team-a":{"max_workers":2}}}`; a worker group
   with `max_replicas: 5` → `400`.
5. Quota: `{"quotas":{"team-a":{"cpu":"2","memory":"4Gi"}}}`; a second running
   cluster that would exceed it → `409`.
6. `api $ALICE -X PUT $B/api/v1/settings/policy -d '{}'` → `403`.

**Caveat and fix:** policy is written by the platform admin only; there is no
delegated "group admin" who can edit just their project's section. Add a
project-scoped `policy_write` permission. GPU sizing is carried in the spec but
grace has no GPU nodes, so the GPU cap is only validated, not scheduled.

### R8 — Automatic cleanup, ownership recorded, state recovered on restart (CRITICAL)

**Status:** Built.

**Automated (`r08_cleanup`, plus `pack`):** `TestClusterIdCanBeReusedAfterDelete`
(all lanes); `TestTTLReaperTerminates`, `TestOwnershipIsRecordedOnKubernetesObjects`,
`TestOutOfBandCRDeletionIsReprovisioned` (cluster); `TestRecordSurvivesControlPlaneRestart`,
`TestDeleteAcceptedThenRestartStillReaps` (`restart`); `TestImageTagIsRequired`,
`TestDashboardNginxResolvesUpstreamAtRequestTime` (pack).

**Manual:**
1. Ownership: `k -n bifrost get raycluster alice-c1 -o jsonpath='{.metadata.labels}'`
   shows `bifrost.dev/owner` and `bifrost.dev/project`; the head Service and
   NetworkPolicy carry the same labels.
2. Restart survival: `k -n bifrost rollout restart deploy/bifrost`; when the
   new pod is `1/1`, `GET /api/v1/clusters/alice-c1` still returns the record
   and the cluster is untouched.
3. Delete during outage: `DELETE /api/v1/clusters/alice-c1` → `202`, then
   immediately `k -n bifrost rollout restart deploy/bifrost`. After the
   restart the RayCluster is reaped anyway.
4. Out-of-band deletion: `k -n bifrost delete raycluster alice-c2` for a
   running cluster; within a minute the reconciler recreates it and the API
   shows `condition` explaining the reprovision.
5. TTL: create with `ttl_seconds: 180`; after ~4 min it is `terminated` and gone.
6. Re-create a deleted id: `POST` with the same id → `201` and it provisions
   (no zombie record).

**Caveat and fix:** "gateway failure" is simulated as a control-plane pod
restart. A node-loss or database-loss drill on grace is still owed; run one
before a production claim.

### R9 — Start/stop clusters from JupyterLab (CRITICAL)

**Status:** Built in the `bifrost-jupyter` extension; not covered by the
requirement framework (no automated test).

**Manual:**
1. In a JupyterHub singleuser server with the extension installed, open the
   Bifrost panel; it lists clusters in the user's project.
2. Start a cluster from the panel; the API audit trail (`GET /api/v1/audit`
   as admin) shows `create_cluster` with the user's subject.
3. Stop, suspend, resume from the panel; each maps to the corresponding
   operation and the panel status follows `observed_state`.

**Caveat and fix:** grace's JupyterHub image does not ship the extension and
its network policy blocks the API, so this row is verified against a local
JupyterLab pointed at grace's API with a personal access token. Add a
Playwright spec in `grace-e2e` once the data-science-pack image includes the
extension.

### R10 — Use nebi environments on the cluster (CRITICAL)

**Status:** Provisioner side cleared (any Ray image runs, wget or not);
blocked externally on nebi + Artifact Keeper publishing Ray images.

**Automated (`r10_nebi_envs`):** `TestImageWithoutWgetReachesRunning` (cluster;
uses `REQ_NOWGET_RAY_IMAGE`).

**Manual:**
1. Create a cluster with the checkmaite Ray image (no wget in it). It reaches
   `running` in ~80 s and the head pod shows `0` restarts after 15 min
   (`k -n bifrost get pod -l ray.io/cluster=<id>`). The liveness probe is a
   Python probe, visible in `k describe pod`.
2. When a nebi-built Ray image exists: add its prefix to the project's
   `allowed_images`, create a cluster with it, and run a job that imports a
   package only that environment provides.

**Caveat and fix:** the external half needs nebi to publish Ray-compatible
images into Artifact Keeper. Once one exists, set `REQ_NOWGET_RAY_IMAGE` to it
in the grace lane so the row is proven with a real environment. The old
`team-b-scoring` cluster on grace still crash-loops on KubeRay's default wget
probe because it predates the fix; delete and recreate it.

### R11 — Pass environment variables to the cluster (CRITICAL)

**Status:** Built in the extension (`runtime_env.env_vars`); API path is
`runtime_env_yaml` on jobs. No requirement test yet.

**Manual:**
1. Submit a job with
   `"runtime_env_yaml":"env_vars:\n  GREETING: hello\n"` and entrypoint
   `python -c "import os; print(os.environ['GREETING'])"`.
2. Job succeeds; `GET /api/v1/clusters/<job cluster>/logs` or the RayJob's
   submitter pod log contains `hello`.

**Caveat and fix:** add `r11_env_vars` with exactly this scenario using the
existing `fixture.SubmitJobBody`; it needs no new product code.

### R12 — Private storage (S3 etc.) from the cluster (CRITICAL)

**Status:** Built (storage catalog in the policy row; env or file projection
into every pod of a cluster, job or service).

**Automated (`r12_private_storage`, plus `pack`):** `TestStorageRefReachesPodsAsSecretRefNeverAsValue`,
`TestSecretValuesNeverAppearInResponses`, `TestUnknownStorageRefIsRefused`,
`TestStorageRefOutsideProjectIsRefused` (all lanes); `TestFileModeMountsAtPath`
(cluster); `TestJobStorageReferenceIsResolvedLikeAClusters` (r05);
`TestRBACGrantsRayJobsAndSecretMetadataOnly` (pack).

**Manual:**
1. On grace create the credentials Secret in the workload namespace:
   `k -n bifrost create secret generic team-a-s3 --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=... --from-literal=AWS_ENDPOINT_URL=...`
2. Catalog it: `PUT /settings/policy {"storage":[{"name":"team-a-s3","secret_name":"team-a-s3","mode":"env","projects":["team-a"]}]}`.
   `GET /settings/policy` shows the entry with no secret values.
3. Create a cluster or job with `"storage":["team-a-s3"]` → `201`. The head
   pod spec references the Secret by name (`envFrom.secretRef`), never inline
   values: `k -n bifrost get pod <head> -o yaml | grep -A2 secretRef`.
4. From a job on that cluster, `python -c "import boto3; print(boto3.client('s3').list_buckets())"`
   lists the bucket.
5. bob referencing `team-a-s3` → `400`/`403`; an unknown name → `400`.
6. File mode: `{"name":"team-a-cfg","secret_name":"team-a-cfg","mode":"file","mount_path":"/etc/creds"}`;
   the pod mounts it at `/etc/creds`.

**Caveat and fix:** the tenant NetworkPolicy allows egress to DNS only, so a
catalogued endpoint is unreachable until an extra egress policy exists (see
`docs/defects/2026-09-03-storage-egress-blocked-by-tenant-policy.md`; the fix
is an `egress` allowance on the storage entry rendered per cluster). The Secret
itself is created by hand with kubectl, and the lanes prove projection with
dummy secrets. A real read is proven by hand on grace against aks3
(`grace-deploy/aks3-*.yaml`, `aks3-job.sh`): a job reading
`s3://team-a/data/sample.parquet` returned 1000 rows. Give the job a worker
group; a head-only job has no CPU for Ray Data tasks.

### R13 — Group capacity via shared pools; fair queueing; admin quotas and weights (LOW)

**Status:** Built for the administrator half; fair sharing is Kueue's.

**Automated (`r13_pools`):** `TestPoolAndAllocationLifecycle` (all lanes).

**Manual:**
1. `POST /api/v1/pools` with `fair_sharing_weight: 2`, a flavor, `cohort`;
   `PUT /pools/{name}/allocations/team-a` with `nominal` cpu/memory;
   `GET /pools/{name}/usage` returns usage. Developer calls → `403`.
2. On grace `k get clusterqueue,localqueue -A` shows the pool and each
   project's LocalQueue; `k get clusterqueue <pool> -o yaml` carries the weight.
3. Contention: allocate `team-a` and `team-b` small nominal quotas in one
   cohort, create clusters in both until one pends; `GET /clusters/{id}`
   shows `condition` naming Kueue admission, and the pending one starts when
   the other is deleted.

**Caveat and fix:** step 3 is manual only. Add a two-project contention test
on grace (it needs real capacity so it stays out of kind).

### R14 — Usage visibility: who, what, how long, estimated cost (LOW)

**Status:** Built (metering producer every `--metering-interval`, owner
attribution, job metering).

**Automated (`r14_usage`):** `TestRunningClusterShowsUpInTheUsageReport`,
`TestUsageReportAttributesToTheRequestingUser`, `TestUsageReportFiltersByOwner`,
`TestUsageReportIsNotForProjectMembers`, `TestJobAppearsInHistoryWithSubmitter` (all lanes).

**Manual:**
1. With `alice-c1` running for a few minutes:
   `api $ADMIN "$B/api/v1/usage?from=$(( $(date +%s) - 3600 ))&to=$(date +%s)"`
   groups by project and lists alice's subject with a duration close to the
   cluster's age.
2. `?owner=<alice subject>` filters to her rows; `api $ALICE $B/api/v1/usage` → `403`.
3. Set prices: `PUT /settings/policy {"prices":{"cpu":0.05,"memory":0.01}}` (keys match the `resource_hours` keys in the usage report);
   after the next metering tick each usage group's `cost_usd` is non-zero and the cluster view's
   `est_min_hourly`/`est_max_hourly` are populated.

**Caveat and fix:** grace has no prices configured, so cost fields read zero.
Set prices in the grace values and add an assertion that `cost_usd > 0`.

### R15 — Cluster health and pending reasons without Kubernetes access (LOW)

**Status:** Built (events, logs, metrics, nodes, jobs operations).

**Automated (`r15_health`):** `TestObservabilityOpsOwnVsOther` (all lanes),
`TestUnschedulableClusterSurfacesTheReason` (cluster).

**Manual:**
1. Create a cluster with `head_cpu: "1000"`. `GET /clusters/{id}/events`
   contains `FailedScheduling` / `Insufficient cpu`; `observed_state` never
   reaches `running`. Delete it.
2. For a running cluster, `/events`, `/logs`, `/metrics`, `/nodes`, `/jobs`
   all answer `200` for alice and `403`/`404` for bob.

**Caveat and fix:** none blocking. The UI surfaces events and logs; nodes and
metrics are API-only until the dashboard adds panels.

### R16 — Same UX across Ray and Dask (LOW)

**Status:** Not built, deferred (plan ruling D9). The `engine` field and the
provisioner seam exist so a Dask adapter slots in.

**Automated (`r16_dask`):** `TestDaskClusterIsProvisioned` is a `NotYetBuilt`
marker; it fails by design and the matrix shows the row as not built.

**Manual:** `POST /clusters` with `"engine":"dask"` → `400`/`501` today.

**Caveat and fix:** decide whether Dask is in scope for this program. If yes,
implement a dask-kubernetes provisioner behind the seam and flip the marker
to `Covers`.

### R17 — Same UX across Kubernetes and Slurm (LOW)

**Status:** Not built; the design must not foreclose it.

**Automated (`r17_slurm`):** `TestProvisionerSeamCarriesNoKubernetesTypes`
(a compile-time guard that the provisioner interface has no Kubernetes types).

**Manual:** review `internal/provision` — the interface takes core types only.

**Caveat and fix:** nothing to verify until a Slurm target exists.

### R18 — NIST security baseline and audit evidence (LOW)

**Status:** Partial. Audit hash chain, deny-by-default, no-secret-material,
non-root read-only UBI9-micro image are built. FIPS build is not.

**Automated (`r18_baseline`):** `TestAuditChainVerifiesAfterEvents`,
`TestDeniedRequestIsAudited`, `TestAuditStatusMatchesOutcome`,
`TestNoSecretMaterialInResponses`, `TestEveryRequiredFieldIsEnforced` (all lanes);
`TestControlPlaneRunsNonRootReadOnly` (cluster).

**Manual:**
1. `api $ADMIN $B/api/v1/audit/verify` → `200` with `ok: true`, `events_checked` > 0 and `first_broken_seq: null` after a
   session of activity; every denied call above appears in `GET /audit` with
   `decision: deny`.
2. `GET /audit`, `/auth/users`, `/auth/tokens` never contain a password hash,
   token value or registry token; tokens are identified by `prefix`.
3. `k -n bifrost get pod -l app.kubernetes.io/name=bifrost -o jsonpath='{.items[0].spec.containers[0].securityContext}'`
   shows `runAsNonRoot: true`, `readOnlyRootFilesystem: true`, no capabilities.
4. Image is digest-pinned in the chart values; `k describe pod` shows the digest.

**Caveat and fix:** gateway-proxied requests are audited without an `action` or
`granted_roles`, so an auditor filtering by action misses exactly the requests
that run work (`docs/defects/2026-09-04-gateway-audit-rows-have-no-action-or-roles.md`).
Beyond that: no FIPS variant (`GOFIPS140`), no image vulnerability scan
gate in CI, and no written control mapping. Add a FIPS build target and a
Trivy/Grype job, then a short controls document mapping each test to the
NIST 800-53 control it evidences.

## 4. Consumers: who actually goes through Bifrost

A controlled path only counts if the tools people use take it. On grace:

| Consumer | State | What a tester checks |
|----------|-------|----------------------|
| checkmaite | **Wired 2026-09-04.** Its API submits over Ray's Job Submission REST API to `checkmaite-jobs.ray.100-89-230-107.sslip.io`, a Bifrost cluster in project `checkmaite`, with a Bifrost access token minted for `checkmaite-svc`. Bifrost authorizes each request in that project, audits it under that subject, and meters the cluster to it. | Submit a batch run; then as admin confirm `GET /api/v1/audit` shows rows with `subject: checkmaite-svc` and `cluster: checkmaite-jobs`, and `GET /api/v1/usage` attributes `project: checkmaite` to `owner: checkmaite-svc`. |
| JupyterLab / JupyterHub | **Not wired.** The singleuser image ships no Bifrost extension, the singleuser NetworkPolicy has no egress to the Bifrost namespace, and KubeSpawner stamps no `bifrost.dev/owner` label, so a notebook can neither call the API nor reach its own cluster. | Nothing to check yet; rows 9 and 11 stay untested until the data-science-pack ships those three changes. |
| Gateway hostnames | **Reachable.** One wildcard HTTPRoute covers `*.ray.<domain>` on nebari-gateway, and the gateway certificate carries the matching wildcards (`grace-deploy/bifrost-gateway/`). | `curl -k https://<cluster>.ray.100-89-230-107.sslip.io/api/version` is 401 without a token and 200 with one. |

The token checkmaite holds expires after 90 days, the server maximum, so it
needs re-minting quarterly: rerun `grace-deploy/bifrost-gateway/checkmaite-tenant.sh`
and restart the checkmaite API so the pod picks the new value up.

## 5. Running the lanes yourself

```sh
# L2: fast, no cluster
make report

# kind: dispatch and watch
gh workflow run l3-kind.yml --ref main
gh run watch "$(gh run list --workflow l3-kind.yml -L1 --json databaseId -q '.[0].databaseId')"
gh run download <run-id> -n l3-report   # traceability.md + merged l3.json

# grace: from a laptop on the tailnet
export BIFROST_ADMIN_PASSWORD=...        # from the secret above
make test-l3 TARGET=grace                 # writes .l3/out/*.out and l3-grace.json
```

Lane-tuning knobs live in the workflow env and the grace script:
`REQ_WORKER_REPLICAS`, `REQ_HEAD_CPU`, `REQ_HEAD_MEMORY`, `REQ_EVENTUALLY_TIMEOUT`, `REQ_POSTFLIGHT_TIMEOUT`,
`REQ_GATEWAY_DOMAIN`, `REQ_ADMISSION_DISALLOWED_IMAGE`, `REQ_ADMISSION_MAX_WORKERS`,
`REQ_NOWGET_RAY_IMAGE`, `BIFROST_URL` (`in-cluster` on grace), `BIFROST_INSECURE_TLS`.

## 6. Caveats at a glance

| Row | Caveat | Fix |
|-----|--------|-----|
| 1 | serve endpoint test skipped (no `serve-fixture` capability) | confirm on grace, flip capability in `targets.yaml` |
| 1, 5 | no deploy / submit-job UI | add panels to bifrost-ui and bifrost-jupyter |
| 2, 3, 6 | no wildcard `*.ray.<domain>` route on grace | wildcard HTTPRoute or per-object routes rendered by the pack |
| 3, 6 | real notebook cannot reach the API or its cluster | data-science-pack: egress to `bifrost` ns + `bifrost.dev/owner` label from KubeSpawner |
| 3 | Keycloak SPA mappers added by hand | nebari-operator fix |
| 4, 13 | Kueue fairness shown structurally, not under contention | contention test on grace; Kueue RayCluster integration for serving |
| 7 | no delegated group admin; GPU cap unscheduled | project-scoped policy write; GPU node for a lane |
| 8 | failure drill is a pod restart only | node-loss / DB-loss drill on grace |
| 9, 11 | extension-only, no requirement test; on grace the notebook path is not wired at all (no extension in the image, no egress to the API, no owner label) | `r11_env_vars` now (no product code); Playwright spec for 9 after the image ships the extension |
| 10 | nebi images do not exist yet | external; wire `REQ_NOWGET_RAY_IMAGE` when they do |
| 12 | tenant egress is DNS-only, storage endpoints unreachable without a hand policy; secrets created by hand | `egress` allowance on the storage entry (defect doc 2026-09-03); grace-only read test against aks3 |
| 14 | no prices on grace, `cost_usd` reads null | set prices in values; assert `cost_usd > 0` |
| 16 | Dask deferred | scope decision |
| 18 | no FIPS build, no scan gate, no control mapping | `GOFIPS140` target, Trivy job, controls doc |
| lanes | kind was red until 2026-09-04 (namespace posture ensured only on the cluster path, defect doc 2026-09-04, fixed in #30); now 74 pass / 0 fail on run 33831466919 | make the kind lane merge-blocking on PRs |
| ops | bifrost-pack has no git remote; sync token is a personal token | create the pack repo; fine-grained PAT for `BIFROST_API_PUSH_TOKEN` |
