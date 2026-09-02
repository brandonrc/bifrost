# Session handoff — requirement test framework, P0 (2026-09-02)

Read this first in a new session. It says where everything is, what is done,
what is in flight, and how to resume without re-doing finished work.

## Where things are

| Thing | Location |
|---|---|
| Code branch | `bifrost` repo, branch `feat/req-framework-p0`, pushed to `origin` at `7193cc3` (18 commits over `main`) |
| Worktree | `/Users/khan/openteams/bifrost/.claude/worktrees/req-framework-p0` — enter it with `EnterWorktree path=…`; all P0 work happened here, `main` is untouched |
| Draft PR | `feat/req-framework-p0 → main`, opened only so CI runs (ci.yml triggers on push-to-main or PR). Check: `gh pr list --head feat/req-framework-p0`; `gh pr checks` |
| Spec | `docs/superpowers/specs/2026-09-02-requirement-test-framework-design.md` (binding authority) |
| Plan (P0) | `docs/superpowers/plans/2026-09-02-requirement-framework-p0-core.md` — 11 tasks |
| SDD ledger (live) | `<worktree>/.superpowers/sdd/2026-09-02-requirement-framework-p0-core/progress.md` — git-ignored; snapshot committed beside this file as `2026-09-02-p0-ledger-snapshot.md` |
| Task briefs / reports / review packages | same `.superpowers/sdd/...` dir: `task-N-brief.md`, `task-N-report.md`, `review-<base>..<head>.diff` |
| Grace recon | `2026-09-02-grace-recon.md` beside this file (read-only facts for spec §5a consumer tests) |
| `bifrost-pack` | `/Users/khan/openteams/bifrost-pack` — **local-only repo, no remote**. Branch `fix/nginx-runtime-resolver` holds Task 11a (nginx resolver fix). Cannot be pushed until the user picks an org (`brandonrc` vs `nebari-dev`). |
| `bifrost-ui` | branch `chore/purge-mobula-defaults` at `72d2e25` (mobula purge from functional defaults; 208/208 tests) — local unless pushed |
| `main` (bifrost) | 5 docs-only commits ahead of `origin/main` (defect records, SPEC corrections, spec, plan) — see "pushes" below |

## Task status at handoff

| Task | Status | Commits |
|---|---|---|
| 1 `internal/app.New` | ✅ complete (1 fix round: errcheck) | b4d07bc, 6446dcd |
| 2 `pkg/client` | ✅ complete (1 fix round: document `-response-type-suffix`) | 174aecc, 2b01892 |
| 3 `req` package | ✅ complete (1 fix round: replaced detached-`testing.T` with `req.B` harness; `ParseLine` tolerates `file:line:` decoration) | a81a3be, 42c1d31 |
| 4 `inproc` target | ✅ complete (1 fix round: bcrypt cost knob → 27 s/test → 0.18 s; `Fatalf` not panic) | 0f689e0, 27a8250 |
| 5 validation middleware (**defect 1 fixed**) | ✅ complete (1 fix round: pin media-type enforcement) | 85b5417, 7546b50 |
| 6 generated contract tests (61 subtests, 47 ops) | ✅ complete (1 fix round: non-vacuous field assertion) | 83b122b, ff1b7ae |
| 7 AST guards + req 17 | ✅ complete (1 fix round: `TestMain` exclusion) | dc6b25d, cfcb2d6 |
| 8 `reqreport` | ✅ complete (1 fix round: `skipped` status; cross-file duplicate = error) | 8dcc83e, 93ed1b9 |
| 9 `covreport` + ratchet | ✅ complete (clean) — tier1 81.1 %, tier2 74.4 % local; ratchet `{81, 74.3}` | 4c3fc85 |
| 10 L2 CI job + first matrix | 🔧 implemented `7193cc3`; **review in flight**; **CI run in flight via draft PR** | 7193cc3 |
| 11a pack nginx fix + grace proof | 🔧 in flight in `bifrost-pack` (see below) | — |
| 11b `test/requirements/pack` tests (bifrost) | ⏳ not started — queued behind 10; needs `PACK_CHART=/Users/khan/openteams/bifrost-pack/chart` locally (worktree layout), skips with reason otherwise | — |
| Final whole-branch review | ⏳ not started | — |

