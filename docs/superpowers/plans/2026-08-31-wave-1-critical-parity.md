# Bifrost Wave 1 — Critical Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A working `bifrost serve` — store, reconcile engine, typed KubeRay/Kueue provisioning, OIDC/local auth, the full 47-operation API from the frozen contract, and the federating gateway — passing the store conformance suite and the Ray contract replay. Covers SPEC requirements 3, 6, 7, 8.

**Architecture:** Spec-first API (oapi-codegen v2.8.0 strict-server from the embedded frozen contract), hand-written Store interface over memory/SQLite/Postgres, level-triggered observation-first reconcile, controller-runtime used as a library only (uncached `client.New()` + SSA). Rust reference for every ported behavior: `/Users/khan/openteams/mobula` (branch `fix/openapi-complete-registry` on disk) — each task names its source files; Rust tests are the oracle and port alongside the code.

**Tech Stack:** ADR-0001 stack + ADR-0003 k8s pin set (paste verbatim into go.mod in Task 5). Postgres via pgx v5 + sqlc; SQLite via modernc.org/sqlite (database/sql).

**Spec:** `docs/SPEC.md` · `docs/adr/0001-go-stack.md` · `docs/adr/0002-openapi-codegen-result.md` · `docs/adr/0003-k8s-dependency-pins.md`

## Global Constraints

