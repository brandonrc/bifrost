# SDD ledger — plan: docs/superpowers/plans/2026-09-02-requirement-framework-p0-core.md
Spec: docs/superpowers/specs/2026-09-02-requirement-test-framework-design.md (reachable; binding authority)
Worktree: .claude/worktrees/req-framework-p0  branch feat/req-framework-p0  BASE de79cb7
Baseline: 10 packages ok, 0 failures (race). bifrost-pack repo edited in place on a branch (Task 11).

## Pre-flight conflict scan
| Pair / Task | Produces vs consumes | Finding |
|---|---|---|
| T1↔T4 | app.Config{Store,Registry,Validator,Local,Provisioner,ServiceProvisioner,AllowUnauthenticated,ReconcileInterval}, New, RunLoops(ctx) | match |
| T2↔T4,T6 | client *WithResponse names, *JSONRequestBody aliases | match; ListClusters params arity flagged in plan as check-at-build |
| T3↔T4 | Target: Name API As K8s Namespace Clock Has BaseURL Authorize Cleanup (10) | inproc implements all 10 |
| T3↔T8 | Line.Format / ParseLine; fixture lines | fixture matches regex; T8 TrimSpaces |
| T3↔T7 | guards exclude req/ from Sleep rule; rule-2 dirs contract/pack/r?? | consistent |
| T5↔T6 | 400 message names field (openapi3filter "property \"id\" is missing"); validator inside RequireAuth ⇒ anon 401 | consistent |
| T5↔T4 | inproc valid body carries every required field incl. worker_groups items | consistent |
| T8↔T10 | reqreport flags -in -lane -out -allow-untested | consistent in Makefile+CI |
| T2,T9,T10 | ci.yml edited thrice, sequential | no overlap |
| T10↔T11 | CI checkout ../bifrost-pack vs helm.go ../../../../bifrost-pack/chart | correct for sibling layout & CI; WRONG inside this worktree (resolves under .claude/worktrees/) |
| T11 self | ChartDir's Skipf embeds a REQ line mid-message | will NOT parse (ParseLine is whole-line anchored) |
| T8 self | status(): Passed>0,Skipped>0,NYB=0 ⇒ "built" | overstates vs spec §2.4 in a lane with skips |
| T1 self | serve_test.go uses built.handler/.store | plan updates them |
| T5 self | TestValidRequestBodyReachesHandler uses worker_groups: [] | risk if schema has minItems; adjust fixture not validator |

Ruling: local runs of pack tests set PACK_CHART=/Users/khan/openteams/bifrost-pack/chart — the relative default is for the sibling layout and CI, not this worktree — costs nothing if wrong (test skips with reason).
Ruling: T11 ChartDir emits the skip as t.Log(req.Line{Kind:"skip",...}.Format()) then t.Skip(msg), importing req — so the skip is attributed in the matrix — cost if wrong: one unattributed skip.
Ruling: T8 status() stands for P0 (L2-only lane; skips are listed per row); P2 must gate "built" on the L3 column per spec §2.4 — cost if wrong: an L2 row reads built while an L3 test is skipped, visible in the same table.

