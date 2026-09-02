# Grace cluster recon for §5a consumer tests (jupyter + checkmaite -> bifrost)

Read-only recon (`kubectl get/describe/logs/exec <read-only cmd>` only). Gathering facts to inform
integration test design for:
(a) jupyter pod -> bifrost creates Ray cluster -> `ray.init(address)` -> task -> stop
(b) checkmaite -> bifrost Jobs gateway -> `ray job submit`

Access: `ssh geraci@grace`, `export KUBECONFIG=/var/snap/microk8s/current/credentials/client.config`,
`microk8s helm3 list -A`.

---

## 1. `jupyter` namespace

**Pods running:**
| pod | image | labels |
|---|---|---|
| `alice-nb` | `ghcr.io/dask/dask:2024.5.0-py3.12` | `app=spike-notebook, mobula.dev/owner=alice` |
| `bob-nb` | `ghcr.io/dask/dask:2024.5.0-py3.12` | `app=spike-notebook, mobula.dev/owner=bob` |
| `hub-...` | `quay.io/nebari/nebari-data-science-pack-jupyterhub:sha-16c1922` | JupyterHub hub |
| `proxy-...` | `quay.io/jupyterhub/configurable-http-proxy:5.2.0` | CHP proxy |
| `continuous-image-puller-...` | `registry.k8s.io/pause:3.10.1` | image pre-puller |

**Important:** `alice-nb`/`bob-nb` are NOT JupyterHub-spawned singleuser pods. Their labels
(`app=spike-notebook`, `mobula.dev/owner=<user>`) don't match the `singleuser` NetworkPolicy's
podSelector (`app=jupyterhub, component=singleuser-server, release=data-science-pack`), so **no
NetworkPolicy currently applies to them at all** — they have unrestricted egress today. They appear
to be manually-created spike/test pods from an earlier prototype project called "mobula" (see §5/§7
below — `mobula`/`mobula-ui` images exist in the internal registry, but no `mobula` namespace exists
on the cluster).

Real user pods are spawned via JupyterHub + KubeSpawner (config in `jupyter/nebari-data-science-pack-hub-config`
ConfigMap, keys `00-chart-derived.py`, `01-spawner.py`, `02-jhub-apps.py`, `03-nebi-envs.py`).
Chart values (`helm3 -n jupyter get values data-science-pack`) define **profile_options.image** choices:
- `small-instance` / `medium-instance` (default profiles): `quay.io/nebari/nebari-data-science-pack-jupyterlab:sha-16c1922` — this is the **default docker-stacks-style image**, not confirmed to have Ray.
- **`ray-256` profile**: `localhost:32000/jupyter-ray:2.56.0-r3` — an image purpose-built with Ray, version-matched to the shared RayCluster's `rayVersion: 2.56.0`. This is almost certainly the profile intended for Ray consumers. Confirmed present in the in-cluster registry with tags `2.56.0`, `2.56.0-r1`, `2.56.0-r2`, `2.56.0-r3`.

**Checked `alice-nb` (dask image) directly:**
- Python 3.12.3 (conda-forge)
- `import ray` → `ModuleNotFoundError: No module named 'ray'` — **no Ray in this image**
- `pip show bifrost-jupyter` / `pip show bifrost-client` → both **not found**
- `wget` present; `curl` **not** present (`which curl wget` only returned `/usr/bin/wget`)

We could not check the `ray-256`-profile image (`jupyter-ray:2.56.0-r3`) directly — no running pod
uses it and we did not spawn one (read-only recon). Its name/version strongly suggest Ray 2.56.0 is
pre-installed, but this is unverified.

**imagePullSecrets:** none on any jupyter pod — registry pulls are anonymous (see §5).

## 2. `checkmaite` namespace

**Pods:** API (`checkmaite-nebari-checkmaite-pack-api`, image `localhost:32000/checkmaite-api:2.56.0-r2-mobula`),
UI (`checkmaite-nebari-checkmaite-pack-ui`, image `localhost:32000/checkmaite-ui:latest`), Postgres,
plus CronJob-spawned backup pods (alpine / bitnami postgresql images).

