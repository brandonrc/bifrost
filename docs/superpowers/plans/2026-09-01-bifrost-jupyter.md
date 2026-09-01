# bifrost-jupyter Implementation Plan (Wave 2-C)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A JupyterLab extension (`bifrost-jupyter`) giving notebook users a dask-gateway-like UX for Ray — start/stop their own Bifrost-managed clusters, pass env vars to jobs, and get a working connection from the kernel — all fronted by the Bifrost control plane.

**Architecture:** One pip-installable wheel from `jupyterlab/extension-template` (`kind=frontend-and-server`): a TS labextension panel + a Python `jupyter-server` extension (same-origin proxy holding the credential and calling Bifrost via `bifrost_client`) + a kernel-side helper. Spike-first: prove the end-to-end risk surface (auth/owner-match/create/connect) with no UI before building the panel.

**Tech Stack:** TypeScript (JupyterLab 4.x, `@jupyterlab/*`), Python 3.11+ (jupyter-server 2.x extension, `bifrost_client`), Ray Jobs API (`JobSubmissionClient`). New repo `github.com/brandonrc/bifrost-jupyter`.

**Spec:** `docs/superpowers/specs/2026-09-01-bifrost-jupyter-design.md` (in the bifrost repo).

## Global Constraints

- **No Bifrost backend or contract change.** This is a pure client of the merged, frozen Bifrost API. If any task appears to need a `ClusterSpec` field or endpoint that doesn't exist, STOP and surface it — do not assume a backend change.
- Bifrost API base + auth: server extension attaches `Authorization: Bearer <token>`; token is a `mob_` PAT (dev) or an OIDC access token (prod). The browser NEVER holds the token or calls Bifrost directly — only same-origin `/bifrost/*` routes.
- Connect path is the Ray **Jobs API** (`JobSubmissionClient(address="https://<gateway-host>", headers={"Authorization": "Bearer …"})`). Ray Client (`ray://:10001`) is an advanced/in-cluster-owner-pod-only option, always printed with its NetworkPolicy caveat.
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

- [ ] **Step 1:** Decide the distribution (record one line in the bifrost-api README): **(a)** publish to PyPI (needs `PYPI_API_TOKEN` — a user/secret action), **(b)** publish to GitHub Packages (Python) using `GITHUB_TOKEN`, or **(c)** as a stopgap, build the wheel in generate.yml and attach it as a release asset the extension pins by URL. Recommendation: (b) if PyPI token is unavailable, since the TS client already ships via GitHub Packages and the auth pattern is proven; (a) once the token exists.
- [ ] **Step 2:** Wire the chosen path in `generate.yml`'s python job (flip the dispatch-only gate to publish-on-push-to-main for the chosen registry; keep the fail-red-on-missing-secret behavior). If a controller ruling is needed on secrets, surface it — do not invent a token.
- [ ] **Step 3:** Verify: from a clean venv, `pip install bifrost_client` (from the chosen index) succeeds and `python -c "import bifrost_client"` works. Record the exact install line and pinned version for Task 3+.
- [ ] **Step 4:** Commit in bifrost-api (conventional, no AI footer).

### Task 2: Scaffold `bifrost-jupyter` repo

**Files:** new repo — copier template output + CI.
**Interfaces:** Produces a buildable/installable empty extension: `pip install -e .` + `jupyter labextension list` shows it; `jupyter server extension list` shows the server extension enabled.

- [ ] **Step 1:** Scaffold with the official template: `pip install copier jupyterlab; copier copy --trust https://github.com/jupyterlab/extension-template .` answering `kind=frontend-and-server`, name `bifrost-jupyter`, python package `bifrost_jupyter`. (If the network/template is unavailable, STOP and report — do not hand-fabricate the JL4 scaffold.)
- [ ] **Step 2:** Add runtime deps: the Python package depends on `bifrost_client` (pinned per Task 1) and `ray[default]` (for `JobSubmissionClient` in the kernel helper — but keep it an extra `bifrost-jupyter[kernel]` if it bloats the server env; server extension itself needs only `bifrost_client`).
- [ ] **Step 3:** Add CI (`.github/workflows/ci.yml`): Python (ruff + mypy + pytest), TS (eslint + tsc + jest + `jlpm build`), and an install smoke (`pip install -e .` then assert both extensions register). Add `.gitignore`, Apache-2.0 LICENSE (copy from bifrost), a README stub.
- [ ] **Step 4:** Verify locally: `pip install -e .`, `jupyter labextension list` and `jupyter server extension list` both show `bifrost-jupyter`. Commit; `gh repo create brandonrc/bifrost-jupyter --public --source . --push`.

### Task 3: THE SPIKE — server route + kernel helper, no UI (the risk-surface proof)

