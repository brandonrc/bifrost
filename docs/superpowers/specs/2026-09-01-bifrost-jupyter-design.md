# bifrost-jupyter — Design Spec (Wave 2-C)

**Status:** approved design · Wave 2 sub-project C · greenfield (no Rust oracle).
**Repo to create:** `github.com/brandonrc/bifrost-jupyter` (new).
**Requirements served:** #9 (start/stop clusters from JupyterLab), #11 (env vars),
and #6's kernel-side connect handoff (Ray address back to the notebook) — the
"dask-gateway-like UX, for Ray, fronted by Bifrost."

This design was produced by a 3-agent independent research panel that converged
unanimously (including a designated skeptic) on the architecture below. Findings
are grounded in the Bifrost source (`internal/api/gateway.go`, `gateway_ws.go`,
`middleware.go`, `internal/auth/flows.go`, `internal/provision/kuberay.go`,
`bifrost-api/openapi.json`) and current JupyterLab 4.x / jupyter-server 2.x /
Ray 2.5x / JupyterHub docs (2026).

## 1. The two load-bearing facts

1. **Bifrost has no CORS.** The Go server emits zero `Access-Control-Allow-*`
   headers and no preflight handling (verified by grep across `internal/api`).
   JupyterLab is served by jupyter-server on the Hub origin; Bifrost is a
   different origin. A browser-only labextension calling Bifrost is therefore
   blocked outright. **This makes the Python `jupyter-server` extension
   mandatory, not a preference** — it is the same-origin proxy that dodges CORS
   and keeps the bearer token server-side, never in browser JS.

2. **The control plane and the interactive plane are separate.** Bifrost's
   federating gateway is an HTTP + WebSocket reverse proxy keyed on the Host
   header; it proxies each cluster's Ray dashboard, Jobs API (HTTP 8265), and
   the WS log-tail with credential strip-and-swap. It does **not** carry Ray
   Client (`ray://:10001`, gRPC). Port 10001 is reachable only in-cluster, via a
   per-owner NetworkPolicy that pins ingress to the pod labeled
   `bifrost.dev/owner == <cluster owner>`. Consequence: everything #9/#11 and
   observability need goes through Bifrost over HTTP; the interactive
   `ray.init("ray://…")` path does not and is deployment-fragile.

## 2. Decisions (locked)

- **#11 env vars → job layer.** `ClusterSpec` has no `env`/`runtime_env` field
  (props: engine, head_cpu/memory, idle_timeout_secs, image, name, owner,
  project, ray_version, ttl_seconds, worker_groups). Env vars attach to the Ray
  Job's `runtime_env.env_vars` at submit time. **No Bifrost backend change, no
  contract bump** — Wave 2-C is a pure client-side deliverable.
- **#6 connect → Ray Jobs API first.** The kernel gets a
  `JobSubmissionClient(address="http://<id>-head-svc.<ns>.svc:8265")` — the Ray
  Jobs API, the path Ray upstream now recommends. Ray Client
  (`ray://…:10001`) is offered only as an "advanced, same-cluster,
  owner-pod-only" option with its NetworkPolicy caveat printed alongside.

  **AMENDED during implementation (T3 spike, user-approved):** the address is
  the cluster's **in-cluster head service**, resolved client-side from
  `(id, namespace)` — *not* a Bifrost gateway host, and it carries **no auth
  header**. The spike found that the only endpoint exposing a gateway hostname
  (`GET /registry/clusters`) is Admin-only, so a normal user's token 403s; and
  the per-owner NetworkPolicy already admits the owner's notebook pod to
  `:8265`/`:10001` directly, making that reachability (not a token) the gate.
  Consequence: connect and job-submit work for **in-cluster (Nebari) notebooks**;
  **remote/off-cluster connect is deferred** (needs an owner-scoped address
  endpoint + self-serve gateway registration — a tracked backend follow-up).

## 3. Architecture

One repo, one pip-installable wheel, scaffolded from the official
`jupyterlab/extension-template` copier template, `kind=frontend-and-server`
(ships a prebuilt/federated TS labextension **and** a jupyter-server Python
extension in a single wheel — the `dask-labextension` shape). Three units:

