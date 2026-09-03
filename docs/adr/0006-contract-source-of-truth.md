# ADR-0006: The contract's source of truth is `internal/api/openapi.json`

Status: accepted · 2026-09-02 · Supersedes in part ADR-0002 (the "frozen" framing); refines ADR-0001 #2 (spec-first stays)

## Context

Through Wave 1 the contract was **frozen**: `openapi.json` was exported once from
the Rust reference (47 operations across 36 paths), committed as the founding
artifact of `github.com/brandonrc/bifrost-api`, and vendored verbatim into
`internal/api/openapi.json`. bifrost-api was the authority; bifrost's copy was
checked against it by `ci.yml`'s `spec-sync` job (`diff -u` against
`bifrost-api@main`, red on any difference).

That arrangement had exactly one admissible state — the two files byte-identical —
and it worked while the port's job was parity. Wave 2 adds operations the Rust
reference never had (ephemeral RayJobs, project-scoped serving, serving pools,
profiles, per-user usage). The first such change exposes the two failure modes
the frozen model forces:

1. **Edit bifrost first.** The handler PR turns `spec-sync` red on merge and
   stays red until someone hand-edits bifrost-api to match. Every bifrost-side
   contract change is a two-repo, two-PR, red-in-between dance, and the "do not
   hand-edit" rule in bifrost-api's README is violated by the very act of
   keeping it in sync.
2. **Edit bifrost-api first.** The contract changes before any server implements
   it; the SDK pipeline publishes clients for operations that 404 (or 501) until
   the bifrost PR lands. The gate is green and the system is lying — the
   "silent incompatibility" SPEC.md rules out.

Neither has a server-side truth to anchor on, because the truth *was* the
export. Now that the Go server is the reference implementation, the file the
server is generated from is the only place a contract change can be checked
against the code that implements it.

## Decision

- **`internal/api/openapi.json` in `brandonrc/bifrost` is the source of truth**
  for the Bifrost REST contract. It is hand-edited, in this repo, and nowhere
  else.
- **`brandonrc/bifrost-api` is a downstream publish target.** It hosts the
  published copy (`openapi.json`, plus a regenerated `openapi.yaml` companion)
  and the SDK pipeline. Its copy is written only by bifrost's
  `.github/workflows/sync-api.yml`, which runs on every push to `main` that
  touches the contract, commits as `bifrost-ci` with
  `chore: sync contract from bifrost@<sha>`, and pushes directly to bifrost-api
  `main` — where `generate.yml` regenerates and publishes the TS/Python SDKs.
  Nobody edits `openapi.json` in bifrost-api by hand.
- **The `spec-sync` CI job is deleted.** After the flip every legitimate
  bifrost-side change would fail it until the push landed; it measured the
  wrong invariant. The lockstep gate is now the `api + client codegen drift`
  step in `ci.yml`'s `test` job (regenerate, `git diff --exit-code` on both
  `zz_generated_*` files), plus `sync-api.yml` for publication.
- **A missing `BIFROST_API_PUSH_TOKEN` on a main push is a hard failure**, not
  a notice-and-skip as in the Rust predecessor workflow this was modelled on. A main
  push is expected to publish; a skip would leave the SDKs silently behind
  the server. `concurrency: sync-api` serialises the pushes so two merges
  cannot race.

## What "spec-first" still means

ADR-0001 #2 is unchanged in substance: the server is generated *from* the
spec, never the reverse. oapi-codegen (`strict-server`, `std-http`) turns
`openapi.json` into the `StrictServerInterface` the handlers must satisfy, and
`pkg/client` is generated from the same file. A handler that does not match
the contract is a compile error; a contract change without its handler is a
compile error; a regenerated file that differs from the committed one is a CI
failure. The file is also the one served at `/api/v1/openapi.json` via
`go:embed`. What changed is only *who owns the file*: the server repo, not a
sibling.

"Frozen" is retired as a description of the contract. Its 3.1 shape, the
utoipa nullability idiom, and the codegen command ADR-0002 validated all still
hold; the operation count no longer does and is not a claim any document
should make.

## The rule for a contract change

1. Edit `internal/api/openapi.json` **in the same PR** as the handler (or the
   handler change) that implements it. A contract-only PR and a handler-only
   PR are both wrong: the first publishes SDKs for nothing, the second cannot
   compile.
2. Run `go generate ./internal/api/... ./pkg/client/...` and **commit the
   generated output** (`internal/api/zz_generated_api.go`,
   `pkg/client/zz_generated_client.go`). CI regenerates and diffs; drift is red.
3. Run `make report` and commit the regenerated
   `docs/requirements/traceability.md|json` when requirement tests moved.
4. Merge. `sync-api.yml` publishes the new contract to bifrost-api on the push
   to `main`; bifrost-api's `generate.yml` publishes the SDKs from there.

During the Wave 2 build-out the contract edit itself is confined to the
`contract-seams` package (one PR, merged first) so parallel feature branches
never touch the JSON or the generated files; that is a scheduling rule for
that plan, not part of this decision.

## Consequences

- bifrost-api's README, `validate.yml` header, and TODO are rewritten to
  describe the file as pushed, not frozen; an advisory `upstream-drift` job
  there diffs its copy against `brandonrc/bifrost@main` so a missed push is
  visible without gating anything (the push itself is the gate, on this side).
- The first `sync-api` run after this lands rewrites bifrost-api's
  `openapi.yaml` once (PyYAML's serialisation differs from the Ruby one that
  produced the original; the documents are equal) and is a no-op thereafter
  until the contract changes.
- `BIFROST_API_PUSH_TOKEN` is currently the repo owner's `gh` token (plan
  ruling D10); it is to be replaced with a fine-grained PAT scoped to
  `contents: write` on `brandonrc/bifrost-api`.
- Anything else that consumed bifrost-api as an authority (bifrost-ui,
  bifrost-jupyter) is unaffected in mechanism — they keep consuming the
  published SDKs — but their contract now moves when bifrost `main` moves.
