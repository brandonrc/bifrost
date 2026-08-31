# Bifrost — Program Specification

**Bifrost** is a FOSS, Anyscale-grade control plane for Ray and Dask clusters,
written in Go. It is the successor to [mobula](https://github.com/brandonrc/mobula)
(Rust), which remains the **frozen executable reference** until Bifrost reaches
parity. The name: Bifrost is the guarded bridge between realms — a gateway that
decides who crosses. That is requirement 3.

## Why a port, on the record

The Rust implementation is sound. The port exists because the team that will
maintain and support this long-term (Nebari) works in Go, and because the
Kubernetes ecosystem Bifrost drives (KubeRay, Kueue, client-go) is Go-native.
The trade accepted: compile-time data-race freedom and no-GC gateway
determinism (Rust guarantees) become CI-enforced disciplines (Go processes).
The disciplines are therefore **non-negotiable CI gates from day one** (see
"Port governance"). This paragraph supersedes mobula `REQUIREMENTS.md:87`
("Rust for the control plane … no GC pauses in the proxy path"): measured
gateway p99 under the contract-replay load rig is the acceptance evidence.

## Requirements (Ray Software Pack — Desired User Experience)

ALL rows ship. Priority governs **sequence**, never scope-cut.

| # | User need | Priority | Mobula reference status |
|---|---|---|---|
| 1 | Deploy models from within Jupyter | CRITICAL | Partial (RayService + 4 service ops) |
| 2 | Groups share models privately (one shared RayService per group, every request authenticated, caller must belong to owning group) | HIGH | Partial |
| 3 | RBAC for model serving and cluster access; direct Ray Serve/dashboard/Jobs/GCS access blocked | HIGH | **Built** (OIDC, deny-by-default RBAC, gateway) |
| 4 | Serving in separate resource pool from compute clusters | CRITICAL | In flight (Kueue pools) |
| 5 | UI runs jobs via ephemeral RayJob (cluster created per job, removed after) | CRITICAL | **Gap** |
| 6 | Self-serve private clusters (dask-gateway UX): request RayCluster from gateway, Ray Client address back, no shared heads/workers/object store, list/stop own clusters, idle cleanup, approved options only, own-cluster dashboard access | CRITICAL | **Core built** (lifecycle controller, registry) |
| 7 | Group admins control profiles, images, CPU/mem/GPU, max workers | CRITICAL | Partial (policy/quota, flavors) |
| 8 | Automatic cleanup even after gateway failure; ownership recorded; state recovered on restart | CRITICAL | **Built** (persistent Store, observation-first reconcile) |
| 9 | Start/stop clusters from JupyterLab (extension) | CRITICAL | **Not built** (client-side; Python SDK is the substrate) |
| 10 | Use nebi environments on the cluster | CRITICAL | Blocked on nebi + Artifact Keeper |
| 11 | Pass environment variables to the cluster (JupyterLab extension) | CRITICAL | **Not built** |
| 12 | Private storage (e.g. S3) from the cluster | CRITICAL | Needs exploration |
| 13 | Group capacity via shared resource pools; fair queueing; admin quotas/weights | LOW | In flight (Kueue) |
| 14 | Usage visibility: who requested what, duration, estimated cost | LOW | **Built** (metering, price sheets) |
| 15 | Cluster health / pending-reasons without direct K8s access | LOW | **Built** (5 observability ops) |
| 16 | Same UX across Ray and Dask (ports-and-adapters compute contract) | LOW | **Built** (EngineRouter, Dask adapter) |
| 17 | Same UX across Kubernetes and Slurm | LOW | Not built (design must not foreclose) |
| 18 | NIST security baseline operation + audit evidence | LOW | **Built** (audit hash chain, FIPS variant, STIG/UBI images) |

## Architecture (inherited from mobula, translated to Go idiom)

Single Go module `github.com/brandonrc/bifrost`. Crate boundaries become
package boundaries with an enforced import graph (depguard):

| mobula crate | bifrost package | Responsibility |
|---|---|---|
| mobula-core | `internal/core` | Domain model: ClusterSpec/State machine, pools, registry, RBAC records, audit model. **Must not import k8s or DB packages** (mobula ADR-0002). |
| mobula-policy | `internal/policy` | Pure functions: resource accounting, cost estimation, quota admission, K8s quantity parsing. No I/O. |
| mobula-provision | `internal/provision` | The ONLY k8s-aware package. Provisioner interface; spec→manifest translators for KubeRay/Kueue/Dask (typed upstream APIs); EngineRouter. |
| mobula-controller | `internal/controller` | Store interface (memory/SQLite/Postgres) + level-triggered observation-first reconcile engine, pool reconciler, metering loop. |
| mobula-auth | `internal/auth` | OIDC discovery/JWKS/RS256 validation, RBAC permission sets, device-code/client-credentials/token-exchange flows, local users + `mob_*` PATs (bcrypt). |
| mobula-api | `internal/api` | REST surface (all frozen-contract operations), auth middleware, **federating Ray Jobs gateway** (Host-based routing, credential strip-and-swap, WS bridge). |
| mobula-proxy | `internal/proxy` | Standalone-mode single-upstream identity proxy. |
| mobula-cli | `cmd/bifrost` | Single binary: serve, login, token, exchange. |

Key invariants carried over verbatim:
- Caller credentials terminate at the control plane; only the cluster's static
  Ray token travels southbound (mobula ADR-0003).
- Level-triggered reconcile: observed state reconstructed every pass, never
  trusted from the store; SSA field manager `bifrost`; `replicas` omitted so
  the Ray autoscaler keeps ownership (ADR-0007).
- Deny-by-default authz; fail closed on non-loopback bind without auth.
- Gateway limits: 64 MiB body cap, 64 in-flight, WS connect/idle timeouts,
  frame/message caps.

## The frozen contract

`openapi.json` exported from mobula (verified complete: **47 operations across
36 paths**, registry-completeness now enforced by the guard test
`openapi_document_registers_every_annotated_operation` on mobula branch
`fix/openapi-complete-registry`), normalized once for stable key ordering, is
the founding commit of `bifrost-api` and the port's specification. Per ADR-0001 the Go
server is **spec-first**: oapi-codegen strict-server stubs are generated from
the frozen file and the file itself is served via `go:embed` — parity is by
construction and handler/spec drift is a compile error. The
TS/Python/Rust SDK pipeline in `bifrost-api` is inherited from `mobula-api`
unchanged except: push token actually configured; secret-gated steps fail red
instead of skipping silently; SBOM generation added.

## Language-independent acceptance assets

1. **Frozen OpenAPI contract** — API shape (above).
2. **`contract/jobs_client_replay.py`** — copied verbatim from mobula. Replays
   the real Ray `JobSubmissionClient` (version negotiation, package upload,
   submit, status, logs, WS log tail) through the gateway. The Go gateway
   ships when this passes, plus a load rig measuring gateway p99.
3. **Store conformance scenarios** — mobula's `tests/store.rs` scenario suite
   (1408 lines) re-expressed in Go against the same Store semantics; Postgres
   service container in CI, per-test schema.

## Port governance (day-one CI gates, before any feature code)

- `go test -race ./...` — always, full suite, no exceptions.
- golangci-lint with at minimum: `errcheck`, `exhaustive` (state-machine
  switches), `depguard` (import boundaries: core/policy ban k8s+DB imports),
  `govet`, `staticcheck`. nilaway if integration is practical.
- Coverage gate ≥ 90% (mobula's bar), excluding `cmd/` main wiring and live
  k8s client wrappers (same exclusions as mobula's llvm-cov config).
- Spec-diff check against the frozen contract (once `internal/api` exists).
- `govulncheck` in CI.
- No cgo anywhere (`CGO_ENABLED=0` enforced in CI build): pure-Go SQLite
  (`modernc.org/sqlite`), static binary, UBI9-micro image posture inherited
  from mobula ADR-0008. FIPS variant later via `GOFIPS140` (req 18) — native,
  still no cgo; fail-closed startup check via `crypto/fips140`.
- Test-only auth-bypass constructors live in `internal/api/apitest` (a
  separate package importable only from `_test.go` files — enforced by
  depguard rule), replacing mobula's `test-util` cargo feature.
- Commit hygiene: no AI-attribution footers (project convention).

## Audit hash chain (decision point, Wave 3)

Mobula's chain is `sha256(prev_hash ‖ canonical serde_json(event))`. Decision
deferred to the Wave 3 plan with two admissible outcomes: (a) byte-exact
canonical JSON reproduction in Go (field order + explicit nulls verified by a
cross-language fixture test), or (b) a deliberate, documented chain-break
migration record ("genesis v2" row linking to the final Rust-chain hash).
Silent incompatibility is not admissible.

## Waves

**Wave 0 — Foundations (this plan cycle)**
0.1 Fix mobula ApiDoc registry → export complete spec (in mobula, Rust).
0.2 Stack decisions locked from community research (recorded as ADR-0001).
0.3 Repos: `bifrost` scaffold (module, CI gates, lint config, layout),
    `bifrost-api` (frozen contract + inherited SDK pipeline + SBOM),
    `bifrost-ui` (fork of mobula-ui, client package swap) — created under
    github.com/brandonrc.
0.4 `internal/core` + `internal/policy` ported (pure logic, no I/O — proves
    the toolchain and the coverage/lint gates on real code).

**Wave 1 — CRITICAL parity (reqs 3, 6, 7, 8)**
Store interface + memory/SQLite/Postgres impls + conformance suite; reconcile
engine; provision translators with typed KubeRay APIs; auth (OIDC + local);
API surface to frozen contract; gateway + WS bridge behind contract replay +
load rig. Rust reference retired from primary duty at Wave 1 exit.

**Wave 2 — CRITICAL greenfield (reqs 1, 2, 4, 5, 9, 11; explore 10, 12)**
Ephemeral RayJob flow for UI; group RayService serving; serving pools;
JupyterLab extension (TS + `bifrost_client` Python SDK — may start during
Wave 1 against the Rust backend, contract makes it backend-agnostic); env-var
passthrough; nebi/storage exploration spikes.

**Wave 3 — HIGH/LOW completion (reqs 13, 14, 15, 16, 17-nonforeclosure, 18)**
Kueue fair-share pools; metering + cost; observability ops; Dask adapter;
audit chain (decision above); FIPS variant; NIST evidence docs.

Each wave gets its own implementation plan in `docs/superpowers/plans/`,
argued from this spec, executed subagent-per-task with review gates.

## Out of scope for the port (tracked, not built)

Slurm backend (req 17 — design keeps Provisioner interface engine-agnostic,
nothing more), cloud VM provisioners, at-rest encryption (mobula issue #60
equivalent), streaming gateway bodies (inherit buffered design + limits;
streaming is a tracked follow-up exactly as in mobula).