### 3.1 TS labextension (browser)
A "Ray Clusters" sidebar panel:
- Profile picker (dropdown of admin-approved shapes — see §5).
- Start / Stop / Suspend / Resume buttons; live status list.
- A "Connect" action that injects a ready-to-run `JobSubmissionClient(...)` cell
  into the active notebook (#6).
- A job-submit affordance carrying an env-var key/value editor (#11 →
  `runtime_env.env_vars`).

It calls **only** its co-located server extension, same-origin, authenticated by
Jupyter's own XSRF/cookie via `ServerConnection.makeRequest`. It never touches
Bifrost directly and never holds a Bifrost/OIDC token in JS. It does **not**
depend on `@brandonrc/bifrost-client` (that stays the bifrost-ui React app's
consumer).

### 3.2 Python jupyter-server extension (per-user server pod)
A thin proxy that maps panel actions → Bifrost REST using the published Python
**`bifrost_client`** (reuse, do not reimplement). It:
- holds the user credential server-side (§4) and attaches it as
  `Authorization: Bearer …` to Bifrost;
- owns the profile→`ClusterSpec` mapping so users never send raw manifests (§5);
- exposes same-origin routes under `/bifrost/*`: start (`POST /clusters`), stop
  (`DELETE /clusters/{id}`), suspend/resume, list/status
  (`GET /clusters`, `GET /clusters/{id}` + `/events` `/logs` `/metrics`
  `/nodes`), and a `get-address` helper returning the in-cluster
  head-service `JobSubmissionClient` snippet (see the #6 amendment in §2).

### 3.3 Kernel-side helper (same wheel)
`from bifrost_jupyter import connect` (thin wrapper over the Jobs API / optional
`bifrost_client`) returning a preconfigured `JobSubmissionClient` and the
connect snippet; optionally a `%bifrost` IPython magic. Requirement #6 is
inherently kernel-side; this is the dask-gateway programmatic parallel.

## 4. Auth

**Production path — OIDC token passthrough.** In the Nebari target (JupyterHub +
Keycloak, shared realm with Bifrost), the spawner injects the Keycloak access
token into the singleuser pod via `enable_auth_state: true` + a spawner/auth
hook (off by default — a **deployment prerequisite**, documented, not code). The
server extension reads that token from the pod env and forwards it to Bifrost.
Because Bifrost stamps `ClusterSpec.owner` from the request identity
(`preferred_username`, else `sub`) and the per-owner NetworkPolicy keys `:10001`
on that owner, **the identity the extension presents must equal the identity
that labels the pod** — OIDC passthrough is what guarantees that.

Where audiences don't line up, use Bifrost's **RFC 8693 token exchange**
(already shipped: `internal/auth/flows.go`, `GrantTypeTokenExchange`) to mint a
correctly-scoped subject token.

**Token lifetime.** OIDC access tokens expire and a pre-spawn snapshot goes
stale on long sessions.

**AMENDED during implementation (T9, reviewer-confirmed) — the original
"mint a session PAT and use it thereafter" is WITHDRAWN as unsatisfiable.**
It is not merely unimplemented; it cannot be made to work on the OIDC path:

* `CreateToken` calls `requireLocal()` (`internal/api/local_auth.go:126`,
  `:39-43`), so an OIDC-only deployment — the flags are independent
  (`cmd/bifrost/serve.go:151-170`) — gets `404 local auth is not enabled`.
* With local auth on, it mints via `IssueToken(ctx, identity.Subject, …)`
  (`local_auth.go:137`), whose `GetLocalUser(ctx, username)`
  (`internal/auth/local.go:566`) is keyed on the OIDC `sub`
  (`internal/auth/validator.go:483`), not `preferred_username` (`:487`) →
  `LocalAuthErrUnknownUser` (`local.go:571`), surfaced to the caller as a
  generic `500 store error` (`authz.go:230-232`).
* **The structural part.** Seeding a local user whose `Username` *is* the sub
  makes the lookup succeed — but `identityOf` (`local.go:422`) copies that same
  `user.Username` into the identity (`local.go:439`), and `Identity.Owner()`
  (`internal/auth/rbac.go:335`) returns it. The lookup key and `Owner()`'s value
  are **one field**: "the mint succeeds" and "`Owner()` yields
  `preferred_username`" are mutually exclusive. Clusters would then carry a UUID
  `bifrost.dev/owner` label (`internal/provision/kuberay.go:64`) and the
  per-owner NetworkPolicy (`kuberay.go:719`) would stop admitting the notebook
  pod to `:8265`/`:10001` — silently breaking Ray Client while the Jobs API
  kept working. Group-derived project roles are lost too.

**What ships instead:** lifetime is handled by *refreshing the OIDC credential*
(`grant_type=refresh_token` at the IdP, `_FILE` env forms re-read so a rotating
token is picked up), which preserves the OIDC identity and therefore owner-match.
A credential within 60s of expiry is re-resolved; a Bifrost 401 triggers exactly
one refresh-and-retry; an unrefreshable credential yields an actionable 401.
`BIFROST_MINT_PAT=1` keeps the mint available, opt-in, scoped to deployments
where Bifrost's PAT identity *is* the pod owner (i.e. not the Nebari
owner-label target).

Both T9 and the reviewer confirmed the above empirically with throwaway Go tests
against `internal/api` (built, run, deleted; repo left clean).

**Dev / non-Hub fallback — pasted `mob_` PAT.** Stored server-side (extension
settings), never in browser JS. Warn loudly: if the PAT's subject ≠ the pod's
owner label, `:10001` (Ray Client) stays blocked — but the Jobs-API path still
works, so this is acceptable for dev.

## 5. Profiles

The frozen API has no user-facing "profile" object. Admin-approved shapes come
from `GET /api/v1/pools` / `FlavorSpec`, or a curated allowlist the server
extension holds in config. The panel shows named profiles; the server extension
maps a choice to a `CreateCluster` body (id + `ClusterSpec` with
image/head/worker_groups). Users never see raw manifests (satisfies #6/#7's
"approved options, not arbitrary manifests").

## 6. Requirement → API mapping

| Req | UX | Bifrost call(s) |
|---|---|---|
| #9 start | panel "Start" (profile + optional env for a first job) | `POST /api/v1/clusters` (owner stamped from token) → poll `GET /clusters/{id}` to `observed_state=running` |
| #9 stop | "Stop" | `DELETE /api/v1/clusters/{id}` (202) |
| #9 suspend/resume | buttons | `POST /clusters/{id}/suspend` \| `/resume` |
| #6 connect | "Connect" → inject cell | server `get-address` → `JobSubmissionClient("http://<id>-head-svc.<ns>.svc:8265")`, no auth header (§2 amendment); Ray Client snippet as advanced option |
| #11 env vars | job-submit form | server `POST /bifrost/clusters/{id}/jobs` → Ray Jobs REST `POST /api/jobs/` on the in-cluster head service with `runtime_env.env_vars` |
| status | panel list | `GET /clusters`, `GET /clusters/{id}` + obs endpoints |

## 7. Risks (carried into the plan)

1. **Owner/label match (biggest).** OIDC `preferred_username` must equal the
   notebook pod's owner label the NetworkPolicy keys on. If Nebari's singleuser
   pod label differs, `:10001` is silently blocked. **The spike verifies this
   end-to-end first.**
2. **auth_state plumbing is deployment config, not code.** Needs
   `enable_auth_state` + spawner env injection in the Nebari/Hub config;
   document as a prerequisite; PAT fallback unblocks dev.
3. **Idle-reaping blind spot.** An interactive cluster submits no gateway jobs,
   so `idle_timeout_secs` can't see it — approved profiles must set
   `ttl_seconds` so interactive clusters still get reaped.
4. **`bifrost_client` not on PyPI yet.** Its publish lane is `workflow_dispatch`
   gated pending a `PYPI_API_TOKEN`. Prerequisite decision (plan Task 0): (a)
   publish `bifrost_client` to PyPI, (b) install from GitHub Packages, or (c)
   vendor the generated client into the extension wheel. Recommendation: (a) or
   (b); avoid vendoring so the client tracks the contract.
5. **Token lifetime** (see §4) — mitigated by the session-start PAT mint.

## 8. Sequencing — spike first

This is greenfield with real ecosystem unknowns, so the plan front-loads a thin
end-to-end spike before fanning out:

- **Spike / thinnest slice (proves the whole risk surface):** server extension +
  kernel helper, **no UI yet**. One `/bifrost/clusters` POST handler expanding a
  single hard-coded approved profile into a `ClusterSpec`, credential read
  server-side, `POST /clusters` → poll to running, a `get-address` handler, and
  a Python helper returning a working `JobSubmissionClient`. Success criterion:
  from a real Keycloak-authed Nebari notebook (or a dev notebook with a PAT),
  one call spins a cluster and a trivial job runs through the gateway Jobs API.
  This exercises owner-match, create, poll, and the auth path — the risks that
  would invalidate the whole design — before any UI investment.
- **Then, in order:** the TS panel (start/status), stop/suspend/resume, the
  job-submit env-var editor (#11), the dashboard-via-gateway iframe, profiles
  from pools/flavors, and finally swap PAT→OIDC-passthrough as the production
  auth path.

## 9. Testing

- **Server extension (Python):** unit tests with a faked `bifrost_client` /
  recorded Bifrost HTTP; assert profile→`ClusterSpec` mapping, the credential is
  attached server-side and never returned to the browser, `get-address` builds
  the correct gateway-host `JobSubmissionClient` snippet, and error mapping
  (Bifrost 403/404/409 → sane panel errors).
- **TS labextension:** jest/JL-testutils for the panel state machine and that it
  only ever calls same-origin `/bifrost/*` (never Bifrost directly, never a
  token in a request from JS).
- **Kernel helper:** returns a correctly-configured client; the injected cell is
  syntactically runnable.
- **End-to-end (spike + CI-later):** against a dev Bifrost with a memory/sqlite
  store and a fake or kind-hosted KubeRay — the spike's "one call → running →
  job runs" loop becomes the acceptance test. A live Nebari+Keycloak run is a
  dispatch/manual gate (like Bifrost's contract replay), not push CI.

## 10. Out of scope (Wave 2-C)

No Bifrost backend or contract changes (the env decision keeps it client-only).
The `bifrost-ui` React console is unaffected (different consumer, keeps
`@brandonrc/bifrost-client`). Model serving (#1/#2/#4) and the ephemeral-RayJob
flow (#5) are Wave 2 sub-projects A and B, specced separately. nebi-env (#10)
and private-S3 (#12) are the Wave 2 spikes (sub-project D).