**How checkmaite currently reaches Ray today** — found in the API Deployment's env (grepped for
`ray|8265|10001|rayserve|ray-gw|RAY_ADDRESS`):
```
RAY_JOBS_NAMESPACE (or similar) = ray-jobs
CHECKMAITE_RAY_JOBS_ADDRESS      = https://ray-gw.100-89-230-107.sslip.io
CHECKMAITE_RAY_JOBS_OIDC_ISSUER  = <value, not grepped further>
CHECKMAITE_RAY_JOBS_CLIENT_ID    <- from secret checkmaite-ray-jobs-oidc
CHECKMAITE_RAY_JOBS_CLIENT_SECRET <- from secret checkmaite-ray-jobs-oidc
CHECKMAITE_RAY_JOBS_TLS_VERIFY
```
So checkmaite already talks to **some** external Jobs gateway hostname `ray-gw.100-89-230-107.sslip.io`
via OIDC client-credentials, **not bifrost's Service** (`bifrost:8484`) directly. There is no existing
NebariApp/Ingress/HTTPRoute matching `ray-gw.*` that we found (not in `checkmaite`/`bifrost`
namespaces) — this hostname may be dangling/aspirational, or served by a resource outside the
namespaces we checked. **Needs follow-up**: confirm whether `ray-gw` still resolves to anything live,
or whether checkmaite's config needs to be repointed at bifrost's Jobs gateway once it exists.

