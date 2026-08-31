# Bifrost Wave 0 — Foundations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the bifrost repo with all CI discipline gates live, prove the toolchain by porting the two pure-logic packages (core, policy) from the Rust reference, validate the spec-first codegen bet, and bootstrap bifrost-api.

**Architecture:** Single Go module `github.com/brandonrc/bifrost` (ADR-0001 #1). Rust reference for all ported code: `/Users/khan/openteams/mobula` — every port task names its source files; the Rust tests are the behavioral oracle and get ported alongside the code.

**Tech Stack:** Go ≥1.25 (toolchain 1.26), golangci-lint v2, oapi-codegen ≥2.8.0, no cgo anywhere.

**Spec:** `docs/SPEC.md` (this repo) + `docs/adr/0001-go-stack.md`

## Global Constraints

- Module path `github.com/brandonrc/bifrost`; license Apache-2.0 (match mobula).
- `CGO_ENABLED=0` for all builds and tests.
- `internal/core` and `internal/policy` import NOTHING outside stdlib + each other (depguard-enforced; mobula ADR-0002).
- Every commit message: conventional-commit style, NO AI-attribution footers (no "Generated with", no Co-Authored-By: Claude) — project convention.
- All work on branch `main` of the new repo (greenfield; no force pushes after first push to GitHub).
- Port fidelity rule: when porting, translate behavior and port the Rust unit tests as Go tests. Do not "improve" logic in a port task; file follow-ups instead. Naming: Rust `snake_case` fns → Go `CamelCase` exported / `camelCase` unexported; keep domain vocabulary identical (ClusterSpec, DesiredState, etc.).

---

### Task 1: Repo scaffold

**Files:**
- Create: `go.mod`, `LICENSE`, `README.md`, `.gitignore`, `Makefile`

**Interfaces:**
- Produces: buildable empty module; `make lint test build` targets used by every later task and CI.

- [ ] **Step 1: go.mod**

```
module github.com/brandonrc/bifrost

go 1.25

toolchain go1.26.2
```

- [ ] **Step 2: LICENSE** — copy `/Users/khan/openteams/mobula/LICENSE` (Apache-2.0) verbatim.

- [ ] **Step 3: README.md**

```markdown
# Bifrost

A FOSS, Anyscale-grade control plane for Ray and Dask clusters, in Go.
Successor to [mobula](https://github.com/brandonrc/mobula) (Rust), which is the
frozen executable reference until parity. See `docs/SPEC.md` for the program
specification and `docs/adr/` for decisions.

Status: Wave 0 (foundations). Not yet usable.
```

- [ ] **Step 4: .gitignore**

```
/bifrost
*.out
coverage.txt
dist/
```

- [ ] **Step 5: Makefile**

```makefile
export CGO_ENABLED=0

.PHONY: build test lint cover
build:
	go build ./...
test:
	go test -race ./...
cover:
	go test -race -covermode=atomic -coverprofile=coverage.txt ./...
	go tool cover -func=coverage.txt | tail -1
lint:
	golangci-lint run
```

- [ ] **Step 6: Verify** — Run `go build ./...` (succeeds, empty module). Run `git add -A && git commit -m "chore: scaffold bifrost module"` (docs/SPEC.md, ADRs, and this plan are included in this first commit).

### Task 2: Lint + CI gates

**Files:**
- Create: `.golangci.yml`, `.github/workflows/ci.yml`, `scripts/coverage-gate.sh`

**Interfaces:**
- Produces: CI that every later task must keep green; depguard boundary rule consumed by Tasks 4–5.

- [ ] **Step 1: .golangci.yml**

```yaml
version: "2"
linters:
  default: standard
  enable:
    - exhaustive
    - depguard
  settings:
    errcheck:
      check-type-assertions: true
    exhaustive:
      default-signifies-exhaustive: true
    depguard:
      rules:
        pure-domain:
          files: ["**/internal/core/**", "**/internal/policy/**"]
          deny:
            - pkg: "k8s.io"
              desc: "core/policy are k8s-free (mobula ADR-0002)"
            - pkg: "sigs.k8s.io"
              desc: "core/policy are k8s-free (mobula ADR-0002)"
            - pkg: "github.com/jackc/pgx"
              desc: "core/policy are I/O-free"
            - pkg: "database/sql"
              desc: "core/policy are I/O-free"
```

- [ ] **Step 2: scripts/coverage-gate.sh** (executable)

```bash
#!/usr/bin/env bash
# Ratcheting coverage gate: fails if total coverage drops below THRESHOLD.
# Ratchet THRESHOLD upward as coverage grows; target 90 (SPEC.md governance).
set -euo pipefail
THRESHOLD="${COVERAGE_THRESHOLD:-70}"
total=$(go tool cover -func=coverage.txt | tail -1 | awk '{gsub("%","",$3); print $3}')
echo "total coverage: ${total}% (gate: ${THRESHOLD}%)"
awk -v t="$total" -v g="$THRESHOLD" 'BEGIN { exit (t+0 < g+0) ? 1 : 0 }'
```

- [ ] **Step 3: .github/workflows/ci.yml**

```yaml
name: ci
on:
  push: { branches: [main] }
  pull_request:
env:
  CGO_ENABLED: "0"
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - uses: golangci/golangci-lint-action@v8
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - run: go test -race -covermode=atomic -coverprofile=coverage.txt ./...
      - run: ./scripts/coverage-gate.sh
  vuln:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...
  nilaway-advisory:
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - run: go run go.uber.org/nilaway/cmd/nilaway@latest ./... || true
```

- [ ] **Step 4: Verify locally** — `golangci-lint run` may not be installed locally; that's acceptable (CI owns it). Run `go vet ./...` as the local floor. `chmod +x scripts/coverage-gate.sh`.

- [ ] **Step 5: Commit** — `git commit -m "ci: lint config, race+coverage gates, govulncheck, nilaway advisory"`

### Task 3: Codegen de-risk spike (ADR-0001 #2's load-bearing bet)

**Files:**
- Create: `docs/adr/0002-openapi-codegen-result.md` (records the outcome)
- Uses: the completed spec from mobula branch `fix/openapi-complete-registry` (workspace root `openapi.json` after `cargo test -p mobula-api export_openapi`)

**Interfaces:**
- Consumes: complete frozen-candidate `openapi.json` from the mobula fix branch.
- Produces: go/no-go on direct 3.1 codegen vs 3.0.3-downgrade fallback; the command line that Wave 1's api package will use.

- [ ] **Step 1:** Copy the spec: `cp /Users/khan/openteams/mobula/openapi.json /tmp/bifrost-spec-candidate.json`
- [ ] **Step 2:** Run generation: `go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest -generate types,std-http,strict-server -package api /tmp/bifrost-spec-candidate.json > /tmp/gen_test_api.go` — capture any errors verbatim.
- [ ] **Step 3:** If generation succeeds: `cd /tmp && go vet` the output in a scratch module; skim the generated types for all operation groups (clusters, pools, services, auth, audit, usage, settings, access, registry, obs) checking nullability (`*T` vs `T`) and enums look sane.
- [ ] **Step 4:** If generation fails on 3.1 constructs: run the fallback — `npx @apiture/openapi-down-convert --input /tmp/bifrost-spec-candidate.json --output /tmp/bifrost-spec-30.json` then repeat Step 2 against the 3.0.3 file. Record which constructs broke.
- [ ] **Step 5:** Write `docs/adr/0002-openapi-codegen-result.md`: outcome (direct-3.1 | downgrade-fallback), exact oapi-codegen version pinned, the generation command, and any spec constructs that needed attention. Commit: `git commit -m "docs: record codegen de-risk outcome (ADR-0002)"`

### Task 4: Port internal/core

**Files:**
- Create: `internal/core/cluster.go`, `internal/core/cluster_test.go`, plus one file pair per Rust source module
- Reference: `/Users/khan/openteams/mobula/crates/mobula-core/src/` — `cluster.rs`, `pool.rs`, `node.rs`, `obs.rs`, `audit.rs`, `auth.rs`, `service.rs`, `job.rs`, `registry` parts of `lib.rs` (~2.2K LOC). SKIP `crypto.rs` (FIPS check — Wave 3).

**Interfaces:**
- Produces (exact names later tasks rely on):

```go
package core

type DesiredState string // DesiredRunning | DesiredSuspended | DesiredTerminated
type ClusterState string // states exactly as the Rust enum serializes them (serde rename strings are the wire truth — read the #[serde] attributes)
type ClusterSpec struct{ ... } // field-for-field from cluster.rs, JSON tags matching serde output exactly (the frozen OpenAPI schemas are the arbiter)
type Engine string // EngineRay | EngineDask
func (s ClusterState) CanTransitionTo(next ClusterState) bool // port the state-machine transition rules
```

- [ ] **Step 1:** Read every Rust source file listed above end-to-end, including `#[cfg(test)]` modules, before writing Go. The serde attributes (`rename_all`, `default`, `skip_serializing_if`) define the JSON wire format — encode them as Go struct tags; cross-check field names against the frozen spec's schemas.
- [ ] **Step 2:** For each Rust module: write the Go test file FIRST by porting the Rust `#[test]` functions (same cases, same expected values), run `go test ./internal/core/... ` to see them fail, then port the implementation, then see them pass. Work one module at a time; commit per module: `git commit -m "feat(core): port <module> from mobula-core"`.
- [ ] **Step 3:** Exhaustiveness: every `match` on a state/engine enum in Rust becomes a `switch` with all cases and NO default (so the `exhaustive` linter guards additions). Every Rust `Option<T>` field becomes `*T` (or a zero-value-safe type when the Rust `#[serde(default)]` says so).
- [ ] **Step 4:** Run `make test && go vet ./...`; verify depguard boundary holds (core imports stdlib only). Commit any stragglers.

### Task 5: Port internal/policy

**Files:**
- Create: `internal/policy/*.go` + `*_test.go` per Rust module
- Reference: `/Users/khan/openteams/mobula/crates/mobula-policy/src/` (~1.2K LOC: ResourceMap accounting, cost estimation from price sheets, quota admission, GPU sharing, K8s quantity parsing)

**Interfaces:**
- Consumes: `core.ClusterSpec`, `core` pool/flavor types from Task 4.
- Produces:

```go
package policy

type ResourceMap map[string]Quantity // sparse, arbitrary k8s resource names
func ParseQuantity(s string) (Quantity, error) // port the k8s quantity parser exactly, incl. binary/decimal suffixes — port every parser test case
type PriceSheet struct{ ... }
func (p *PriceSheet) Estimate(spec *core.ClusterSpec) (Estimate, error)
func AdmitQuota(usage, request, limit ResourceMap) error // admission decision, pure
```

- [ ] **Step 1:** Same test-first porting protocol as Task 4 (policy has 44 Rust tests — all get ported; the quantity parser and cost-estimation edge cases are the value here).
- [ ] **Step 2:** `make test`, commit per module: `git commit -m "feat(policy): port <module> from mobula-policy"`.
- [ ] **Step 3:** Measure coverage (`make cover`); set `COVERAGE_THRESHOLD` in ci.yml env to the measured value rounded down to nearest 5 (ratchet起点), commit: `git commit -m "ci: set initial coverage ratchet"`.

### Task 6: Publish bifrost to GitHub

- [ ] **Step 1:** `gh repo create brandonrc/bifrost --public --source /Users/khan/openteams/bifrost --description "FOSS Anyscale-grade control plane for Ray and Dask, in Go" --push`
- [ ] **Step 2:** Verify CI runs green on GitHub Actions; fix anything red before proceeding.

### Task 7: Bootstrap bifrost-api (blocked on the mobula spec-completion branch)

**Files:**
- Create: new repo `/Users/khan/openteams/bifrost-api` — `openapi.json` (frozen), `openapi.yaml`, `README.md`, `redocly.yaml`, `.spectral.yaml`, `sdk/{typescript,python,rust}/config.yaml`, `.github/workflows/{validate.yml,generate.yml}`

**Interfaces:**
- Consumes: the complete `openapi.json` from mobula branch `fix/openapi-complete-registry` (all 48 operations), normalized (run through `jq -S .` once for stable key ordering — record the exact command in the README).
- Produces: `@brandonrc/bifrost-client` (npm, GitHub Packages), `bifrost_client` (Python), `bifrost-client` (Rust crate) SDK configs.

- [ ] **Step 1:** Copy `/Users/khan/openteams/mobula-api/{redocly.yaml,.spectral.yaml,sdk,.github}` as the starting point; rename all `mobula` → `bifrost` in package names and workflow env.
- [ ] **Step 2:** In both workflows: change every secret-gated `::notice::`-skip into a hard `exit 1` when the secret is absent on main-branch pushes (SPEC.md: silent skips are a defect).
- [ ] **Step 3:** Add SBOM step to generate.yml: `anchore/sbom-action@v0` producing CycloneDX for each SDK artifact.
- [ ] **Step 4:** `jq -S . < mobula/openapi.json > openapi.json` (the freeze), commit `feat: freeze bifrost API contract v1 (from mobula@<sha>, 48 operations)`, then `gh repo create brandonrc/bifrost-api --public --source . --push`.

### Task 8: Fork bifrost-ui

- [ ] **Step 1:** `cp -R /Users/khan/openteams/mobula-ui /Users/khan/openteams/bifrost-ui` (fresh `git init`, don't carry history — mobula-ui history stays in mobula-ui).
- [ ] **Step 2:** Rename: `package.json` name → `bifrost-ui`, description updated; `@brandonrc/mobula-client` dependency stays AS-IS until `@brandonrc/bifrost-client@0.1.x` is published from Task 7's pipeline, then swap import paths (`grep -rl "@brandonrc/mobula-client" src/`) — record this as a TODO in the README if the client isn't published yet.
- [ ] **Step 3:** Rebrand user-visible strings: `grep -rn "Mobula\|mobula" src/ index.html` and replace with Bifrost (skip the client import until Step 2's swap).
- [ ] **Step 4:** `npm install && npm run build && npm test` must pass; commit `chore: fork bifrost-ui from mobula-ui`, `gh repo create brandonrc/bifrost-ui --public --source . --push`.

---

## Self-review notes

- Spec coverage: Wave 0 items 0.1 (mobula branch — running separately), 0.2 (ADR-0001 done), 0.3 (Tasks 1,2,6,7,8), 0.4 (Tasks 4,5) ✅; codegen de-risk (ADR-0001 risk #1) = Task 3 ✅.
- Task 4/5 port-by-reference is intentional: the Rust source + its tests are the literal content; the Interfaces blocks pin the names later waves consume.
- Ordering: Tasks 1→2→3 sequential; 4→5 sequential after 2; 6 after 5; 7 blocked on external branch; 8 independent after 7's repo exists (can start earlier minus the client swap).