Every completed task: `make lint` 0 issues, `CGO_ENABLED=1 go test -race ./...` green, independent reviewer approved after fix round.

## In flight at handoff — how to check each

- **Task 10 CI**: `gh pr checks` on the draft PR. Expected jobs: lint, test (now runs `covreport`), vuln, spec-sync, nilaway-advisory, `requirements-l2`. If `requirements-l2` fails on `git diff --exit-code -- docs/requirements/`, the matrix differed in CI — a real finding (determinism), not a flake to rerun.
- **Task 10 review**: report would be the reviewer's message; if the session is gone, re-dispatch a task review over `review-4c3fc85..7193cc3.diff` with the Task 10 brief + report (both in the SDD dir).
- **Task 11a on grace**: `ssh geraci@grace`, `export KUBECONFIG=/var/snap/microk8s/current/credentials/client.config`; `microk8s helm3 history bifrost -n bifrost` (a new revision = the upgrade landed); `kubectl -n bifrost get pods` (UI pod Running); `kubectl -n bifrost exec <ui-pod> -- grep -E 'resolver|proxy_pass' /etc/nginx/conf.d/default.conf` (must show `resolver <ip> valid=10s` and `proxy_pass $bifrost_upstream`); `kubectl -n kube-system get deploy coredns` (**must be replicas=1** — if 0, the proof was interrupted: `kubectl -n kube-system scale deploy coredns --replicas=1`). Report expected at `.superpowers/sdd/.../task-11a-report.md`; chart commit on `bifrost-pack` branch `fix/nginx-runtime-resolver`.

## How to resume

1. `EnterWorktree path=/Users/khan/openteams/bifrost/.claude/worktrees/req-framework-p0`.
2. Read the live ledger (`.superpowers/sdd/2026-09-02-requirement-framework-p0-core/progress.md`). Tasks with a `complete (` line are DONE — do not re-dispatch. Resume at Task 10 completion (resolve CI ⚠️, close review), then 11b, then the final whole-branch review, then `superpowers:finishing-a-development-branch`.
3. Invoke `superpowers:subagent-driven-development` with the plan file; it will find the ledger and pick up.
4. Process rules that were learned the hard way this session (all ledgered): one implementer writes to the bifrost branch at a time; every dispatch requires `make lint` with complete output in the report; implementers may decline commit trailers on CLAUDE.md grounds — the controller then amends the message (metadata only); `-allow-untested` in reqreport is P0-only and the P1 plan removes it.

