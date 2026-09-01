# bifrost-jupyter Implementation Plan (Wave 2-C)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A JupyterLab extension (`bifrost-jupyter`) giving notebook users a dask-gateway-like UX for Ray — start/stop their own Bifrost-managed clusters, pass env vars to jobs, and get a working connection from the kernel — all fronted by the Bifrost control plane.

**Architecture:** One pip-installable wheel from `jupyterlab/extension-template` (`kind=frontend-and-server`): a TS labextension panel + a Python `jupyter-server` extension (same-origin proxy holding the credential and calling Bifrost via `bifrost_client`) + a kernel-side helper. Spike-first: prove the end-to-end risk surface (auth/owner-match/create/connect) with no UI before building the panel.

**Tech Stack:** TypeScript (JupyterLab 4.x, `@jupyterlab/*`), Python 3.11+ (jupyter-server 2.x extension, `bifrost_client`), Ray Jobs API (`JobSubmissionClient`). New repo `github.com/brandonrc/bifrost-jupyter`.

**Spec:** `docs/superpowers/specs/2026-09-01-bifrost-jupyter-design.md` (in the bifrost repo).

> **Reconciled against shipped code — Task 10.** Wave 2-C is delivered
> (bifrost-jupyter `main`, T1–T9 merged and CI-green). Three things this plan
> assumed turned out to be wrong once measured against the Bifrost source, and
> each was ruled on and shipped differently: the connect/dashboard address
> (T3/T8), the job-submit client (T7), and the session PAT mint (T9). Those
> statements are **left in place and annotated `AMENDED`** rather than rewritten,
> so the record shows what was believed, what was found, and why the shipped
> shape differs. Completed steps are checked off. See also the design spec's own
> §2 and §4 amendments (bifrost `53c20da`, `482eacc`).

## Global Constraints

- **No Bifrost backend or contract change.** This is a pure client of the merged, frozen Bifrost API. If any task appears to need a `ClusterSpec` field or endpoint that doesn't exist, STOP and surface it — do not assume a backend change.
- Bifrost API base + auth: server extension attaches `Authorization: Bearer <token>`; token is a `mob_` PAT (dev) or an OIDC access token (prod). The browser NEVER holds the token or calls Bifrost directly — only same-origin `/bifrost/*` routes.
- Connect path is the Ray **Jobs API** (`JobSubmissionClient(address="https://<gateway-host>", headers={"Authorization": "Bearer …"})`). Ray Client (`ray://:10001`) is an advanced/in-cluster-owner-pod-only option, always printed with its NetworkPolicy caveat.
  - **AMENDED (T3 spike, user-approved; design §2).** The gateway host and the auth header are both wrong. The only endpoint exposing a gateway hostname is `GET /registry/clusters`, which is **Admin-only**, so a normal user 403s — the original spike passed only because it ran under `--dev-allow-unauthenticated`. Shipped instead: the address is the cluster's **in-cluster head service**, derived client-side from `(id, namespace)` — `http://<id>-head-svc.<ns>.svc:8265` — and carries **no auth header**. The gate on that path is the per-owner NetworkPolicy **plus** the validated cluster id (T8). Remote/off-cluster connect is deferred; it needs an owner-scoped address endpoint Bifrost does not have. Ray Client's caveat stands as written.