**Files:** `bifrost_jupyter/handlers.py`, `bifrost_jupyter/bifrost.py` (client wrapper), `bifrost_jupyter/connect.py` (kernel helper), `bifrost_jupyter/_profiles.py` (one hard-coded profile), tests under `bifrost_jupyter/tests/`.
**Interfaces:**
- Produces server route `POST /bifrost/clusters` → `{id, status}`; `GET /bifrost/clusters/{id}/address` → `{jobs_address, headers_hint, snippet}`.
- Produces `bifrost_jupyter.connect(cluster_id) -> JobSubmissionClient`.

This task exists to prove the design before UI investment: auth/owner-match → create → poll → connect → run a job over the gateway Jobs API. Success invalidates or validates the whole architecture.

- [ ] **Step 1:** `bifrost.py` — a thin wrapper constructing a `bifrost_client` configured from env: `BIFROST_API_URL`, and the credential (a `mob_` PAT via `BIFROST_TOKEN` for the spike; OIDC comes in Task 9). One `create_cluster(spec)`, `get_cluster(id)`, `delete_cluster(id)`.
- [ ] **Step 2:** `_profiles.py` — one hard-coded approved profile `{name: "small", image, head_cpu, head_memory, worker_groups:[…], ray_version, ttl_seconds}` → returns a `CreateCluster` body (id generated per-user, `ttl_seconds` set).
- [ ] **Step 3:** `handlers.py` — `POST /bifrost/clusters`: read credential server-side, expand the "small" profile, `create_cluster`, return `{id, status}`. `GET /bifrost/clusters/{id}/address`: resolve the cluster's gateway host from the registry/get-cluster, return `{jobs_address: "https://<host>", headers_hint, snippet}` (the snippet is a runnable `JobSubmissionClient(...)`). Errors: map Bifrost 401/403/404/409 → JSON `{error}` with the right HTTP status; NEVER echo the token or upstream internal error text.
- [ ] **Step 4:** `connect.py` — `connect(cluster_id)` calls the local server route (or bifrost directly kernel-side with the same token) and returns a live `JobSubmissionClient`.
- [ ] **Step 5:** Tests (pytest, faked bifrost_client / recorded HTTP): profile→CreateCluster mapping is correct incl. ttl_seconds; the credential is attached to the outbound bifrost call and is NOT present in any handler response body or log; address builder produces the gateway-host Jobs address; error mapping. NO real cluster needed for unit tests.
- [ ] **Step 6:** **Acceptance (manual, recorded in the task report):** against a dev Bifrost (memory/sqlite store + a reachable KubeRay, or a documented fake) with a `mob_` PAT, from a notebook: call the route → cluster reaches running → `connect(id).submit_job(entrypoint="python -c 'import ray; ray.init(); print(ray.cluster_resources())'")` runs through the gateway Jobs API and returns. If a live KubeRay isn't available in-session, wire it as the CI/manual acceptance and prove as much as the environment allows (handler + client + snippet correctness), stating explicitly what was live vs pending.
- [ ] **Step 7:** Commit.

### Task 4: Profiles from config / pools-flavors

**Files:** `bifrost_jupyter/_profiles.py` (extend), `bifrost_jupyter/config.py`, tests.
**Interfaces:** Produces `list_profiles() -> [ProfileView]` and `profile_to_spec(name, overrides) -> CreateCluster`.

- [ ] **Step 1:** Config-driven allowlist (traitlets config on the server extension) of named profiles; optionally hydrate from `GET /api/v1/pools` / `FlavorSpec`. Every profile sets `ttl_seconds`.
- [ ] **Step 2:** `GET /bifrost/profiles` handler returns the safe view (no raw manifest surface). Tests: unknown profile → error; ttl always set; no field lets a caller inject arbitrary spec.
- [ ] **Step 3:** Commit.

### Task 5: TS panel — start + status

**Files:** `src/` (template TS), `src/BifrostPanel.tsx`, `src/api.ts` (same-origin client), `src/index.ts` (plugin registration), jest tests.
**Interfaces:** A sidebar widget; `api.ts` wraps `ServerConnection.makeRequest` to `/bifrost/*` only.

- [ ] **Step 1:** `api.ts` — typed calls to the server extension routes via `ServerConnection.makeRequest` (same-origin, XSRF cookie). No Bifrost URL, no token, ever, in this file.
- [ ] **Step 2:** Panel: profile dropdown (from `GET /bifrost/profiles`), "Start" button → `POST /bifrost/clusters`, a status list polling `GET /bifrost/clusters`. Register as a left-sidebar widget.
- [ ] **Step 3:** jest tests: panel only ever calls `/bifrost/*`; start disabled without a profile; status renders states.
- [ ] **Step 4:** Commit.

### Task 6: Stop / suspend / resume + connect-cell injection