Task 1: implemented (commit b4d07bc); review dispatched on review-de79cb7..b4d07bc.diff
Task 1: minor (deferred): internal/app/app_test.go has no direct RunLoops test (nil-Provisioner no-op; pool loop only when PoolProvisioner)
Task 1: ⚠️ commit trailers — resolved by controller: git log -1 %B shows both exact trailers
Task 1: complete (commits de79cb7..b4d07bc, review clean)
Task 2: BASE b4d07bc; implementer dispatched (haiku)
Task 2: implemented (commit 174aecc); review dispatched on review-b4d07bc..174aecc.diff
Ruling: accept `-response-type-suffix HTTPResponse` in pkg/client codegen — the contract has a schema named LoginResponse that collides with the wrapper oapi-codegen would emit; plan text assumed default names. Task 4 must use *client.UpsertAssignmentHTTPResponse / *client.LoginHTTPResponse, ListClustersWithResponse(ctx) with no params, DeleteClusterWithResponse(ctx, id, nil) — cost if wrong: compile errors in Task 4, caught immediately.
Task 2: minor (deferred): pkg/client/client_test.go:29-30 asserts JSON200 != nil only; should assert decoded Name/version fields
Task 2: review verdict Approved with 1 Important (undocumented -response-type-suffix rationale in gen.go) -> fix round 1 dispatched to impl-t2
Task 1: REOPENED — make lint fails: internal/app/app_test.go:23 errcheck (resp.Body.Close unchecked). Plan-mandated code; implementer+reviewer both skipped lint. Fix round 1 queued behind Task 2 fix to avoid concurrent commits on one branch.
Ruling: every implementer dispatch from here on states `make lint` must pass and its full output goes in the report — cost if wrong: none, it is the existing CI gate.
Task 2: fix round 1/5 dispatched-fix landed (commit 2b01892); scoped re-review dispatched on review-174aecc..2b01892.diff
Task 1: fix round 1/5 dispatched to impl-t1 (errcheck at internal/app/app_test.go:23)
Task 2: fix round 1/5 (1 addressed, 0 open — gen.go comment; commits 174aecc..2b01892)
Task 2: complete (commits b4d07bc..2b01892, review clean after 1 fix round)
Task 1: fix round 1/5 fix landed (commit 6446dcd, `_ = resp.Body.Close()` per middleware_probes_test.go pattern); scoped re-review dispatched
Task 3: BASE 6446dcd; implementer dispatched (sonnet) in parallel with T1 re-review (read-only)
Task 1: fix round 1/5 (1 addressed, 0 open — errcheck; commits 2b01892..6446dcd)
Task 1: complete (commits de79cb7..b4d07bc + 6446dcd, review clean after 1 fix round)
Task 3: implemented (commit a81a3be) DONE_WITH_CONCERNS — brief's NotYetBuilt via t.Run cannot invert (subtest failure cascades to parent synchronously); implementer replaced with detached *testing.T on own goroutine. Review dispatched (opus) with focus on detached-T hazards.
Ruling: plan text for NotYetBuilt was defective; the implementer's deviation is accepted in principle pending review of the replacement mechanism — cost if wrong: NotYetBuilt bodies leak t.Cleanup on real clusters, caught by P2 postflight.
Task 3: review verdict ❌ spec / Needs fixes — 6 Important: (1) panicking body reads as "built"; (2) skipping body reads as "built"; (3) detached T's Cleanup never fires (target.Get(st) would leak clusters), undocumented; (4) Parallel/Run/Deadline nil-deref, Context() nil; (5) body logs discarded incl. Covers REQ lines; (6) ParseLine anchors ^\s*REQ: but t.Log prefixes "file:line: " — emit→parse pipeline broken [plan-mandated]. ⚠️ trailers resolved by controller (git log shows both exact).
Task 3: minor (deferred): -race flips zero-value T Failed(); vestigial thunk req.go:73; recorder.Logf no-op so outcome lines unasserted; RunID/Name/Eventually/NeedK8s untested; Eventually never re-checks after final sleep; go mod tidy pruned ~12 unused indirect modules (build green).
Ruling: NotYetBuilt drops the detached *testing.T. New mechanism: a framework-owned harness `*req.B` implementing a `req.T` interface (Helper Log Logf Error Errorf Fatal Fatalf Skip Skipf Failed Cleanup Name) that *testing.T also satisfies; Fatal/Skip via sentinel panics recovered in NotYetBuilt; any other panic = body failure with the value logged; B.Cleanup forwards to the outer t.Cleanup; B.Log/Error forward to outer t.Log (never Fail); skipped body = neither built nor failed, logged as skip. Covers/NeedsCapability/NeedK8s/Eventually accept req.T. Bodies obtain the Target from the OUTER t. Cost if wrong: bodies cannot use t.Run/t.Parallel/TempDir/Setenv (documented), and P1 tests must follow the outer-target pattern — visible at first P1 test.
Ruling: ParseLine tolerates Go's `file.go:NN: ` log decoration (optional prefix in the regex) and the round-trip test feeds a decorated line; Task 8's fixture must use decorated lines — cost if wrong: reqreport parses nothing, caught by its own golden test.
Ruling (visibility, made earlier): commit trailers follow the session's explicit attribution instruction, which states it replaces earlier guidance, over CLAUDE.md's no-trailer rule — cost if wrong: trailers stripped in a follow-up commit-message rewrite before merge; the user sees this list.
Task 3: fix round 1/5 dispatched to impl-t3
Task 3: fix round 1/5 fix landed (commit 42c1d31: req.T/req.B harness; ParseLine decoration); scoped re-review dispatched
Task 3: fix round 1/5 (6 addressed, 0 open — req.T/req.B harness; ParseLine decoration; commits a81a3be..42c1d31)
Task 3: minor (deferred): req_test.go:106-111 dropped the assertion on "remove the NotYetBuilt marker" phrase; reportNotYetBuilt reads b.skipped/b.failed without b.mu (race only if a body leaks a goroutine)
Task 3: complete (commits 6446dcd..42c1d31, review clean after 1 fix round)
Task 4: BASE 42c1d31; implementer dispatched (sonnet)
Task 4: implemented (commit 0f689e0); ~27s/test from bcrypt cost-12 seeding per fresh target — spec §3 L2 budget <3min for the lane; review dispatched with that constraint
Task 4: review ❌ / Needs fixes — 2 Important: (1) L2 budget: bcrypt cost 12 is ~2.24s/op under -race; a fresh inproc target costs ~27s (4 seed hashes 9s + ~8 per-request token verifies 18s); ~52 P1 tests ≈ 24 min vs spec §3 <3 min. (2) inproc token() panics on login failure → one flake kills the whole L2 binary. Minors deferred: Cleanup ignores delete status and its error is discarded; token-cache TOCTOU (double login under t.Parallel); API() rebuilds the client per call.
Ruling: Task 4 fix round may touch internal/auth (outside its file list) to add an explicit bcrypt cost knob: `NewLocalAuthenticatorWithCost(store, loginTTLSecs, tokenMaxDays, cost)` + `HashPasswordWithCost(pw, cost)`; `NewLocalAuthenticator`/`HashPassword` keep cost 12 unchanged and a unit test pins that; the knob rejects cost outside [bcrypt.MinCost, bcrypt.MaxCost]; inproc uses bcrypt.MinCost. Spec §3 budget is binding; the alternative (fewer tests, or a slower non-race lane) weakens the lane. Cost if wrong: a production caller could pick the WithCost constructor — mitigated by the name and the pinned-default test.
Ruling: inproc keeps its testing.TB from New and fails via t.Fatalf on login failure instead of panic — cost if wrong: Fatalf from a non-test goroutine misbehaves; API() is only called from test goroutines today, documented on the method.
Task 4: fix round 1/5 dispatched to impl-t4
Task 4: fix round 1/5 fix landed (commit 27a8250; 26.87s -> 0.18s per test; auth cost knob; Fatalf not panic); scoped re-review dispatched
Task 4: fix round 1/5 (2 addressed, 0 open — auth cost knob + Fatalf; commits 0f689e0..27a8250)
Task 4: minor (deferred): Authorize() lacks t.Helper(); As() still panics on unknown principal (out of ruling scope); Cleanup ignores delete status; token-cache TOCTOU; API() rebuilds client per call
Task 4: complete (commits 42c1d31..27a8250, review clean after 1 fix round)
Task 5: BASE 27a8250; implementer dispatched (sonnet)
Task 5: implemented (7bfa580, DONE_WITH_CONCERNS) — committed without trailers because my ruling crossed its commit; amend requested. Brief corrections: Store.List not ListClusters; GET /docs is NOT mounted by the Go server (pre-existing 404; the auth allowlist still names it) so the pass-through test asserts !=400 only.
Observation (deferred to final review): /docs appears in the public allowlist ("Rust seven") but nothing serves it in Go — allowlist entry for an unmounted path.
Task 5: amended to 85b5417 (trailers added, content unchanged); review dispatched
Task 5: review ✅ spec / Approved with 1 Important: media-type enforcement (missing/non-JSON Content-Type → 400) is an untested behaviour change on the primary write path. Minors deferred: validate_test.go:70 dropped the SpecPath not-404 assertion (signal covered by server_test.go:52 + middleware_probes_test.go:407); set openapi3.SchemaErrorDetailsDisabled to trim multi-KB 400 bodies; no body cap on the control-plane path (pre-existing: generated decoder also read unbounded) — add http.MaxBytesReader ahead of validation; context.Background() in validate.go:56 (plan-mandated); openapi.json embedded twice (gen.go + server.go, plan-mandated); ContractDocument() has no callers yet (plan-mandated, for Task 6/8).
Ruling: keep 400 (not 415) for media-type rejections — the API has one client-actionable 400 shape and the contract declares no 415 response; a 415 would itself be an undeclared status — cost if wrong: a later contract revision adds 415 and this changes; pinned by a test either way.
Task 5: fix round 1/5 dispatched to impl-t5
Task 5: fix round 1/5 fix landed (commit 7546b50: media-type test + doc; charset param accepted); scoped re-review dispatched
Task 5: fix round 1/5 (1 addressed, 0 open — media-type pin; commits 85b5417..7546b50)
Task 5: complete (commits 27a8250..7546b50, review clean after 1 fix round). Defect 1 FIXED.
Task 6: BASE 7546b50; implementer dispatched (sonnet)
Task 6: implemented (69ebc9b, no trailers again despite dispatch instruction — amend requested). 18 required-field cases / 10 body ops / 43 non-public ops / 4 public / 47 total; ~2.2s wall under -race. Brief floor "~26" was my arithmetic error; implementer corrected to <15 with real counts. RED with validator bypassed: missing spec → 201, missing id → still 400 (domain guard) — defence in depth proven.
Task 6: implementer declined the trailer amend on principle (peer message vs CLAUDE.md); controller amended message only → 83b122b (3 files, 231 insertions, identical content). Review dispatched.
Ruling: when an implementer refuses the trailer amend, the controller amends the message itself — metadata only, never content — cost if wrong: none beyond the standing trailer ruling.
Task 6: review ✅ spec / Approved with 1 Important: required_test.go:96-98 strings.Contains(raw, field) is vacuous — openapi3filter dumps the enclosing schema into every 400 (SchemaErrorDetailsDisabled never set), so every property name appears regardless of cause. Minors deferred: dummy() has no minItems/minLength awareness; redundant loop-var shadowing (brief verbatim). Follow-up for final review: set openapi3.SchemaErrorDetailsDisabled in validate.go (Task 5 code; production error bodies shrink).
Task 6: fix round 1/5 dispatched to impl-t6
Task 6: fix round 1/5 fix landed (1dce603 → amended ff1b7ae with trailers; assertion now matches "property %q is missing" against the decoded JSON message; 8/18 cases were vacuous under a full-schema-dump error shape, 10 under a MultiError shape were not). Scoped re-review dispatched.
Observation (final review): implementer asks whether SchemaErrorDetailsDisabled silences both openapi3filter error shapes or only the single-SchemaError one.
Task 6: fix round 1/5 (1 addressed, 0 open — reason-phrase assertion; commits 83b122b..ff1b7ae)
Task 6: complete (commits 7546b50..ff1b7ae, review clean after 1 fix round)
Task 7: BASE ff1b7ae; implementer dispatched (sonnet)
Task 7: implemented (1e93f68 → amended dc6b25d with trailers); 3 RED proofs recorded; Rule-2 filter refactored to isReqDir for staticcheck QF1001. Review dispatched.
Task 7: review ✅ spec, 1 Important (plan-mandated): Rule 2 has no TestMain exclusion → false-positive on legitimate shared-setup TestMain in contract/pack/r?? packages. Named risks 1-4 all clean. ⚠️ trailers resolved by controller (git log shows both). Report tail truncated after Important 1; not re-requested — finding is unambiguous.
Task 7: fix round 1/5 dispatched to impl-t7
Task 7: fix round 1/5 fix landed (9203b84 → amended cfcb2d6; TestMain excluded, narrowness proven with sibling fixture). Scoped re-review dispatched.
Task 7: fix round 1/5 (1 addressed, 0 open — TestMain exclusion; commits dc6b25d..cfcb2d6)
Task 7: complete (commits ff1b7ae..cfcb2d6, review clean after 1 fix round)
Task 8: BASE cfcb2d6; implementer dispatched (sonnet)
Task 8: implemented (ee8a0b9 → amended 8dcc83e with trailers); golden read: r3 failing, r5 not-yet-built, r6 skip-only renders BUILT (implementer flagged), r7 parent/subtest failing, 14 untested. Review dispatched.
Task 8: review ❌ / Needs fixes — 2 Important: (1) status() renders "built" for a skip-only row, contradicting spec §2.4 "Built ⇔ every covering test passed"; (2) same Test name across two -in files silently merges lines (double-counts) and clobbers outcome. Minors deferred: no bounds check on l.Req in absorb (malformed REQ panics); no fixture logging REQ inside a subtest; lint/race claims self-reported.
Ruling (supersedes the earlier P0 status() ruling): status derivation becomes — untested: Tests==0; failing: Failed>0; not-yet-built: NYB==Tests; skipped: Passed==0 && Skipped>0 && NYB==0 (tests exist, none ran on this lane; counts as populated, not a hard failure); partial: NYB>0 || Skipped>0 (something outstanding); built: Passed==Tests. Spec §2.4 gains the "skipped" value — docs edit queued for the final-review fix wave. Cost if wrong: a lane with legitimately skipped tests never reads built until L3 fills the column — which is exactly what §2.4 asks for.
Ruling: a Test name appearing in more than one -in file is a hard error ("appears in both X and Y"), not a merge — P0 has one input; P3's JUnit feed gets its own namespace when designed. Cost if wrong: a legitimate rerun-merge use case is blocked loudly rather than silently wrong.
Task 8: fix round 1/5 dispatched to impl-t8
Task 8: fix round 1/5 fix landed (3fc3a4b → amended 93ed1b9; status() skipped/partial branches; cross-file duplicate → error; golden r6 skipped, r8 partial). Scoped re-review dispatched.
Task 8: fix round 1/5 (2 addressed, 0 open — status derivation; cross-file duplicate error; commits 8dcc83e..93ed1b9)
Task 8: minor (deferred): Row.Status doc comment lists values out of evaluation order; absorb has no bounds check on l.Req; no fixture logging REQ inside a subtest
Task 8: complete (commits cfcb2d6..93ed1b9, review clean after 1 fix round)
Task 9: BASE 93ed1b9; implementer dispatched (sonnet)
Ruling: Task 11 split — 11a (bifrost-pack repo: chart fix + grace DNS-down proof) runs NOW in parallel with Task 9 because it touches a different repo; 11b (bifrost: test/requirements/pack) queues after Task 10. Cost if wrong: none — disjoint trees.
Parallel: grace read-only recon for spec §5a consumer tests dispatched (writes only to the SDD workspace).
Task 11a: dispatched (sonnet) on bifrost-pack branch fix/nginx-runtime-resolver
Task 9: implemented (771c6b4 → amended 4c3fc85); tier1=81.1% tier2=74.4% local (no Postgres); ratchet seeded {81, 74.3} — note 81 not 81.1 (float truncation in -update, harmless direction); Step-8 gate failure proven. Review dispatched. Task 10 held until T9 review (both edit ci.yml).
Recon (grace, read-only) — inputs to P2 / spec §5a, see grace-recon.md:
- jupyter `singleuser` NetworkPolicy has NO egress to the bifrost namespace (only ray, mobula[gone], artifact-keeper, DNS, public); KubeSpawner sets no `bifrost.dev/owner` label so bifrost's per-owner ingress rule can never match a notebook pod. Consumer path (a) is blocked by TWO missing pieces on the data-science-pack side, not by bifrost.
- checkmaite is wired to CHECKMAITE_RAY_JOBS_ADDRESS=https://ray-gw.100-89-230-107.sslip.io — that hostname was mobula's `mobula-cluster-shared` route, DELETED with mobula today. checkmaite's Ray jobs path currently points at nothing. Bifrost's federating gateway must serve that host (or checkmaite is repointed) — a P2/§5a decision the user should see.
- Jupyter has a Ray-capable profile image localhost:32000/jupyter-ray:2.56.0-r3 (matches shared cluster Ray 2.56.0); default profiles carry no Ray.
- Node: 40 CPU / ~108Gi allocatable, ~31%/36% requested → ~8-9 concurrent 3CPU/8Gi test clusters fit.
- team-b-scoring head 66 restarts / worker 117 restarts in 11h: defect 2 (wget probes) is not just "never ready" — the LIVENESS probe kills the pod every ~10 min. docs/defects/2026-09-02-health-probes-assume-wget.md understates severity; amend in the final-review docs wave.
Task 9: minor (deferred): report claimed trailers omitted but controller had amended them (report accuracy only); TestRatchetTolerance lacks the exact boundary case have==ratchet-0.5
Task 9: complete (commits 93ed1b9..4c3fc85, review clean, no fix round)
Ruling: Task 10 omits the bifrost-pack checkout step — github.com/brandonrc/bifrost-pack does not exist (the pack repo has no remote; org decision pending with the user). pack/ tests skip in CI with a recorded reason until it does; a comment in ci.yml names the condition. Cost if wrong: pack template tests run only locally until the remote exists.
Task 10: BASE 4c3fc85; implementer dispatched (sonnet)
Task 10: BLOCKED (correctly) — req/req_test.go self-tests call the real NotYetBuilt(t, 5, …) so 4 REQ lines land on row 5 (not-yet-built); guards exempt req/ so nothing caught it.
Ruling: reqreport ignores events whose Package has suffix /test/requirements/req (framework self-tests are not requirement tests); implemented in Task 10 by ruling (cross-task edit to cmd/reqreport), pinned by a fixture event; doc comment names why. Cost if wrong: a future genuine requirement test placed in req/ would be invisible — the guards already forbid Covers-bearing tests there by exemption, so none should exist.
Task 10: implemented (e477187 → amended 7193cc3); reqreport now ignores the req self-test package by Package suffix (fixture-pinned); matrix rows 3/17/18 built, 15 untested; make test-l2 ~6s; push go-ahead given.
Task 10: review dispatched on review-4c3fc85..7193cc3.diff (CI result pending from impl-t10, resolved by controller as ⚠️)
Ruling: ci.yml triggers only on push-to-main or pull_request; a DRAFT PR feat/req-framework-p0 → main is opened to run CI (also the eventual integration path; draft cannot be merged accidentally). Cost if wrong: an early PR visible to the repo owner — who is the user.
