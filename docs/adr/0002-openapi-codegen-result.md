# ADR-0002: OpenAPI codegen de-risk spike — result

Status: accepted · Wave 0 · Resolves the load-bearing bet flagged in ADR-0001 #2/Risks.
**Superseded in part by [ADR-0006](0006-contract-source-of-truth.md)** (2026-09-02): the
contract is no longer frozen and `internal/api/openapi.json` in this repo — not
bifrost-api — is its source of truth. The codegen verdict, command, and the 3.1
findings below still stand; the "47 operations" figure is a point-in-time count.

## Outcome

**Direct 3.1 generation works.** No fallback needed. oapi-codegen v2.8.0's "initial"
OpenAPI 3.1 support handles the frozen mobula spec (`openapi.json`, OpenAPI 3.1.0,
emitted by utoipa v5; 47 operations across 36 paths) cleanly, including its
JSON-Schema-style `oneOf: [{type: "null"}, {$ref}]` nullability idiom — the one
3.1 construct most likely to trip an "initial" 3.1 implementation.

## Command Wave 1's api package should use

```
go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 \
  -generate types,std-http,strict-server -package api openapi.json
```

Pin: `oapi-codegen/v2` resolved `@latest` to **v2.8.0** (matches the version pinned
in ADR-0001 #2; no drift) — now tracked canonically as a `tool` directive in
`go.mod` (`go get -tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0`),
so `go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen` resolves the
pinned version without needing the `@v2.8.0` suffix repeated at every call site.
Run it from *inside* the target Go module (i.e. with a
`go.mod` in scope) — run bare (no enclosing module) it still generates correctly but
emits a benign warning:

```
WARNING: ... std-http-server: Encountered an error while trying to find a
`go.mod` or `tools.mod` in this directory, or 5 levels above it
```

Re-running the identical command from inside a real module produced byte-identical
output and no warning. Wave 1's api package (which will have its own `go.mod` in
scope) will never see this.

Generated stub code needs the runtime dependency `github.com/oapi-codegen/runtime`
(resolved to v1.7.0 via `go mod tidy`) plus its transitive deps
(`github.com/apapsch/go-jsonmerge/v2`, `github.com/google/uuid`, `github.com/oapi-codegen/nullable`).

## Spike procedure

1. `cp mobula/openapi.json /tmp/bifrost-spec-candidate.json`
2. Ran the command above against it — **exit 0**, 6203 lines generated, no errors.
3. Scratch module: `go mod init spike` in `/tmp/bifrost-codegen-spike`, copied the
   generated file in, `go mod tidy`, `go vet ./...`, `go build ./...` — **all clean,
   zero warnings, zero errors.**
4. Fallback (Step 4, 3.0.3 downgrade via `apiture/openapi-down-convert`) was **not
   exercised** — direct generation succeeded, so it wasn't needed. `npx` and `node`
   are available locally (`/opt/homebrew/bin/npx`, `/opt/homebrew/bin/node`) if a
   future spec construct forces this path; command would be
   `npx @apiture/openapi-down-convert --input <spec> --output <spec-30.json>`.

## Quality spot-check

- **Strict-server interface: exactly 47 methods**, one per spec operation — full
  coverage, no operations silently dropped. Confirmed across every operation group:
  access (assignments/roles), audit (events/verify), local_auth/tokens
  (login/logout/providers/tokens/users), clusters (CRUD + events/jobs/logs/metrics/
  nodes/suspend/resume), identity, jobs, metrics, pools (CRUD + allocations/usage),
  registry, services (CRUD), settings/policy, usage, cluster obs, healthz/version.
- **Nullability**: the spec has 13 `oneOf: [{type: "null"}, {$ref}]` sites (utoipa's
  3.1 encoding of Rust `Option<T>`, replacing 3.0's `nullable: true`). Every one
  generated as a plain Go pointer field with `omitempty` and no data loss — e.g.
  `ClusterMetrics.Cpu *ResourceStat`, `PoolSpec.GpuSharing *GpuSharing`,
  `UpdateUserRequest.Role *LocalRole`, `AuditEvent.Required *AuditRequired` (the
  awkwardly-named `required` field, a JSON Schema keyword collision, also came
  through clean), and the `decision` query param on `GET /api/v1/audit`
  (`ListAuditEventsParams.Decision *AuditDecision`, with correct `form`/`json`
  struct tags). No spec `oneOf` was flattened, dropped, or replaced with
  `interface{}`/`json.RawMessage`.
- **Enums**: generated as `type X string` + a `const` block of typed values + a
  `Valid() bool` helper (e.g. `AuditDecision`, `Engine`, `GpuSharing`, `LocalRole`,
  `UpgradeStrategy`). Clean, idiomatic, no surprises.
- **`additionalProperties` + named fields** (e.g. `BudgetView`, `window_secs` plus
  open extra keys) generated correct `Get`/`Set`/`UnmarshalJSON`/`MarshalJSON`
  boilerplate — the only `json.RawMessage` usage in the file, and it's the expected
  oapi-codegen pattern for this construct, not an escape hatch for something it
  couldn't model.
- **`interface{}` usage**: 47 occurrences, all in the strict-server adapter
  boilerplate (`func(ctx, w, r, request interface{}) (interface{}, error)` per
  handler) — standard strict-server plumbing, unrelated to spec fidelity.
- **No mangled/anonymous-schema type names** (no `FooN1`/`FooN2` numeric-suffix
  types), because the spec defines all reusable shapes as named `$ref` components
  rather than inline anonymous schemas.
- No `oneOf`/`anyOf`/`allOf` combinators other than the 13 nullable-wrapper sites
  above appear in the spec (`anyOf` count: 0, `allOf` count: 0), so there was no
  true discriminated-union construct to stress-test union-type generation against.
  That remains untested; flag it if a future spec revision adds a real
  multi-variant `oneOf`.

## Verdict

Go with **direct 3.1 generation**, oapi-codegen v2.8.0, `-generate types,std-http,strict-server`.
No fallback wiring needed in Wave 1. Re-run this spike if the spec ever introduces
a true multi-variant `oneOf`/discriminated union (only the null-wrapper idiom has
been validated).