**Files:** `handlers.py` (+routes), `src/BifrostPanel.tsx` (+controls), `connect` cell injection in `src/`.
**Interfaces:** routes `DELETE /bifrost/clusters/{id}`, `POST /bifrost/clusters/{id}/suspend|resume`; panel "Connect" injects a runnable cell.

- [ ] **Step 1:** Server routes mapping to `DELETE /api/v1/clusters/{id}`, suspend/resume. Panel buttons + confirm on stop.
- [ ] **Step 2:** "Connect" → fetch `/bifrost/clusters/{id}/address` → inject a notebook cell with the `JobSubmissionClient` snippet (+ the Ray Client advanced option as a comment with its caveat).
- [ ] **Step 3:** Tests both sides (server route mapping; panel injects a syntactically runnable cell). Commit.

### Task 7: Job submit with env vars (#11)

**Files:** `handlers.py` (job-submit route), `src/` (env-var editor), `connect.py`, tests.
**Interfaces:** `POST /bifrost/clusters/{id}/jobs` with `{entrypoint, env_vars, ...}` → submits via the Jobs API with `runtime_env.env_vars`.

- [ ] **Step 1:** Panel job-submit form with a key/value env-var editor; server route submits through `JobSubmissionClient` with `runtime_env={"env_vars": {...}}`. This is where #11 lives.
- [ ] **Step 2:** Tests: env vars land in `runtime_env.env_vars`; submit returns a job id; status/logs readable via the gateway. Commit.

### Task 8: Dashboard access (observability, #15-adjacent)

**Files:** `handlers.py` (optional dashboard proxy or link), `src/`.
- [ ] **Step 1:** Surface the cluster's Ray dashboard through the Bifrost gateway host (link out, or a jupyter-server-proxy-style same-origin iframe if CORS/framing allows — investigate; link-out is the safe default). No new auth surface. Commit.

### Task 9: Production auth — OIDC passthrough + token lifetime

**Files:** `bifrost.py` (credential resolution), `config.py`, docs.
**Interfaces:** credential precedence: OIDC access token from pod env (Nebari `auth_state` injection) → optional RFC 8693 exchange → session-start minted PAT; `mob_` PAT (`BIFROST_TOKEN`) remains the dev fallback.

- [ ] **Step 1:** Resolve the OIDC access token from the documented pod env var; forward as Bearer. If audiences require it, use Bifrost's RFC 8693 token exchange to mint a subject token.
- [ ] **Step 2:** Token lifetime: at session start, mint a longer-lived Bifrost PAT via `POST /api/v1/auth/tokens` and use it thereafter (or refresh from auth_state). Document the `enable_auth_state` + spawner-hook deployment prerequisite in the README.
- [ ] **Step 3:** Tests: credential precedence (OIDC > PAT), the exchange path (faked), and that an expired token triggers the mint/refresh, not a silent 401. Verify owner-match note is documented (OIDC subject must equal the pod owner label or Ray Client `:10001` is blocked — Jobs API still works). Commit.

### Task 10: Packaging, docs, acceptance

**Files:** `pyproject.toml`, `README.md`, `docs/`, CI release job.
- [ ] **Step 1:** Wheel ships prebuilt JS (no Node at install); `pip install bifrost-jupyter` enables both extensions. README: install, the `enable_auth_state` prerequisite, `ttl_seconds`-in-profiles requirement, the Jobs-API-vs-Ray-Client explanation, the owner-match caveat.
- [ ] **Step 2:** Acceptance doc: the spike loop (Task 3) generalized to the full UX (start via panel → status → connect cell → submit job with env vars → stop), run against a dev Bifrost; a live Nebari+Keycloak run wired as a manual/dispatch gate (like Bifrost's contract replay), not push CI.
- [ ] **Step 3:** Tag/release; commit.

---

## Self-review notes

- Spec coverage: #9 (T3/T5/T6), #11 (T7), #6 connect (T3/T6), profiles/#7-approved-options (T4), auth incl. all §4 paths (T9), the 5 spec risks (owner-match T3 acceptance + T9 docs; auth_state prerequisite T9/T10; ttl_seconds T4; bifrost_client availability T1; token lifetime T9). Backend-unchanged constraint enforced in Global Constraints + T-level "STOP if you need a backend change."
- Ordering: T1 (unblocks Python dep) → T2 (scaffold) → **T3 spike (risk proof — gate before UI)** → T4 → T5 → T6 → T7 → T8 → T9 (swap dev-PAT → prod OIDC) → T10. T5+ (frontend) can pipeline against T4's server routes.
- Greenfield honesty: T2 uses the real copier template (STOP if unavailable rather than fabricate JL4 scaffold); T3 is deliberately the reference implementation the later server tasks extend; the live-KubeRay acceptance is explicitly labeled live-vs-pending per environment.
- This is one sub-project (C). Sub-projects A (ephemeral RayJob), B (model serving), D (spikes) get their own spec→plan cycles.