## Rulings made on the user's behalf (exhaustive, from the ledger)
- Ruling: local runs of pack tests set PACK_CHART=/Users/khan/openteams/bifrost-pack/chart — the relative default is for the sibling layout and CI, not this worktree — costs nothing if wrong (test skips with reason).
- Ruling: T11 ChartDir emits the skip as t.Log(req.Line{Kind:"skip",...}.Format()) then t.Skip(msg), importing req — so the skip is attributed in the matrix — cost if wrong: one unattributed skip.
- Ruling: T8 status() stands for P0 (L2-only lane; skips are listed per row); P2 must gate "built" on the L3 column per spec §2.4 — cost if wrong: an L2 row reads built while an L3 test is skipped, visible in the same table.
- Ruling: accept `-response-type-suffix HTTPResponse` in pkg/client codegen — the contract has a schema named LoginResponse that collides with the wrapper oapi-codegen would emit; plan text assumed default names. Task 4 must use *client.UpsertAssignmentHTTPResponse / *client.LoginHTTPResponse, ListClustersWithResponse(ctx) with no params, DeleteClusterWithResponse(ctx, id, nil) — cost if wrong: compile errors in Task 4, caught immediately.
- Ruling: every implementer dispatch from here on states `make lint` must pass and its full output goes in the report — cost if wrong: none, it is the existing CI gate.
- Ruling: plan text for NotYetBuilt was defective; the implementer's deviation is accepted in principle pending review of the replacement mechanism — cost if wrong: NotYetBuilt bodies leak t.Cleanup on real clusters, caught by P2 postflight.
- Ruling: NotYetBuilt drops the detached *testing.T. New mechanism: a framework-owned harness `*req.B` implementing a `req.T` interface (Helper Log Logf Error Errorf Fatal Fatalf Skip Skipf Failed Cleanup Name) that *testing.T also satisfies; Fatal/Skip via sentinel panics recovered in NotYetBuilt; any other panic = body failure with the value logged; B.Cleanup forwards to the outer t.Cleanup; B.Log/Error forward to outer t.Log (never Fail); skipped body = neither built nor failed, logged as skip. Covers/NeedsCapability/NeedK8s/Eventually accept req.T. Bodies obtain the Target from the OUTER t. Cost if wrong: bodies cannot use t.Run/t.Parallel/TempDir/Setenv (documented), and P1 tests must follow the outer-target pattern — visible at first P1 test.
- Ruling: ParseLine tolerates Go's `file.go:NN: ` log decoration (optional prefix in the regex) and the round-trip test feeds a decorated line; Task 8's fixture must use decorated lines — cost if wrong: reqreport parses nothing, caught by its own golden test.
- Ruling (visibility, made earlier): commit trailers follow the session's explicit attribution instruction, which states it replaces earlier guidance, over CLAUDE.md's no-trailer rule — cost if wrong: trailers stripped in a follow-up commit-message rewrite before merge; the user sees this list.
- Ruling: Task 4 fix round may touch internal/auth (outside its file list) to add an explicit bcrypt cost knob: `NewLocalAuthenticatorWithCost(store, loginTTLSecs, tokenMaxDays, cost)` + `HashPasswordWithCost(pw, cost)`; `NewLocalAuthenticator`/`HashPassword` keep cost 12 unchanged and a unit test pins that; the knob rejects cost outside [bcrypt.MinCost, bcrypt.MaxCost]; inproc uses bcrypt.MinCost. Spec §3 budget is binding; the alternative (fewer tests, or a slower non-race lane) weakens the lane. Cost if wrong: a production caller could pick the WithCost constructor — mitigated by the name and the pinned-default test.
- Ruling: inproc keeps its testing.TB from New and fails via t.Fatalf on login failure instead of panic — cost if wrong: Fatalf from a non-test goroutine misbehaves; API() is only called from test goroutines today, documented on the method.
- Ruling: keep 400 (not 415) for media-type rejections — the API has one client-actionable 400 shape and the contract declares no 415 response; a 415 would itself be an undeclared status — cost if wrong: a later contract revision adds 415 and this changes; pinned by a test either way.
- Ruling: when an implementer refuses the trailer amend, the controller amends the message itself — metadata only, never content — cost if wrong: none beyond the standing trailer ruling.
- Ruling (supersedes the earlier P0 status() ruling): status derivation becomes — untested: Tests==0; failing: Failed>0; not-yet-built: NYB==Tests; skipped: Passed==0 && Skipped>0 && NYB==0 (tests exist, none ran on this lane; counts as populated, not a hard failure); partial: NYB>0 || Skipped>0 (something outstanding); built: Passed==Tests. Spec §2.4 gains the "skipped" value — docs edit queued for the final-review fix wave. Cost if wrong: a lane with legitimately skipped tests never reads built until L3 fills the column — which is exactly what §2.4 asks for.
- Ruling: a Test name appearing in more than one -in file is a hard error ("appears in both X and Y"), not a merge — P0 has one input; P3's JUnit feed gets its own namespace when designed. Cost if wrong: a legitimate rerun-merge use case is blocked loudly rather than silently wrong.
- Ruling: Task 11 split — 11a (bifrost-pack repo: chart fix + grace DNS-down proof) runs NOW in parallel with Task 9 because it touches a different repo; 11b (bifrost: test/requirements/pack) queues after Task 10. Cost if wrong: none — disjoint trees.
- Ruling: Task 10 omits the bifrost-pack checkout step — github.com/brandonrc/bifrost-pack does not exist (the pack repo has no remote; org decision pending with the user). pack/ tests skip in CI with a recorded reason until it does; a comment in ci.yml names the condition. Cost if wrong: pack template tests run only locally until the remote exists.
- Ruling: reqreport ignores events whose Package has suffix /test/requirements/req (framework self-tests are not requirement tests); implemented in Task 10 by ruling (cross-task edit to cmd/reqreport), pinned by a fixture event; doc comment names why. Cost if wrong: a future genuine requirement test placed in req/ would be invisible — the guards already forbid Covers-bearing tests there by exemption, so none should exist.
- Ruling: ci.yml triggers only on push-to-main or pull_request; a DRAFT PR feat/req-framework-p0 → main is opened to run CI (also the eventual integration path; draft cannot be merged accidentally). Cost if wrong: an early PR visible to the repo owner — who is the user.

