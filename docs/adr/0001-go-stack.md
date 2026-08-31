# ADR-0001: Go stack decisions

Status: accepted · Wave 0 · Decisions from community-practice research (sources at bottom).

| # | Question | Decision | Rejected |
|---|---|---|---|
| 1 | Layout | Single module `github.com/brandonrc/bifrost`, `cmd/bifrost` + `internal/`, no `pkg/`. Authority: go.dev "Organizing a Go module"; Argo CD/Grafana/Temporal pattern. Split an `apis/` module only if external consumers ever need our types. | Multi-module repo (versioning pain, zero benefit today) |
| 2 | HTTP+OpenAPI | **Spec-first**: oapi-codegen ≥ v2.8.0 (initial OpenAPI 3.1 support; needs Go ≥ 1.25), strict-server mode, std-http stubs generated from the frozen contract; the frozen 3.1 file itself is served via `go:embed` — parity by construction, drift = compile error. **Fallback** (if utoipa spec trips the "initial" 3.1 support): downgrade a build-time copy to 3.0.3 with `apiture/openapi-down-convert` for codegen only, still serving the frozen 3.1 file (maintainer-documented workaround). De-risk in Wave 0 by running generation against the real spec. | huma (code-first: two generators never emit identical docs → CI diff becomes a normalization project); ogen (no real 3.1); swaggo (2.0-era) |
| 3 | Router | stdlib net/http (Go 1.22+ pattern routing) — codegen owns routing; chi is a signature-compatible later upgrade if its middleware is wanted. | gin/echo (custom contexts buy nothing under codegen) |
| 4 | Database | pgx v5 + sqlc (Postgres); modernc.org/sqlite via database/sql (SQLite, no cgo). Hand-written `Store` interface + in-memory impl stay as in the Rust reference; the Postgres impl wraps sqlc's `Queries`. Per-engine sqlc query files if dialects diverge; the interface absorbs it. | GORM (runtime reflection, opaque SQL); mattn/go-sqlite3 (cgo); hand-rolled scanning (loses compile-time checking) |
| 5 | Kubernetes | controller-runtime **as a library**: `client.New()` uncached, scheme registers KubeRay (`ray-operator/apis`) + Kueue types; SSA via `client.Apply` + `client.FieldOwner("bifrost")` + `ForceOwnership`. No manager, no informers, no kubebuilder. Watches are an additive upgrade later on the same client API. | Full manager/Reconciler (watch-driven-operator machinery we don't need); raw dynamic client (loses typing we now have) |
| 6 | WebSocket | coder/websocket (maintained nhooyr continuation): context-first, safe concurrent writes, same lib both bridge legs, `NetConn()` for io.Copy-style relay. | gorilla (archived-then-revived, low activity, no context); x/net/websocket (deprecated by Go maintainers) |
| 7 | JWT/OIDC | coreos/go-oidc v3 (discovery + remote JWKS caching + verification) + golang-jwt/jwt v5 (self-minted tokens). Standard pairing. | lestrrat-go/jwx (capable but heavier than needed); go-jose alone (no discovery) |
| 8 | Lint/CI | golangci-lint v2, `default: standard` (errcheck/unused/govet/staticcheck) + `exhaustive`, `depguard` (internal/core + internal/policy may not import k8s.io/*, sigs.k8s.io/*, controller-runtime), errcheck `check-type-assertions: true`. nilaway standalone as **non-blocking advisory job** (not golangci-bundled by design). `go test -race ./...` always. Coverage: `-covermode=atomic -coverprofile` + threshold script; start at measured coverage and ratchet up to 90%. | nilaway-as-blocking (false-positive rate); aspirational day-one 90% gate (ratchet instead) |
| 9 | CLI | cobra (kubectl/helm/kueuectl ecosystem standard; serve/login/token/exchange shape). No viper unless config layering is needed. | stdlib flag (hand-rolled dispatch); urfave/cli (no ecosystem alignment) |
| 10 | Module path | `github.com/brandonrc/bifrost` verbatim. Vanity domains are over-engineering; `internal/` keeps export surface near zero. | vanity import path |

## Risks

- **#2 is the load-bearing bet.** oapi-codegen 3.1 support is self-described "initial." Wave 0 includes generating against the real frozen spec and eyeballing all 48 operations; fallback preserves the architecture.
- Toolchain: Go ≥ 1.25 required (oapi-codegen); we target the installed 1.26.x.
- k8s dependency set (controller-runtime, k8s.io/*, KubeRay apis, Kueue apis) must be pinned to one compatible k8s minor and upgraded as a set.
- Normalize the utoipa export once (stable key ordering) before freezing so parity checks never flake on serialization order.

## Sources

go.dev/doc/modules/layout · github.com/oapi-codegen/oapi-codegen/releases/tag/v2.8.0 (+ issue #373) · jvt.me/posts/2025/05/04/oapi-codegen-trick-openapi-3-1 · github.com/apiture/openapi-down-convert · github.com/ogen-go/ogen/discussions/1410 · websocket.org/guides/languages/go · github.com/golangci/golangci-lint/issues/4045 · pkg.go.dev/go.uber.org/nilaway/cmd/gclplugin · brandur.org/sqlc · github.com/gogs/gogs/issues/7882
