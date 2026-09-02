# Requirement Test Framework — Design

**Date**: 2026-09-02
**Status**: approved for implementation (sections 1–5 reviewed in conversation; 6–9 written on the same basis)
**Scope**: `bifrost` (primary), `bifrost-pack`, `grace-e2e`, `bifrost-jupyter` (adapters only)

## 0. Why this exists

The 18-row requirement table in `docs/SPEC.md` is the contract with JATIC.
On 2026-09-02 reading it end to end against the code showed two rows marked
**Built** for components that do not exist (#16: no `EngineRouter`, no Dask
provisioner) or have no producer (#14: `RecordUsageSamples` is never called;
the live usage report is empty after hours of clusters), and a third (#7)
that the Wave 1 exit record claimed closed while the table said Partial.

The same day, running the real deployment surfaced four defects none of the
existing suites could see:

1. `POST /clusters` with no `id` and no `spec` returned **201**, persisted
   an empty-id record, wedged a reconcile retry loop forever, and left a
   row no API route can delete. No request validation runs; `required` in
   the contract is enforced only where a handler hand-checks.
2. Provisioned clusters never report `ready` on an image without `wget`,
   because the emitted probes shell out to `wget | grep`. Ray was healthy.
3. The usage report is a correct reader over a permanently empty table.
4. The dashboard's nginx resolves `proxy_pass http://bifrost:8484` once at
   startup and exits if DNS is not yet up, so every node reboot takes the
   UI down until backoff happens to land after CoreDNS.

All four share a shape: **the component is correct; the wiring around it is
not.** Unit tests pass. Nothing exercises the path. This framework exists to
make requirement status *derived from tests that run the path*, so a row
cannot say Built unless something proved it, and to make the four defects
above permanent regression tests rather than anecdotes.

Decisions already taken with the user, carried in unchanged:

- Unbuilt requirements get tests **now**, red until built.
- Tiered E2E: in-process on every PR; real kind+Calico nightly.
- 95% coverage, risk-tiered; mutation testing on the critical packages.
- Grace is a **playground** (user, 2026-09-02): the framework may modify
  it freely and may schedule runs against it. The one thing worth
  preserving there is the set of packs bifrost is meant to serve —
  `data-science-pack` (jupyter) and `checkmaite` — because they are the
  integration consumers, not because they are off-limits.
- **End goal, stated by the user**: bifrost running on grace so that
  data-science-pack and checkmaite leverage it for Ray handling. §5a turns
  that sentence into tests.
- Framework lives at `bifrost/test/requirements` (not a separate repo, not
  the pack).

## 1. Layout and the `Target` seam

```
bifrost/
  internal/app/               # NEW: New(cfg) → *App; the one wiring function
                              #   used by cmd/bifrost/serve.go AND the inproc target
  pkg/client/                 # NEW: Go client generated from internal/api/openapi.json
                              #   (oapi-codegen client mode; covered by the spec-sync gate)
  test/requirements/
    req/                      # Covers, NotYetBuilt, NeedsCapability, Target, run-id, cleanup
    target/
      inproc/                 # L2: app.New with fake Provisioner + memory Store + fake Clock
      cluster/                # L3: API URL + kubeconfig; kind and grace are configs of this
    contract/                 # generated negatives across every operation
    pack/                     # helm-template assertions against ../../../bifrost-pack/chart
    r01_serve_from_jupyter/   # one package per requirement, numbered
    r02_group_serving/
    ...
    r18_baseline/
    soak/                     # stress lane; different pass criteria, so separate
    cmd/reqreport/            # go test -json (+ JUnit) → docs/requirements/traceability.md
    cmd/covreport/            # coverprofile → per-tier numbers vs .coverage-ratchet.json
    guards_test.go            # AST guards (§1.3)
```

### 1.1 `Target`

```go
type Target interface {
    // API returns a client authenticated as the target's current principal.
    API() *client.ClientWithResponses
    // As returns a Target bound to another principal. Principals are named
    // roles seeded identically on every target: admin, operator,
    // dev-a (project team-a), dev-b (project team-b), anon.
    As(principal string) Target
    // K8s returns a controller-runtime client for the target namespace, or
    // ok=false when the target has no Kubernetes (inproc).
    K8s() (c ctrlclient.Client, ok bool)
    Namespace() string
    // Clock returns a controllable clock on inproc and nil on real clusters.
    // Tests needing to advance time call req.AdvanceOrWait(t, tgt, d): advance
    // the fake clock on inproc, sleep on cluster.
    Clock() FakeClock
    RunID() string
}
```

`Target` is chosen by `REQ_TARGET` (`inproc` default, `kind`, `grace`) —
**not by build tags**. A single test file runs on every lane; a test that
needs Kubernetes calls `req.NeedK8s(t, tgt)` which skips on inproc with a
recorded reason. This is what makes "the same test on fake and real" literal
rather than aspirational.

### 1.2 `inproc` is the real wiring

`inproc` calls `app.New(cfg)` — the **same constructor `cmd/bifrost/serve.go`
uses** — with a fake `provision.Provisioner`, the memory `Store`, a fake
`Clock`, and local auth. Real API server, real middleware, real reconciler,
real store interface, real audit chain. Only the Kubernetes edge is faked.

The fake Provisioner converges `provisioning → running` on the next reconcile
tick and **validates names the way the real API server would** (RFC 1123,
non-empty). Defect 1 fails on inproc within one tick because of that.

Extracting `internal/app.New` is a prerequisite: today the wiring lives in
`runServe`. It is a refactor with a strict invariant — `serve.go` after the
change must construct nothing itself except flags and signal handling.

### 1.3 Guards (AST tests, run in L1)

- No file under `test/requirements/` except `target/inproc/*` may import
  `bifrost/internal/...`. Tests speak the public contract, as a user would.
- Every `_test.go` under `r??_*/` has at least one `req.Covers` or
  `req.NotYetBuilt` in every `Test*` function.
- `internal/provision.Provisioner`'s method signatures reference no
  `k8s.io/*` or `sigs.k8s.io/*` types (this *is* the req 17 test).

### 1.4 Cleanup

Every cluster a test creates is named with the run prefix:
`t<runid>-<short>`. No server change is needed: the API already stamps
`bifrost.dev/cluster-id=<id>` on every Kubernetes object it owns, so the
prefix is visible both through the API (list, filter) and on the cluster
(label selector on `bifrost.dev/cluster-id`). `t.Cleanup` deletes through
the API every cluster carrying the prefix; on real clusters a postflight
lists any Kubernetes object whose `cluster-id` label carries the prefix and
fails the run if the list is non-empty. This deliberately avoids a test-only
server flag that grace's production deployment would have had to enable.

## 2. Traceability

### 2.1 Declaring coverage

```go
func TestCreateReturnsRayClientAddress(t *testing.T) {
    tgt := req.Target(t)
    req.Covers(t, 6, "gateway returns a Ray Client address for the caller's own cluster")
    ...
}
```

`Covers` emits `t.Logf("REQ: covers=6 reason=%q", ...)`. `go test -json`
carries `t.Log` output unchanged, so there is no custom runner and nothing
that can drift from the test it annotates. A test may `Covers` two
requirements when one scenario genuinely proves both; each call must carry
its own reason.

### 2.2 `NotYetBuilt`

```go
req.NotYetBuilt(t, 5, "ephemeral RayJob flow — Wave 2")
```

Semantics are strict and asymmetric:

- The body runs to completion.
- If it **fails**, the requirement reports `not-yet-built`; the test itself
  reports PASS to `go test` (so CI is green) with the REQ line marking the
  expected failure.
- If it **passes**, the test FAILS: *"requirement 5 appears built; remove the
  NotYetBuilt marker in the same PR that made this pass."*

The marker can only be removed by a human, in the PR that builds the thing.
A requirement row cannot turn Built without a test turning green first.

Implementation: `NotYetBuilt` registers a `t.Cleanup` that inspects
`t.Failed()`; it uses a sub-`testing.T` via `t.Run` so the inner failure can
be observed and inverted without leaking into the outer result.

### 2.3 `NeedsCapability`

```go
req.NeedsCapability(t, tgt, "keycloak")   // also: "artifact-keeper", "gateway", "calico"
```

Skips with a recorded reason when the target lacks the capability. Targets
declare capabilities in their config. The report shows *why* an L3 column
is partial, not just a fraction.

### 2.4 `reqreport`

Input: one or more `go test -json` streams plus zero or more JUnit XML files
(§7). Output: `docs/requirements/traceability.md` plus `traceability.json`.

| # | Requirement | Priority | Tests | L2 | L3 | Status |
|---|---|---|---|---|---|---|

Rules:

- **Status is derived, never typed.** `Built` ⇔ every covering test passed
  on the most recent L3 run *and* no `NotYetBuilt` marker exists in the row.
  `Partial` ⇔ some tests pass on L3 and at least one `NotYetBuilt` remains.
  `Not yet built` ⇔ all tests carry the marker. `Untested` ⇔ zero tests —
  and **that is a hard failure of the report**: 18 rows means 18 populated
  rows.
- The report records skips with reasons under each row.
- CI regenerates the report and fails if the committed file differs. The
  committed file is therefore always the truth of the last run.
- `docs/SPEC.md`'s status column is replaced by a link to the row in
  `traceability.md`. The SPEC stops carrying its own status.

## 3. Lanes and gating

| Lane | Question | Target | Trigger | Budget |
|---|---|---|---|---|
| L1 | Does each unit do what it says? | none | every push | < 2 min |
| L2 | Does the wiring work end to end? | inproc | every push, **merge-blocking** | < 3 min |
| L3 | Does it work on real Kubernetes with an enforcing CNI? | kind + Calico | nightly; `workflow_dispatch` on any PR | ~25 min |
| L3-grace | Does it work in the real environment, for the real consumers? | grace | nightly after the kind lane, and `make test-l3 TARGET=grace` | ~20 min |
| Soak | Does it stay correct under load? | kind + Calico | nightly, separate job; reported, not gating | ~30 min |

**L3 must use Calico.** kindnet ignores NetworkPolicy, so isolation tests
pass vacuously on it. The kind job is lifted from `rayserve-pack`'s
`.github/workflows/test.yaml` (no default CNI, then Calico). The lane's
first test, `TestCNIEnforces`, creates a deny-all policy and asserts a probe
is *blocked*; if it is reachable the lane is invalid and every subsequent
test is failed with that reason. A green isolation result must mean
something.

**L3-grace is the same suite with a different config**, plus the
consumer integration tests in §5a that only grace can host. See §6 for the
preflight/postflight that keeps a scheduled run from leaving anything
behind.

**Soak** pass criteria (initial; ratcheted after a week of baseline):
20 concurrent creates converge within 10 min; **every one is cleaned up**
(req 8 under load); Jobs-gateway p99 < 250 ms at 5 submits/s sustained for
10 min; reconcile tick p95 flat (< 2× baseline) from 1 → 40 clusters.

## 4. Coverage and mutation

| Tier | Packages | Line | Mutation (gremlins) |
|---|---|---|---|
| 1 | `internal/auth`, `internal/policy`, `internal/core` | **≥ 95%** | **≥ 80%** killed, gating |
| 2 | `internal/api`, `internal/controller`, `internal/provision`, `internal/app` | **≥ 90%** | measured & reported, not gating |
| 3 | `internal/provision/live`, `cmd/bifrost` | excluded (as today) | — |

- Generated code (`zz_generated_api.go`, `pkg/client`) excluded from the
  denominator. **The exclusion list lives in one file
  (`.coverage-exclude`) and `covreport` prints it** so "excluded" is never
  invisible.
- **Ratchet, not cliff.** `.coverage-ratchet.json` holds the last passing
  per-tier numbers. A PR may lower a number by at most 0.5 points; on merge
  the floor rises to the new value. The existing 80% single gate is
  replaced by this. The first framework PR sets the ratchet from current
  numbers and prints the gap per package.
- Mutation runs on Tier 1 only, nightly and on PRs touching Tier 1 files.

## 5. Test map (69 tests; 22 `NotYetBuilt` on day one)

Lane column: L2 = inproc-capable; L3 = needs Kubernetes; G = grace-only
capability; A = adapter-fed (Playwright/pytest).

**#1 Deploy models from Jupyter** (CRITICAL) — 3, all `NotYetBuilt`
- deploy RayService via API → 201, converges to serving (L3)
- service endpoint answers only through the gateway with a token (L3)
- deploy from the extension panel (A)

**#2 Groups share models privately** (HIGH) — 4
- owning-group member → 200 (L2/L3)
- non-member → 403 (L2/L3)
- anonymous → 401 (L2)
- second deploy in same group reuses the one RayService (L2) `NotYetBuilt`

**#3 RBAC; direct access blocked** (HIGH) — 7
- generated role × operation matrix: a checked-in `permissions.yaml` in the test tree states, for every operation, which of the four roles may call it; the test walks the contract and asserts the server agrees. Drift between the file and the server is the failure (L2)
- developer cannot create cluster (L2)
- :8265 dashboard unreachable from another owner's pod (L3, Calico)
- :10001 Ray Client unreachable cross-owner (L3)
- :6379 GCS unreachable from the jupyter namespace (L3)
- head `/api/jobs` unreachable directly; reachable via gateway (L3)
- gateway strips caller credential and swaps southbound (L2)

**#4 Serving in separate resource pool** (CRITICAL) — 2, `NotYetBuilt`
- RayService admitted to the serving queue, not compute (L3)
- compute cluster cannot consume serving quota (L2)

**#5 Ephemeral RayJob** (CRITICAL) — 4, `NotYetBuilt`
- submit → RayCluster created (L3)
- job completes → cluster removed (L3)
- job fails → cluster removed, status failed (L3)
- UI submit path (A)

**#6 Self-serve private clusters** (CRITICAL) — 9
- create → 201 with Ray Client address (L2/L3)
- list shows only caller's clusters (L2)
- stop own → terminated (L2/L3)
- stop another's → 403 (L2)
- idle timeout → cleanup (L2 fake clock; L3 real)
- disallowed option → 400 (L2)
- own dashboard via proxy → 200; another's → 403 (L3)
- **owner's Ray Client connect from a jupyter-namespace pod succeeds; non-owner's is blocked** (L3). Replaces `grace-e2e/tests/cluster/netpol-probe.spec.ts`, which asserts an *arbitrary* pod reaches :10001 — the opposite of the requirement. The old spec is deleted in the same change.
- two clusters have distinct head services (no shared head) (L3)

**#7 Group-admin controls** (CRITICAL) — 5
- quota set → over-quota create 409 (L2)
- max workers set → exceeded 400 (L2)
- image allowlist → disallowed image 400 (L2) `NotYetBuilt`
- profile defined → user selects by name (L2) `NotYetBuilt`
- developer cannot update policy → 403 (L2)

**#8 Automatic cleanup after gateway failure** (CRITICAL) — 5
- kill bifrost mid-provision → restart → converges (L3)
- kill bifrost after delete accepted → restart → reaped (L3)
- ownership labels present on the RayCluster (L3)
- record survives process restart (L2, sqlite file)
- **an unactionable record neither blocks other clusters nor retries unbounded; it is dead-lettered and deletable** (L2) ← defect 1

**#9 Start/stop from JupyterLab** (CRITICAL) — 3 (A, existing pytest)

**#10 nebi environments** (CRITICAL) — 3
- **image with no `wget`/`curl` reaches `ready`** (L3) ← defect 2
- artifact-keeper image ref pulls and runs (L3, G) `NotYetBuilt`
- allowlist accepts a registry prefix (L2) `NotYetBuilt`

**#11 Env vars** (CRITICAL) — 2
- editor → `runtime_env.env_vars` (A)
- submitted job observes `$FOO` (L3)

**#12 Private S3** (CRITICAL) — 2, `NotYetBuilt`
- credentials reach pods via secret ref, never in spec (L3)
- credentials never appear in any API response or log line (L2)

**#13 Fair queueing** (LOW) — 3
- two projects, one pool: quota per project (L2)
- LocalQueue per project (L3)
- weights respected (L3) `NotYetBuilt`

**#14 Usage visibility** (LOW) — 2, `NotYetBuilt`
- **cluster runs N min → report shows owner, duration, cost > 0** (L2 fake clock; L3) ← defect 3
- report attributes to the requesting principal (L2)

**#15 Health / pending reasons** (LOW) — 2
- unschedulable → reason surfaced via API (L3)
- 5 observability ops: own → 200, another's → 403 (L2)

**#16 Ray/Dask same UX** (LOW) — 2, `NotYetBuilt`
- `engine=dask` → DaskCluster provisioned (L3)
- list/stop identical for dask (L2)

**#17 Slurm non-foreclosure** (LOW) — 1
- AST guard: `Provisioner` signature carries no k8s types (L1)

**#18 NIST baseline** (LOW) — 5
- audit chain verifies after N events (L2)
- **audit `status` equals the actual outcome; a create that persists nothing valid is not audited 201** (L2) ← defect 1
- container runs non-root, static binary (L3: securityContext + `id`)
- no secret material in captured logs (L2)
- FIPS build (L1) `NotYetBuilt`

**Contract** (credited to #3 and #18) — 2 generated tests, all operations
- **every operation × every `required` field omitted → 400** (L2) ← defect 1
- every non-public operation without a token → 401 (L2)

**Pack** (credited to #6 and #8 deployability) — 3
- **nginx `proxy_pass` uses a variable and a `resolver`** (L1, `helm template`) ← defect 4
- `image.tag` required; render refuses without it (L1)
- kind install → all pods ready (L3)

## 5a. Consumer integration on grace (the end goal, as tests)

The point of bifrost on grace is that two existing packs use it for Ray.
These tests are `NeedsCapability(t, tgt, "consumers")` — grace only — and
are credited to the requirements they exercise. They are the acceptance
tests the user will verify by hand; the framework runs them first.

- **data-science-pack → bifrost** (credits #6, #9, #11): from a pod in the
  `jupyter` namespace running the data-science-pack image, using
  `bifrost-jupyter`'s client path: create a cluster → receive the Ray
  Client address → `ray.init(address)` succeeds → a trivial remote task
  returns → stop → cluster gone. A second jupyter pod as a different
  principal cannot connect to that address (Calico + per-owner policy).
- **checkmaite → bifrost** (credits #3, #6): from the `checkmaite`
  namespace, `ray job submit` against the bifrost Jobs gateway with a
  checkmaite-held token → job runs on a cluster owned by that principal →
  logs stream back through the gateway → a direct submit to the head's
  :8265 is refused by the network.
- **The rayserve-pack shared cluster is untouched** (credits #4 by
  negation, until #4 is built): bifrost-managed traffic never lands on the
  `ray` namespace's shared head.

These are the tests that answer "does it work for the people it is for",
which is a different question from "does it satisfy row N".

## 6. Real-cluster safety (kind and grace)

The same preflight/postflight wraps every L3 run; grace adds refusals.

**Preflight**
- Assert `REQ_TARGET` matches the kubeconfig context by an explicit
  allowlist in `targets.yaml`; refuse otherwise.
- On grace: refuse unless namespace is exactly `bifrost`; refuse if any
  cluster with a `t*-` run prefix already exists (a previous run leaked; a
  human cleans it, not the next run).
- Snapshot `helm list -A` (release, revision) and the count of pods per
  namespace **outside** the target namespace.
- Run `TestCNIEnforces` first; on failure, fail the lane.

**Postflight**
- Delete by run prefix; list what remains; fail if non-empty.
- Re-snapshot and **diff against preflight**: any revision change or pod
  count change outside the target namespace fails the run *loudly*, with
  the diff. This encodes the manual check done by hand on 2026-09-02.

**Never**
- No test may `helm upgrade`, `kubectl delete ns`, or touch any object
  outside the run prefix. The cluster target's `K8s()` client is wrapped:
  Delete/Patch/Update on an object whose `bifrost.dev/cluster-id` label
  lacks the run prefix returns an error.
- Grace runs are scoped to the `bifrost` namespace plus read-only probes
  from the `jupyter` and `checkmaite` namespaces (§5a). The consumer packs
  are never upgraded, restarted or reconfigured by a test: they are what we
  are integrating *with*.

## 7. Non-Go feeds

Playwright (`grace-e2e`) and pytest (`bifrost-jupyter`) keep their homes.
They participate by convention, not by rewrite:

- Test titles include `[req:N]`. Playwright: `test('[req:9] start panel creates a cluster', ...)`.
  pytest: a `@pytest.mark.req(9)` marker whose plugin appends `[req:9]` to
  the JUnit `name`.
- Both emit JUnit XML (Playwright `reporter: junit`; pytest `--junitxml`).
- `reqreport` ingests JUnit; a `[req:N]` title contributes to row N in the
  A column. A JUnit test without a tag is ignored, and the count of
  ignored tests is printed.

## 8. Error handling and failure modes of the framework itself

- **Target unreachable** (L3): fail fast in preflight with the URL and the
  error; do not run tests that will all time out.
- **Flaky convergence**: `req.Eventually(t, timeout, fn)` with the timeout
  chosen per lane (`inproc` 5 s, cluster 5 min). No bare `time.Sleep` in
  tests; the AST guard forbids it under `r??_*/`.
- **Marker drift**: `NotYetBuilt` on a passing test fails the suite (§2.2).
  `Covers` on a requirement number > 18 or < 1 panics at test start.
- **Report drift**: committed `traceability.md` ≠ regenerated → CI fails.
- **Leaked resources**: postflight fails the run and prints them; a
  subsequent grace run refuses to start until a human clears them.
- **Clock misuse**: calling `Clock().Advance` on a cluster target panics
  with "use req.AdvanceOrWait".

## 9. Definition of done

The framework is done when, on `main`:

1. `traceability.md` exists with **18 populated rows**, regenerated by CI,
   and `docs/SPEC.md`'s status column links to it.
2. L2 runs on every push in under 3 minutes and is merge-blocking.
3. L3 runs nightly on kind+Calico; `TestCNIEnforces` guards it.
4. The L3-grace lane runs nightly with preflight/postflight, and the §5a
   consumer tests pass: a data-science-pack pod and checkmaite each drive
   Ray through bifrost.
5. All four 2026-09-02 defects have a named test. Defects 1, 2 and 4 are
   fixed and their tests pass (validation middleware; interpreter-based
   probes; nginx resolver). Defect 3's test fails under `NotYetBuilt` until
   the metering loop lands — that is Wave 3 scope, not a bug fix.
6. Tier 1 coverage ≥ 95% and mutation ≥ 80%, gated by ratchet.
7. `netpol-probe.spec.ts` is gone and its replacement asserts the
   requirement.

## 10. Phasing (input to the implementation plan)

- **P0 — Core.** `internal/app.New` extraction; `pkg/client` generation;
  `req` package; `inproc` target; `reqreport`/`covreport`; L2 CI job;
  ratchet from current numbers; the two generated contract tests; the
  three pack tests. *Defect 1 and defect 4 get their tests here, and are
  fixed here* (validation middleware; nginx resolver).
- **P1 — L2 requirement tests.** All L2-capable tests for #3, #6, #7, #8,
  #13, #15, #18, plus every `NotYetBuilt` test across all rows.
  `traceability.md` has 18 rows at the end of P1.
- **P2 — L3 lane.** kind+Calico workflow; `cluster` target; preflight/
  postflight; `TestCNIEnforces`; isolation tests; restart tests; *defect 2
  gets its test and its fix here* (probes use the interpreter, not `wget`).
- **P3 — Adapters and grace.** `[req:N]` convention in Playwright and
  pytest; JUnit ingestion; delete `netpol-probe.spec.ts`; grace target and
  Makefile; one confirmed grace run.
- **P4 — Quality gates.** gremlins on Tier 1; soak lane; SPEC status
  column → link.

Each phase is independently mergeable and leaves CI green.