## Deferred minors (for the final whole-branch review to triage)
See the ledger snapshot's `minor (deferred)` lines. Notable: set `openapi3.SchemaErrorDetailsDisabled` in `internal/api/validate.go` (shrinks 400 bodies; check it silences both openapi3filter error shapes); add `http.MaxBytesReader` ahead of validation on the control-plane path; `/docs` is in the auth allowlist but nothing serves it; spec §2.4 needs the `skipped` status value added; `docs/defects/2026-09-02-health-probes-assume-wget.md` understates severity — the liveness probe **restarts** the pod every ~10 min (team-b-scoring: 66/117 restarts in 11 h), not just "never ready".

## Grace state (playground per the user)
- bifrost release rev ≥7 in ns `bifrost`, image `ghcr.io/brandonrc/bifrost:sha-95dcede`, UI `ghcr.io/brandonrc/bifrost-ui:latest`; API 401 unauth, healthz 200.
- `team-b-scoring` RayCluster (created today via the API, project team-b) — crash-looping on the wget liveness probe (defect 2). Safe to delete via the API if it becomes noise.
- **mobula fully removed** (release, ns, RayCluster, 5 NebariApps, 5 HTTPRoutes, 3 PVCs, retained 50 Gi PV). 12 releases remain at prior revisions.
- Consequence found by recon: checkmaite's `CHECKMAITE_RAY_JOBS_ADDRESS=https://ray-gw.100-89-230-107.sslip.io` pointed at mobula's deleted route — **checkmaite's Ray path is currently dead**; bifrost's gateway must serve that host or checkmaite is repointed (P2 / spec §5a decision for the user).
- jupyter `singleuser` NetworkPolicy has no egress to `bifrost`; KubeSpawner sets no `bifrost.dev/owner` label — consumer path (a) needs data-science-pack changes.
- Node: 40 CPU / ~108 Gi allocatable, ~31 %/36 % requested.

## Pushes / durability at handoff
- `feat/req-framework-p0` pushed (`7193cc3` + this handoff commit).
- bifrost `main`: 5 docs commits pushed with this handoff (docs only).
- `bifrost-ui` `chore/purge-mobula-defaults`: pushed if the push below succeeded (see session log); otherwise local.
- `bifrost-pack`: **cannot be pushed** (no remote). It lives only at `/Users/khan/openteams/bifrost-pack`. Decision needed: create `github.com/brandonrc/bifrost-pack` (or under `nebari-dev`) and push `main` + `fix/nginx-runtime-resolver`.

## Requirement status (verified against code and cluster, not the doc)
6 of 18 built: #3, #6, #8, #9, #11, #15 — roughly 35–40 % by priority weighting. #14 and #16 were marked Built in SPEC.md for components that do not exist (corrected in `63b4014`). Remaining CRITICAL: #1, #2, #4, #5, #7, #10, #12. Once P1 lands, `make report` regenerates this from tests.

## Next phases (each needs its own plan from the spec's §10)
P1 L2 requirement tests for all 18 rows (removes `-allow-untested`); P2 kind+Calico lane, cluster target, `TestCNIEnforces`, defect-2 probe fix, §5a consumer tests; P3 Playwright/pytest JUnit feeds, delete `grace-e2e/tests/cluster/netpol-probe.spec.ts`; P4 gremlins mutation on tier 1, soak lane, SPEC status column → links.