**Credentials held today (names only, no values):** Secret `checkmaite-ray-jobs-oidc` (Opaque, 2 data
keys — this is almost certainly `client_id`/`client_secret` for the OIDC client used against
`CHECKMAITE_RAY_JOBS_*`). Also `checkmaite-db-credentials` and
`checkmaite-nebari-checkmaite-pack-oidc-client` (checkmaite's own app auth, unrelated to Ray).

**NebariApp** `checkmaite-nebari-checkmaite-pack`: hostname `checkmaite.100-89-230-107.sslip.io`,
auth via Keycloak realm `nebari` (`issuerURL: http://keycloak.100-89-230-107.sslip.io/auth/realms/nebari`),
`provisionClient: true` — checkmaite has its own Keycloak client provisioned by the NebariApp
reconciler for its own UI/API auth (separate from the `checkmaite-ray-jobs-oidc` secret, which is a
second, distinct OIDC client specifically for the Ray Jobs gateway call).

## 3. Shared `ray` namespace RayCluster + NetworkPolicies

**RayCluster** `shared-nebari-rayserve-pack-qj59s` (helm release `shared`, chart
`nebari-rayserve-pack-0.5.0`): 2 workers, 5 CPU / 20Gi total, `rayVersion: 2.56.0`, image
`localhost:32000/checkmaite-api:2.56.0-r1` (same image family used for the bifrost-provisioned
`team-b-scoring` cluster below — a checkmaite-flavored Ray image, not a generic `rayproject/ray` image).

**All NetworkPolicies on the cluster** (`kubectl get networkpolicy -A`):
| namespace | policy | selects |
|---|---|---|
| bifrost | `bifrost-cluster-team-b-scoring` | `bifrost.dev/cluster-id=team-b-scoring` |
| bifrost | `bifrost-default-deny` | any pod with `bifrost.dev/cluster-id` label |
| bifrost | `bifrost-tenant-allow` | any pod with `bifrost.dev/cluster-id` label |
| checkmaite | `allow-backups-to-postgresql`, `checkmaite-...-api`, `checkmaite-...-ui`, `checkmaite-postgresql` | (checkmaite-internal) |
| jupyter | `hub`, `nebari-data-science-pack-singleuser-gateway-egress`, `proxy`, `singleuser` | hub/proxy/singleuser components |
| ray | `shared-nebari-rayserve-pack-ray-cluster`, `shared-nebari-rayserve-pack-ray-head` | ray.io labels |

**Full specs of the three `bifrost-*` policies** (§6 below).

**Critical finding — is `jupyter -> bifrost-provisioned-cluster:10001` routable today? NO, on two
independent counts:**

1. The `jupyter/singleuser` NetworkPolicy (which governs real JupyterHub-spawned singleuser pods —
   labels `app=jupyterhub, component=singleuser-server, release=data-science-pack`) has an explicit
   egress allow-list. It allows:
   - hub (8081), proxy (8000), autohttps (8080/8443) — internal jupyterhub plumbing
   - DNS (53) to kube-system + private CIDRs
   - **`ray` namespace, port 10001 only** (the *shared* rayserve cluster, not bifrost-provisioned ones)
   - **`mobula` namespace, ports 10001+8265** — but no `mobula` namespace exists on this cluster (dead/future rule)
   - **`artifact-keeper` namespace, port 8080**
   - a catch-all `0.0.0.0/0 EXCEPT 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.169.254/32` (public internet only)

   There is **no rule allowing egress to the `bifrost` namespace at all** (neither to bifrost's API
   Service on 8484, nor to a bifrost-provisioned RayCluster's head service on 10001/8265). Since pod
   IPs and Service ClusterIPs here are all within `10.0.0.0/8` (pod CIDR `10.1.213.0/24`, Service
   CIDR `10.152.183.0/24`), the public-internet catch-all rule does not cover them either. **A real
   JupyterHub singleuser pod cannot reach `bifrost:8484` or any bifrost-provisioned cluster's head
   service today** — this needs a new egress rule in the `jupyter` namespace (either added to
   `nebari-data-science-pack`'s singleuser NetworkPolicy, or bifrost could ship an equivalent policy
   allowing jupyter singleuser pods egress into the bifrost namespace).

2. Even if jupyter-side egress were fixed, entry into a bifrost-provisioned cluster is gated by
   `bifrost-cluster-<id>`, which only admits ingress from `jupyter` namespace pods carrying the label
   **`bifrost.dev/owner: admin`** (see §6 — this looks like a placeholder/example value baked in per
   cluster at provision time, presumably meant to be the actual owner's identity, not literally
   `admin`). A real singleuser pod would need to carry a matching `bifrost.dev/owner=<username>` label
   (KubeSpawner would have to inject it) for bifrost's per-cluster NetworkPolicy to admit it.

   The `bifrost-tenant-allow` policy separately admits ingress on 8265/10001 only from pods/namespaces
   labeled `bifrost.dev/control-plane: "true"`, and admits 8265/52365/8000 from any namespace's
   `kuberay-operator` pods — neither of these paths helps a jupyter consumer.

Because `alice-nb`/`bob-nb` match none of the `singleuser` policy's selectors, they are **not
currently network-restricted** and could reach bifrost/RayCluster services today if used as a stand-in
for a "jupyter pod" in a quick manual test — but that would not be representative of how a real
KubeSpawner-launched notebook pod behaves, and should not be relied on for the actual test design.

## 4. Keycloak

Pod: `keycloak-keycloakx-0` (keycloakx chart, image not grepped further; helm chart `keycloakx-7.2.3`,
app version `26.6.4`). `kcadm.sh get realms` without `--server`/credentials fails immediately
(`No server specified. Use --server, or 'kcadm.sh config credentials'.`) — **could not enumerate
realms/clients without admin credentials**, and we did not attempt to obtain or use any (out of
scope for read-only recon). Skipped per instructions.

From NebariApp specs (`kubectl -n bifrost get nebariapp -o yaml` and `checkmaite`'s NebariApp):
- Realm in use cluster-wide: `nebari` (from checkmaite's `issuerURL: http://keycloak.../auth/realms/nebari`).
- **`bifrost` NebariApp** (hostname `bifrost-api.100-89-230-107.sslip.io`, Service `bifrost:8484`):
  `auth.enabled: false`, `auth.provisionClient: true`, provider `keycloak` — client provisioning is
  configured but auth is currently **disabled** at the gateway (status condition `AuthReady=False`,
  reason `AuthDisabled`). So the bifrost API is reachable without any token today.
- **`bifrost-ui` NebariApp** (hostname `bifrost.100-89-230-107.sslip.io`, Service `bifrost-ui:80`):
  same — `auth.enabled: false`, provisionClient: true, currently unauthenticated.
- Both bifrost NebariApps have `provisionClient: true` but auth is off, so **no Keycloak client for
  bifrost/bifrost-ui is actually being enforced yet** (a client may or may not have been silently
  provisioned by the reconciler regardless of `enabled: false` — we did not check Keycloak directly
  to confirm, per the constraint above).

## 5. `artifact-keeper` namespace

Services: `artifact-keeper-backend` (8080/9090), `artifact-keeper-web` (3000), plus dtrack/opensearch/
postgres/scanner-adapter/trivy backing services, and an `ExternalName` service `backend` pointing at
`artifact-keeper-backend.artifact-keeper.svc.cluster.local`.

NebariApp `artifact-keeper-api`: hostname `artifacts.100-89-230-107.sslip.io`, routes `/api /health
/ready /v2 /pypi /npm /maven /cargo /conda /go /helm /nuget /debian /rpm` to Service
`artifact-keeper-backend:8080`. So artifact-keeper is a **full package/OCI registry proxy** (the `/v2`
prefix is the OCI Distribution API) exposed externally at `artifacts.100-89-230-107.sslip.io`.

**However, this is NOT what RayCluster images actually reference.** All RayCluster/Deployment images
we found (`checkmaite-api`, `checkmaite-ui`, `jupyter-ray`, `bifrost`, `bifrost-ui`, `mobula`,
`mobula-ui`, `nebari-operator`) use `localhost:32000/<repo>:<tag>` — this is a **separate, plain
Docker Registry v2** (image `registry:2.8.3`) running in its own `container-registry` namespace
(Service `registry`, `NodePort 5000:32000/TCP`). We queried its catalog directly (read-only GET,
no credentials) and got a clean list:
```
{"repositories":["artifact-keeper-backend","artifact-keeper-web","bifrost","bifrost-ui",
"checkmaite-api","checkmaite-ui","jupyter-ray","mobula","mobula-ui","nebari-operator"]}
```
i.e. **pulls are anonymous** — no `imagePullSecrets` are set on any pod in `bifrost` or `jupyter`
(checked both namespaces explicitly, all empty). A RayCluster spec referencing
`localhost:32000/jupyter-ray:2.56.0-r3` works because every node in this single-node microk8s cluster
resolves `localhost:32000` to the containerd-configured registry mirror for that NodePort — **this
pattern would need adjusting for a real multi-node cluster** (image ref would need to be the in-cluster
DNS name `registry.container-registry.svc:5000/...` or a real external registry, not `localhost:32000`,
which only works because kubelet and the registry NodePort happen to share the same host here).

## 6. `bifrost` namespace

**RayClusters:** one, `team-b-scoring` — 1 worker, 3 CPU / 8Gi total, image
`localhost:32000/checkmaite-api:2.56.0-r1`, `rayVersion: 2.56.0`. **Status: unhealthy** —
`team-b-scoring-head` pod is `0/1 Running` with 66 restarts in 11h, worker pod `0/1 Running` with 117
restarts in 11h (crash-looping; we did not `logs`/investigate further as it's outside the recon scope,
but flagging since a test against this specific cluster would likely fail regardless of network setup).

**Head Service** `team-b-scoring-head-svc` (ClusterIP: `None`, headless): ports
`10001/TCP` (Ray Client), `8265/TCP` (dashboard/jobs), `6379/TCP` (GCS), `8080/TCP`, `8000/TCP` (serve).

**Other bifrost pods:** `bifrost-7f6568859f-tdfl6` (control-plane API, image
`ghcr.io/brandonrc/bifrost:sha-95dcede`), two `bifrost-ui-*` pods (one apparently mid-rollout, image
`ghcr.io/brandonrc/bifrost-ui:latest`), and `bfr4` (`Completed`, image `curlimages/curl:8.10.1` — a
one-off debug/smoke-test pod, not part of the running system).

**bifrost Service:** `bifrost` ClusterIP `10.152.183.219:8484/TCP`. **bifrost-ui Service:**
`bifrost-ui` ClusterIP `10.152.183.183:80/TCP`.

**Three NetworkPolicies, full specs:**

`bifrost-default-deny` — selects any pod with a `bifrost.dev/cluster-id` label (i.e. every
bifrost-provisioned Ray head/worker pod), denies all ingress+egress by default (baseline zero-trust).
```yaml
spec:
  podSelector:
    matchExpressions:
    - {key: bifrost.dev/cluster-id, operator: Exists}
  policyTypes: [Ingress, Egress]
```

`bifrost-tenant-allow` — same podSelector (all cluster-id-labeled pods), then re-opens:
```yaml
spec:
  egress:
  - ports: [{port: 53, protocol: UDP}, {port: 53, protocol: TCP}]
    to: [{namespaceSelector: {kubernetes.io/metadata.name: kube-system}, podSelector: {k8s-app: kube-dns}}]
  ingress:
  - from:
    - podSelector: {bifrost.dev/control-plane: "true"}
    - namespaceSelector: {bifrost.dev/control-plane: "true"}
      podSelector: {bifrost.dev/control-plane: "true"}
    ports: [{port: 8265, protocol: TCP}, {port: 10001, protocol: TCP}]
  - from:
    - namespaceSelector: {}  # any namespace
      podSelector: {app.kubernetes.io/name: kuberay-operator}
    ports: [{port: 8265, protocol: TCP}, {port: 52365, protocol: TCP}, {port: 8000, protocol: TCP}]
```
i.e. only the bifrost control plane itself (labeled `bifrost.dev/control-plane=true`) and any
`kuberay-operator` pod (any namespace) can reach a provisioned cluster's dashboard/client ports by
default — **no tenant namespace (jupyter, checkmaite) is admitted by this baseline policy.**

`bifrost-cluster-team-b-scoring` (dynamically created per-cluster, generated at cluster-create time —
11h old, i.e. same age as the RayCluster itself):
```yaml
spec:
  podSelector: {bifrost.dev/cluster-id: team-b-scoring}
  egress:
  - to: [{podSelector: {bifrost.dev/cluster-id: team-b-scoring}}]   # intra-cluster head<->worker only
  ingress:
  - from: [{podSelector: {bifrost.dev/cluster-id: team-b-scoring}}]  # intra-cluster
  - from:
    - namespaceSelector: {kubernetes.io/metadata.name: jupyter}
      podSelector: {bifrost.dev/owner: admin}
    ports: [{port: 10001, protocol: TCP}, {port: 8265, protocol: TCP}]
```
This is the mechanism that's supposed to let a specific jupyter pod reach *its own* bifrost-provisioned
cluster: bifrost auto-generates one of these per RayCluster, scoped to admit only jupyter pods
carrying `bifrost.dev/owner=<the cluster's owner>` (here literally `admin`, i.e. whoever owns
`team-b-scoring` is named `admin`). **This confirms the design intent** — bifrost expects the calling
jupyter pod to carry a `bifrost.dev/owner` label matching the cluster owner. Today's KubeSpawner
config does not set any `bifrost.dev/*` labels on singleuser pods (not found in `01-spawner.py` grep,
and confirmed absent on `alice-nb`/`bob-nb`, though those aren't real singleuser pods anyway) — this
label injection would need to be added to the data-science-pack's KubeSpawner profile/hook for §5a
test (a) to work end-to-end.

## 7. Node facts

Single-node cluster (microk8s), node `grace`, `Ready`, `v1.35.6`, Ubuntu 26.04 LTS, containerd 2.1.6.

**Allocatable:** 40 CPU, ~108 GiB memory (`113191588Ki`), 110 pods, ~392 GiB ephemeral storage.

**Current allocation (46 pods total on the node):**
| resource | requests | limits |
|---|---|---|
| CPU | 12705m (31%) | 32600m (81%) |
| memory | 39950Mi (36%) | 78314Mi (70%) |
| ephemeral-storage | 1280Mi (0%) | 8Gi (2%) |

Headroom for concurrent test clusters: roughly **27 CPU / ~68 GiB memory unrequested** (limits are
already at 81% CPU / 70% memory, so **limit headroom is tighter than request headroom** — on a
single-node cluster, exceeding node capacity via limits alone won't OOM/evict as long as requests fit,
but real usage under a test load will contend). At the observed per-cluster footprint (`team-b-scoring`:
3 CPU/8Gi requested for 1 head + 1 worker), request-wise there's room for **roughly 8-9 similarly-sized
concurrent test clusters** before requests saturate the node; fewer if profiles request more per node,
and this is a **single node** so all pods (test clusters + jupyter + checkmaite + everything else)
compete for the same 40 CPUs — no horizontal scaling available for parallel test runs.

---

## Implications for §5a tests

**What's possible today:**
- The bifrost API (`bifrost:8484`) and bifrost-ui (`bifrost-ui:80`) are unauthenticated in-cluster —
  any pod that can reach them on the network can call the API without a token right now (auth is
  provisioned but disabled).
- checkmaite already has a working env-var/secret wiring pattern (`CHECKMAITE_RAY_JOBS_*` +
  `checkmaite-ray-jobs-oidc` secret) for an OIDC-secured Jobs gateway call — a template exists, but it
  currently points at `ray-gw.100-89-230-107.sslip.io`, not bifrost; needs to be repointed and
  verified once bifrost's Jobs gateway is live.
- A Ray-capable jupyter image exists and is version-matched to the shared cluster: `ray-256` profile,
  `localhost:32000/jupyter-ray:2.56.0-r3`, Ray 2.56.0. (Unverified directly — no running pod to exec
  into — but name/tag/registry match are strong signals.) The default jupyter profile images (dask,
  jupyterlab base) do **not** have Ray.
- Anonymous, working in-cluster registry (`registry.container-registry.svc:5000`, NodePort
  `localhost:32000`) already holds bifrost/checkmaite/jupyter-ray images — no imagePullSecret plumbing
  needed for test images, as long as `localhost:32000`-style refs keep working (single-node quirk, see §5).

**What's blocked outright for test (a) (jupyter -> bifrost -> ray.init):**
1. **Network:** the `jupyter/singleuser` NetworkPolicy has no egress rule to the `bifrost` namespace at
   all (neither port 8484 for the API nor 10001/8265 for a provisioned cluster). This must be added
   before a real singleuser pod can even open a TCP connection to bifrost. (The catch-all
   "public internet" egress rule explicitly excludes `10.0.0.0/8`, which covers all in-cluster pod/service
   IPs here, so it provides no help.)
2. **Identity/labels:** bifrost's per-cluster NetworkPolicy (`bifrost-cluster-<id>`) only admits jupyter
   pods labeled `bifrost.dev/owner=<owner>`. KubeSpawner does not currently set this label on singleuser
   pods. Needs a KubeSpawner hook/profile change (or bifrost needs a different admission mechanism not
   dependent on a label the jupyter chart doesn't know about).
3. **Image:** default jupyter profiles lack Ray; the test must explicitly select the `ray-256` profile
   image, and that image's actual Ray installation/version is still unverified (would need a quick spawn
   + `ray.__version__` check, which we did not do since no such pod was running and spawning one is a
   write action outside this recon's scope).
4. `curl` is absent from the (dask) image we could check; if the `ray-256` image also lacks `curl`,
   any test step that shells out to curl the bifrost API (vs. using `bifrost-client`/`requests`) needs
   `wget` or a Python HTTP client instead. No `bifrost-jupyter`/`bifrost-client` package is installed in
   the image we checked — if such a client package is expected to exist, it isn't baked into any image
   yet.

**What's blocked / needs confirmation for test (b) (checkmaite -> bifrost Jobs gateway -> ray job submit):**
1. checkmaite's `CHECKMAITE_RAY_JOBS_ADDRESS` currently points at `ray-gw.100-89-230-107.sslip.io`, a
   hostname we could not find a matching NebariApp/Ingress/HTTPRoute for in `checkmaite` or `bifrost`
   namespaces — **needs confirmation this endpoint is live and/or needs repointing at bifrost's actual
   Jobs gateway hostname once bifrost exposes one.**
2. checkmaite already holds an OIDC secret (`checkmaite-ray-jobs-oidc`) scoped for this exact purpose,
   suggesting bifrost's Jobs gateway is expected to require OIDC client-credentials auth — but bifrost's
   NebariApp currently has **`auth.enabled: false`**. Test (b) needs bifrost auth turned on (or the test
   needs to account for it being off) and confirmation that a Keycloak client matching
   `checkmaite-ray-jobs-oidc`'s client_id actually exists in the `nebari` realm (we could not check
   Keycloak directly — no admin credentials used, per read-only constraint).
3. No NetworkPolicy blocks `checkmaite` reaching `bifrost` specifically (checkmaite's own NetworkPolicies
   only constrain ingress to its own pods, not its egress), so network-wise this path is likely open —
   but this should be double-checked against the actual bifrost Jobs gateway port/Service once it's
   identified, since `bifrost-tenant-allow`'s ingress rules (8265/10001/52365/8000, gated to
   control-plane-labeled pods or kuberay-operator) don't obviously include a path for checkmaite-namespace
   callers either, if the Jobs gateway routes through a `bifrost.dev/cluster-id`-labeled pod.

**Cluster capacity:** single node, 40 CPU / ~108Gi allocatable, currently ~31%/36% requested. Room for
roughly 8-9 concurrent `team-b-scoring`-sized (3 CPU/8Gi) test clusters by request accounting, but this
is a shared single-node cluster running everything else too — plan test concurrency conservatively and
clean up clusters promptly after each test.