- Env vars (#11) attach to a job's `runtime_env.env_vars` at submit time, never to cluster create.
- Approved profiles only: users pick a named profile; the server extension maps it to a `CreateCluster` body. Users never send raw manifests. Interactive-cluster profiles MUST set `ttl_seconds` (idle_timeout can't see interactive clusters).
- Commits: conventional style, NO AI-attribution footers (no "Generated with", no Co-Authored-By). Applies in the new repo too.
- Python: ruff + mypy clean; pytest. TS: eslint + tsc clean; jest. Both wired in CI before feature tasks land (Task 2).
- Secrets never in browser JS, never in a log line, never in an error returned to the client, never in a URL/query.

---

### Task 1: Make `bifrost_client` (Python) installable

**Files:** `bifrost-api` repo — `.github/workflows/generate.yml` (modify), plus a decision record.
**Interfaces:** Produces a `pip install`-able `bifrost_client` (a version string the extension pins).

The server extension depends on the generated Python client, which is not yet on PyPI (its publish lane is `workflow_dispatch`-gated pending `PYPI_API_TOKEN`). Resolve before scaffolding.

- [x] **Step 1:** Decide the distribution (record one line in the bifrost-api README): **(a)** publish to PyPI (needs `PYPI_API_TOKEN` — a user/secret action), **(b)** publish to GitHub Packages (Python) using `GITHUB_TOKEN`, or **(c)** as a stopgap, build the wheel in generate.yml and attach it as a release asset the extension pins by URL. Recommendation: (b) if PyPI token is unavailable, since the TS client already ships via GitHub Packages and the auth pattern is proven; (a) once the token exists.
  - **AMENDED (T1).** (b) is not available: GitHub Packages has **no pip registry** (their roadmap#94). Shipped (c) — the wheel is a GitHub **release asset**, pinned by a PEP 508 direct reference in `pyproject.toml` (`bifrost_client @ https://…/python-v0.1.4/…whl`, with `allow-direct-references`). Caveat carried into the README: this unauthenticated URL install works only while `bifrost-api` is public.
- [x] **Step 2:** Wire the chosen path in `generate.yml`'s python job (flip the dispatch-only gate to publish-on-push-to-main for the chosen registry; keep the fail-red-on-missing-secret behavior). If a controller ruling is needed on secrets, surface it — do not invent a token.
- [x] **Step 3:** Verify: from a clean venv, `pip install bifrost_client` (from the chosen index) succeeds and `python -c "import bifrost_client"` works. Record the exact install line and pinned version for Task 3+.
- [x] **Step 4:** Commit in bifrost-api (conventional, no AI footer).

### Task 2: Scaffold `bifrost-jupyter` repo

**Files:** new repo — copier template output + CI.
**Interfaces:** Produces a buildable/installable empty extension: `pip install -e .` + `jupyter labextension list` shows it; `jupyter server extension list` shows the server extension enabled.

- [x] **Step 1:** Scaffold with the official template: `pip install copier jupyterlab; copier copy --trust https://github.com/jupyterlab/extension-template .` answering `kind=frontend-and-server`, name `bifrost-jupyter`, python package `bifrost_jupyter`. (If the network/template is unavailable, STOP and report — do not hand-fabricate the JL4 scaffold.)
- [x] **Step 2:** Add runtime deps: the Python package depends on `bifrost_client` (pinned per Task 1) and `ray[default]` (for `JobSubmissionClient` in the kernel helper — but keep it an extra `bifrost-jupyter[kernel]` if it bloats the server env; server extension itself needs only `bifrost_client`).
- [x] **Step 3:** Add CI (`.github/workflows/ci.yml`): Python (ruff + mypy + pytest), TS (eslint + tsc + jest + `jlpm build`), and an install smoke (`pip install -e .` then assert both extensions register). Add `.gitignore`, Apache-2.0 LICENSE (copy from bifrost), a README stub.
- [x] **Step 4:** Verify locally: `pip install -e .`, `jupyter labextension list` and `jupyter server extension list` both show `bifrost-jupyter`. Commit; `gh repo create brandonrc/bifrost-jupyter --public --source . --push`.

### Task 3: THE SPIKE — server route + kernel helper, no UI (the risk-surface proof)

**Files:** `bifrost_jupyter/handlers.py`, `bifrost_jupyter/bifrost.py` (client wrapper), `bifrost_jupyter/connect.py` (kernel helper), `bifrost_jupyter/_profiles.py` (one hard-coded profile), tests under `bifrost_jupyter/tests/`.
**Interfaces:**
- Produces server route `POST /bifrost/clusters` → `{id, status}`; `GET /bifrost/clusters/{id}/address` → `{jobs_address, headers_hint, snippet}`.
- Produces `bifrost_jupyter.connect(cluster_id) -> JobSubmissionClient`.
- **AMENDED (T3 fix round 1).** The address response ships as `{jobs_address, ray_client_address, snippet}` — no `headers_hint`, because there is no header to hint at (see the Global Constraints amendment). `connect(cluster_id)` derives the address locally and makes **no** call to Bifrost or to the local server route; two tests assert that absence.

This task exists to prove the design before UI investment: auth/owner-match → create → poll → connect → run a job over the gateway Jobs API. Success invalidates or validates the whole architecture.

- [x] **Step 1:** `bifrost.py` — a thin wrapper constructing a `bifrost_client` configured from env: `BIFROST_API_URL`, and the credential (a `mob_` PAT via `BIFROST_TOKEN` for the spike; OIDC comes in Task 9). One `create_cluster(spec)`, `get_cluster(id)`, `delete_cluster(id)`.
- [x] **Step 2:** `_profiles.py` — one hard-coded approved profile `{name: "small", image, head_cpu, head_memory, worker_groups:[…], ray_version, ttl_seconds}` → returns a `CreateCluster` body (id generated per-user, `ttl_seconds` set).
- [x] **Step 3:** `handlers.py` — `POST /bifrost/clusters`: read credential server-side, expand the "small" profile, `create_cluster`, return `{id, status}`. `GET /bifrost/clusters/{id}/address`: resolve the cluster's gateway host from the registry/get-cluster, return `{jobs_address: "https://<host>", headers_hint, snippet}` (the snippet is a runnable `JobSubmissionClient(...)`). Errors: map Bifrost 401/403/404/409 → JSON `{error}` with the right HTTP status; NEVER echo the token or upstream internal error text.
  - **AMENDED (T3 fix round 1).** "Resolve the gateway host from the registry" was the design-invalidating part: `RegistryApi.list_registry` is Admin-only. The registry call is gone entirely; the address is derived from `(id, namespace)`. The error-mapping and no-token-echo requirements shipped as written and are still asserted.
- [x] **Step 4:** `connect.py` — `connect(cluster_id)` calls the local server route (or bifrost directly kernel-side with the same token) and returns a live `JobSubmissionClient`.
  - **AMENDED (T3 fix round 1).** Neither: it calls nothing at all. The address is derivable from the cluster id plus the configured namespace, so the helper is offline and token-free.
- [x] **Step 5:** Tests (pytest, faked bifrost_client / recorded HTTP): profile→CreateCluster mapping is correct incl. ttl_seconds; the credential is attached to the outbound bifrost call and is NOT present in any handler response body or log; address builder produces the gateway-host Jobs address; error mapping. NO real cluster needed for unit tests.
- [x] **Step 6:** **Acceptance (manual, recorded in the task report):** against a dev Bifrost (memory/sqlite store + a reachable KubeRay, or a documented fake) with a `mob_` PAT, from a notebook: call the route → cluster reaches running → `connect(id).submit_job(entrypoint="python -c 'import ray; ray.init(); print(ray.cluster_resources())'")` runs through the gateway Jobs API and returns. If a live KubeRay isn't available in-session, wire it as the CI/manual acceptance and prove as much as the environment allows (handler + client + snippet correctness), stating explicitly what was live vs pending.
  - **AMENDED (T3, then T10).** "Runs through the gateway Jobs API" is superseded — jobs go to the cluster's own head service, not a gateway. The live loop was re-verified **with auth enforced** (`--local-auth` + a real Bearer) after the first attempt was found to have run under `--dev-allow-unauthenticated`, which nulls the identity and short-circuits all authz. The generalised acceptance now lives in the extension repo at `docs/ACCEPTANCE.md`, with a live-vs-pending table; a real job run still needs KubeRay (Gate B).
- [x] **Step 7:** Commit.

### Task 4: Profiles from config / pools-flavors

**Files:** `bifrost_jupyter/_profiles.py` (extend), `bifrost_jupyter/config.py`, tests.
**Interfaces:** Produces `list_profiles() -> [ProfileView]` and `profile_to_spec(name, overrides) -> CreateCluster`.

- [x] **Step 1:** Config-driven allowlist (traitlets config on the server extension) of named profiles; optionally hydrate from `GET /api/v1/pools` / `FlavorSpec`. Every profile sets `ttl_seconds`.
  - **AMENDED (T4).** The optional pools/flavors hydration is **deferred, deliberately**: `PoolView`/`FlavorSpec` describe scheduling capacity, not head/worker shape, so the mapping would be lossy and misleading. Shipped as config-only (`BifrostConfig.profiles`, built-ins `small`/`medium`/`gpu`, all `ttl_seconds=3600`). `ttl_seconds` is structurally required — a positional field on the profile type, not a default.
- [x] **Step 2:** `GET /bifrost/profiles` handler returns the safe view (no raw manifest surface). Tests: unknown profile → error; ttl always set; no field lets a caller inject arbitrary spec.
- [x] **Step 3:** Commit.

### Task 5: TS panel — start + status

**Files:** `src/` (template TS), `src/BifrostPanel.tsx`, `src/api.ts` (same-origin client), `src/index.ts` (plugin registration), jest tests.
**Interfaces:** A sidebar widget; `api.ts` wraps `ServerConnection.makeRequest` to `/bifrost/*` only.
  - **AMENDED (T5, controller ruling).** Scope expanded mid-task: the panel's status list needs `GET /bifrost/clusters`, which the spike never built and this plan assumed existed. Ruled in-scope — it is extension code wrapping Bifrost's existing project-scoped `GET /api/v1/clusters`, not a Go backend change. `BifrostClient.list_clusters` + the GET handler landed on the same branch. Also added later on the same task: an unconfigured install must answer `200 {"configured": false}` rather than a 500, after a bare install broke JupyterLab's headless startup check in CI.

- [x] **Step 1:** `api.ts` — typed calls to the server extension routes via `ServerConnection.makeRequest` (same-origin, XSRF cookie). No Bifrost URL, no token, ever, in this file.
- [x] **Step 2:** Panel: profile dropdown (from `GET /bifrost/profiles`), "Start" button → `POST /bifrost/clusters`, a status list polling `GET /bifrost/clusters`. Register as a left-sidebar widget.
- [x] **Step 3:** jest tests: panel only ever calls `/bifrost/*`; start disabled without a profile; status renders states.
- [x] **Step 4:** Commit.

### Task 6: Stop / suspend / resume + connect-cell injection

**Files:** `handlers.py` (+routes), `src/BifrostPanel.tsx` (+controls), `connect` cell injection in `src/`.
**Interfaces:** routes `DELETE /bifrost/clusters/{id}`, `POST /bifrost/clusters/{id}/suspend|resume`; panel "Connect" injects a runnable cell.

- [x] **Step 1:** Server routes mapping to `DELETE /api/v1/clusters/{id}`, suspend/resume. Panel buttons + confirm on stop.
- [x] **Step 2:** "Connect" → fetch `/bifrost/clusters/{id}/address` → inject a notebook cell with the `JobSubmissionClient` snippet (+ the Ray Client advanced option as a comment with its caveat).
- [x] **Step 3:** Tests both sides (server route mapping; panel injects a syntactically runnable cell). Commit.

### Task 7: Job submit with env vars (#11)

**Files:** `handlers.py` (job-submit route), `src/` (env-var editor), `connect.py`, tests.
**Interfaces:** `POST /bifrost/clusters/{id}/jobs` with `{entrypoint, env_vars, ...}` → submits via the Jobs API with `runtime_env.env_vars`.

- [x] **Step 1:** Panel job-submit form with a key/value env-var editor; server route submits through `JobSubmissionClient` with `runtime_env={"env_vars": {...}}`. This is where #11 lives.
  - **AMENDED (T7, controller ruling).** Not through `JobSubmissionClient`: Ray is deliberately a `[kernel]` extra and must not be pulled into the per-user *server* environment. The server speaks the **raw Ray Jobs REST contract** (`POST /api/jobs/`) over tornado's `AsyncHTTPClient` instead — wire shape doc-verified and cross-checked against Ray's own `job_head.py`/`common.py`. `runtime_env.env_vars` is unchanged. The job-status view is an **allowlist** (`status`/`message`/`start_time`/`end_time`), not a passthrough, so submitted env *values* can never be read back.
- [ ] **Step 2 — PARTIAL:** Tests: env vars land in `runtime_env.env_vars`; submit returns a job id; status/logs readable via the gateway. Commit.
  - **AMENDED (T7), and left UNCHECKED deliberately.** Env vars → `runtime_env.env_vars` and job submit/status shipped and are tested. **The log tail was never built** — T7 self-flagged it as a follow-up and no later task picked it up. "Via the gateway" is superseded either way (head service, as above). The box stays open because a checked one would stop anyone from looking for the missing half.

### Task 8: Dashboard access (observability, #15-adjacent)

**Files:** `handlers.py` (optional dashboard proxy or link), `src/`.
- [x] **Step 1:** Surface the cluster's Ray dashboard through the Bifrost gateway host (link out, or a jupyter-server-proxy-style same-origin iframe if CORS/framing allows — investigate; link-out is the safe default). No new auth surface. Commit.
  - **AMENDED (T8, controller correction before dispatch).** Gateway-host link-out was dead on arrival for the same reason as T3's address: the registry is Admin-only. The dashboard is on the *same* `:8265` the NetworkPolicy already admits the notebook pod to, so it ships as a **same-origin proxy** through the server extension — hand-rolled in `_dashboard.py`, **no `jupyter-server-proxy` dependency** (Ray's dashboard is built `PUBLIC_URL="."` with a `HashRouter` and no WebSocket, so no asset rewriting is needed; and jsp's host allowlist is unscopeable). Two deliberate limits: **GET/HEAD only** (proxying writes would mean exempting the route from Jupyter's XSRF check, i.e. a CSRF path into the cluster), and a plain `JupyterHandler` rather than `APIHandler`, whose `default-src 'none'` CSP would blank the iframe.
- [x] **Step 2 (unplanned, Critical — added during T8 review):** Close an SSRF that pre-dated this task. `cluster_id` was never validated anywhere and was f-stringed into the upstream host, so an id like `evil.example:9999?` chose the host the *server* connected to — cross-site triggerable on a plain GET via the ambient Jupyter cookie. Fixed at the class level: one `_address.validate_cluster_id` (single RFC 1123 label) plus a per-route `_ClusterIdMixin` guard on **all seven** `{id}` routes, including the already-merged jobs/address/lifecycle ones. Mutation-checked (neutering the validator fails 109 tests) and swept for sibling instances of the pattern.

### Task 9: Production auth — OIDC passthrough + token lifetime

**Files:** `bifrost.py` (credential resolution), `config.py`, docs.
**Interfaces:** credential precedence: OIDC access token from pod env (Nebari `auth_state` injection) → optional RFC 8693 exchange → session-start minted PAT; `mob_` PAT (`BIFROST_TOKEN`) remains the dev fallback.

- [x] **Step 1:** Resolve the OIDC access token from the documented pod env var; forward as Bearer. If audiences require it, use Bifrost's RFC 8693 token exchange to mint a subject token.
  - **AMENDED (T9).** There is no "documented pod env var" upstream — the `auth_state` hook is deployment-written — so the extension defines `BIFROST_OIDC_TOKEN_FILE`/`BIFROST_OIDC_TOKEN` and also accepts the conventional `ACCESS_TOKEN`. And the exchange is not *Bifrost's*: Bifrost exposes no exchange endpoint, and `internal/auth/flows.go` says outright that it "mints nothing" — `ExchangeToken` posts to the **IdP's** token endpoint. The extension does the same, https-only.
- [x] **Step 2:** Token lifetime: at session start, mint a longer-lived Bifrost PAT via `POST /api/v1/auth/tokens` and use it thereafter (or refresh from auth_state). Document the `enable_auth_state` + spawner-hook deployment prerequisite in the README.
  - **AMENDED (T9 — the mint is WITHDRAWN as unsatisfiable; design §4 amended in bifrost `482eacc`).** `POST /api/v1/auth/tokens` cannot serve the OIDC path. It is gated on `requireLocal()` (404 without `--local-auth`), and it mints via `IssueToken(identity.Subject, …)`, which needs a **local user row keyed on the OIDC `sub`**. Worse, the lookup key and `Identity.Owner()` are the *same* `user.Username` field, so making the mint succeed **requires** `Username = sub` — which is exactly what then stamps the wrong `bifrost.dev/owner` and silently breaks the per-owner NetworkPolicy on `:8265`/`:10001`. "The mint works" and "`Owner()` yields `preferred_username`" are mutually exclusive requirements on one field; no configuration satisfies both. Shipped instead: the OIDC identity is **kept**, and lifetime is handled by re-reading/refreshing it (`grant_type=refresh_token` at the IdP, plus re-read of the `_FILE` forms) with a single refresh-and-retry on a Bifrost 401. The mint remains implemented behind `BIFROST_MINT_PAT=1`, off by default, degrading to the OIDC token if it fails. The `enable_auth_state` prerequisite is documented as required.
- [x] **Step 3:** Tests: credential precedence (OIDC > PAT), the exchange path (faked), and that an expired token triggers the mint/refresh, not a silent 401. Verify owner-match note is documented (OIDC subject must equal the pod owner label or Ray Client `:10001` is blocked — Jobs API still works). Commit.
  - **AMENDED (T9).** One correction to the owner-match note as written. "Jobs API still works" was true of the *gateway-proxied* Jobs path — `TenantAllowNetworkPolicy` admits the **control plane** to `:8265`/`:10001` (`internal/provision/kuberay.go:680`), so a mismatched owner only cost you Ray Client. That stopped being true when T3 moved the extension to talk to the head service **directly**. The tier-2 per-owner rule in `ClusterAllowNetworkPolicy` (`kuberay.go:723-741`) is **one ingress rule**: a single peer block ANDing the notebook namespace with `bifrost.dev/owner=<owner>` (`kuberay.go:64`, `NotebookNamespace` `kuberay.go:68`), guarding `Ports: {tcpPort(10001), tcpPort(8265)}` (`kuberay.go:740`) **together**. One selector, both ports — so an owner mismatch removes job submit, job status and the dashboard in the same stroke as Ray Client. What keeps working is the **Bifrost control plane** (start/list/stop) — which is exactly what makes the failure confusing. Documented that way. Also: owner-match is **not observable through the Bifrost API** — `ClusterView` exposes no `owner` field — so it can only be checked against the RayCluster's k8s label (see `docs/ACCEPTANCE.md`, Gate B).

### Task 10: Packaging, docs, acceptance

**Files:** `pyproject.toml`, `README.md`, `docs/`, CI release job.
- [x] **Step 1:** Wheel ships prebuilt JS (no Node at install); `pip install bifrost-jupyter` enables both extensions. README: install, the `enable_auth_state` prerequisite, `ttl_seconds`-in-profiles requirement, the Jobs-API-vs-Ray-Client explanation, the owner-match caveat.
  - Verified by building the wheel and installing it into a clean venv **with Node absent from `PATH`**: both extensions report `enabled … OK`, and the served routes answer live. Recorded in `docs/ACCEPTANCE.md` (A4) and the README's Install section.
- [x] **Step 2:** Acceptance doc: the spike loop (Task 3) generalized to the full UX (start via panel → status → connect cell → submit job with env vars → stop), run against a dev Bifrost; a live Nebari+Keycloak run wired as a manual/dispatch gate (like Bifrost's contract replay), not push CI.
  - Shipped as `docs/ACCEPTANCE.md` in the extension repo, with recorded transcripts. Gate A runs under `--local-auth` with a real Bearer and additionally covers the two authorization behaviours the plan never called out for acceptance: the operator-role 403, and credential expiry → refresh → actionable 401. Gate B (Nebari + Keycloak) is specified but **not run** — no hub, IdP or KubeRay available.
- [ ] **Step 3:** Tag/release; commit.
  - **NOT DONE, deliberately.** Cutting a release is a human decision and runs through the repo's `jupyter-releaser` lane (`prep-release`/`publish-release` workflows), which needs repo secrets and a maintainer's version bump. Everything a release consumes is in place and CI-verified; the tag itself is left to a maintainer.

---

## Self-review notes

- Spec coverage: #9 (T3/T5/T6), #11 (T7), #6 connect (T3/T6), profiles/#7-approved-options (T4), auth incl. all §4 paths (T9), the 5 spec risks (owner-match T3 acceptance + T9 docs; auth_state prerequisite T9/T10; ttl_seconds T4; bifrost_client availability T1; token lifetime T9). Backend-unchanged constraint enforced in Global Constraints + T-level "STOP if you need a backend change."
- Ordering: T1 (unblocks Python dep) → T2 (scaffold) → **T3 spike (risk proof — gate before UI)** → T4 → T5 → T6 → T7 → T8 → T9 (swap dev-PAT → prod OIDC) → T10. T5+ (frontend) can pipeline against T4's server routes.
- Greenfield honesty: T2 uses the real copier template (STOP if unavailable rather than fabricate JL4 scaffold); T3 is deliberately the reference implementation the later server tasks extend; the live-KubeRay acceptance is explicitly labeled live-vs-pending per environment.
- This is one sub-project (C). Sub-projects A (ephemeral RayJob), B (model serving), D (spikes) get their own spec→plan cycles.

## Reconciliation summary (Task 10)

What this plan got wrong, and what it cost to find out:

1. **Three separate assumptions about reachable addresses** (connect T3, job submit T7, dashboard T8) all traced to one unchecked belief: that a normal user can discover a cluster's gateway host. `GET /registry/clusters` is Admin-only, so none of them could work as written. The first was caught only because a reviewer noticed the "live" proof had run with authentication disabled.
2. **The session PAT mint (T9)** was not merely awkward but unsatisfiable — two requirements on one database field. Found by reading the Bifrost source rather than implementing the step as written.
3. **A Critical SSRF** sat in code merged by T3/T5/T7 and was surfaced by T8's review, not by any of the three tasks that shipped it. It lived in an unexamined assumption ("`cluster_id` is always our generated slug"), not in a line anyone was reviewing.

The common thread is that every one of these was found by checking a claim against the source or against a real, auth-enforced run — never by re-reading the plan. Two habits carried forward: **never accept a "live-verified" claim made with authentication disabled**, and **fix a Critical finding at the class level, then sweep the siblings of the defective pattern**.