- All Wave 0 CI gates stay green on every task: race tests, pinned golangci-lint (errcheck/exhaustive/depguard), coverage ratchet (75 → ratchet upward when a task raises the total; never lower it), govulncheck, no-cgo build posture step.
- Conventional commits, NO AI-attribution footers.
- Port fidelity: translate behavior, port the Rust tests; improvements are report notes, not code. The frozen contract's schemas arbitrate all wire formats.
- Established core/policy patterns are binding: MarshalJSON-on-private-alias for nil→`[]`/`{}` and enum defaults; strict enum ingress+egress guards; error types are value-typed with `Unwrap()` chains matching Rust `source` fields; membership-test switches may use `default:` with a comment, state-guard switches may not.
- Security invariants (SPEC): deny-by-default authz; fail closed binding non-loopback without auth; caller credentials never travel southbound; credential-bearing records never marshal (error-guard pattern).
- depguard additions in this wave: `internal/controller` may not import `k8s.io/*`/`sigs.k8s.io/*` (only `internal/provision` may); `internal/api/apitest` importable only from `*_test.go` (the test-util guard).
- New live-k8s wrapper code goes in `internal/provision/live/` (already in `COVERAGE_EXCLUDE`), kept thin; everything testable stays outside it.
- Product identity: user-visible strings say Bifrost; the binary is `bifrost`; `VersionInfo.name` returns `"bifrost"` (rebranded contract is authoritative).
- Audit hash chain (ruling, supersedes SPEC's Wave-3 deferral): no production legacy chains exist, so the chain is implemented fresh — `sha256(prev_hash ‖ canonicalJSON(event))` with the canonical form defined by Go's `encoding/json` field order as declared in `internal/core/audit.go`, documented in an ADR written by Task 1.

---

### Task 1: Store domain types + Store interface + in-memory store

**Files:** `internal/controller/store.go`, `store_memory.go`, `store_memory_test.go`, `docs/adr/0004-audit-chain-format.md`
**Reference:** `/Users/khan/openteams/mobula/crates/mobula-controller/src/store.rs` (types + trait + `InMemoryStore`, incl. `DesiredState` — ledgered Wave-0 pointer: it lives HERE, not core), hash chain at `store.rs:392`.
**Interfaces — produces (later tasks consume verbatim):**
```go
package controller
type DesiredState string // DesiredRunning | DesiredSuspended | DesiredTerminated (serde strings from store.rs)
type StoredCluster struct{ ... } // field-for-field from store.rs
type Store interface { /* every method of the Rust Store trait, ctx-first, (T, error) returns */ }
func NewMemoryStore() Store
```
- [ ] Port every store type and the full `Store` trait; port `InMemoryStore` (mutex-guarded maps, as Rust) with its inline tests.
- [ ] Implement the audit hash chain (fresh canonical format) + chain-verification method; write ADR-0004 documenting the byte format and why no migration exists.
- [ ] `go test -race ./internal/controller/...` green; commit per logical unit.

### Task 2: Store conformance suite (the language-independent oracle)

**Files:** `internal/controller/storetest/conformance.go` (exported `RunConformance(t, func() Store)`), wired to memory store.
**Reference:** `/Users/khan/openteams/mobula/crates/mobula-controller/tests/store.rs` (1408 lines of scenarios).
- [ ] Re-express every Rust scenario as a named subtest in `RunConformance`; run against `NewMemoryStore()`. Scenario parity table (Rust test name → Go subtest) goes in the task report.
- [ ] Commit; this suite is the acceptance gate for Tasks 3–4.

### Task 3: SQLite store

**Files:** `internal/controller/store_sqlite.go` + migrations + `store_sqlite_test.go`.
**Reference:** `store_sqlite.rs` (note its `audit_lock` serialization — reproduce the equivalent).
- [ ] modernc.org/sqlite via database/sql; schema from the Rust migrations; `RunConformance` green against it.

### Task 4: Postgres store

**Files:** `internal/controller/store_postgres.go`, `sqlc.yaml` + query files, CI job addition.
**Reference:** `store_postgres.rs`/Rust SQL; per-test schema isolation as mobula does.
- [ ] pgx v5 + sqlc; `RunConformance` green when `BIFROST_TEST_POSTGRES_URL` set; add a Postgres service container to ci.yml running the conformance suite; gate stays skip-not-fail when the URL is absent locally.

### Task 5: Provision — pure translators (typed KubeRay/Kueue)

**Files:** `internal/provision/{provisioner.go,kuberay.go,kueue.go}` + tests; go.mod gets the ADR-0003 require block.
**Reference:** `/Users/khan/openteams/mobula/crates/mobula-provision/src/{lib.rs,kuberay.rs,kueue.rs}` (kuberay.rs is 1,991 lines — the biggest single translation in the wave). SKIP dask.rs and router.rs multi-engine parts (Wave 3); keep the `Provisioner` interface engine-shaped so they slot in.
**Interfaces — produces:** `type Provisioner interface` (from the Rust trait); `func RayClusterFor(spec *core.ClusterSpec) *rayv1.RayCluster` etc. — typed structs, not unstructured.
- [ ] Port spec→manifest translation to typed `apis/ray/v1` + `apis/kueue/v1beta2` structs. CRITICAL invariants: `replicas` omitted (autoscaler ownership, ADR-0007); every Rust manifest test ports — assert against marshaled YAML/JSON of the typed structs.

### Task 6: Provision — live client

**Files:** `internal/provision/live/client.go` (thin, coverage-excluded).
**Reference:** the `kuberay` feature-gated client code in mobula-provision.
- [ ] controller-runtime `client.New()` uncached, scheme registering ray v1 + kueue v1beta2 + core types; SSA apply with `client.FieldOwner("bifrost")` + `ForceOwnership`; observe/logs/events reads. Compile + vet; no live-cluster CI in this wave (kind e2e is a follow-up workflow, mirrored from mobula's weekly jobs, added disabled-by-default).

### Task 7: Auth — validator + RBAC

**Files:** `internal/auth/{validator.go,rbac.go}` + tests.
**Reference:** `/Users/khan/openteams/mobula/crates/mobula-auth/src/lib.rs` (Validator: OIDC discovery, issuer cross-check #16, JWKS cache with 30s refresh cooldown, RS256 verify; RBAC permission sets, wildcard warnings #35, project_roles #103).
- [ ] go-oidc v3 for discovery/JWKS + golang-jwt where Rust used jsonwebtoken; port every validation test (use generated test keys as Rust tests do). Preserve the fail-fast discover semantics and the refresh cooldown.

### Task 8: Auth — flows + local auth

**Files:** `internal/auth/{flows.go,local.go}` + tests.
**Reference:** `flows.rs` (device code RFC 8628, client credentials, token exchange RFC 8693), `local.rs` (bcrypt cost 12 off the hot goroutine, `mob_*` PAT format — RULING: keep the `mob_` prefix; it's wire/UX-compatible with existing tokens and the contract, rename is a Wave-3 decision if ever). **SUPERSEDED (sub-project A):** the rename was taken — the prefix is now `bfr_` (`auth.TokenScheme`), as a hard cutover with no compatibility window, because the deployment was rebuilt and every token reissued.
- [ ] Port flows + local users/PATs; bcrypt via x/crypto; hashing on a worker goroutine mirroring Rust's spawn_blocking rationale.

### Task 9: Reconcile engine + pool reconciler

**Files:** `internal/controller/{reconcile.go,pool_reconcile.go}` + tests.
**Reference:** `reconcile.rs` (level-triggered, observation-first, idempotency keys, generation + spec-fingerprint drift, TTL/idle reaping from job history, token-bucket limits), `pool_reconcile.rs`. SKIP metering.rs (Wave 3).
**Interfaces — consumes:** `Store` (T1), `Provisioner` (T5). Produces: `func RunReconciler(ctx, Store, Provisioner, Options) error` + pool equivalent.
- [ ] Port the engine with its full test suite (fake provisioner pattern from the Rust tests). Every state-machine switch obeys the exhaustive rules. This is the concurrency heart — expect the race detector to earn its keep; `-race` locally before every commit.

### Task 10: API skeleton — codegen, embed, middleware, health/version

**Files:** `internal/api/{gen.go,server.go,middleware.go,apitest/}` + `.golangci.yml` depguard addition + tests.
- [ ] Vendor the frozen contract (copy from bifrost-api pinned by sha into `internal/api/openapi.json`), `//go:generate go tool oapi-codegen -generate types,std-http,strict-server ...`; commit generated code; `go:embed` serves the spec at the contract's documented path; CI step diffs vendored spec vs bifrost-api HEAD (info-normalized).
- [ ] Auth middleware: deny-by-default, fail-closed non-loopback bind check (port the Rust guard + its tests); `apitest` package with dev-mode constructors + depguard rule confining it to tests.
- [ ] Implement healthz + version (`name: "bifrost"`). Every other strict-server method returns 501 with a canonical body — the interface compiles complete from day one and burns down.

### Task 11: API handlers — clusters, registry, settings, access

**Reference:** `crates/mobula-api/src/{clusters.rs (incl. per-project admission locks #44 — port the lock AND file the store-transaction follow-up), registry.rs, settings.rs, access.rs}`.
- [ ] Port handler logic behind the generated strict-server interface; port the Rust handler tests onto `apitest` fixtures.

### Task 12: API handlers — pools, services, obs, usage, audit, local-auth

**Reference:** `pools.rs, services.rs, cluster_obs.rs, usage.rs, audit.rs, local_auth.rs`.
- [ ] Same protocol; all 501s eliminated — 47/47 implemented, spec-diff CI check green.

### Task 13: Gateway — HTTP federating proxy

**Files:** `internal/api/gateway.go` + tests.
**Reference:** `gateway.rs` (738 lines): Host-match middleware BEFORE route matching (shadowing invariant), credential strip + static Ray token injection southbound, GatewayLimits (64 MiB body, 64 in-flight, timeouts), buffered bodies (streaming stays a tracked follow-up as in Rust).
- [ ] Port HTTP proxying + every gateway test except WS.

### Task 14: Gateway — WebSocket bridge + contract replay

**Files:** `internal/api/gateway_ws.go`, `contract/jobs_client_replay.py` (verbatim copy), `.github/workflows/contract.yml`.
**Reference:** `gateway.rs:403-600` (WS bridge), mobula's `contract/` + `contract.yml`.
- [ ] coder/websocket both legs; frame relay with the Rust limit semantics (semaphore held for bridge lifetime, connect/idle timeouts, frame/message caps).
- [ ] Copy the replay script verbatim; port the workflow to run `ray start --head` + `bifrost serve` and replay through the gateway; runs on-demand + weekly. **Wave exit requires a green replay run.**

### Task 15: CLI — serve/login/token/exchange

**Files:** `cmd/bifrost/*.go`.
**Reference:** `crates/mobula-cli/src/main.rs`. cobra; wire store selection (memory/sqlite/postgres), reconcilers, API server, registry file loading; port the CLI-level tests that exist; binary name `bifrost` everywhere user-visible.

### Task 16: Acceptance — replay + load rig + UI swap + spec sync

- [ ] Run the contract replay end-to-end locally; record the run in the task report. Small load rig (`scripts/gateway-load.sh`, e.g. `hey`/`vegeta` against a local gateway) capturing p99 — the SPEC's REQUIREMENTS-amendment evidence; record numbers in `docs/adr/0005-gateway-p99-evidence.md`.
- [ ] bifrost-ui: swap `@brandonrc/mobula-client` → `@brandonrc/bifrost-client`, rename the CLI literals to `bifrost`, sweep remaining legacy strings (README operational section included); build+tests green; push.
- [ ] bifrost-api: enable the spec-sync CI (server-emitted spec diff-gated against the contract repo, fail-red).
- [ ] SPEC.md status update: Wave 1 exit recorded.

---

## Self-review notes

- Coverage: every SPEC Wave-1 bullet (store+conformance, reconcile, provision typed APIs, auth, API to contract, gateway behind replay) maps to tasks 1–16; reqs 3 (T7/T8/T10-13), 6 (T5/T9/T11), 7 (T11/T12 + policy from Wave 0), 8 (T1-T4/T9).
- Deliberate scope-outs restated: metering loop, Dask adapter/EngineRouter multi-engine, FIPS, standalone proxy (`internal/proxy`) — Wave 3; kind-cluster live e2e workflows land disabled-by-default in T6/T14 and activate in Wave 3.
- Dependency order: T1→T2→{T3,T4}; T5→T6; T7→T8; T9 needs T1+T5; T10 needs T7; T11/T12 need T9+T10; T13→T14 need T11; T15 needs all; T16 last. Parallel lanes where trees don't overlap follow Wave 0's non-overlap ruling.
- Known Wave-0 pointers carried in: DesiredState in store (T1), #44 admission-lock follow-up (T11), registry copies-not-pointers semantics (gateway reads, T13).
